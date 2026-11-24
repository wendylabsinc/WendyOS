package flashengine

import (
	"bytes"
	"cmp"
	"context"
	"crypto/md5"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/cli/tegraflash/t234"
)

// The fixtures are the FileToFlash.txt and the primary GPT image of the
// published WendyOS 0.20.2 Jetson AGX Thor flash package, unmodified.
const thorFixture = "testdata/thor-0.20.2"

const (
	thorQSPI = "/dev/block/810c5b0000.spi"
	thorNVMe = "/dev/nvme0n1"
	// The data partition is GPT entry 17: 512 MiB from LBA 55681024, which
	// first boot grows to the end of the disk. FileToFlash.txt has no row
	// for it.
	thorDataStart = 28508684288
	thorDataEnd   = 29045555200
	// thorLayoutDisk is the disk the package's GPT was built for; the flash
	// writes the backup GPT in its last 33 sectors.
	thorLayoutDisk = 251255586816
	thorBackupGPT  = 33 * 512
	thorDisk1TB    = 1000204886016
)

// thorStale is what a flash of the 0.20.2 layout must clear whatever the
// disk's size: the first 8 MiB and the last 2 MiB of each partition
// FileToFlash.txt has no row for, neighbours merged.
var thorStale = []t234.Extent{
	{Start: 20480, End: 8409088},             // recovery, start
	{Start: 81809408, End: 84692992},         // recovery, end; recovery-dtb, whole
	{Start: 621563904, End: 629952512},       // A_reserved_on_user, start
	{Start: 1022906368, End: 1033392128},     // A_reserved_on_user, end; recovery_alt, start
	{Start: 1106792448, End: 1118064640},     // recovery_alt, end; recovery-dtb_alt, whole; esp_alt, start
	{Start: 1644449792, End: 1654935552},     // esp_alt, end; B_reserved_on_user, start
	{Start: 2047889408, End: 2049986560},     // B_reserved_on_user, end
	{Start: 2049998848, End: 2058387456},     // UDA, start
	{Start: 2467332096, End: 2469429248},     // UDA, end
	{Start: thorDataStart, End: 28517072896}, // data, start: the old filesystem's superblock, or its LUKS headers
	{Start: 29043458048, End: thorDataEnd},   // data, end as the layout sizes it
}

// thorStaleOn adds what depends on the disk: the last 2 MiB below the backup
// GPT, where data ends once first boot has grown it, and where the previous
// installation's data ended too.
func thorStaleOn(disk int64) []t234.Extent {
	end := disk - thorBackupGPT
	return append(slices.Clone(thorStale), t234.Extent{Start: end - t234.StaleTailBytes, End: end})
}

// fakeThor stands in for a Thor in its flashing initrd. It keeps what is
// pushed to it, answers for the size of its disk, and records every write and
// every read-back check nvdd is asked for, in order.
type fakeThor struct {
	diskSize int64
	// sizeAnswer, when set, replaces what blockdev prints.
	sizeAnswer string
	// fail is asked before each shell command; an error fails the command.
	fail func(cmd string) error

	files  map[string][]byte
	pushes int
	cmds   []string
	writes []fakeWrite
	// readZero holds the ranges an md5 check found to read back as zeros.
	readZero []t234.Extent
}

type fakeWrite struct {
	seq    int // index of the command in cmds
	device string
	t234.Extent
	zeros bool // every byte written was zero
}

var nvddFlag = regexp.MustCompile(`--(inputbin|device|startoffset|partsize|md5sum)[= ]+(\S+)`)

func (f *fakeThor) Push(r io.Reader, remotePath string, _ int) error {
	data, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	if f.files == nil {
		f.files = map[string][]byte{}
	}
	f.files[remotePath] = data
	f.pushes++
	return nil
}

