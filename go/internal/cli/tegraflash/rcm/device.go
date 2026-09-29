//go:build darwin || linux

// USB device handling for the RCM stage (bootROM level).
// USB transfer mechanics translated from NVIDIA tegrarcm usb.c
// (BSD 3-Clause License, Copyright (c) 2011-2016 NVIDIA CORPORATION)
package rcm

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"time"

	"github.com/google/gousb"
)

// isUSBAccessErr reports whether err is a libusb LIBUSB_ERROR_ACCESS (-3), raised
// when the OS refuses to let wendy seize the recovery device — on macOS because
// its own kernel driver is bound to the re-enumerated device (root needed), on
// Linux because of udev permissions. gousb's macOS auto-detach path wraps the
// libusb error in a formatted string that errors.Is cannot see through, so we also
// match the "bad access [code" text — the same fallback used at the ADB stage.
func isUSBAccessErr(err error) bool {
	return errors.Is(err, gousb.ErrorAccess) ||
		strings.Contains(err.Error(), "bad access [code")
}

// Device represents a Jetson in RCM mode.
type Device struct {
	ctx    *gousb.Context
	dev    *gousb.Device
	iface  *gousb.Interface
	in     *gousb.InEndpoint
	out    *gousb.OutEndpoint
	doneFn func()
}

func openDevice(ctx *gousb.Context, dev *gousb.Device) (*Device, error) {
	// On Linux a kernel driver bound to the interface makes the claim fail with
	// "busy"; auto-detach clears it. On macOS auto-detach must stay OFF: libusb
	// implements detach there as a device capture that requires root, so it
	// fails with ERROR_ACCESS on any device a kernel driver matched. The T234
	// (Orin) APX device is a composite device that AppleUSBHostCompositeDevice
	// claims, tripping exactly that path — and no detach is needed, since that
	// driver only sets the configuration and leaves interface 0 unclaimed.
	// (T264/Thor never hit this: its recovery device matches no kernel driver.)
	if runtime.GOOS == "linux" {
		_ = dev.SetAutoDetach(true)
	}

	cfg, err := dev.Config(1)
	if err != nil {
		if isUSBAccessErr(err) {
			return nil, fmt.Errorf("%w: claiming USB interface: %v", ErrUSBAccess, err)
		}
		return nil, fmt.Errorf("claiming config: %w", err)
	}

	iface, done, err := dev.DefaultInterface()
	if err != nil {
		cfg.Close()
		if isUSBAccessErr(err) {
			return nil, fmt.Errorf("%w: claiming USB interface: %v", ErrUSBAccess, err)
		}
		return nil, fmt.Errorf("claiming interface: %w", err)
	}

	// Find bulk IN and OUT endpoints
	var inEP *gousb.InEndpoint
	var outEP *gousb.OutEndpoint

	ifaceDesc := iface.Setting
	for _, ep := range ifaceDesc.Endpoints {
		if ep.TransferType != gousb.TransferTypeBulk {
			continue
		}
		if ep.Direction == gousb.EndpointDirectionIn && inEP == nil {
			inEP, err = iface.InEndpoint(int(ep.Number))
			if err != nil {
				done()
				return nil, fmt.Errorf("opening IN endpoint: %w", err)
			}
		} else if ep.Direction == gousb.EndpointDirectionOut && outEP == nil {
			outEP, err = iface.OutEndpoint(int(ep.Number))
			if err != nil {
				done()
				return nil, fmt.Errorf("opening OUT endpoint: %w", err)
			}
		}
	}

	if inEP == nil || outEP == nil {
		done()
		return nil, fmt.Errorf("device missing bulk IN or OUT endpoints")
	}

	// Do NOT pre-submit a speculative bulk-IN read here. T264's bootROM sends no
	// UID at connect, so the read only times out — and on macOS a timed-out bulk-IN
	// read is destructive: libusb_cancel_transfer aborts the whole endpoint and
	// issues a CLEAR_FEATURE/ENDPOINT_HALT, desyncing the data toggle (libusb #1110).
	// Read the chip ID via the EP0 control transfer instead (ReadChipID).
	return &Device{
		ctx:    ctx,
		dev:    dev,
		iface:  iface,
		in:     inEP,
		out:    outEP,
		doneFn: done,
	}, nil
}

