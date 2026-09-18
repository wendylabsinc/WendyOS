package commands

import (
	"testing"

	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

func TestContainerStorageDegradedExplicitField(t *testing.T) {
	trueVal, falseVal := true, false

	// Explicit true is honoured even when derivation would disagree (no
	// WendyOS signal, no /data partition — derivation would say false).
	resp := &agentpb.GetAgentVersionResponse{
		ContainerStorageDegraded: &trueVal,
	}
	if !containerStorageDegraded(resp) {
		t.Fatal("explicit true field must be honoured")
	}

	// Explicit false is honoured even when derivation would disagree (a
	// WendyOS device whose container storage is on "/" with no "/data"
	// partition — derivation would say false anyway here, so make
	// derivation say true by adding a /data partition, and confirm the
	// explicit false still wins).
	osVersion := "WendyOS-1.0.0"
	resp2 := &agentpb.GetAgentVersionResponse{
		ContainerStorageDegraded: &falseVal,
		OsVersion:                &osVersion,
		ContainerStorage:         &agentpb.DiskPartition{Mountpoint: "/"},
		Partitions: []*agentpb.DiskPartition{
			{Mountpoint: "/data"},
		},
	}
	if containerStorageDegraded(resp2) {
		t.Fatal("explicit false field must be honoured even when derivation would say true")
	}
}

func TestContainerStorageDegradedDerivedForOldAgent(t *testing.T) {
	osVersion := "WendyOS-1.0.0"
	resp := &agentpb.GetAgentVersionResponse{
		OsVersion:        &osVersion,
		ContainerStorage: &agentpb.DiskPartition{Mountpoint: "/", Device: "/dev/nvme0n1p1"},
		Partitions: []*agentpb.DiskPartition{
			{Mountpoint: "/", Device: "/dev/nvme0n1p1"},
			{Mountpoint: "/data", Device: "/dev/nvme0n1p2"},
		},
	}
	if !containerStorageDegraded(resp) {
		t.Fatal("nil field on a WendyOS agent with container storage on / and a /data partition must derive to degraded")
	}
}

func TestContainerStorageDegradedNoopOffWendyOS(t *testing.T) {
	resp := &agentpb.GetAgentVersionResponse{
		Os:               "linux",
		ContainerStorage: &agentpb.DiskPartition{Mountpoint: "/", Device: "/dev/sda1"},
		Partitions: []*agentpb.DiskPartition{
			{Mountpoint: "/", Device: "/dev/sda1"},
			{Mountpoint: "/data", Device: "/dev/sda2"},
		},
	}
	if containerStorageDegraded(resp) {
		t.Fatal("a non-WendyOS agent (e.g. plain Ubuntu) must never be considered degraded")
	}
}

func TestContainerStorageDegradedNoopWithoutDataPartition(t *testing.T) {
	osVersion := "WendyOS-1.0.0"
	resp := &agentpb.GetAgentVersionResponse{
		OsVersion:        &osVersion,
		ContainerStorage: &agentpb.DiskPartition{Mountpoint: "/", Device: "/dev/mmcblk0p2"},
		Partitions: []*agentpb.DiskPartition{
			{Mountpoint: "/", Device: "/dev/mmcblk0p2"},
			{Mountpoint: "/boot", Device: "/dev/mmcblk0p1"},
		},
	}
	if containerStorageDegraded(resp) {
		t.Fatal("without a /data partition there is nothing to fail over to; must not be considered degraded")
	}
}

func TestPreflightContainerStorageError(t *testing.T) {
	degraded := true
	resp := &agentpb.GetAgentVersionResponse{
		ContainerStorageDegraded: &degraded,
		ContainerStorage: &agentpb.DiskPartition{
			Mountpoint: "/",
			Device:     "/dev/nvme0n1p1",
			UsedBytes:  92_000_000_000,
			TotalBytes: 100_000_000_000,
		},
	}
	err := preflightContainerStorage(resp)
	if err == nil {
		t.Fatal("expected an error when container storage is degraded")
	}
	want := "Deploy refused: container storage on this device is on the OS root slot (/ on /dev/nvme0n1p1, 92 GB of 100 GB used) because the /data bind mount is not active. Power-cycle the device; if 'wendy device info' still shows container storage on /, the OS did not mount /data (WDY-3127)."
	if got := err.Error(); got != want {
		t.Fatalf("preflightContainerStorage error =\n%q\nwant\n%q", got, want)
	}

	healthy := false
	healthyResp := &agentpb.GetAgentVersionResponse{ContainerStorageDegraded: &healthy}
	if err := preflightContainerStorage(healthyResp); err != nil {
		t.Fatalf("expected nil error for a healthy device, got %v", err)
	}
}

// TestPreflightContainerStorageErrorWithoutPartitionDetails covers the
// PREFLIGHT_ERR variant that drops the "(/ on <dev>, <used> of <total>
// used)" parenthetical when partition details are unavailable: no
// ContainerStorage partition at all, and a ContainerStorage partition
// reported with TotalBytes == 0 (disk usage inspection failed).
func TestPreflightContainerStorageErrorWithoutPartitionDetails(t *testing.T) {
	degraded := true
	want := "Deploy refused: container storage on this device is on the OS root slot because the /data bind mount is not active. Power-cycle the device; if 'wendy device info' still shows container storage on /, the OS did not mount /data (WDY-3127)."

	cases := []struct {
		name string
		resp *agentpb.GetAgentVersionResponse
	}{
		{
			name: "no container storage partition reported",
			resp: &agentpb.GetAgentVersionResponse{
				ContainerStorageDegraded: &degraded,
			},
		},
		{
			name: "container storage partition reported with zero total bytes",
			resp: &agentpb.GetAgentVersionResponse{
				ContainerStorageDegraded: &degraded,
				ContainerStorage: &agentpb.DiskPartition{
					Mountpoint: "/",
					Device:     "/dev/nvme0n1p1",
					TotalBytes: 0,
				},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := preflightContainerStorage(tc.resp)
			if err == nil {
				t.Fatal("expected an error when container storage is degraded")
			}
			if got := err.Error(); got != want {
				t.Fatalf("preflightContainerStorage error =\n%q\nwant\n%q", got, want)
			}
		})
	}
}