func (f *fakeThor) Shell(cmd string) (string, error) {
	f.cmds = append(f.cmds, cmd)
	if f.fail != nil {
		if err := f.fail(cmd); err != nil {
			return "", err
		}
	}
	switch {
	case strings.HasPrefix(cmd, "blockdev --getsize64 "):
		if f.sizeAnswer != "" {
			return f.sizeAnswer, nil
		}
		return fmt.Sprintf("%d\n", f.diskSize), nil
	case strings.HasPrefix(cmd, "rm "):
		delete(f.files, strings.TrimPrefix(cmd, "rm "))
	case strings.HasPrefix(cmd, remoteNvdd+" "):
		flags := map[string]string{}
		for _, m := range nvddFlag.FindAllStringSubmatch(cmd, -1) {
			flags[m[1]] = m[2]
		}
		start, _ := strconv.ParseInt(flags["startoffset"], 10, 64)
		size, _ := strconv.ParseInt(flags["partsize"], 10, 64)
		if in := flags["inputbin"]; in != "" {
			data, ok := f.files[in]
			if !ok {
				return "", fmt.Errorf("nvdd: %s was never pushed", in)
			}
			if int64(len(data)) > size {
				return "", fmt.Errorf("nvdd: %s (%d bytes) is larger than --partsize %d", in, len(data), size)
			}
			f.writes = append(f.writes, fakeWrite{
				seq:    len(f.cmds) - 1,
				device: flags["device"],
				Extent: t234.Extent{Start: start, End: start + int64(len(data))},
				zeros:  bytes.Count(data, []byte{0}) == len(data),
			})
		} else if sum := flags["md5sum"]; sum != "" && sum == zeroSum(size) {
			f.readZero = append(f.readZero, t234.Extent{Start: start, End: start + size})
		}
	}
	return "", nil
}

func zeroSum(n int64) string {
	sum := md5.Sum(make([]byte, n))
	return hex.EncodeToString(sum[:])
}

// cleared returns the ranges of dev that were overwritten with zeros, merged.
func (f *fakeThor) cleared(dev string) []t234.Extent {
	var xs []t234.Extent
	for _, w := range f.writes {
		if w.zeros && w.device == dev {
			xs = append(xs, w.Extent)
		}
	}
	return mergeTestExtents(xs)
}

func mergeTestExtents(xs []t234.Extent) []t234.Extent {
	slices.SortFunc(xs, func(a, b t234.Extent) int { return cmp.Compare(a.Start, b.Start) })
	var out []t234.Extent
	for _, x := range xs {
		if n := len(out); n > 0 && x.Start <= out[n-1].End {
			out[n-1].End = max(out[n-1].End, x.End)
			continue
		}
		out = append(out, x)
	}
	return out
}

// firstCommand returns the index of the first command for which match holds,
// or -1.
func (f *fakeThor) firstCommand(match func(string) bool) int {
	return slices.IndexFunc(f.cmds, match)
}

func isQSPIErase(cmd string) bool { return strings.Contains(cmd, " -E eoffset=0,esize=0") }

