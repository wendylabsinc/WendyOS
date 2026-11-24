package flashengine

import (
	"crypto/md5"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/wendylabsinc/wendy/go/internal/cli/tegraflash/t234"
)

// A Thor flash writes only the partitions FileToFlash.txt has a row for. The
// layout's GPT has more: `data`, UDA, the recovery and reserved partitions
// and the alternate ESP have no image, so nothing is written to them and they
// keep what the previous installation left. WendyOS puts `data` at the same
// offset on every release and grows it to the end of the disk on first boot,
// so the new OS would find the old filesystem intact and mount it, and the
// board would come back as the old enrolled device with its old apps.
//
// So the flash clears the start and end of every partition it has no row
// for. The rule and the windows are the Orin writer's (t234.StaleExtents):
// they hold every signature that makes first boot reuse a partition, and
// first boot then finds `data` blank and formats it. The old filesystem's
// backup superblocks lie between the windows and are left, as on Orin: blkid
// and mount read only the primary superblock, which is in the first window,
// and first boot runs e2fsck, which could fall back to a backup, only on a
// partition where blkid found a filesystem.
//
// Where the partitions are comes from the package: the primary GPT image,
// which is the table the flash writes at LBA 1 and verifies there. It is read
// before the device is touched, so a package whose layout cannot be read
// stops the flash with nothing written.
//
// The device adds one number, its size, because `data` does not end where the
// layout says. The layout is built for a disk of one size and gives `data`
// 512 MiB; first boot grows it to the end of the disk the board really has
// and looks for a filesystem only then. The previous installation did the
// same, so its /data ended at the end of the disk too. The end of the last
// partition is therefore cleared in both places: where the layout puts it,
// which a probe before the grow sees, and below the backup GPT at the real
// end of the disk, which a probe after the grow sees. The backup GPT itself
// is never cleared: parted rewrites it there once the table is on the disk
// (fixSecondaryGPT).
//
// The clearing comes first, before QSPI is erased and before any partition is
// written. It can then never overwrite something the flash wrote; a failure
// stops the flash while the board still has its boot firmware and its OS; and
// the new OS is never in place over the old data.

// ErrOldDataNotCleared marks a flash that stopped because it could not clear
// what the previous installation left behind. The flash stops before it
// erases QSPI or writes a partition, so the board's boot firmware and OS are
// unchanged.
var ErrOldDataNotCleared = errors.New("could not clear the previous installation's data")

// primaryGPT is the FileToFlash.txt row that writes a disk's partition table.
const primaryGPT = "primary_gpt"

// staleTarget is one disk of the flash plan: its layout, and the ranges the
// flash writes, which the clearing leaves alone.
type staleTarget struct {
	device  string
	layout  gptLayout
	written []t234.Extent
}

// planStaleClear works out, from the package alone, where each disk of the
// plan must be cleared. QSPI is left out: the flash erases it whole. A disk
// without a partition table in the package is an error, not a disk with
// nothing to clear, so a package this cannot read never flashes with the old
// data left in place.
func planStaleClear(imagesDir string, parts []partition) ([]staleTarget, error) {
	const stopped = "%w, so the flash stopped before anything was written: "
	var targets []staleTarget
	index := map[string]int{}
	found := map[string]bool{}
	for _, p := range parts {
		if isQSPI(p.Device) {
			continue
		}
		i, ok := index[p.Device]
		if !ok {
			i = len(targets)
			index[p.Device] = i
			targets = append(targets, staleTarget{device: p.Device})
		}
		// A row's whole range is the flash's: resize2fs grows a filesystem
		// to the end of its partition after the image is written.
		targets[i].written = append(targets[i].written, t234.Extent{Start: p.Start, End: p.Start + p.Size})
		if p.Name != primaryGPT {
			continue
		}
		data, err := readGPTImage(filepath.Join(imagesDir, p.FileName))
		if err != nil {
			return nil, fmt.Errorf(stopped+"reading the flash package's partition table: %w", ErrOldDataNotCleared, err)
		}
		layout, err := parseGPTImage(data, p.Start)
		if err != nil {
			return nil, fmt.Errorf(stopped+"the flash package's partition table %s cannot be read: %w", ErrOldDataNotCleared, p.FileName, err)
		}
		targets[i].layout, found[p.Device] = layout, true
	}
	for _, t := range targets {
		if !found[t.device] {
			return nil, fmt.Errorf(stopped+"the flash package has no partition table (%s) for %s", ErrOldDataNotCleared, primaryGPT, t.device)
		}
	}
	return targets, nil
}

// readGPTImage reads a primary GPT image. A header and its entries take
// 16.5 KiB; gptImageMax is far more than any table needs and keeps a
// mislabelled row from being read whole.
func readGPTImage(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, gptImageMax))
}

