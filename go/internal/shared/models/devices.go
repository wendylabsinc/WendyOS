// Package models defines device types and collections used across the CLI and agent.
package models

import (
	"encoding/json"
	"fmt"
	"sort"
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

func (d USBDevice) HumanReadable() string {
	s := d.Name
	if d.AgentVersion != "" {
		s += " v" + d.AgentVersion
	}
	return strings.TrimSpace(s)
}

// LANDevice represents a device discovered via mDNS on the local network.
type LANDevice struct {
	ID          string `json:"id"`
	DisplayName string `json:"displayName"`
	Hostname    string `json:"hostname"`
	IPAddress   string `json:"ipAddress,omitempty"`
	Port        int    `json:"port"`
	IsMTLS      bool   `json:"isMTLS,omitempty"`
	// AssetID is the cloud asset ID from the assetid TXT record; 0 when the
	// device is unprovisioned or pre-mesh.
	AssetID int32 `json:"assetId,omitempty"`
	// MeshName is the device's friendly mesh name from the `name` TXT record
	// (e.g. "brave-dolphin"); empty when unadvertised or pre-mesh.
	MeshName string `json:"meshName,omitempty"`
	// OrgID is the device's cloud organization id from the `orgid` TXT record;
	// 0 when unprovisioned or pre-mesh.
	OrgID            int32  `json:"orgId,omitempty"`
	InterfaceType    string `json:"interfaceType"`
	NetworkInterface string `json:"-"`
	USB              string `json:"usb,omitempty"`
	IsWendyDevice    bool   `json:"isWendyDevice"`
	AgentVersion     string `json:"agentVersion,omitempty"`
	DeviceType       string `json:"deviceType,omitempty"`
	OS               string `json:"os,omitempty"`
	OSVersion        string `json:"osVersion,omitempty"`
	CPUArchitecture  string `json:"cpuArchitecture,omitempty"`
}

func (d LANDevice) HumanReadable() string {
	s := fmt.Sprintf("%s @ %s:%d", d.DisplayName, d.Hostname, d.Port)
	if d.AgentVersion != "" {
		s += " v" + d.AgentVersion
	}
	return strings.TrimSpace(s)
}

// HostKey returns this device's stable cross-transport identity: its lowercased
// mDNS hostname without the trailing ".local" (or ".local.") suffix. This is the
// value BLE advertises verbatim as its local name, so it lets a LAN entry merge
// with the same physical device seen over Bluetooth. Empty when the hostname is
// unknown.
func (d LANDevice) HostKey() string {
	return normalizeHostKey(d.Hostname)
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

// HostKey returns this device's stable cross-transport identity derived from the
// advertised BLE local name, which WendyOS sets to the raw os.Hostname(). It
// matches LANDevice.HostKey for the same physical device. Empty when no usable
// name was advertised (e.g. the "WendyOS Device" fallback normalizes to a value
// that simply won't match any LAN hostname, leaving the device as its own row).
func (d BluetoothDevice) HostKey() string {
	return normalizeHostKey(d.DisplayName)
}

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
	// Simulators are the VMs this machine runs itself. Listed apart from
	// LANDevices: they are reached like a LAN device but are not one, and a
	// reader that wants devices on the network must not have to filter them.
	Simulators []LANDevice `json:"simulators,omitempty"`
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
	Externals []*ExternalDevice
}

// ConnectionTypes returns a human-readable list of available transports,
// e.g. "LAN", "BLE", "USB", "LAN, BLE", "LAN, USB", ...
func (d *DiscoveredDevice) ConnectionTypes() string {
	var types []string
	if d.LAN != nil {
		types = append(types, "LAN")
	}
	if d.Bluetooth != nil {
		if d.Bluetooth.IsWendyAgent() {
			types = append(types, "BLE")
		} else {
			types = append(types, "BLE (Lite)")
		}
	}
	for _, ext := range d.Externals {
		if ct := ext.ConnectionType(); ct != "" {
			if ext.ProviderKey == "wendy-lite" {
				types = append(types, ct+" (Lite)")
			} else {
				types = append(types, ct)
			}
		}
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
	for _, ext := range d.Externals {
		if ext == nil || ext.ConnectionInfo == nil {
			continue
		}
		if ip := ext.ConnectionInfo["ip"]; ip != "" {
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

// normalizeHostKey lowercases a name and strips the mDNS ".local" (or ".local.")
// suffix so an mDNS hostname and a BLE local name for the same device produce the
// same key.
func normalizeHostKey(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.TrimSuffix(s, ".local.")
	s = strings.TrimSuffix(s, ".local")
	return s
}

// MergedDevices returns a deduplicated slice of DiscoveredDevice by merging LAN,
// Bluetooth, and wendy-lite entries that refer to the same physical device. Two
// transports match when they share a normalized hostname (the stable identity
// BLE and mDNS both carry) or, failing that, a case-insensitive DisplayName. LAN
// metadata takes precedence; other transports backfill missing fields.
func (c *DevicesCollection) MergedDevices() []DiscoveredDevice {
	// A device is indexed under every key it can be matched by (hostname and
	// display name), so a later transport finds it regardless of which key it
	// shares. order holds one pointer per physical device to preserve insertion
	// order without double-counting multi-keyed entries.
	byKey := make(map[string]*DiscoveredDevice)
	var order []*DiscoveredDevice

	register := func(d *DiscoveredDevice, keys ...string) {
		for _, k := range keys {
			if k != "" {
				byKey[k] = d
			}
		}
	}
	lookup := func(keys ...string) *DiscoveredDevice {
		for _, k := range keys {
			if k == "" {
				continue
			}
			if d, ok := byKey[k]; ok {
				return d
			}
		}
		return nil
	}

	for i := range c.LANDevices {
		d := &c.LANDevices[i]
		merged := &DiscoveredDevice{
			DisplayName:     d.DisplayName,
			AgentVersion:    d.AgentVersion,
			OS:              d.OS,
			OSVersion:       d.OSVersion,
			CPUArchitecture: d.CPUArchitecture,
			LAN:             d,
		}
		register(merged, d.HostKey(), strings.ToLower(d.DisplayName))
		order = append(order, merged)
	}

	for i := range c.BluetoothDevices {
		d := &c.BluetoothDevices[i]
		if existing := lookup(d.HostKey(), strings.ToLower(d.DisplayName)); existing != nil {
			// Merge BLE into the existing entry.
			existing.Bluetooth = d
			// Index the existing device under the BLE key too so a later
			// transport can still find it by hostname.
			register(existing, d.HostKey())
			// Backfill any fields the existing entry is missing.
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
			continue
		}
		// BLE-only device.
		merged := &DiscoveredDevice{
			DisplayName:     d.DisplayName,
			AgentVersion:    d.AgentVersion,
			OS:              d.OS,
			OSVersion:       d.OSVersion,
			CPUArchitecture: d.CPUArchitecture,
			Bluetooth:       d,
		}
		register(merged, d.HostKey(), strings.ToLower(d.DisplayName))
		order = append(order, merged)
	}

	// Merge wendy-lite external devices by name. These represent the same
	// physical Wendy Lite hardware discovered via mDNS (WiFi) instead of BLE.
	for i := range c.ExternalDevices {
		d := &c.ExternalDevices[i]
		if d.ProviderKey != "wendy-lite" {
			continue
		}
		if existing := lookup(strings.ToLower(d.DisplayName)); existing != nil {
			existing.Externals = append(existing.Externals, d)
			sort.Slice(existing.Externals, func(i, j int) bool {
				return existing.Externals[i].Rank() > existing.Externals[j].Rank()
			})
			if existing.CPUArchitecture == "" {
				existing.CPUArchitecture = d.CPUArchitecture
			}
			continue
		}
		merged := &DiscoveredDevice{
			DisplayName:     d.DisplayName,
			CPUArchitecture: d.CPUArchitecture,
			Externals:       []*ExternalDevice{d},
		}
		register(merged, strings.ToLower(d.DisplayName))
		order = append(order, merged)
	}

	result := make([]DiscoveredDevice, 0, len(order))
	for _, d := range order {
		result = append(result, *d)
	}
	return result
}

// IsEmpty returns true if no devices were found across any interface.
func (c *DevicesCollection) IsEmpty() bool {
	return len(c.USBDevices) == 0 &&
		len(c.LANDevices) == 0 &&
		len(c.BluetoothDevices) == 0 &&
		len(c.EthernetInterfaces) == 0 &&
		len(c.ExternalDevices) == 0 &&
		len(c.Simulators) == 0
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
