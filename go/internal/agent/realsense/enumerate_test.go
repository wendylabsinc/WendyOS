package realsense

import (
	"os"
	"path/filepath"
	"testing"
)

// fakeSysfs builds the sysfs shape Detect walks: real device directories
// under sys/devices, and sys/class/video4linux/videoN symlinks whose
// `device` link points at the owning USB interface — the same indirection
// the kernel publishes, built at test time because symlinks don't belong in
// testdata.
type fakeCamera struct {
	usbPath string // e.g. "1-4"
	vid     string
	pid     string
	serial  string
	nodes   map[string]string // videoN -> interface number
	name    string
}

func fakeSysfs(t *testing.T, cams []fakeCamera) string {
	t.Helper()
	root := t.TempDir()
	classDir := filepath.Join(root, "sys/class/video4linux")
	if err := os.MkdirAll(classDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, cam := range cams {
		devDir := filepath.Join(root, "sys/devices/usb1", cam.usbPath)
		write := func(path, content string) {
			t.Helper()
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(content+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		write(filepath.Join(devDir, "idVendor"), cam.vid)
		write(filepath.Join(devDir, "idProduct"), cam.pid)
		write(filepath.Join(devDir, "serial"), cam.serial)
		for node, iface := range cam.nodes {
			ifaceDir := filepath.Join(devDir, cam.usbPath+":1."+iface)
			write(filepath.Join(ifaceDir, "bInterfaceNumber"), iface)
			nodeDir := filepath.Join(ifaceDir, "video4linux", node)
			write(filepath.Join(nodeDir, "name"), cam.name)
			if err := os.Symlink(ifaceDir, filepath.Join(nodeDir, "device")); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(nodeDir, filepath.Join(classDir, node)); err != nil {
				t.Fatal(err)
			}
		}
	}
	return root
}

func TestDetectOneD435i(t *testing.T) {
	// The G1's camera as observed: depth function on interface 00 with its
	// metadata node, colour on 03 likewise.
	root := fakeSysfs(t, []fakeCamera{{
		usbPath: "1-4", vid: "8086", pid: "0b3a", serial: "337122071091",
		name: "Intel(R) RealSense(TM) Depth Camera 435i",
		nodes: map[string]string{
			"video0": "00", "video1": "00", // capture + metadata
			"video2": "02", "video3": "02", // IR
			"video4": "03", "video5": "03", // colour + metadata
		},
	}})
	cams, err := Detect(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(cams) != 1 {
		t.Fatalf("got %d cameras, want 1: %+v", len(cams), cams)
	}
	c := cams[0]
	if c.Serial != "337122071091" || c.Model != "D435i" || c.PID != "0b3a" {
		t.Errorf("identity: %+v", c)
	}
	if c.DepthNode != "/dev/video0" {
		t.Errorf("depth node = %q, want /dev/video0 (capture, not metadata)", c.DepthNode)
	}
	if c.ColourNode != "/dev/video4" {
		t.Errorf("colour node = %q, want /dev/video4", c.ColourNode)
	}
}

func TestDetectRenumberedNodes(t *testing.T) {
	// The same camera after a power cycle, at high node numbers: identity
	// must come from sysfs, never from node order.
	root := fakeSysfs(t, []fakeCamera{{
		usbPath: "1-4", vid: "8086", pid: "0b3a", serial: "337122071091",
		name: "Intel(R) RealSense(TM) Depth Camera 435i",
		nodes: map[string]string{
			"video10": "03", "video11": "03",
			"video6": "00", "video7": "00",
		},
	}})
	cams, err := Detect(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(cams) != 1 || cams[0].DepthNode != "/dev/video6" || cams[0].ColourNode != "/dev/video10" {
		t.Fatalf("got %+v", cams)
	}
}

func TestDetectIgnoresOtherCameras(t *testing.T) {
	root := fakeSysfs(t, []fakeCamera{
		{
			usbPath: "1-2", vid: "046d", pid: "085e", serial: "ABC",
			name:  "Logitech BRIO",
			nodes: map[string]string{"video0": "00"},
		},
		{
			// Orbbec: a different vendor's drawer, not this one.
			usbPath: "1-3", vid: "2bc5", pid: "0403", serial: "ORB1",
			name:  "Astra Pro",
			nodes: map[string]string{"video2": "00"},
		},
	})
	cams, err := Detect(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(cams) != 0 {
		t.Fatalf("got %+v, want none", cams)
	}
}

func TestDetectTwoCamerasAndUnknownModel(t *testing.T) {
	root := fakeSysfs(t, []fakeCamera{
		{
			usbPath: "1-4", vid: "8086", pid: "0b3a", serial: "AAA",
			name:  "Intel(R) RealSense(TM) Depth Camera 435i",
			nodes: map[string]string{"video0": "00", "video2": "03"},
		},
		{
			usbPath: "1-5", vid: "8086", pid: "0bff", serial: "BBB",
			name:  "Intel(R) RealSense(TM) Depth Camera 999",
			nodes: map[string]string{"video4": "00"},
		},
	})
	cams, err := Detect(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(cams) != 2 {
		t.Fatalf("got %d cameras: %+v", len(cams), cams)
	}
	if cams[0].Serial != "AAA" || cams[0].Model != "D435i" {
		t.Errorf("first: %+v", cams[0])
	}
	// Unknown PID still enumerates — with no model name, so depth gating can
	// fail visibly rather than wrongly — and may lack a colour node.
	if cams[1].Serial != "BBB" || cams[1].Model != "" || cams[1].ColourNode != "" {
		t.Errorf("second: %+v", cams[1])
	}
}
