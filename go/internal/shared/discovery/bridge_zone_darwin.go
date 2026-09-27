//go:build darwin

package discovery

import (
	"context"
	"os/exec"
)

// bridgeMaster returns the bridge iface is a member of, or "" if none. It
// shells out to ifconfig, so LinkLocalDialZone only calls it for interfaces
// that already lack an IPv6 link-local address.
func bridgeMaster(iface string) string {
	ctx, cancel := context.WithTimeout(context.Background(), usbDisplayNameTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ifconfig").Output()
	if err != nil {
		return ""
	}
	return parseBridgeMembers(string(out))[iface]
}
