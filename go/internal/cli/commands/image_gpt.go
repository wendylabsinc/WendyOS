package commands

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"syscall"

	"github.com/diskfs/go-diskfs/backend"
	"github.com/diskfs/go-diskfs/partition/gpt"
	"github.com/wendylabsinc/wendy/go/internal/cli/tegraflash/t234"
)

// A GPT image declares the size of the disk it was built for: its backup
// header sits in that disk's last sector. The Jetson NVMe image declares a
// 251 GB disk while its partitions end at 27.6 GB (the data partition grows
// on first boot), so the image's size says little about the drive it needs.
// It needs room for its partitions plus the backup GPT. A smaller drive holds
// it once the part of the image past the drive's end — only the old backup
// GPT lies there — is skipped and the GPT is rewritten for the drive's real
// size, as `sgdisk -e` does. Linux rejects a GPT whose last usable LBA lies
// past the end of the disk, so that rewrite is not optional (WDY-3974).

// imageHeadBytes is how much of an image is read to parse its primary GPT:
// the protective MBR, the header and its entries, at 512- or 4096-byte sectors.
const imageHeadBytes = 1 << 20

// gptSectorSizes are the logical sector sizes a GPT is looked for at.
var gptSectorSizes = []int64{512, 4096}

// imageGPT is the primary GPT read from the start of an image or a drive.
type imageGPT struct {
	table      *gpt.Table
	sectorSize int64
	// declaredSize is the disk size the GPT was built for: the backup
	// header's LBA plus one, in bytes.
	declaredSize int64
	// requiredSize is the smallest drive that holds every partition and,
	// after them, the backup partition entries and header.
	requiredSize int64
}

// readImageGPT parses the primary GPT at the start of f. It returns nil when
// f carries none that can be rewritten for another disk size — an MBR image,
// or a GPT whose geometry go-diskfs cannot write back — and the caller then
// keeps checking the image's full size.
func readImageGPT(f backend.File) *imageGPT {
	for _, ss := range gptSectorSizes {
		// Whole sectors only: raw devices refuse partial-sector reads.
		hdr := make([]byte, ss)
		if _, err := f.ReadAt(hdr, ss); err != nil || string(hdr[:8]) != "EFI PART" {
			continue
		}
		return parseImageGPT(f, ss, hdr)
	}
	return nil
}

func parseImageGPT(f backend.File, ss int64, hdr []byte) *imageGPT {
	// go-diskfs reads the entries wherever the header points but writes them
	// back right after the header, and only as 128-byte entries that fill
	// whole sectors. Leave any other layout to the full-size check.
	entries := int64(binary.LittleEndian.Uint32(hdr[80:84]))
	if binary.LittleEndian.Uint64(hdr[24:32]) != 1 ||
		binary.LittleEndian.Uint64(hdr[72:80]) != 2 ||
		binary.LittleEndian.Uint32(hdr[84:88]) != gpt.PartitionEntrySize ||
		entries == 0 || entries*gpt.PartitionEntrySize%ss != 0 {
		return nil
	}
	table, err := gpt.Read(f, int(ss), int(ss))
	if err != nil || len(table.Partitions) == 0 {
		return nil
	}
	declaredSectors := int64(table.TotalSize() / uint64(ss))
	lastUsable := int64(table.LastDataSector())
	var lastEnd int64
	for _, p := range table.Partitions {
		lastEnd = max(lastEnd, int64(p.End))
	}
	if lastEnd > lastUsable || lastUsable >= declaredSectors-1 {
		return nil
	}
	backupSectors := entries*gpt.PartitionEntrySize/ss + 1
	return &imageGPT{
		table:        table,
		sectorSize:   ss,
		declaredSize: declaredSectors * ss,
		requiredSize: (lastEnd + 1 + backupSectors) * ss,
	}
}

// imageHead serves the first bytes of an image to go-diskfs, which reads a
// GPT through backend.File; only ReadAt is used.
type imageHead struct{ *bytes.Reader }

func (imageHead) Stat() (fs.FileInfo, error) { return nil, errors.ErrUnsupported }
func (imageHead) Close() error               { return nil }

// readImageHead reads the first imageHeadBytes of an image (fewer if it is
// shorter) and parses its GPT, which is nil when there is none or the read
// failed.
func readImageHead(r io.Reader) ([]byte, *imageGPT, error) {
	head := make([]byte, imageHeadBytes)
	n, err := io.ReadFull(r, head)
	head = head[:n]
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return head, nil, err
	}
	return head, readImageGPT(imageHead{bytes.NewReader(head)}), nil
}

