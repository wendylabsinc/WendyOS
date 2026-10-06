//go:build darwin || linux || windows

package t234

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func withUSBScan(t *testing.T, scan func() ([]usbDevice, error)) {
	t.Helper()
	previous := scanUSBDevices
	scanUSBDevices = scan
	t.Cleanup(func() { scanUSBDevices = previous })
}

func TestDescribeTegraUSBIgnoresOtherGadgets(t *testing.T) {
	// Another Linux board in device mode shares 1d6b:0104; it must not pass
	// for the Jetson that left USB.
	devs := []usbDevice{{VID: GadgetVendorID, PID: GadgetProductID, Serial: "8cfdf0aeeaee", PortPath: "4-1"}}
	got := describeTegraUSB(devs, "efd45047")
	if !strings.HasPrefix(got, "This Jetson is not on USB") || !strings.Contains(got, "session efd45047") {
		t.Fatalf("hint = %q, want the Jetson reported absent", got)
	}
	if !strings.Contains(got, "not this Jetson): 1d6b:0104 (serial 8cfdf0aeeaee, usb 4-1)") {
		t.Fatalf("hint = %q, want the bystander gadget listed separately", got)
	}
}

func TestDescribeTegraUSBMatchesSession(t *testing.T) {
	devs := []usbDevice{
		{VID: GadgetVendorID, PID: GadgetProductID, Serial: "EFD45047", PortPath: "3-1"},
		{VID: 0x046d, PID: 0xc52b, PortPath: "3-2"},
	}
	got := describeTegraUSB(devs, "efd45047")
	if got != "Tegra USB devices present: 1d6b:0104 (flashing gadget, serial EFD45047, usb 3-1)" {
		t.Fatalf("hint = %q", got)
	}
}

func TestDescribeTegraUSBBeforeSession(t *testing.T) {
	devs := []usbDevice{{VID: GadgetVendorID, PID: GadgetProductID, PortPath: "3-1"}}
	if got := describeTegraUSB(devs, ""); !strings.Contains(got, "flashing gadget, serial none, usb 3-1") {
		t.Fatalf("hint = %q", got)
	}
	if got := describeTegraUSB(nil, ""); !strings.Contains(got, "or flashing gadget (1d6b:0104) is present") {
		t.Fatalf("empty hint = %q", got)
	}
}

func withMissingGrace(t *testing.T, d time.Duration) {
	t.Helper()
	previousGrace, previousInterval := gadgetMissingGrace, gadgetCheckInterval
	gadgetMissingGrace, gadgetCheckInterval = d, 0
	t.Cleanup(func() { gadgetMissingGrace, gadgetCheckInterval = previousGrace, previousInterval })
}

func TestRootfsWaitReportsMissingGadgetOnceAndKeepsWaiting(t *testing.T) {
	withFastUMSPoll(t)
	withMissingGrace(t, time.Millisecond)
	calls := 0
	withUMSScan(t, func() ([]UMSDisk, error) {
		if calls > 0 { // the rootfs appears only after the hint fired
			return []UMSDisk{{DevPath: "/dev/rootfs", Vendor: "mmcblk0", PortPath: "1-3", Serial: "12345678"}}, nil
		}
		return nil, nil
	})
	withUSBScan(t, func() ([]usbDevice, error) {
		return []usbDevice{{VID: GadgetVendorID, PID: GadgetProductID, Serial: "aaaaaaaa", PortPath: "1-4"}}, nil
	})
	sel := LUNSelector{Vendor: "mmcblk0", PortPath: "1-3", Session: "12345678", OnMissing: func(time.Duration) { calls++ }}
	disk, err := waitForUMSDiskConfirmed(context.Background(), sel, time.Minute)
	if err != nil || disk.DevPath != "/dev/rootfs" {
		t.Fatalf("wait = %+v, %v; want the rootfs disk after the hint", disk, err)
	}
	if calls != 1 {
		t.Fatalf("OnMissing called %d times, want 1", calls)
	}
}

// A Jetson back in recovery on this flash's recovery port rebooted: the flash
// cannot finish, so both LUN waits fail at once instead of timing out.
func TestLUNWaitsFailWhenJetsonBackInRecovery(t *testing.T) {
	withFastUMSPoll(t)
	withMissingGrace(t, time.Hour)
	withUMSScan(t, func() ([]UMSDisk, error) { return nil, nil })
	withUSBScan(t, func() ([]usbDevice, error) {
		return []usbDevice{{VID: 0x0955, PID: 0x7023, PortPath: "3-1"}}, nil
	})
	sel := LUNSelector{Vendor: "rootfs", PortPath: "4-1", Session: "12345678", RecoveryPort: "3-1"}
	if _, err := waitForUMSDiskConfirmed(context.Background(), sel, time.Minute); !errors.Is(err, ErrJetsonInRecovery) {
		t.Fatalf("rootfs wait = %v, want ErrJetsonInRecovery", err)
	}
	if _, err := WaitForUMSDiskAt(context.Background(), sel, time.Minute); !errors.Is(err, ErrJetsonInRecovery) {
		t.Fatalf("final-status wait = %v, want ErrJetsonInRecovery", err)
	}
}

// Another board in recovery mode on a different port is not this Jetson.
func TestLUNWaitIgnoresRecoveryDeviceOnOtherPort(t *testing.T) {
	withFastUMSPoll(t)
	withMissingGrace(t, time.Hour)
	withUMSScan(t, func() ([]UMSDisk, error) { return nil, nil })
	withUSBScan(t, func() ([]usbDevice, error) {
		return []usbDevice{{VID: 0x0955, PID: 0x7023, PortPath: "3-2"}}, nil
	})
	sel := LUNSelector{Vendor: "rootfs", PortPath: "4-1", Session: "12345678", RecoveryPort: "3-1"}
	if _, err := waitForUMSDiskConfirmed(context.Background(), sel, 50*time.Millisecond); err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("wait = %v, want a timeout", err)
	}
}

func TestRootfsWaitQuietWhileGadgetOnUSB(t *testing.T) {
	withFastUMSPoll(t)
	withMissingGrace(t, time.Millisecond)
	withUMSScan(t, func() ([]UMSDisk, error) { return nil, nil })
	withUSBScan(t, func() ([]usbDevice, error) {
		return []usbDevice{{VID: GadgetVendorID, PID: GadgetProductID, Serial: "12345678", PortPath: "1-3"}}, nil
	})
	sel := LUNSelector{Vendor: "mmcblk0", PortPath: "1-3", Session: "12345678",
		OnMissing: func(time.Duration) { t.Fatal("OnMissing fired while the session gadget was on USB") }}
	_, err := waitForUMSDiskConfirmed(context.Background(), sel, 50*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("wait error = %v, want timeout", err)
	}
}
