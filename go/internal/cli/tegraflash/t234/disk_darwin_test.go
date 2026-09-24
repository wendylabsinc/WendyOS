//go:build darwin

package t234

import "testing"

func TestMacUSBPortPathMatchesLibusbTopology(t *testing.T) {
	// macOS locationID: bus 20, downstream ports 1 then 2.
	if got := macUSBPortPath(0x14120000); got != "20-1.2" {
		t.Fatalf("port key = %q, want 20-1.2", got)
	}
}

// ioregNestedHub reproduces `ioreg -rc IOUSBHostDevice -l -w0` on an Apple
// Silicon Mac where the Jetson flashpkg gadget enumerates behind the machine's
// internal USB 2.0 hub: the hub is the sole column-0 IOUSBHostDevice and the
// gadget is nested inside it. The required fields (idVendor, locationID, SCSI
// Vendor Identification, whole-disk BSD Name) all live in the gadget's subtree.
const ioregNestedHub = `+-o USB2.0 Hub@00100000  <class IOUSBHostDevice, id 0x100000abc, registered, matched, active, busy 0 (0 ms), retain 30>
  | {
  |   "idVendor" = 1452
  |   "idProduct" = 32789
  |   "USB Vendor Name" = "Apple"
  |   "USB Product Name" = "USB2.0 Hub"
  |   "locationID" = 1048576
  | }
  | 
  | +-o Linux for Tegra@00120000  <class IOUSBHostDevice, id 0x100034f5c, registered, matched, active, busy 0 (0 ms), retain 20>
  | | {
  | |   "idVendor" = 7531
  | |   "idProduct" = 260
  | |   "locationID" = 1179648
  | |   "USB Serial Number" = "ddb4ab3d"
  | | }
  | | 
  | | +-o Mass Storage@0  <class IOUSBHostInterface, id 0x100034f60, registered, matched, active, busy 0 (0 ms), retain 12>
  | | | +-o IOSCSILogicalUnitNub@0  <class IOSCSILogicalUnitNub, id 0x100034f80, registered, matched, active, busy 0 (0 ms), retain 8>
  | | | | {
  | | | |   "Vendor Identification" = "flashpkg"
  | | | |   "Product Identification" = "ddb4ab3d"
  | | | | }
  | | | | +-o IOSCSIPeripheralDeviceType00  <class IOSCSIPeripheralDeviceType00, id 0x100034f90>
  | | | | | +-o IOBlockStorageServices  <class IOBlockStorageServices, id 0x100034fa0>
  | | | | | | +-o flashpkg ddb4ab3d Media  <class IOMedia, id 0x100034fb0, registered, matched, active>
  | | | | | | | {
  | | | | | | |   "BSD Name" = "disk4"
  | | | | | | |   "Whole" = Yes
  | | | | | | |   "Leaf" = Yes
  | | | | | | |   "Size" = 134217728
  | | | | | | | }
`

// ioregDirect is the same gadget attached directly to a root port (no internal
// hub), where it is a column-0 IOUSBHostDevice — the known-good topology.
const ioregDirect = `+-o Linux for Tegra@00100000  <class IOUSBHostDevice, id 0x100034f5c, registered, matched, active, busy 0 (0 ms), retain 20>
  | {
  |   "idVendor" = 7531
  |   "idProduct" = 260
  |   "locationID" = 1048576
  |   "USB Serial Number" = "ddb4ab3d"
  | }
  | +-o IOSCSILogicalUnitNub@0  <class IOSCSILogicalUnitNub, id 0x100034f80>
  | | {
  | |   "Vendor Identification" = "flashpkg"
  | |   "Product Identification" = "ddb4ab3d"
  | | }
  | | +-o flashpkg ddb4ab3d Media  <class IOMedia, id 0x100034fb0>
  | | | {
  | | |   "BSD Name" = "disk4"
  | | |   "Whole" = Yes
  | | |   "Size" = 134217728
  | | | }
`

