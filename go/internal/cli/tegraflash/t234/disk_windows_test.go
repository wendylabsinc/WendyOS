//go:build windows

package t234

import (
	"encoding/binary"
	"testing"
)

// buildDescriptor assembles a minimal STORAGE_DEVICE_DESCRIPTOR with the given
// vendor/product strings and bus type.
func buildDescriptor(vendor, product string, busType uint32) []byte {
	head := 36
	buf := make([]byte, head+len(vendor)+1+len(product)+1)
	binary.LittleEndian.PutUint32(buf[0:], 1)                // Version
	binary.LittleEndian.PutUint32(buf[4:], uint32(len(buf))) // Size
	vOff := head
	pOff := head + len(vendor) + 1
	binary.LittleEndian.PutUint32(buf[12:], uint32(vOff)) // VendorIdOffset
	binary.LittleEndian.PutUint32(buf[16:], uint32(pOff)) // ProductIdOffset
	binary.LittleEndian.PutUint32(buf[28:], busType)      // BusType
	copy(buf[vOff:], vendor)
	copy(buf[pOff:], product)
	return buf
}

func TestParseStorageDeviceDescriptor(t *testing.T) {
	// The gadget splits "<export><serial>" across the fixed-width INQUIRY
	// vendor (8) and product (16) fields; the descriptor carries them verbatim.
	buf := buildDescriptor("flashpkg", "12ab34cd", busTypeUsb)
	vendor, product, busType, err := parseStorageDeviceDescriptor(buf)
	if err != nil {
		t.Fatal(err)
	}
	if vendor != "flashpkg" || product != "12ab34cd" || busType != busTypeUsb {
		t.Fatalf("got vendor=%q product=%q bus=%d", vendor, product, busType)
	}
	if name, serial := splitInquiry(vendor, product); name != "flashpkg" || serial != "12ab34cd" {
		t.Fatalf("splitInquiry = %q/%q", name, serial)
	}

	// Zero offsets (fields absent) yield empty strings, not a slice panic.
	buf = buildDescriptor("", "", busTypeUsb)
	binary.LittleEndian.PutUint32(buf[12:], 0)
	binary.LittleEndian.PutUint32(buf[16:], 0)
	if vendor, product, _, err = parseStorageDeviceDescriptor(buf); err != nil || vendor != "" || product != "" {
		t.Fatalf("absent fields: vendor=%q product=%q err=%v", vendor, product, err)
	}

	// An offset pointing past the buffer is ignored, not read out of bounds.
	buf = buildDescriptor("x", "y", busTypeUsb)
	binary.LittleEndian.PutUint32(buf[12:], 4096)
	if vendor, _, _, err = parseStorageDeviceDescriptor(buf); err != nil || vendor != "" {
		t.Fatalf("out-of-range offset: vendor=%q err=%v", vendor, err)
	}

	// Truncated descriptor errors instead of misparsing.
	if _, _, _, err = parseStorageDeviceDescriptor(make([]byte, 20)); err == nil {
		t.Fatal("short descriptor should error")
	}
}

func TestPhysicalDriveNumber(t *testing.T) {
	cases := []struct {
		in   string
		n    uint32
		want bool
	}{
		{`\\.\PhysicalDrive0`, 0, true},
		{`\\.\PhysicalDrive12`, 12, true},
		{`\\.\PhysicalDrive`, 0, false},
		{`\\.\PhysicalDrive1x`, 0, false},
		{`/dev/sdb`, 0, false},
	}
	for _, tc := range cases {
		n, ok := physicalDriveNumber(tc.in)
		if ok != tc.want || (ok && n != tc.n) {
			t.Fatalf("physicalDriveNumber(%q) = %d,%v want %d,%v", tc.in, n, ok, tc.n, tc.want)
		}
	}
}

// TestGadgetPortMap pins the composite-gadget normalization: when usbccgp
// splits the gadget into MI_xx function devnodes, the USBSTOR disk parents to
// the MI child — whose location path (#USBMI suffix) and synthesized trailer
// would break the recovery-port correlation — so the map
// must resolve function nodes to the composite root's location path.
func TestGadgetPortMap(t *testing.T) {
	const rootLoc = `PCIROOT(0)#PCI(1400)#USBROOT(0)#USB(2)`
	root := usbDeviceNode{
		InstanceID: `USB\VID_1D6B&PID_0104\F3885343`,
		VID:        GadgetVendorID, PID: GadgetProductID,
		LocationPath: rootLoc,
	}
	mi := usbDeviceNode{
		InstanceID: `USB\VID_1D6B&PID_0104&MI_00\7&2C54F607&0&0000`,
		VID:        GadgetVendorID, PID: GadgetProductID,
		ParentInstanceID: root.InstanceID,
		LocationPath:     rootLoc + `#USBMI(0)`,
	}

	ports := gadgetPortMap([]usbDeviceNode{root, mi})
	if got := ports[`USB\VID_1D6B&PID_0104&MI_00\7&2C54F607&0&0000`]; got != rootLoc {
		t.Fatalf("MI function node port = %q, want root location %q", got, rootLoc)
	}
	if got := ports[`USB\VID_1D6B&PID_0104\F3885343`]; got != rootLoc {
		t.Fatalf("root node port = %q, want %q", got, rootLoc)
	}

	// Single-function gadget: the root is the USBSTOR parent; nothing to resolve.
	ports = gadgetPortMap([]usbDeviceNode{root})
	if got := ports[`USB\VID_1D6B&PID_0104\F3885343`]; got != rootLoc {
		t.Fatalf("single-function port = %q, want %q", got, rootLoc)
	}

	// An orphaned MI node (root missing from the scan) keeps its own path
	// rather than mapping to "".
	ports = gadgetPortMap([]usbDeviceNode{mi})
	if got := ports[`USB\VID_1D6B&PID_0104&MI_00\7&2C54F607&0&0000`]; got != rootLoc+`#USBMI(0)` {
		t.Fatalf("orphan MI node port = %q", got)
	}
}
