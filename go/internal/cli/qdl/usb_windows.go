//go:build windows

package qdl

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/tegraflash/winusb"
	"golang.org/x/sys/windows"
)

const VendorQualcomm = 0x05c6
const ProductEDL = 0x9008

func Supported() bool { return true }

// Windows uses inbox WinUSB, with a separately versioned Qualcomm binding.
// List uses SetupAPI and works before the driver is installed.
func List() ([]DeviceInfo, error) {
	devices, err := winusb.ListVendor(VendorQualcomm)
	if err != nil {
		return nil, err
	}
	var out []DeviceInfo
	for _, d := range devices {
		if !isWindowsEDL(d) {
			continue
		}
		out = append(out, DeviceInfo{Serial: edlField(d.Product, "SN"), CID: edlField(d.Product, "CID"),
			Instance: d.InstanceID, Location: d.LocationPath})
	}
	return out, nil
}

func isWindowsEDL(d winusb.Device) bool {
	if d.VID != VendorQualcomm || d.PID != ProductEDL {
		return false
	}
	for _, id := range d.CompatibleIDs {
		for _, protocol := range []string{"10", "11", "13", "FF"} {
			if strings.EqualFold(id, `USB\Class_FF&SubClass_FF&Prot_`+protocol) {
				return true
			}
		}
	}
	return false
}

type USBConn struct {
	dev  *winusb.USBDevice
	info DeviceInfo
}

func Open(want DeviceInfo) (*USBConn, error) {
	devices, err := List()
	if err != nil {
		return nil, err
	}
	var matches []DeviceInfo
	for _, d := range devices {
		if d.matches(want) {
			matches = append(matches, d)
		}
	}
	if len(matches) != 1 {
		return nil, fmt.Errorf("expected one matching EDL device, found %d", len(matches))
	}
	info := matches[0]
	d, err := winusb.OpenProfileInstance(winusb.Device{InstanceID: info.Instance, VID: VendorQualcomm, PID: ProductEDL}, winusb.DragonwingDriver())
	if err != nil {
		return nil, errors.Join(ErrUSBAccess, err)
	}
	return &USBConn{d, info}, nil
}

func (c *USBConn) Info() DeviceInfo { return c.info }
func (c *USBConn) Close() error     { c.dev.Close(); return nil }

func windowsTimeout(err error) bool {
	return errors.Is(err, windows.ERROR_SEM_TIMEOUT) || errors.Is(err, windows.ERROR_TIMEOUT)
}

func (c *USBConn) Read(p []byte, timeout time.Duration) (int, error) {
	if err := c.dev.SetTransferTimeout(timeout, true); err != nil {
		return 0, err
	}
	n, err := c.dev.ReadBulk(p)
	return edlReadResult(n, err)
}

func edlReadResult(n int, err error) (int, error) {
	if windowsTimeout(err) {
		if n > 0 {
			return n, nil
		}
		return 0, ErrTimeout
	}
	if err != nil {
		return n, err
	}
	if n == 0 {
		return 0, ErrTimeout
	}
	return n, nil
}

func (c *USBConn) Write(p []byte, timeout time.Duration) (int, error) {
	if err := c.dev.SetTransferTimeout(timeout, false); err != nil {
		return 0, err
	}
	return edlWrite(p, int(c.dev.OutMaxPacket()), c.dev.WriteBulk)
}

// Keep packet-sized chunks and append one ZLP at the logical transfer end,
// never between chunks. Reject short writes instead of spinning or truncating.
func edlWrite(p []byte, maxPacket int, write func([]byte) (int, error)) (int, error) {
	const chunk = 16 * 1024
	sent := 0
	for sent < len(p) {
		end := min(sent+chunk, len(p))
		n, err := write(p[sent:end])
		want := end - sent
		sent += n
		if err != nil {
			if windowsTimeout(err) && sent == 0 {
				err = errors.Join(ErrTimeout, err)
			}
			return sent, err
		}
		if n != want {
			return sent, io.ErrShortWrite
		}
	}
	if maxPacket > 0 && len(p) > 0 && len(p)%maxPacket == 0 {
		if _, err := write(nil); err != nil {
			// The payload has already been delivered. Do not mark this as a
			// retryable timeout: Firehose would resend the entire command.
			return sent, fmt.Errorf("sending zero-length packet: %w", err)
		}
	}
	return sent, nil
}
