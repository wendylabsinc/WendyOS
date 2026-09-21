package qdl

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// The testdata descriptors are the real ones produced by a Dragonwing
// IQ-8275 image build, so these tests pin the parser against the shape the
// device actually ships rather than a hand-written approximation.

func TestParseRawProgramRealDescriptor(t *testing.T) {
	f, err := os.Open("testdata/rawprogram0.xml")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close() //nolint:errcheck
	got, err := ParseRawProgram(f)
	if err != nil {
		t.Fatalf("ParseRawProgram: %v", err)
	}

	// Every entry is returned in descriptor order, config and data included:
	// dropping the payload-less ones is LoadFlashPlan's job, so that a caller
	// can seed one first.
	wantLabels := []string{"efi", "config", "rootfsA", "rootfsB", "data", "PrimaryGPT", "BackupGPT"}
	if len(got) != len(wantLabels) {
		t.Fatalf("kept %d entries, want %d: %+v", len(got), len(wantLabels), got)
	}
	for i, want := range wantLabels {
		if got[i].Label != want {
			t.Errorf("entry %d label = %q, want %q", i, got[i].Label, want)
		}
		if got[i].SectorSize != 4096 {
			t.Errorf("entry %s sector size = %d, want 4096", got[i].Label, got[i].SectorSize)
		}
	}

	// config and data are the partitions the flash must leave alone; the
	// descriptor says so by giving them no payload.
	for _, i := range []int{1, 4} {
		if got[i].Filename != "" {
			t.Errorf("entry %s has payload %q, want none", got[i].Label, got[i].Filename)
		}
	}

	// Both slots are written from the same payload; a flash that skipped
	// rootfsB would leave the inactive slot stale and break the first OTA.
	if got[2].Filename != got[3].Filename {
		t.Errorf("rootfsA/rootfsB payloads differ: %q vs %q", got[2].Filename, got[3].Filename)
	}
	if got[2].StartSector == got[3].StartSector {
		t.Errorf("rootfsA and rootfsB share start_sector %q", got[2].StartSector)
	}
}

func TestParseRawProgramKeepsSectorExpressions(t *testing.T) {
	// BackupGPT sits at a disk-relative offset. Parsing it into a number
	// would be wrong: only the programmer on the device knows the disk size.
	f, err := os.Open("testdata/rawprogram0.xml")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close() //nolint:errcheck
	got, err := ParseRawProgram(f)
	if err != nil {
		t.Fatal(err)
	}
	var backup *ProgramEntry
	for i := range got {
		if got[i].Label == "BackupGPT" {
			backup = &got[i]
		}
	}
	if backup == nil {
		t.Fatal("no BackupGPT entry")
	}
	if backup.StartSector != "NUM_DISK_SECTORS-5." {
		t.Errorf("BackupGPT start_sector = %q, want the expression verbatim", backup.StartSector)
	}
}

func TestParsePatchesKeepsOnlyDiskTargets(t *testing.T) {
	f, err := os.Open("testdata/patch0.xml")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close() //nolint:errcheck
	got, err := ParsePatches(f)
	if err != nil {
		t.Fatalf("ParsePatches: %v", err)
	}
	// 26 entries in the file; the 13 naming a gpt_*.bin file are host-side
	// rewrites that we deliberately do not perform.
	if len(got) != 13 {
		t.Fatalf("kept %d patches, want 13", len(got))
	}
	for _, p := range got {
		if p.Filename != diskPatchTarget {
			t.Errorf("kept a patch targeting %q", p.Filename)
		}
	}
	// The CRC and size expressions must survive untouched.
	var sawCRC, sawDiskSectors bool
	for _, p := range got {
		if strings.HasPrefix(p.Value, "CRC32(") {
			sawCRC = true
		}
		if strings.Contains(p.Value, "NUM_DISK_SECTORS") {
			sawDiskSectors = true
		}
	}
	if !sawCRC || !sawDiskSectors {
		t.Errorf("expression values were lost (crc=%v diskSectors=%v)", sawCRC, sawDiskSectors)
	}
}

func TestParseRawProgramRejectsSparse(t *testing.T) {
	const doc = `<data><program label="x" filename="a.img" sparse="true"
		SECTOR_SIZE_IN_BYTES="4096" start_sector="1" num_partition_sectors="1"/></data>`
	if _, err := ParseRawProgram(strings.NewReader(doc)); err == nil {
		t.Fatal("want an error for a sparse entry, got nil")
	}
}

