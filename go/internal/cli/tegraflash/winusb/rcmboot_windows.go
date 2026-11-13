//go:build windows

package winusb

// Stage-1 RCM boot over WinUSB: send the pre-signed bootROM image chain + membct
// + blob to a Jetson in recovery mode so it boots the initrd-flash payload and
// re-enumerates as the ADB flashing gadget. This is the Windows equivalent of the
// gousb-based bringup package (macOS/Linux); the send sequence and framing match.

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/tegraflash/rcm"
)

// StageOneOptions controls a Windows stage-1 RCM boot.
type StageOneOptions struct {
	Stage1Dir       string   // dir holding the RCM-boot artifacts
	MemBCT          string   // membct filename (RAMCODE/2)
	Blob            string   // payload filename; empty means the legacy Thor blob.bin
	SendOrder       []string // bootROM image filenames in send order
	Location        string   // optional location path to pin the device
	Instance        string   // optional PnP instance ID pinning the exact devnode (wins over Location)
	ExpectedProduct uint16
	ExpectedECID    string
	Out             io.Writer
}

// bootROM image filenames (mirror of package bringup).
const (
	fileBctBR  = "br_bct_BR.bct"
	fileMB1    = "mb1_t264_prod_aligned_sigheader.bin.encrypt"
	filePSCBL1 = "psc_bl1_t264_prod_aligned_sigheader.bin.encrypt"
	fileBctMB1 = "mb1_bct_MB1_sigheader.bct.encrypt"
	fileBlob   = "blob.bin"
)

var defaultSendOrder = []string{fileBctBR, fileMB1, filePSCBL1, fileBctMB1}

// StageOneBoot performs the stage-1 RCM boot. On success the device has booted
// the blob and is re-enumerating as the initrd-flash ADB gadget (0955:7100).
func StageOneBoot(opts StageOneOptions) error {
	out := opts.Out
	if out == nil {
		out = os.Stdout
	}
	order := opts.SendOrder
	if len(order) == 0 {
		order = defaultSendOrder
	}

	// Load all images up front so a missing file fails before we touch the device.
	images := make([][]byte, 0, len(order))
	for _, name := range order {
		data, err := os.ReadFile(filepath.Join(opts.Stage1Dir, name))
		if err != nil {
			return fmt.Errorf("reading rcm-boot artifact %s: %w", name, err)
		}
		images = append(images, data)
	}
	bctMem, err := os.ReadFile(filepath.Join(opts.Stage1Dir, opts.MemBCT))
	if err != nil {
		return fmt.Errorf("reading membct %s: %w", opts.MemBCT, err)
	}
	blobName := opts.Blob
	if blobName == "" {
		blobName = fileBlob
	}
	blob, err := os.ReadFile(filepath.Join(opts.Stage1Dir, blobName))
	if err != nil {
		return fmt.Errorf("reading blob %s: %w", blobName, err)
	}

	fmt.Fprintln(out, "Opening Jetson in recovery mode over WinUSB…")
	var dev *USBDevice
	if opts.Instance != "" {
		dev, err = OpenInstance(opts.Instance, opts.ExpectedProduct)
	} else {
		dev, err = OpenExpected(opts.Location, opts.ExpectedProduct)
	}
	if err != nil {
		return err
	}
	defer dev.Close()
	if err := rcm.VerifyChipID(opts.ExpectedECID, dev.ReadChipID); err != nil {
		return err
	}
	// The bootROM often sends no status; bound the tolerant reads so they don't
	// block on the long default ADB timeout.
	dev.SetReadTimeout(responseReadTimeoutMs)
	fmt.Fprintf(out, "  OUT pipe: maxPacket=%d, maxTransfer=%d, chunk=%d\n",
		dev.OutMaxPacket(), dev.MaxTransferSize(), bulkChunk)

	// Send the bootROM image chain verbatim (no status read between images: the
	// bootROM ACKs only bct_mb1). WriteImage chunks each image and appends a
	// trailing ZLP for exact-multiple lengths (end-of-transfer framing).
	fmt.Fprintf(out, "Sending %d bootROM images…\n", len(order))
	for i, img := range images {
		if err := dev.WriteImage(img); err != nil {
			return fmt.Errorf("sending bootROM image %d (%s, %d bytes): %w", i, order[i], len(img), err)
		}
	}
	// Read the bct_mb1 response (mb1 version) — completes the bootROM handshake and
	// keeps mb1 alive for the bct_mem/blob downloads.
	if v := readResponse(out, dev); v != "" {
		fmt.Fprintf(out, "  mb1 up: %s\n", v)
	}

	fmt.Fprintf(out, "Sending bct_mem (%s, %d bytes)…\n", opts.MemBCT, len(bctMem))
	if err := dev.WriteImage(bctMem); err != nil {
		return fmt.Errorf("sending bct_mem: %w", err)
	}
	readResponse(out, dev)

	fmt.Fprintf(out, "Sending blob (%d bytes; mb2/UEFI/initrd)…\n", len(blob))
	t0 := time.Now()
	if err := dev.WriteImage(blob); err != nil {
		return fmt.Errorf("sending blob: %w", err)
	}
	fmt.Fprintf(out, "  blob sent in %v; mb1 is booting the payload.\n", time.Since(t0).Round(time.Millisecond))
	readResponse(out, dev)
	return nil
}

// readResponse does a tolerant bulk-IN read of any status the device returns.
func readResponse(out io.Writer, dev *USBDevice) string {
	buf := make([]byte, 512)
	n, err := dev.ReadBulk(buf)
	if err != nil || n == 0 {
		return ""
	}
	printable := make([]byte, 0, n)
	for _, c := range buf[:n] {
		if c == 0 {
			break
		}
		if c >= 0x20 && c < 0x7f {
			printable = append(printable, c)
		}
	}
	return string(printable)
}
