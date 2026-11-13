//go:build darwin || linux || windows

package t234

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// Legacy images replace the gadget between disks. Linux needs a complete USB
// disconnect to refresh the cached SCSI identity: medium eject alone can miss
// the brief empty interval and retain the rootfs vendor on the status package.
func (s *Stage2) releaseLegacy(ctx context.Context, disk UMSDisk) error {
	fmt.Fprintf(s.Out, "  releasing %s (legacy handoff)\n", disk.DevPath)
	if err := s.RunHelper(ctx, HelperRequest{LegacyEject: true, Writer: WriterOptions{Device: disk.DevPath}}, nil); err != nil {
		fmt.Fprintf(s.Out, "  warning: legacy release failed (%v); checking whether the disk disconnected\n", err)
	}
	// A command can fail after it removed the device. Observe the handoff
	// before the fallback, so it cannot disconnect the next stage's gadget.
	gone, err := s.waitForLegacyDiskGone(ctx, disk)
	if err != nil || gone {
		return err
	}
	fmt.Fprintf(s.Out, "  forcing a legacy USB disconnect for %s\n", disk.DevPath)
	if err := s.RunHelper(ctx, HelperRequest{Release: true, ReleaseSerial: disk.Serial, ReleasePort: disk.PortPath}, nil); err != nil {
		return fmt.Errorf("releasing %s: %w", disk.DevPath, err)
	}
	gone, err = s.waitForLegacyDiskGone(ctx, disk)
	if err != nil {
		return err
	}
	if !gone {
		return fmt.Errorf("%s did not disconnect after legacy release", disk.DevPath)
	}
	return nil
}

var scanUMSLUNs = listUMSLUNs

// Include empty LUNs: ejecting media is not evidence that the legacy gadget
// has disconnected, and a lingering node must not count as final status.
func (s *Stage2) waitForLegacyDiskGone(ctx context.Context, disk UMSDisk) (bool, error) {
	deadline := time.Now().Add(disappearWait)
	for time.Now().Before(deadline) {
		disks, err := scanUMSLUNs()
		gone := err == nil
		for _, d := range disks {
			if d.DevPath == disk.DevPath && d.Vendor == disk.Vendor && d.PortPath == disk.PortPath && strings.EqualFold(d.Serial, disk.Serial) {
				gone = false
			}
		}
		if gone {
			return true, nil
		}
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-time.After(umsPollInterval):
		}
	}
	return false, nil
}