func (d *Device) String() string {
	desc := d.dev.Desc
	return fmt.Sprintf("NVIDIA 0x%04x:0x%04x", uint16(desc.Vendor), uint16(desc.Product))
}

func (d *Device) Close() {
	if d.doneFn != nil {
		d.doneFn()
	}
	d.dev.Close()
	d.ctx.Close()
}

// ReadWithTimeout reads from the bulk IN endpoint, returning when buf is filled,
// the device sends a short packet, or the timeout elapses. Pass a max-packet-sized
// buf (>= 512 for high-speed bulk): a sub-packet read length can error on macOS IOKit.
func (d *Device) ReadWithTimeout(buf []byte, timeout time.Duration) (int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return d.in.ReadContext(ctx, buf)
}

// ReadChipID reads BR_CID over EP0 on this already-claimed device handle.
func (d *Device) ReadChipID() (string, error) {
	buf := make([]byte, 96)
	n, err := d.dev.Control(0x80, 0x06, 0x0303, 0, buf)
	if err != nil {
		return "", err
	}
	return parseChipIDDescriptor(buf, n)
}

// Write sends buf to the bulk OUT endpoint the way the bootROM expects:
//
//   - split into chunks of at most 16 KiB (0x4000) — a single large bulk OUT fails
//     on macOS IOKit with kIOReturnNotResponding;
//   - if the total length is an exact multiple of the endpoint max packet size,
//     follow with a zero-length packet to mark end-of-transfer.
func (d *Device) Write(buf []byte) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	const maxChunk = 0x4000 // 16 KiB
	for off := 0; off < len(buf); {
		end := off + maxChunk
		if end > len(buf) {
			end = len(buf)
		}
		n, err := d.out.WriteContext(ctx, buf[off:end])
		if err != nil {
			return err
		}
		off += n
	}

	if mps := d.out.Desc.MaxPacketSize; mps > 0 && len(buf) > 0 && len(buf)%mps == 0 {
		if _, err := d.out.WriteContext(ctx, []byte{}); err != nil {
			return fmt.Errorf("sending zero-length packet: %w", err)
		}
	}
	return nil
}

// parseChipIDDescriptor extracts the BR_CID from a GET_STRING_DESCRIPTOR (index 3)
// response (buf[0]=bLength, buf[1]=0x03, buf[2:]=UTF-16LE payload). The bootROM
// stores the hex string reversed, so we take each code unit's low byte and reverse.
func parseChipIDDescriptor(buf []byte, n int) (string, error) {
	if n < 4 {
		return "", fmt.Errorf("chip-id descriptor too short: got %d bytes, need at least 4", n)
	}
	length := int(buf[0])
	if length > n {
		length = n
	}
	var sb strings.Builder
	for i := 2; i+1 < length; i += 2 {
		c := buf[i]
		if !isHexDigit(c) {
			return "", fmt.Errorf("chip-id descriptor byte 0x%02x is not a hex digit", c)
		}
		sb.WriteByte(c)
	}
	if sb.Len() == 0 {
		return "", fmt.Errorf("chip-id descriptor empty")
	}
	return reverseASCII(sb.String()), nil
}

func isHexDigit(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'A' && c <= 'F') || (c >= 'a' && c <= 'f')
}

func reverseASCII(s string) string {
	b := []byte(s)
	for i, j := 0, len(b)-1; i < j; i, j = i+1, j-1 {
		b[i], b[j] = b[j], b[i]
	}
	return string(b)
}
