package config

import "github.com/wendylabsinc/wendy-go/devices"

// Pin sources. A pin's source records how much the CLI knows about it, which
// decides who may overwrite it: cloud spoke to the org's cloud over an
// authenticated session, lan only observed a certificate on the local network.
const (
	PinSourceLAN   = devices.PinSourceLAN
	PinSourceCloud = devices.PinSourceCloud
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
type DevicePin = devices.DevicePin

// PinVerdict is the result of comparing an observed device identity against the
// stored pin for its hostname.
type PinVerdict = devices.PinVerdict

const (
	// PinFirstUse means no pin is recorded for the hostname yet.
	PinFirstUse = devices.PinFirstUse
	// PinMatch means the observed identity matches the stored pin.
	PinMatch = devices.PinMatch
	// PinMismatch means the observed org, cloud host, or asset id differs from
	// the stored pin.
	PinMismatch = devices.PinMismatch
	// PinAdoptAsset means org + cloud host match a pin that predates asset
	// pinning, and the observed asset id should be backfilled into it. Org and
	// cloud already vouch for this connection, so it is not an attack signal —
	// it is the one-time upgrade of an older pin.
	PinAdoptAsset = devices.PinAdoptAsset
)

func (c *Config) DevicePinFor(hostname string) (DevicePin, bool) {
	return (&devices.HostPins{DevicePins: c.DevicePins}).DevicePinFor(hostname)
}
func (c *Config) EvaluateDevicePin(hostname string, orgID int, cloudGRPC, assetID string) PinVerdict {
	return (&devices.HostPins{DevicePins: c.DevicePins}).EvaluateDevicePin(hostname, orgID, cloudGRPC, assetID)
}
func (c *Config) PinSource(hostname string) string {
	return (&devices.HostPins{DevicePins: c.DevicePins}).PinSource(hostname)
}
func (c *Config) SetDevicePinFrom(hostname string, orgID int, cloudGRPC, assetID, principal, source string) {
	pins := &devices.HostPins{DevicePins: c.DevicePins}
	pins.SetDevicePinFrom(hostname, orgID, cloudGRPC, assetID, principal, source)
	c.DevicePins = pins.DevicePins
}
func (c *Config) SetDevicePin(hostname string, orgID int, cloudGRPC, assetID, principal string) {
	c.SetDevicePinFrom(hostname, orgID, cloudGRPC, assetID, principal, devices.PinSourceLAN)
}
func (c *Config) ClearDevicePin(hostname string) {
	(&devices.HostPins{DevicePins: c.DevicePins}).ClearDevicePin(hostname)
}
