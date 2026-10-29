package commands

import (
	"bytes"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf16"
)

// testPart is a partition of a synthetic GPT image, in sectors.
type testPart struct {
	first, last int64
	name        string
}

// linuxFSTypeGUID is 0FC63DAF-8483-4772-8E79-3D69D8477DE4 in GPT on-disk form.
var linuxFSTypeGUID = []byte{0xaf, 0x3d, 0xc6, 0x0f, 0x83, 0x84, 0x72, 0x47, 0x8e, 0x79, 0x3d, 0x69, 0xd8, 0x47, 0x7d, 0xe4}

// buildTestGPTImage lays out an image the way the WendyOS Jetson image is
// built: a protective MBR and a GPT declaring a disk of sectors sectors of ss
// bytes, partitions that end long before that disk does, and the backup GPT
// in the declared disk's last sectors. Each partition's first and last sector
// carry a marker, so a test can tell whether partition data survived. The GPT
// is written by hand, independently of the go-diskfs code under test.
func buildTestGPTImage(t *testing.T, ss, sectors int64, parts []testPart) []byte {
	t.Helper()
	entrySectors := int64(128 * 128 / ss)
	img := make([]byte, sectors*ss)

	pmbr := img[446:462]
	pmbr[1], pmbr[2], pmbr[3] = 0x00, 0x02, 0x00
	pmbr[4] = 0xEE
	pmbr[5], pmbr[6], pmbr[7] = 0xFF, 0xFF, 0xFF
	binary.LittleEndian.PutUint32(pmbr[8:12], 1)
	binary.LittleEndian.PutUint32(pmbr[12:16], protectiveMBRSectors(sectors))
	img[510], img[511] = 0x55, 0xAA

	entries := make([]byte, 128*128)
	for i, p := range parts {
		e := entries[i*128 : (i+1)*128]
		copy(e[0:16], linuxFSTypeGUID)
		e[16], e[31] = byte(i+1), 0xAB
		binary.LittleEndian.PutUint64(e[32:40], uint64(p.first))
		binary.LittleEndian.PutUint64(e[40:48], uint64(p.last))
		for j, u := range utf16.Encode([]rune(p.name)) {
			binary.LittleEndian.PutUint16(e[56+2*j:], u)
		}
		fill(img[p.first*ss:(p.first+1)*ss], byte(0x10+i))
		fill(img[p.last*ss:(p.last+1)*ss], byte(0x20+i))
	}
	header := func(my, alt, entriesLBA int64) []byte {
		h := make([]byte, ss)
		copy(h[0:8], "EFI PART")
		binary.LittleEndian.PutUint32(h[8:12], 0x00010000)
		binary.LittleEndian.PutUint32(h[12:16], 92)
		binary.LittleEndian.PutUint64(h[24:32], uint64(my))
		binary.LittleEndian.PutUint64(h[32:40], uint64(alt))
		binary.LittleEndian.PutUint64(h[40:48], uint64(2+entrySectors))
		binary.LittleEndian.PutUint64(h[48:56], uint64(sectors-2-entrySectors))
		copy(h[56:72], "wendy-disk-guid!")
		binary.LittleEndian.PutUint64(h[72:80], uint64(entriesLBA))
		binary.LittleEndian.PutUint32(h[80:84], 128)
		binary.LittleEndian.PutUint32(h[84:88], 128)
		binary.LittleEndian.PutUint32(h[88:92], crc32.ChecksumIEEE(entries))
		binary.LittleEndian.PutUint32(h[16:20], crc32.ChecksumIEEE(h[0:92]))
		return h
	}
	copy(img[ss:], header(1, sectors-1, 2))
	copy(img[2*ss:], entries)
	copy(img[(sectors-1-entrySectors)*ss:], entries)
	copy(img[(sectors-1)*ss:], header(sectors-1, 1, sectors-1-entrySectors))
	return img
}

func fill(b []byte, v byte) {
	for i := range b {
		b[i] = v
	}
}

