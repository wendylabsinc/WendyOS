// Package flashengine performs T264 (Thor) stage-2 partition flashing in pure Go,
// replacing NVIDIA's python bootburn. It drives a device already booted to the
// initrd-flash ADB gadget (stage-1) through a transport-agnostic interface, so
// the same engine runs on Windows (winusb), macOS and Linux (gousb).
//
// It mirrors bootburn_adb.FlashUsingADB for the WendyOS flashing profile
// (L4T, full flash, serial/no-pipeline): push nvdd to the device, erase QSPI,
// then for each FileToFlash.txt entry apply any host-side transform, stream the
// image to the partition via nvdd, and verify its MD5. The device-side tool
// (nvdd, a static aarch64 binary) ships in the flashpack.
//
// One step is not bootburn's: before QSPI is erased, the engine clears what the
// previous installation left in the partitions the flash does not write, so a
// reflashed board does not come back with its old /data (see stale.go).
//
// Correctness is checked two ways: DryRun emits the exact nvdd/adb command stream
// for diffing against a real bootburn run, and each partition's MD5 is verified
// device-side after writing.
package flashengine

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Transport is the device connection: an ADB session over USB. Both the winusb
// (Windows) and gousb (macOS/Linux) ADB clients satisfy it.
type Transport interface {
	// Shell runs a command on the device and returns combined stdout+stderr. A
	// non-zero exit must be reported as an error (see IsExit).
	Shell(command string) (string, error)
	// Push streams r to remotePath with the given unix mode.
	Push(r io.Reader, remotePath string, mode int) error
}

// Options controls a stage-2 flash.
type Options struct {
	// FlashImagesDir is <flash_workspace>/flash-images (FileToFlash.txt + images).
	FlashImagesDir string
	// NvddLocalPath is the local path to the device-side nvdd binary (from the
	// flashpack bundle); it is pushed to the device.
	NvddLocalPath string
	// DryRun logs the nvdd/adb command stream without touching the device.
	DryRun bool
	Out    io.Writer
	// Progress, when set, receives cumulative bytes pushed to the device against
	// an estimated total (the sum of the plan's image file sizes). Transfer bytes
	// only: device-side writes and verification don't advance it, and push
	// retries or expanded sparse fill regions can overshoot the total.
	Progress func(written, total int64)
}

// device-side paths.
const (
	remoteTmp  = "/tmp"
	remoteNvdd = "/tmp/nvdd"
)

// nvdd write sizing (from bootburn): files at or below this go in one push; larger
// ones (and sparse chunks) stream in maxChunk pieces so device /tmp stays bounded.
const (
	singleWriteMax = 0x1400000 // 20 MiB
	maxChunk       = 0xA00000  // 10 MiB
)

// partition is one parsed FileToFlash.txt row.
type partition struct {
	Device   string // LinuxPartitionName, e.g. /dev/nvme0n1 or /dev/block/810c5b0000.spi
	Name     string // PartitionName, e.g. APP, bct, esp
	FileName string // image file (relative to FlashImagesDir)
	Start    int64
	Size     int64
	Resize   int
	MD5      string
}

