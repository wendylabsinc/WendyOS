//go:build windows

package commands

// Windows hooks for the shared T234 Orin install flow. Recovery enumeration
// and the stage-1 RCM boot go through the WinUSB backend (package winusb);
// stage 2 needs no USB driver at all — the flashing gadget is a mass-storage
// device that binds the inbox usbstor/disk stack, and raw block writes run
// in-process (the whole process is elevated once via UAC, so there is no
// sudo-style helper re-exec).

import (
	"context"
	"errors"
	"fmt"
	"io"

	"golang.org/x/sys/windows"

	"github.com/wendylabsinc/wendy/go/internal/cli/tegraflash/flashpack"
	"github.com/wendylabsinc/wendy/go/internal/cli/tegraflash/rcm"
	"github.com/wendylabsinc/wendy/go/internal/cli/tegraflash/t234"
	"github.com/wendylabsinc/wendy/go/internal/cli/tegraflash/winusb"
	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
)

// pickOrinRecoveryDevice lists T234 modules in recovery mode via SetupAPI
// (which reads identity from the hub, so no driver is needed to scan) and
// selects one, filtered to the install's module family. If the chosen device
// is not bound to wendy's WinUSB interface, the driver is installed first —
// checked per device, not host-globally: a Thor bound by a driver package
// staged before the T234 IDs were added must not mask a driverless Orin.
func pickOrinRecoveryDevice(opts t234InstallOptions) (rcm.RecoveryDevice, error) {
	match := orinRecoveryMatch(opts.DeviceType)
	scan := func() ([]winusb.Device, error) {
		devs, err := winusb.ListDevices()
		if err != nil {
			return nil, err
		}
		var recovery []winusb.Device
		for _, d := range devs {
			if d.IsT234() && match(d.RecoveryDevice()) {
				recovery = append(recovery, d)
			}
		}
		return recovery, nil
	}
	for {
		recovery, err := scan()
		if err != nil {
			return rcm.RecoveryDevice{}, err
		}
		if len(recovery) == 0 {
			// The user already confirmed the board is in recovery mode, so an
			// empty scan usually means cabling or the recovery sequence needs
			// another try. Wait passively (spinner) until a device appears.
			if recovery, err = waitForRecovery(orinRecoveryHints(opts), scan); err != nil {
				return rcm.RecoveryDevice{}, err
			}
		}
		var chosen winusb.Device
		switch len(recovery) {
		case 1:
			chosen = recovery[0]
		default:
			// Keyed by InstanceID — always present and unique. LocationPath is
			// best-effort (empty under USB redirection) and empty/duplicate keys
			// would collapse picker rows, resolving a destructive selection to
			// the wrong board.
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
			sel, err := pickFromItems("Select the "+opts.DeviceName+" to flash", items)
			if err != nil {
				return rcm.RecoveryDevice{}, err
			}
			chosen = byKey[sel]
		}
		// Stage 2 correlates the flashing disks to the board by physical USB
		// port; without a location path (USB-over-IP, RDP redirection, some
		// VMs) a raw write cannot be tied to the chosen board. Fail closed
		// before anything destructive, mirroring WaitForUMSDiskAt.
		if chosen.LocationPath == "" {
			return rcm.RecoveryDevice{}, fmt.Errorf("cannot determine the physical USB port of %s; refusing an uncorrelated recovery flash — connect the Jetson to a direct (non-redirected) USB port", chosen.Describe())
		}
		if err := ensureOrinDriver(chosen); err != nil {
			return rcm.RecoveryDevice{}, err
		}
		return chosen.RecoveryDevice(), nil
	}
}

// ensureOrinDriver installs the Jetson WinUSB driver when the selected
// recovery device needs a binding update. The Orin raw-disk flow already
// elevates before the wizard; the shared driver helper also supports callers
// that are not elevated without restarting their install session.
func ensureOrinDriver(d winusb.Device) error {
	return ensureUSBDriver(d, winusb.JetsonDriver())
}

// orinStageOne performs the stage-1 RCM boot over WinUSB with the file chain
// declared by the flashpack manifest.
func orinStageOne(fp *flashpack.Flashpack, dev rcm.RecoveryDevice, out io.Writer) error {
	order, memBCT, blob, err := t234RCMFiles(fp)
	if err != nil {
		return err
	}
	return winusb.StageOneBoot(winusb.StageOneOptions{
		Stage1Dir:       fp.Root,
		MemBCT:          memBCT,
		Blob:            blob,
		SendOrder:       order,
		Location:        dev.PathKey,
		Instance:        dev.Instance,
		ExpectedProduct: dev.Product,
		Out:             out,
	})
}

// isWinRawDiskAccessError reports a Windows raw-disk open/write refusal:
// ERROR_ACCESS_DENIED usually means a mounted volume still covers the write
// region (or the process is not actually elevated), ERROR_SHARING_VIOLATION
// that another process (antivirus, backup, indexing, Explorer) holds the
// disk. Matched with errors.Is — the write path runs in-process, so the
// syscall.Errno survives every %w wrap — never against rendered message
// text, which FormatMessage localizes on non-English systems.
func isWinRawDiskAccessError(err error) bool {
	return errors.Is(err, windows.ERROR_ACCESS_DENIED) || errors.Is(err, windows.ERROR_SHARING_VIOLATION)
}

// runT234Helper executes one raw block operation in-process: the process is
// already elevated (preAuthElevation → UAC), so unlike macOS/Linux there is no
// privileged re-exec. Cancellation caveat: a raw write cannot be interrupted
// mid-syscall, so on ctx cancel this returns while the write runs to completion
// on the background goroutine — the process does not abort it (the sole-console
// pause can even outlive it). Already-written bytes stay either way; the step
// runner's abort warnings cover the indeterminate device state. Progress
// forwarding is gated on ctx so the outliving goroutine stops touching the
// torn-down UI once cancelled.
func runT234Helper(ctx context.Context, req t234.HelperRequest, onProgress func(done, total int64)) error {
	result := make(chan error, 1)
	go func() {
		result <- t234.RunHelperRequest(req, &progressWriter{onProgress: func(done, total int64) {
			if ctx.Err() == nil && onProgress != nil {
				onProgress(done, total)
			}
		}})
	}()
	select {
	case err := <-result:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}