func TestParseRawProgramRejectsMissingFields(t *testing.T) {
	for name, doc := range map[string]string{
		"no sector size": `<data><program label="x" filename="a.img" start_sector="1"/></data>`,
		"no start":       `<data><program label="x" filename="a.img" SECTOR_SIZE_IN_BYTES="4096"/></data>`,
	} {
		if _, err := ParseRawProgram(strings.NewReader(doc)); err == nil {
			t.Errorf("%s: want an error, got nil", name)
		}
	}
}

func TestLoadFlashPlanRejectsEscapingPayload(t *testing.T) {
	// The bundle is downloaded from a remote manifest, so a descriptor must
	// not be able to name a file outside the directory it was unpacked into.
	dir := t.TempDir()
	write(t, dir, "rawprogram0.xml", `<data><program label="x" filename="../../etc/passwd"
		SECTOR_SIZE_IN_BYTES="4096" start_sector="1" num_partition_sectors="1"/></data>`)
	write(t, dir, "patch0.xml", `<patches></patches>`)
	_, err := LoadFlashPlan(dir)
	if err == nil || !strings.Contains(err.Error(), "unsafe payload path") {
		t.Fatalf("want an unsafe-path error, got %v", err)
	}
}

func TestLoadFlashPlanRejectsMissingPayload(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "rawprogram0.xml", `<data><program label="x" filename="absent.img"
		SECTOR_SIZE_IN_BYTES="4096" start_sector="1" num_partition_sectors="1"/></data>`)
	write(t, dir, "patch0.xml", `<patches></patches>`)
	_, err := LoadFlashPlan(dir)
	if err == nil || !strings.Contains(err.Error(), "is missing") {
		t.Fatalf("want a missing-payload error, got %v", err)
	}
}

func TestLoadFlashPlanRejectsNothingToFlash(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "rawprogram0.xml", `<data><program label="data" filename=""
		SECTOR_SIZE_IN_BYTES="4096" start_sector="1" num_partition_sectors="1"/></data>`)
	write(t, dir, "patch0.xml", `<patches></patches>`)
	if _, err := LoadFlashPlan(dir); err == nil {
		t.Fatal("want an error when every entry is skipped, got nil")
	}
}

