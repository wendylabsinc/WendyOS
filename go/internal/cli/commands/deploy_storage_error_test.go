package commands

import (
	"errors"
	"fmt"
	"testing"

	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

func TestIsDeviceOutOfSpace(t *testing.T) {
	outOfSpace := []error{
		errors.New("pushing OCI layout to registry: POST https://host/v2/x/blobs/uploads/: UNKNOWN: write /var/lib/containerd/io.containerd.metadata.v1.bolt/meta.db: no space left on device"),
		fmt.Errorf("creating container: %w", errors.New("write /data/foo: no space left on device")),
	}
	for _, err := range outOfSpace {
		if !isDeviceOutOfSpace(err) {
			t.Errorf("isDeviceOutOfSpace(%q) = false, want true", err)
		}
	}

	notOutOfSpace := []error{
		nil,
		errors.New("ERROR: failed to solve: process \"/bin/sh -c go build\" did not complete successfully: exit code: 1"),
		errors.New("dial tcp: connection refused"),
	}
	for _, err := range notOutOfSpace {
		if isDeviceOutOfSpace(err) {
			t.Errorf("isDeviceOutOfSpace(%v) = true, want false", err)
		}
	}
}

func TestIsContainerStorageDegradedErrorMatchesRegistryCode(t *testing.T) {
	cases := []struct {
		name string
		err  error
	}{
		{
			name: "registry HTTP 507 code as rendered by go-containerregistry/buildx",
			err:  errors.New(`failed to push: failed to do request: POST https://host/v2/x/blobs/uploads/: WENDY_STORAGE_DEGRADED: container storage is degraded`),
		},
		{
			name: "agent gRPC FailedPrecondition text",
			err:  errors.New("rpc error: code = FailedPrecondition desc = container storage is on the OS root slot (WDY-3127)"),
		},
		{
			name: "the CLI's own typed preflight error",
			err:  &containerStorageDegradedError{},
		},
		{
			name: "the typed preflight error wrapped by another error",
			err:  fmt.Errorf("creating container: %w", &containerStorageDegradedError{}),
		},
	}
	for _, tt := range cases {
		if !isContainerStorageDegradedError(tt.err) {
			t.Errorf("%s: isContainerStorageDegradedError(%v) = false, want true", tt.name, tt.err)
		}
	}

	notDegraded := []error{
		nil,
		errors.New("write /data/foo: no space left on device"),
		errors.New("dial tcp: connection refused"),
	}
	for _, err := range notDegraded {
		if isContainerStorageDegradedError(err) {
			t.Errorf("isContainerStorageDegradedError(%v) = true, want false", err)
		}
	}
}

func TestShouldRetryPushDoesNotRetryOutOfSpace(t *testing.T) {
	out := `pushing OCI layout to registry: POST https://host/v2/x/blobs/uploads/: UNKNOWN: write /var/lib/containerd/io.containerd.metadata.v1.bolt/meta.db: no space left on device`
	if shouldRetryPush(nil, 1, out, nil) {
		t.Fatal("shouldRetryPush must not retry an out-of-space failure")
	}
}

func TestShouldRetryPushDoesNotRetryStorageDegraded(t *testing.T) {
	cases := []string{
		`failed to push host.docker.internal:50342/app:latest: failed to do request: POST .../blobs/uploads/: WENDY_STORAGE_DEGRADED: container storage is degraded`,
		`rpc error: code = FailedPrecondition desc = container storage is on the OS root slot (WDY-3127)`,
	}
	for _, out := range cases {
		if shouldRetryPush(nil, 1, out, nil) {
			t.Errorf("shouldRetryPush(%q) = true, want false", out)
		}
	}
}

func TestDescribeDeployStorageFailureNamesPartitionAndPruneRemedy(t *testing.T) {
	origErr := errors.New("pushing OCI layout to registry: POST https://host/v2/x/blobs/uploads/: UNKNOWN: write /var/lib/containerd/io.containerd.metadata.v1.bolt/meta.db: no space left on device")

	t.Run("known partition", func(t *testing.T) {
		resp := &agentpb.GetAgentVersionResponse{
			ContainerStorage: &agentpb.DiskPartition{
				Mountpoint: "/data",
				Device:     "/dev/nvme0n1p2",
				UsedBytes:  19_500_000_000,
				TotalBytes: 20_000_000_000,
			},
		}
		got := describeDeployStorageFailure(origErr, resp)
		want := "The device's container storage is full: /data (/dev/nvme0n1p2) 19.5 GB of 20 GB used. " +
			"Free space with 'wendy device cache prune --all' (releases cached layers not used by any deployed app; deployed apps are kept), then rerun. " +
			"Original error: " + origErr.Error()
		if got.Error() != want {
			t.Errorf("got  %q\nwant %q", got.Error(), want)
		}
		if !errors.Is(got, origErr) {
			t.Error("describeDeployStorageFailure must wrap the original error (errors.Is)")
		}
	})

	t.Run("unknown partition", func(t *testing.T) {
		got := describeDeployStorageFailure(origErr, nil)
		want := "The device's container storage is full. " +
			"Free space with 'wendy device cache prune --all' (releases cached layers not used by any deployed app; deployed apps are kept), then rerun. " +
			"Original error: " + origErr.Error()
		if got.Error() != want {
			t.Errorf("got  %q\nwant %q", got.Error(), want)
		}
		if !errors.Is(got, origErr) {
			t.Error("describeDeployStorageFailure must wrap the original error (errors.Is)")
		}
	})
}

func TestDescribeDeployStorageFailureOnRootSlotRecommendsReboot(t *testing.T) {
	t.Run("ENOSPC on a degraded device", func(t *testing.T) {
		origErr := errors.New("write /var/lib/containerd/io.containerd.metadata.v1.bolt/meta.db: no space left on device")
		degradedTrue := true
		resp := &agentpb.GetAgentVersionResponse{
			ContainerStorageDegraded: &degradedTrue,
			ContainerStorage: &agentpb.DiskPartition{
				Mountpoint: "/",
				Device:     "/dev/nvme0n1p1",
				UsedBytes:  9_000_000_000,
				TotalBytes: 10_000_000_000,
			},
		}
		got := describeDeployStorageFailure(origErr, resp)
		want := (&containerStorageDegradedError{storage: resp.GetContainerStorage()}).Error() +
			" Original error: " + origErr.Error()
		if got.Error() != want {
			t.Errorf("got  %q\nwant %q", got.Error(), want)
		}
		if !errors.Is(got, origErr) {
			t.Error("describeDeployStorageFailure must wrap the original error (errors.Is)")
		}
	})

	t.Run("bare storage-degraded error, no ENOSPC text", func(t *testing.T) {
		origErr := errors.New("rpc error: code = FailedPrecondition desc = container storage is on the OS root slot (WDY-3127)")
		got := describeDeployStorageFailure(origErr, nil)
		want := (&containerStorageDegradedError{}).Error() + " Original error: " + origErr.Error()
		if got.Error() != want {
			t.Errorf("got  %q\nwant %q", got.Error(), want)
		}
		if !errors.Is(got, origErr) {
			t.Error("describeDeployStorageFailure must wrap the original error (errors.Is)")
		}
	})
}

func TestDescribeDeployStorageFailurePassesThroughUnrelatedErrors(t *testing.T) {
	origErr := errors.New("dial tcp 192.168.1.20:5555: connect: connection refused")
	got := describeDeployStorageFailure(origErr, nil)
	if got != origErr {
		t.Errorf("describeDeployStorageFailure must return unrelated errors unchanged: got %v, want %v", got, origErr)
	}
	if !errors.Is(got, origErr) {
		t.Error("errors.Is must hold for a passed-through error")
	}

	if describeDeployStorageFailure(nil, nil) != nil {
		t.Error("describeDeployStorageFailure(nil, ...) must return nil")
	}
}
