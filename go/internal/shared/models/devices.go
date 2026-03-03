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
	InterfaceType   string `json:"interfaceType"`
	IsWendyDevice   bool   `json:"isWendyDevice"`
	AgentVersion    string `json:"agentVersion,omitempty"`
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