func write(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestResolveBundleDir(t *testing.T) {
	t.Run("descriptors at the root", func(t *testing.T) {
		dir := t.TempDir()
		write(t, dir, "rawprogram0.xml", "<data/>")
		got, err := ResolveBundleDir(dir)
		if err != nil || got != dir {
			t.Fatalf("got %q, %v; want %q", got, err, dir)
		}
	})

	t.Run("descriptors one level down", func(t *testing.T) {
		// The shape a published bundle actually has.
		dir := t.TempDir()
		inner := filepath.Join(dir, "wendyos-image-iq-8275-evk-wendyos")
		if err := os.MkdirAll(inner, 0o755); err != nil {
			t.Fatal(err)
		}
		write(t, inner, "rawprogram0.xml", "<data/>")
		got, err := ResolveBundleDir(dir)
		if err != nil || got != inner {
			t.Fatalf("got %q, %v; want %q", got, err, inner)
		}
	})

	t.Run("ambiguous is an error, not a guess", func(t *testing.T) {
		// The real bundle ships a sail_nor/ variant for the SPI-NOR part;
		// picking the wrong one would flash the wrong storage.
		dir := t.TempDir()
		for _, name := range []string{"main", "sail_nor"} {
			sub := filepath.Join(dir, name)
			if err := os.MkdirAll(sub, 0o755); err != nil {
				t.Fatal(err)
			}
			write(t, sub, "rawprogram0.xml", "<data/>")
		}
		if _, err := ResolveBundleDir(dir); err == nil {
			t.Fatal("want an error for an ambiguous bundle, got nil")
		}
	})

	t.Run("missing descriptor", func(t *testing.T) {
		if _, err := ResolveBundleDir(t.TempDir()); err == nil {
			t.Fatal("want an error, got nil")
		}
	})
}

func TestLoadFlashPlanRequiresDiskPatches(t *testing.T) {
	// A patch file holding only host-side gpt_*.bin rewrites leaves the GPT on
	// the device at its placeholder size, so the last partition would not span
	// the disk — a silent truncation the flash must refuse rather than report
	// as success.
	dir := t.TempDir()
	write(t, dir, "rawprogram0.xml", `<data><program label="efi" filename="efi.bin"
		SECTOR_SIZE_IN_BYTES="4096" start_sector="6" num_partition_sectors="1"/></data>`)
	write(t, dir, "patch0.xml", `<patches><patch filename="gpt_main0.bin" SECTOR_SIZE_IN_BYTES="4096"
		start_sector="1" byte_offset="1" size_in_bytes="4" value="0" what="host-side only"/></patches>`)
	write(t, dir, "efi.bin", "payload")

	_, err := LoadFlashPlan(dir)
	if err == nil || !strings.Contains(err.Error(), "applies no patches") {
		t.Fatalf("want a no-patches error, got %v", err)
	}
}

func TestLoadFlashPlanAcceptsTheRealBundleShape(t *testing.T) {
	// The descriptors shipped by a real build must load cleanly.
	dir := t.TempDir()
	for _, name := range []string{"rawprogram0.xml", "patch0.xml"} {
		body, err := os.ReadFile(filepath.Join("testdata", name))
		if err != nil {
			t.Fatal(err)
		}
		write(t, dir, name, string(body))
	}
	// The descriptor states each payload's exact size, so stand them up as
	// sparse files of that size: instant, and costs no disk.
	for name, size := range map[string]int64{
		"efi.bin":         524288 * 1024,
		"rootfs.img":      12582912 * 1024,
		"gpt_main0.bin":   24 * 1024,
		"gpt_backup0.bin": 20 * 1024,
	} {
		f, err := os.Create(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if err := f.Truncate(size); err != nil {
			t.Fatal(err)
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
	}
	plan, err := LoadFlashPlan(dir)
	if err != nil {
		t.Fatalf("LoadFlashPlan on the real descriptors: %v", err)
	}
	if len(plan.Programs) != 5 || len(plan.Patches) != 13 {
		t.Errorf("got %d programs / %d patches, want 5 / 13", len(plan.Programs), len(plan.Patches))
	}
}

func TestLoadFlashPlanRejectsTruncatedPayload(t *testing.T) {
	// SectorsFor only catches a payload too big for its partition, so a
	// truncated one would be programmed as a few sectors and reported as a
	// successful flash of an unbootable board.
	dir := t.TempDir()
	write(t, dir, "rawprogram0.xml", `<data><program label="rootfsA" filename="rootfs.img"
		SECTOR_SIZE_IN_BYTES="4096" start_sector="1" num_partition_sectors="3145728"
		size_in_KB="12582912.0"/></data>`)
	write(t, dir, "patch0.xml", `<patches><patch filename="DISK" SECTOR_SIZE_IN_BYTES="4096"
		start_sector="1" byte_offset="1" size_in_bytes="4" value="0" what="x"/></patches>`)
	write(t, dir, "rootfs.img", "truncated")

	_, err := LoadFlashPlan(dir)
	if err == nil || !strings.Contains(err.Error(), "descriptor declares") {
		t.Fatalf("want a size-mismatch error, got %v", err)
	}
}

func TestParseRawProgramBoundsTheSectorSize(t *testing.T) {
	// The sector size multiplies descriptor-supplied offsets downstream.
	for name, size := range map[string]string{
		"zero":         "0",
		"not a power":  "1000",
		"absurdly big": "4294967295",
		"too small":    "8",
	} {
		doc := `<data><program label="x" filename="a.img" start_sector="1"
			num_partition_sectors="1" SECTOR_SIZE_IN_BYTES="` + size + `"/></data>`
		if _, err := ParseRawProgram(strings.NewReader(doc)); err == nil {
			t.Errorf("%s (%s): accepted", name, size)
		}
	}
	for _, size := range []string{"512", "4096", "65536"} {
		doc := `<data><program label="x" filename="a.img" start_sector="1"
			num_partition_sectors="1" SECTOR_SIZE_IN_BYTES="` + size + `"/></data>`
		if _, err := ParseRawProgram(strings.NewReader(doc)); err != nil {
			t.Errorf("rejected a legitimate sector size %s: %v", size, err)
		}
	}
}

func TestSectorsForRejectsOverflowingOffset(t *testing.T) {
	// uint32 offset times uint32 sector size overflows int64 and used to wrap
	// positive, passing every check and then failing mid-flash at the seek.
	e := ProgramEntry{SectorSize: 4294967295, FileOffset: 4294967295, NumSectors: 100,
		Label: "x", Filename: "p.img"}
	if n, err := SectorsFor(e, 1024); err == nil {
		t.Errorf("accepted an overflowing descriptor: %d sectors", n)
	}
}

// realBundle stands up a directory holding the real descriptors plus a
// sparse file of the exact declared size for every payload they name.
func realBundle(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, name := range []string{"rawprogram0.xml", "patch0.xml"} {
		body, err := os.ReadFile(filepath.Join("testdata", name))
		if err != nil {
			t.Fatal(err)
		}
		write(t, dir, name, string(body))
	}
	for name, size := range map[string]int64{
		"efi.bin":         524288 * 1024,
		"rootfs.img":      12582912 * 1024,
		"gpt_main0.bin":   24 * 1024,
		"gpt_backup0.bin": 20 * 1024,
	} {
		sparse(t, filepath.Join(dir, name), size)
	}
	return dir
}

func sparse(t *testing.T, path string, size int64) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(size); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

func labels(plan *FlashPlan) []string {
	out := make([]string, len(plan.Programs))
	for i, p := range plan.Programs {
		out[i] = p.Label
	}
	return out
}

func TestLoadFlashPlanSeedsConfigInDescriptorOrder(t *testing.T) {
	dir := realBundle(t)
	seed := filepath.Join(t.TempDir(), "wendy-config.img")
	sparse(t, seed, 262144*1024) // the size the descriptor declares for config

	plan, err := LoadFlashPlan(dir)
	if err != nil {
		t.Fatalf("LoadFlashPlan: %v", err)
	}
	if err := plan.Seed("config", seed); err != nil {
		t.Fatalf("Seed(config): %v", err)
	}
	// Position matters: the GPT entries must stay last, or the disk is
	// repartitioned out from under the payloads already written.
	want := []string{"efi", "config", "rootfsA", "rootfsB", "PrimaryGPT", "BackupGPT"}
	if got := labels(plan); !slices.Equal(got, want) {
		t.Fatalf("programs = %v, want %v", got, want)
	}
	cfg := plan.Programs[1]
	if cfg.localPath != seed {
		t.Errorf("config payload = %q, want the generated image %q", cfg.localPath, seed)
	}
	// Geometry must come from the descriptor, never from the caller.
	if cfg.SectorSize != 4096 || cfg.NumSectors != 65536 || cfg.StartSector != "131078" {
		t.Errorf("config geometry = %d bps / %d sectors @ %s, want the descriptor's",
			cfg.SectorSize, cfg.NumSectors, cfg.StartSector)
	}
}

func TestSeedNeverReachesData(t *testing.T) {
	// data is the one partition the descriptor leaves unsized, so neither
	// payload guard can bound a write to it. Blank is the only way in, and
	// seeding must never arrive there by accident.
	dir := realBundle(t)
	seed := filepath.Join(t.TempDir(), "wendy-config.img")
	sparse(t, seed, 262144*1024)

	for name, withSeed := range map[string]bool{"plain flash": false, "seeded flash": true} {
		plan, err := LoadFlashPlan(dir)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if withSeed {
			if err := plan.Seed("config", seed); err != nil {
				t.Fatalf("%s: %v", name, err)
			}
		}
		if slices.Contains(labels(plan), "data") {
			t.Errorf("%s: data is in the flash plan", name)
		}
	}
}

func TestLoadFlashPlanWithoutSeedIsUnchanged(t *testing.T) {
	// A flash with nothing to seed must still leave config alone.
	plan, err := LoadFlashPlan(realBundle(t))
	if err != nil {
		t.Fatalf("LoadFlashPlan: %v", err)
	}
	want := []string{"efi", "rootfsA", "rootfsB", "PrimaryGPT", "BackupGPT"}
	if got := labels(plan); !slices.Equal(got, want) {
		t.Fatalf("programs = %v, want %v", got, want)
	}
}

func TestLoadFlashPlanRejectsUnseedablePartitions(t *testing.T) {
	dir := realBundle(t)
	seed := filepath.Join(t.TempDir(), "wendy-config.img")
	sparse(t, seed, 262144*1024)

	for name, tc := range map[string]struct{ label, want string }{
		// Seeding a partition the bundle already supplies would flash
		// something other than the build the manifest vouched for.
		"already supplied": {"rootfsA", "already supplies"},
		"not declared":     {"nonesuch", "declares no such partition"},
		// data is sized by the device, so neither payload check can bound it.
		"device-sized": {"data", "declares no size"},
	} {
		plan, err := LoadFlashPlan(dir)
		if err != nil {
			t.Fatal(err)
		}
		err = plan.Seed(tc.label, seed)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: want an error containing %q, got %v", name, tc.want, err)
		}
	}
}

func TestLoadFlashPlanChecksSeededPayloadSize(t *testing.T) {
	// The descriptor's size_in_KB is the only integrity signal a payload
	// carries, and a short config image would flash as a few sectors.
	dir := realBundle(t)
	seed := filepath.Join(t.TempDir(), "wendy-config.img")
	sparse(t, seed, 4096)

	plan, err := LoadFlashPlan(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := plan.Seed("config", seed); err == nil || !strings.Contains(err.Error(), "descriptor declares") {
		t.Fatalf("want a size-mismatch error, got %v", err)
	}
}

func TestDescriptorCannotSupplyALocalPath(t *testing.T) {
	// localPath bypasses SafeJoin, so it must be unreachable from XML: a
	// descriptor naming a path outside the bundle stays contained.
	dir := t.TempDir()
	write(t, dir, "rawprogram0.xml", `<data><program label="x" filename="a.img" localPath="/etc/passwd"
		SECTOR_SIZE_IN_BYTES="4096" start_sector="1" num_partition_sectors="1"/></data>`)
	write(t, dir, "patch0.xml", `<patches><patch filename="DISK" SECTOR_SIZE_IN_BYTES="4096"
		start_sector="1" byte_offset="1" size_in_bytes="4" value="0" what="x"/></patches>`)

	_, err := LoadFlashPlan(dir)
	if err == nil || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("want the payload to resolve inside the bundle and be missing, got %v", err)
	}
}

func TestDeclaredGeometryComesFromTheDescriptor(t *testing.T) {
	plan, err := LoadFlashPlan(realBundle(t))
	if err != nil {
		t.Fatal(err)
	}
	// The host builds its config image to these figures, so a copy of them
	// drifting from the bundle would break every flash.
	e, err := plan.Declared("config")
	if err != nil || e.SizeKB != 262144 || e.SectorSize != 4096 {
		t.Errorf("config = %v KB / %d bps (%v), want 262144/4096", e.SizeKB, e.SectorSize, err)
	}
	// A bundle that declares no config partition must be distinguishable, so a
	// caller can skip seeding instead of failing the flash.
	if _, err := plan.Declared("nonesuch"); !errors.Is(err, ErrNoSuchPartition) {
		t.Errorf("unknown label = %v, want ErrNoSuchPartition", err)
	}
}

func TestBlankTargetsDataExactly(t *testing.T) {
	dir := realBundle(t)
	zeros := filepath.Join(t.TempDir(), "wendy-zeros.bin")
	sparse(t, zeros, 1<<20)

	plan, err := LoadFlashPlan(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := plan.Blank("data", zeros); err != nil {
		t.Fatalf("Blank(data): %v", err)
	}
	var e ProgramEntry
	for _, p := range plan.Programs {
		if p.Label == "data" {
			e = p
		}
	}
	// Geometry must come from the descriptor, never from the caller: writing
	// at the wrong LBA would land in a neighbouring partition.
	if e.StartSector != "6488070" || e.SectorSize != 4096 {
		t.Errorf("start=%s sector=%d, want the descriptor's 6488070/4096", e.StartSector, e.SectorSize)
	}
	// The descriptor leaves data unsized, so both payload guards are off until
	// BlankEntry sets them from the file. Without that a wrong-sized payload
	// would stream unbounded across the disk.
	if e.NumSectors != 256 || e.SizeKB != 1024 {
		t.Errorf("NumSectors=%d SizeKB=%v, want 256/1024 derived from the payload", e.NumSectors, e.SizeKB)
	}
	if n, err := SectorsFor(e, 1<<20); err != nil || n != 256 {
		t.Errorf("SectorsFor = %d, %v; want 256", n, err)
	}
	// It must land in descriptor order, so the GPT entries still go last.
	want := []string{"efi", "rootfsA", "rootfsB", "data", "PrimaryGPT", "BackupGPT"}
	if got := labels(plan); !slices.Equal(got, want) {
		t.Errorf("programs = %v, want %v", got, want)
	}
}

func TestBlankRefusesUnsafeTargets(t *testing.T) {
	dir := realBundle(t)
	good := filepath.Join(t.TempDir(), "z.bin")
	sparse(t, good, 1<<20)

	plan, err := LoadFlashPlan(dir)
	if err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct{ label, payload, want string }{
		// Blanking a partition the bundle supplies would corrupt the build.
		"bundle supplies it": {"rootfsA", good, "already supplies"},
		"not declared":       {"nonesuch", good, "declares no such partition"},
	} {
		if err := plan.Blank(tc.label, tc.payload); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: want an error containing %q, got %v", name, tc.want, err)
		}
	}

	// An oversized or mis-aligned payload is the failure that would overrun the
	// partition, so it must be refused rather than truncated.
	for name, size := range map[string]int64{
		"too large":       (1 << 20) + 4096,
		"not sector-wide": 4095,
		"empty":           0,
	} {
		p := filepath.Join(t.TempDir(), name)
		sparse(t, p, size)
		if err := plan.Blank("data", p); err == nil {
			t.Errorf("%s (%d bytes): want an error, got nil", name, size)
		}
	}
}
