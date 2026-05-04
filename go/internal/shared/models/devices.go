// Package models defines device types and collections used across the CLI and agent.
package models

import (
	"encoding/json"
	"fmt"
	"strings"
)

// InterfaceType represents the type of device interface.
type InterfaceType string

const (
	InterfaceUSB       InterfaceType = "usb"
	InterfaceEthernet  InterfaceType = "ethernet"
	InterfaceLAN       InterfaceType = "lan"
	InterfaceBluetooth InterfaceType = "bluetooth"
	InterfaceExternal  InterfaceType = "external"
)

// ESP32 USB identifiers (Espressif ESP32-C6).
const (
	ESP32VendorID  = "0x303a"
	ESP32ProductID = "0x1001"
)

// USBDevice represents a USB-connected Wendy device.
type USBDevice struct {
	Name              string `json:"name"`
	DisplayName       string `json:"displayName"`
	SerialNumber      string `json:"serialNumber,omitempty"`
	VendorID          string `json:"vendorId"`
	ProductID         string `json:"productId"`
	USBVersion        string `json:"usbVersion,omitempty"`
	MaxPowerMilliamps int    `json:"maxPowerMilliamps,omitempty"`
	Hostname          string `json:"hostname,omitempty"`
	AgentVersion      string `json:"agentVersion,omitempty"`
	IsWendyDevice     bool   `json:"isWendyDevice"`
	IsESP32           bool   `json:"isESP32,omitempty"`
}

// HumanReadable returns a human-friendly string describing this USB device.
func (d USBDevice) HumanReadable() string {
	s := d.Name
	if d.AgentVersion != "" {
		s += " v" + d.AgentVersion
	}
	return strings.TrimSpace(s)
}

// LANDevice represents a device discovered via mDNS on the local network.
type LANDevice struct {
	ID              string `json:"id"`
	DisplayName     string `json:"displayName"`
	Hostname        string `json:"hostname"`
	IPAddress       string `json:"ipAddress,omitempty"`
	Port            int    `json:"port"`
	IsMTLS          bool   `json:"isMTLS,omitempty"`
	InterfaceType   string `json:"interfaceType"`
	IsWendyDevice   bool   `json:"isWendyDevice"`
	AgentVersion    string `json:"agentVersion,omitempty"`
	DeviceType      string `json:"deviceType,omitempty"`
	OS              string `json:"os,omitempty"`
	OSVersion       string `json:"osVersion,omitempty"`
	CPUArchitecture string `json:"cpuArchitecture,omitempty"`
}

// HumanReadable returns a human-friendly string describing this LAN device.
func (d LANDevice) HumanReadable() string {
	s := fmt.Sprintf("%s @ %s:%d", d.DisplayName, d.Hostname, d.Port)
	if d.AgentVersion != "" {
		s += " v" + d.AgentVersion
	}
	return strings.TrimSpace(s)
}

// BluetoothDevice represents a Bluetooth-discovered Wendy device.
type BluetoothDevice struct {
	ID              string `json:"id"`
	DisplayName     string `json:"displayName"`
	Name            string `json:"name,omitempty"`
	Address         string `json:"address"`
	RSSI            int    `json:"rssi"`
	IsWendyDevice   bool   `json:"isWendyDevice"`
	AgentVersion    string `json:"agentVersion,omitempty"`
	OS              string `json:"os,omitempty"`
	OSVersion       string `json:"osVersion,omitempty"`
	CPUArchitecture string `json:"cpuArchitecture,omitempty"`
	L2CAPPSM        uint16 `json:"l2capPSM,omitempty"`
}

// IsWendyAgent returns true if this device supports the WendyOS agent
// protobuf-over-L2CAP protocol (as opposed to Wendy Lite GATT provisioning).
func (d BluetoothDevice) IsWendyAgent() bool {
	return d.L2CAPPSM > 0
}

