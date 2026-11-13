//go:build !darwin && !windows

package discovery

// usbDisplayNameResolver has nothing to add on Linux and other unix-likes:
// Linux classifies by the USB identity in sysfs (see IsWendyUSBGadget), and
// elsewhere the interface name is all there is. A nil resolver means
// "classify without a display name".
func usbDisplayNameResolver() func(string) string { return nil }
