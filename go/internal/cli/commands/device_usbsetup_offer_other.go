//go:build !linux

package commands

import "context"

// maybeOfferUSBSetup is Linux-only; on other platforms the USB-C gadget link is
// configured by the OS (macOS) or manually (Windows), so there's nothing to offer.
func maybeOfferUSBSetup(_ context.Context) error { return nil }

// pendingUSBSetupIface is always "": only Linux hosts need USB-C setup.
var pendingUSBSetupIface = func() string { return "" }
