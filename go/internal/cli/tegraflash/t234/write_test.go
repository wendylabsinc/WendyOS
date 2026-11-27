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

// TestRunWriterReflashOntoAlignBoundary reflashes a board that an older CLI
// installed with every partition on a 1 MiB boundary. The layout asks for
// config and data on 4 MiB boundaries, so both now start later than the
// previous installation put them, as they do on a real Jetson. The old /data
// superblock then lies inside the new config partition. The write must put
// both partitions, both GPT copies and the stale-data windows on the new
// offsets, so first boot finds no filesystem at the start of data.
func TestRunWriterReflashOntoAlignBoundary(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "rootfs.img"), []byte("ROOTFS-CONTENT"), 0o644); err != nil {
		t.Fatal(err)
	}
	configImage := bytes.Repeat([]byte{'C'}, 4*mib) // fills its partition, as the real config image does
	if err := os.WriteFile(filepath.Join(dir, "config.img"), configImage, 0o644); err != nil {
		t.Fatal(err)
	}
	part := func(name, id string, size int, file, extra string) string {
		return `<partition name="` + name + `" id="` + id + `" type="data"><allocation_policy>sequential</allocation_policy>` +
			`<filesystem_type>basic</filesystem_type><size>` + strconv.Itoa(size) + `</size><allocation_attribute>0x8</allocation_attribute>` +
			extra + `<filename>` + file + `</filename></partition>`
	}
	load := func(name, boundary string) (string, *Plan) {
		t.Helper()
		xml := `<?xml version="1.0"?><partition_layout version="01.00.0000"><device type="external" instance="0" sector_size="512">` +
			part("APP", "1", 1*mib, "rootfs.img", "") + part("config", "2", 4*mib, "config.img", boundary) + part("data", "3", 16*mib, "", boundary) +
			`</device></partition_layout>`
		layout := filepath.Join(dir, name)
		if err := os.WriteFile(layout, []byte(xml), 0o644); err != nil {
			t.Fatal(err)
		}
		p, err := LoadXMLPlan(layout, dir, "nvme0n1")
		if err != nil {
			t.Fatal(err)
		}
		return layout, p
	}
	offsets := func(p *Plan) [3]int64 {
		return [3]int64{p.Partitions[0].StartSector * sectorSize, p.Partitions[1].StartSector * sectorSize, p.Partitions[2].StartSector * sectorSize}
	}

	// Where the older CLI put the previous installation.
	_, before := load("before.xml", "")
	if got := offsets(before); got != [3]int64{1 * mib, 2 * mib, 6 * mib} {
		t.Fatalf("without align_boundary APP, config and data are placed at %d; the checks below assume 1, 2 and 6 MiB", got)
	}
	oldData := int64(6 * mib)
	layout, plan := load("initrd-flash.xml", `<align_boundary> 4194304 </align_boundary>`)
	if got := offsets(plan); got != [3]int64{1 * mib, 4 * mib, 8 * mib} {
		t.Fatalf("APP, config and data are placed at %d, want 1, 4 and 8 MiB: the layout asks for config and data on 4 MiB boundaries", got)
	}
	app, config, data := int64(1*mib), int64(4*mib), int64(8*mib)

	const diskSize = 32 * mib
	stale := bytes.Repeat([]byte{0xA5}, diskSize)
	binary.LittleEndian.PutUint16(stale[oldData+1024+56:], ext4Magic) // the old /data filesystem
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

	if string(got[app:app+14]) != "ROOTFS-CONTENT" {
		t.Errorf("APP starts with %q, want the image", got[app:app+14])
	}
	if !bytes.Equal(got[config:config+4*mib], configImage) {
		t.Error("the config image is not at 4 MiB")
	}
	if binary.LittleEndian.Uint16(got[oldData+1024+56:]) == ext4Magic {
		t.Error("the old /data superblock survived the reflash")
	}
	isZero := func(b []byte) bool { return bytes.Count(b, []byte{0}) == len(b) }
	isStale := func(b []byte) bool { return bytes.Count(b, []byte{0xA5}) == len(b) }
	end := data + 16*mib
	for _, c := range []struct {
		what      string
		from, to  int64
		wantClear bool
	}{
		{"the rest of APP, which the image leaves empty", app + sectorSize, app + 1*mib, true},
		{"the padding before config, outside every partition", app + 1*mib, config, false},
		{"the start of data", data, data + StaleHeadBytes, true},
		{"the middle of data", data + StaleHeadBytes, end - StaleTailBytes, false},
		{"the end of data", end - StaleTailBytes, end, true},
		{"the free space after the last partition", end, diskSize - gptBackupSectors*sectorSize, false},
	} {
		region := got[c.from:c.to]
		if c.wantClear && !isZero(region) {
			t.Errorf("%s [%d, %d) still holds stale data", c.what, c.from, c.to)
		}
		if !c.wantClear && !isStale(region) {
			t.Errorf("%s [%d, %d) was written, want it left alone", c.what, c.from, c.to)
		}
	}

	// Both GPT copies describe the partitions where they were written.
	resolved, err := plan.ResolveForDevice(diskSize / sectorSize)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyGPT(bytes.NewReader(got), resolved, diskSize); err != nil {
		t.Fatalf("GPT on disk does not match the plan: %v", err)
	}
	backup := got[diskSize-gptBackupSectors*sectorSize:]
	for copyName, entries := range map[string][]byte{"primary": got[2*sectorSize:], "backup": backup} {
		for _, want := range []struct {
			name        string
			number      int
			first, last uint64
		}{
			{"config", 2, uint64(config / sectorSize), uint64(data/sectorSize) - 1},
			{"data", 3, uint64(data / sectorSize), uint64(end/sectorSize) - 1},
		} {
			e := entries[(want.number-1)*gptEntrySize:]
			if first, last := binary.LittleEndian.Uint64(e[32:40]), binary.LittleEndian.Uint64(e[40:48]); first != want.first || last != want.last {
				t.Errorf("%s GPT has %s at LBA %d to %d, want %d to %d", copyName, want.name, first, last, want.first, want.last)
			}
		}
	}
	if string(backup[gptEntrySectors*sectorSize:][:8]) != "EFI PART" {
		t.Error("no backup GPT header on the last sector")
	}
}

