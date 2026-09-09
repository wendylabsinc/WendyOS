// Package qdl speaks Qualcomm's Sahara and Firehose protocols to a device in
// Emergency Download (EDL) mode, so flashing needs no external qdl binary.
package qdl

import (
	"encoding/xml"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/wendylabsinc/wendy/go/internal/cli/archive"
)

// diskPatchTarget marks a patch the device applies to its own storage. Entries
// naming a GPT file are for host-side tools that rewrite the image first; we
// program those unpatched and let the DISK entries fix up the copy on disk.
const diskPatchTarget = "DISK"

// ProgramEntry is one <program> element of a rawprogram XML: a file to write at
// a sector offset within a physical partition.
type ProgramEntry struct {
	SectorSize uint32 `xml:"SECTOR_SIZE_IN_BYTES,attr"`
	NumSectors uint32 `xml:"num_partition_sectors,attr"`
	Partition  uint32 `xml:"physical_partition_number,attr"`
	FileOffset uint32 `xml:"file_sector_offset,attr"`
	Sparse     bool   `xml:"sparse,attr"`
	Filename   string `xml:"filename,attr"`
	Label      string `xml:"label,attr"`
	// SizeKB is the descriptor's own statement of the payload size — the only
	// per-payload integrity signal the bundle carries, so a mismatch means a
	// truncated or wrong file.
	SizeKB float64 `xml:"size_in_KB,attr"`
	// StartSector stays a string because the descriptor may express it
	// relative to the disk size (e.g. "NUM_DISK_SECTORS-5."); the programmer
	// on the device evaluates it, so we must not parse or rewrite it.
	StartSector string `xml:"start_sector,attr"`
}

// PatchEntry is one <patch> element: an in-place edit of a few bytes, used to
// stretch the GPT to the real disk size.
type PatchEntry struct {
	SectorSize  uint32 `xml:"SECTOR_SIZE_IN_BYTES,attr"`
	ByteOffset  uint32 `xml:"byte_offset,attr"`
	Partition   uint32 `xml:"physical_partition_number,attr"`
	SizeInBytes uint32 `xml:"size_in_bytes,attr"`
	Filename    string `xml:"filename,attr"`
	What        string `xml:"what,attr"`
	// StartSector and Value are strings for the same reason as above: they
	// carry expressions such as "NUM_DISK_SECTORS-6." and "CRC32(2,4096)"
	// that only the device can resolve.
	StartSector string `xml:"start_sector,attr"`
	Value       string `xml:"value,attr"`
}

// FlashPlan is the ordered work a flash performs: program every payload, then
// patch the GPT in place.
type FlashPlan struct {
	Dir      string
	Programs []ProgramEntry
	Patches  []PatchEntry
}

// ParseRawProgram reads a rawprogram XML descriptor.
//
// Entries with an empty filename are dropped, not flashed: that is how the
// descriptor marks a partition to leave alone (on WendyOS, `config` and `data`,
// which is why device identity and Wi-Fi credentials survive a reflash).
func ParseRawProgram(r io.Reader) ([]ProgramEntry, error) {
	var doc struct {
		Programs []ProgramEntry `xml:"program"`
	}
	if err := xml.NewDecoder(r).Decode(&doc); err != nil {
		return nil, fmt.Errorf("parsing rawprogram xml: %w", err)
	}
	out := make([]ProgramEntry, 0, len(doc.Programs))
	for _, p := range doc.Programs {
		if p.Filename == "" {
			continue
		}
		// Bounded once, here: the sector size multiplies descriptor-supplied
		// offsets downstream, so an implausible one must never get through.
		if p.SectorSize < 512 || p.SectorSize > 64*1024 || p.SectorSize&(p.SectorSize-1) != 0 {
			return nil, fmt.Errorf("program entry %q has an implausible sector size of %d bytes",
				p.Label, p.SectorSize)
		}
		if p.StartSector == "" {
			return nil, fmt.Errorf("program entry %q has no start_sector", p.Label)
		}
		if p.Sparse {
			return nil, fmt.Errorf("program entry %q is sparse, which is not supported", p.Label)
		}
		out = append(out, p)
	}
	return out, nil
}

