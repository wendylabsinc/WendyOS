// Package bringup performs the T264 (Thor) stage-1 RCM boot from a host: it sends
// the bootROM image chain over USB Recovery Mode and then bct_mem + the blob, so
// mb1 boots the payload and the device comes up as the initrd-flash ADB gadget.
// All images are sent verbatim; reading the bct_mb1 response on the same handle is
// what keeps mb1 alive between downloads.
//
// The input files live in the flashpack's stage1/ dir, signed and generated offline
// by the builder (WendyOS-Builder/scripts/make-thor-flashpack.sh) — they are
// class-level for ODM-open devices, so one set per BSP serves every board:
//
//	br_bct_BR.bct                                    -> bct_br
//	mb1_t264_prod_aligned_sigheader.bin.encrypt      -> mb1
//	psc_bl1_t264_prod_aligned_sigheader.bin.encrypt  -> psc_bl1
//	mb1_bct_MB1_sigheader.bct.encrypt                -> bct_mb1
//	membct_<RAMCODE/2>_sigheader.bct.encrypt         -> bct_mem (membct_6 for RAMCODE 12)
//	blob.bin                                         -> the ~171 MB mb2/UEFI/initrd payload
package bringup

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/tegraflash/rcm"
)

// Artifact file names within the rcm-boot directory.
const (
	FileBctBR     = "br_bct_BR.bct"
	FileMB1       = "mb1_t264_prod_aligned_sigheader.bin.encrypt"
	FilePSCBL1    = "psc_bl1_t264_prod_aligned_sigheader.bin.encrypt"
	FileBctMB1    = "mb1_bct_MB1_sigheader.bct.encrypt"
	FileBlob      = "blob.bin"
	DefaultMemBCT = "membct_6_sigheader.bct.encrypt"
)

// Options controls a stage-1 RCM boot.
type Options struct {
	// Dir holds the rcm-boot artifacts (the rcmboot_blob directory, or any dir
	// containing the files named above).
	Dir string
	// MemBCT is the membct filename to use; empty means DefaultMemBCT. The correct
	// one is selected by the on-board RAMCODE (RAMCODE/2); membct_6 fits RAMCODE 12.
	MemBCT string
	// Blob is the manifest-provided mb2/UEFI/initrd payload filename. Empty uses
	// the legacy Thor filename.
	Blob string
	// DevicePath, when set, pins which Jetson in recovery mode to flash (a PathKey
	// from rcm.ListRecoveryDevices). Empty flashes the first device found.
	DevicePath string
	// ExpectedProduct pins the selected Jetson family across the re-open. This
	// prevents a different Jetson on the same host from satisfying the wait.
	ExpectedProduct uint16
	// ExpectedECID pins the chip on the claimed handle, before any boot upload.
	ExpectedECID string
	// SendOrder is the bootROM image filenames to send, in order. Empty uses the
	// built-in default (bct_br → mb1 → psc_bl1 → bct_mb1). Driving this from the
	// flashpack manifest lets a future BSP reorder the chain without a code change.
	SendOrder []string
	Out       io.Writer
}

// DefaultSendOrder is the bootROM download order when the manifest omits one.
var DefaultSendOrder = []string{FileBctBR, FileMB1, FilePSCBL1, FileBctMB1}

// Run executes the stage-1 RCM boot. On success the device has booted the blob and
// is re-enumerating as the initrd-flash ADB gadget.
func Run(opts Options) error {
	out := opts.Out
	if out == nil {
		out = os.Stdout
	}
	memBCT := opts.MemBCT
	if memBCT == "" {
		memBCT = DefaultMemBCT
	}

	order := opts.SendOrder
	if len(order) == 0 {
		order = DefaultSendOrder
	}
	images := make([][]byte, 0, len(order))
	for _, name := range order {
		data, err := read(opts.Dir, name)
		if err != nil {
			return err
		}
		images = append(images, data)
	}
	bctMem, err := read(opts.Dir, memBCT)
	if err != nil {
		return err
	}
	blobName := opts.Blob
	if blobName == "" {
		blobName = FileBlob
	}
	blob, err := read(opts.Dir, blobName)
	if err != nil {
		return err
	}

	fmt.Fprintln(out, "Waiting for Jetson in USB recovery mode...")
	dev, err := rcm.WaitForDeviceAt(opts.DevicePath, opts.ExpectedProduct)
	if err != nil {
		return fmt.Errorf("waiting for device: %w", err)
	}
	defer dev.Close()
	if err := rcm.VerifyChipID(opts.ExpectedECID, dev.ReadChipID); err != nil {
		return err
	}
	fmt.Fprintf(out, "  device: %s\n", dev.String())

	fmt.Fprintf(out, "Sending %d bootROM images (%s)...\n", len(order), strings.Join(order, ", "))
	if err := rcm.DownloadBootROMImages(dev, images); err != nil {
		return fmt.Errorf("bootROM image sequence: %w", err)
	}
	// Read the bct_mb1 response (the mb1 version) on the same handle. This
	// completes the bootROM handshake and is what keeps mb1 alive for bct_mem/blob.
	if v := readResponse(dev); v != "" {
		fmt.Fprintf(out, "  mb1 up: %s\n", v)
	}

	fmt.Fprintf(out, "Sending bct_mem (%s, %d bytes)...\n", memBCT, len(bctMem))
	if err := dev.Write(bctMem); err != nil {
		return fmt.Errorf("sending bct_mem: %w", err)
	}
	readResponse(dev)

	fmt.Fprintf(out, "Sending blob (%d bytes; mb2/UEFI/initrd)...\n", len(blob))
	t0 := time.Now()
	if err := dev.Write(blob); err != nil {
		return fmt.Errorf("sending blob: %w", err)
	}
	fmt.Fprintf(out, "  blob sent in %v; mb1 is booting the payload.\n", time.Since(t0).Round(time.Millisecond))
	readResponse(dev)
	return nil
}

// readResponse does a tolerant bulk-IN read of any status the device returns and
// returns it as a printable string (empty if none / not printable).
func readResponse(dev *rcm.Device) string {
	buf := make([]byte, 512)
	n, err := dev.ReadWithTimeout(buf, 2*time.Second)
	if err != nil || n == 0 {
		return ""
	}
	out := make([]byte, 0, n)
	for _, c := range buf[:n] {
		if c == 0 {
			break
		}
		if c >= 0x20 && c < 0x7f {
			out = append(out, c)
		}
	}
	return string(out)
}

func read(dir, name string) ([]byte, error) {
	p := filepath.Join(dir, name)
	data, err := os.ReadFile(p)
	if err != nil {
		return nil, fmt.Errorf("reading rcm-boot artifact %s: %w", name, err)
	}
	return data, nil
}
