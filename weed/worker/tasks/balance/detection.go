package balance

import (
	"fmt"
	"time"

	"github.com/seaweedfs/seaweedfs/weed/admin/topology"
	"github.com/seaweedfs/seaweedfs/weed/glog"
	"github.com/seaweedfs/seaweedfs/weed/pb/worker_pb"
	"github.com/seaweedfs/seaweedfs/weed/worker/tasks/base"
	"github.com/seaweedfs/seaweedfs/weed/worker/types"
)

// Detection implements the detection logic for balance tasks
func Detection(metrics []*types.VolumeHealthMetrics, clusterInfo *types.ClusterInfo, config base.TaskConfig) ([]*types.TaskDetectionResult, error) {
	if !config.IsEnabled() {
		return nil, nil
	}

	balanceConfig := config.(*Config)

	// Skip if cluster is too small
	minVolumeCount := 2 // More reasonable for small clusters
	if len(metrics) < minVolumeCount {
		glog.Infof("BALANCE: No tasks created - cluster too small (%d volumes, need ≥%d)", len(metrics), minVolumeCount)
		return nil, nil
	}

	// Analyze volume distribution across servers
	serverVolumeCounts := make(map[string]int)
	for _, metric := range metrics {
		serverVolumeCounts[metric.Server]++
	}

	if len(serverVolumeCounts) < balanceConfig.MinServerCount {
		glog.Infof("BALANCE: No tasks created - too few servers (%d servers, need ≥%d)", len(serverVolumeCounts), balanceConfig.MinServerCount)
		return nil, nil
	}

	// Calculate balance metrics
	totalVolumes := len(metrics)
	avgVolumesPerServer := float64(totalVolumes) / float64(len(serverVolumeCounts))

	maxVolumes := 0
	minVolumes := totalVolumes
	maxServer := ""
	minServer := ""

	for server, count := range serverVolumeCounts {
		if count > maxVolumes {
			maxVolumes = count
			maxServer = server
		}
		if count < minVolumes {
			minVolumes = count
			minServer = server
		}
	}

	// Check if imbalance exceeds threshold
	imbalanceRatio := float64(maxVolumes-minVolumes) / avgVolumesPerServer
	if imbalanceRatio <= balanceConfig.ImbalanceThreshold {
		glog.Infof("BALANCE: No tasks created - cluster well balanced. Imbalance=%.1f%% (threshold=%.1f%%). Max=%d volumes on %s, Min=%d on %s, Avg=%.1f",
			imbalanceRatio*100, balanceConfig.ImbalanceThreshold*100, maxVolumes, maxServer, minVolumes, minServer, avgVolumesPerServer)
		return nil, nil
	}

	// Select a volume from the overloaded server for balance
	var selectedVolume *types.VolumeHealthMetrics
	for _, metric := range metrics {
		if metric.Server == maxServer {
			selectedVolume = metric
			break
		}
	}

	if selectedVolume == nil {
		glog.Warningf("BALANCE: Could not find volume on overloaded server %s", maxServer)
		return nil, nil
	}

	// Create balance task with volume and destination planning info
	reason := fmt.Sprintf("Cluster imbalance detected: %.1f%% (max: %d on %s, min: %d on %s, avg: %.1f)",
		imbalanceRatio*100, maxVolumes, maxServer, minVolumes, minServer, avgVolumesPerServer)

	task := &types.TaskDetectionResult{
		TaskType:   types.TaskTypeBalance,
		VolumeID:   selectedVolume.VolumeID,
		Server:     selectedVolume.Server,
		Collection: selectedVolume.Collection,
		Priority:   types.TaskPriorityNormal,
		Reason:     reason,
		ScheduleAt: time.Now(),
	}

	// Plan destination if ActiveTopology is available
	if clusterInfo.ActiveTopology != nil {
		destinationPlan, err := planBalanceDestination(clusterInfo.ActiveTopology, selectedVolume)
		if err != nil {
			glog.Warningf("Failed to plan balance destination for volume %d: %v", selectedVolume.VolumeID, err)
			return nil, nil // Skip this task if destination planning fails
		}

		// Create typed parameters with destination information
		task.TypedParams = &worker_pb.TaskParams{
			VolumeId:   selectedVolume.VolumeID,
			Server:     selectedVolume.Server,
			Collection: selectedVolume.Collection,
			VolumeSize: selectedVolume.Size, // Store original volume size for tracking changes
			TaskParams: &worker_pb.TaskParams_BalanceParams{
				BalanceParams: &worker_pb.BalanceTaskParams{
					DestNode:           destinationPlan.TargetNode,
					EstimatedSize:      destinationPlan.ExpectedSize,
					PlacementScore:     destinationPlan.PlacementScore,
					PlacementConflicts: destinationPlan.Conflicts,
					ForceMove:          false,
					TimeoutSeconds:     600, // 10 minutes default
				},
			},
		}

		glog.V(1).Infof("Planned balance destination for volume %d: %s -> %s (score: %.2f)",
			selectedVolume.VolumeID, selectedVolume.Server, destinationPlan.TargetNode, destinationPlan.PlacementScore)
	} else {
		glog.Warningf("No ActiveTopology available for destination planning in balance detection")
		return nil, nil
	}

	return []*types.TaskDetectionResult{task}, nil
}