// HumanReadable returns a human-friendly string describing this Bluetooth device.
func (d BluetoothDevice) HumanReadable() string {
	s := d.DisplayName
	if d.AgentVersion != "" {
		s += " v" + d.AgentVersion
	}
	if d.RSSI != 0 {
		s += fmt.Sprintf(" (RSSI: %d)", d.RSSI)
	}
	return strings.TrimSpace(s)
}

// EthernetInterface represents an Ethernet or Wi-Fi interface connected to a Wendy device.
type EthernetInterface struct {
	Name          string `json:"name"`
	DisplayName   string `json:"displayName"`
	IPAddress     string `json:"ipAddress,omitempty"`
	Hostname      string `json:"hostname,omitempty"`
	MACAddress    string `json:"macAddress,omitempty"`
	LinkSpeed     string `json:"linkSpeed,omitempty"`
	IsWendyDevice bool   `json:"isWendyDevice"`
	AgentVersion  string `json:"agentVersion,omitempty"`
}

// HumanReadable returns a human-friendly string describing this Ethernet interface.
func (d EthernetInterface) HumanReadable() string {
	parts := []string{fmt.Sprintf("%s @ %s", d.DisplayName, d.Name)}
	if d.AgentVersion != "" {
		parts = append(parts, "v"+d.AgentVersion)
	}
	if d.MACAddress != "" {
		parts = append(parts, "["+d.MACAddress+"]")
	}
	if d.LinkSpeed != "" {
		parts = append(parts, "["+d.LinkSpeed+"]")
	}
	return strings.Join(parts, " ")
}

// DevicesCollection holds all discovered devices across interface types.
type DevicesCollection struct {
	USBDevices         []USBDevice         `json:"usbDevices"`
	LANDevices         []LANDevice         `json:"lanDevices"`
	BluetoothDevices   []BluetoothDevice   `json:"bluetoothDevices"`
	EthernetInterfaces []EthernetInterface `json:"ethernetDevices"`
	ExternalDevices    []ExternalDevice    `json:"externalDevices,omitempty"`
}

// DiscoveredDevice represents a single physical device that may have been
// discovered via LAN (mDNS), Bluetooth, or both. When the same device appears
// on multiple transports, they are merged into one DiscoveredDevice.
type DiscoveredDevice struct {
	DisplayName     string
	AgentVersion    string
	OS              string
	OSVersion       string
	CPUArchitecture string

	LAN       *LANDevice
	Bluetooth *BluetoothDevice
	External  *ExternalDevice
}

// ConnectionTypes returns a human-readable list of available transports,
// e.g. "LAN", "Bluetooth", or "LAN, Bluetooth".
func (d *DiscoveredDevice) ConnectionTypes() string {
	var types []string
	if d.LAN != nil {
		types = append(types, "LAN")
	}
	if d.Bluetooth != nil {
		if d.Bluetooth.IsWendyAgent() {
			types = append(types, "Bluetooth")
		} else {
			types = append(types, "BLE (Lite)")
		}
	}
	if d.External != nil {
		types = append(types, "LAN (Lite)")
	}
	return strings.Join(types, ", ")
}

// Address returns the best available address for display purposes.
// Prefers the LAN IP/hostname over the BLE address.
func (d *DiscoveredDevice) Address() string {
	if d.LAN != nil {
		if d.LAN.IPAddress != "" {
			return d.LAN.IPAddress
		}
		return d.LAN.Hostname
	}
	if d.External != nil {
		if ip := d.External.ConnectionInfo["ip"]; ip != "" {
			return ip
		}
	}
	if d.Bluetooth != nil {
		return d.Bluetooth.Address
	}
	return ""
}

// Port returns the LAN port if available, or 0.
func (d *DiscoveredDevice) Port() int {
	if d.LAN != nil {
		return d.LAN.Port
	}
	return 0
}

