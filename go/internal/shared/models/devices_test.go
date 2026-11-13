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
	if d.ConnectionTypes() != "LAN, BLE" {
		t.Errorf("ConnectionTypes() = %q, want %q", d.ConnectionTypes(), "LAN, BLE")
	}
	if d.Address() != "192.168.1.10" {
		t.Errorf("Address() = %q, want %q", d.Address(), "192.168.1.10")
	}
	if d.Port() != 50051 {
		t.Errorf("Port() = %d, want %d", d.Port(), 50051)
	}
}

// TestMergedDevices_LANAndBLEByHostname reproduces the real-world duplicate:
// mDNS advertises a friendly displayname TXT record ("Agx Orin") while BLE only
// advertises the raw hostname ("wendyos-agx-orin"). The two must still merge
// into one device via their shared hostname.
func TestMergedDevices_LANAndBLEByHostname(t *testing.T) {
	c := &DevicesCollection{
		LANDevices: []LANDevice{
			{DisplayName: "Agx Orin", Hostname: "wendyos-agx-orin.local", IPAddress: "192.168.1.10", Port: 50051, AgentVersion: "1.0.0", OS: "linux"},
		},
		BluetoothDevices: []BluetoothDevice{
			{DisplayName: "wendyos-agx-orin", Address: "EE48983F-300C-3A22-395B-290A76741CA1", RSSI: -50, L2CAPPSM: 128},
		},
	}

	merged := c.MergedDevices()
	if len(merged) != 1 {
		t.Fatalf("MergedDevices() returned %d entries, want 1 (BLE should merge into LAN by hostname)", len(merged))
	}
	d := merged[0]
	if d.LAN == nil || d.Bluetooth == nil {
		t.Fatalf("expected both LAN and Bluetooth set; got LAN=%v Bluetooth=%v", d.LAN, d.Bluetooth)
	}
	// LAN's friendly display name takes precedence.
	if d.DisplayName != "Agx Orin" {
		t.Errorf("DisplayName = %q, want %q", d.DisplayName, "Agx Orin")
	}
	if d.ConnectionTypes() != "LAN, BLE" {
		t.Errorf("ConnectionTypes() = %q, want %q", d.ConnectionTypes(), "LAN, BLE")
	}
}

