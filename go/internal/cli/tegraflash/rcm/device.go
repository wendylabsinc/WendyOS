//go:build darwin || linux

// USB device handling for the RCM stage (bootROM level).
// USB transfer mechanics translated from NVIDIA tegrarcm usb.c
// (BSD 3-Clause License, Copyright (c) 2011-2016 NVIDIA CORPORATION)
package rcm

import (
	"context"
	"fmt"
	"time"

	"github.com/google/gousb"
)

type uidResult struct {
	data []byte
	err  error
}

// Device represents a Jetson in RCM mode.
type Device struct {
	ctx    *gousb.Context
	dev    *gousb.Device
	iface  *gousb.Interface
	in     *gousb.InEndpoint
	out    *gousb.OutEndpoint
	doneFn func()
	uidCh  <-chan uidResult
}

// WaitForDevice blocks until an Orin appears in RCM mode (up to 60 s).
func WaitForDevice() (*Device, error) {
	ctx := gousb.NewContext()
	ctx.Debug(0) // suppress libusb noise (LIBUSB_ERROR_INTERRUPTED, etc.)

	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		dev, err := ctx.OpenDeviceWithVIDPID(VendorNVIDIA, ProductOrin)
		if err == nil && dev != nil {
			d, err := openDevice(ctx, dev)
			if err != nil {
				dev.Close()
				return nil, err
			}
			return d, nil
		}
		time.Sleep(500 * time.Millisecond)
	}

	ctx.Close()
	return nil, fmt.Errorf("timed out waiting for Orin in recovery mode")
}

// WaitForNv3p waits for the device to re-enumerate after loading the applet.
// The applet may change the USB PID; we look for any NVIDIA device.
func WaitForNv3p() (*Device, error) {
	ctx := gousb.NewContext()
	ctx.Debug(0) // suppress libusb noise

	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		devs, err := ctx.OpenDevices(func(desc *gousb.DeviceDesc) bool {
			return desc.Vendor == gousb.ID(VendorNVIDIA)
		})
		if err == nil {
			for _, dev := range devs {
				d, err := openDevice(ctx, dev)
				if err == nil {
					return d, nil
				}
				dev.Close()
			}
		}
		time.Sleep(500 * time.Millisecond)
	}

	ctx.Close()
	return nil, fmt.Errorf("timed out waiting for nv3p device")
}

func openDevice(ctx *gousb.Context, dev *gousb.Device) (*Device, error) {
	cfg, err := dev.Config(1)
	if err != nil {
		return nil, fmt.Errorf("claiming config: %w", err)
	}

	iface, done, err := dev.DefaultInterface()
	if err != nil {
		cfg.Close()
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

	// Submit the UID read transfer immediately after endpoint setup. The T234
	// bootROM sends the UID right when the interface is claimed; submitting here
	// (before returning to the caller) maximises the capture window on macOS,
	// where IOKit drops bulk IN data if no transfer is pending.
	ch := make(chan uidResult, 1)
	go func() {
		buf := make([]byte, 16)
		rctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		defer cancel()
		n, rerr := inEP.ReadContext(rctx, buf)
		if rerr != nil {
			ch <- uidResult{err: rerr}
		} else {
			ch <- uidResult{data: buf[:n]}
		}
		close(ch)
	}()

	return &Device{
		ctx:    ctx,
		dev:    dev,
		iface:  iface,
		in:     inEP,
		out:    outEP,
		doneFn: done,
		uidCh:  ch,
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

// Read reads from the bulk IN endpoint.
func (d *Device) Read(buf []byte) (int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return d.in.ReadContext(ctx, buf)
}

// Write writes to the bulk OUT endpoint.
func (d *Device) Write(buf []byte) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err := d.out.WriteContext(ctx, buf)
	return err
}

// ReadUID returns the unique ID sent by the Orin bootROM on first connect.
// The read is pre-submitted in openDevice to avoid missing the UID on macOS.
func (d *Device) ReadUID() ([]byte, error) {
	result, ok := <-d.uidCh
	if !ok {
		return nil, fmt.Errorf("UID channel closed")
	}
	if result.err != nil {
		return nil, result.err
	}
	return result.data, nil
}

// LoadApplet sends the RCM message containing the applet to the device.
// The device verifies (in open mode: always passes) and executes the applet.
// After this call the device re-enumerates; use WaitForNv3p to reconnect.
func (d *Device) LoadApplet(applet []byte) error {
	msg, err := BuildDLMiniloader(applet, [48]byte{})
	if err != nil {
		return fmt.Errorf("building RCM message: %w", err)
	}

	if err := d.Write(msg); err != nil {
		return fmt.Errorf("sending RCM message: %w", err)
	}

	// Read back status word (4 bytes)
	status := make([]byte, 4)
	if _, err := d.Read(status); err != nil {
		// Device may reset before sending status — treat read error as success
		// TODO: verify T234 status response format on real hardware
		return nil
	}
	_ = status
	return nil
}
