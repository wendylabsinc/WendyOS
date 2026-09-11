package commands

import (
	"fmt"

	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

const diskWarningThresholdPercent int64 = 85

type diskUsageAlert struct {
	Mountpoint       string
	UsedPercent      int64
	Device           string
	ContainerStorage bool
}

// highDiskUsage returns the fullest partition above the warning threshold.
// Partition data takes precedence over the legacy root-only fields.
func highDiskUsage(partitions []*agentpb.DiskPartition, legacyUsed, legacyTotal *int64, storage ...*agentpb.DiskPartition) (diskUsageAlert, bool) {
	var (
		best      diskUsageAlert
		bestRatio float64
	)
	var containerStorage *agentpb.DiskPartition
	if len(storage) > 0 && storage[0] != nil {
		containerStorage = storage[0]
		partitions = append(append([]*agentpb.DiskPartition{}, partitions...), containerStorage)
	}
	for _, p := range partitions {
		if p == nil || p.GetTotalBytes() <= 0 || p.GetUsedBytes() < 0 || p.GetUsedBytes() > p.GetTotalBytes() {
			continue
		}
		ratio := float64(p.GetUsedBytes()) / float64(p.GetTotalBytes())
		if ratio > float64(diskWarningThresholdPercent)/100 && ratio > bestRatio {
			bestRatio = ratio
			best = diskUsageAlert{Mountpoint: p.GetMountpoint(), Device: p.GetDevice(), UsedPercent: int64(ratio * 100), ContainerStorage: samePartition(p, containerStorage)}
		}
	}
	if bestRatio > 0 {
		return best, true
	}
	if len(partitions) == 0 && legacyUsed != nil && legacyTotal != nil && *legacyTotal > 0 && *legacyUsed >= 0 {
		ratio := float64(*legacyUsed) / float64(*legacyTotal)
		if ratio > float64(diskWarningThresholdPercent)/100 {
			return diskUsageAlert{Mountpoint: "/", UsedPercent: int64(ratio * 100)}, true
		}
	}
	return diskUsageAlert{}, false
}

func diskUsageWarningText(alert diskUsageAlert) string {
	mountpoint := alert.Mountpoint
	if mountpoint == "" {
		mountpoint = "the device disk"
	} else {
		mountpoint = fmt.Sprintf("disk %s", mountpoint)
	}
	text := fmt.Sprintf("%s is %d%% full.", mountpoint, alert.UsedPercent)
	if alert.ContainerStorage {
		return text + " Free cached container data with 'wendy device cache prune'."
	}
	return text + " Free space on this filesystem."
}

func samePartition(a, b *agentpb.DiskPartition) bool {
	if a == nil || b == nil {
		return false
	}
	return (a.GetDevice() != "" && a.GetDevice() == b.GetDevice()) || (a.GetMountpoint() != "" && a.GetMountpoint() == b.GetMountpoint())
}

func printRunDiskUsageWarning(resp *agentpb.GetAgentVersionResponse) {
	if resp == nil {
		return
	}
	if alert, ok := highDiskUsage(resp.GetPartitions(), resp.DiskUsedBytes, resp.DiskTotalBytes, resp.GetContainerStorage()); ok {
		cliNotice("Warning: %s", diskUsageWarningText(alert))
	}
}
