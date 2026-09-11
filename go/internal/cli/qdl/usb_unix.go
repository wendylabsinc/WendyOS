//go:build darwin || linux

package qdl

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"time"

	"github.com/google/gousb"
)

// A Qualcomm SoC in Emergency Download mode.
const (
	VendorQualcomm gousb.ID = 0x05c6
	ProductEDL     gousb.ID = 0x9008
)

// The EDL interface is vendor-specific. Protocol 0xff is the generic case;
// 0x10, 0x11 and 0x13 are the Sahara variants shipped by different boot ROMs.
var edlProtocols = []gousb.Protocol{0xff, 0x10, 0x11, 0x13}

// outChunkSize bounds a single bulk OUT transfer, and must stay a multiple of
// the endpoint's max packet size for the ZLP rule below. macOS needs a small
// one: a large bulk OUT fails there with kIOReturnNotResponding.
var outChunkSize = func() int {
	if runtime.GOOS == "darwin" {
		return 16 * 1024
	}
	return 1024 * 1024
}()

// Supported reports whether this platform has an EDL transport.
func Supported() bool { return true }

// USBConn is a Conn backed by a device in EDL mode.
type USBConn struct {
	ctx  *gousb.Context
	dev  *gousb.Device
	done func()
	in   *gousb.InEndpoint
	out  *gousb.OutEndpoint
	info DeviceInfo
}

// List reports every attached device in EDL mode.
func List() ([]DeviceInfo, error) {
	ctx := gousb.NewContext()
	defer ctx.Close() //nolint:errcheck

	var found []DeviceInfo
	// OpenDevices opens each match; close them so a later Open can claim one.
	devs, err := ctx.OpenDevices(func(desc *gousb.DeviceDesc) bool {
		return desc.Vendor == VendorQualcomm && desc.Product == ProductEDL
	})
	for _, d := range devs {
		found = append(found, describe(d))
		_ = d.Close()
	}
	// Report what was found even on a partial failure: one device we cannot
	// open must not hide the one the caller is after.
	if err != nil && len(found) == 0 {
		if isAccessErr(err) {
			return nil, fmt.Errorf("%w: %v", ErrUSBAccess, err)
		}
		return nil, fmt.Errorf("scanning USB for a device in EDL mode: %w", err)
	}
	return found, nil
}

// Open claims the device described by want. A zero DeviceInfo claims the only
// attached device.
func Open(want DeviceInfo) (*USBConn, error) {
	ctx := gousb.NewContext()
	devs, err := ctx.OpenDevices(func(desc *gousb.DeviceDesc) bool {
		return desc.Vendor == VendorQualcomm && desc.Product == ProductEDL
	})
	if err != nil && len(devs) == 0 {
		_ = ctx.Close()
		return nil, fmt.Errorf("opening the device in EDL mode: %w", err)
	}

	var chosen *gousb.Device
	for _, d := range devs {
		if chosen == nil && describe(d).matches(want) {
			chosen = d
			continue
		}
		_ = d.Close()
	}
	if chosen == nil {
		_ = ctx.Close()
		if want.Serial != "" {
			return nil, fmt.Errorf("no device in EDL mode with serial %q", want.Serial)
		}
		if want.Bus != 0 || want.Address != 0 {
			return nil, fmt.Errorf("no device in EDL mode on bus %d device %d", want.Bus, want.Address)
		}
		return nil, errors.New("no device in EDL mode")
	}

	conn, err := claim(ctx, chosen)
	if err != nil {
		_ = chosen.Close()
		_ = ctx.Close()
		return nil, err
	}
	return conn, nil
}

func claim(ctx *gousb.Context, dev *gousb.Device) (*USBConn, error) {
	// On Linux the QDLoader interface is already bound to qcserial, so the
	// claim fails as busy unless we detach it first. On macOS auto-detach
	// must stay off: libusb implements it there as a device capture that
	// needs root, and it fails outright on any driver-matched device.
	if runtime.GOOS == "linux" {
		_ = dev.SetAutoDetach(true)
	}

	cfgNum, ifNum, altNum, ok := findEDLInterface(dev.Desc)
	if !ok {
		return nil, fmt.Errorf("device %s exposes no EDL interface", dev.Desc)
	}

	cfg, err := dev.Config(cfgNum)
	if err != nil {
		if isAccessErr(err) {
			return nil, fmt.Errorf("%w: %v", ErrUSBAccess, err)
		}
		return nil, fmt.Errorf("claiming USB config %d: %w", cfgNum, err)
	}
	iface, err := cfg.Interface(ifNum, altNum)
	if err != nil {
		_ = cfg.Close()
		if isAccessErr(err) {
			return nil, fmt.Errorf("%w: %v", ErrUSBAccess, err)
		}
		return nil, fmt.Errorf("claiming USB interface %d: %w", ifNum, err)
	}

	var in *gousb.InEndpoint
	var out *gousb.OutEndpoint
	for _, ep := range iface.Setting.Endpoints {
		if ep.TransferType != gousb.TransferTypeBulk {
			continue
		}
		switch {
		case ep.Direction == gousb.EndpointDirectionIn && in == nil:
			in, err = iface.InEndpoint(int(ep.Number))
		case ep.Direction == gousb.EndpointDirectionOut && out == nil:
			out, err = iface.OutEndpoint(int(ep.Number))
		}
		if err != nil {
			iface.Close()
			_ = cfg.Close()
			return nil, fmt.Errorf("opening bulk endpoint: %w", err)
		}
	}
	if in == nil || out == nil {
		iface.Close()
		_ = cfg.Close()
		return nil, errors.New("EDL interface is missing a bulk endpoint pair")
	}

	return &USBConn{
		ctx: ctx,
		dev: dev,
		done: func() {
			iface.Close()
			_ = cfg.Close()
		},
		in:   in,
		out:  out,
		info: describe(dev),
	}, nil
}

