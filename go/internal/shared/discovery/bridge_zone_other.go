//go:build !darwin

package discovery

// bridgeMaster is darwin-only for now: macOS Internet Sharing is the setup
// known to bridge a WendyOS USB gadget link.
func bridgeMaster(string) string { return "" }
