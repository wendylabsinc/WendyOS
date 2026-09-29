//go:build darwin || linux

// Package adb drives NVIDIA's T264 initrd-flash ADB gadget over the gousb bulk
// endpoints on macOS/Linux. Device discovery, interface claiming, and the USB
// transfers live here; the ADB wire protocol (CNXN, stream multiplexing, shell,
// sync push) is shared with the Windows WinUSB backend in package adbproto.
//
// The ADB interface is identified by USB vendor class 0xFF / subclass 0x42 /
// protocol 0x01.
package adb

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/google/gousb"

	"github.com/wendylabsinc/wendy/go/internal/cli/tegraflash/adbproto"
)

const (
	classVendor = 0xff
	subclassADB = 0x42
	protocolADB = 0x01
)

// adbPortKey is the stable physical-location key (bus + parent-port chain), matching
// rcm.portKey, so a device can be tracked across the RCM→ADB re-enumeration.
func adbPortKey(desc *gousb.DeviceDesc) string {
	parts := make([]string, len(desc.Path))
	for i, p := range desc.Path {
		parts[i] = strconv.Itoa(p)
	}
	return fmt.Sprintf("%d-%s", desc.Bus, strings.Join(parts, "."))
}

// Device is a connected ADB transport over USB. It implements adbproto.Transport
// and exposes Shell/Push through an embedded *adbproto.Conn.
type Device struct {
	ctx   *gousb.Context
	dev   *gousb.Device
	cfg   *gousb.Config
	iface *gousb.Interface
	in    *gousb.InEndpoint
	out   *gousb.OutEndpoint
	conn  *adbproto.Conn
}

// Read implements adbproto.Transport: one bulk-IN transfer bounded by timeout.
// Reads into a full 64 KiB-capable buffer at the caller's discretion (a
// sub-packet read length can error on macOS IOKit; the shared code sizes it).
func (d *Device) Read(p []byte, timeout time.Duration) (int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return d.in.ReadContext(ctx, p)
}

// Write implements adbproto.Transport: a bulk-OUT transfer. gousb/libusb does not
// append an end-of-transfer ZLP, so no special framing is needed.
func (d *Device) Write(p []byte) error {
	ctx, cancel := context.WithTimeout(context.Background(), adbproto.IOTimeout)
	defer cancel()
	_, err := d.out.WriteContext(ctx, p)
	return err
}

// Shell runs command on the device (shell-protocol-v2); a non-zero exit yields an
// *adbproto.ExitError.
func (d *Device) Shell(command string) (string, error) { return d.conn.Shell(command) }

// Push streams r to remotePath via the sync SEND service with the given unix mode.
func (d *Device) Push(r io.Reader, remotePath string, mode int) error {
	return d.conn.Push(r, remotePath, mode)
}

// Open finds a USB device exposing an ADB interface, claims it, and performs the
// CNXN handshake. It retries on transient failures: macOS/libusb does not release
// the interface synchronously when a previous process exits, so a claim can
// briefly fail with "bad access" until the kernel frees it.
func Open() (*Device, error) {
	return openAt(os.Getenv("WENDY_ADB_PATH"), false)
}

// OpenAt requires the Thor flashing gadget at this exact physical USB path.
// It never falls back to another ADB device when USB topology changes.
func OpenAt(path string) (*Device, error) {
	if path == "" {
		return nil, fmt.Errorf("a physical USB path is required")
	}
	return openAt(path, true)
}

func openAt(path string, strict bool) (*Device, error) {
	var lastErr error
	for attempt := 0; attempt < 12; attempt++ {
		d, err := openOnce(path, strict)
		if err == nil {
			return d, nil
		}
		lastErr = err
		time.Sleep(time.Duration(150+attempt*150) * time.Millisecond)
	}
	return nil, lastErr
}

// ErrUSBAccess marks an open failure caused by the OS denying access to the USB
// device (missing udev rule / needs sudo), as opposed to the gadget being absent.
// Callers classify it to show the right remediation.
var ErrUSBAccess = errors.New("USB access denied opening the flashing gadget")

