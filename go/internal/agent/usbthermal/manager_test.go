package usbthermal

import (
	"os"
	"path/filepath"
	"testing"
)

const exxonStableID = "by-id:usb-TOPDON_TECHNOLOGY_Co.__Ltd._TC001_Max_YFDVS000000000001-thermal"

// TestListFindsTheTC001Max: the camera is found from sysfs alone, by its USB
// ids, and nothing else on the bus is.
func TestListFindsTheTC001Max(t *testing.T) {
	b := newFakeBus(t)
	b.plug("1-2", "0bda", "5489", 1, 2, "", "Generic", "4-Port USB 2.0 Hub")
	b.plug("1-2.1", "0c45", "6366", 1, 4, "SN0001", "Arducam Technology Co., Ltd.", "USB 2.0 Camera")
	b.plugTC001Max("1-2.3", 15)
	// Interface directories and root hubs have idVendor-less or different
	// shapes; they must be skipped, not misread.
	if err := os.MkdirAll(filepath.Join(b.sysfs, "bus", "usb", "devices", "1-2.3:1.1"), 0o755); err != nil {
		t.Fatal(err)
	}
	b.plug("usb1", "1d6b", "0002", 1, 1, "", "Linux", "xHCI Host Controller")
	m := newTestManager(t, b, nil)

	got := m.List()
	if len(got) != 1 {
		t.Fatalf("listed %d cameras, want the TC001 Max only: %+v", len(got), got)
	}
	want := Listing{
		ID:       IDBandStart,
		StableID: exxonStableID,
		Name:     "TC001 Max (thermal)",
		Model:    "TC001 Max",
		Serial:   "YFDVS000000000001",
		Port:     "1-2.3",
		Layout:   Layout{Width: 256, Height: 192},
	}
	if got[0] != want {
		t.Fatalf("listing\n got %+v\nwant %+v", got[0], want)
	}
	if id, ok := m.ResolveStableID(exxonStableID); !ok || id != IDBandStart {
		t.Fatalf("ResolveStableID = %d, %v", id, ok)
	}
	if l, ok := m.Lookup(IDBandStart); !ok || l.StableID != exxonStableID {
		t.Fatalf("Lookup(%d) = %+v, %v", IDBandStart, l, ok)
	}
	if _, ok := m.Lookup(IDBandStart + 1); ok {
		t.Fatal("Lookup answered for an id no camera holds")
	}
	if _, ok := m.ResolveStableID("by-id:usb-TOPDON_TECHNOLOGY_Co.__Ltd._TC001_Max_YFDVS00000000000"); ok {
		t.Fatal("a prefix of the stable id resolved: the match must be exact")
	}
}

// TestTwinsSharingASerial: two cameras with the same factory serial cannot
// share one by-id name; the second is named by its port, as udev does.
func TestTwinsSharingASerial(t *testing.T) {
	b := newFakeBus(t)
	b.plugTC001Max("1-2.3", 15)
	b.plugTC001Max("1-2.4", 16)
	m := newTestManager(t, b, nil)
	got := m.List()
	if len(got) != 2 {
		t.Fatalf("listed %d cameras, want 2", len(got))
	}
	if got[0].StableID != exxonStableID || got[1].StableID != "by-path:usb-1-2.4-thermal" {
		t.Fatalf("stable ids %q and %q", got[0].StableID, got[1].StableID)
	}
	if got[0].ID != IDBandStart || got[1].ID != IDBandStart+1 {
		t.Fatalf("device ids %d and %d", got[0].ID, got[1].ID)
	}
}

// TestNoSerialMeansByPath: a camera with no serial has only its port.
func TestNoSerialMeansByPath(t *testing.T) {
	b := newFakeBus(t)
	b.plug("3-1", "3474", "4962", 3, 7, "", "", "")
	m := newTestManager(t, b, nil)
	got := m.List()
	if len(got) != 1 || got[0].StableID != "by-path:usb-3-1-thermal" || got[0].Model != "TC001 Max" {
		t.Fatalf("listing %+v", got)
	}
}

func TestUdevClean(t *testing.T) {
	cases := map[string]string{
		"TOPDON TECHNOLOGY Co., Ltd.": "TOPDON_TECHNOLOGY_Co.__Ltd.",
		"  TC001   Max ":              "TC001_Max",
		"a/b\\c*d":                    "a_b_c_d",
		"keep #+-.:=@_":               "keep_#+-.:=@_",
		"Ünïcode":                     "Ünïcode",
	}
	for in, want := range cases {
		if got := udevClean(in); got != want {
			t.Errorf("udevClean(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestDeviceIDs: ids are stable across rescans and unplugs, skip numbers a
// real /dev/videoN holds, and are reused only when the band is full.
func TestDeviceIDs(t *testing.T) {
	b := newFakeBus(t)
	b.plugTC001Max("1-2.3", 15)
	m := newTestManager(t, b, func(id uint32) bool { return id == IDBandStart })
	if got := m.List(); len(got) != 1 || got[0].ID != IDBandStart+1 {
		t.Fatalf("listing %+v: id %d belongs to a /dev/video node, want %d", got, IDBandStart, IDBandStart+1)
	}
	b.unplug("1-2.3")
	if got := m.List(); len(got) != 0 {
		t.Fatalf("unplugged camera still listed: %+v", got)
	}
	b.plug("1-1", "3474", "4962", 1, 20, "OTHER", "TOPDON TECHNOLOGY Co., Ltd.", "TC001 Max")
	b.plugTC001Max("1-2.3", 21)
	got := m.List()
	if len(got) != 2 {
		t.Fatalf("listing %+v", got)
	}
	ids := map[string]uint32{got[0].StableID: got[0].ID, got[1].StableID: got[1].ID}
	if ids[exxonStableID] != IDBandStart+1 {
		t.Fatalf("the replugged camera moved from id %d to %d", IDBandStart+1, ids[exxonStableID])
	}

	// Fill the band with cameras that come and go: a ninth camera may take
	// the number of one that is no longer attached.
	full := newFakeBus(t)
	mf := newTestManager(t, full, nil)
	for i := range IDBandEnd - IDBandStart + 1 {
		full.plug("2-"+string(rune('a'+i)), "3474", "4962", 2, 10+i, "S"+string(rune('a'+i)), "V", "TC001 Max")
	}
	if got := mf.List(); len(got) != IDBandEnd-IDBandStart+1 {
		t.Fatalf("listed %d of a full band", len(got))
	}
	full.unplug("2-a")
	full.plug("2-z", "3474", "4962", 2, 99, "Sz", "V", "TC001 Max")
	var newcomer *Listing
	for _, l := range mf.List() {
		if l.Serial == "Sz" {
			newcomer = &l
		}
	}
	if newcomer == nil || newcomer.ID != IDBandStart {
		t.Fatalf("the ninth camera got %+v, want the id the unplugged one freed (%d)", newcomer, IDBandStart)
	}
}