// thorWorkspace is a flash-images directory with the real FileToFlash.txt and
// primary GPT image. The other images are stand-ins: small, and never zeros,
// so a write of zeros can only be the clearing of old data.
func thorWorkspace(t *testing.T) Options {
	t.Helper()
	dir := t.TempDir()
	for _, name := range []string{"FileToFlash.txt", "gpt_primary_9_0.bin"} {
		b, err := os.ReadFile(filepath.Join(thorFixture, name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := parseFileToFlash(filepath.Join(dir, "FileToFlash.txt"))
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		path := filepath.Join(dir, r.FileName)
		if _, err := os.Stat(path); err == nil {
			continue
		}
		if err := os.WriteFile(path, bytes.Repeat([]byte{0x5A}, int(min(4096, r.Size))), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	nvdd := filepath.Join(dir, "nvdd")
	if err := os.WriteFile(nvdd, []byte("nvdd"), 0o755); err != nil {
		t.Fatal(err)
	}
	return Options{FlashImagesDir: dir, NvddLocalPath: nvdd, Out: io.Discard}
}

// thorRows returns the fixture's rows for dev as byte ranges: what the flash
// writes there.
func thorRows(t *testing.T, opts Options, dev string) []t234.Extent {
	t.Helper()
	rows, err := parseFileToFlash(filepath.Join(opts.FlashImagesDir, "FileToFlash.txt"))
	if err != nil {
		t.Fatal(err)
	}
	var xs []t234.Extent
	for _, r := range rows {
		if r.Device == dev {
			xs = append(xs, t234.Extent{Start: r.Start, End: r.Start + r.Size})
		}
	}
	return xs
}

func extentsString(xs []t234.Extent) string {
	var b strings.Builder
	for _, x := range xs {
		fmt.Fprintf(&b, "\n  [%d, %d)", x.Start, x.End)
	}
	return b.String()
}

// TestRunClearsThePreviousInstallation flashes the real 0.20.2 layout onto a
// used disk. No row of FileToFlash.txt covers the data partition, so before
// this the previous installation's /data survived and first boot adopted it.
// The flash must zero the start and end of every partition it does not write,
// and the end of the disk, where the grown data partition ends.
func TestRunClearsThePreviousInstallation(t *testing.T) {
	for _, tc := range []struct {
		name string
		disk int64
	}{
		{"a disk larger than the layout was built for", thorDisk1TB},
		{"a disk of exactly the layout's size", thorLayoutDisk},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dev, opts := &fakeThor{diskSize: tc.disk}, thorWorkspace(t)
			if err := Run(context.Background(), dev, opts); err != nil {
				t.Fatalf("Run: %v", err)
			}
			want := thorStaleOn(tc.disk)
			if got := dev.cleared(thorNVMe); !slices.Equal(got, want) {
				t.Fatalf("cleared on %s:%s\nwant:%s", thorNVMe, extentsString(got), extentsString(want))
			}
			if got := mergeTestExtents(slices.Clone(dev.readZero)); !slices.Equal(got, want) {
				t.Errorf("ranges checked to read back as zeros:%s\nwant:%s", extentsString(got), extentsString(want))
			}
			if got := dev.cleared(thorQSPI); len(got) != 0 {
				t.Errorf("zeros were written to QSPI, which is erased whole:%s", extentsString(got))
			}
			if len(dev.files) != 1 || dev.files[remoteNvdd] == nil {
				t.Errorf("%d files left on the device; only nvdd should be", len(dev.files))
			}
		})
	}
}

// TestRunClearsOnlyWhatItDoesNotWrite pins the two ways the clearing could
// damage a flash: by overlapping a partition the flash writes or the backup
// GPT, and by running after a write.
func TestRunClearsOnlyWhatItDoesNotWrite(t *testing.T) {
	dev, opts := &fakeThor{diskSize: thorDisk1TB}, thorWorkspace(t)
	if err := Run(context.Background(), dev, opts); err != nil {
		t.Fatalf("Run: %v", err)
	}
	rows := thorRows(t, opts, thorNVMe)
	lastClear := -1
	for _, w := range dev.writes {
		if !w.zeros {
			continue
		}
		lastClear = w.seq
		for _, r := range rows {
			if w.Start < r.End && r.Start < w.End {
				t.Errorf("cleared [%d, %d), inside [%d, %d) that the flash writes", w.Start, w.End, r.Start, r.End)
			}
		}
		if w.End > thorDisk1TB-thorBackupGPT {
			t.Errorf("cleared [%d, %d), which reaches the backup GPT at the end of the disk", w.Start, w.End)
		}
	}
	if lastClear < 0 {
		t.Fatal("nothing was cleared")
	}
	if erase := dev.firstCommand(isQSPIErase); erase < lastClear {
		t.Errorf("QSPI was erased at command %d, before the clearing ended at command %d", erase, lastClear)
	}
	for _, w := range dev.writes {
		if !w.zeros && w.seq < lastClear {
			t.Fatalf("%s was written at [%d, %d) before the clearing ended; clearing must come first", w.device, w.Start, w.End)
		}
	}
}

// TestRunStopsWhenOldDataCannotBeCleared: a flash that cannot clear the old
// data must fail, and must fail before it erases QSPI or writes a partition,
// so the board is not left flashed with the old /data in place.
func TestRunStopsWhenOldDataCannotBeCleared(t *testing.T) {
	dataHead := fmt.Sprintf("%d", thorDataStart)
	for _, tc := range []struct {
		name string
		dev  *fakeThor
	}{
		{"the disk's size cannot be read", &fakeThor{diskSize: thorDisk1TB, fail: func(cmd string) error {
			if strings.HasPrefix(cmd, "blockdev ") {
				return errors.New("exit status 127")
			}
			return nil
		}}},
		{"the disk's size is not a number", &fakeThor{sizeAnswer: "blockdev: ioctl error on BLKGETSIZE64\n"}},
		{"a clearing write fails", &fakeThor{diskSize: thorDisk1TB, fail: func(cmd string) error {
			if strings.Contains(cmd, "--inputbin") && strings.Contains(cmd, "--startoffset="+dataHead) {
				return errors.New("exit status 1")
			}
			return nil
		}}},
		{"a cleared range does not read back as zeros", &fakeThor{diskSize: thorDisk1TB, fail: func(cmd string) error {
			if strings.Contains(cmd, "--md5sum") && strings.Contains(cmd, "--startoffset "+dataHead) {
				return errors.New("exit status 1")
			}
			return nil
		}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := Run(context.Background(), tc.dev, thorWorkspace(t))
			if err == nil {
				t.Fatal("Run succeeded; the old data partition was left as it was")
			}
			if !errors.Is(err, ErrOldDataNotCleared) {
				t.Errorf("error is not ErrOldDataNotCleared: %v", err)
			}
			if !strings.Contains(err.Error(), thorNVMe) || !strings.Contains(err.Error(), "before the boot firmware or OS was changed") {
				t.Errorf("error does not say what failed and how far the flash got: %v", err)
			}
			if i := tc.dev.firstCommand(isQSPIErase); i >= 0 {
				t.Errorf("QSPI was erased (command %d) although the clearing failed", i)
			}
			for _, w := range tc.dev.writes {
				if !w.zeros {
					t.Fatalf("%s was written at [%d, %d) although the clearing failed", w.device, w.Start, w.End)
				}
			}
		})
	}
}

// TestRunStopsOnAPackageWhoseLayoutCannotBeRead: where to clear comes from
// the package's primary GPT image. Without it the flash must stop before it
// touches the device, not flash and leave the old data.
func TestRunStopsOnAPackageWhoseLayoutCannotBeRead(t *testing.T) {
	for _, tc := range []struct {
		name   string
		damage func(t *testing.T, dir string)
	}{
		{"FileToFlash.txt has no primary_gpt row for the disk", func(t *testing.T, dir string) {
			path := filepath.Join(dir, "FileToFlash.txt")
			b, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var kept []string
			for _, line := range strings.Split(string(b), "\n") {
				if !strings.Contains(line, " primary_gpt ") {
					kept = append(kept, line)
				}
			}
			if err := os.WriteFile(path, []byte(strings.Join(kept, "\n")), 0o644); err != nil {
				t.Fatal(err)
			}
		}},
		{"the primary GPT image is damaged", func(t *testing.T, dir string) {
			path := filepath.Join(dir, "gpt_primary_9_0.bin")
			b, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			b[512+32] ^= 0xFF // first partition entry's first LBA
			if err := os.WriteFile(path, b, 0o644); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dev, opts := &fakeThor{diskSize: thorDisk1TB}, thorWorkspace(t)
			tc.damage(t, opts.FlashImagesDir)
			err := Run(context.Background(), dev, opts)
			if !errors.Is(err, ErrOldDataNotCleared) {
				t.Fatalf("Run = %v, want ErrOldDataNotCleared", err)
			}
			if !strings.Contains(err.Error(), "before anything was written") {
				t.Errorf("error does not say nothing was written: %v", err)
			}
			if len(dev.cmds) != 0 || dev.pushes != 0 {
				t.Errorf("the device was touched: %d commands, %d pushes", len(dev.cmds), dev.pushes)
			}
		})
	}
}

// TestDryRunListsTheClearing: a dry run never reaches the device, so it
// cannot know the disk's size; it still lists the clearing the layout alone
// decides.
func TestDryRunListsTheClearing(t *testing.T) {
	opts := thorWorkspace(t)
	var out bytes.Buffer
	opts.DryRun, opts.Out = true, &out
	dev := &fakeThor{fail: func(cmd string) error { return errors.New("a dry run must not reach the device") }}
	if err := Run(context.Background(), dev, opts); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(dev.cmds) != 0 || dev.pushes != 0 {
		t.Fatalf("the device was touched: %d commands, %d pushes", len(dev.cmds), dev.pushes)
	}
	clear := fmt.Sprintf("--device %s --startoffset=%d --l4t", thorNVMe, thorDataStart)
	erase := "-E eoffset=0,esize=0"
	i, j := strings.Index(out.String(), clear), strings.Index(out.String(), erase)
	if i < 0 || j < 0 || i > j {
		t.Fatalf("the dry run does not list the clearing of the data partition (at byte %d of the output) before the QSPI erase (at byte %d)", i, j)
	}
}

// TestParseGPTImageReadsTheThorLayout checks the parser against the real
// primary GPT image: every partition, in table order, as byte ranges.
func TestParseGPTImageReadsTheThorLayout(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(thorFixture, "gpt_primary_9_0.bin"))
	if err != nil {
		t.Fatal(err)
	}
	// FileToFlash.txt writes the image at byte 512: LBA 1 of a 512-byte-sector disk.
	got, err := parseGPTImage(data, 512)
	if err != nil {
		t.Fatalf("parseGPTImage: %v", err)
	}
	if got.sectorSize != 512 || got.backupBytes != thorBackupGPT {
		t.Errorf("sector size %d, backup GPT %d bytes; want 512 and %d", got.sectorSize, got.backupBytes, thorBackupGPT)
	}
	want := []gptPartition{
		{"APP", 2469429248, 15354331136},
		{"APP_b", 15354331136, 28239233024},
		{"recovery", 20480, 83906560},
		{"recovery-dtb", 83906560, 84692992},
		{"esp", 84692992, 621563904},
		{"A_reserved_on_user", 621563904, 1025003520},
		{"recovery_alt", 1025003520, 1108889600},
		{"recovery-dtb_alt", 1108889600, 1109676032},
		{"esp_alt", 1109676032, 1646546944},
		{"B_reserved_on_user", 1646546944, 2049986560},
		{"UDA", 2049998848, 2469429248},
		{"config", 28240248832, 28508684288},
		{"data", thorDataStart, thorDataEnd},
	}
	if !slices.Equal(got.partitions, want) {
		t.Fatalf("partitions:\n%+v\nwant:\n%+v", got.partitions, want)
	}
}

// testGPTImage builds a primary GPT image as a flash package ships it: the
// header, then the partition entries.
func testGPTImage(sectorSize int, parts []gptPartition) []byte {
	const entries, entrySize = 128, 128
	img := make([]byte, sectorSize+entries*entrySize)
	table := img[sectorSize:]
	for i, p := range parts {
		e := table[i*entrySize:]
		e[0] = 0xAF // any type GUID but the unused one
		binary.LittleEndian.PutUint64(e[32:], uint64(p.start/int64(sectorSize)))
		binary.LittleEndian.PutUint64(e[40:], uint64(p.end/int64(sectorSize)-1))
		for j, r := range p.name {
			binary.LittleEndian.PutUint16(e[56+2*j:], uint16(r))
		}
	}
	h := img[:sectorSize]
	copy(h, "EFI PART")
	binary.LittleEndian.PutUint32(h[8:], 0x00010000)
	binary.LittleEndian.PutUint32(h[12:], 92)
	binary.LittleEndian.PutUint64(h[24:], 1)
	binary.LittleEndian.PutUint64(h[72:], 2)
	binary.LittleEndian.PutUint32(h[80:], entries)
	binary.LittleEndian.PutUint32(h[84:], entrySize)
	binary.LittleEndian.PutUint32(h[88:], crc32.ChecksumIEEE(table))
	binary.LittleEndian.PutUint32(h[16:], crc32.ChecksumIEEE(h[:92]))
	return img
}

func TestParseGPTImage(t *testing.T) {
	parts := []gptPartition{{"APP", 1 << 20, 3 << 20}, {"data", 3 << 20, 19 << 20}}
	t.Run("512-byte sectors", func(t *testing.T) {
		got, err := parseGPTImage(testGPTImage(512, parts), 512)
		if err != nil || got.sectorSize != 512 || got.backupBytes != 33*512 || !slices.Equal(got.partitions, parts) {
			t.Fatalf("got %+v, %v", got, err)
		}
	})
	t.Run("4096-byte sectors", func(t *testing.T) {
		got, err := parseGPTImage(testGPTImage(4096, parts), 4096)
		if err != nil || got.sectorSize != 4096 || got.backupBytes != 5*4096 || !slices.Equal(got.partitions, parts) {
			t.Fatalf("got %+v, %v", got, err)
		}
	})
	for _, tc := range []struct {
		name   string
		offset int64
		damage func(img []byte) []byte
	}{
		{"no GPT signature", 512, func(img []byte) []byte { img[0] = 'X'; return img }},
		{"a header that fails its checksum", 512, func(img []byte) []byte { img[40]++; return img }},
		{"partition entries that fail their checksum", 512, func(img []byte) []byte { img[512+40]++; return img }},
		{"an image cut short", 512, func(img []byte) []byte { return img[:len(img)-512] }},
		{"less than a header", 512, func(img []byte) []byte { return img[:64] }},
		{"an offset that is not the header's sector", 1000, func(img []byte) []byte { return img }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got, err := parseGPTImage(tc.damage(testGPTImage(512, parts)), tc.offset); err == nil {
				t.Fatalf("parsed %+v; want an error", got)
			}
		})
	}
}

