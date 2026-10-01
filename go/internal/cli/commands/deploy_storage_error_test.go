package commands

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

// TestIsDeviceOutOfSpaceRequiresDeviceMarker asserts isDeviceOutOfSpace
// requires a device-side marker in addition to the bare "no space left on
// device" text (WDY-3127 I1): containerd's pusher renders any non-2xx
// registry response as an "unexpected status ..." line without the body, so
// the only way "no space left on device" ever reaches the CLI verbatim is
// via an agent gRPC description or a registry push URL shape. A LOCAL
// BuildKit worker's own full disk (a bare buildx "failed to solve" error)
// must NOT be misattributed to the device.
func TestIsDeviceOutOfSpaceRequiresDeviceMarker(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "agent gRPC desc form",
			err:  errors.New("rpc error: code = ResourceExhausted desc = write /var/lib/containerd/io.containerd.metadata.v1.bolt/meta.db: no space left on device"),
			want: true,
		},
		{
			name: "registry push form",
			err:  errors.New("pushing OCI layout to registry: POST https://host/v2/x/blobs/uploads/: UNKNOWN: write /var/lib/containerd/io.containerd.metadata.v1.bolt/meta.db: no space left on device"),
			want: true,
		},
		{
			name: "registry push URL shape without the 'pushing OCI layout to registry' prefix",
			err:  errors.New("failed to push host.docker.internal:50342/app:latest: failed to do request: POST https://host/v2/x/blobs/uploads/: write /var/lib/containerd/io.containerd.metadata.v1.bolt/meta.db: no space left on device"),
			want: true,
		},
		{
			name: "bare buildx local build failure",
			err:  errors.New(`failed to solve: process "/bin/sh -c go build ./..." did not complete successfully: write /tmp/go-build/foo: no space left on device`),
			want: false,
		},
		{
			name: "no ENOSPC text at all",
			err:  errors.New("dial tcp: connection refused"),
			want: false,
		},
		{
			name: "nil error",
			err:  nil,
			want: false,
		},
	}
	for _, tc := range cases {
		if got := isDeviceOutOfSpace(tc.err); got != tc.want {
			t.Errorf("%s: isDeviceOutOfSpace(%v) = %v, want %v", tc.name, tc.err, got, tc.want)
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

// TestIsContainerStorageDegradedErrorMatches507 asserts the registry's HTTP
// 507 Insufficient Storage status is recognised even when containerd's
// pusher has stripped everything else (no WENDY_STORAGE_DEGRADED code, no
// (WDY-3127) marker) from the fused buildx build+push path (WDY-3127 I2).
func TestIsContainerStorageDegradedErrorMatches507(t *testing.T) {
	err := errors.New(`failed to push host.docker.internal:50342/app:latest: failed to do request: POST https://host/v2/x/blobs/uploads/: unexpected status from POST request to http://host.docker.internal:50342/v2/x/blobs/uploads/: 507 Insufficient Storage`)
	if !isContainerStorageDegradedError(err) {
		t.Fatalf("isContainerStorageDegradedError(%v) = false, want true for a 507 Insufficient Storage response", err)
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

// TestShouldRetryPushDoesNotRetryInsufficientStorage asserts a bare 507
// Insufficient Storage line (the shape containerd's pusher actually prints
// on the fused buildx build+push path, with no WENDY_STORAGE_DEGRADED code
// and no (WDY-3127) marker) stops the retry loop instead of matching the
// transient "failed to push" regex and being retried 3x (WDY-3127 I2).
func TestShouldRetryPushDoesNotRetryInsufficientStorage(t *testing.T) {
	out := `failed to push host.docker.internal:50342/app:latest: failed to do request: POST https://host/v2/x/blobs/uploads/: unexpected status from POST request to http://host.docker.internal:50342/v2/x/blobs/uploads/: 507 Insufficient Storage`
	if shouldRetryPush(nil, 1, out, nil) {
		t.Fatal("shouldRetryPush must not retry a 507 Insufficient Storage failure")
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
			"Free space with 'wendy device cache prune', then rerun. " +
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
			"Free space with 'wendy device cache prune', then rerun. " +
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
		origErr := errors.New("rpc error: code = ResourceExhausted desc = write /var/lib/containerd/io.containerd.metadata.v1.bolt/meta.db: no space left on device")
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
		// The error text already carries the agent's own (WDY-3127) marker
		// (a gRPC FailedPrecondition description), so describeDeployStorageFailure
		// must not prepend its own copy of the remedy (M4) — it just marks
		// the deploy refused.
		origErr := errors.New("rpc error: code = FailedPrecondition desc = container storage is on the OS root slot (WDY-3127)")
		got := describeDeployStorageFailure(origErr, nil)
		want := "deploy refused: " + origErr.Error()
		if got.Error() != want {
			t.Errorf("got  %q\nwant %q", got.Error(), want)
		}
		if !errors.Is(got, origErr) {
			t.Error("describeDeployStorageFailure must wrap the original error (errors.Is)")
		}
	})

	t.Run("507 Insufficient Storage, no remedy text survived the pusher", func(t *testing.T) {
		// Unlike the FailedPrecondition text above, containerd's pusher has
		// stripped everything but the bare HTTP status line, so there is no
		// remedy text to avoid duplicating: this must get the full CLI
		// DEGRADED message, not a bare "deploy refused: " wrap (M4).
		origErr := errors.New(`failed to push host.docker.internal:50342/app:latest: failed to do request: POST https://host/v2/x/blobs/uploads/: unexpected status from POST request to http://host.docker.internal:50342/v2/x/blobs/uploads/: 507 Insufficient Storage`)
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

// TestDescribeDeployStorageFailureDoesNotDuplicateAgentRemedy asserts the
// "don't duplicate the remedy" rule (M4) is scoped to exactly the case where
// err's OWN text already carries it: the (WDY-3127) marker, which only the
// agent's gRPC FailedPrecondition description ends every message with. The
// bare WENDY_STORAGE_DEGRADED code (no message) and bare 507 Insufficient
// Storage forms carry no remedy text at all — containerd's pusher strips
// everything else on the fused buildx build+push path — so those must still
// get the full CLI DEGRADED message; only THAT copy's remedy should appear,
// exactly once.
func TestDescribeDeployStorageFailureDoesNotDuplicateAgentRemedy(t *testing.T) {
	const remedySnippet = "Power-cycle the device"

	t.Run("ticket marker form: err's own text already has the remedy", func(t *testing.T) {
		origErr := fmt.Errorf("creating container: %w", errors.New(
			"rpc error: code = FailedPrecondition desc = container storage is on the OS root slot: /var/lib/containerd resolves to / because the /data bind mount is not active; refusing to ingest images so the root filesystem cannot fill up. "+
				"Power-cycle the device; if 'wendy device info' still shows container storage on /, the OS did not mount /data (WDY-3127)."))

		got := describeDeployStorageFailure(origErr, nil)

		if count := strings.Count(got.Error(), remedySnippet); count != 1 {
			t.Fatalf("output contains %q %d times, want exactly once:\n%s", remedySnippet, count, got.Error())
		}
		want := "deploy refused: " + origErr.Error()
		if got.Error() != want {
			t.Errorf("got  %q\nwant %q (the CLI must not prepend its own remedy copy)", got.Error(), want)
		}
		if !errors.Is(got, origErr) {
			t.Error("describeDeployStorageFailure must wrap the original error (errors.Is)")
		}
	})

	t.Run("bare 507 form: no remedy text survived the pusher", func(t *testing.T) {
		origErr := errors.New(`failed to push host.docker.internal:50342/app:latest: failed to do request: POST https://host/v2/x/blobs/uploads/: unexpected status from POST request to http://host.docker.internal:50342/v2/x/blobs/uploads/: 507 Insufficient Storage`)

		got := describeDeployStorageFailure(origErr, nil)

		if count := strings.Count(got.Error(), remedySnippet); count != 1 {
			t.Fatalf("output contains %q %d times, want exactly once (from the CLI's own copy):\n%s", remedySnippet, count, got.Error())
		}
		want := (&containerStorageDegradedError{}).Error() + " Original error: " + origErr.Error()
		if got.Error() != want {
			t.Errorf("got  %q\nwant %q", got.Error(), want)
		}
		if !errors.Is(got, origErr) {
			t.Error("describeDeployStorageFailure must wrap the original error (errors.Is)")
		}
	})

	t.Run("bare WENDY_STORAGE_DEGRADED code, no message: no remedy text survived either", func(t *testing.T) {
		origErr := errors.New(`failed to push: failed to do request: POST https://host/v2/x/blobs/uploads/: WENDY_STORAGE_DEGRADED: container storage is degraded`)

		got := describeDeployStorageFailure(origErr, nil)

		if count := strings.Count(got.Error(), remedySnippet); count != 1 {
			t.Fatalf("output contains %q %d times, want exactly once (from the CLI's own copy):\n%s", remedySnippet, count, got.Error())
		}
		want := (&containerStorageDegradedError{}).Error() + " Original error: " + origErr.Error()
		if got.Error() != want {
			t.Errorf("got  %q\nwant %q", got.Error(), want)
		}
		if !errors.Is(got, origErr) {
			t.Error("describeDeployStorageFailure must wrap the original error (errors.Is)")
		}
	})
}

// TestDescribeDeployStorageFailureLeavesLocalBuildFailureAlone asserts a
// local build error mentioning ENOSPC (a full LOCAL BuildKit worker disk,
// not the device's) is returned as the identical value, since it carries no
// device-side marker (WDY-3127 I1).
func TestDescribeDeployStorageFailureLeavesLocalBuildFailureAlone(t *testing.T) {
	origErr := errors.New(`failed to solve: process "/bin/sh -c go build ./..." did not complete successfully: write /var/lib/docker/tmp/buildkit/foo: no space left on device`)
	resp := &agentpb.GetAgentVersionResponse{
		ContainerStorage: &agentpb.DiskPartition{
			Mountpoint: "/data",
			Device:     "/dev/nvme0n1p2",
			UsedBytes:  19_500_000_000,
			TotalBytes: 20_000_000_000,
		},
	}
	got := describeDeployStorageFailure(origErr, resp)
	if got != origErr {
		t.Errorf("describeDeployStorageFailure must leave a local build ENOSPC error unchanged: got %v, want the identical value %v", got, origErr)
	}
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
