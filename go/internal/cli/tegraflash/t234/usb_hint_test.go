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
	got, onUSB := describeTegraUSB(devs, "efd45047", "")
	if onUSB || !strings.HasPrefix(got, "This Jetson is not on USB") || !strings.Contains(got, "session efd45047") {
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
	got, onUSB := describeTegraUSB(devs, "efd45047", "")
	if !onUSB || got != "This Jetson's flashing gadget is on USB but is not showing the disk: 1d6b:0104 (flashing gadget, serial EFD45047, usb 3-1)" {
		t.Fatalf("hint = %q", got)
	}
}

// Before the session is known a 1d6b:0104 gadget used to be reported as this
// Jetson's flashing gadget whatever it was (WDY-4295). Now that takes evidence:
// with none, the gadget may be this Jetson or another board.
func TestDescribeTegraUSBBeforeSession(t *testing.T) {
	devs := []usbDevice{{VID: GadgetVendorID, PID: GadgetProductID, PortPath: "3-1"}}
	if got, onUSB := describeTegraUSB(devs, "", ""); !onUSB || got != "A USB gadget that may be this Jetson or another board is present: 1d6b:0104 (serial none, usb 3-1)" {
		t.Fatalf("gadget without evidence = %q", got)
	}
	if got, _ := describeTegraUSB(devs, "", "3-1"); !strings.Contains(got, "flashing gadget, serial none, usb 3-1") {
		t.Fatalf("gadget on the recovery port = %q", got)
	}
	if got, onUSB := describeTegraUSB(nil, "", ""); onUSB || !strings.Contains(got, "nor its flashing gadget (1d6b:0104) is present") {
		t.Fatalf("empty hint = %q", got)
	}
}

// Without a recovery port there is nothing to tell this Jetson's recovery
// device from another Jetson's, so the message does not claim it.
func TestDescribeTegraUSBRecoveryDeviceWithoutPort(t *testing.T) {
	devs := []usbDevice{{VID: 0x0955, PID: 0x7023, PortPath: "3-2"}}
	if got, onUSB := describeTegraUSB(devs, "efd45047", ""); !onUSB || got != "NVIDIA recovery-mode devices on USB: 0955:7023 (AGX Orin 32GB APX recovery, usb 3-2)" {
		t.Fatalf("hint = %q", got)
	}
}

func withRawInquiry(t *testing.T, raw string) {
	t.Helper()
	previous := scanRawInquiry
	scanRawInquiry = func() string { return raw }
	t.Cleanup(func() { scanRawInquiry = previous })
}

func withHostOS(t *testing.T, goos string) {
	t.Helper()
	previous := hostOS
	hostOS = goos
	t.Cleanup(func() { hostOS = previous })
}

// lunTimeout lets a LUN wait time out against the given USB bus and raw LUN
// listing, and returns the message.
func lunTimeout(t *testing.T, sel LUNSelector, goos, raw string, devs []usbDevice) string {
	t.Helper()
	withFastUMSPoll(t)
	withHostOS(t, goos)
	withRawInquiry(t, raw)
	withUMSScan(t, func() ([]UMSDisk, error) { return nil, nil })
	withUSBScan(t, func() ([]usbDevice, error) { return devs, nil })
	_, err := WaitForUMSDiskAt(context.Background(), sel, 3*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "timed out waiting for USB storage") {
		t.Fatalf("wait = %v, want a timeout", err)
	}
	return err.Error()
}

// flashpkgTimeout is the first wait of a flash timing out: no session yet, and
// the Jetson was in recovery mode at usb 1-1.
func flashpkgTimeout(t *testing.T, goos string, devs ...usbDevice) string {
	t.Helper()
	return lunTimeout(t, LUNSelector{Vendor: FlashpkgVendor, PortPath: "1-1", PortHint: true}, goos, "", devs)
}

// A running WendyOS board and the flashing initrd's gadget share 1d6b:0104;
// their product strings are gadget-setup.sh's and meta-tegra's
// initrd-flash.scheme.in's.
var (
	bystanderBoard = usbDevice{VID: GadgetVendorID, PID: GadgetProductID, Serial: "1421425005379", Product: "WendyOS Device brave-otter", PortPath: "4-1"}
	flashGadget    = usbDevice{VID: GadgetVendorID, PID: GadgetProductID, Serial: "ddb4ab3d", Product: "Linux for Tegra", PortPath: "1-1"}
)

const bystanderLabel = `1d6b:0104 (running board "WendyOS Device brave-otter", serial 1421425005379, usb 4-1)`
const bystanderListed = "Other USB gadgets present (not this Jetson): " + bystanderLabel