// verifyTestGPT checks, independently of go-diskfs, that disk carries a GPT
// valid for its own size: protective MBR, both headers and their CRCs, the
// backup and last usable LBAs, both entry arrays, and the partitions of parts.
func verifyTestGPT(t *testing.T, disk []byte, ss int64, parts []testPart) {
	t.Helper()
	sectors := int64(len(disk)) / ss
	entrySectors := int64(128 * 128 / ss)

	pmbr := disk[446:462]
	if disk[510] != 0x55 || disk[511] != 0xAA || pmbr[4] != 0xEE {
		t.Fatal("protective MBR is missing")
	}
	if got, want := binary.LittleEndian.Uint32(pmbr[12:16]), protectiveMBRSectors(sectors); got != want {
		t.Errorf("protective MBR covers %d sectors, want %d", got, want)
	}
	header := func(name string, h []byte, my, alt, entriesLBA int64) []byte {
		t.Helper()
		if string(h[:8]) != "EFI PART" || binary.LittleEndian.Uint32(h[12:16]) != 92 {
			t.Fatalf("%s header: bad signature or size", name)
		}
		zeroed := append([]byte(nil), h[:92]...)
		binary.LittleEndian.PutUint32(zeroed[16:20], 0)
		if crc32.ChecksumIEEE(zeroed) != binary.LittleEndian.Uint32(h[16:20]) {
			t.Fatalf("%s header: CRC mismatch", name)
		}
		for _, f := range []struct {
			field     string
			off       int
			got, want int64
		}{
			{"my LBA", 24, 0, my},
			{"alternate LBA", 32, 0, alt},
			{"first usable LBA", 40, 0, 2 + entrySectors},
			{"last usable LBA", 48, 0, sectors - 2 - entrySectors},
			{"entries LBA", 72, 0, entriesLBA},
		} {
			if got := int64(binary.LittleEndian.Uint64(h[f.off : f.off+8])); got != f.want {
				t.Errorf("%s header: %s = %d, want %d", name, f.field, got, f.want)
			}
		}
		entries := disk[entriesLBA*ss : entriesLBA*ss+128*128]
		if crc32.ChecksumIEEE(entries) != binary.LittleEndian.Uint32(h[88:92]) {
			t.Fatalf("%s header: entries CRC mismatch", name)
		}
		return entries
	}
	primary, backup := disk[ss:2*ss], disk[(sectors-1)*ss:]
	pe := header("primary", primary, 1, sectors-1, 2)
	be := header("backup", backup, sectors-1, 1, sectors-1-entrySectors)
	if !bytes.Equal(pe, be) {
		t.Error("primary and backup partition entries differ")
	}
	if !bytes.Equal(primary[56:72], backup[56:72]) {
		t.Error("primary and backup disk GUIDs differ")
	}
	for i, p := range parts {
		e := pe[i*128 : (i+1)*128]
		if first, last := int64(binary.LittleEndian.Uint64(e[32:40])), int64(binary.LittleEndian.Uint64(e[40:48])); first != p.first || last != p.last {
			t.Errorf("partition %s spans %d-%d, want %d-%d", p.name, first, last, p.first, p.last)
		}
		if !bytes.Equal(e[0:16], linuxFSTypeGUID) {
			t.Errorf("partition %s lost its type GUID", p.name)
		}
	}
}

// requirePartitionData fails unless every partition's bytes on disk match the
// image.
func requirePartitionData(t *testing.T, disk, img []byte, ss int64, parts []testPart) {
	t.Helper()
	for _, p := range parts {
		lo, hi := p.first*ss, (p.last+1)*ss
		if !bytes.Equal(disk[lo:hi], img[lo:hi]) {
			t.Fatalf("partition %s data differs from the image", p.name)
		}
	}
}

// bmapForImage maps every non-zero block of img, as bmaptool would.
func bmapForImage(img []byte, bs int64) *Bmap {
	b := &Bmap{BlockSize: bs, ImageSize: int64(len(img))}
	zero := make([]byte, bs)
	for blk := int64(0); blk*bs < b.ImageSize; blk++ {
		if bytes.Equal(img[blk*bs:min((blk+1)*bs, b.ImageSize)], zero[:min(bs, b.ImageSize-blk*bs)]) {
			continue
		}
		if n := len(b.Ranges); n > 0 && b.Ranges[n-1].Last == blk-1 {
			b.Ranges[n-1].Last = blk
		} else {
			b.Ranges = append(b.Ranges, BmapRange{First: blk, Last: blk})
		}
	}
	for i := range b.Ranges {
		r := &b.Ranges[i]
		r.Checksum = blockChecksum(img[r.First*bs : min((r.Last+1)*bs, b.ImageSize)])
	}
	return b
}

// newTestDrive creates a file of size bytes standing in for a drive.
func newTestDrive(t *testing.T, size int64) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "drive.img")
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
	return path
}

