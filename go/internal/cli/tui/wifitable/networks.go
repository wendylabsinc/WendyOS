// Package wifitable implements the interactive `wendy device wifi` table.
package wifitable

import (
	"sort"

	agentpb "github.com/wendylabsinc/wendy/proto/gen/agentpb"
)

// Network is a CLI-side view of a WiFi network merged across scan results and
// saved profiles. Fields are plain values (plus the WiFiSecurityType enum,
// which we reuse from the generated protobuf package so we don't duplicate the
// enum definition) so the TUI and sorting logic can be unit-tested without a
// gRPC transport.
type Network struct {
	SSID      string
	Known     bool
	Connected bool
	Security  agentpb.WiFiSecurityType
	// Signal is the 0-100 percent signal reported by the scanner, or 0 if
	// the network is known but not currently visible.
	Signal   int32
	Priority int32
}

// FromProto converts a ListWiFiNetworksResponse network into the local Network.
func FromProto(n *agentpb.ListWiFiNetworksResponse_WiFiNetwork) Network {
	out := Network{
		SSID:      n.GetSsid(),
		Known:     n.GetIsKnown(),
		Connected: n.GetIsConnected(),
		Security:  n.GetSecurity(),
	}
	if n.SignalStrength != nil {
		out.Signal = *n.SignalStrength
	}
	if n.Priority != nil {
		out.Priority = *n.Priority
	}
	return out
}

// Sort sorts the networks in-place: known networks first (by descending
// priority; ties broken by descending signal), then unknown networks by
// descending signal, then by SSID as a final tie-break. The connected network
// is always pulled to the very top so it is obvious at a glance.
func Sort(networks []Network) {
	sort.SliceStable(networks, func(i, j int) bool {
		a, b := networks[i], networks[j]
		if a.Connected != b.Connected {
			return a.Connected
		}
		if a.Known != b.Known {
			return a.Known
		}
		if a.Known {
			if a.Priority != b.Priority {
				return a.Priority > b.Priority
			}
		}
		if a.Signal != b.Signal {
			return a.Signal > b.Signal
		}
		return a.SSID < b.SSID
	})
}

// SecurityLabel returns a short, human-readable label for a WiFiSecurityType.
func SecurityLabel(t agentpb.WiFiSecurityType) string {
	switch t {
	case agentpb.WiFiSecurityType_WIFI_SECURITY_TYPE_OPEN:
		return "Open"
	case agentpb.WiFiSecurityType_WIFI_SECURITY_TYPE_WEP:
		return "WEP"
	case agentpb.WiFiSecurityType_WIFI_SECURITY_TYPE_WPA_PSK:
		return "WPA"
	case agentpb.WiFiSecurityType_WIFI_SECURITY_TYPE_WPA2_PSK:
		return "WPA2"
	case agentpb.WiFiSecurityType_WIFI_SECURITY_TYPE_WPA3_SAE:
		return "WPA3"
	case agentpb.WiFiSecurityType_WIFI_SECURITY_TYPE_WPA2_ENTERPRISE:
		return "WPA2-Ent"
	default:
		return ""
	}
}

// IsSecured returns true if the security type requires a password.
func IsSecured(t agentpb.WiFiSecurityType) bool {
	switch t {
	case agentpb.WiFiSecurityType_WIFI_SECURITY_TYPE_OPEN,
		agentpb.WiFiSecurityType_WIFI_SECURITY_TYPE_UNSPECIFIED:
		return false
	}
	return true
}

// KnownSSIDsInOrder returns the SSIDs of known networks in the slice's current
// order. Useful for building a ReorderKnownWiFiNetworksRequest after a rank
// commit.
func KnownSSIDsInOrder(networks []Network) []string {
	var out []string
	for _, n := range networks {
		if n.Known {
			out = append(out, n.SSID)
		}
	}
	return out
}

// MoveUp moves the network at idx one position toward the top, staying within
// the block of known networks. Returns the new cursor position. If the move is
// not possible (idx at top of known block, or idx is not a known network),
// returns idx unchanged.
func MoveUp(networks []Network, idx int) int {
	if idx <= 0 || idx >= len(networks) || !networks[idx].Known {
		return idx
	}
	if !networks[idx-1].Known {
		return idx
	}
	networks[idx], networks[idx-1] = networks[idx-1], networks[idx]
	return idx - 1
}

// MoveDown is the mirror of MoveUp.
func MoveDown(networks []Network, idx int) int {
	if idx < 0 || idx+1 >= len(networks) || !networks[idx].Known {
		return idx
	}
	if !networks[idx+1].Known {
		return idx
	}
	networks[idx], networks[idx+1] = networks[idx+1], networks[idx]
	return idx + 1
}
