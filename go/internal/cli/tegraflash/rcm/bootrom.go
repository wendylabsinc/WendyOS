//go:build (darwin || linux) && !ios

package rcm

import "fmt"

// DownloadBootROMImages sends each pre-signed RCM blob verbatim over bulk OUT, in
// the order given (the T264 sequence is bct_br → mb1 → psc_bl1 → bct_mb1). No status
// word is read between images: the bootROM ACKs none of them except bct_mb1, so a
// per-image read would just time out — and a timed-out bulk-IN read is destructive
// on macOS (libusb aborts the endpoint, libusb #1110). The blobs are sent raw, with
// no RCM40/DL_MINILOADER envelope; wrapping them makes the bootROM reset the device.
func DownloadBootROMImages(dev *Device, images [][]byte) error {
	for i, img := range images {
		if err := dev.Write(img); err != nil {
			return fmt.Errorf("sending bootROM image %d (%d bytes): %w", i, len(img), err)
		}
	}
	return nil
}
