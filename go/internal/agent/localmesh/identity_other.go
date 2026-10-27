//go:build !linux

package localmesh

import "net"

// No permanent-address query is available here; use the persisted random ID.
func persistentMAC(string) (net.HardwareAddr, bool) { return nil, false }
