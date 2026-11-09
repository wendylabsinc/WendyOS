package commands

import "fmt"

// sectorWriterAt adapts a raw drive's positional write to the io.WriterAt the
// block-map writers take. pwrite must write p at off in one call, without
// reading or moving a file pointer other calls share: on NVMe drives
// applyBmapSeekable calls WriteAt from several goroutines at once
// (writersForStorage). A seek followed by a write lets another writer's seek
// land in between and puts the chunk at that writer's offset, and since the
// block map's checksums cover the image rather than the drive, the flash
// still reports success. On Windows pwrite is WriteFile with the offset in an
// OVERLAPPED (handleWriterAt).
type sectorWriterAt struct {
	pwrite func(p []byte, off int64) (int, error)
}

func (s sectorWriterAt) WriteAt(p []byte, off int64) (int, error) {
	// Raw physical-drive writes must be a multiple of the sector size. Pad the
	// final sub-sector chunk with zeros (matches writeImageToDisk's behavior).
	const sector = 512
	buf := p
	if rem := len(p) % sector; rem != 0 {
		padded := make([]byte, len(p)+(sector-rem))
		copy(padded, p)
		buf = padded
	}
	done, err := s.pwrite(buf, off)
	if err != nil {
		return 0, err
	}
	if done < len(buf) {
		return 0, fmt.Errorf("short write at offset %d: wrote %d of %d bytes", off, done, len(buf))
	}
	// Report only the caller's requested byte count, not the padding.
	return len(p), nil
}
