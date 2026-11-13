package robotarm

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/protobuf/proto"
)

type linuxFixture struct{ root, tty, dev string }

func newLinuxFixture(t *testing.T) linuxFixture {
	t.Helper()
	root := t.TempDir()
	f := linuxFixture{root, filepath.Join(root, "sys/class/tty"), filepath.Join(root, "dev")}
	for _, p := range []string{f.tty, f.dev} {
		if err := os.MkdirAll(p, 0700); err != nil {
			t.Fatal(err)
		}
	}
	return f
}
func (f linuxFixture) add(t *testing.T, node, location, serial, vid, pid string) {
	t.Helper()
	// CDC ACM ttys hang directly off their USB interface.
	f.link(t, node, f.usbInterface(t, location, serial, vid, pid), "usb")
}

// usbInterface creates a USB device at location and returns its interface 1.0.
func (f linuxFixture) usbInterface(t *testing.T, location, serial, vid, pid string) string {
	t.Helper()
	usb := filepath.Join(f.root, "sys/devices/usb1", location)
	iface := filepath.Join(usb, location+":1.0")
	if err := os.MkdirAll(iface, 0700); err != nil {
		t.Fatal(err)
	}
	for k, v := range map[string]string{"idVendor": vid, "idProduct": pid, "serial": serial, "manufacturer": "Adapter vendor", "product": "USB serial"} {
		if err := os.WriteFile(filepath.Join(usb, k), []byte(v+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(iface, "bInterfaceNumber"), []byte("01\n"), 0600); err != nil {
		t.Fatal(err)
	}
	return iface
}

// link points a tty's device at endpoint and gives endpoint a subsystem.
func (f linuxFixture) link(t *testing.T, node, endpoint, subsystem string) {
	t.Helper()
	if err := os.MkdirAll(endpoint, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(f.root, "sys/bus", subsystem), filepath.Join(endpoint, "subsystem")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(f.tty, node), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(endpoint, filepath.Join(f.tty, node, "device")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.dev, node), nil, 0000); err != nil {
		t.Fatal(err)
	} // Metadata works even for inaccessible/busy ports.
}
func (f linuxFixture) scan(t *testing.T) []*pb.RobotArm {
	t.Helper()
	ds, err := DiscoverLinux(context.Background(), f.tty, f.dev)
	if err != nil {
		t.Fatal(err)
	}
	return ds
}

func TestLinuxUSBMetadataClassificationAndRenumbering(t *testing.T) {
	f := newLinuxFixture(t)
	f.add(t, "ttyACM0", "1-2", "A", "1a86", "55d3")
	f.add(t, "ttyUSB0", "1-3", "B", "0403", "6001") // Generic FTDI, not labelled an arm.
	if err := os.MkdirAll(filepath.Join(f.tty, "ttyS0"), 0700); err != nil {
		t.Fatal(err)
	}
	ds := f.scan(t)
	if len(ds) != 2 || !ds[0].Candidate || ds[1].Candidate || ds[0].Verified || ds[0].Configured {
		t.Fatalf("incorrect classification: %v", ds)
	}
	d := ds[0]
	if d.Id != "usb:1a86:55d3:s:41:1" || d.SerialNumber != "A" || d.UsbLocation != "1-2" || d.AccessStatus != "unknown" {
		t.Fatalf("lost USB metadata: %v", d)
	}
	saved := proto.Clone(d).(*pb.RobotArm)
	saved.Configured = true
	saved.Model = "so101"
	saved.Name = "Follower"
	if err := os.Rename(filepath.Join(f.tty, "ttyACM0"), filepath.Join(f.tty, "ttyACM7")); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(f.dev, "ttyACM0"), filepath.Join(f.dev, "ttyACM7")); err != nil {
		t.Fatal(err)
	}
	merged := Merge(f.scan(t), []*pb.RobotArm{saved})
	if merged[1].DevicePath != filepath.Join(f.dev, "ttyACM7") || !merged[1].Configured || merged[1].Name != "Follower" {
		t.Fatalf("renumbering lost binding: %v", merged)
	}
	if err := os.Remove(filepath.Join(f.dev, "ttyACM7")); err != nil {
		t.Fatal(err)
	}
	merged = Merge(f.scan(t), []*pb.RobotArm{saved})
	if merged[1].Connected || merged[1].DevicePath != "" || merged[1].ConnectionStatus != "disconnected" {
		t.Fatalf("unplug lost/offline binding: %v", merged)
	}
	// Replug in another USB socket with the same serial remains the same binding.
	f.add(t, "ttyACM9", "1-4", "A", "1a86", "55d3")
	merged = Merge(f.scan(t), []*pb.RobotArm{saved})
	if !merged[1].Connected || merged[1].UsbLocation != "1-4" || merged[1].Name != "Follower" {
		t.Fatalf("replug failed: %v", merged)
	}
}
func TestLinuxUSBSerialPortsOnlyNotOtherTTYsOnUSB(t *testing.T) {
	f := newLinuxFixture(t)
	// usb-serial drivers (ch341, ftdi_sio, cp210x) put a port device below the interface.
	f.link(t, "ttyUSB0", filepath.Join(f.usbInterface(t, "1-2", "A", "1a86", "7523"), "ttyUSB0"), "usb-serial")
	// An rfcomm tty on a USB Bluetooth controller also has a USB ancestor.
	f.link(t, "rfcomm0", filepath.Join(f.usbInterface(t, "1-10", "", "8087", "0026"), "bluetooth/hci0/hci0:256"), "bluetooth")
	ds := f.scan(t)
	if len(ds) != 1 || ds[0].DevicePath != filepath.Join(f.dev, "ttyUSB0") || ds[0].UsbInterface != "1" || !ds[0].Candidate {
		t.Fatalf("want only the usb-serial port: %v", ds)
	}
}
func TestLinuxMissingSerialAndDuplicateIdentity(t *testing.T) {
	f := newLinuxFixture(t)
	f.add(t, "ttyACM0", "1-2", "", "1a86", "55d3")
	f.add(t, "ttyACM1", "1-3", "", "1a86", "55d3")
	ds := f.scan(t)
	if len(ds) != 2 || ds[0].Id == ds[1].Id || ds[0].IdentityKind != "usb_location" {
		t.Fatalf("missing serial collapsed adapters: %v", ds)
	}
	f.add(t, "ttyUSB0", "1-4", "duplicate", "1a86", "7523")
	f.add(t, "ttyUSB1", "1-5", "duplicate", "1a86", "7523")
	merged := Merge(f.scan(t), nil)
	ambiguous := 0
	for _, d := range merged {
		if d.ConnectionStatus == "ambiguous" {
			ambiguous++
			if d.Connected {
				t.Fatal("duplicate connected")
			}
		}
	}
	if ambiguous != 2 {
		t.Fatalf("duplicates silently selected: %v", merged)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := DiscoverLinux(ctx, f.tty, f.dev); err == nil {
		t.Fatal("cancel ignored")
	}
	if _, err := DiscoverLinux(context.Background(), "/nonexistent-wendy-test", f.dev); err == nil {
		t.Fatal("missing inventory reported as empty")
	}
}

func TestLiveLinuxMetadata(t *testing.T) {
	if runtime.GOOS != "linux" || os.Getenv("WENDY_USB_METADATA_SMOKE") != "1" {
		t.Skip("read-only Linux metadata smoke is opt-in")
	}
	devices, err := Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range devices {
		if d.VendorId == "" || d.ProductId == "" || d.Verified || d.AccessStatus != "unknown" {
			t.Fatalf("invalid metadata: %v", d)
		}
	}
	t.Logf("Linux sysfs/devfs metadata: %d USB serial endpoints (no serial opens)", len(devices))
}