// describe reads the identifying strings off an open device.
func describe(dev *gousb.Device) DeviceInfo {
	usbSerial, _ := dev.SerialNumber()
	product, _ := dev.Product()
	return DeviceInfo{
		Serial:  edlSerial(usbSerial, product),
		CID:     edlField(product, "CID"),
		Bus:     dev.Desc.Bus,
		Address: dev.Desc.Address,
	}
}

// findEDLInterface locates the vendor-specific interface carrying Sahara and
// Firehose, which is not necessarily the default interface.
func findEDLInterface(desc *gousb.DeviceDesc) (cfgNum, ifNum, altNum int, ok bool) {
	for _, cd := range desc.Configs {
		for _, id := range cd.Interfaces {
			for _, alt := range id.AltSettings {
				if alt.Class != 0xff || alt.SubClass != 0xff {
					continue
				}
				for _, p := range edlProtocols {
					if alt.Protocol == p {
						return cd.Number, id.Number, alt.Alternate, true
					}
				}
			}
		}
	}
	return 0, 0, 0, false
}

// Info describes the claimed device.
func (c *USBConn) Info() DeviceInfo { return c.info }

// Read pulls one bulk transfer, reporting ErrTimeout when the device stays
// quiet — an expected outcome, since both protocol layers poll.
func (c *USBConn) Read(p []byte, timeout time.Duration) (int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	n, err := c.in.ReadContext(ctx, p)
	// Bytes plus a deadline is a successful short read. Bytes plus any other
	// error is not: gousb reports a buffer overflow or a vanished device that
	// way, and treating it as clean would silently truncate the message.
	if n > 0 {
		if err == nil || isTimeout(err) {
			return n, nil
		}
		return n, fmt.Errorf("reading from the device: %w", err)
	}
	if err != nil {
		if isTimeout(err) {
			return 0, ErrTimeout
		}
		return 0, fmt.Errorf("reading from the device: %w", err)
	}
	return 0, ErrTimeout
}

// Write sends buf in chunks, followed by a zero-length packet when the total
// lands on a packet boundary — the device needs that to see the transfer end.
func (c *USBConn) Write(buf []byte, timeout time.Duration) (int, error) {
	// Each transfer gets the full timeout, as qdl does. Sharing one deadline
	// across the chunks and the trailing packet meant the last transfer could
	// be submitted on an already-expired context and fail a delivered write.
	write := func(b []byte) (int, error) {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		return c.out.WriteContext(ctx, b)
	}

	sent := 0
	for sent < len(buf) {
		end := min(sent+outChunkSize, len(buf))
		n, err := write(buf[sent:end])
		sent += n
		if err != nil {
			if isTimeout(err) && n == 0 {
				return sent, ErrTimeout
			}
			return sent, fmt.Errorf("writing to the device: %w", err)
		}
	}

	if mps := c.out.Desc.MaxPacketSize; mps > 0 && len(buf) > 0 && len(buf)%mps == 0 {
		if _, err := write([]byte{}); err != nil {
			return sent, fmt.Errorf("sending zero-length packet: %w", err)
		}
	}
	return sent, nil
}

// Close releases the interface and the USB context.
func (c *USBConn) Close() error {
	if c.done != nil {
		c.done()
	}
	err := c.dev.Close()
	if cerr := c.ctx.Close(); err == nil {
		err = cerr
	}
	return err
}

// isTimeout reports whether a transfer simply did not complete in time.
//
// gousb cancels the underlying transfer when the context deadline fires, and
// libusb then reports it as cancelled rather than timed out — so both mean the
// same thing to a caller that is polling.
// isAccessErr matches libusb's LIBUSB_ERROR_ACCESS. gousb's macOS auto-detach
// path formats it into a string errors.Is cannot see through, so the text is
// matched too — the same fallback the Jetson transport uses.
func isAccessErr(err error) bool {
	return errors.Is(err, gousb.ErrorAccess) || strings.Contains(err.Error(), "bad access [code")
}

func isTimeout(err error) bool {
	return errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, gousb.TransferTimedOut) ||
		errors.Is(err, gousb.TransferCancelled)
}
