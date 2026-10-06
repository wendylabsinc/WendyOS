//go:build darwin || linux

package commands

// macOS/Linux USB hooks for the shared Thor install flow: recovery-device
// enumeration and stage-1 RCM boot use gousb (packages rcm/bringup); the stage-2
// ADB transport is the serverless gousb adb client, which drives the shared
// flashengine — no more python bootburn, adb shim, or PyYAML.

import (
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"golang.org/x/sys/unix"

	"github.com/wendylabsinc/wendy/go/internal/cli/tegraflash/adb"
	"github.com/wendylabsinc/wendy/go/internal/cli/tegraflash/bringup"
	"github.com/wendylabsinc/wendy/go/internal/cli/tegraflash/flashengine"
	"github.com/wendylabsinc/wendy/go/internal/cli/tegraflash/flashpack"
	"github.com/wendylabsinc/wendy/go/internal/cli/tegraflash/rcm"
	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
)

// thorPrepareHost stops a conflicting adb server that would claim the gadget.
func thorPrepareHost(out io.Writer) error {
	if stopConflictingADBServer() {
		fmt.Fprintln(out, tui.InfoMessage("Stopped a running adb server (it would hold the Thor's flashing gadget)."))
	}
	return nil
}

// pickThorRecoveryDevice lists Jetsons in recovery mode and selects one, with a
// rescan loop and USB-access guidance.
func pickThorRecoveryDevice() (thorDevice, error) {
	dev, err := pickUnixRecoveryDevice(thorRecoveryHints(), func(d rcm.RecoveryDevice) bool { return d.IsThor() })
	if err != nil {
		return thorDevice{}, err
	}
	return thorDevice{PathKey: dev.PathKey, Label: dev.Describe()}, nil
}

// pickUnixRecoveryDevice selects only devices matching the requested family.
// Filtering happens before the single-device fast path and on every rescan, so
// an attached Orin can never be selected by a Thor installation (or vice versa).
func pickUnixRecoveryDevice(hints recoveryWaitHints, match func(rcm.RecoveryDevice) bool) (rcm.RecoveryDevice, error) {
	scan := func() ([]rcm.RecoveryDevice, error) {
		devs, err := rcm.ListRecoveryDevices()
		return filterRecoveryDevices(devs, match), err
	}
	for {
		devs, err := scan()
		switch {
		case errors.Is(err, rcm.ErrUSBAccess) && len(devs) == 0:
			fmt.Println("\n" + usbAccessHintBox())
			fmt.Print("Press Enter to rescan, or 'q' to quit: ")
			if readQuit() {
				return rcm.RecoveryDevice{}, fmt.Errorf("cannot open the Jetson's USB device: permission denied")
			}
			continue
		case errors.Is(err, rcm.ErrUSBAccess):
			fmt.Println()
			fmt.Println(tui.WarningMessage("Another Jetson in recovery mode was detected but couldn't be opened (USB access denied) — it is NOT listed below."))
		case err != nil:
			return rcm.RecoveryDevice{}, err
		}
		if len(devs) == 0 {
			// The user already confirmed the board is in recovery mode, so an empty
			// scan usually means cabling or the button sequence needs another try.
			// Wait passively (spinner) until a device appears or the user quits.
			if devs, err = waitForRecovery(hints, scan); err != nil {
				return rcm.RecoveryDevice{}, err
			}
		}
		switch len(devs) {
		case 1:
			return devs[0], nil
		default:
			var items []tui.PickerItem
			byKey := make(map[string]rcm.RecoveryDevice, len(devs))
			for _, d := range devs {
				byKey[d.PathKey] = d
				items = append(items, tui.PickerItem{
					Name:    d.Describe(),
					Section: "Recovery devices",
					SortKey: d.PathKey,
					Value:   d.PathKey,
				})
			}
			sel, err := pickFromItems("Select the "+hints.label+" to flash", items)
			if err != nil {
				return rcm.RecoveryDevice{}, err
			}
			return byKey[sel], nil
		}
	}
}

func filterRecoveryDevices(devs []rcm.RecoveryDevice, match func(rcm.RecoveryDevice) bool) []rcm.RecoveryDevice {
	filtered := make([]rcm.RecoveryDevice, 0, len(devs))
	for _, d := range devs {
		if match(d) {
			filtered = append(filtered, d)
		}
	}
	return filtered
}

// thorIsUSBAccessErr reports whether err is the OS denying wendy access to the
// Jetson's USB device, which routes into usbAccessHintBox. This covers both the
// recovery-device scan (rcm.ErrUSBAccess) and opening the re-enumerated flashing
// gadget (adb.ErrUSBAccess) — the gadget has its own PID, so a udev rule covering
// only the recovery PIDs fails here even after a clean stage 1.
func thorIsUSBAccessErr(err error) bool {
	return errors.Is(err, rcm.ErrUSBAccess) || errors.Is(err, adb.ErrUSBAccess)
}

// diskAvailBytes reports the bytes available to the user on the volume holding
// path. ok=false when statfs fails or reports implausible values (buggy FUSE
// drivers), in which case the disk-space preflight is skipped.
func diskAvailBytes(path string) (avail int64, ok bool) {
	var stat unix.Statfs_t
	if err := unix.Statfs(path, &stat); err != nil {
		return 0, false
	}
	if stat.Bsize <= 0 {
		return 0, false // implausible block size: can't size the disk safely
	}
	avail = int64(stat.Bavail) * int64(stat.Bsize)
	if avail < 0 {
		return 0, false
	}
	return avail, true
}

// thorStageOne performs the stage-1 RCM boot over gousb.
func thorStageOne(fp *flashpack.Flashpack, dev thorDevice, out io.Writer) error {
	return bringup.Run(bringup.Options{
		Dir:             fp.Stage1Dir(),
		MemBCT:          fp.MemBCT(),
		DevicePath:      dev.PathKey,
		ExpectedProduct: uint16(rcm.ProductThor),
		ExpectedECID:    dev.ExpectedECID,
		SendOrder:       fp.Manifest.Stage1SendOrder,
		Out:             out,
	})
}

// thorOpenGadget opens the initrd-flash ADB gadget as a stage-2 transport,
// pinning the selected device via WENDY_ADB_PATH and retrying while it
// re-enumerates after stage-1.
func thorOpenGadget(dev thorDevice, out io.Writer) (flashengine.Transport, func(), error) {
	if dev.PathKey != "" {
		os.Setenv("WENDY_ADB_PATH", dev.PathKey)
	}
	deadline := time.Now().Add(60 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		var d *adb.Device
		var err error
		if dev.ExpectedECID != "" {
			d, err = adb.OpenAt(dev.PathKey)
		} else {
			d, err = adb.Open()
		}
		if err == nil {
			return d, func() { d.Close() }, nil
		}
		lastErr = err
		time.Sleep(time.Second)
	}
	// Nothing was written yet either way. A permission denial gets the USB-access
	// guidance (udev rule / sudo); anything else is the calmer "gadget unreachable".
	if errors.Is(lastErr, adb.ErrUSBAccess) {
		return nil, nil, lastErr
	}
	return nil, nil, fmt.Errorf("%w (over ADB): %v", errGadgetUnreachable, lastErr)
}