// readImageGPTFrom opens an image and parses the GPT at its start. Any
// failure yields nil, which keeps the full-size check.
func readImageGPTFrom(open func() (io.ReadCloser, error)) *imageGPT {
	r, err := open()
	if err != nil {
		return nil
	}
	defer r.Close()
	_, g, _ := readImageHead(r)
	return g
}

// peekGPT parses the GPT at the start of the stream without consuming it:
// the bytes it reads are put back in front of the stream, and a read error
// is kept for the write to report.
func (s *imageStream) peekGPT() *imageGPT {
	head, g, err := readImageHead(s.ReadCloser)
	rest := io.Reader(s.ReadCloser)
	if err != nil {
		rest = errReader{err}
	}
	s.ReadCloser = struct {
		io.Reader
		io.Closer
	}{io.MultiReader(bytes.NewReader(head), rest), s.ReadCloser}
	return g
}

type errReader struct{ err error }

func (r errReader) Read([]byte) (int, error) { return 0, r.err }

// imageFit is how an image goes onto a particular drive.
type imageFit struct {
	// writeLimit, when positive, is the drive's end: image bytes at or past
	// it are not written. Only set for a GPT image whose partitions end
	// before it.
	writeLimit int64
	// relocateGPT rewrites the GPT for the drive's real size once the image
	// is written.
	relocateGPT bool
}

// fitImageToDrive refuses a drive that cannot hold the image and says how to
// write it. imageSize is the byte count the write would produce and layout the
// image's GPT, or nil when it has none: then the drive must hold the whole
// image, as before. An unknown drive size skips the check.
func fitImageToDrive(imageSize int64, layout *imageGPT, d drive) (imageFit, error) {
	if layout == nil || d.SizeBytes <= 0 {
		return imageFit{}, checkImageFitsDrive(imageSize, d)
	}
	if layout.requiredSize > d.SizeBytes {
		return imageFit{}, driveTooSmallError(d, formatBytes(layout.requiredSize))
	}
	var fit imageFit
	if max(imageSize, layout.declaredSize) > d.SizeBytes {
		fit.writeLimit = d.SizeBytes
	}
	fit.relocateGPT = layout.declaredSize != d.SizeBytes
	return fit, nil
}

// noteImageFit tells the user when an image goes onto a drive smaller than
// the disk its GPT declares, which would otherwise look like a mistake.
func noteImageFit(fit imageFit, layout *imageGPT, d drive) {
	if fit.writeLimit <= 0 || layout == nil {
		return
	}
	fmt.Printf("Note: the image's partition table is laid out for a %s disk, but its partitions need only %s; "+
		"writing them to %s and resizing the partition table for the drive.\n",
		formatBytes(layout.declaredSize), formatBytes(layout.requiredSize), d.DevicePath)
}

// limitReader stops a sequential write at the drive's end.
func (f imageFit) limitReader(r io.Reader) io.Reader {
	if f.writeLimit <= 0 {
		return r
	}
	return io.LimitReader(r, f.writeLimit)
}

// sequentialTotal is how many bytes of a size-byte image a sequential write
// puts on the drive.
func (f imageFit) sequentialTotal(size int64) int64 {
	if f.writeLimit > 0 && size > f.writeLimit {
		return f.writeLimit
	}
	return size
}

// clipWrites drops the part of every write at or past limit, the end of a
// drive smaller than the image; a limit of 0 writes everything. Dropped bytes
// still count as written, so a block-map write still reads and verifies them.
func clipWrites(w io.WriterAt, limit int64) io.WriterAt {
	if limit <= 0 {
		return w
	}
	return clipWriterAt{w: w, limit: limit}
}

type clipWriterAt struct {
	w     io.WriterAt
	limit int64
}

func (c clipWriterAt) WriteAt(p []byte, off int64) (int, error) {
	if off >= c.limit {
		return len(p), nil
	}
	if off+int64(len(p)) <= c.limit {
		return c.w.WriteAt(p, off)
	}
	if n, err := c.w.WriteAt(p[:c.limit-off], off); err != nil {
		return n, err
	}
	return len(p), nil
}

// gptRelocationError is a failure to rewrite the GPT for the drive's size
// after the image itself was written. Writing the image again would not help,
// so the full-image fallback is not tried for it.
type gptRelocationError struct {
	drive string
	err   error
}

func (e *gptRelocationError) Error() string {
	return fmt.Sprintf("the image was written, but rewriting its partition table for the size of %s failed: %v", e.drive, e.err)
}