// FuzzParseGPTImage: the image comes from a downloaded package, so whatever
// it holds the parser must return, not panic, and a layout it accepts must
// be made of real byte ranges.
func FuzzParseGPTImage(f *testing.F) {
	real, err := os.ReadFile(filepath.Join(thorFixture, "gpt_primary_9_0.bin"))
	if err != nil {
		f.Fatal(err)
	}
	f.Add(real, int64(512))
	f.Add(testGPTImage(4096, []gptPartition{{"data", 1 << 20, 9 << 20}}), int64(4096))
	f.Add([]byte("EFI PART"), int64(512))
	f.Fuzz(func(t *testing.T, data []byte, offset int64) {
		layout, err := parseGPTImage(data, offset)
		if err != nil {
			return
		}
		if layout.sectorSize <= 0 || layout.backupBytes < 2*layout.sectorSize {
			t.Fatalf("accepted a layout with sector size %d and a %d-byte backup GPT", layout.sectorSize, layout.backupBytes)
		}
		for _, p := range layout.partitions {
			if p.start < 0 || p.end <= p.start {
				t.Fatalf("accepted partition %q as [%d, %d)", p.name, p.start, p.end)
			}
		}
	})
}

// TestStaleExtentsFollowTheDisk covers the disks the layout was not built
// for. Nothing may be cleared at or past the backup GPT, wherever the disk
// puts it.
func TestStaleExtentsFollowTheDisk(t *testing.T) {
	const mib = 1 << 20
	layout := gptLayout{sectorSize: 512, backupBytes: 33 * 512, partitions: []gptPartition{
		{"APP", 1 * mib, 65 * mib},
		{"data", 65 * mib, 129 * mib},
	}}
	target := staleTarget{device: thorNVMe, layout: layout, written: []t234.Extent{{Start: 0, End: 65 * mib}}}
	head := t234.Extent{Start: 65 * mib, End: 73 * mib}
	for _, tc := range []struct {
		name string
		disk int64
		want []t234.Extent
	}{
		{"unknown size: the layout's own start and end", 0,
			[]t234.Extent{head, {Start: 127 * mib, End: 129 * mib}}},
		{"larger disk: also the end of the disk, below the backup GPT", 1024 * mib,
			[]t234.Extent{head, {Start: 127 * mib, End: 129 * mib}, {Start: 1022*mib - 33*512, End: 1024*mib - 33*512}}},
		{"disk that ends inside data: nothing at or past the backup GPT", 100 * mib,
			[]t234.Extent{head, {Start: 98*mib - 33*512, End: 100*mib - 33*512}}},
		{"disk that ends before data: nothing", 64 * mib, nil},
		{"size that is not whole sectors: whole sectors only", 1024*mib + 100,
			[]t234.Extent{head, {Start: 127 * mib, End: 129 * mib}, {Start: 1022*mib - 33*512, End: 1024*mib - 33*512}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := target.extents(tc.disk); !slices.Equal(got, tc.want) {
				t.Fatalf("extents:%s\nwant:%s", extentsString(got), extentsString(tc.want))
			}
		})
	}
}

func TestStaleExtentsDoNotExtendFixedFinalPartition(t *testing.T) {
	const mib = 1 << 20
	target := staleTarget{
		layout: gptLayout{sectorSize: 512, backupBytes: 33 * 512, partitions: []gptPartition{
			{"config", 65 * mib, 129 * mib},
			{"data", 1 * mib, 65 * mib},
		}},
		written: []t234.Extent{{Start: mib, End: 129 * mib}},
	}
	if got := target.extents(1024 * mib); len(got) != 0 {
		t.Fatalf("fully imaged layout should not need clearing; got:%s", extentsString(got))
	}
}