// A blank filesystem the layout asks for is created where the partition is
// placed, on its align_boundary.
func TestRunWriterBlankFilesystemOnAlignBoundary(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "rootfs.img"), []byte("ROOTFS-CONTENT"), 0o644); err != nil {
		t.Fatal(err)
	}
	layout := filepath.Join(dir, "initrd-flash.xml")
	xml := `<?xml version="1.0"?><partition_layout version="01.00.0000"><device type="external" instance="0" sector_size="512">
<partition name="APP" id="1" type="data"><allocation_policy>sequential</allocation_policy><filesystem_type>basic</filesystem_type><size>8388608</size><allocation_attribute>8</allocation_attribute><filename>rootfs.img</filename></partition>
<partition name="data" id="2" type="data"><allocation_policy>sequential</allocation_policy><filesystem_type>ext4</filesystem_type><size>67108864</size><allocation_attribute>8</allocation_attribute><align_boundary> 4194304 </align_boundary></partition>
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
	dev, err := os.Open(disk)
	if err != nil {
		t.Fatal(err)
	}
	defer dev.Close()
	// APP ends at 9 MiB, where 1 MiB placement would start data. The next
	// 4 MiB boundary is 12 MiB.
	magicAt := func(partition int64) uint16 {
		magic := make([]byte, 2)
		if _, err := dev.ReadAt(magic, partition+1024+56); err != nil {
			t.Fatal(err)
		}
		return binary.LittleEndian.Uint16(magic)
	}
	if got := magicAt(12 * mib); got != ext4Magic {
		t.Errorf("no ext4 superblock at 12 MiB (magic %#x)", got)
	}
	if got := magicAt(9 * mib); got == ext4Magic {
		t.Error("the ext4 filesystem was created at 9 MiB, off the 4 MiB boundary")
	}
	entry := make([]byte, gptEntrySize)
	if _, err := dev.ReadAt(entry, 2*sectorSize+gptEntrySize); err != nil {
		t.Fatal(err)
	}
	if first := binary.LittleEndian.Uint64(entry[32:40]); first != 12*mib/sectorSize {
		t.Errorf("GPT has data at LBA %d, want %d", first, 12*mib/sectorSize)
	}
}
