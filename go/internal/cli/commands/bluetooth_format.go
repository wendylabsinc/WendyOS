package commands

import (
	"fmt"

	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

// formatLinkTimeout renders a peripheral's BLE link supervision timeout for
// the list table: "0.5 s" at the agent's value, "3.0 s (device)" when the
// peripheral's own request is the one in force, blank when the agent
// reports none (not connected, Classic, or an older agent).
func formatLinkTimeout(d *agentpb.DiscoveredBluetoothPeripheral) string {
	if d.SupervisionTimeoutMs == nil {
		return ""
	}
	ms := d.GetSupervisionTimeoutMs()
	s := fmt.Sprintf("%.1f s", float64(ms)/1000)
	if ms%100 != 0 {
		s = fmt.Sprintf("%.2f s", float64(ms)/1000)
	}
	if req := d.GetRequestedSupervisionTimeoutMs(); req != 0 && ms >= req {
		s += " (device)"
	}
	return s
}