// relocateTestDrive runs the post-write GPT rewrite on a drive file, as the
// writers do once the image is written.
func relocateTestDrive(t *testing.T, path string) error {
	t.Helper()
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	return relocateGPTOnDevice(f)
}

// readTestDrive returns the drive's bytes, failing if a write went past its end.
func readTestDrive(t *testing.T, path string, size int64) []byte {
	t.Helper()
	disk, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if int64(len(disk)) != size {
		t.Fatalf("drive grew from %d to %d bytes: the write went past its end", size, len(disk))
	}
	return disk
}

// A Jetson-like image at a small scale: it declares an 8 MiB disk (16384
// sectors) and its last partition (the data partition) ends at 4 MiB.
var jetsonLikeParts = []testPart{
	{2048, 4095, "APP"},
	{4096, 6143, "APP_b"},
	{6144, 8191, "data"},
}

const (
	jetsonLikeSectors  = 16384
	jetsonLikeRequired = (8191 + 1 + 33) * 512 // last partition + backup entries and header
)

func TestReadImageGPTMeasuresPartitions(t *testing.T) {
	img := buildTestGPTImage(t, 512, jetsonLikeSectors, jetsonLikeParts)
	g := readImageGPT(imageHead{bytes.NewReader(img[:imageHeadBytes])})
	if g == nil {
		t.Fatal("readImageGPT found no GPT")
	}
	if g.sectorSize != 512 || g.declaredSize != int64(len(img)) || g.requiredSize != jetsonLikeRequired {
		t.Fatalf("readImageGPT = sector %d, declared %d, required %d; want 512, %d, %d",
			g.sectorSize, g.declaredSize, g.requiredSize, len(img), jetsonLikeRequired)
	}
}

func TestImageStreamPeekGPT(t *testing.T) {
	img := buildTestGPTImage(t, 512, jetsonLikeSectors, jetsonLikeParts)
	s := &imageStream{ReadCloser: io.NopCloser(bytes.NewReader(img)), uncompressedSize: int64(len(img))}
	if g := s.peekGPT(); g == nil || g.requiredSize != jetsonLikeRequired {
		t.Fatalf("peekGPT() = %+v, want the image's GPT", g)
	}
	if got, err := io.ReadAll(s); err != nil || !bytes.Equal(got, img) {
		t.Fatalf("stream after peekGPT: %d bytes, %v; want the whole image", len(got), err)
	}

	// A short image without a GPT comes back unchanged too.
	short := []byte("not a disk image")
	s = &imageStream{ReadCloser: io.NopCloser(bytes.NewReader(short))}
	if g := s.peekGPT(); g != nil {
		t.Fatalf("peekGPT() found a GPT in %q", short)
	}
	if got, err := io.ReadAll(s); err != nil || !bytes.Equal(got, short) {
		t.Fatalf("stream after peekGPT = %q, %v; want %q", got, err, short)
	}

	// A read error is kept for the write to report, after the bytes read.
	boom := errors.New("boom")
	s = &imageStream{ReadCloser: io.NopCloser(io.MultiReader(bytes.NewReader(img[:4096]), errReader{boom}))}
	if g := s.peekGPT(); g != nil {
		t.Fatal("peekGPT() parsed a GPT from a failed read")
	}
	if got, err := io.ReadAll(s); !errors.Is(err, boom) || !bytes.Equal(got, img[:4096]) {
		t.Fatalf("stream after a failed peek: %d bytes, %v; want 4096 bytes then boom", len(got), err)
	}
}

func TestReadImageGPTFourKSectors(t *testing.T) {
	// 4096-byte sectors: 4 sectors of entries, so the backup GPT takes 5.
	parts := []testPart{{256, 511, "APP"}, {512, 767, "data"}}
	img := buildTestGPTImage(t, 4096, 2048, parts)
	g := readImageGPTFrom(func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(img)), nil })
	if g == nil {
		t.Fatal("readImageGPT found no GPT")
	}
	if want := int64(767+1+5) * 4096; g.sectorSize != 4096 || g.requiredSize != want {
		t.Fatalf("readImageGPT = sector %d, required %d; want 4096, %d", g.sectorSize, g.requiredSize, want)
	}

	drive := int64(1024 * 4096)
	path := newTestDrive(t, drive)
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(f, imageFit{writeLimit: drive}.limitReader(bytes.NewReader(img))); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if err := relocateTestDrive(t, path); err != nil {
		t.Fatalf("relocate: %v", err)
	}
	disk := readTestDrive(t, path, drive)
	verifyTestGPT(t, disk, 4096, parts)
	requirePartitionData(t, disk, img, 4096, parts)
}

