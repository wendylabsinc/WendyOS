//go:build darwin || linux || windows

package t234

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/tegraflash/rcm"
)

// The USB gadget the flashing initrd exposes (Linux Foundation composite
// gadget IDs, from meta-tegra's initrd-flash.scheme.in).
const (
	GadgetVendorID  = 0x1d6b
	GadgetProductID = 0x0104
)

// FlashpkgVendor is the export name of the command-package LUN. init-flash.sh
// writes "<export_name><serial>" into the gadget's inquiry_string; listUMSDisks
// splits it back into the export name and session serial via splitInquiry.
const FlashpkgVendor = "flashpkg"

const sessionSerialLen = 8

// splitInquiry recovers the export name and session id after SCSI splits the
// gadget inquiry string across its fixed-width vendor and product fields.
func splitInquiry(vendor, product string) (name, serial string) {
	combined := strings.TrimSpace(vendor) + strings.TrimSpace(product)
	if len(combined) <= sessionSerialLen {
		return combined, ""
	}
	return combined[:len(combined)-sessionSerialLen], combined[len(combined)-sessionSerialLen:]
}

// UMSDisk is one USB mass-storage LUN the flashing initrd exposed. Only LUNs
// with a medium are listed: the initrd switches media in place, so a loaded
// medium is what "exported" means.
type UMSDisk struct {
	DevPath   string // e.g. /dev/disk4 or /dev/sdb
	RawPath   string // e.g. /dev/rdisk4 (same as DevPath on Linux)
	SizeBytes int64
	Vendor    string // SCSI inquiry vendor: "flashpkg" or "rootfs"
	Serial    string // SCSI inquiry product: the device's 8-hex session id
	PortPath  string // physical USB topology key, same form as rcm.RecoveryDevice.PathKey
}

type LUNSelector struct {
	Vendor   string
	PortPath string
	Session  string
	// PortHint marks PortPath as preferred rather than required. The first LUN
	// after RCM boot, or any LUN after a replug, can train at a different USB
	// speed than before, and the USB2/USB3 phys of one physical connector are
	// distinct root-hub ports — so the gadget legitimately appears off the
	// recovery port. With PortHint set, an exact port match still wins, but a
	// single off-port candidate is accepted; several candidates fail closed.
	PortHint bool
	// Refresh (may be nil) runs every gadgetCheckInterval while waiting.
	Refresh func()
	// OnMissing (may be nil) fires once when the session's gadget has been off
	// USB for gadgetMissingGrace. The wait fails with ErrJetsonInRecovery if the
	// Jetson shows up in recovery mode at RecoveryPort instead.
	OnMissing    func(gone time.Duration)
	RecoveryPort string
}

var scanUMSDisks = listUMSDisks
var umsPollInterval = time.Second

// observedUMSHint formats the raw SCSI INQUIRY strings of every USB
// mass-storage LUN currently visible, for diagnosing a wait that timed out. It
// reports the vendor/product fields verbatim (before splitInquiry rejoins them)
// plus the BSD/block name, so a device advertising an unexpected export name —
// or a LUN the host never assigned a whole-disk node to — is obvious.
func observedUMSHint(session string) string {
	var parts []string
	if raw := strings.TrimRight(rawUMSInquiry(), "\n"); raw != "" {
		parts = append(parts, "USB mass-storage LUNs currently visible (raw SCSI INQUIRY):\n"+raw)
	} else {
		parts = append(parts, "No USB mass-storage LUNs are currently visible to this computer.")
	}
	// The board's USB identity tells the failure apart: back in recovery
	// (0955:70xx) means it rebooted mid-sequence; still the gadget (1d6b:0104)
	// means the initrd stalled between commands; absent means it left USB.
	if devs, err := scanUSBDevices(); err != nil {
		parts = append(parts, "Could not list USB devices: "+err.Error())
	} else {
		parts = append(parts, describeTegraUSB(devs, session))
	}
	return strings.Join(parts, "\n")
}

// usbDevice is one device on the host's USB bus: just what the Tegra
// diagnostics need to tell this Jetson apart from bystanders.
type usbDevice struct {
	VID, PID uint16
	Serial   string
	PortPath string
}

var scanUSBDevices = listUSBDevices

