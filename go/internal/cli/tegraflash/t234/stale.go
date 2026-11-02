package t234

import (
	"cmp"
	"fmt"
	"io"
	"slices"
)

// A reflash must leave the board as it would be on a brand-new disk, where
// every byte the image does not write reads as zero. Writing only what the
// image holds leaves the rest of the disk as the previous installation left
// it, and WendyOS lays `data` out at the same offset on every release: on
// first boot the new OS grows the partition back over the old filesystem,
// finds it intact, mounts it, and the board comes back as the old enrolled
// device. So both disk writers clear the start and end of every partition
// wherever the image writes nothing. No filesystem, LUKS, RAID or partition
// table signature survives there, and first boot formats the partition as it
// would on a new disk.
//
// Clearing whole partitions would cost as much as writing them. The
// signatures blkid probes to name what a partition holds sit within a few MiB
// of its start or end, so only those windows are cleared. The CLI's
// disk-image writer uses the same windows, so both writers agree on what a
// reflash erases.
const (
	// StaleHeadBytes reaches past every start-of-partition signature blkid
	// probes. The deepest is LUKS2's secondary header, which sits at the end
	// of the header area and so anywhere from 16 KiB to 4 MiB in; blkid
	// still finds a container by it once the primary header is gone.
	StaleHeadBytes = 8 << 20
	// StaleTailBytes covers the end-of-partition signatures: md RAID 0.90 and
	// 1.0 superblocks, ZFS's trailing labels, and BIOS RAID metadata, whose
	// deepest (Promise FastTrack) sits about 1.5 MiB before the end.
	StaleTailBytes = 2 << 20
)

// zeroChunk is the largest single write ZeroExtents makes.
const zeroChunk = 1 << 20

// Extent is the byte range [Start, End) of a disk.
type Extent struct {
	Start, End int64
}

// StaleExtents returns what a flash must clear: the first StaleHeadBytes and
// the last StaleTailBytes of each partition, less every range in written,
// sorted and merged. Neither list needs to be sorted.
func StaleExtents(partitions, written []Extent) []Extent {
	var windows []Extent
	for _, p := range partitions {
		if p.End <= p.Start {
			continue
		}
		windows = append(windows,
			Extent{p.Start, min(p.End, p.Start+StaleHeadBytes)},
			Extent{max(p.Start, p.End-StaleTailBytes), p.End})
	}
	return subtractExtents(mergeExtents(windows), mergeExtents(written))
}

// ZeroExtents writes zeros over each extent. Raw disks accept only whole,
// aligned sectors, so an extent is shrunk to the sectors it fully covers:
// a partial sector at either end is left alone rather than rounded out over
// bytes the caller did not ask to clear.
func ZeroExtents(dev io.WriterAt, extents []Extent, sectorSize int64) error {
	zeros := make([]byte, zeroChunk)
	for _, e := range extents {
		start := (e.Start + sectorSize - 1) / sectorSize * sectorSize
		end := e.End / sectorSize * sectorSize
		for off := start; off < end; {
			n := min(int64(len(zeros)), end-off)
			if _, err := dev.WriteAt(zeros[:n], off); err != nil {
				return fmt.Errorf("clearing %d bytes at %d: %w", n, off, err)
			}
			off += n
		}
	}
	return nil
}

// mergeExtents sorts extents and joins those that overlap or touch, dropping
// empty ones.
func mergeExtents(in []Extent) []Extent {
	sorted := slices.Clone(in)
	slices.SortFunc(sorted, func(a, b Extent) int { return cmp.Compare(a.Start, b.Start) })
	var out []Extent
	for _, e := range sorted {
		if e.End <= e.Start {
			continue
		}
		if n := len(out); n > 0 && e.Start <= out[n-1].End {
			out[n-1].End = max(out[n-1].End, e.End)
			continue
		}
		out = append(out, e)
	}
	return out
}

// subtractExtents returns the parts of from that no extent in remove covers.
// Both must be sorted and merged.
func subtractExtents(from, remove []Extent) []Extent {
	var out []Extent
	i := 0
	for _, e := range from {
		start := e.Start
		for i < len(remove) && remove[i].End <= start {
			i++
		}
		for j := i; j < len(remove) && remove[j].Start < e.End; j++ {
			if remove[j].Start > start {
				out = append(out, Extent{start, remove[j].Start})
			}
			start = max(start, remove[j].End)
		}
		if start < e.End {
			out = append(out, Extent{start, e.End})
		}
	}
	return out
}
