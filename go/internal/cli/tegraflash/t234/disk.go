//go:build darwin || linux || windows

package t234

import (
	"context"
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

// UMSDisk is one USB mass-storage LUN the flashing initrd exposed.
type UMSDisk struct {
	DevPath      string // e.g. /dev/disk4 or /dev/sdb
	RawPath      string // e.g. /dev/rdisk4 (same as DevPath on Linux)
	SizeBytes    int64
	USBSpeedMbps int64  // negotiated link speed; zero when unavailable
	Vendor       string // SCSI inquiry vendor, e.g. "flashpkg" or "mmcblk0"
	Serial       string // SCSI inquiry product: the device's 8-hex session id
	PortPath     string // physical USB topology key, same form as rcm.RecoveryDevice.PathKey
}

type LUNSelector struct {
	Vendor   string
	PortPath string
	Session  string
	// PortHint marks PortPath as preferred rather than required. The first LUN
	// after RCM boot can train at a different USB speed than the bootROM's
	// recovery device, and the USB2/USB3 phys of one physical connector are
	// distinct root-hub ports — so the gadget legitimately appears off the
	// recovery port. With PortHint set, an exact port match still wins, but a
	// single off-port candidate is accepted; several candidates fail closed.
	PortHint bool
}

var scanUMSDisks = listUMSDisks
var umsPollInterval = time.Second

// observedUMSHint formats the raw SCSI INQUIRY strings of every USB
// mass-storage LUN currently visible, for diagnosing a wait that timed out. It
// reports the vendor/product fields verbatim (before splitInquiry rejoins them)
// plus the BSD/block name, so a device advertising an unexpected export name —
// or a LUN the host never assigned a whole-disk node to — is obvious.
func observedUMSHint() string {
	var parts []string
	if raw := strings.TrimRight(rawUMSInquiry(), "\n"); raw != "" {
		parts = append(parts, "USB mass-storage LUNs currently visible (raw SCSI INQUIRY):\n"+raw)
	} else {
		parts = append(parts, "No USB mass-storage LUNs are currently visible to this computer.")
	}
	// The board's USB identity tells the failure apart: back in recovery
	// (0955:70xx) means it rebooted mid-sequence; still the gadget (1d6b:0104)
	// means the initrd stalled between commands; absent means it left USB.
	parts = append(parts, tegraUSBHint())
	return strings.Join(parts, "\n")
}

// tegraUSBLabel names a Tegra-relevant USB device, or "" for anything else.
func tegraUSBLabel(vendor, product uint16) string {
	switch {
	case vendor == 0x0955:
		if name, ok := rcm.T234ModuleName(product); ok {
			return fmt.Sprintf("0955:%04x (%s APX recovery)", product, name)
		}
		return fmt.Sprintf("0955:%04x (NVIDIA recovery)", product)
	case vendor == GadgetVendorID && product == GadgetProductID:
		return "1d6b:0104 (flashing gadget)"
	}
	return ""
}

// WaitForUMSDisk polls until a LUN with the given SCSI vendor string appears
// (the flashing initrd names LUNs after what they carry: "flashpkg" or the
// rootfs device). It returns an error when several match — wendy flashes one
// Orin at a time and must not write into the wrong board.
func WaitForUMSDisk(ctx context.Context, vendor string, timeout time.Duration) (UMSDisk, error) {
	return WaitForUMSDiskAt(ctx, LUNSelector{Vendor: vendor}, timeout)
}

// WaitForUMSDiskAt correlates a LUN by export name, selected physical USB port,
// and (after the first LUN) the device's session identifier. Any missing or
// ambiguous topology correlation fails closed before a raw disk write.
func WaitForUMSDiskAt(ctx context.Context, selector LUNSelector, timeout time.Duration) (UMSDisk, error) {
	deadline := time.Now().Add(timeout)
	for {
		disks, err := scanUMSDisks()
		if err == nil {
			var matches []UMSDisk
			for _, d := range disks {
				if d.Vendor != selector.Vendor {
					continue
				}
				if selector.PortPath != "" && d.PortPath == "" {
					return UMSDisk{}, fmt.Errorf("USB storage %q appeared as %s but its physical USB port could not be determined; refusing an uncorrelated raw write", selector.Vendor, d.DevPath)
				}
				if (selector.PortPath == "" || d.PortPath == selector.PortPath) && (selector.Session == "" || strings.EqualFold(d.Serial, selector.Session)) {
					matches = append(matches, d)
				}
			}
			switch {
			case len(matches) == 1:
				return matches[0], nil
			case len(matches) > 1:
				return UMSDisk{}, fmt.Errorf("found %d USB storage devices matching %q at port %q/session %q — correlation is ambiguous", len(matches), selector.Vendor, selector.PortPath, selector.Session)
			}
			if selector.PortHint && selector.PortPath != "" {
				var offPort []UMSDisk
				for _, d := range disks {
					if d.Vendor == selector.Vendor && d.PortPath != "" && (selector.Session == "" || strings.EqualFold(d.Serial, selector.Session)) {
						offPort = append(offPort, d)
					}
				}
				switch {
				case len(offPort) == 1:
					return offPort[0], nil
				case len(offPort) > 1:
					return UMSDisk{}, fmt.Errorf("found %d USB storage devices matching %q while none is at the recovery port %q — cannot tell which board re-enumerated; flash one Jetson at a time", len(offPort), selector.Vendor, selector.PortPath)
				}
			}
			// The device exports "flashpkg" instead of the requested LUN when
			// its side of the flash failed early — surface that instead of
			// timing out (mirrors the bundle's initrd-flash host script).
			if selector.Vendor != FlashpkgVendor {
				for _, d := range disks {
					if d.Vendor == FlashpkgVendor && (selector.PortPath == "" || d.PortPath == selector.PortPath) && (selector.Session == "" || strings.EqualFold(d.Serial, selector.Session)) {
						return UMSDisk{}, fmt.Errorf("device exported %q instead of %q — the device-side flash failed early; its logs are in the flash package", FlashpkgVendor, selector.Vendor)
					}
				}
			}
		}
		if time.Now().After(deadline) {
			if err != nil {
				return UMSDisk{}, fmt.Errorf("timed out waiting for USB storage %q at port %q/session %q (last scan error: %v)\n%s", selector.Vendor, selector.PortPath, selector.Session, err, observedUMSHint())
			}
			return UMSDisk{}, fmt.Errorf("timed out waiting for USB storage %q at port %q/session %q\n%s", selector.Vendor, selector.PortPath, selector.Session, observedUMSHint())
		}
		select {
		case <-ctx.Done():
			return UMSDisk{}, ctx.Err()
		case <-time.After(umsPollInterval):
		}
	}
}
