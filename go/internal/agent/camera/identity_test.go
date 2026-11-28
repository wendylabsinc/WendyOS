package camera

import (
	"os"
	"path/filepath"
	"testing"
)

// identityFixture is a sysfs tree: class/<base> entries whose device link
// points into devices/, the way /sys/class/video4linux does.
type identityFixture struct {
	t    *testing.T
	root string
}

func newIdentityFixture(t *testing.T) *identityFixture {
	t.Helper()
	root := t.TempDir()
	prev := sysfsRoot
	sysfsRoot = filepath.Join(root, "class")
	t.Cleanup(func() { sysfsRoot = prev })
	if err := os.MkdirAll(sysfsRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	return &identityFixture{t: t, root: root}
}

// node adds class/<base> with index and name; a non-empty port links its
// device to devices/<port>.
func (f *identityFixture) node(base, port, index, name string) {
	f.t.Helper()
	dir := filepath.Join(sysfsRoot, base)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		f.t.Fatal(err)
	}
	if port != "" {
		target := filepath.Join(f.root, "devices", port)
		if err := os.MkdirAll(target, 0o755); err != nil {
			f.t.Fatal(err)
		}
		rel, err := filepath.Rel(dir, target)
		if err != nil {
			f.t.Fatal(err)
		}
		if err := os.Symlink(rel, filepath.Join(dir, "device")); err != nil {
			f.t.Fatal(err)
		}
	}
	for file, value := range map[string]string{"index": index, "name": name} {
		if err := os.WriteFile(filepath.Join(dir, file), []byte(value+"\n"), 0o644); err != nil {
			f.t.Fatal(err)
		}
	}
}

// device is the resolved directory a port's link points at.
func (f *identityFixture) device(port string) string {
	f.t.Helper()
	dir, err := filepath.EvalSymlinks(filepath.Join(f.root, "devices", port))
	if err != nil {
		f.t.Fatal(err)
	}
	return dir
}

func TestIdentityOf_ReadsPortIndexAndName(t *testing.T) {
	f := newIdentityFixture(t)
	f.node("video0", "usb1/1-2/1-2.1/1-2.1:1.0", "0", "Brio 101")
	f.node("video1", "usb1/1-2/1-2.1/1-2.1:1.0", "1", "Brio 101")
	f.node("video254", "", "0", "Brio 101") // a v4l2loopback node: no device link

	got, err := IdentityOf("video0")
	if err != nil {
		t.Fatalf("IdentityOf(video0): %v", err)
	}
	want := Identity{Device: f.device("usb1/1-2/1-2.1/1-2.1:1.0"), Index: "0", Name: "Brio 101"}
	if got != want {
		t.Errorf("IdentityOf(video0) = %+v, want %+v", got, want)
	}

	if _, err := IdentityOf("video254"); err == nil {
		t.Error("IdentityOf(video254) without a device link succeeded")
	}
	if _, err := IdentityOf("video9"); err == nil {
		t.Error("IdentityOf of a missing node succeeded")
	}

	present := PresentIdentities()
	if len(present) != 2 || present["video0"] != want || present["video1"].Index != "1" {
		t.Errorf("PresentIdentities() = %+v, want video0 and video1 only", present)
	}
}

// attributes writes sysfs attribute files into devices/<dir>.
func (f *identityFixture) attributes(dir string, values map[string]string) {
	f.t.Helper()
	target := filepath.Join(f.root, "devices", dir)
	if err := os.MkdirAll(target, 0o755); err != nil {
		f.t.Fatal(err)
	}
	for file, value := range values {
		if err := os.WriteFile(filepath.Join(target, file), []byte(value+"\n"), 0o644); err != nil {
			f.t.Fatal(err)
		}
	}
}