func (e *gptRelocationError) Unwrap() error { return e.err }

// relocateGPTOnDevice rewrites the GPT on a freshly written drive for the
// drive's size, which it reads from the device itself.
func relocateGPTOnDevice(dev *os.File) error {
	size, err := t234.BlockDeviceSize(dev)
	if err != nil {
		return err
	}
	return relocateGPT(dev, size)
}

// relocateGPT rewrites the GPT on a freshly written drive of size bytes the
// way `sgdisk -e` does: the primary header's backup LBA and last usable LBA
// move to the drive's real end, the backup entries and header are written
// there, every CRC is recomputed, and the protective MBR is resized. The
// partitions themselves are not changed. It refuses, writing nothing, when a
// partition would not fit, and does nothing when the GPT already fits.
func relocateGPT(dev backend.WritableFile, size int64) error {
	g := readImageGPT(dev)
	if g == nil {
		return errors.New("no GPT found at the start of the drive")
	}
	if g.requiredSize > size {
		return fmt.Errorf("its partitions need %s but the drive holds %s", formatBytes(g.requiredSize), formatBytes(size))
	}
	sectors := size / g.sectorSize
	newSize := sectors * g.sectorSize
	if g.declaredSize == newSize {
		return nil
	}
	// go-diskfs writes the protective MBR at a byte offset raw devices
	// refuse and truncates its size past 2 TiB; resizeProtectiveMBR does it.
	g.table.ProtectiveMBR = false
	g.table.Resize(uint64(newSize))
	if err := g.table.Write(dev, newSize); err != nil {
		return err
	}
	if err := resizeProtectiveMBR(dev, g.sectorSize, g.declaredSize/g.sectorSize, sectors); err != nil {
		return fmt.Errorf("resizing the protective MBR: %w", err)
	}
	after := readImageGPT(dev)
	if after == nil || after.declaredSize != newSize {
		return errors.New("the rewritten GPT does not read back")
	}
	if err := after.table.Verify(dev, uint64(newSize)); err != nil {
		return fmt.Errorf("verifying the rewritten GPT: %w", err)
	}
	return nil
}

// resizeProtectiveMBR points the protective MBR's 0xEE entry at the drive's
// last sector, capped at 0xFFFFFFFF as the UEFI spec requires. An MBR whose
// 0xEE entry did not cover the image's whole declared disk (a hybrid MBR, for
// one) is left alone.
func resizeProtectiveMBR(dev interface {
	io.ReaderAt
	io.WriterAt
}, ss, oldSectors, newSectors int64) error {
	mbr := make([]byte, ss)
	if _, err := dev.ReadAt(mbr, 0); err != nil {
		return err
	}
	entry := mbr[446:462]
	if mbr[510] != 0x55 || mbr[511] != 0xAA || entry[4] != 0xEE ||
		binary.LittleEndian.Uint32(entry[12:16]) != protectiveMBRSectors(oldSectors) {
		return nil
	}
	binary.LittleEndian.PutUint32(entry[12:16], protectiveMBRSectors(newSectors))
	_, err := dev.WriteAt(mbr, 0)
	return err
}

// protectiveMBRSectors is the size field of a protective MBR on a disk of
// diskSectors sectors: every sector after LBA 0, capped at 0xFFFFFFFF.
func protectiveMBRSectors(diskSectors int64) uint32 {
	return uint32(min(diskSectors-1, 0xFFFFFFFF))
}

// runGPTRelocate is the body of the hidden __gpt-relocate helper, which macOS
// and Linux re-exec under sudo after writing an image to a drive of another
// size than the image's GPT declares. It runs as root.
func runGPTRelocate(devicePath string) (retErr error) {
	if err := validateDeviceTarget(devicePath); err != nil {
		return err
	}
	dev, err := os.OpenFile(devicePath, os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("opening device %s: %w", devicePath, err)
	}
	defer func() {
		if cerr := dev.Close(); cerr != nil && retErr == nil {
			retErr = fmt.Errorf("closing device %s: %w", devicePath, cerr)
		}
	}()
	if err := relocateGPTOnDevice(dev); err != nil {
		return err
	}
	// macOS raw character devices reject fsync with ENOTTY; the parent runs
	// sync(8) after this exits, as for the block-map write.
	if err := dev.Sync(); err != nil && !errors.Is(err, syscall.ENOTTY) {
		return fmt.Errorf("syncing device %s: %w", devicePath, err)
	}
	return nil
}
