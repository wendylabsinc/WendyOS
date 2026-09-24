//go:build darwin || linux || windows

package t234

import (
	"context"
	"errors"
	"io"
	"slices"
	"testing"
	"time"
)

func withPollingMissing(t *testing.T, missing bool) {
	t.Helper()
	previous := pollingMissing
	pollingMissing = func(string) bool { return missing }
	t.Cleanup(func() { pollingMissing = previous })
}

func withFastEjectRetry(t *testing.T) {
	t.Helper()
	previousWait, previousDelay := disappearWait, ejectRetryDelay
	disappearWait, ejectRetryDelay = 10*time.Millisecond, 0
	t.Cleanup(func() { disappearWait, ejectRetryDelay = previousWait, previousDelay })
}

// The initrd only moves on after an eject, so a failed one is retried after
// another unmount (a desktop may have re-mounted a partition) before giving up.
func TestReleaseRetriesEjectAfterUnmount(t *testing.T) {
	withFastUMSPoll(t)
	withFastEjectRetry(t)
	disk := UMSDisk{DevPath: "/dev/sdb", Vendor: RootfsLUNVendor, PortPath: "1-3", Serial: "12345678"}
	ejected := false
	withUMSScan(t, func() ([]UMSDisk, error) {
		if ejected {
			return nil, nil
		}
		return []UMSDisk{disk}, nil
	})
	var ops []string
	stage := &Stage2{Out: io.Discard,
		RunHelper: func(_ context.Context, req HelperRequest, _ func(int64, int64)) error {
			switch {
			case req.Unmount:
				ops = append(ops, "unmount")
			case req.Eject:
				ops = append(ops, "eject")
				if len(ops) == 1 {
					return errors.New("umount: target is busy")
				}
				ejected = true
			}
			return nil
		},
	}
	if err := stage.release(context.Background(), disk); err != nil {
		t.Fatalf("release = %v", err)
	}
	if want := []string{"eject", "unmount", "eject"}; !slices.Equal(ops, want) {
		t.Fatalf("helper ops = %v, want %v", ops, want)
	}
}

func TestReleaseFailsWhenEjectKeepsFailing(t *testing.T) {
	withFastUMSPoll(t)
	withFastEjectRetry(t)
	disk := UMSDisk{DevPath: "/dev/sdb", Vendor: RootfsLUNVendor, PortPath: "1-3", Serial: "12345678"}
	withUMSScan(t, func() ([]UMSDisk, error) { return []UMSDisk{disk}, nil })
	ejects := 0
	stage := &Stage2{Out: io.Discard,
		RunHelper: func(_ context.Context, req HelperRequest, _ func(int64, int64)) error {
			if req.Eject {
				ejects++
				return errors.New("eject: unable to eject")
			}
			return nil
		},
	}
	if err := stage.release(context.Background(), disk); err == nil || ejects != ejectAttempts {
		t.Fatalf("release = %v after %d ejects, want an error after %d", err, ejects, ejectAttempts)
	}
}

// A LUN still showing its medium after a successful eject is stale; treating
// it as released would let the next wait mistake it for the device's status.
func TestReleaseFailsWhenMediumStays(t *testing.T) {
	withFastUMSPoll(t)
	withFastEjectRetry(t)
	disk := UMSDisk{DevPath: "/dev/sda", Vendor: FlashpkgVendor, PortPath: "1-3", Serial: "12345678"}
	withUMSScan(t, func() ([]UMSDisk, error) { return []UMSDisk{disk}, nil })
	stage := &Stage2{Out: io.Discard,
		RunHelper: func(context.Context, HelperRequest, func(int64, int64)) error { return nil },
	}
	if err := stage.release(context.Background(), disk); err == nil {
		t.Fatal("release succeeded although the medium never went away")
	}
}

// After a replug the gadget can come back on the connector's other root-hub
// port; its session still identifies it, and the flash follows it there.
func TestRootfsWaitFollowsReplugToOtherPort(t *testing.T) {
	withFastUMSPoll(t)
	withUMSScan(t, func() ([]UMSDisk, error) {
		return []UMSDisk{
			{DevPath: "/dev/sdc", Vendor: RootfsLUNVendor, PortPath: "2-3", Serial: "aaaaaaaa"},
			{DevPath: "/dev/sdb", Vendor: RootfsLUNVendor, PortPath: "1-3", Serial: "12345678"},
		}, nil
	})
	disk, err := waitForUMSDiskConfirmed(context.Background(), LUNSelector{Vendor: RootfsLUNVendor, PortPath: "2-3", PortHint: true, Session: "12345678"}, time.Second)
	if err != nil || disk.DevPath != "/dev/sdb" {
		t.Fatalf("wait = %+v, %v; want this session's LUN on its new port", disk, err)
	}
}

// A replugged LUN starts with host media polling off, so the waits keep
// re-applying it through Refresh.
func TestLUNWaitsRunRefresh(t *testing.T) {
	withFastUMSPoll(t)
	previous := gadgetCheckInterval
	gadgetCheckInterval = 0
	t.Cleanup(func() { gadgetCheckInterval = previous })
	scans := 0
	withUMSScan(t, func() ([]UMSDisk, error) {
		scans++
		if scans < 3 {
			return nil, nil
		}
		return []UMSDisk{{DevPath: "/dev/sdb", Vendor: RootfsLUNVendor, PortPath: "1-3", Serial: "12345678"}}, nil
	})
	refreshes := 0
	sel := LUNSelector{Vendor: RootfsLUNVendor, PortPath: "1-3", Session: "12345678", Refresh: func() { refreshes++ }}
	if _, err := waitForUMSDiskConfirmed(context.Background(), sel, time.Second); err != nil || refreshes != 3 {
		t.Fatalf("rootfs wait: err=%v refreshes=%d, want 3", err, refreshes)
	}
	scans, refreshes = 0, 0
	if _, err := WaitForUMSDiskAt(context.Background(), sel, time.Second); err != nil || refreshes != 3 {
		t.Fatalf("final-status wait: err=%v refreshes=%d, want 3", err, refreshes)
	}
}
