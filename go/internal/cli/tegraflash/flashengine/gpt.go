package flashengine

import (
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"math"
	"unicode/utf16"
)

// gptLayout is the partition layout a flash package gives a disk, read from
// the primary GPT image the flash writes to it.
type gptLayout struct {
	sectorSize int64
	// backupBytes is the room the backup GPT takes at the end of a disk: the
	// partition entries, then the header.
	backupBytes int64
	// partitions are the table's used entries, in table order.
	partitions []gptPartition
}

// gptPartition is one partition, as the byte range [start, end) of the disk.
type gptPartition struct {
	name       string
	start, end int64
}

const (
	gptHeaderSize = 92  // the header's fields; the rest of its sector is padding
	gptEntrySize  = 128 // the smallest partition entry
)

// parseGPTImage reads a primary GPT image as a flash package ships it: the
// header's sector followed by the partition entries, which the flash writes
// to the disk at byte offset. The header records its own LBA, so offset gives
// the sector size the layout was built for. Both checksums are verified: a
// damaged image is refused, not read as some other layout.
func parseGPTImage(data []byte, offset int64) (gptLayout, error) {
	le := binary.LittleEndian
	if len(data) < gptHeaderSize || string(data[:8]) != "EFI PART" {
		return gptLayout{}, fmt.Errorf("it does not start with a GPT header")
	}
	headerSize := le.Uint32(data[12:16])
	if headerSize < gptHeaderSize || uint64(headerSize) > uint64(len(data)) {
		return gptLayout{}, fmt.Errorf("its header claims a size of %d bytes", headerSize)
	}
	header := append([]byte(nil), data[:headerSize]...)
	le.PutUint32(header[16:20], 0)
	if crc32.ChecksumIEEE(header) != le.Uint32(data[16:20]) {
		return gptLayout{}, fmt.Errorf("its header fails its checksum")
	}

	headerLBA := le.Uint64(data[24:32])
	if headerLBA == 0 || offset <= 0 || uint64(offset)%headerLBA != 0 {
		return gptLayout{}, fmt.Errorf("its header is for LBA %d but is written at byte %d", headerLBA, offset)
	}
	sector := offset / int64(headerLBA)
	if sector != 512 && sector != 4096 {
		return gptLayout{}, fmt.Errorf("its header is for LBA %d but is written at byte %d: not a sector size disks have", headerLBA, offset)
	}

	// The entries follow the header in the image as they do on the disk.
	entriesLBA, count, entrySize := le.Uint64(data[72:80]), uint64(le.Uint32(data[80:84])), uint64(le.Uint32(data[84:88]))
	sectors := uint64(len(data)) / uint64(sector)
	if entriesLBA <= headerLBA || entriesLBA-headerLBA > sectors || count == 0 || entrySize < gptEntrySize {
		return gptLayout{}, fmt.Errorf("its header describes %d partition entries of %d bytes at LBA %d", count, entrySize, entriesLBA)
	}
	tableStart, tableSize := (entriesLBA-headerLBA)*uint64(sector), count*entrySize
	if tableSize > uint64(len(data)) || tableStart > uint64(len(data))-tableSize {
		return gptLayout{}, fmt.Errorf("it is %d bytes, too short for its %d partition entries", len(data), count)
	}
	table := data[tableStart : tableStart+tableSize]
	if crc32.ChecksumIEEE(table) != le.Uint32(data[88:92]) {
		return gptLayout{}, fmt.Errorf("its partition entries fail their checksum")
	}

	layout := gptLayout{
		sectorSize:  sector,
		backupBytes: (int64(tableSize+uint64(sector)-1)/sector + 1) * sector,
	}
	lastLBA := uint64(math.MaxInt64/sector) - 1 // the last LBA whose end is still an int64 byte offset
	for i := uint64(0); i < count; i++ {
		e := table[i*entrySize : (i+1)*entrySize]
		if [16]byte(e[:16]) == [16]byte{} {
			continue // unused entry
		}
		first, last := le.Uint64(e[32:40]), le.Uint64(e[40:48])
		if last < first || last > lastLBA {
			return gptLayout{}, fmt.Errorf("its partition entry %d spans LBA %d to %d", i+1, first, last)
		}
		var name []uint16
		for off := 56; off+2 <= gptEntrySize; off += 2 {
			u := le.Uint16(e[off:])
			if u == 0 {
				break
			}
			name = append(name, u)
		}
		layout.partitions = append(layout.partitions, gptPartition{
			name:  string(utf16.Decode(name)),
			start: int64(first) * sector,
			end:   int64(last+1) * sector,
		})
	}
	return layout, nil
}