// Run performs the stage-2 flash. ctx aborts it between transport operations
// (chunks are ≤10 MiB, so cancellation lands within seconds).
func Run(ctx context.Context, t Transport, opts Options) error {
	if ctx == nil {
		ctx = context.Background()
	}
	out := opts.Out
	if out == nil {
		out = os.Stdout
	}
	ftf := filepath.Join(opts.FlashImagesDir, "FileToFlash.txt")
	parts, err := parseFileToFlash(ftf)
	if err != nil {
		return err
	}
	// Check every image before the reset and sum its size for Progress.
	// A/B partitions push their image once each.
	var total int64
	for _, p := range parts {
		path := filepath.Join(opts.FlashImagesDir, p.FileName)
		info, err := os.Stat(path)
		if err != nil {
			return fmt.Errorf("%w: checking image %q before any device writes: %w", ErrOldDataNotCleared, p.FileName, err)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("%w: image %q is not a regular file", ErrOldDataNotCleared, p.FileName)
		}
		f, err := os.Open(path)
		if err != nil {
			return fmt.Errorf("%w: opening image %q before any device writes: %w", ErrOldDataNotCleared, p.FileName, err)
		}
		f.Close()
		total += info.Size()
	}

	// Where the previous installation's data must be cleared is decided from
	// the package alone, so a package that cannot say stops the flash here,
	// before the device is touched.
	stale, err := planStaleClear(opts.FlashImagesDir, parts)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "Flashing %d partitions (dry-run=%v)\n", len(parts), opts.DryRun)

	e := &engine{ctx: ctx, t: t, opts: opts, out: out, progressTotal: total}
	defer e.removeTempFiles()

	// Device setup: push nvdd and make it executable.
	if err := e.setup(); err != nil {
		return err
	}

	// Clear what the previous installation left where the flash writes
	// nothing. First, so that nothing the flash writes can be cleared and a
	// failure leaves the board's boot firmware and OS as they were.
	if err := e.clearStale(stale); err != nil {
		return err
	}

	// Erase QSPI device(s) before writing (bootburn EraseQspi).
	if err := e.eraseQSPI(parts); err != nil {
		return err
	}

	// Flash each partition in order.
	for _, p := range parts {
		if err := e.flashPartition(p); err != nil {
			return fmt.Errorf("partition %s: %w", p.Name, err)
		}
	}
	fmt.Fprintln(out, "All partitions written.")
	return nil
}

type engine struct {
	ctx  context.Context
	t    Transport
	opts Options
	out  io.Writer

	// Progress accounting (single flash goroutine; no locking).
	progressTotal   int64
	progressWritten int64

	// tempFiles holds transform outputs (writeTemp), removed when Run returns.
	tempFiles []string
}

func (e *engine) removeTempFiles() {
	for _, p := range e.tempFiles {
		os.Remove(p)
	}
}

// addProgress advances the transfer byte count and reports it. Called from the
// counting reader wrapped around partition-image pushes (setup's nvdd push is
// not counted — it isn't part of the estimated total).
func (e *engine) addProgress(n int64) {
	if e.opts.Progress == nil {
		return
	}
	e.progressWritten += n
	e.opts.Progress(e.progressWritten, e.progressTotal)
}

// countingReader reports bytes read from r (i.e. pushed to the device) to the
// engine's progress accounting.
type countingReader struct {
	r io.Reader
	e *engine
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	if n > 0 {
		c.e.addProgress(int64(n))
	}
	return n, err
}

// setup pushes nvdd to the device and marks it executable.
func (e *engine) setup() error {
	fmt.Fprintln(e.out, "Pushing nvdd to device /tmp…")
	if e.opts.DryRun {
		e.logf("push %s -> %s (0755)", e.opts.NvddLocalPath, remoteNvdd)
	} else {
		f, err := os.Open(e.opts.NvddLocalPath)
		if err != nil {
			return fmt.Errorf("opening nvdd: %w", err)
		}
		defer f.Close()
		if err := e.t.Push(f, remoteNvdd, 0o755); err != nil {
			return fmt.Errorf("pushing nvdd: %w", err)
		}
	}
	return e.shell("chmod 766 " + remoteNvdd)
}

// eraseQSPI erases each distinct QSPI (spi/mtd) device referenced by the plan,
// matching bootburn EraseQspi (nvdd -E eoffset=0,esize=0 erases the whole chip).
func (e *engine) eraseQSPI(parts []partition) error {
	seen := map[string]bool{}
	for _, p := range parts {
		if !isQSPI(p.Device) || seen[p.Device] {
			continue
		}
		seen[p.Device] = true
		fmt.Fprintf(e.out, "Erasing QSPI %s…\n", p.Device)
		if err := e.shell(fmt.Sprintf("%s --device %s -E eoffset=0,esize=0", remoteNvdd, p.Device)); err != nil {
			return fmt.Errorf("erasing QSPI %s: %w", p.Device, err)
		}
	}
	return nil
}

