package t234

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func writeXMLFixture(t *testing.T, allocation string) (layout, images string) {
	t.Helper()
	images = t.TempDir()
	if err := os.WriteFile(filepath.Join(images, "rootfs.img"), []byte("rootfs"), 0o644); err != nil {
		t.Fatal(err)
	}
	xml := `<?xml version="1.0"?><partition_layout version="01.00.0000">
<device type="external" instance="0" sector_size="512">
<partition name="master_boot_record" type="protective_master_boot_record"><allocation_policy>sequential</allocation_policy><filesystem_type>basic</filesystem_type><size>512</size><allocation_attribute>8</allocation_attribute></partition>
<partition name="APP" id="1" type="data"><allocation_policy>sequential</allocation_policy><filesystem_type>basic</filesystem_type><size>1048576</size><allocation_attribute>8</allocation_attribute><filename>rootfs.img</filename><partition_type_guid>0FC63DAF-8483-4772-8E79-3D69D8477DE4</partition_type_guid></partition>
<partition name="data" id="2" type="data"><allocation_policy>sequential</allocation_policy><filesystem_type>ext4</filesystem_type><size>1048576</size><allocation_attribute>` + allocation + `</allocation_attribute></partition>
<partition name="secondary_gpt" type="secondary_gpt"><allocation_policy>sequential</allocation_policy><filesystem_type>basic</filesystem_type><size>0xFFFFFFFFFFFFFFFF</size><allocation_attribute>8</allocation_attribute></partition>
</device></partition_layout>`
	layout = filepath.Join(images, "initrd-flash.xml")
	if err := os.WriteFile(layout, []byte(xml), 0o644); err != nil {
		t.Fatal(err)
	}
	return layout, images
}

func TestLoadXMLPlanAndFillToEnd(t *testing.T) {
	layout, images := writeXMLFixture(t, "0x808")
	p, err := LoadXMLPlan(layout, images, "nvme0n1")
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := p.ResolveForDevice(10000)
	if err != nil {
		t.Fatal(err)
	}
	data := resolved.Partitions[1]
	if !data.FillToEnd || data.StartSector+data.SizeSectors != 10000-gptBackupSectors {
		t.Fatalf("fill partition = %+v", data)
	}
}

func TestLoadXMLPlanRejectsMissingImage(t *testing.T) {
	layout, images := writeXMLFixture(t, "8")
	if err := os.Remove(filepath.Join(images, "rootfs.img")); err != nil {
		t.Fatal(err)
	}
	_, err := LoadXMLPlan(layout, images, "nvme0n1")
	if err == nil || !strings.Contains(err.Error(), "references rootfs.img") {
		t.Fatalf("missing-image error = %v", err)
	}
}

