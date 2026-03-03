package models

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestUSBDevice_HumanReadable(t *testing.T) {
	tests := []struct {
		name   string
		device USBDevice
		want   string
	}{
		{
			name:   "name only",
			device: USBDevice{Name: "Jetson Orin Nano"},
			want:   "Jetson Orin Nano",
		},
		{
			name:   "with agent version",
			device: USBDevice{Name: "Jetson Orin Nano", AgentVersion: "2.1.0"},
			want:   "Jetson Orin Nano v2.1.0",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.device.HumanReadable()
			if got != tt.want {
				t.Errorf("HumanReadable() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestLANDevice_HumanReadable(t *testing.T) {
	tests := []struct {
		name   string
		device LANDevice
		want   string
	}{
		{
			name: "basic",
			device: LANDevice{
				DisplayName: "Wendy Dev",
				Hostname:    "wendyos-zestful-stork.local",
				Port:        50051,
			},
			want: "Wendy Dev @ wendyos-zestful-stork.local:50051",
		},
		{
			name: "with version",
			device: LANDevice{
				DisplayName:  "Wendy Dev",
				Hostname:     "wendyos-zestful-stork.local",
				Port:         50051,
				AgentVersion: "1.0.0",
			},
			want: "Wendy Dev @ wendyos-zestful-stork.local:50051 v1.0.0",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.device.HumanReadable()
			if got != tt.want {
				t.Errorf("HumanReadable() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestBluetoothDevice_HumanReadable(t *testing.T) {
	tests := []struct {
		name   string
		device BluetoothDevice
		want   string
	}{
		{
			name:   "name only",
			device: BluetoothDevice{DisplayName: "Wendy BT"},
			want:   "Wendy BT",
		},
		{
			name:   "with version and rssi",
			device: BluetoothDevice{DisplayName: "Wendy BT", AgentVersion: "1.2.3", RSSI: -45},
			want:   "Wendy BT v1.2.3 (RSSI: -45)",
		},
		{
			name:   "with version no rssi",
			device: BluetoothDevice{DisplayName: "Wendy BT", AgentVersion: "1.2.3"},
			want:   "Wendy BT v1.2.3",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.device.HumanReadable()
			if got != tt.want {
				t.Errorf("HumanReadable() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestDevicesCollection_IsEmpty(t *testing.T) {
	empty := &DevicesCollection{}
	if !empty.IsEmpty() {
		t.Error("IsEmpty() = false for empty collection, want true")
	}

	withUSB := &DevicesCollection{
		USBDevices: []USBDevice{{Name: "test"}},
	}
	if withUSB.IsEmpty() {
		t.Error("IsEmpty() = true for collection with USB device, want false")
	}

	withLAN := &DevicesCollection{
		LANDevices: []LANDevice{{Hostname: "test.local"}},
	}
	if withLAN.IsEmpty() {
		t.Error("IsEmpty() = true for collection with LAN device, want false")
	}

	withBT := &DevicesCollection{
		BluetoothDevices: []BluetoothDevice{{DisplayName: "bt"}},
	}
	if withBT.IsEmpty() {
		t.Error("IsEmpty() = true for collection with Bluetooth device, want false")
	}
}

func TestDevicesCollection_ToJSON(t *testing.T) {
	collection := &DevicesCollection{
		USBDevices: []USBDevice{
			{Name: "Jetson", VendorID: "0955", ProductID: "7045", IsWendyDevice: true},
		},
		LANDevices:         []LANDevice{},
		BluetoothDevices:   []BluetoothDevice{},
		EthernetInterfaces: []EthernetInterface{},
	}

	jsonStr, err := collection.ToJSON()
	if err != nil {
		t.Fatalf("ToJSON() error = %v", err)
	}

	// Should be valid JSON.
	var parsed map[string]json.RawMessage
	if err := json.Unmarshal([]byte(jsonStr), &parsed); err != nil {
		t.Fatalf("ToJSON() produced invalid JSON: %v", err)
	}

	if _, ok := parsed["usbDevices"]; !ok {
		t.Error("ToJSON() missing usbDevices key")
	}

	if !strings.Contains(jsonStr, "Jetson") {
		t.Error("ToJSON() output missing device name")
	}
}

func TestMergedDevices_LANAndBLESameName(t *testing.T) {
	c := &DevicesCollection{
		LANDevices: []LANDevice{
			{DisplayName: "Wendy Dev", Hostname: "wendyos.local", IPAddress: "192.168.1.10", Port: 50051, AgentVersion: "1.0.0", OS: "linux"},
		},
		BluetoothDevices: []BluetoothDevice{
			{DisplayName: "Wendy Dev", Address: "AA:BB:CC:DD:EE:FF", AgentVersion: "1.0.0", RSSI: -50, L2CAPPSM: 128},
		},
	}

	merged := c.MergedDevices()
	if len(merged) != 1 {
		t.Fatalf("MergedDevices() returned %d entries, want 1", len(merged))
	}

	d := merged[0]
	if d.DisplayName != "Wendy Dev" {
		t.Errorf("DisplayName = %q, want %q", d.DisplayName, "Wendy Dev")
	}
	if d.LAN == nil {
		t.Fatal("LAN is nil, want non-nil")
	}
	if d.Bluetooth == nil {
		t.Fatal("Bluetooth is nil, want non-nil")
	}
	if d.ConnectionTypes() != "LAN, Bluetooth" {
		t.Errorf("ConnectionTypes() = %q, want %q", d.ConnectionTypes(), "LAN, Bluetooth")
	}
	if d.Address() != "192.168.1.10" {
		t.Errorf("Address() = %q, want %q", d.Address(), "192.168.1.10")
	}
	if d.Port() != 50051 {
		t.Errorf("Port() = %d, want %d", d.Port(), 50051)
	}
}

func TestMergedDevices_CaseInsensitive(t *testing.T) {
	c := &DevicesCollection{
		LANDevices: []LANDevice{
			{DisplayName: "Wendy Dev", Hostname: "wendyos.local", Port: 50051},
		},
		BluetoothDevices: []BluetoothDevice{
			{DisplayName: "wendy dev", Address: "AA:BB:CC:DD:EE:FF"},
		},
	}

	merged := c.MergedDevices()
	if len(merged) != 1 {
		t.Fatalf("MergedDevices() returned %d entries, want 1", len(merged))
	}
	if merged[0].LAN == nil || merged[0].Bluetooth == nil {
		t.Error("case-insensitive match failed: expected both LAN and Bluetooth to be set")
	}
	// LAN display name takes precedence
	if merged[0].DisplayName != "Wendy Dev" {
		t.Errorf("DisplayName = %q, want %q (LAN takes precedence)", merged[0].DisplayName, "Wendy Dev")
	}
}

func TestMergedDevices_BLEOnly(t *testing.T) {
	c := &DevicesCollection{
		BluetoothDevices: []BluetoothDevice{
			{DisplayName: "BLE Only", Address: "11:22:33:44:55:66", AgentVersion: "2.0.0", L2CAPPSM: 128},
		},
	}

	merged := c.MergedDevices()
	if len(merged) != 1 {
		t.Fatalf("MergedDevices() returned %d entries, want 1", len(merged))
	}

	d := merged[0]
	if d.LAN != nil {
		t.Error("LAN should be nil for BLE-only device")
	}
	if d.Bluetooth == nil {
		t.Fatal("Bluetooth should be non-nil")
	}
	if d.ConnectionTypes() != "Bluetooth" {
		t.Errorf("ConnectionTypes() = %q, want %q", d.ConnectionTypes(), "Bluetooth")
	}
	if d.Address() != "11:22:33:44:55:66" {
		t.Errorf("Address() = %q, want BLE address", d.Address())
	}
	if d.Port() != 0 {
		t.Errorf("Port() = %d, want 0 for BLE-only", d.Port())
	}
}

func TestMergedDevices_LANOnly(t *testing.T) {
	c := &DevicesCollection{
		LANDevices: []LANDevice{
			{DisplayName: "LAN Only", Hostname: "wendyos.local", Port: 50051},
		},
	}

	merged := c.MergedDevices()
	if len(merged) != 1 {
		t.Fatalf("MergedDevices() returned %d entries, want 1", len(merged))
	}

	d := merged[0]
	if d.LAN == nil {
		t.Fatal("LAN should be non-nil")
	}
	if d.Bluetooth != nil {
		t.Error("Bluetooth should be nil for LAN-only device")
	}
	if d.ConnectionTypes() != "LAN" {
		t.Errorf("ConnectionTypes() = %q, want %q", d.ConnectionTypes(), "LAN")
	}
}

func TestMergedDevices_Empty(t *testing.T) {
	c := &DevicesCollection{}
	merged := c.MergedDevices()
	if len(merged) != 0 {
		t.Fatalf("MergedDevices() returned %d entries, want 0", len(merged))
	}
}

func TestMergedDevices_BLEBackfill(t *testing.T) {
	c := &DevicesCollection{
		LANDevices: []LANDevice{
			{DisplayName: "Wendy Dev", Hostname: "wendyos.local", Port: 50051},
		},
		BluetoothDevices: []BluetoothDevice{
			{DisplayName: "Wendy Dev", Address: "AA:BB:CC:DD:EE:FF", AgentVersion: "1.5.0", OS: "linux", OSVersion: "6.1", CPUArchitecture: "aarch64"},
		},
	}

	merged := c.MergedDevices()
	if len(merged) != 1 {
		t.Fatalf("MergedDevices() returned %d entries, want 1", len(merged))
	}

	d := merged[0]
	// LAN had no AgentVersion, BLE should backfill
	if d.AgentVersion != "1.5.0" {
		t.Errorf("AgentVersion = %q, want %q (backfilled from BLE)", d.AgentVersion, "1.5.0")
	}
	if d.OS != "linux" {
		t.Errorf("OS = %q, want %q (backfilled from BLE)", d.OS, "linux")
	}
	if d.OSVersion != "6.1" {
		t.Errorf("OSVersion = %q, want %q (backfilled from BLE)", d.OSVersion, "6.1")
	}
	if d.CPUArchitecture != "aarch64" {
		t.Errorf("CPUArchitecture = %q, want %q (backfilled from BLE)", d.CPUArchitecture, "aarch64")
	}
}

func TestMergedDevices_LANMetadataTakesPrecedence(t *testing.T) {
	c := &DevicesCollection{
		LANDevices: []LANDevice{
			{DisplayName: "Wendy Dev", Hostname: "wendyos.local", Port: 50051, AgentVersion: "2.0.0", OS: "linux"},
		},
		BluetoothDevices: []BluetoothDevice{
			{DisplayName: "Wendy Dev", Address: "AA:BB:CC:DD:EE:FF", AgentVersion: "1.5.0", OS: "freebsd"},
		},
	}

	merged := c.MergedDevices()
	if len(merged) != 1 {
		t.Fatalf("MergedDevices() returned %d entries, want 1", len(merged))
	}

	d := merged[0]
	// LAN metadata takes precedence
	if d.AgentVersion != "2.0.0" {
		t.Errorf("AgentVersion = %q, want %q (LAN takes precedence)", d.AgentVersion, "2.0.0")
	}
	if d.OS != "linux" {
		t.Errorf("OS = %q, want %q (LAN takes precedence)", d.OS, "linux")
	}
}

func TestMergedDevices_MultipleDistinctDevices(t *testing.T) {
	c := &DevicesCollection{
		LANDevices: []LANDevice{
			{DisplayName: "Device A", Hostname: "a.local", Port: 50051},
			{DisplayName: "Device B", Hostname: "b.local", Port: 50051},
		},
		BluetoothDevices: []BluetoothDevice{
			{DisplayName: "Device C", Address: "CC:CC:CC:CC:CC:CC"},
			{DisplayName: "Device A", Address: "AA:AA:AA:AA:AA:AA"},
		},
	}

	merged := c.MergedDevices()
	if len(merged) != 3 {
		t.Fatalf("MergedDevices() returned %d entries, want 3", len(merged))
	}

	// Device A should be merged (index 0)
	if merged[0].DisplayName != "Device A" || merged[0].LAN == nil || merged[0].Bluetooth == nil {
		t.Error("Device A should be merged with both LAN and Bluetooth")
	}
	// Device B is LAN-only (index 1)
	if merged[1].DisplayName != "Device B" || merged[1].LAN == nil || merged[1].Bluetooth != nil {
		t.Error("Device B should be LAN-only")
	}
	// Device C is BLE-only (index 2)
	if merged[2].DisplayName != "Device C" || merged[2].LAN != nil || merged[2].Bluetooth == nil {
		t.Error("Device C should be Bluetooth-only")
	}
}

func TestMergedDevices_BLELiteDevice(t *testing.T) {
	c := &DevicesCollection{
		BluetoothDevices: []BluetoothDevice{
			{DisplayName: "Wendy Lite", Address: "AA:BB:CC:DD:EE:FF", L2CAPPSM: 0, IsWendyDevice: true},
		},
	}

	merged := c.MergedDevices()
	if len(merged) != 1 {
		t.Fatalf("MergedDevices() returned %d entries, want 1", len(merged))
	}

	d := merged[0]
	if d.ConnectionTypes() != "BLE (Lite)" {
		t.Errorf("ConnectionTypes() = %q, want %q", d.ConnectionTypes(), "BLE (Lite)")
	}
	if d.Bluetooth == nil {
		t.Fatal("Bluetooth should be non-nil")
	}
	if d.LAN != nil {
		t.Error("LAN should be nil for Lite-only device")
	}
}

func TestBluetoothDevice_IsWendyAgent(t *testing.T) {
	tests := []struct {
		name string
		psm  uint16
		want bool
	}{
		{name: "agent with L2CAPPSM=128", psm: 128, want: true},
		{name: "lite with L2CAPPSM=0", psm: 0, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := BluetoothDevice{L2CAPPSM: tt.psm}
			if got := d.IsWendyAgent(); got != tt.want {
				t.Errorf("IsWendyAgent() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestInterfaceType_Constants(t *testing.T) {
	tests := []struct {
		iface InterfaceType
		want  string
	}{
		{InterfaceUSB, "usb"},
		{InterfaceEthernet, "ethernet"},
		{InterfaceLAN, "lan"},
		{InterfaceBluetooth, "bluetooth"},
	}

	for _, tt := range tests {
		if string(tt.iface) != tt.want {
			t.Errorf("InterfaceType = %q, want %q", string(tt.iface), tt.want)
		}
	}
}