// MergedDevices returns a deduplicated slice of DiscoveredDevice by merging
// LAN and Bluetooth entries that share the same DisplayName (case-insensitive).
// LAN metadata takes precedence; BLE backfills missing fields.
func (c *DevicesCollection) MergedDevices() []DiscoveredDevice {
	// Index by normalized (lower-case) display name.
	byName := make(map[string]*DiscoveredDevice)
	var order []string // preserve insertion order

	for i := range c.LANDevices {
		d := &c.LANDevices[i]
		key := strings.ToLower(d.DisplayName)
		merged := &DiscoveredDevice{
			DisplayName:     d.DisplayName,
			AgentVersion:    d.AgentVersion,
			OS:              d.OS,
			OSVersion:       d.OSVersion,
			CPUArchitecture: d.CPUArchitecture,
			LAN:             d,
		}
		byName[key] = merged
		order = append(order, key)
	}

	for i := range c.BluetoothDevices {
		d := &c.BluetoothDevices[i]
		key := strings.ToLower(d.DisplayName)
		if existing, ok := byName[key]; ok {
			// Merge BLE into existing LAN entry.
			existing.Bluetooth = d
			// Backfill any fields the LAN entry is missing.
			if existing.AgentVersion == "" {
				existing.AgentVersion = d.AgentVersion
			}
			if existing.OS == "" {
				existing.OS = d.OS
			}
			if existing.OSVersion == "" {
				existing.OSVersion = d.OSVersion
			}
			if existing.CPUArchitecture == "" {
				existing.CPUArchitecture = d.CPUArchitecture
			}
		} else {
			// BLE-only device.
			merged := &DiscoveredDevice{
				DisplayName:     d.DisplayName,
				AgentVersion:    d.AgentVersion,
				OS:              d.OS,
				OSVersion:       d.OSVersion,
				CPUArchitecture: d.CPUArchitecture,
				Bluetooth:       d,
			}
			byName[key] = merged
			order = append(order, key)
		}
	}

	// Merge wendy-lite external devices by name. These represent the same
	// physical Wendy Lite hardware discovered via mDNS (WiFi) instead of BLE.
	for i := range c.ExternalDevices {
		d := &c.ExternalDevices[i]
		if d.ProviderKey != "wendy-lite" {
			continue
		}
		key := strings.ToLower(d.DisplayName)
		if existing, ok := byName[key]; ok {
			existing.External = d
			if existing.CPUArchitecture == "" {
				existing.CPUArchitecture = d.CPUArchitecture
			}
		} else {
			merged := &DiscoveredDevice{
				DisplayName:     d.DisplayName,
				CPUArchitecture: d.CPUArchitecture,
				External:        d,
			}
			byName[key] = merged
			order = append(order, key)
		}
	}

	result := make([]DiscoveredDevice, 0, len(order))
	for _, key := range order {
		result = append(result, *byName[key])
	}
	return result
}

// IsEmpty returns true if no devices were found across any interface.
func (c *DevicesCollection) IsEmpty() bool {
	return len(c.USBDevices) == 0 &&
		len(c.LANDevices) == 0 &&
		len(c.BluetoothDevices) == 0 &&
		len(c.EthernetInterfaces) == 0 &&
		len(c.ExternalDevices) == 0
}

// ToJSON returns a pretty-printed JSON representation of the collection.
func (c *DevicesCollection) ToJSON() (string, error) {
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return "", fmt.Errorf("marshaling devices to JSON: %w", err)
	}
	return string(data), nil
}

// ToHumanReadable returns a human-readable summary of all discovered devices.
func (c *DevicesCollection) ToHumanReadable() string {
	if c.IsEmpty() {
		return "No devices found."
	}

	var sb strings.Builder

	for _, d := range c.USBDevices {
		sb.WriteString("\n" + d.HumanReadable())
	}
	for _, d := range c.EthernetInterfaces {
		sb.WriteString("\n" + d.HumanReadable())
	}
	for _, d := range c.LANDevices {
		sb.WriteString("\n" + d.HumanReadable())
	}
	for _, d := range c.BluetoothDevices {
		sb.WriteString("\n" + d.HumanReadable())
	}
	for _, d := range c.ExternalDevices {
		sb.WriteString("\n" + d.HumanReadable())
	}

	return sb.String()
}
