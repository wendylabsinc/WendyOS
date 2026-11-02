//go:build !linux

package usbthermal

// usbfs is Linux-only. The agent only ships for Linux; this lets the package
// (and the video service that uses it) build and test elsewhere.
func defaultOpen() OpenFunc {
	return func(string) (Device, error) { return nil, ErrUnsupported }
}