func TestReadImageGPTIgnoresMBRImages(t *testing.T) {
	// A Raspberry Pi style image: an MBR partition table and no GPT.
	img := make([]byte, 4<<20)
	part := img[446:462]
	part[4] = 0x0C // FAT32 LBA
	binary.LittleEndian.PutUint32(part[8:12], 8192)
	binary.LittleEndian.PutUint32(part[12:16], 512)
	img[510], img[511] = 0x55, 0xAA
	if g := readImageGPTFrom(func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(img)), nil }); g != nil {
		t.Fatalf("readImageGPT found a GPT in an MBR image: %+v", g)
	}

	// Without a GPT the drive must hold the whole image, as before.
	small := drive{Name: "SD Card", DevicePath: "/dev/sdb", SizeBytes: int64(len(img)) - 512}
	if _, err := fitImageToDrive(int64(len(img)), nil, small); err == nil || !strings.Contains(err.Error(), "4.2 MB") {
		t.Fatalf("fitImageToDrive(MBR image, smaller drive) = %v, want the full-size refusal", err)
	}
	big := drive{Name: "SD Card", DevicePath: "/dev/sdb", SizeBytes: int64(len(img))}
	if fit, err := fitImageToDrive(int64(len(img)), nil, big); err != nil || fit != (imageFit{}) {
		t.Fatalf("fitImageToDrive(MBR image, equal drive) = %+v, %v; want a plain write", fit, err)
	}
}

func TestFitImageToDrive(t *testing.T) {
	// The numbers from WDY-3974: the Jetson Orin Nano 0.19.5 NVMe image
	// declares a 251.3 GB disk, its partitions end at 27.64 GB, and the drive
	// is a 128 GB SSD in a USB enclosure.
	jetson := &imageGPT{sectorSize: 512, declaredSize: 251_255_586_816, requiredSize: 27_640_000_000}
	ssd := drive{Name: "RTL9210C", DevicePath: "/dev/disk8", SizeBytes: 128_035_676_160}
	tests := []struct {
		name      string
		imageSize int64
		layout    *imageGPT
		d         drive
		want      imageFit
		wantErr   string
	}{
		{"WDY-3974: GPT image on a smaller drive that holds its partitions", 251_255_586_816, jetson, ssd,
			imageFit{writeLimit: ssd.SizeBytes, relocateGPT: true}, ""},
		{"drive too small for the partitions", 251_255_586_816, jetson,
			drive{Name: "SD Card", DevicePath: "/dev/disk4", SizeBytes: 15_931_539_456}, imageFit{}, "needs 27.6 GB"},
		{"drive exactly the partitions' size", 251_255_586_816, jetson,
			drive{DevicePath: "/dev/disk4", SizeBytes: jetson.requiredSize}, imageFit{writeLimit: jetson.requiredSize, relocateGPT: true}, ""},
		{"drive larger than the declared disk", 251_255_586_816, jetson,
			drive{DevicePath: "/dev/disk4", SizeBytes: 500_107_862_016}, imageFit{relocateGPT: true}, ""},
		{"drive the declared disk's size", 251_255_586_816, jetson,
			drive{DevicePath: "/dev/disk4", SizeBytes: 251_255_586_816}, imageFit{}, ""},
		{"unknown image size", 0, jetson, ssd, imageFit{writeLimit: ssd.SizeBytes, relocateGPT: true}, ""},
		{"unknown drive size keeps the old behaviour", 251_255_586_816, jetson, drive{DevicePath: "/dev/disk4"}, imageFit{}, ""},
		{"no GPT: image larger than drive", 18_165_548_032, nil,
			drive{DevicePath: "/dev/sdb", SizeBytes: 7_948_206_080}, imageFit{}, "needs 18.2 GB"},
		{"no GPT: image fits", 4_000_000_000, nil, drive{DevicePath: "/dev/sdb", SizeBytes: 7_948_206_080}, imageFit{}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fit, err := fitImageToDrive(tt.imageSize, tt.layout, tt.d)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) || !strings.Contains(err.Error(), tt.d.DevicePath) {
					t.Fatalf("fitImageToDrive() = %+v, %v; want an error naming %s and containing %q", fit, err, tt.d.DevicePath, tt.wantErr)
				}
				return
			}
			if err != nil || fit != tt.want {
				t.Fatalf("fitImageToDrive() = %+v, %v; want %+v", fit, err, tt.want)
			}
		})
	}
}

