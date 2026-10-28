package models

import "github.com/wendylabsinc/wendy/go/internal/shared/meshid"

// MeshDevice is a fresh signed management observation with a usable authorized
// route from the selected operator's authenticated adjacent router. It is a
// separate path, never a synthetic physical LAN sighting.
type MeshDevice struct {
	Selector  string   `json:"selector"`
	Principal string   `json:"principal"`
	Tenant    string   `json:"tenant"`
	Device    string   `json:"device"`
	Name      string   `json:"name"`
	Platform  string   `json:"platform"`
	Transport string   `json:"transport"`
	Address   string   `json:"address"`
	Caps      []string `json:"caps,omitempty"`
	Expires   int64    `json:"expiresAtUnixMs"`
	Revision  uint64   `json:"revision,string"`
	Metric    uint32   `json:"metric,omitempty"`
	NextHop   string   `json:"nextHopPrincipal,omitempty"`
}
type MeshDiscoveryState struct {
	SourceResponseBytes int    `json:"sourceResponseBytes,omitempty"`
	PollIntervalMillis  int64  `json:"pollIntervalMillis,omitempty"`
	HeartbeatMillis     int64  `json:"heartbeatMillis,omitempty"`
	State               string `json:"state"`
	Source              string `json:"source,omitempty"`
	Captured            int64  `json:"capturedAtUnixMs,omitempty"`
	Truncated           bool   `json:"truncated,omitempty"`
}

func CanonicalDevicePrincipal(principal string) string {
	device, err := meshid.DeviceFromPrincipal(principal)
	if err != nil || device.Principal() != principal {
		return ""
	}
	return principal
}