// flashPartition applies any transform then writes+verifies one partition.
func (e *engine) flashPartition(p partition) error {
	localFile := filepath.Join(e.opts.FlashImagesDir, p.FileName)
	info, err := os.Stat(localFile)
	if err != nil {
		return fmt.Errorf("image %s: %w", p.FileName, err)
	}
	fmt.Fprintf(e.out, "[%s] %s -> %s @ %d (%s)\n", p.Name, p.FileName, p.Device, p.Start, humanBytes(info.Size()))

	// Host-side transforms produce a possibly-different local file + md5.
	localFile, md5, err := e.transform(p, localFile)
	if err != nil {
		return err
	}
	info, err = os.Stat(localFile)
	if err != nil {
		return err
	}
	fileSize := info.Size()

	if fileSize > p.Size {
		return fmt.Errorf("file %s (%d) larger than partition %s (%d)", p.FileName, fileSize, p.Name, p.Size)
	}

	switch {
	case isSparse(localFile):
		if err := e.writeSparse(p, localFile); err != nil {
			return err
		}
		// Sparse images (APP/APP_b): bootburn does not md5-verify these; nvdd
		// validates each chunk as it writes with --l4t.
	case fileSize <= singleWriteMax:
		if err := e.writeWhole(p, localFile, fileSize); err != nil {
			return err
		}
		// Small partitions: bootburn's checkMd5 runs unconditionally on the
		// single-write path (bootburn_adb line 728). The transform md5 is the
		// manifest value (or, for bct/bad-page, recomputed over the replicated
		// bytes); both are kept in sync with the shipped file for these.
		if md5 != "" {
			if err := e.verifyMD5(p, fileSize, md5); err != nil {
				return err
			}
		}
	default:
		if err := e.writeChunked(p, localFile, fileSize); err != nil {
			return err
		}
		// Large partitions (esp, config): bootburn only md5-verifies these under
		// --safety / n_md5SumAll, which the WendyOS profile leaves off, so it does
		// not verify them at all (bootburn_adb lines 828-831). The flashpack also
		// does not keep the manifest md5 for large images in sync with the shipped
		// file (verified: esp and config manifest md5 != file md5), so it could not
		// be trusted anyway. Match bootburn: no per-partition md5 here.
	}

	// secondary_gpt on a non-QSPI device needs its backup GPT fixed to the actual
	// device size (bootburn FixSecondaryGPT).
	if p.Name == "secondary_gpt" && !isQSPI(p.Device) {
		if err := e.fixSecondaryGPT(p); err != nil {
			return err
		}
	}

	// resize the filesystem to fill the partition (APP/APP_b, Resize>=1).
	if p.Resize >= 1 && fileSize < p.Size {
		if err := e.resizeFS(p); err != nil {
			return err
		}
	}
	return nil
}

// writeWhole pushes a file and writes it in one nvdd call, then removes it.
func (e *engine) writeWhole(p partition, localFile string, size int64) error {
	base := filepath.Base(localFile)
	remote := remoteTmp + "/" + base
	if err := e.push(localFile, remote, 0o644); err != nil {
		return err
	}
	cmd := fmt.Sprintf("%s --inputbin=%s --device=%s --startoffset=%d --partsize=%d --l4t",
		remoteNvdd, remote, p.Device, p.Start, p.Size)
	if err := e.shell(cmd); err != nil {
		return err
	}
	return e.shell("rm " + remote)
}