func openOnce(wantPath string, strict bool) (*Device, error) {
	ctx := gousb.NewContext()
	ctx.Debug(0)

	// Open every device exposing an ADB interface; we select among them below. We do
	// NOT pre-filter by WENDY_ADB_PATH here because the flashing gadget can
	// re-enumerate at a different USB location than the RCM device was selected at.
	devs, err := ctx.OpenDevices(func(desc *gousb.DeviceDesc) bool {
		if strict && (adbPortKey(desc) != wantPath || uint16(desc.Vendor) != 0x0955 || uint16(desc.Product) != 0x7100) {
			return false
		}
		_, _, _, ok := findADBInterface(desc)
		return ok
	})
	// OpenDevices returns opened handles plus an aggregate error; proceed as long as
	// we got at least one usable handle.
	if len(devs) == 0 {
		ctx.Close()
		// Distinguish permissions from absence: the gadget re-enumerates with its
		// own PID (0955:7100), so a host whose udev rules cover only the recovery
		// PIDs hits this even after a clean stage 1.
		if errors.Is(err, gousb.ErrorAccess) {
			return nil, fmt.Errorf("%w: %v (install a udev rule covering USB vendor 0955, or run with sudo)", ErrUSBAccess, err)
		}
		return nil, fmt.Errorf("no USB device with an ADB interface (ff/42/01) found: %v", err)
	}

	// Pick which device to drive. WENDY_ADB_PATH pins a physical USB location (bus +
	// parent-port chain) so a multi-device host flashes the chosen board. But the
	// flashing gadget often re-enumerates at a *different* port than the RCM device:
	// on macOS the RCM device is USB-2 Hi-Speed while the ADB gadget is USB-3
	// SuperSpeed, which lands on the companion port (e.g. 1-1 -> 1-2). So when the pin
	// matches nothing but exactly one ADB device is present, fall back to it.
	sel := 0
	if wantPath != "" {
		sel = -1
		for i, d := range devs {
			if adbPortKey(d.Desc) == wantPath {
				sel = i
				break
			}
		}
		if sel == -1 {
			if len(devs) == 1 && !strict {
				sel = 0
				fmt.Fprintf(os.Stderr, "wendy adb: no ADB device at usb %s; using the only ADB device present (usb %s)\n", wantPath, adbPortKey(devs[0].Desc))
			} else {
				for _, d := range devs {
					d.Close()
				}
				ctx.Close()
				return nil, fmt.Errorf("no ADB device at usb %s among %d ADB devices present", wantPath, len(devs))
			}
		}
	}
	dev := devs[sel]
	for i, d := range devs {
		if i != sel {
			d.Close()
		}
	}

	// On Linux a kernel driver bound to the interface makes the claim fail with
	// "busy"; auto-detach clears it. No-op on macOS (gousb swallows NOT_SUPPORTED).
	_ = dev.SetAutoDetach(true)

	cfgNum, ifNum, altNum, ok := findADBInterface(dev.Desc)
	if !ok {
		dev.Close()
		ctx.Close()
		return nil, fmt.Errorf("ADB interface disappeared")
	}

	cfg, err := dev.Config(cfgNum)
	if err != nil {
		// Logged (not just returned) because Open()'s retry loop otherwise swallows
		// why the claim keeps failing.
		fmt.Fprintf(os.Stderr, "wendy adb: usb %s: claiming config %d failed: %v\n", adbPortKey(dev.Desc), cfgNum, err)
		dev.Close()
		ctx.Close()
		return nil, fmt.Errorf("claiming config %d: %w", cfgNum, err)
	}
	iface, err := cfg.Interface(ifNum, altNum)
	if err != nil {
		fmt.Fprintf(os.Stderr, "wendy adb: usb %s: claiming interface %d.%d failed: %v\n", adbPortKey(dev.Desc), ifNum, altNum, err)
		cfg.Close()
		dev.Close()
		ctx.Close()
		return nil, fmt.Errorf("claiming interface %d.%d: %w", ifNum, altNum, err)
	}

	var inEP *gousb.InEndpoint
	var outEP *gousb.OutEndpoint
	for _, ep := range iface.Setting.Endpoints {
		if ep.TransferType != gousb.TransferTypeBulk {
			continue
		}
		if ep.Direction == gousb.EndpointDirectionIn && inEP == nil {
			inEP, _ = iface.InEndpoint(ep.Number)
		} else if ep.Direction == gousb.EndpointDirectionOut && outEP == nil {
			outEP, _ = iface.OutEndpoint(ep.Number)
		}
	}
	if inEP == nil || outEP == nil {
		iface.Close()
		cfg.Close()
		dev.Close()
		ctx.Close()
		return nil, fmt.Errorf("ADB interface is missing bulk endpoints")
	}

	d := &Device{ctx: ctx, dev: dev, cfg: cfg, iface: iface, in: inEP, out: outEP}
	d.conn = adbproto.NewConn(d)
	if err := d.conn.Connect(); err != nil {
		// The ADB interface was found and claimed but the CNXN handshake failed.
		// Surface why here: Open() retries internally and often the process is killed
		// mid-retry before Open() returns, so this is the only place the underlying
		// reason (adbd not up, USB read error, auth) is visible in the flash log.
		fmt.Fprintf(os.Stderr, "wendy adb: claimed ADB interface at usb %s but CNXN handshake failed: %v\n", adbPortKey(dev.Desc), err)
		d.Close()
		return nil, err
	}
	return d, nil
}

// VendorPresent reports whether any USB device with the given vendor id is present.
// It only inspects descriptors (the filter returns false), so no device is claimed.
func VendorPresent(vid uint16) bool {
	ctx := gousb.NewContext()
	defer ctx.Close()
	found := false
	devs, _ := ctx.OpenDevices(func(desc *gousb.DeviceDesc) bool {
		if uint16(desc.Vendor) == vid {
			found = true
		}
		return false
	})
	for _, d := range devs {
		d.Close()
	}
	return found
}

// findADBInterface returns the config/interface/alt numbers of the first ADB
// interface (vendor class 0xFF, subclass 0x42, protocol 0x01) in desc.
func findADBInterface(desc *gousb.DeviceDesc) (cfgNum, ifNum, altNum int, ok bool) {
	for _, c := range desc.Configs {
		for _, i := range c.Interfaces {
			for _, a := range i.AltSettings {
				if uint8(a.Class) == classVendor && uint8(a.SubClass) == subclassADB && uint8(a.Protocol) == protocolADB {
					return c.Number, a.Number, a.Alternate, true
				}
			}
		}
	}
	return 0, 0, 0, false
}

// Close releases the USB resources.
func (d *Device) Close() {
	if d.iface != nil {
		d.iface.Close()
	}
	if d.cfg != nil {
		d.cfg.Close()
	}
	if d.dev != nil {
		d.dev.Close()
	}
	if d.ctx != nil {
		d.ctx.Close()
	}
}