// TestMergedDevices_BLEBeforeLANByHostname covers the same match when the BLE
// entry is processed and the LAN entry arrives via hostname regardless of the
// order the transports are registered in.
func TestMergedDevices_HostnameCaseAndTrailingDot(t *testing.T) {
	c := &DevicesCollection{
		LANDevices: []LANDevice{
			{DisplayName: "Jetson Orin Nano", Hostname: "WendyOS-Jetson-Orin-Nano.local.", Port: 50051},
		},
		BluetoothDevices: []BluetoothDevice{
			{DisplayName: "wendyos-jetson-orin-nano", Address: "9B1BB406-E1B2-69D7-F678-DE3F2BB66B68", L2CAPPSM: 128},
		},
	}

	merged := c.MergedDevices()
	if len(merged) != 1 {
		t.Fatalf("MergedDevices() returned %d entries, want 1 (case/trailing-dot insensitive hostname match)", len(merged))
	}
	if merged[0].LAN == nil || merged[0].Bluetooth == nil {
		t.Error("expected both LAN and Bluetooth to be set")
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
	if d.ConnectionTypes() != "BLE" {
		t.Errorf("ConnectionTypes() = %q, want %q", d.ConnectionTypes(), "BLE")
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

// TestMergedDevices_CopiesAddresses proves the multi-interface address union
// carried on a LANDevice reaches the merged DiscoveredDevice, so the dial ladder
// downstream can try every path the device was seen at.
func TestMergedDevices_CopiesAddresses(t *testing.T) {
	c := &DevicesCollection{
		LANDevices: []LANDevice{
			{
				DisplayName: "Multi Homed",
				Hostname:    "orin.local",
				IPAddress:   "192.168.1.69",
				Addresses:   []string{"192.168.1.69", "169.254.1.2"},
				Port:        50051,
			},
		},
	}

	merged := c.MergedDevices()
	if len(merged) != 1 {
		t.Fatalf("MergedDevices() returned %d entries, want 1", len(merged))
	}
	d := merged[0]
	if d.IPAddress != "192.168.1.69" {
		t.Errorf("primary IPAddress = %q, want 192.168.1.69", d.IPAddress)
	}
	if len(d.Addresses) != 2 || d.Addresses[0] != "192.168.1.69" || d.Addresses[1] != "169.254.1.2" {
		t.Errorf("Addresses = %v, want [192.168.1.69 169.254.1.2]", d.Addresses)
	}
}

func TestMergedDevices_Empty(t *testing.T) {
	c := &DevicesCollection{}
	merged := c.MergedDevices()
	if len(merged) != 0 {
		t.Fatalf("MergedDevices() returned %d entries, want 0", len(merged))
	}
}

func TestMergedDevices_CopiesPKITenant(t *testing.T) {
	const tenant = "11111111-2222-4333-8444-555555555555"
	const principal = "spiffe://wendy.sh/tenant/" + tenant + "/device/lite-board-a"
	c := DevicesCollection{LANDevices: []LANDevice{{
		ID: "board-a", TenantUUID: tenant, PrincipalURI: principal, IsMTLS: true, WendyLite: true, Sensorlink: true,
	}}}
	merged := c.MergedDevices()
	if len(merged) != 1 || merged[0].TenantUUID != tenant || merged[0].PrincipalURI != principal || !merged[0].IsMTLS ||
		!merged[0].WendyLite || !merged[0].Sensorlink {
		t.Fatalf("PKI enrollment metadata lost during merge: %+v", merged)
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
	if d.ConnectionTypes() != "BLE" {
		t.Errorf("ConnectionTypes() = %q, want %q", d.ConnectionTypes(), "BLE")
	}
	if d.Bluetooth == nil {
		t.Fatal("Bluetooth should be non-nil")
	}
	if d.LAN != nil {
		t.Error("LAN should be nil for Lite-only device")
	}
}

// liteExternal builds a wendy-lite ExternalDevice for the MergedDevices tests.
func liteExternal(id, displayName string, info map[string]string) ExternalDevice {
	return ExternalDevice{ID: id, DisplayName: displayName, ProviderKey: "wendy-lite", ConnectionInfo: info}
}

// An enrolled board keeps its hardware id as deviceId over USB but publishes
// the enrolled UUID on LAN and BLE; the three sightings are still one board.
func TestMergedDevices_LiteEnrolledAcrossTransports(t *testing.T) {
	const enrolled = "0b8e3a52-6f1d-4c8e-9a47-2d5f1e6c7b90"
	c := &DevicesCollection{
		ExternalDevices: []ExternalDevice{
			liteExternal("wendy-lite:AA:BB", "Lite Board", map[string]string{
				"type": "BLE", "deviceId": enrolled, "enrolledDeviceId": enrolled,
			}),
			liteExternal("wendy-lite:lite-board.local.", "Lite Board", map[string]string{
				"type": "LAN", "deviceId": enrolled,
			}),
			liteExternal("wendy-lite:/dev/cu.usbmodem2101", "Lite Board", map[string]string{
				"type": "USB", "deviceId": "a1b2c3d4e5f6", "enrolledDeviceId": enrolled,
			}),
		},
	}

	merged := c.MergedDevices()
	if len(merged) != 1 {
		t.Fatalf("MergedDevices() returned %d entries, want 1: %+v", len(merged), merged)
	}
	var types []string
	for _, ext := range merged[0].Externals {
		types = append(types, ext.ConnectionType())
	}
	if got := strings.Join(types, ","); got != "USB,LAN,BLE" {
		t.Errorf("Externals in rank order = %s, want USB,LAN,BLE", got)
	}
}

func TestMergedDevices_LiteUnenrolledMergesOnDeviceID(t *testing.T) {
	c := &DevicesCollection{
		ExternalDevices: []ExternalDevice{
			liteExternal("wendy-lite:/dev/cu.usbmodem2101", "Lite Board", map[string]string{"type": "USB", "deviceId": "a1b2c3d4e5f6"}),
			liteExternal("wendy-lite:lite-board.local.", "Lite Board", map[string]string{"type": "LAN", "deviceId": "a1b2c3d4e5f6"}),
		},
	}

	merged := c.MergedDevices()
	if len(merged) != 1 || len(merged[0].Externals) != 2 {
		t.Fatalf("MergedDevices() = %+v, want one entry with both transports", merged)
	}
}

// The display name is not an identity: two boards can share one.
func TestMergedDevices_LiteSameNameDifferentBoards(t *testing.T) {
	c := &DevicesCollection{
		ExternalDevices: []ExternalDevice{
			liteExternal("wendy-lite:/dev/cu.usbmodem2101", "Wendy Lite", map[string]string{"type": "USB", "deviceId": "a1b2c3d4e5f6"}),
			liteExternal("wendy-lite:/dev/cu.usbmodem2201", "Wendy Lite", map[string]string{"type": "USB", "deviceId": "0f1e2d3c4b5a"}),
		},
	}

	if merged := c.MergedDevices(); len(merged) != 2 {
		t.Fatalf("MergedDevices() returned %d entries, want 2", len(merged))
	}
}

// LiteDedupID is a case-sensitive id, so the merge compares it exactly.
func TestMergedDevices_LiteIDsDifferingOnlyInCase(t *testing.T) {
	c := &DevicesCollection{
		ExternalDevices: []ExternalDevice{
			liteExternal("wendy-lite:/dev/cu.usbmodem2101", "Lite Board", map[string]string{"type": "USB", "deviceId": "a1b2c3d4e5f6"}),
			liteExternal("wendy-lite:lite-board.local.", "Lite Board", map[string]string{"type": "LAN", "deviceId": "A1B2C3D4E5F6"}),
		},
	}

	if merged := c.MergedDevices(); len(merged) != 2 {
		t.Fatalf("MergedDevices() returned %d entries, want 2", len(merged))
	}
}

// An unflashed board has no id yet, so nothing may merge into its row.
func TestMergedDevices_LiteUnflashedStandsAlone(t *testing.T) {
	c := &DevicesCollection{
		ExternalDevices: []ExternalDevice{
			liteExternal("wendy-lite:/dev/cu.usbmodem2101", "ESP32 (unflashed)", map[string]string{"type": "USB", "needsInstall": "true"}),
			liteExternal("wendy-lite:/dev/cu.usbmodem2201", "ESP32 (unflashed)", map[string]string{"type": "USB", "needsInstall": "true"}),
		},
	}

	if merged := c.MergedDevices(); len(merged) != 2 {
		t.Fatalf("MergedDevices() returned %d entries, want 2", len(merged))
	}
}

func TestMergedDevices_LiteNotMergedIntoWendyOSDeviceWithSameName(t *testing.T) {
	c := &DevicesCollection{
		LANDevices: []LANDevice{
			{ID: "jetson", DisplayName: "Robot", Hostname: "robot.local.", IPAddress: "10.0.0.2"},
		},
		ExternalDevices: []ExternalDevice{
			liteExternal("wendy-lite:/dev/cu.usbmodem2101", "Robot", map[string]string{"type": "USB", "deviceId": "a1b2c3d4e5f6"}),
		},
	}

	merged := c.MergedDevices()
	if len(merged) != 2 {
		t.Fatalf("MergedDevices() returned %d entries, want 2", len(merged))
	}
	if len(merged[0].Externals) != 0 {
		t.Errorf("WendyOS LAN entry picked up Lite transports: %+v", merged[0].Externals)
	}
}

func TestExternalDevice_LiteDedupID(t *testing.T) {
	tests := []struct {
		name string
		info map[string]string
		want string
	}{
		{name: "enrolled id preferred", info: map[string]string{"deviceId": "a1b2c3d4e5f6", "enrolledDeviceId": "uuid-1"}, want: "uuid-1"},
		{name: "device id fallback", info: map[string]string{"deviceId": "a1b2c3d4e5f6"}, want: "a1b2c3d4e5f6"},
		{name: "empty enrolled id falls back", info: map[string]string{"deviceId": "a1b2c3d4e5f6", "enrolledDeviceId": ""}, want: "a1b2c3d4e5f6"},
		{name: "returned unchanged", info: map[string]string{"enrolledDeviceId": "Uuid-ABC"}, want: "Uuid-ABC"},
		{name: "no id", info: map[string]string{"needsInstall": "true"}, want: ""},
		{name: "nil map", info: nil, want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := (ExternalDevice{ConnectionInfo: tt.info}).LiteDedupID(); got != tt.want {
				t.Errorf("LiteDedupID() = %q, want %q", got, tt.want)
			}
		})
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