// TestBmapWriteToSmallerDrive writes a GPT image declaring an 8 MiB disk to a
// 5 MiB drive through the block-map helper's write, then rewrites the GPT as
// the writers do afterwards.
func TestBmapWriteToSmallerDrive(t *testing.T) {
	img := buildTestGPTImage(t, 512, jetsonLikeSectors, jetsonLikeParts)
	b := bmapForImage(img, 4096)
	const driveSize = 5 << 20

	layout := readImageGPTFrom(func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(img)), nil })
	fit, err := fitImageToDrive(b.ImageSize, layout, drive{DevicePath: "/dev/disk8", SizeBytes: driveSize})
	if err != nil {
		t.Fatalf("fitImageToDrive: %v", err)
	}
	if fit != (imageFit{writeLimit: driveSize, relocateGPT: true}) {
		t.Fatalf("fit = %+v, want a write clipped at the drive's end and a GPT rewrite", fit)
	}

	dir := t.TempDir()
	bmapPath := filepath.Join(dir, "img.bmap")
	if err := os.WriteFile(bmapPath, []byte(bmapXML(b)), 0o644); err != nil {
		t.Fatal(err)
	}
	path := newTestDrive(t, driveSize)
	if err := runBmapWrite(path, bmapPath, bytes.NewReader(img), fit.writeLimit); err != nil {
		t.Fatalf("runBmapWrite: %v", err)
	}
	if err := relocateTestDrive(t, path); err != nil {
		t.Fatalf("relocate: %v", err)
	}
	disk := readTestDrive(t, path, driveSize)
	verifyTestGPT(t, disk, 512, jetsonLikeParts)
	requirePartitionData(t, disk, img, 512, jetsonLikeParts)

	// The rewrite is idempotent: a second pass finds a GPT that already fits.
	if err := relocateTestDrive(t, path); err != nil {
		t.Fatalf("second relocate: %v", err)
	}
	if !bytes.Equal(readTestDrive(t, path, driveSize), disk) {
		t.Fatal("a second relocate changed a GPT that already fit the drive")
	}
}

// TestSeekableBmapWriteToSmallerDrive covers the seekable-zstd path: the GPT is
// read from the compressed image, and the mapped backup GPT past the drive's
// end is read and verified but not written.
func TestSeekableBmapWriteToSmallerDrive(t *testing.T) {
	img := buildTestGPTImage(t, 512, jetsonLikeSectors, jetsonLikeParts)
	b := bmapForImage(img, 4096)
	zst := filepath.Join(t.TempDir(), "image.img.zst")
	if err := os.WriteFile(zst, encodeSeekable(t, img, 64<<10), 0o644); err != nil {
		t.Fatal(err)
	}
	// Exactly what the partitions need: not even a whole 4 KiB block.
	const driveSize = jetsonLikeRequired

	layout := readImageGPTFrom(func() (io.ReadCloser, error) { return openSeekableZstd(zst) })
	fit, err := fitImageToDrive(b.ImageSize, layout, drive{DevicePath: "/dev/disk8", SizeBytes: driveSize})
	if err != nil {
		t.Fatalf("fitImageToDrive: %v", err)
	}

	si, err := openSeekableZstd(zst)
	if err != nil {
		t.Fatal(err)
	}
	defer si.Close()
	path := newTestDrive(t, driveSize)
	dev, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	var progress int64
	if err := applyBmapSeekable(si, clipWrites(dev, fit.writeLimit), b, func(n int64) { progress = n }); err != nil {
		t.Fatalf("applyBmapSeekable: %v", err)
	}
	dev.Close()
	if progress != mappedBytes(b) {
		t.Errorf("progress = %d, want every mapped byte (%d), skipped ones included", progress, mappedBytes(b))
	}
	if err := relocateTestDrive(t, path); err != nil {
		t.Fatalf("relocate: %v", err)
	}
	disk := readTestDrive(t, path, driveSize)
	verifyTestGPT(t, disk, 512, jetsonLikeParts)
	requirePartitionData(t, disk, img, 512, jetsonLikeParts)
}