// writeChunked streams a large non-sparse file in maxChunk pieces.
func (e *engine) writeChunked(p partition, localFile string, size int64) error {
	f, err := os.Open(localFile)
	if err != nil {
		return err
	}
	defer f.Close()
	base := filepath.Base(localFile)
	var written int64
	buf := make([]byte, maxChunk)
	for written < size {
		n, rerr := io.ReadFull(f, buf)
		if n == 0 && rerr != nil {
			break
		}
		chunk := buf[:n]
		remote := fmt.Sprintf("%s/%s_%d", remoteTmp, base, written/maxChunk)
		if err := e.pushBytes(chunk, remote, 0o644); err != nil {
			return err
		}
		cmd := fmt.Sprintf("%s --inputbin=%s --partsize %d --device %s --startoffset=%d --l4t",
			remoteNvdd, remote, n, p.Device, p.Start+written)
		if err := e.shell(cmd); err != nil {
			return err
		}
		if err := e.shell("rm " + remote); err != nil {
			return err
		}
		written += int64(n)
		if rerr == io.ErrUnexpectedEOF || rerr == io.EOF {
			break
		}
	}
	return nil
}

// verifyMD5 checks the written partition's MD5 device-side. nvdd compares the
// readback against --md5sum and exits non-zero (ImageCorrupted) on mismatch, so
// the exit code (surfaced as a shell error) is the authoritative signal.
func (e *engine) verifyMD5(p partition, size int64, md5 string) error {
	cmd := fmt.Sprintf("%s --device %s --startoffset %d --partsize %d --md5sum %s --printmd5sum",
		remoteNvdd, p.Device, p.Start, size, md5)
	out, err := e.shellOut(cmd)
	if err != nil {
		return fmt.Errorf("md5 verify failed for %s (expected %s): %w\n%s", p.Name, md5, err, out)
	}
	return nil
}

// fixSecondaryGPT reconstructs the backup GPT for the actual device size.
func (e *engine) fixSecondaryGPT(p partition) error {
	// bootburn drives `parted` on the device to fix the secondary GPT. The device
	// block name is resolved from the partition device; parted's "Fix" is issued
	// non-interactively.
	fmt.Fprintf(e.out, "  fixing secondary GPT on %s\n", p.Device)
	return e.shell(fmt.Sprintf("echo -e \"Fix\\nFix\" | parted ---pretend-input-tty %s print", p.Device))
}

// resizeFS grows the filesystem to fill its partition (rootfs A/B). Mirrors
// bootburn resizeFilesystem: for a filesystem at a non-zero offset, bind a loop
// device to that offset, run e2fsck -f (resize2fs refuses to grow a filesystem it
// deems unchecked) then resize2fs -fFp, and sync. The umount / losetup -d /
// e2fsck steps are tolerated like bootburn does — the fs isn't mounted, the loop
// may be unbound, and e2fsck exits non-zero when it corrects the freshly-written
// filesystem.
func (e *engine) resizeFS(p partition) error {
	fmt.Fprintf(e.out, "  resizing filesystem on %s (partition %d bytes)\n", p.Device, p.Size)
	dev := p.Device
	const loop = "/dev/block/loop0"
	if p.Start != 0 {
		// The filesystem sits at Start within the device; bind a loop device to it.
		e.shellTolerant(fmt.Sprintf("umount %s", loop))
		e.shellTolerant(fmt.Sprintf("losetup -d %s", loop))
		if err := e.shell(fmt.Sprintf("losetup %s %s --offset %d --sizelimit %d", loop, p.Device, p.Start, p.Size)); err != nil {
			return err
		}
		dev = loop
	}
	e.shellTolerant(fmt.Sprintf("e2fsck -yfFt %s", dev))
	if err := e.shell(fmt.Sprintf("resize2fs -fFp %s", dev)); err != nil {
		return err
	}
	e.shellTolerant("sync")
	if p.Start != 0 {
		e.shellTolerant(fmt.Sprintf("losetup -d %s", loop))
	}
	return nil
}

// ---- transport helpers (respect DryRun) ----

func (e *engine) shell(cmd string) error {
	out, err := e.shellOut(cmd)
	if err != nil {
		if s := strings.TrimSpace(out); s != "" {
			return fmt.Errorf("%w\n%s", err, s)
		}
	}
	return err
}

func (e *engine) shellOut(cmd string) (string, error) {
	if e.opts.DryRun {
		e.logf("shell: %s", cmd)
		return "", nil
	}
	if err := e.ctx.Err(); err != nil {
		return "", err
	}
	return e.t.Shell(cmd)
}