// describeTegraUSB reports which Tegra-relevant USB devices are present.
// Any Linux board in device mode can use 1d6b:0104, so a gadget only counts
// as this Jetson when its USB serial is the flash session id.
func describeTegraUSB(devs []usbDevice, session string) string {
	var ours, others []string
	for _, d := range devs {
		label, mine := tegraUSBLabel(d, session)
		switch {
		case label == "":
		case mine:
			ours = append(ours, label)
		default:
			others = append(others, label)
		}
	}
	var b strings.Builder
	if len(ours) == 0 {
		b.WriteString("This Jetson is not on USB: no NVIDIA recovery device (0955:*)")
		if session != "" {
			fmt.Fprintf(&b, " and no flashing gadget with session %s", session)
		} else {
			b.WriteString(" or flashing gadget (1d6b:0104)")
		}
		b.WriteString(" is present.")
	} else {
		b.WriteString("Tegra USB devices present: " + strings.Join(ours, ", "))
	}
	if len(others) > 0 {
		b.WriteString("\nOther USB gadgets present (not this Jetson): " + strings.Join(others, ", "))
	}
	return b.String()
}

// tegraUSBLabel names a Tegra-relevant USB device ("" for anything else) and
// reports whether it can be this flash's Jetson. Before the session is known,
// any gadget may be it.
func tegraUSBLabel(d usbDevice, session string) (label string, mine bool) {
	at := ""
	if d.PortPath != "" {
		at = ", usb " + d.PortPath
	}
	switch {
	case d.VID == rcm.VendorNVIDIA:
		if name, ok := rcm.T234ModuleName(d.PID); ok {
			return fmt.Sprintf("0955:%04x (%s APX recovery%s)", d.PID, name, at), true
		}
		return fmt.Sprintf("0955:%04x (NVIDIA recovery%s)", d.PID, at), true
	case d.VID == GadgetVendorID && d.PID == GadgetProductID:
		serial := d.Serial
		if serial == "" {
			serial = "none"
		}
		if session == "" || isSessionGadget(d, session) {
			return fmt.Sprintf("1d6b:0104 (flashing gadget, serial %s%s)", serial, at), true
		}
		return fmt.Sprintf("1d6b:0104 (serial %s%s)", serial, at), false
	}
	return "", false
}

// ErrJetsonInRecovery reports that the Jetson rebooted into USB recovery mode
// mid-flash, so the flash cannot finish.
var ErrJetsonInRecovery = errors.New("the Jetson rebooted into USB recovery mode, so the flash did not complete; start the install again")

// lunWatch runs a LUN wait's periodic work every gadgetCheckInterval: the
// selector's Refresh, then tracking whether the session's gadget is on USB.
type lunWatch struct {
	sel                LUNSelector
	missingSince, next time.Time
}

func (w *lunWatch) tick() error {
	if time.Now().Before(w.next) {
		return nil
	}
	w.next = time.Now().Add(gadgetCheckInterval)
	if w.sel.Refresh != nil {
		w.sel.Refresh()
	}
	if w.sel.Session == "" || (w.sel.OnMissing == nil && w.sel.RecoveryPort == "") {
		return nil
	}
	devs, err := scanUSBDevices()
	if err != nil {
		return nil
	}
	onUSB := false
	for _, d := range devs {
		switch {
		case isSessionGadget(d, w.sel.Session):
			onUSB = true
		case d.VID == rcm.VendorNVIDIA && rcm.IsT234RecoveryPID(d.PID) && w.sel.RecoveryPort != "" && d.PortPath == w.sel.RecoveryPort:
			return ErrJetsonInRecovery
		}
	}
	switch {
	case onUSB:
		w.missingSince = time.Time{}
	case w.missingSince.IsZero():
		w.missingSince = time.Now()
	case w.sel.OnMissing != nil && time.Since(w.missingSince) >= gadgetMissingGrace:
		w.sel.OnMissing(time.Since(w.missingSince))
		w.sel.OnMissing = nil
	}
	return nil
}

// isSessionGadget reports whether d is the flashing gadget of this session.
func isSessionGadget(d usbDevice, session string) bool {
	return d.VID == GadgetVendorID && d.PID == GadgetProductID && strings.EqualFold(d.Serial, session)
}

