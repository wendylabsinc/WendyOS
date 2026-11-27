//go:build darwin || linux || windows

package t234

import (
	"context"
	"errors"
	"fmt"
	"runtime"
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
	DevPath      string // e.g. /dev/disk4 or /dev/sdb
	RawPath      string // e.g. /dev/rdisk4 (same as DevPath on Linux)
	SizeBytes    int64
	USBSpeedMbps int64  // negotiated link speed; zero when unavailable
	Vendor       string // SCSI inquiry vendor: "flashpkg", "rootfs" or a legacy device name such as "mmcblk0"
	Serial       string // SCSI inquiry product: the device's 8-hex session id
	PortPath     string // physical USB topology key, same form as rcm.RecoveryDevice.PathKey
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

// scanRawInquiry and hostOS are vars so tests can drive the timeout diagnosis
// without this host's disks, and check host-specific hints on any platform.
var (
	scanRawInquiry = rawUMSInquiry
	hostOS         = runtime.GOOS
)

// observedUMSHint diagnoses a LUN wait that timed out. It formats the raw SCSI
// INQUIRY strings of every USB mass-storage LUN currently visible: the
// vendor/product fields verbatim (before splitInquiry rejoins them) plus the
// BSD/block name, so a device advertising an unexpected export name — or a LUN
// the host never assigned a whole-disk node to — is obvious. Then it says what
// this Jetson is doing on USB.
func observedUMSHint(sel LUNSelector) string {
	var parts []string
	raw := strings.TrimRight(scanRawInquiry(), "\n")
	if raw != "" {
		parts = append(parts, "USB mass-storage LUNs currently visible (raw SCSI INQUIRY):\n"+raw)
	} else {
		parts = append(parts, "No USB mass-storage LUNs are currently visible to this computer.")
	}
	devs, err := scanUSBDevices()
	if err != nil {
		return strings.Join(append(parts, "Could not list USB devices: "+err.Error()), "\n")
	}
	usb, onUSB := describeTegraUSB(devs, sel.Session, recoveryPortOf(sel))
	parts = append(parts, usb)
	// Only the first wait of a flash (no session yet) can be held up by the
	// prompt, and only a gadget and a disk that never showed up fit it.
	if hostOS == "darwin" && sel.Session == "" && !onUSB && !strings.Contains(raw, sel.Vendor) {
		parts = append(parts, macAccessoryHint)
	}
	return strings.Join(parts, "\n")
}

// macAccessoryHint names a possible, unconfirmed cause of a flashing gadget
// that never reaches a Mac: Apple silicon laptops ask "Allow accessory to
// connect?" before a new USB accessory may connect, and the flashing gadget is
// a different USB device from the recovery-mode one the person already allowed.
const macAccessoryHint = `On macOS, an unanswered "Allow accessory to connect?" prompt is one possible cause: macOS can ask again when the Jetson reconnects as its flashing gadget, and keeps it disconnected until you choose Allow. If you see that prompt, choose Allow. Then put the Jetson back in recovery mode and start the install again.`

// recoveryPortOf is the USB port the selector's Jetson uses in recovery mode
// ("" if unknown). Before the gadget is adopted (no session yet) that is the
// port the wait itself is pinned to.
func recoveryPortOf(sel LUNSelector) string {
	if sel.RecoveryPort == "" && sel.Session == "" {
		return sel.PortPath
	}
	return sel.RecoveryPort
}

// usbDevice is one device on the host's USB bus: just what the Tegra
// diagnostics need to tell this Jetson apart from bystanders.
type usbDevice struct {
	VID, PID uint16
	Serial   string
	Product  string // USB product string; empty where the host scan does not read it (Windows)
	PortPath string
}

var scanUSBDevices = listUSBDevices

// USB product strings of the two kinds of 1d6b:0104 gadget a Jetson shows: the
// flashing initrd's (meta-tegra's initrd-flash.scheme.in) and the start of a
// running WendyOS board's (gadget-setup.sh: "WendyOS <name>", and "WendyOS
// Device <name>" on images from before the prefix was shortened).
const (
	flashGadgetProduct   = "Linux for Tegra"
	wendyOSProductPrefix = "WendyOS "
)

// describeTegraUSB says which situation this Jetson is in on USB — in recovery
// mode (it restarted, or never ran the flashing image), flashing gadget up (it
// has not offered the disk, or the host did not attach it) or absent — and
// lists the devices behind that. onUSB reports whether a device that is or may
// be this Jetson is present.
//
// Any Linux board in device mode can use 1d6b:0104, and a running WendyOS
// board does, so a gadget only counts as this Jetson on evidence: its USB
// serial is the flash session id or, before the session is known, it is on
// recoveryPort (where this Jetson was in recovery mode, "" if unknown) or is
// the only gadget with the flashing initrd's product string. The gadget can
// come up on the connector's other root-hub port (see LUNSelector.PortHint),
// so a gadget elsewhere with no evidence either way is reported as possibly
// this Jetson, not as another board. A running WendyOS board (by its product
// string) is never the flashing gadget; it is listed with the other boards.
func describeTegraUSB(devs []usbDevice, session, recoveryPort string) (text string, onUSB bool) {
	var recovery, gadget, unsure, others []string
	var candidates []usbDevice // gadgets the session cannot tell apart yet
	runningBoard := false      // a running WendyOS board is listed under others
	for _, d := range devs {
		onPort := recoveryPort != "" && d.PortPath == recoveryPort
		switch {
		case d.VID == rcm.VendorNVIDIA && (onPort || recoveryPort == ""):
			recovery = append(recovery, tegraUSBLabel(d, ""))
		case d.VID == rcm.VendorNVIDIA:
			others = append(others, tegraUSBLabel(d, ""))
		case d.VID != GadgetVendorID || d.PID != GadgetProductID:
		case session != "" && isSessionGadget(d, session):
			gadget = append(gadget, tegraUSBLabel(d, "flashing gadget"))
		case session != "":
			others = append(others, tegraUSBLabel(d, ""))
		case !strings.HasPrefix(d.Product, wendyOSProductPrefix):
			candidates = append(candidates, d)
		case onPort: // this Jetson's own port: it may have started its installed WendyOS
			unsure = append(unsure, tegraUSBLabel(d, fmt.Sprintf("running board %q", d.Product)))
		default:
			others = append(others, tegraUSBLabel(d, fmt.Sprintf("running board %q", d.Product)))
			runningBoard = true
		}
	}
	inRecovery := recoveryPort != "" && len(recovery) > 0
	ours := -1
	if !inRecovery { // a Jetson in recovery mode is not also a gadget
		ours = ourGadget(candidates, recoveryPort)
	}
	for i, d := range candidates {
		product := ""
		if d.Product != "" {
			product = fmt.Sprintf("%q", d.Product)
		}
		switch {
		case i == ours:
			gadget = append(gadget, tegraUSBLabel(d, "flashing gadget"))
		case inRecovery || ours >= 0: // this Jetson is accounted for
			others = append(others, tegraUSBLabel(d, product))
		default:
			unsure = append(unsure, tegraUSBLabel(d, product))
		}
	}

	var lines []string
	switch {
	case inRecovery:
		lines = append(lines, "This Jetson is in USB recovery mode, so the flashing image is not running: "+strings.Join(recovery, ", "))
	case len(recovery) > 0: // no port to tell this Jetson from another one
		lines = append(lines, "NVIDIA recovery-mode devices on USB: "+strings.Join(recovery, ", "))
	}
	if len(gadget) > 0 {
		lines = append(lines, "This Jetson's flashing gadget is on USB but is not showing the disk: "+strings.Join(gadget, ", "))
	}
	switch len(unsure) {
	case 0:
	case 1:
		lines = append(lines, "A USB gadget that may be this Jetson or another board is present: "+unsure[0])
	default:
		lines = append(lines, fmt.Sprintf("%d USB gadgets that may be this Jetson or other boards are present: %s", len(unsure), strings.Join(unsure, ", ")))
	}
	onUSB = len(lines) > 0
	if !onUSB {
		its := "1d6b:0104"
		if session != "" {
			its = "session " + session
		}
		lines = append(lines, fmt.Sprintf("This Jetson is not on USB: neither its recovery device (0955:*) nor its flashing gadget (%s) is present.", its))
	}
	switch {
	case len(others) == 0:
	case runningBoard && !inRecovery && len(gadget) == 0:
		// With this Jetson found nowhere, a running board on another port may
		// be it: like the flashing gadget, WendyOS's gadget can come up on the
		// connector's other root-hub port.
		lines = append(lines, "Other USB gadgets present (not this Jetson, unless it started WendyOS instead of the flashing image): "+strings.Join(others, ", "))
	default:
		lines = append(lines, "Other USB gadgets present (not this Jetson): "+strings.Join(others, ", "))
	}
	return strings.Join(lines, "\n"), onUSB
}

// ourGadget picks this Jetson's flashing gadget among gadgets seen before the
// session is known: the one on the recovery port, else the only one with the
// flashing initrd's product string. It returns -1 when neither singles one out.
func ourGadget(candidates []usbDevice, recoveryPort string) int {
	ours, flashing := -1, 0
	for i, d := range candidates {
		if recoveryPort != "" && d.PortPath == recoveryPort {
			return i
		}
		if d.Product == flashGadgetProduct {
			ours, flashing = i, flashing+1
		}
	}
	if flashing != 1 {
		return -1
	}
	return ours
}

// tegraUSBLabel names an NVIDIA recovery device or a 1d6b:0104 gadget; what
// (may be empty) says which kind of gadget it is.
func tegraUSBLabel(d usbDevice, what string) string {
	at := ""
	if d.PortPath != "" {
		at = ", usb " + d.PortPath
	}
	if d.VID == rcm.VendorNVIDIA {
		if name, ok := rcm.T234ModuleName(d.PID); ok {
			return fmt.Sprintf("0955:%04x (%s APX recovery%s)", d.PID, name, at)
		}
		return fmt.Sprintf("0955:%04x (NVIDIA recovery%s)", d.PID, at)
	}
	serial := d.Serial
	if serial == "" {
		serial = "none"
	}
	if what != "" {
		what += ", "
	}
	return fmt.Sprintf("1d6b:0104 (%sserial %s%s)", what, serial, at)
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
			return fmt.Errorf("timed out waiting for USB storage %q at port %q/session %q (last scan error: %v)\n%s", selector.Vendor, selector.PortPath, selector.Session, scanErr, observedUMSHint(selector))
		}
		return fmt.Errorf("timed out waiting for USB storage %q at port %q/session %q\n%s", selector.Vendor, selector.PortPath, selector.Session, observedUMSHint(selector))
	})
}
