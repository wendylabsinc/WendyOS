//go:build !linux

package discovery

// usbGadgetIdentityFn is nil off Linux: there is no sysfs to read the USB
// identity from, so looksLikeUSBConnection uses the interface and display names.
var usbGadgetIdentityFn func(string) bool
