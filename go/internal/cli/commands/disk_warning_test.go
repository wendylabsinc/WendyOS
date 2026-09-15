package commands

import (
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

func TestHighDiskUsageThresholdAndFullestPartition(t *testing.T) {
	parts := []*agentpb.DiskPartition{
		{Mountpoint: "/", UsedBytes: 850, TotalBytes: 1000},
		{Mountpoint: "/data", UsedBytes: 960, TotalBytes: 1000},
		{Mountpoint: "/boot", UsedBytes: 9, TotalBytes: 10},
	}
	alert, ok := highDiskUsage(parts, nil, nil)
	if !ok || alert.Mountpoint != "/data" || alert.UsedPercent != 96 {
		t.Fatalf("alert = %+v, ok=%v", alert, ok)
	}
	if strings.Contains(diskUsageWarningText(alert), "cache prune") {
		t.Fatal("unconfirmed storage must not recommend container pruning")
	}
}

func TestContainerStoragePruningAdvice(t *testing.T) {
	root := &agentpb.DiskPartition{Mountpoint: "/", Device: "/dev/root", UsedBytes: 99, TotalBytes: 100}
	storage := &agentpb.DiskPartition{Mountpoint: "/data", Device: "/dev/nvme0n1p1", UsedBytes: 10, TotalBytes: 100}
	alert, _ := highDiskUsage([]*agentpb.DiskPartition{root, storage}, nil, nil, storage)
	if strings.Contains(diskUsageWarningText(alert), "cache prune") {
		t.Fatal("root pressure recommended pruning container storage on a different filesystem")
	}
	root.UsedBytes, storage.UsedBytes = 10, 99
	alert, _ = highDiskUsage([]*agentpb.DiskPartition{root, storage}, nil, nil, storage)
	if !strings.Contains(diskUsageWarningText(alert), "cache prune") {
		t.Fatal("confirmed container pressure missing pruning advice")
	}
	table := formatPartitionTable([]*agentpb.DiskPartition{root, storage}, storage)
	if strings.Count(table, "/data") != 1 || !strings.Contains(table, "/data (container storage)") || strings.Index(table, "/data") > strings.Index(table, "/ ") {
		t.Fatalf("storage ordering/label/deduplication: %s", table)
	}
}

func TestHighDiskUsageDoesNotWarnAtExactlyThreshold(t *testing.T) {
	used, total := int64(85), int64(100)
	if alert, ok := highDiskUsage(nil, &used, &total); ok {
		t.Fatalf("unexpected alert: %+v", alert)
	}
}
