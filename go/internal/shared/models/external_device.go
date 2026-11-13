package models

import "fmt"

// ExternalDevice represents a device managed by a pluggable provider (local, Docker, ADB, etc.).
type ExternalDevice struct {
	ID              string            `json:"id"` // identifies the connection to the device, not the device itself
	DisplayName     string            `json:"displayName"`
	ProviderKey     string            `json:"providerKey"`
	ConnectionInfo  map[string]string `json:"connectionInfo,omitempty"`
	IsWendyDevice   bool              `json:"isWendyDevice"`
	AgentVersion    string            `json:"agentVersion,omitempty"`
	OS              string            `json:"os,omitempty"`
	OSVersion       string            `json:"osVersion,omitempty"`
	CPUArchitecture string            `json:"cpuArchitecture,omitempty"`
}

func (d ExternalDevice) HumanReadable() string {
	s := d.DisplayName
	if s == "" {
		s = d.ID
	}
	if d.ProviderKey != "" {
		s += fmt.Sprintf(" (%s)", d.ProviderKey)
	}
	if d.AgentVersion != "" {
		s += " v" + d.AgentVersion
	}
	return s
}

// Some pluggable providers (e.g. MicroWendy) can connect to devices over multiple transport types (USB, LAN, BLE, Cloud).
// In that case, this function returns the transport type. Possible values are "USB", "LAN", "BLE", "Cloud".
// This string is displayed to the user in the device list, under "type".
// If the provider does not support multiple transport types, this function returns an empty string.
func (d ExternalDevice) ConnectionType() string {
	if d.ConnectionInfo != nil {
		if t, ok := d.ConnectionInfo["type"]; ok {
			return t
		}
	}
	return ""
}

// When a device can be reached in several ways, we should use the way that has the highest rank.
func (d ExternalDevice) Rank() int {
	if d.ConnectionType() == "USB" {
		return 3
	}
	if d.ConnectionType() == "LAN" {
		return 2
	}
	if d.ConnectionType() == "BLE" {
		return 1
	}
	return 0
}

// LiteDedupID is the key that collapses one Wendy Lite board's USB, LAN and BLE
// sightings into one row: the enrolled device UUID when the transport reported
// one, else the device id it published (the hardware id on an unenrolled
// board). Preferring the enrolled UUID is what lets USB, whose device id stays
// the hardware id after enrollment, meet LAN and BLE. It is a case-sensitive id,
// returned exactly as reported; empty when the row carries neither (an
// unflashed board).
func (d ExternalDevice) LiteDedupID() string {
	if id := d.ConnectionInfo["enrolledDeviceId"]; id != "" {
		return id
	}
	return d.ConnectionInfo["deviceId"]
}
