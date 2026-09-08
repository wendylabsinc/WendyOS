package config

import "strings"

// Pin sources. A pin's source records how much the CLI knows about it, which
// decides who may overwrite it: cloud spoke to the org's cloud over an
// authenticated session, lan only observed a certificate on the local network.
const (
	PinSourceLAN   = "lan"
	PinSourceCloud = "cloud"
)

// DevicePin binds a device hostname to the organisation, cloud host, and asset
// its TLS identity must belong to (WDY-1149). It is deliberately NOT a
// certificate fingerprint: a device legitimately rotates or re-enrolls its
// cert, which we must not treat as an attack. What we anchor is the device's
// org + cloud + asset id — so a change of trust domain (a different
// organisation or cloud host) OR of the device itself (a different asset id
// answering at this hostname) trips the pin, while a routine cert rotation does
// not.
//
// AssetID is the numeric cloud asset id carried in the agent certificate's
// "urn:wendy:org:<org>:asset:<assetID>" URI SAN, kept as a string because that
// is how the URN carries it. It is empty for pins written before asset ids were
// pinned, and for devices whose certificate carries no asset identity at all.
//
// Source records where the pin came from: empty means a pin written before
// sources were recorded, read as PinSourceLAN.
type DevicePin struct {
	OrgID     int    `json:"orgId"`
	CloudGRPC string `json:"cloudGRPC"`
	AssetID   string `json:"assetId,omitempty"`
	Source    string `json:"source,omitempty"`
	// Principal is the tenant SPIFFE principal the device's certificate
	// carries, when it has one. It is recorded — not compared — because it is
	// the key the SPKI pin store files the device under, and `wendy device
	// unpin <hostname>` has no other way to reach that entry: the (org, asset)
	// pair a legacy pin holds cannot be turned back into a principal without
	// the tenant. Empty for an old-chain device and for pins written before
	// the SPIFFE cutover; backfilled on the next successful connect.
	Principal string `json:"principal,omitempty"`
}

// PinVerdict is the result of comparing an observed device identity against the
// stored pin for its hostname.
type PinVerdict int

const (
	// PinFirstUse means no pin is recorded for the hostname yet.
	PinFirstUse PinVerdict = iota
	// PinMatch means the observed identity matches the stored pin.
	PinMatch
	// PinMismatch means the observed org, cloud host, or asset id differs from
	// the stored pin.
	PinMismatch
	// PinAdoptAsset means org + cloud host match a pin that predates asset
	// pinning, and the observed asset id should be backfilled into it. Org and
	// cloud already vouch for this connection, so it is not an attack signal —
	// it is the one-time upgrade of an older pin.
	PinAdoptAsset
)

// normalizePinHost lowercases, trims whitespace, and strips a trailing dot and
// ".local" suffix so cosmetic variants of the same hostname key the same pin.
func normalizePinHost(host string) string {
	h := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
	return strings.TrimSuffix(h, ".local")
}

// DevicePinFor returns the stored pin for a hostname, if any.
func (c *Config) DevicePinFor(hostname string) (DevicePin, bool) {
	p, ok := c.DevicePins[normalizePinHost(hostname)]
	return p, ok
}

// EvaluateDevicePin compares an observed (orgID, cloudGRPC, assetID) for a
// hostname against the stored pin without mutating the config.
//
// An empty observed assetID means "this device's certificate carries no asset
// identity" (a legacy cert), not "a different device": it can never produce a
// mismatch on its own, because absence of evidence is not evidence of a swap.
func (c *Config) EvaluateDevicePin(hostname string, orgID int, cloudGRPC, assetID string) PinVerdict {
	pin, ok := c.DevicePinFor(hostname)
	if !ok {
		return PinFirstUse
	}
	if pin.OrgID != orgID || pin.CloudGRPC != cloudGRPC {
		return PinMismatch
	}
	switch {
	case assetID == "":
		return PinMatch
	case pin.AssetID == "":
		if pin.Source == PinSourceCloud {
			// Cloud said this device has no asset identity; a LAN sighting is
			// not evidence to the contrary.
			return PinMatch
		}
		return PinAdoptAsset
	case pin.AssetID != assetID:
		return PinMismatch
	default:
		return PinMatch
	}
}

// PinSource returns the recorded source for a hostname's pin, defaulting to
// PinSourceLAN for pins written before sources existed and for unpinned hosts.
func (c *Config) PinSource(hostname string) string {
	pin, ok := c.DevicePinFor(hostname)
	if !ok || pin.Source == "" {
		return PinSourceLAN
	}
	return pin.Source
}

// SetDevicePinFrom records a pin and where it came from. A cloud-sourced write
// is authoritative and overwrites whatever was there.
func (c *Config) SetDevicePinFrom(hostname string, orgID int, cloudGRPC, assetID, principal, source string) {
	if c.DevicePins == nil {
		c.DevicePins = make(map[string]DevicePin)
	}
	c.DevicePins[normalizePinHost(hostname)] = DevicePin{
		OrgID: orgID, CloudGRPC: cloudGRPC, AssetID: assetID, Principal: principal, Source: source,
	}
}

// SetDevicePin records (or replaces) the pin for a hostname.
func (c *Config) SetDevicePin(hostname string, orgID int, cloudGRPC, assetID, principal string) {
	c.SetDevicePinFrom(hostname, orgID, cloudGRPC, assetID, principal, PinSourceLAN)
}

// ClearDevicePin drops the pin for a hostname. It is for the case where the
// user has confirmed that the device at this hostname legitimately no longer
// has a Wendy identity to pin — an unenrolled or reflashed device — so its next
// enrollment starts from a clean first use rather than being challenged against
// an identity that is gone.
func (c *Config) ClearDevicePin(hostname string) {
	delete(c.DevicePins, normalizePinHost(hostname))
}
