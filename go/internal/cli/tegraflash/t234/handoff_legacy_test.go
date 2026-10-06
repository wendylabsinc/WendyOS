//go:build darwin || linux || windows

package t234

import (
	"context"
	"errors"
	"io"
	"slices"
	"testing"
)

func TestLegacyEmptyLUNRequiresScopedDisconnect(t *testing.T) {
	withFastUMSPoll(t)
	withFastEjectRetry(t)
	disk := UMSDisk{DevPath: "/dev/sdb", Vendor: FlashpkgVendor, PortPath: "1-3", Serial: "12345678"}
	disconnected := false
	// The empty LUN still exists after eject; only release withdraws it.
	withUMSScan(t, func() ([]UMSDisk, error) {
		if disconnected {
			return nil, nil
		}
		return []UMSDisk{disk}, nil
	})
	releases := 0
	stage := Stage2{USBMode: USBModeLegacy, Out: io.Discard,
		RunHelper: func(_ context.Context, req HelperRequest, _ func(int64, int64)) error {
			if req.Release {
				releases++
				if req.ReleaseSerial != disk.Serial || req.ReleasePort != disk.PortPath {
					t.Fatal("disconnect was not scoped to the verified gadget")
				}
				disconnected = true
			}
			return nil
		},
	}
	if err := stage.release(context.Background(), disk); err != nil || releases != 1 {
		t.Fatalf("legacy release = %v; disconnects = %d", err, releases)
	}
}

func TestLegacyEjectErrorAfterDisconnectNeedsNoFallback(t *testing.T) {
	withFastUMSPoll(t)
	disk := UMSDisk{DevPath: "/dev/sdb", Vendor: FlashpkgVendor, PortPath: "1-3", Serial: "12345678"}
	withUMSScan(t, func() ([]UMSDisk, error) { return nil, nil })
	stage := Stage2{USBMode: USBModeLegacy, Out: io.Discard,
		RunHelper: func(_ context.Context, req HelperRequest, _ func(int64, int64)) error {
			if req.Release {
				t.Fatal("unexpected disconnect fallback")
			}
			return errors.New("power-off failed after removing the device")
		},
	}
	if err := stage.release(context.Background(), disk); err != nil {
		t.Fatal(err)
	}
}

func TestLegacyPowerOffFailureUsesDisconnectWithoutMediumEject(t *testing.T) {
	withFastUMSPoll(t)
	withFastEjectRetry(t)
	disk := UMSDisk{DevPath: "/dev/sdb", Vendor: "mmcblk0", PortPath: "1-3", Serial: "12345678"}
	disconnected := false
	withUMSScan(t, func() ([]UMSDisk, error) {
		if disconnected {
			return nil, nil
		}
		return []UMSDisk{disk}, nil
	})
	var ops []string
	stage := Stage2{USBMode: USBModeLegacy, Out: io.Discard,
		RunHelper: func(_ context.Context, req HelperRequest, _ func(int64, int64)) error {
			switch {
			case req.LegacyEject:
				ops = append(ops, "power-off")
				return errors.New("udisksctl unavailable")
			case req.Release:
				if req.ReleaseSerial != disk.Serial || req.ReleasePort != disk.PortPath {
					t.Fatal("unscoped disconnect")
				}
				ops = append(ops, "disconnect")
				disconnected = true
			default:
				t.Fatal("legacy release used a medium-only operation")
			}
			return nil
		},
	}
	if err := stage.release(context.Background(), disk); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(ops, []string{"power-off", "disconnect"}) {
		t.Fatalf("operations = %v", ops)
	}
}