const gptImageMax = 1 << 20

// extents returns what to clear on a disk of diskSize bytes, as whole
// sectors: the start and end of each partition, less what the flash writes.
// Nothing at or past the backup GPT is returned. A diskSize of 0 is a disk
// whose size is not known (a dry run); the end of the disk is then left out.
func (t staleTarget) extents(diskSize int64) []t234.Extent {
	sector := t.layout.sectorSize
	limit := int64(math.MaxInt64)
	if diskSize > 0 {
		limit = diskSize/sector*sector - t.layout.backupBytes
	}
	var parts []t234.Extent
	var last gptPartition
	for _, p := range t.layout.partitions {
		x := t234.Extent{Start: p.start, End: min(p.end, limit)}
		if x.End <= x.Start {
			continue
		}
		parts = append(parts, x)
		if p.end > last.end {
			last = p
		}
	}
	// First boot grows the final data partition to the backup GPT.
	if diskSize > 0 && last.name == "data" && last.end < limit {
		parts = append(parts, t234.Extent{Start: last.start, End: limit})
	}
	var out []t234.Extent
	for _, x := range t234.StaleExtents(parts, t.written) {
		x.Start = (x.Start + sector - 1) / sector * sector
		x.End = x.End / sector * sector
		if x.Start < x.End {
			out = append(out, x)
		}
	}
	return out
}

// clearStale clears what the previous installation left on each disk of the
// plan. Any failure is ErrOldDataNotCleared.
func (e *engine) clearStale(targets []staleTarget) error {
	for _, t := range targets {
		if err := e.clearStaleOn(t); err != nil {
			return fmt.Errorf("%w on %s, so the flash stopped before the boot firmware or OS was changed: %w", ErrOldDataNotCleared, t.device, err)
		}
	}
	return nil
}

func (e *engine) clearStaleOn(t staleTarget) error {
	fmt.Fprintf(e.out, "Clearing the previous installation's data on %s…\n", t.device)
	size, err := e.deviceSize(t.device)
	if err != nil {
		return err
	}
	if size == 0 {
		e.logf("a dry run does not know the size of %s; a flash can also clear the grown data partition's tail", t.device)
	} else {
		fmt.Fprintf(e.out, "  %s is %d bytes\n", t.device, size)
	}
	extents := t.extents(size)
	for _, x := range extents {
		e.progressTotal += x.End - x.Start
	}
	for _, x := range extents {
		fmt.Fprintf(e.out, "  clearing %s at %d\n", humanBytes(x.End-x.Start), x.Start)
		if err := e.clearExtent(t.device, x); err != nil {
			return fmt.Errorf("clearing %d bytes at %d: %w", x.End-x.Start, x.Start, err)
		}
	}
	return nil
}

// deviceSize asks the device for the size of dev in bytes. blockdev is the one
// device-side tool the clearing adds to those the flash already runs: it
// ships in the flashing initrd (util-linux, like blkdiscard and losetup), and
// NVIDIA's own flasher calls it there. A dry run cannot ask and gets 0.
func (e *engine) deviceSize(dev string) (int64, error) {
	out, err := e.shellOut("blockdev --getsize64 " + dev)
	if e.opts.DryRun {
		return 0, nil
	}
	answer := strings.TrimSpace(out)
	if err != nil {
		if answer != "" {
			return 0, fmt.Errorf("reading the disk's size: %w\n%s", err, answer)
		}
		return 0, fmt.Errorf("reading the disk's size: %w", err)
	}
	size, err := strconv.ParseInt(answer, 10, 64)
	if err != nil || size <= 0 {
		return 0, fmt.Errorf("reading the disk's size: the device answered %q", answer)
	}
	return size, nil
}

// clearExtent overwrites [x.Start, x.End) of dev with zeros, then has the
// device check that the range reads back as zeros. The zeros are pushed and
// written through nvdd, and checked by its md5 read-back, exactly as the
// flash writes and checks its images; nvdd is a closed tool, so the check is
// what shows the zeros reached the disk.
func (e *engine) clearExtent(dev string, x t234.Extent) error {
	if err := e.streamFill(partition{Device: dev}, "stale", x.Start, x.End-x.Start, [4]byte{}); err != nil {
		return err
	}
	for off := x.Start; off < x.End; {
		n := min(int64(maxChunk), x.End-off)
		sum := md5.Sum(make([]byte, n))
		p := partition{Device: dev, Name: fmt.Sprintf("the cleared range at %d", off), Start: off}
		if err := e.verifyMD5(p, n, hex.EncodeToString(sum[:])); err != nil {
			return err
		}
		off += n
	}
	return nil
}