// Before the session is known, a running WendyOS board on the same computer
// must not be reported as this Jetson's flashing gadget (WDY-4295). With this
// Jetson nowhere else on USB, the host cannot rule out that the running board
// is this Jetson after it started its installed WendyOS, so the message says so.
func TestFlashpkgTimeoutDoesNotBlameBystanderBoard(t *testing.T) {
	got := flashpkgTimeout(t, "linux", bystanderBoard)
	if !strings.Contains(got, "This Jetson is not on USB: neither its recovery device (0955:*) nor its flashing gadget (1d6b:0104) is present.") {
		t.Errorf("message does not say the Jetson is absent:\n%s", got)
	}
	if !strings.Contains(got, "Other USB gadgets present (not this Jetson, unless it started WendyOS instead of the flashing image): "+bystanderLabel) {
		t.Errorf("bystander is not listed as another board:\n%s", got)
	}
	if strings.Contains(got, "flashing gadget, serial 1421425005379") {
		t.Errorf("bystander is called the flashing gadget:\n%s", got)
	}
}

// A running board on the port this Jetson was in recovery mode on is not a
// bystander: the Jetson may have started its installed WendyOS instead of the
// flashing image.
func TestFlashpkgTimeoutRunningBoardOnRecoveryPort(t *testing.T) {
	board := bystanderBoard
	board.PortPath = "1-1"
	got := flashpkgTimeout(t, "darwin", board)
	if !strings.Contains(got, `A USB gadget that may be this Jetson or another board is present: 1d6b:0104 (running board "WendyOS Device brave-otter", serial 1421425005379, usb 1-1)`) {
		t.Errorf("running board on the recovery port:\n%s", got)
	}
	for _, claim := range []string{"not this Jetson", "not on USB", "Allow accessory"} {
		if strings.Contains(got, claim) {
			t.Errorf("message claims %q without evidence:\n%s", claim, got)
		}
	}
}

// Evidence that a gadget is this Jetson: it is on the port the Jetson was in
// recovery mode on, or it is the only gadget with the flashing initrd's
// product string (the gadget can come up on the connector's other USB port).
func TestFlashpkgTimeoutTellsOurGadgetFromBystander(t *testing.T) {
	onRecoveryPort := usbDevice{VID: GadgetVendorID, PID: GadgetProductID, Serial: "ddb4ab3d", PortPath: "1-1"}
	offPort := flashGadget
	offPort.PortPath = "1-2"
	for name, ours := range map[string]usbDevice{"on the recovery port": onRecoveryPort, "by product string": offPort} {
		t.Run(name, func(t *testing.T) {
			got := flashpkgTimeout(t, "linux", bystanderBoard, ours)
			want := "This Jetson's flashing gadget is on USB but is not showing the disk: 1d6b:0104 (flashing gadget, serial ddb4ab3d, usb " + ours.PortPath + ")"
			if !strings.Contains(got, want) || !strings.Contains(got, bystanderListed) {
				t.Errorf("message = \n%s\nwant %q and the bystander listed separately", got, want)
			}
		})
	}
}

func TestFlashpkgTimeoutOurGadgetOnly(t *testing.T) {
	got := flashpkgTimeout(t, "linux", flashGadget)
	if !strings.Contains(got, "This Jetson's flashing gadget is on USB but is not showing the disk: 1d6b:0104 (flashing gadget, serial ddb4ab3d, usb 1-1)") {
		t.Errorf("message does not say the gadget is up without its disk:\n%s", got)
	}
	if strings.Contains(got, "Other USB gadgets") || strings.Contains(got, "not on USB") {
		t.Errorf("message reports devices that are not there:\n%s", got)
	}
}

// A gadget with no evidence either way (no product string, not on the recovery
// port) may be this Jetson or another board, and the message says just that.
func TestFlashpkgTimeoutUnidentifiedGadgets(t *testing.T) {
	one := usbDevice{VID: GadgetVendorID, PID: GadgetProductID, PortPath: "3-1"}
	got := flashpkgTimeout(t, "linux", one)
	if !strings.Contains(got, "A USB gadget that may be this Jetson or another board is present: 1d6b:0104 (serial none, usb 3-1)") {
		t.Errorf("one unidentified gadget:\n%s", got)
	}
	two := flashGadget
	two.PortPath, two.Serial = "3-2", "aaaaaaaa"
	offPort := flashGadget
	offPort.PortPath = "1-2"
	got = flashpkgTimeout(t, "linux", offPort, two)
	if !strings.Contains(got, `2 USB gadgets that may be this Jetson or other boards are present: 1d6b:0104 ("Linux for Tegra", serial ddb4ab3d, usb 1-2), 1d6b:0104 ("Linux for Tegra", serial aaaaaaaa, usb 3-2)`) {
		t.Errorf("two flashing gadgets off the recovery port:\n%s", got)
	}
	for _, claim := range []string{"flashing gadget, serial", "not on USB", "not this Jetson"} {
		if strings.Contains(got, claim) {
			t.Errorf("message claims %q without evidence:\n%s", claim, got)
		}
	}
}