// planBalanceDestination plans the destination for a balance operation
// This function implements destination planning logic directly in the detection phase
func planBalanceDestination(activeTopology *topology.ActiveTopology, selectedVolume *types.VolumeHealthMetrics) (*topology.DestinationPlan, error) {
	// Get source node information from topology
	var sourceRack, sourceDC string

	// Extract rack and DC from topology info
	topologyInfo := activeTopology.GetTopologyInfo()
	if topologyInfo != nil {
		for _, dc := range topologyInfo.DataCenterInfos {
			for _, rack := range dc.RackInfos {
				for _, dataNodeInfo := range rack.DataNodeInfos {
					if dataNodeInfo.Id == selectedVolume.Server {
						sourceDC = dc.Id
						sourceRack = rack.Id
						break
					}
				}
				if sourceRack != "" {
					break
				}
			}
			if sourceDC != "" {
				break
			}
		}
	}

	// Get available disks, excluding the source node
	availableDisks := activeTopology.GetAvailableDisks(topology.TaskTypeBalance, selectedVolume.Server)
	if len(availableDisks) == 0 {
		return nil, fmt.Errorf("no available disks for balance operation")
	}

	// Find the best destination disk based on balance criteria
	var bestDisk *topology.DiskInfo
	bestScore := -1.0

	for _, disk := range availableDisks {
		score := calculateBalanceScore(disk, sourceRack, sourceDC, selectedVolume.Size)
		if score > bestScore {
			bestScore = score
			bestDisk = disk
		}
	}

	if bestDisk == nil {
		return nil, fmt.Errorf("no suitable destination found for balance operation")
	}

	return &topology.DestinationPlan{
		TargetNode:     bestDisk.NodeID,
		TargetDisk:     bestDisk.DiskID,
		TargetRack:     bestDisk.Rack,
		TargetDC:       bestDisk.DataCenter,
		ExpectedSize:   selectedVolume.Size,
		PlacementScore: bestScore,
		Conflicts:      checkPlacementConflicts(bestDisk, sourceRack, sourceDC),
	}, nil
}

// calculateBalanceScore calculates placement score for balance operations
func calculateBalanceScore(disk *topology.DiskInfo, sourceRack, sourceDC string, volumeSize uint64) float64 {
	if disk.DiskInfo == nil {
		return 0.0
	}

	score := 0.0

	// Prefer disks with lower current volume count (better for balance)
	if disk.DiskInfo.MaxVolumeCount > 0 {
		utilization := float64(disk.DiskInfo.VolumeCount) / float64(disk.DiskInfo.MaxVolumeCount)
		score += (1.0 - utilization) * 40.0 // Up to 40 points for low utilization
	}

	// Prefer different racks for better distribution
	if disk.Rack != sourceRack {
		score += 30.0
	}

	// Prefer different data centers for better distribution
	if disk.DataCenter != sourceDC {
		score += 20.0
	}

	// Prefer disks with lower current load
	score += (10.0 - float64(disk.LoadCount)) // Up to 10 points for low load

	return score
}

// checkPlacementConflicts checks for placement rule conflicts
func checkPlacementConflicts(disk *topology.DiskInfo, sourceRack, sourceDC string) []string {
	var conflicts []string

	// For now, implement basic conflict detection
	// This could be extended with more sophisticated placement rules
	if disk.Rack == sourceRack && disk.DataCenter == sourceDC {
		conflicts = append(conflicts, "same_rack_as_source")
	}

	return conflicts
}