// ParsePatches reads a patch XML descriptor, keeping only the entries the
// device applies to its own storage.
func ParsePatches(r io.Reader) ([]PatchEntry, error) {
	var doc struct {
		Patches []PatchEntry `xml:"patch"`
	}
	if err := xml.NewDecoder(r).Decode(&doc); err != nil {
		return nil, fmt.Errorf("parsing patch xml: %w", err)
	}
	out := make([]PatchEntry, 0, len(doc.Patches))
	for _, p := range doc.Patches {
		if p.Filename != diskPatchTarget {
			continue
		}
		if p.SectorSize == 0 {
			return nil, fmt.Errorf("patch %q has no SECTOR_SIZE_IN_BYTES", p.What)
		}
		out = append(out, p)
	}
	return out, nil
}

// LoadFlashPlan resolves the descriptors in an extracted bundle directory.
func LoadFlashPlan(dir string) (*FlashPlan, error) {
	programs, err := parseFile(dir, "rawprogram0.xml", ParseRawProgram)
	if err != nil {
		return nil, err
	}
	patches, err := parseFile(dir, "patch0.xml", ParsePatches)
	if err != nil {
		return nil, err
	}
	plan := &FlashPlan{Dir: dir, Programs: programs, Patches: patches}
	if len(plan.Programs) == 0 {
		return nil, fmt.Errorf("%s lists no partitions to flash", filepath.Join(dir, "rawprogram0.xml"))
	}
	for _, p := range plan.Programs {
		if err := checkPayload(dir, p); err != nil {
			return nil, err
		}
	}
	// Without the patch pass the GPT keeps the descriptor's placeholder size
	// and the last partition never spans the disk — a silently truncated
	// /data rather than a visible failure.
	if len(plan.Patches) == 0 {
		return nil, fmt.Errorf("%s applies no patches to the disk", filepath.Join(dir, "patch0.xml"))
	}
	return plan, nil
}

func parseFile[T any](dir, name string, parse func(io.Reader) ([]T, error)) ([]T, error) {
	f, err := os.Open(filepath.Join(dir, name))
	if err != nil {
		return nil, fmt.Errorf("opening flash descriptor: %w", err)
	}
	defer f.Close() //nolint:errcheck // read-only
	return parse(f)
}

// checkPayload rejects a descriptor entry whose payload escapes the bundle,
// is missing, or does not fit the partition it names.
//
// The size check belongs here rather than mid-flash: the GPT entries come last,
// so a mismatch discovered while programming aborts after the whole rootfs has
// already been written.
func checkPayload(dir string, e ProgramEntry) error {
	path, err := archive.SafeJoin(dir, e.Filename)
	if err != nil || path == "" {
		return fmt.Errorf("flash descriptor names an unsafe payload path %q", e.Filename)
	}
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("payload %q named by the flash descriptor is missing: %w", e.Filename, err)
	}
	// SectorsFor only rejects a payload too large for its partition. Without
	// this a truncated one would be programmed as a handful of sectors and the
	// flash would report success on an unbootable board.
	if want := int64(e.SizeKB * 1024); want > 0 && info.Size() != want {
		return fmt.Errorf("payload %q is %d bytes but the descriptor declares %d",
			e.Filename, info.Size(), want)
	}
	_, err = SectorsFor(e, info.Size())
	return err
}

// ResolveBundleDir finds the directory holding the flash descriptors. A
// published bundle wraps them in one top-level directory; a hand-unpacked one
// may not, so accept either shape.
func ResolveBundleDir(root string) (string, error) {
	const descriptor = "rawprogram0.xml"
	if _, err := os.Stat(filepath.Join(root, descriptor)); err == nil {
		return root, nil
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return "", fmt.Errorf("reading bundle: %w", err)
	}
	var found []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, err := os.Stat(filepath.Join(root, e.Name(), descriptor)); err == nil {
			found = append(found, e.Name())
		}
	}
	switch len(found) {
	case 1:
		return filepath.Join(root, found[0]), nil
	case 0:
		return "", fmt.Errorf("no %s found in the flash bundle", descriptor)
	default:
		// Guessing risks flashing the wrong storage: the bundle also
		// carries a sail_nor/ variant for the companion SPI-NOR part.
		return "", fmt.Errorf("flash bundle has %d candidate directories (%v); cannot tell which to flash",
			len(found), found)
	}
}