func TestFlashpkgTimeoutNothingOnUSB(t *testing.T) {
	got := flashpkgTimeout(t, "linux", usbDevice{VID: 0x046d, PID: 0xc52b, PortPath: "3-2"})
	want := `timed out waiting for USB storage "flashpkg" at port "1-1"/session ""
No USB mass-storage LUNs are currently visible to this computer.
This Jetson is not on USB: neither its recovery device (0955:*) nor its flashing gadget (1d6b:0104) is present.`
	if got != want {
		t.Errorf("message = \n%s\nwant\n%s", got, want)
	}
}

// A Jetson in recovery mode is this Jetson only on the port it was picked at.
func TestFlashpkgTimeoutJetsonInRecovery(t *testing.T) {
	got := flashpkgTimeout(t, "linux", usbDevice{VID: 0x0955, PID: 0x7523, PortPath: "1-1"}, bystanderBoard, flashGadgetAt("3-2"))
	if !strings.Contains(got, "This Jetson is in USB recovery mode, so the flashing image is not running: 0955:7523 (Orin Nano 8GB APX recovery, usb 1-1)") {
		t.Errorf("recovery mode not reported:\n%s", got)
	}
	if !strings.Contains(got, `(not this Jetson): 1d6b:0104 (running board "WendyOS Device brave-otter", serial 1421425005379, usb 4-1), 1d6b:0104 ("Linux for Tegra", serial ddb4ab3d, usb 3-2)`) {
		t.Errorf("gadgets not listed as other boards while the Jetson is in recovery mode:\n%s", got)
	}
	got = flashpkgTimeout(t, "linux", usbDevice{VID: 0x0955, PID: 0x7023, PortPath: "3-2"})
	if !strings.Contains(got, "This Jetson is not on USB") || !strings.Contains(got, "(not this Jetson): 0955:7023 (AGX Orin 32GB APX recovery, usb 3-2)") {
		t.Errorf("another board in recovery mode passed for this Jetson:\n%s", got)
	}
}

func flashGadgetAt(port string) usbDevice {
	d := flashGadget
	d.PortPath = port
	return d
}

// On macOS an unanswered "Allow accessory to connect?" prompt may be what
// keeps the flashing gadget off USB (unconfirmed). The message names it as a
// possible cause only when nothing that could be this Jetson is visible, and
// only before the gadget was ever seen.
func TestFlashpkgTimeoutMacAccessoryHint(t *testing.T) {
	const hint = `"Allow accessory to connect?"`
	first := LUNSelector{Vendor: FlashpkgVendor, PortPath: "1-1", PortHint: true}
	later := LUNSelector{Vendor: FlashpkgVendor, PortPath: "1-1", PortHint: true, Session: "ddb4ab3d", RecoveryPort: "1-1"}
	for _, tc := range []struct {
		name, goos, raw string
		sel             LUNSelector
		devs            []usbDevice
		want            bool
	}{
		{name: "macOS, nothing on USB", goos: "darwin", sel: first, want: true},
		{name: "macOS, only a bystander board", goos: "darwin", sel: first, devs: []usbDevice{bystanderBoard}, want: true},
		{name: "macOS, unrelated USB disk", goos: "darwin", sel: first, raw: `  - vendor="Samsung" product="PSSD T7" bsd="disk6" size=1000204886016`, want: true},
		{name: "Linux", goos: "linux", sel: first},
		{name: "Windows", goos: "windows", sel: first},
		{name: "macOS, our gadget is up", goos: "darwin", sel: first, devs: []usbDevice{flashGadget}},
		{name: "macOS, a gadget that may be ours", goos: "darwin", sel: first, devs: []usbDevice{{VID: GadgetVendorID, PID: GadgetProductID, PortPath: "3-1"}}},
		{name: "macOS, Jetson in recovery mode", goos: "darwin", sel: first, devs: []usbDevice{{VID: 0x0955, PID: 0x7523, PortPath: "1-1"}}},
		{name: "macOS, flashpkg LUN without a disk node", goos: "darwin", sel: first, raw: `  - vendor="flashpkg" product="ddb4ab3d" bsd="(no BSD name)" size=0`},
		{name: "macOS, after the gadget was adopted", goos: "darwin", sel: later},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := lunTimeout(t, tc.sel, tc.goos, tc.raw, tc.devs)
			if strings.Contains(got, hint) != tc.want {
				t.Errorf("accessory hint shown = %v, want %v:\n%s", !tc.want, tc.want, got)
			}
			if tc.want && !strings.Contains(got, "one possible cause") {
				t.Errorf("hint reads as a diagnosis, not a possible cause:\n%s", got)
			}
		})
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
