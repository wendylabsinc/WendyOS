//go:build darwin || linux || windows

package t234

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"unicode/utf8"
)

func TestClampLabel(t *testing.T) {
	cases := []struct {
		name, in string
		max      int
		want     string
	}{
		{"short-ext4", "UDA", ext4LabelMax, "UDA"},
		{"exact-ext4", "0123456789abcdef", ext4LabelMax, "0123456789abcdef"},
		{"long-ext4", "this-label-is-way-too-long", ext4LabelMax, "this-label-is-wa"},
		{"short-fat32", "ESP", fat32LabelMax, "ESP"},
		{"long-fat32", "config-partition", fat32LabelMax, "config-part"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := clampLabel(tc.in, tc.max); got != tc.want {
				t.Fatalf("clampLabel(%q, %d) = %q, want %q", tc.in, tc.max, got, tc.want)
			}
		})
	}
	// A multi-byte rune straddling the limit is dropped whole, not split.
	if got := clampLabel("ααααααααα", ext4LabelMax); len(got) > ext4LabelMax || !utf8.ValidString(got) {
		t.Fatalf("clampLabel truncated a rune: %q (len %d)", got, len(got))
	}
}

func TestRunWriterXMLPlanFileBackedDisk(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "rootfs.img"), []byte("ROOTFS-CONTENT"), 0o644); err != nil {
		t.Fatal(err)
	}
	layout := filepath.Join(dir, "initrd-flash.xml")
	xml := `<?xml version="1.0"?><partition_layout version="01.00.0000"><device type="external" instance="0" sector_size="512">
<partition name="APP" id="1" type="data"><allocation_policy>sequential</allocation_policy><filesystem_type>basic</filesystem_type><size>8388608</size><allocation_attribute>8</allocation_attribute><filename>rootfs.img</filename></partition>
<partition name="data" id="2" type="data"><allocation_policy>sequential</allocation_policy><filesystem_type>ext4</filesystem_type><size>67108864</size><allocation_attribute>8</allocation_attribute></partition>
</device></partition_layout>`
	if err := os.WriteFile(layout, []byte(xml), 0o644); err != nil {
		t.Fatal(err)
	}
	disk := filepath.Join(dir, "disk.img")
	f, err := os.Create(disk)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(96 << 20); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if err := RunWriter(WriterOptions{Device: disk, WritePlan: true, LayoutPath: layout, ImagesDir: dir, RootfsDevice: "nvme0n1"}); err != nil {
		t.Fatal(err)
	}
	p, err := LoadXMLPlan(layout, dir, "nvme0n1")
	if err != nil {
		t.Fatal(err)
	}
	dev, _ := os.Open(disk)
	defer dev.Close()
	buf := make([]byte, len("ROOTFS-CONTENT"))
	if _, err := dev.ReadAt(buf, p.Partitions[0].StartSector*sectorSize); err != nil || string(buf) != "ROOTFS-CONTENT" {
		t.Fatalf("rootfs content = %q, err=%v", buf, err)
	}
	magic := make([]byte, 2)
	if _, err := dev.ReadAt(magic, p.Partitions[1].StartSector*sectorSize+1024+56); err != nil || binary.LittleEndian.Uint16(magic) != ext4Magic {
		t.Fatalf("blank ext4 magic = %x, err=%v", magic, err)
	}
}

// TestRunWriterClearsStalePartitions reflashes a disk that still holds a
// previous installation: every byte is stale, and the empty `data` partition
// starts with an old ext4 superblock. The layout ships `data` and UDA without
// an image, as WendyOS's Jetson layouts do. The write must clear the start and
// end of both, and the unwritten rest of APP, so first boot finds no
// filesystem to reuse; it must keep the image, and must not clear whole
// partitions or anything outside them.
func TestRunWriterClearsStalePartitions(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "rootfs.img"), []byte("ROOTFS-CONTENT"), 0o644); err != nil {
		t.Fatal(err)
	}
	layout := filepath.Join(dir, "initrd-flash.xml")
	part := func(name, id string, size int, file string) string {
		return `<partition name="` + name + `" id="` + id + `" type="data"><allocation_policy>sequential</allocation_policy>` +
			`<filesystem_type>basic</filesystem_type><size>` + strconv.Itoa(size) + `</size><allocation_attribute>0x8</allocation_attribute>` +
			`<filename>` + file + `</filename></partition>`
	}
	xml := `<?xml version="1.0"?><partition_layout version="01.00.0000"><device type="external" instance="0" sector_size="512">` +
		part("APP", "1", 4*mib, "rootfs.img") + part("data", "2", 16*mib, "") + part("UDA", "3", 1*mib, "") +
		`</device></partition_layout>`
	if err := os.WriteFile(layout, []byte(xml), 0o644); err != nil {
		t.Fatal(err)
	}
	p, err := LoadXMLPlan(layout, dir, "nvme0n1")
	if err != nil {
		t.Fatal(err)
	}
	app, data, uda := p.Partitions[0].StartSector*sectorSize, p.Partitions[1].StartSector*sectorSize, p.Partitions[2].StartSector*sectorSize
	if app != 1*mib || data != 5*mib || uda != 21*mib {
		t.Fatalf("partitions placed at %d, %d, %d; the checks below assume 1, 5 and 21 MiB", app, data, uda)
	}

	const diskSize = 32 * mib
	stale := bytes.Repeat([]byte{0xA5}, diskSize)
	binary.LittleEndian.PutUint16(stale[data+1024+56:], ext4Magic) // the old /data filesystem
	disk := filepath.Join(dir, "disk.img")
	if err := os.WriteFile(disk, stale, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := RunWriter(WriterOptions{Device: disk, WritePlan: true, LayoutPath: layout, ImagesDir: dir, RootfsDevice: "nvme0n1"}); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(disk)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != diskSize {
		t.Fatalf("disk grew to %d bytes", len(got))
	}

	isZero := func(b []byte) bool { return bytes.Count(b, []byte{0}) == len(b) }
	isStale := func(b []byte) bool { return bytes.Count(b, []byte{0xA5}) == len(b) }
	if string(got[app:app+14]) != "ROOTFS-CONTENT" {
		t.Fatalf("APP starts with %q, want the image", got[app:app+14])
	}
	for _, c := range []struct {
		what      string
		from, to  int64
		wantClear bool
	}{
		{"the rest of APP, which the image leaves empty", app + sectorSize, data, true},
		{"the start of data, with the old superblock", data, data + StaleHeadBytes, true},
		{"the middle of data", data + StaleHeadBytes, uda - StaleTailBytes, false},
		{"the end of data", uda - StaleTailBytes, uda, true},
		{"UDA, smaller than the two windows", uda, uda + 1*mib, true},
		{"the gap between the primary GPT and APP", 34 * sectorSize, app, false},
		{"the free space after the last partition", uda + 1*mib, diskSize - 33*sectorSize, false},
	} {
		region := got[c.from:c.to]
		if c.wantClear && !isZero(region) {
			t.Errorf("%s [%d, %d) still holds stale data", c.what, c.from, c.to)
		}
		if !c.wantClear && !isStale(region) {
			t.Errorf("%s [%d, %d) was written, want it left alone", c.what, c.from, c.to)
		}
	}
}
