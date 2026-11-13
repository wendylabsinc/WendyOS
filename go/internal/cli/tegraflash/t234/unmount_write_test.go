//go:build darwin || linux || windows

package t234

import (
	"context"
	"errors"
	"io"
	"testing"
)

func TestRootfsWriteRefusesBusyMount(t *testing.T) {
	withFastUMSPoll(t)
	withPollingMissing(t, false)
	disk := UMSDisk{DevPath: "/dev/sdb", RawPath: "/dev/sdb", Vendor: RootfsLUNVendor, PortPath: "1-3", Serial: "12345678", SizeBytes: 64 << 30}
	withUMSScan(t, func() ([]UMSDisk, error) { return []UMSDisk{disk}, nil })
	busy := errors.New("config partition is busy")
	unmounts := 0
	stage := &Stage2{Out: io.Discard, USBMode: USBModeSingle, PortPath: disk.PortPath, Session: disk.Serial, Plan: &Plan{RootfsDevice: "mmcblk0"},
		RunHelper: func(_ context.Context, req HelperRequest, _ func(int64, int64)) error {
			if req.Unmount {
				unmounts++
				return busy
			}
			if req.Writer.WritePlan || req.Eject || req.Release || req.LegacyEject {
				t.Fatal("wrote or released a disk with a busy mounted partition")
			}
			return nil
		},
	}
	if err := stage.WriteRootfsDevice(context.Background()); !errors.Is(err, busy) || unmounts != 1 {
		t.Fatalf("write = %v, unmounts = %d", err, unmounts)
	}
}