// pollLUNs scans the LUNs every umsPollInterval until match settles the wait
// (done or an error), the context ends, or timeout passes; onTimeout gets the
// last scan error.
func pollLUNs(ctx context.Context, sel LUNSelector, timeout time.Duration, match func([]UMSDisk) (UMSDisk, bool, error), onTimeout func(scanErr error) error) (UMSDisk, error) {
	deadline := time.Now().Add(timeout)
	watch := lunWatch{sel: sel}
	for {
		if err := watch.tick(); err != nil {
			return UMSDisk{}, err
		}
		disks, err := scanUMSDisks()
		if err == nil {
			if d, done, merr := match(disks); done || merr != nil {
				return d, merr
			}
		}
		if time.Now().After(deadline) {
			return UMSDisk{}, onTimeout(err)
		}
		select {
		case <-ctx.Done():
			return UMSDisk{}, ctx.Err()
		case <-time.After(umsPollInterval):
		}
	}
}

// offPortLUN resolves a PortHint wait with no match on the expected port: a
// single LUN of this vendor and session on another port is the one, several
// fail closed.
func offPortLUN(disks []UMSDisk, sel LUNSelector) (UMSDisk, bool, error) {
	var offPort []UMSDisk
	for _, d := range disks {
		if d.Vendor == sel.Vendor && d.PortPath != "" && d.PortPath != sel.PortPath && (sel.Session == "" || strings.EqualFold(d.Serial, sel.Session)) {
			offPort = append(offPort, d)
		}
	}
	switch len(offPort) {
	case 0:
		return UMSDisk{}, false, nil
	case 1:
		return offPort[0], true, nil
	}
	return UMSDisk{}, false, fmt.Errorf("found %d USB storage devices matching %q while none is at the expected port %q — cannot tell which board re-enumerated; flash one Jetson at a time", len(offPort), sel.Vendor, sel.PortPath)
}

// WaitForUMSDiskAt correlates a LUN by export name, selected physical USB port,
// and (after the first LUN) the device's session identifier. Any missing or
// ambiguous topology correlation fails closed before a raw disk write.
func WaitForUMSDiskAt(ctx context.Context, selector LUNSelector, timeout time.Duration) (UMSDisk, error) {
	return pollLUNs(ctx, selector, timeout, func(disks []UMSDisk) (UMSDisk, bool, error) {
		var matches []UMSDisk
		for _, d := range disks {
			if d.Vendor != selector.Vendor {
				continue
			}
			if selector.PortPath != "" && d.PortPath == "" {
				return UMSDisk{}, false, fmt.Errorf("USB storage %q appeared as %s but its physical USB port could not be determined; refusing an uncorrelated raw write", selector.Vendor, d.DevPath)
			}
			if (selector.PortPath == "" || d.PortPath == selector.PortPath) && (selector.Session == "" || strings.EqualFold(d.Serial, selector.Session)) {
				matches = append(matches, d)
			}
		}
		switch {
		case len(matches) == 1:
			return matches[0], true, nil
		case len(matches) > 1:
			return UMSDisk{}, false, fmt.Errorf("found %d USB storage devices matching %q at port %q/session %q — correlation is ambiguous", len(matches), selector.Vendor, selector.PortPath, selector.Session)
		}
		if selector.PortHint && selector.PortPath != "" {
			if d, ok, err := offPortLUN(disks, selector); ok || err != nil {
				return d, ok, err
			}
		}
		// The device exports "flashpkg" instead of the requested LUN when
		// its side of the flash failed early — surface that instead of
		// timing out (mirrors the bundle's initrd-flash host script).
		if selector.Vendor != FlashpkgVendor {
			for _, d := range disks {
				if d.Vendor == FlashpkgVendor && (selector.PortPath == "" || d.PortPath == selector.PortPath) && (selector.Session == "" || strings.EqualFold(d.Serial, selector.Session)) {
					return UMSDisk{}, false, fmt.Errorf("device exported %q instead of %q — the device-side flash failed early; its logs are in the flash package", FlashpkgVendor, selector.Vendor)
				}
			}
		}
		return UMSDisk{}, false, nil
	}, func(scanErr error) error {
		if scanErr != nil {
			return fmt.Errorf("timed out waiting for USB storage %q at port %q/session %q (last scan error: %v)\n%s", selector.Vendor, selector.PortPath, selector.Session, scanErr, observedUMSHint(selector.Session))
		}
		return fmt.Errorf("timed out waiting for USB storage %q at port %q/session %q\n%s", selector.Vendor, selector.PortPath, selector.Session, observedUMSHint(selector.Session))
	})
}
