package localmesh

import "net"

// PhysicalLANInterface is an enabled Ethernet or infrastructure Wi-Fi link.
// It intentionally excludes app bridges and mesh carrier interfaces.
type PhysicalLANInterface struct {
	Name  string
	Index int
	IP    net.IP
	Net   *net.IPNet
	Cost  uint16
}