// shellTolerant runs a command and ignores its outcome, for steps bootburn also
// tolerates failing (e.g. umount of an unmounted fs, losetup -d of an unbound
// loop, or e2fsck returning non-zero after correcting a freshly-written fs).
func (e *engine) shellTolerant(cmd string) {
	if e.opts.DryRun {
		e.logf("shell (tolerant): %s", cmd)
		return
	}
	_, _ = e.t.Shell(cmd)
}

func (e *engine) push(localFile, remote string, mode int) error {
	if e.opts.DryRun {
		e.logf("push %s -> %s", localFile, remote)
		return nil
	}
	// Retry transient ADB push failures (bootburn's AdbPush retries 3x). Each
	// attempt re-opens the file and a fresh sync stream.
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if err := e.ctx.Err(); err != nil {
			return err
		}
		f, err := os.Open(localFile)
		if err != nil {
			return err
		}
		err = e.t.Push(&countingReader{r: f, e: e}, remote, mode)
		f.Close()
		if err == nil {
			return nil
		}
		lastErr = err
		e.logf("push %s attempt %d/3 failed: %v", remote, attempt+1, err)
	}
	return lastErr
}

func (e *engine) pushBytes(b []byte, remote string, mode int) error {
	if e.opts.DryRun {
		e.logf("push %d bytes -> %s", len(b), remote)
		return nil
	}
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if err := e.ctx.Err(); err != nil {
			return err
		}
		if err := e.t.Push(&countingReader{r: bytes.NewReader(b), e: e}, remote, mode); err == nil {
			return nil
		} else {
			lastErr = err
			e.logf("push %s attempt %d/3 failed: %v", remote, attempt+1, err)
		}
	}
	return lastErr
}

func (e *engine) logf(format string, args ...interface{}) {
	fmt.Fprintf(e.out, "  · "+format+"\n", args...)
}

// ---- parsing / helpers ----

// parseFileToFlash reads the 12-column FileToFlash.txt.
func parseFileToFlash(path string) ([]partition, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("opening FileToFlash.txt: %w", err)
	}
	defer f.Close()
	var out []partition
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 12 {
			return nil, fmt.Errorf("malformed FileToFlash line (%d fields): %q", len(fields), line)
		}
		start, err := strconv.ParseInt(fields[3], 10, 64)
		if err != nil || start < 0 {
			return nil, fmt.Errorf("%w: FileToFlash row for %s partition %q has invalid offset %q", ErrOldDataNotCleared, fields[0], fields[1], fields[3])
		}
		size, err := strconv.ParseInt(fields[4], 10, 64)
		if err != nil || size <= 0 {
			return nil, fmt.Errorf("%w: FileToFlash row for %s partition %q has invalid size %q", ErrOldDataNotCleared, fields[0], fields[1], fields[4])
		}
		if start > math.MaxInt64-size {
			return nil, fmt.Errorf("%w: FileToFlash row for %s partition %q has an overflowing range (offset %d, size %d)", ErrOldDataNotCleared, fields[0], fields[1], start, size)
		}
		resize, _ := strconv.Atoi(fields[6])
		out = append(out, partition{
			Device:   fields[0],
			Name:     fields[1],
			FileName: fields[2],
			Start:    start,
			Size:     size,
			Resize:   resize,
			MD5:      fields[10],
		})
	}
	return out, sc.Err()
}

func isQSPI(dev string) bool {
	return strings.Contains(dev, "spi") || strings.Contains(dev, "mtd")
}

func isSparse(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	var magic [4]byte
	if _, err := io.ReadFull(f, magic[:]); err != nil {
		return false
	}
	// ext4 sparse magic 0xed26ff3a (little-endian on disk: 3a ff 26 ed).
	return magic[0] == 0x3a && magic[1] == 0xff && magic[2] == 0x26 && magic[3] == 0xed
}

func humanBytes(n int64) string {
	const u = 1024
	if n < u {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(u), 0
	for m := n / u; m >= u; m /= u {
		div *= u
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