func TestLoadXMLPlanRejectsUnsafeMetadataBeforeWriting(t *testing.T) {
	layout, images := writeXMLFixture(t, "8")
	data, err := os.ReadFile(layout)
	if err != nil {
		t.Fatal(err)
	}
	badGUID := strings.Replace(string(data), "</partition_type_guid>", "-not-a-guid</partition_type_guid>", 1)
	if err := os.WriteFile(layout, []byte(badGUID), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadXMLPlan(layout, images, "nvme0n1"); err == nil || !strings.Contains(err.Error(), "partition_type_guid") {
		t.Fatalf("invalid-GUID error = %v", err)
	}

	if _, err := LoadXMLPlan(layout, images, "sda"); err == nil || !strings.Contains(err.Error(), "unsupported rootfs device") {
		t.Fatalf("unsupported-rootfs error = %v", err)
	}
}

func TestLoadXMLPlanRejectsTrailingRootElement(t *testing.T) {
	layout, images := writeXMLFixture(t, "8")
	f, err := os.OpenFile(layout, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("<partition_layout/>"); err != nil {
		f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadXMLPlan(layout, images, "nvme0n1"); err == nil {
		t.Fatal("trailing root element was accepted")
	}
}

func TestLoadXMLPlanMatchesAGXOrinSgdiskGolden(t *testing.T) {
	dir := t.TempDir()
	var xml strings.Builder
	xml.WriteString(`<?xml version="1.0"?><partition_layout version="01.00.0000"><device type="sdmmc_user" instance="3" sector_size="512">`)
	for _, part := range agxOrinEMMCPlan {
		fmt.Fprintf(&xml, `<partition name="%s" id="%d" type="data"><allocation_policy>sequential</allocation_policy><filesystem_type>basic</filesystem_type><size>%d</size><allocation_attribute>8</allocation_attribute>`, part.Name, part.Number, part.SizeSectors*sectorSize)
		if part.File != "" {
			if err := os.WriteFile(filepath.Join(dir, part.File), []byte(part.Name), 0o644); err != nil {
				t.Fatal(err)
			}
			fmt.Fprintf(&xml, `<filename>%s</filename>`, part.File)
		}
		if part.TypeGuid != "" {
			fmt.Fprintf(&xml, `<partition_type_guid>%s</partition_type_guid>`, part.TypeGuid)
		}
		xml.WriteString(`</partition>`)
	}
	xml.WriteString(`</device></partition_layout>`)
	layout := filepath.Join(dir, "initrd-flash.xml")
	if err := os.WriteFile(layout, []byte(xml.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	plan, err := LoadXMLPlan(layout, dir, "mmcblk0")
	if err != nil {
		t.Fatal(err)
	}
	for _, part := range plan.Partitions {
		if want := sgdiskStarts[part.Number]; part.StartSector != want {
			t.Errorf("partition %d (%s) starts at %d, want golden %d", part.Number, part.Name, part.StartSector, want)
		}
	}
}

// orinNanoNVMeStarts is where 1 MiB placement puts each partition of the real
// Jetson Orin Nano NVMe layout in testdata, keyed by partition number. It is
// what the CLI wrote before it read align_boundary: config (16) and data (17)
// each start 1 MiB past a 4 MiB boundary.
var orinNanoNVMeStarts = map[int]int64{
	3: 2048, 4: 264192, 5: 266240, 6: 331776, 7: 593920, 8: 595968,
	9: 661504, 10: 825344, 11: 827392, 12: 958464, 13: 1122304, 14: 1124352,
	15: 1255424, 1: 2074624, 2: 27240448, 16: 52406272, 17: 52930560,
}

// alignBoundaryElement matches one align_boundary line of the layout fixture.
var alignBoundaryElement = regexp.MustCompile(`[ \t]*<align_boundary>[^<]*</align_boundary>\r?\n`)

const fourMiBBoundary = "<align_boundary> 4194304 </align_boundary>"

// orinNanoNVMeLayout writes the real Jetson Orin Nano NVMe layout (testdata,
// verbatim from the WendyOS 0.20.2 flashpack) next to stand-ins for the images
// it names. The layout carries align_boundary 16384 on UDA, APP and APP_b, as
// NVIDIA ships them, and 4194304 on the two partitions WendyOS adds, config
// and data. edit, when not nil, changes the layout before it is written.
func orinNanoNVMeLayout(t *testing.T, edit func(layout string) string) (layout, images string) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "orin-nano-nvme-0.20.2-initrd-flash.xml"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	if n16k, n4m := strings.Count(text, "<align_boundary> 16384 </align_boundary>"), strings.Count(text, fourMiBBoundary); n16k != 3 || n4m != 2 || len(alignBoundaryElement.FindAllString(text, -1)) != 5 {
		t.Fatalf("layout fixture has %d 16 KiB and %d 4 MiB align_boundary elements, want 3 and 2 and no others", n16k, n4m)
	}
	if edit != nil {
		text = edit(text)
	}
	images = t.TempDir()
	for _, name := range []string{"boot.img", "tegra234-p3768-0000+p3767-0005-nv-super.dtb", "esp.img", "wendyos-image.ext4", "config-partition.fat32.img"} {
		if err := os.WriteFile(filepath.Join(images, name), []byte(name), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	layout = filepath.Join(images, "initrd-flash.xml")
	if err := os.WriteFile(layout, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	return layout, images
}

func checkStarts(t *testing.T, plan *Plan, want map[int]int64) {
	t.Helper()
	if len(plan.Partitions) != len(want) {
		t.Fatalf("plan has %d partitions, want %d", len(plan.Partitions), len(want))
	}
	for _, part := range plan.Partitions {
		if start, ok := want[part.Number]; !ok || part.StartSector != start {
			t.Errorf("partition %d (%s) starts at sector %d (byte %d), want sector %d (byte %d)",
				part.Number, part.Name, part.StartSector, part.StartSector*sectorSize, start, start*sectorSize)
		}
	}
}

// The layout asks for config and data on 4 MiB boundaries. Both move to the
// next one, 3 MiB later, and every other partition stays where 1 MiB
// placement puts it: 1 MiB already satisfies NVIDIA's 16 KiB boundaries.
func TestLoadXMLPlanStartsConfigAndDataOnAlignBoundary(t *testing.T) {
	layout, images := orinNanoNVMeLayout(t, nil)
	plan, err := LoadXMLPlan(layout, images, "nvme0n1")
	if err != nil {
		t.Fatal(err)
	}
	want := maps.Clone(orinNanoNVMeStarts)
	want[16], want[17] = 52412416, 52936704
	checkStarts(t, plan, want)
	const fourMiB = (4 << 20) / sectorSize
	for _, part := range plan.Partitions {
		if (part.Name == "config" || part.Name == "data") && part.StartSector%fourMiB != 0 {
			t.Errorf("%s starts at byte %d, which is not on a 4 MiB boundary", part.Name, part.StartSector*sectorSize)
		}
	}
	// The disk the layout needs grows with data's start: data is 512 MiB.
	if got, want := plan.MinDeviceSectors(), int64(52936704+1048576+gptBackupSectors); got != want {
		t.Errorf("MinDeviceSectors = %d, want %d", got, want)
	}
}

// Without the 4 MiB boundaries the same layout is placed exactly as before,
// whether or not NVIDIA's 16 KiB boundaries are there: a boundary below 1 MiB
// never pulls a partition off 1 MiB alignment.
func TestLoadXMLPlanWithoutAlignBoundaryPlacesAsBefore(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(layout string) string
	}{
		{"no align_boundary", func(layout string) string { return alignBoundaryElement.ReplaceAllString(layout, "") }},
		{"only NVIDIA's 16 KiB", func(layout string) string { return strings.ReplaceAll(layout, fourMiBBoundary, "") }},
		{"16 KiB on config and data too", func(layout string) string {
			return strings.ReplaceAll(layout, fourMiBBoundary, "<align_boundary> 16384 </align_boundary>")
		}},
		// Several partitions end off a 1 MiB boundary (the kernel-dtb ones are
		// 768 KiB), so this is the case that tells "at least 1 MiB" from "the
		// boundary as given": taken as given, their successors would start on
		// the next 16 KiB instead.
		{"16 KiB on every partition", func(layout string) string {
			bare := alignBoundaryElement.ReplaceAllString(layout, "")
			return strings.ReplaceAll(bare, "</partition>", "<align_boundary> 16384 </align_boundary></partition>")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			layout, images := orinNanoNVMeLayout(t, func(text string) string {
				edited := tc.edit(text)
				if strings.Contains(edited, fourMiBBoundary) {
					t.Fatal("the edit left a 4 MiB align_boundary in the layout")
				}
				return edited
			})
			plan, err := LoadXMLPlan(layout, images, "nvme0n1")
			if err != nil {
				t.Fatal(err)
			}
			checkStarts(t, plan, orinNanoNVMeStarts)
			if got, want := plan.MinDeviceSectors(), int64(52930560+1048576+gptBackupSectors); got != want {
				t.Errorf("MinDeviceSectors = %d, want %d", got, want)
			}
		})
	}
}

// withAlignBoundary adds an align_boundary element to the data partition of a
// writeXMLFixture layout, where 1 MiB placement starts data at sector 4096.
func withAlignBoundary(t *testing.T, layout, boundary string) {
	t.Helper()
	data, err := os.ReadFile(layout)
	if err != nil {
		t.Fatal(err)
	}
	const anchor = "<filesystem_type>ext4</filesystem_type>"
	if strings.Count(string(data), anchor) != 1 {
		t.Fatalf("layout fixture has no single %s to anchor on", anchor)
	}
	edited := strings.Replace(string(data), anchor, anchor+"<align_boundary>"+boundary+"</align_boundary>", 1)
	if err := os.WriteFile(layout, []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLoadXMLPlanAlignBoundaryValues(t *testing.T) {
	for _, tc := range []struct {
		boundary  string
		wantStart int64 // of data; 0 when the layout must be rejected
	}{
		{"4194304", 8192},
		{" 4194304 ", 8192},
		{"0x400000", 8192},
		{"8388608", 16384},
		{"2097152", 4096},
		{"1048576", 4096},
		// Smaller than 1 MiB: a 1 MiB start is on these boundaries too.
		{"16384", 4096},
		{"512", 4096},
		// An empty element asks for nothing.
		{"", 4096},
		{"  ", 4096},
		// Not a boundary a partition can start on.
		{"0", 0},
		{"0x0", 0},
		{"256", 0},
		{"513", 0},
		{"1000", 0},
		{"4194303", 0},
		// Whole sectors, but not a power of two.
		{"3145728", 0},
		{"6291456", 0},
		// Not a number.
		{"-4194304", 0},
		{"4 MiB", 0},
		{"4194304.5", 0},
	} {
		t.Run(fmt.Sprintf("%q", tc.boundary), func(t *testing.T) {
			layout, images := writeXMLFixture(t, "8")
			withAlignBoundary(t, layout, tc.boundary)
			plan, err := LoadXMLPlan(layout, images, "nvme0n1")
			if tc.wantStart == 0 {
				if err == nil {
					t.Fatalf("align_boundary %q was accepted, data placed at sector %d", tc.boundary, plan.Partitions[1].StartSector)
				}
				if !strings.Contains(err.Error(), "align_boundary") || !strings.Contains(err.Error(), "partition data") {
					t.Fatalf("error does not name the partition and align_boundary: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if app, data := plan.Partitions[0].StartSector, plan.Partitions[1].StartSector; app != 2048 || data != tc.wantStart {
				t.Fatalf("APP and data start at sectors %d and %d, want 2048 and %d", app, data, tc.wantStart)
			}
		})
	}
}

// A fill-to-end partition on an align_boundary starts later and still ends on
// the last usable sector, both GPT copies say so, and the smallest disk the
// layout fits on grows by the padding.
func TestAlignBoundaryKeepsFillToEndAndBackupGPT(t *testing.T) {
	layout, images := writeXMLFixture(t, "0x808")
	withAlignBoundary(t, layout, "4194304")
	plan, err := LoadXMLPlan(layout, images, "nvme0n1")
	if err != nil {
		t.Fatal(err)
	}
	const deviceSectors = (32 << 20) / sectorSize
	resolved, err := plan.ResolveForDevice(deviceSectors)
	if err != nil {
		t.Fatal(err)
	}
	data := resolved.Partitions[1]
	if data.StartSector != 8192 || data.StartSector+data.SizeSectors != deviceSectors-gptBackupSectors {
		t.Fatalf("data covers sectors [%d, %d), want [8192, %d)", data.StartSector, data.StartSector+data.SizeSectors, deviceSectors-gptBackupSectors)
	}

	gpt, err := BuildGPT(plan, deviceSectors*sectorSize)
	if err != nil {
		t.Fatal(err)
	}
	if want := int64(deviceSectors-gptBackupSectors) * sectorSize; gpt.BackupOffset != want {
		t.Fatalf("backup GPT at byte %d, want %d", gpt.BackupOffset, want)
	}
	disk := make([]byte, deviceSectors*sectorSize)
	copy(disk, gpt.Primary)
	copy(disk[gpt.BackupOffset:], gpt.Backup)
	if err := VerifyGPT(bytes.NewReader(disk), resolved, int64(len(disk))); err != nil {
		t.Fatalf("GPT does not describe the placed plan: %v", err)
	}
	for copyName, entries := range map[string][]byte{"primary": disk[2*sectorSize:], "backup": disk[gpt.BackupOffset:]} {
		e := entries[(data.Number-1)*gptEntrySize:]
		first, last := binary.LittleEndian.Uint64(e[32:40]), binary.LittleEndian.Uint64(e[40:48])
		if first != 8192 || last != deviceSectors-gptBackupSectors-1 {
			t.Errorf("%s GPT has data at LBA %d to %d, want 8192 to %d", copyName, first, last, deviceSectors-gptBackupSectors-1)
		}
	}

	// APP ends at sector 4096. On 1 MiB placement data (1 MiB at least) would
	// start there and the layout would fit on 6177 sectors.
	const minSectors = 8192 + 2048 + gptBackupSectors
	if got := plan.MinDeviceSectors(); got != minSectors {
		t.Fatalf("MinDeviceSectors = %d, want %d", got, minSectors)
	}
	if _, err := plan.ResolveForDevice(minSectors); err != nil {
		t.Fatalf("a disk of exactly the minimum size was refused: %v", err)
	}
	if _, err := plan.ResolveForDevice(minSectors - 1); err == nil {
		t.Fatal("a disk one sector too small for the aligned layout was accepted")
	}
	if _, err := BuildGPT(plan, (minSectors-1)*sectorSize); err == nil {
		t.Fatal("a GPT was built for a disk one sector too small for the aligned layout")
	}
}