// TestSequentialWriteToSmallerDrive covers the dd path and its fallback: the
// stream stops at the drive's end.
func TestSequentialWriteToSmallerDrive(t *testing.T) {
	img := buildTestGPTImage(t, 512, jetsonLikeSectors, jetsonLikeParts)
	const driveSize = 6 << 20
	fit, err := fitImageToDrive(int64(len(img)), readImageGPT(imageHead{bytes.NewReader(img)}), drive{DevicePath: "/dev/sdb", SizeBytes: driveSize})
	if err != nil {
		t.Fatal(err)
	}
	if got := fit.sequentialTotal(int64(len(img))); got != driveSize {
		t.Errorf("sequentialTotal = %d, want the drive size %d", got, driveSize)
	}
	path := newTestDrive(t, driveSize)
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	n, err := io.Copy(f, fit.limitReader(bytes.NewReader(img)))
	f.Close()
	if err != nil || n != driveSize {
		t.Fatalf("copied %d bytes, %v; want %d", n, err, driveSize)
	}
	if err := relocateTestDrive(t, path); err != nil {
		t.Fatalf("relocate: %v", err)
	}
	disk := readTestDrive(t, path, driveSize)
	verifyTestGPT(t, disk, 512, jetsonLikeParts)
	requirePartitionData(t, disk, img, 512, jetsonLikeParts)
}

func TestRelocateGPTToLargerDrive(t *testing.T) {
	img := buildTestGPTImage(t, 512, jetsonLikeSectors, jetsonLikeParts)
	const driveSize = 12 << 20
	fit, err := fitImageToDrive(int64(len(img)), readImageGPT(imageHead{bytes.NewReader(img)}), drive{DevicePath: "/dev/sdb", SizeBytes: driveSize})
	if err != nil || fit != (imageFit{relocateGPT: true}) {
		t.Fatalf("fitImageToDrive = %+v, %v; want a full write and a GPT rewrite", fit, err)
	}
	path := newTestDrive(t, driveSize)
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(f, fit.limitReader(bytes.NewReader(img))); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if err := relocateTestDrive(t, path); err != nil {
		t.Fatalf("relocate: %v", err)
	}
	disk := readTestDrive(t, path, driveSize)
	verifyTestGPT(t, disk, 512, jetsonLikeParts)
	requirePartitionData(t, disk, img, 512, jetsonLikeParts)
}

// TestRelocateGPTRefusesTruncatedPartition: a drive that cuts into a
// partition is refused before anything is written, and the post-write rewrite
// independently refuses to touch such a drive.
func TestRelocateGPTRefusesTruncatedPartition(t *testing.T) {
	img := buildTestGPTImage(t, 512, jetsonLikeSectors, jetsonLikeParts)
	// The data partition ends exactly at 4 MiB, so the backup GPT cannot fit.
	const driveSize = 4 << 20
	d := drive{Name: "SD Card", DevicePath: "/dev/sdb", SizeBytes: driveSize}
	if _, err := fitImageToDrive(int64(len(img)), readImageGPT(imageHead{bytes.NewReader(img)}), d); err == nil ||
		!strings.Contains(err.Error(), "/dev/sdb") || !strings.Contains(err.Error(), formatBytes(jetsonLikeRequired)) {
		t.Fatalf("fitImageToDrive() = %v; want a refusal naming the drive and %s", err, formatBytes(jetsonLikeRequired))
	}

	path := newTestDrive(t, driveSize)
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(f, io.LimitReader(bytes.NewReader(img), driveSize)); err != nil {
		t.Fatal(err)
	}
	f.Close()
	before := readTestDrive(t, path, driveSize)
	if err := relocateTestDrive(t, path); err == nil || !strings.Contains(err.Error(), "partitions need") {
		t.Fatalf("relocate on a drive that truncates a partition = %v, want a refusal", err)
	}
	if !bytes.Equal(readTestDrive(t, path, driveSize), before) {
		t.Fatal("a refused relocate wrote to the drive")
	}
}