func assertOneFlashpkg(t *testing.T, disks []UMSDisk, wantPort string) {
	t.Helper()
	if len(disks) != 1 {
		t.Fatalf("parseUMSDisks returned %d disks, want 1: %+v", len(disks), disks)
	}
	d := disks[0]
	if d.DevPath != "/dev/disk4" || d.RawPath != "/dev/rdisk4" {
		t.Errorf("dev paths = %q/%q, want /dev/disk4 /dev/rdisk4", d.DevPath, d.RawPath)
	}
	if d.Vendor != "flashpkg" {
		t.Errorf("vendor = %q, want flashpkg", d.Vendor)
	}
	if d.Serial != "ddb4ab3d" {
		t.Errorf("serial = %q, want ddb4ab3d", d.Serial)
	}
	if d.PortPath != wantPort {
		t.Errorf("port = %q, want %q", d.PortPath, wantPort)
	}
	if d.SizeBytes != 134217728 {
		t.Errorf("size = %d, want 134217728", d.SizeBytes)
	}
}

// TestParseUMSDisksNestedHub is the WDY-2621 regression: the flashpkg LUN must
// be found even when the gadget is nested under the Mac's internal USB 2.0 hub.
func TestParseUMSDisksNestedHub(t *testing.T) {
	assertOneFlashpkg(t, parseUMSDisks(ioregNestedHub), "0-1.2")
}

// TestParseUMSDisksDirect confirms the column-0 (direct-attach) topology still
// parses correctly.
func TestParseUMSDisksDirect(t *testing.T) {
	assertOneFlashpkg(t, parseUMSDisks(ioregDirect), "0-1")
}

// TestSplitIoregSubtreesSplitsNestedDevice guards the specific defect: the
// nested gadget must become its own chunk, not be absorbed into the hub's.
func TestSplitIoregSubtreesSplitsNestedDevice(t *testing.T) {
	chunks := splitIoregSubtrees(ioregNestedHub)
	if len(chunks) != 2 {
		t.Fatalf("got %d chunks, want 2 (hub + nested gadget)", len(chunks))
	}
	if ioregInt(chunks[0], "idVendor") != 1452 {
		t.Errorf("chunk 0 idVendor = %d, want 1452 (hub)", ioregInt(chunks[0], "idVendor"))
	}
	if ioregInt(chunks[1], "idVendor") != GadgetVendorID {
		t.Errorf("chunk 1 idVendor = %d, want %d (gadget)", ioregInt(chunks[1], "idVendor"), GadgetVendorID)
	}
}

func TestParseUSBDevicesReadsSerialBehindHub(t *testing.T) {
	devs := parseUSBDevices(ioregNestedHub)
	if len(devs) != 2 {
		t.Fatalf("parseUSBDevices returned %d devices, want hub + gadget: %+v", len(devs), devs)
	}
	g := devs[1]
	if g.VID != GadgetVendorID || g.PID != GadgetProductID || g.Serial != "ddb4ab3d" || g.PortPath != "0-1.2" {
		t.Fatalf("gadget = %+v", g)
	}
}

// A two-LUN gadget: LUN 0 without a medium, LUN 1 with one. Each LUN's
// inquiry must pair with its own IOMedia.
const ioregTwoLUNs = `+-o Linux for Tegra@00100000  <class IOUSBHostDevice, id 0x100034f5c, registered, matched, active, busy 0 (0 ms), retain 20>
  | {
  |   "idVendor" = 7531
  |   "idProduct" = 260
  |   "locationID" = 1048576
  |   "USB Serial Number" = "ddb4ab3d"
  | }
  | +-o IOSCSILogicalUnitNub@0  <class IOSCSILogicalUnitNub, id 0x100034f80>
  | | {
  | |   "Vendor Identification" = "flashpkg"
  | |   "Product Identification" = "ddb4ab3d"
  | | }
  | +-o IOSCSILogicalUnitNub@1  <class IOSCSILogicalUnitNub, id 0x100034f81>
  | | {
  | |   "Vendor Identification" = "rootfs"
  | |   "Product Identification" = "ddb4ab3d"
  | | }
  | | +-o rootfs ddb4ab3d Media  <class IOMedia, id 0x100034fb1>
  | | | {
  | | |   "BSD Name" = "disk5"
  | | |   "Whole" = Yes
  | | |   "Size" = 63585648640
  | | | }
`

func TestParseUMSDisksPairsEachLUNWithItsMedia(t *testing.T) {
	disks := parseUMSDisks(ioregTwoLUNs)
	if len(disks) != 1 {
		t.Fatalf("parseUMSDisks returned %d disks, want only the LUN with media: %+v", len(disks), disks)
	}
	if d := disks[0]; d.Vendor != "rootfs" || d.DevPath != "/dev/disk5" || d.SizeBytes != 63585648640 || d.PortPath != "0-1" {
		t.Fatalf("disk = %+v", d)
	}
}
