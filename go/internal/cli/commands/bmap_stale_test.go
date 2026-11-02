package commands

import (
	"bytes"
	"encoding/binary"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/cli/tegraflash/t234"
)

// A Jetson-like image whose `data` partition ships empty, as the WendyOS
// Jetson NVMe image's does: the block map maps no byte of it. APP carries
// data only in its first and last sectors, so most of it is a hole too.
var staleTestParts = []testPart{
	{2048, 6143, "APP"},   // 2 MiB at 1 MiB
	{6144, 38911, "data"}, // 16 MiB at 3 MiB
}

const (
	staleTestSectors = 40960 // the image declares a 20 MiB disk
	staleDataStart   = 6144 * 512
	staleDataEnd     = 38912 * 512
)

func buildStaleTestImage(t *testing.T) []byte {
	t.Helper()
	img := buildTestGPTImage(t, 512, staleTestSectors, staleTestParts)
	clear(img[staleDataStart:staleDataEnd])
	return img
}

// staleTestBmap maps the image's non-zero blocks and both GPTs whole, as
// bmaptool maps the Jetson NVMe image (its 0.20.1 bmap starts with blocks 0-4
// and ends with the last five). bmapForImage would leave the zero parts of the
// entry arrays unmapped, and on a drive that is not blank those would keep the
// previous table's entries.
func staleTestBmap(img []byte) *Bmap {
	const bs = 4096
	blocks := int64(len(img)) / bs
	gptBlocks := int64(34*512+bs-1) / bs
	b := &Bmap{BlockSize: bs, ImageSize: int64(len(img))}
	for blk := int64(0); blk < blocks; blk++ {
		if blk >= gptBlocks && blk < blocks-gptBlocks && isAll(img[blk*bs:(blk+1)*bs], 0) {
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
		r.Checksum = blockChecksum(img[r.First*bs : (r.Last+1)*bs])
	}
	return b
}

// newStaleTestDrive is a drive a previous installation filled: every byte is
// stale, and the data partition starts with that installation's ext4
// superblock.
func newStaleTestDrive(t *testing.T, size int64) string {
	t.Helper()
	disk := bytes.Repeat([]byte{0xA5}, int(size))
	binary.LittleEndian.PutUint16(disk[staleDataStart+1024+56:], 0xEF53)
	path := filepath.Join(t.TempDir(), "drive.img")
	if err := os.WriteFile(path, disk, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// requireStaleDataCleared fails unless the drive's data partition reads as
// zeros where first boot looks for a filesystem, its start and its end, and
// still holds stale bytes in between: the write clears windows, not whole
// partitions.
func requireStaleDataCleared(t *testing.T, disk []byte) {
	t.Helper()
	headEnd, tailStart := int64(staleDataStart+t234.StaleHeadBytes), int64(staleDataEnd-t234.StaleTailBytes)
	if !isAll(disk[staleDataStart:headEnd], 0) {
		t.Error("the start of the data partition still holds the old filesystem")
	}
	if !isAll(disk[tailStart:staleDataEnd], 0) {
		t.Error("the end of the data partition still holds stale data")
	}
	if !isAll(disk[headEnd:tailStart], 0xA5) {
		t.Error("the middle of the data partition was written; only its start and end should be")
	}
}

func isAll(b []byte, v byte) bool { return bytes.Count(b, []byte{v}) == len(b) }

// TestBmapWriteClearsStalePartitions reflashes a drive over a previous
// installation through the streaming block-map helper. The empty data
// partition must not keep the old filesystem, APP must match the image byte
// for byte (its holes included, as a full write leaves them), and nothing
// outside the image's partitions may be written.
func TestBmapWriteClearsStalePartitions(t *testing.T) {
	img := buildStaleTestImage(t)
	b := staleTestBmap(img)
	for _, r := range b.Ranges {
		if r.First*b.BlockSize < staleDataEnd && (r.Last+1)*b.BlockSize > staleDataStart {
			t.Fatalf("the block map maps blocks %d-%d of the data partition; it should map none", r.First, r.Last)
		}
	}
	bmapPath := filepath.Join(t.TempDir(), "img.bmap")
	if err := os.WriteFile(bmapPath, []byte(bmapXML(b)), 0o644); err != nil {
		t.Fatal(err)
	}
	const driveSize = 24 << 20 // larger than the 20 MiB disk the image declares
	path := newStaleTestDrive(t, driveSize)
	if err := runBmapWrite(path, bmapPath, bytes.NewReader(img), 0); err != nil {
		t.Fatalf("runBmapWrite: %v", err)
	}
	if err := relocateTestDrive(t, path); err != nil {
		t.Fatalf("relocate: %v", err)
	}
	disk := readTestDrive(t, path, driveSize)
	verifyTestGPT(t, disk, 512, staleTestParts)
	requireStaleDataCleared(t, disk)
	requirePartitionData(t, disk, img, 512, staleTestParts[:1])
	imageBackupGPT := b.Ranges[len(b.Ranges)-1].First * b.BlockSize
	if !isAll(disk[staleDataEnd:imageBackupGPT], 0xA5) {
		t.Error("the image's free space after its last partition was written")
	}
	if !isAll(disk[staleTestSectors*512:driveSize-33*512], 0xA5) {
		t.Error("the drive past the image's end was written")
	}
}

// TestSeekableBmapWriteClearsStalePartitions covers the seekable-zstd path on
// a drive smaller than the disk the image declares, where every write is
// clipped at the drive's end: the clear must stay inside the drive too.
func TestSeekableBmapWriteClearsStalePartitions(t *testing.T) {
	img := buildStaleTestImage(t)
	b := staleTestBmap(img)
	zst := filepath.Join(t.TempDir(), "image.img.zst")
	if err := os.WriteFile(zst, encodeSeekable(t, img, 64<<10), 0o644); err != nil {
		t.Fatal(err)
	}
	const driveSize = staleDataEnd + 33*512 // exactly what the partitions need

	layout := readImageGPTFrom(func() (io.ReadCloser, error) { return openSeekableZstd(zst) })
	fit, err := fitImageToDrive(b.ImageSize, layout, drive{DevicePath: "/dev/disk8", SizeBytes: driveSize})
	if err != nil || fit.writeLimit != driveSize {
		t.Fatalf("fitImageToDrive = %+v, %v; want a write clipped at the drive's end", fit, err)
	}
	si, err := openSeekableZstd(zst)
	if err != nil {
		t.Fatal(err)
	}
	defer si.Close()
	path := newStaleTestDrive(t, driveSize)
	dev, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := applyBmapSeekable(si, clipWrites(dev, fit.writeLimit), b, func(int64) {}); err != nil {
		t.Fatalf("applyBmapSeekable: %v", err)
	}
	dev.Close()
	if err := relocateTestDrive(t, path); err != nil {
		t.Fatalf("relocate: %v", err)
	}
	disk := readTestDrive(t, path, driveSize)
	verifyTestGPT(t, disk, 512, staleTestParts)
	requireStaleDataCleared(t, disk)
	requirePartitionData(t, disk, img, 512, staleTestParts[:1])
}

// TestSequentialWriteClearsStaleData pins the full-write path, which has no
// block map and so writes the image's zeros everywhere, including on a drive
// smaller than the disk the image declares.
func TestSequentialWriteClearsStaleData(t *testing.T) {
	img := buildStaleTestImage(t)
	const driveSize = 19<<20 + 64<<10 // room for the partitions, less than the 20 MiB declared
	fit, err := fitImageToDrive(int64(len(img)), readImageGPT(imageHead{bytes.NewReader(img)}), drive{DevicePath: "/dev/sdb", SizeBytes: driveSize})
	if err != nil || fit.writeLimit != driveSize {
		t.Fatalf("fitImageToDrive = %+v, %v; want a write clipped at the drive's end", fit, err)
	}
	path := newStaleTestDrive(t, driveSize)
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
	verifyTestGPT(t, disk, 512, staleTestParts)
	requirePartitionData(t, disk, img, 512, staleTestParts)
}