func TestResizeProtectiveMBR(t *testing.T) {
	newMBR := func(size uint32) *memReadWriterAt {
		m := &memReadWriterAt{buf: make([]byte, 512)}
		e := m.buf[446:462]
		e[4] = 0xEE
		binary.LittleEndian.PutUint32(e[8:12], 1)
		binary.LittleEndian.PutUint32(e[12:16], size)
		m.buf[510], m.buf[511] = 0x55, 0xAA
		return m
	}
	size := func(m *memReadWriterAt) uint32 { return binary.LittleEndian.Uint32(m.buf[446+12:]) }

	m := newMBR(490733567) // the 251 GB disk the Jetson image declares
	if err := resizeProtectiveMBR(m, 512, 490733568, 250069680); err != nil || size(m) != 250069679 {
		t.Fatalf("shrunk protective MBR = %d, %v; want 250069679", size(m), err)
	}
	// Past 2 TiB the field saturates instead of wrapping.
	m = newMBR(490733567)
	if err := resizeProtectiveMBR(m, 512, 490733568, 7814037168); err != nil || size(m) != 0xFFFFFFFF {
		t.Fatalf("protective MBR for a 4 TB drive = %#x, %v; want 0xffffffff", size(m), err)
	}
	// An entry that did not cover the declared disk (a hybrid MBR) stays.
	m = newMBR(2048)
	if err := resizeProtectiveMBR(m, 512, 490733568, 250069680); err != nil || size(m) != 2048 {
		t.Fatalf("hybrid MBR entry = %d, %v; want it left at 2048", size(m), err)
	}
}

type memReadWriterAt struct{ buf []byte }

func (m *memReadWriterAt) ReadAt(p []byte, off int64) (int, error) {
	return copy(p, m.buf[off:]), nil
}

func (m *memReadWriterAt) WriteAt(p []byte, off int64) (int, error) {
	return copy(m.buf[off:], p), nil
}

func TestClipWrites(t *testing.T) {
	dst := newMemWriterAt(16)
	w := clipWrites(dst, 10)
	if n, err := w.WriteAt([]byte("abcdefgh"), 6); err != nil || n != 8 {
		t.Fatalf("straddling WriteAt = %d, %v; want 8, nil", n, err)
	}
	if n, err := w.WriteAt([]byte("zz"), 12); err != nil || n != 2 {
		t.Fatalf("WriteAt past the limit = %d, %v; want 2, nil", n, err)
	}
	if string(dst.buf[6:10]) != "abcd" {
		t.Fatalf("in-range bytes = %q, want %q", dst.buf[6:10], "abcd")
	}
	for off := 10; off < 16; off++ {
		if dst.written[off] {
			t.Fatalf("byte %d past the limit was written", off)
		}
	}
	if clipWrites(dst, 0) != io.WriterAt(dst) {
		t.Fatal("a zero limit should not wrap the writer")
	}
}

func TestBmapDataEndBeforeBackupGPT(t *testing.T) {
	const bs = 4096
	// The WDY-3974 bmap: data ends at block 6551807, then the backup GPT in
	// the image's last 5 blocks (61341691-61341695).
	jetson := &Bmap{BlockSize: bs, ImageSize: 251_255_586_816, Ranges: []BmapRange{
		{First: 0, Last: 100}, {First: 6550784, Last: 6551807}, {First: 61341691, Last: 61341695},
	}}
	if end, ok := bmapDataEndBeforeBackupGPT(jetson); !ok || end != 6551808*bs {
		t.Fatalf("bmapDataEndBeforeBackupGPT(jetson) = %d, %v; want %d, true", end, ok, 6551808*bs)
	}
	for name, b := range map[string]*Bmap{
		"last range ends before the image": {BlockSize: bs, ImageSize: 100 * bs, Ranges: []BmapRange{{First: 0, Last: 1}, {First: 50, Last: 98}}},
		"last range too long for a GPT":    {BlockSize: bs, ImageSize: 100 * bs, Ranges: []BmapRange{{First: 0, Last: 1}, {First: 50, Last: 99}}},
		"single range":                     {BlockSize: bs, ImageSize: 100 * bs, Ranges: []BmapRange{{First: 95, Last: 99}}},
	} {
		if _, ok := bmapDataEndBeforeBackupGPT(b); ok {
			t.Errorf("%s: treated the last range as a backup GPT", name)
		}
	}
}

func TestGPTRelocateCommandIsHidden(t *testing.T) {
	root := NewRootCmd()
	c, _, err := root.Find([]string{"__gpt-relocate"})
	if err != nil {
		t.Fatalf("find __gpt-relocate: %v", err)
	}
	if c.Name() != "__gpt-relocate" || !c.Hidden {
		t.Fatalf("__gpt-relocate should be a hidden command; got name=%q hidden=%v", c.Name(), c.Hidden)
	}
}
