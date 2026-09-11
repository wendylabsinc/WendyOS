//go:build windows

package commands

// Windows USB hooks for the shared Thor install flow. Stage-1 RCM boot, recovery
// enumeration, and the stage-2 ADB transport all go through the WinUSB backend
// (package winusb); each stage checks its actual driver binding. Stage-2 is the
// shared flashengine, identical to macOS/Linux.

import (
	"fmt"
	"io"
	"math"
	"strings"
	"time"

	"golang.org/x/sys/windows"

	"github.com/wendylabsinc/wendy/go/internal/cli/tegraflash/flashengine"
	"github.com/wendylabsinc/wendy/go/internal/cli/tegraflash/flashpack"
	"github.com/wendylabsinc/wendy/go/internal/cli/tegraflash/winusb"
	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
)

// thorPrepareHost stops any conflicting adb server (which would claim the
// flashing gadget over USB even on Windows, via its own WinUSB handle), then
// leaves driver preparation to pickThorRecoveryDevice after target selection.
func thorPrepareHost(out io.Writer) error {
	// A running Google adb server (Android platform-tools) grabs every ADB device
	// it sees — including the Thor flashing gadget — which collides with wendy's
	// own USB access. Stop it first (no-op if none is running).
	if stopConflictingADBServer() {
		fmt.Fprintln(out, tui.InfoMessage("Stopped a running adb server (it would contend for the Thor's USB device)."))
	}

	// Driver setup follows selection below; an interface on a different board
	// cannot establish this target's driver version or gadget ID coverage.
	return nil
}

// pickThorRecoveryDevice lists NVIDIA recovery devices via SetupAPI and selects
// one, rescanning until a Thor in recovery appears.
func pickThorRecoveryDevice() (thorDevice, error) {
	scanRecovery := func() ([]winusb.Device, error) {
		devs, err := winusb.ListDevices()
		if err != nil {
			return nil, err
		}
		var recovery []winusb.Device
		for _, d := range devs {
			if d.IsThor() {
				recovery = append(recovery, d)
			}
		}
		return recovery, nil
	}
	for {
		recovery, err := scanRecovery()
		if err != nil {
			return thorDevice{}, err
		}
		if len(recovery) == 0 {
			// The user already confirmed the Thor is in recovery mode, so an empty
			// scan usually means cabling or the button sequence needs another try.
			// Wait passively (spinner) until a device appears or the user quits.
			if recovery, err = waitForRecovery(thorRecoveryHints(), scanRecovery); err != nil {
				return thorDevice{}, err
			}
		}
		var chosen winusb.Device
		switch len(recovery) {
		case 1:
			chosen = recovery[0]
		default:
			var items []tui.PickerItem
			byKey := map[string]winusb.Device{}
			for _, d := range recovery {
				byKey[d.InstanceID] = d
				items = append(items, tui.PickerItem{
					Name:    d.Describe(),
					Section: "Recovery devices",
					SortKey: d.InstanceID,
					Value:   d.InstanceID,
				})
			}
			sel, err := pickFromItems("Select the Thor to flash", items)
			if err != nil {
				return thorDevice{}, err
			}
			var ok bool
			chosen, ok = byKey[sel]
			if !ok {
				return thorDevice{}, fmt.Errorf("selected Thor is no longer present")
			}
		}
		if chosen.LocationPath == "" {
			return thorDevice{}, fmt.Errorf("cannot determine the physical USB port of %s; connect the Thor to a direct USB port", chosen.Describe())
		}
		if err := ensureUSBDriver(chosen, winusb.JetsonDriver()); err != nil {
			return thorDevice{}, err
		}
		return thorDevice{PathKey: chosen.LocationPath, Label: chosen.Describe(), Instance: chosen.InstanceID}, nil
	}
}

// thorIsUSBAccessErr reports whether err is the OS denying wendy access to the
// Jetson's USB device. On Windows access goes through our own WinUSB driver
// (checked after target selection), so there is no equivalent claim-denied error
// to classify — open failures surface through the normal stage error paths.
func thorIsUSBAccessErr(err error) bool {
	return false
}

// diskAvailBytes reports the bytes available to the user on the volume holding
// path. ok=false when the free-space query fails, in which case the disk-space
// preflight is skipped.
func diskAvailBytes(path string) (int64, bool) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, false
	}
	var availToCaller, total, totalFree uint64
	if err := windows.GetDiskFreeSpaceEx(p, &availToCaller, &total, &totalFree); err != nil {
		return 0, false
	}
	if availToCaller > math.MaxInt64 {
		availToCaller = math.MaxInt64
	}
	return int64(availToCaller), true
}

// thorStageOne performs the stage-1 RCM boot over WinUSB.
func thorStageOne(fp *flashpack.Flashpack, dev thorDevice, out io.Writer) error {
	return winusb.StageOneBoot(winusb.StageOneOptions{
		Stage1Dir:       fp.Stage1Dir(),
		MemBCT:          fp.MemBCT(),
		SendOrder:       fp.Manifest.Stage1SendOrder,
		Location:        dev.PathKey,
		Instance:        dev.Instance,
		ExpectedProduct: winusb.ProductThor,
		Out:             out,
	})
}

// thorOpenGadget opens the initrd-flash ADB gadget as a stage-2 transport,
// retrying while it re-enumerates after stage-1.
func thorOpenGadget(dev thorDevice, out io.Writer) (flashengine.Transport, func(), error) {
	deadline := time.Now().Add(60 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		// Pin the chosen board by physical location across the re-enumeration.
		devices, err := winusb.ListDevices()
		if err != nil {
			return nil, nil, err
		}
		gadget, found, err := findThorGadget(dev.PathKey, devices)
		if err != nil {
			return nil, nil, err
		}
		if !found {
			lastErr = fmt.Errorf("no flashing gadget at USB location %q", dev.PathKey)
			time.Sleep(time.Second)
			continue
		}
		// A staged INF does not override an existing higher-ranked binding.
		// Check the gadget itself before opening it; an elevated helper can
		// repair it without restarting this live flash session.
		if err := ensureUSBDriverTo(out, gadget, winusb.JetsonDriver()); err != nil {
			return nil, nil, fmt.Errorf("%w: preparing flashing gadget driver: %w", errGadgetUnreachable, err)
		}
		d, err := winusb.OpenProfileInstance(gadget, winusb.JetsonDriver())
		if err == nil {
			a, aerr := winusb.NewADB(d)
			if aerr == nil {
				return a, func() { d.Close() }, nil
			}
			d.Close()
			lastErr = aerr
		} else {
			lastErr = err
		}
		time.Sleep(time.Second)
	}
	// Nothing was written yet, so signal the "gadget unreachable, Thor is safe" path.
	return nil, nil, fmt.Errorf("%w (over WinUSB): %v", errGadgetUnreachable, lastErr)
}

func findThorGadget(location string, devices []winusb.Device) (winusb.Device, bool, error) {
	if location == "" {
		return winusb.Device{}, false, fmt.Errorf("missing selected Thor USB location")
	}
	var chosen winusb.Device
	found := false
	for _, d := range devices {
		if d.VID != winusb.VendorNVIDIA || d.PID != winusb.ProductGadget || !strings.EqualFold(d.LocationPath, location) {
			continue
		}
		if found {
			return winusb.Device{}, false, fmt.Errorf("multiple flashing gadgets at USB location %q", location)
		}
		chosen, found = d, true
	}
	return chosen, found, nil
}