func TestUSBOf_ReadsWhatTheCamerasOwnUSBDeviceSays(t *testing.T) {
	f := newIdentityFixture(t)
	// A hub with a serial, and below it a camera with one and a camera without.
	f.attributes("usb1/1-2", map[string]string{"idVendor": "05e3", "idProduct": "0610", "serial": "HUB1"})
	f.attributes("usb1/1-2/1-2.1", map[string]string{"idVendor": "046d", "idProduct": "094c", "serial": "2331AP04K2S8"})
	f.attributes("usb1/1-2/1-2.1/1-2.1:1.0", map[string]string{"bInterfaceNumber": "00"})
	f.attributes("usb1/1-2/1-2.1/1-2.1:1.2", map[string]string{"bInterfaceNumber": "02"})
	f.attributes("usb1/1-2/1-2.2", map[string]string{"idVendor": "0c45", "idProduct": "6366"})
	f.attributes("usb1/1-2/1-2.2/1-2.2:1.0", map[string]string{"bInterfaceNumber": "00"})
	f.node("video0", "usb1/1-2/1-2.1/1-2.1:1.0", "0", "Brio 101")
	f.node("video2", "usb1/1-2/1-2.1/1-2.1:1.2", "0", "Brio 101")
	f.node("video4", "usb1/1-2/1-2.2/1-2.2:1.0", "0", "C920")
	f.node("video6", "usb1/1-2/1-2.2", "0", "C920") // a driver that hangs its node off the USB device
	f.node("video8", "platform/tegra-capture-vi/video4linux", "0", "vi-output, imx219 9-0010")
	f.node("video254", "", "0", "Brio 101") // a v4l2loopback node: no device link

	for base, want := range map[string]USB{
		"video0":   {Vendor: "046d", Product: "094c", Serial: "2331AP04K2S8", Interface: "00"},
		"video2":   {Vendor: "046d", Product: "094c", Serial: "2331AP04K2S8", Interface: "02"},
		"video4":   {Vendor: "0c45", Product: "6366", Interface: "00"},
		"video6":   {Vendor: "0c45", Product: "6366"},
		"video8":   {},
		"video254": {},
		"video9":   {},
	} {
		if got := USBOf(base); got != want {
			t.Errorf("USBOf(%s) = %+v, want %+v", base, got, want)
		}
	}
}

func TestResolve_SamePortWins(t *testing.T) {
	p1 := Identity{Device: "/sys/devices/usb1/1-1/1-1:1.0", Index: "0", Name: "Brio 101"}
	p2 := Identity{Device: "/sys/devices/usb1/1-2/1-2:1.0", Index: "0", Name: "Brio 101"}
	present := map[string]Identity{"video0": p1, "video2": p2}
	none := func(Identity) bool { return false }

	if base, ok := Resolve(p2, present, none); !ok || base != "video2" {
		t.Errorf("Resolve(p2) = %q, %v; want video2", base, ok)
	}
	if base, ok := Resolve(p1, present, none); !ok || base != "video0" {
		t.Errorf("Resolve(p1) = %q, %v; want video0", base, ok)
	}
}

func TestResolve_MovedCameraIsFollowedWhenItsNameIsUnique(t *testing.T) {
	stored := Identity{Device: "/sys/devices/usb1/1-1/1-1:1.0", Index: "0", Name: "Brio 101"}
	moved := Identity{Device: "/sys/devices/usb1/1-3/1-3:1.0", Index: "0", Name: "Brio 101"}
	present := map[string]Identity{
		"video2": moved,
		"video3": {Device: moved.Device, Index: "1", Name: "Brio 101"}, // its metadata node
		"video4": {Device: "/sys/devices/usb1/1-4/1-4:1.0", Index: "0", Name: "C920"},
	}
	if base, ok := Resolve(stored, present, func(Identity) bool { return false }); !ok || base != "video2" {
		t.Errorf("Resolve(moved camera) = %q, %v; want video2", base, ok)
	}

	// A second camera of the same name and index makes the move ambiguous.
	present["video6"] = Identity{Device: "/sys/devices/usb1/1-5/1-5:1.0", Index: "0", Name: "Brio 101"}
	if base, ok := Resolve(stored, present, func(Identity) bool { return false }); ok {
		t.Errorf("Resolve with two candidates = %q, want none", base)
	}

	// Nothing of that name: nothing to follow.
	if base, ok := Resolve(stored, map[string]Identity{"video4": present["video4"]}, nil); ok {
		t.Errorf("Resolve with no candidate = %q, want none", base)
	}
}

func TestResolve_SameModelCamerasNeverCross(t *testing.T) {
	p1 := Identity{Device: "/sys/devices/usb1/1-1/1-1:1.0", Index: "0", Name: "Brio 101"}
	p2 := Identity{Device: "/sys/devices/usb1/1-2/1-2:1.0", Index: "0", Name: "Brio 101"}
	records := map[Identity]bool{p1: true, p2: true}
	claimed := func(id Identity) bool { return records[id] }
	present := map[string]Identity{"video2": p2} // only p2 is plugged in

	if base, ok := Resolve(p1, present, claimed); ok {
		t.Errorf("Resolve(p1) = %q; the camera on p2 belongs to p2's record", base)
	}
	if base, ok := Resolve(p2, present, claimed); !ok || base != "video2" {
		t.Errorf("Resolve(p2) = %q, %v; want video2", base, ok)
	}
}
