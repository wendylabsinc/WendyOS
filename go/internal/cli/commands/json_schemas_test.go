package commands

import (
	"encoding/json"
	"testing"

	"github.com/wendylabsinc/wendy/internal/shared/models"
	agentpb "github.com/wendylabsinc/wendy/proto/gen/agentpb"
)

// ---------- device apps list --json ----------

// TestAppsListJSON verifies the JSON schema produced by `wendy device apps list --json`.
// The command transforms proto AppContainer messages into a custom struct with
// camelCase field names and omitempty semantics.
func TestAppsListJSON_Schema(t *testing.T) {
	// Replicate the inline jsonApp struct from appsListAgent.
	type jsonApp struct {
		Name         string `json:"name"`
		Version      string `json:"version,omitempty"`
		RunningState string `json:"runningState,omitempty"`
		FailureCount uint32 `json:"failureCount,omitempty"`
	}

	apps := []jsonApp{
		{Name: "hello-app", Version: "1.0.0", RunningState: "RUNNING", FailureCount: 0},
		{Name: "broken-app", RunningState: "STOPPED", FailureCount: 5},
	}

	data, err := json.MarshalIndent(apps, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var parsed []map[string]interface{}
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(parsed) != 2 {
		t.Fatalf("expected 2 apps, got %d", len(parsed))
	}

	// First app: all fields present.
	app0 := parsed[0]
	for _, field := range []string{"name", "runningState"} {
		if _, ok := app0[field]; !ok {
			t.Errorf("app[0] missing field %q", field)
		}
	}
	if app0["name"] != "hello-app" {
		t.Errorf("app[0].name = %v; want hello-app", app0["name"])
	}
	if app0["runningState"] != "RUNNING" {
		t.Errorf("app[0].runningState = %v; want RUNNING", app0["runningState"])
	}
	// failureCount=0 must be omitted (omitempty).
	if _, ok := app0["failureCount"]; ok {
		t.Error("app[0].failureCount=0 should be omitted by omitempty")
	}
	// version is present when non-empty.
	if _, ok := app0["version"]; !ok {
		t.Error("app[0].version should be present when non-empty")
	}

	// Second app: version absent, failureCount present.
	app1 := parsed[1]
	if _, ok := app1["version"]; ok {
		t.Error("app[1].version should be omitted when empty")
	}
	if fc, ok := app1["failureCount"]; !ok {
		t.Error("app[1].failureCount=5 should be present")
	} else if fc != float64(5) {
		t.Errorf("app[1].failureCount = %v; want 5", fc)
	}
}

func TestAppsListJSON_EmptyArray(t *testing.T) {
	type jsonApp struct {
		Name         string `json:"name"`
		Version      string `json:"version,omitempty"`
		RunningState string `json:"runningState,omitempty"`
		FailureCount uint32 `json:"failureCount,omitempty"`
	}

	apps := []jsonApp{}
	data, err := json.MarshalIndent(apps, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(data) != "[]" {
		t.Errorf("empty array should serialize as [], got %q", data)
	}
}

// ---------- device volumes list --json ----------

// TestVolumesListJSON verifies the JSON schema produced by `wendy device volumes list --json`.
func TestVolumesListJSON_Schema(t *testing.T) {
	// Replicate the inline jsonVolume struct from newVolumesListCmd.
	type jsonVolume struct {
		Name      string   `json:"name"`
		Path      string   `json:"path"`
		SizeBytes int64    `json:"sizeBytes"`
		Size      string   `json:"size"`
		CreatedAt string   `json:"createdAt"`
		UsedBy    []string `json:"usedBy"`
	}

	vols := []jsonVolume{
		{
			Name:      "myapp-data",
			Path:      "/var/lib/wendy/volumes/myapp-data",
			SizeBytes: 1024 * 1024 * 5, // 5 MB
			Size:      "5.0 MB",
			CreatedAt: "2026-01-15T10:30:00Z",
			UsedBy:    []string{"myapp"},
		},
		{
			Name:      "shared-vol",
			Path:      "/var/lib/wendy/volumes/shared-vol",
			SizeBytes: 0,
			Size:      "0 B",
			CreatedAt: "2026-02-01T08:00:00Z",
			UsedBy:    nil,
		},
	}

	data, err := json.MarshalIndent(vols, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var parsed []map[string]interface{}
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(parsed) != 2 {
		t.Fatalf("expected 2 volumes, got %d", len(parsed))
	}

	// Verify all required fields are present in the first volume.
	vol0 := parsed[0]
	for _, field := range []string{"name", "path", "sizeBytes", "size", "createdAt", "usedBy"} {
		if _, ok := vol0[field]; !ok {
			t.Errorf("vol[0] missing field %q", field)
		}
	}
	if vol0["name"] != "myapp-data" {
		t.Errorf("vol[0].name = %v; want myapp-data", vol0["name"])
	}
	if vol0["sizeBytes"] != float64(1024*1024*5) {
		t.Errorf("vol[0].sizeBytes = %v; want %d", vol0["sizeBytes"], 1024*1024*5)
	}
	usedBy, ok := vol0["usedBy"].([]interface{})
	if !ok {
		t.Fatalf("vol[0].usedBy should be an array, got %T", vol0["usedBy"])
	}
	if len(usedBy) != 1 || usedBy[0] != "myapp" {
		t.Errorf("vol[0].usedBy = %v; want [myapp]", usedBy)
	}

	// Second volume: nil usedBy serializes as null (not omitted, since no omitempty).
	vol1 := parsed[1]
	if _, ok := vol1["usedBy"]; !ok {
		t.Error("vol[1].usedBy should be present (no omitempty on this field)")
	}
}

// ---------- device wifi status --json ----------

// TestWifiStatusJSON verifies the JSON schema produced by `wendy device wifi status --json`.
func TestWifiStatusJSON_Connected(t *testing.T) {
	output := map[string]interface{}{
		"connected": true,
		"ssid":      "HomeNetwork",
	}

	data, err := json.MarshalIndent(output, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var parsed map[string]interface{}
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if connected, ok := parsed["connected"].(bool); !ok || !connected {
		t.Errorf("connected = %v; want true", parsed["connected"])
	}
	if ssid, ok := parsed["ssid"].(string); !ok || ssid != "HomeNetwork" {
		t.Errorf("ssid = %v; want HomeNetwork", parsed["ssid"])
	}
}

func TestWifiStatusJSON_Disconnected(t *testing.T) {
	output := map[string]interface{}{
		"connected": false,
		"ssid":      "",
	}

	data, err := json.MarshalIndent(output, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var parsed map[string]interface{}
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if connected, ok := parsed["connected"].(bool); !ok || connected {
		t.Errorf("connected = %v; want false", parsed["connected"])
	}
	// Both fields must be present regardless of value.
	if _, ok := parsed["ssid"]; !ok {
		t.Error("ssid field must be present even when empty")
	}
}

// ---------- device wifi list --json ----------

// TestWifiNetworksJSON verifies the JSON schema produced by `wendy device wifi list --json`.
// The command marshals []*agentpb.ListWiFiNetworksResponse_WiFiNetwork directly.
// Proto-generated JSON tags use snake_case (signal_strength, not signalStrength).
func TestWifiNetworksJSON_Schema(t *testing.T) {
	strength := int32(75)
	networks := []*agentpb.ListWiFiNetworksResponse_WiFiNetwork{
		{Ssid: "HomeWiFi", SignalStrength: &strength},
		{Ssid: "OfficeWiFi"}, // no signal strength
	}

	data, err := json.MarshalIndent(networks, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var parsed []map[string]interface{}
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(parsed) != 2 {
		t.Fatalf("expected 2 networks, got %d", len(parsed))
	}

	// First network: ssid and signal_strength present.
	net0 := parsed[0]
	if net0["ssid"] != "HomeWiFi" {
		t.Errorf("net[0].ssid = %v; want HomeWiFi", net0["ssid"])
	}
	if sig, ok := net0["signal_strength"]; !ok {
		t.Error("net[0].signal_strength should be present")
	} else if sig != float64(75) {
		t.Errorf("net[0].signal_strength = %v; want 75", sig)
	}

	// Second network: signal_strength absent (nil pointer → omitempty).
	net1 := parsed[1]
	if net1["ssid"] != "OfficeWiFi" {
		t.Errorf("net[1].ssid = %v; want OfficeWiFi", net1["ssid"])
	}
	if _, ok := net1["signal_strength"]; ok {
		t.Error("net[1].signal_strength should be absent (nil pointer, omitempty)")
	}
}

// ---------- hardware list --json ----------

// TestHardwareListJSON verifies the JSON schema produced by `wendy hardware list --json`.
// The command marshals []*agentpb.ListHardwareCapabilitiesResponse_HardwareCapability directly.
// Proto-generated JSON tags: category, device_path, description, properties.
func TestHardwareListJSON_Schema(t *testing.T) {
	caps := []*agentpb.ListHardwareCapabilitiesResponse_HardwareCapability{
		{
			Category:    "gpu",
			DevicePath:  "/dev/nvidia0",
			Description: "NVIDIA Jetson GPU",
			Properties:  map[string]string{"model": "Orin", "vram": "16GB"},
		},
		{
			Category:    "audio",
			DevicePath:  "/dev/snd/controlC0",
			Description: "HDA Audio",
		},
	}

	data, err := json.MarshalIndent(caps, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var parsed []map[string]interface{}
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(parsed) != 2 {
		t.Fatalf("expected 2 capabilities, got %d", len(parsed))
	}

	// First capability: all fields present.
	cap0 := parsed[0]
	if cap0["category"] != "gpu" {
		t.Errorf("cap[0].category = %v; want gpu", cap0["category"])
	}
	// Proto JSON tag is device_path (snake_case).
	if cap0["device_path"] != "/dev/nvidia0" {
		t.Errorf("cap[0].device_path = %v; want /dev/nvidia0", cap0["device_path"])
	}
	if cap0["description"] != "NVIDIA Jetson GPU" {
		t.Errorf("cap[0].description = %v; want NVIDIA Jetson GPU", cap0["description"])
	}
	props, ok := cap0["properties"].(map[string]interface{})
	if !ok {
		t.Fatalf("cap[0].properties should be an object, got %T", cap0["properties"])
	}
	if props["model"] != "Orin" {
		t.Errorf("cap[0].properties.model = %v; want Orin", props["model"])
	}

	// Second capability: no properties (omitted).
	cap1 := parsed[1]
	if _, ok := cap1["properties"]; ok {
		t.Error("cap[1].properties should be absent when nil (omitempty)")
	}
}

// ---------- audio list --json ----------

// TestAudioListJSON verifies the JSON schema produced by `wendy audio list --json`.
// The command marshals []*agentpb.AudioDevice directly.
// Proto-generated JSON tags: id, name, description, type, is_default.
func TestAudioListJSON_Schema(t *testing.T) {
	devices := []*agentpb.AudioDevice{
		{
			Id:          1,
			Name:        "HDA Intel PCH",
			Description: "Analog Stereo",
			Type:        agentpb.AudioDeviceType_AUDIO_DEVICE_TYPE_OUTPUT,
			IsDefault:   true,
		},
		{
			Id:   2,
			Name: "USB Microphone",
			Type: agentpb.AudioDeviceType_AUDIO_DEVICE_TYPE_INPUT,
		},
	}

	data, err := json.MarshalIndent(devices, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var parsed []map[string]interface{}
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(parsed) != 2 {
		t.Fatalf("expected 2 audio devices, got %d", len(parsed))
	}

	dev0 := parsed[0]
	if dev0["id"] != float64(1) {
		t.Errorf("dev[0].id = %v; want 1", dev0["id"])
	}
	if dev0["name"] != "HDA Intel PCH" {
		t.Errorf("dev[0].name = %v; want HDA Intel PCH", dev0["name"])
	}
	// Proto JSON tag is is_default (snake_case).
	if isDefault, ok := dev0["is_default"].(bool); !ok || !isDefault {
		t.Errorf("dev[0].is_default = %v; want true", dev0["is_default"])
	}

	// Second device: is_default absent (false + omitempty).
	dev1 := parsed[1]
	if _, ok := dev1["is_default"]; ok {
		t.Error("dev[1].is_default=false should be omitted (omitempty)")
	}
}

// ---------- discover --json ----------

// TestDiscoverCollectionJSON verifies the JSON schema produced by `wendy discover --json`.
// The DevicesCollection struct serializes all discovered device types.
func TestDiscoverCollectionJSON_AllArraysPresent(t *testing.T) {
	collection := &models.DevicesCollection{
		USBDevices: []models.USBDevice{
			{
				Name:          "ttyUSB0",
				DisplayName:   "WendyOS ESP32",
				VendorID:      "0x303a",
				ProductID:     "0x1001",
				IsWendyDevice: true,
				IsESP32:       true,
			},
		},
		LANDevices: []models.LANDevice{
			{
				ID:            "wendy-alpha",
				DisplayName:   "wendy-alpha",
				Hostname:      "wendy-alpha.local",
				IPAddress:     "192.168.1.10",
				Port:          50051,
				InterfaceType: "lan",
				IsWendyDevice: true,
				AgentVersion:  "2026.01.01-120000",
			},
		},
		BluetoothDevices: []models.BluetoothDevice{
			{
				ID:            "AA:BB:CC:DD:EE:FF",
				DisplayName:   "WendyOS BLE",
				Address:       "AA:BB:CC:DD:EE:FF",
				RSSI:          -65,
				IsWendyDevice: true,
			},
		},
		EthernetInterfaces: []models.EthernetInterface{
			{
				Name:        "eth0",
				DisplayName: "Ethernet eth0",
				IPAddress:   "10.0.0.5",
			},
		},
		ExternalDevices: []models.ExternalDevice{},
	}

	data, err := json.MarshalIndent(collection, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var parsed map[string]interface{}
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// Verify top-level array keys.
	for _, key := range []string{"usbDevices", "lanDevices", "bluetoothDevices", "ethernetDevices"} {
		if _, ok := parsed[key]; !ok {
			t.Errorf("missing top-level field %q", key)
		}
	}

	// Verify USB device fields.
	usbArr, ok := parsed["usbDevices"].([]interface{})
	if !ok || len(usbArr) != 1 {
		t.Fatalf("usbDevices should be array of 1, got %v", parsed["usbDevices"])
	}
	usb0 := usbArr[0].(map[string]interface{})
	for _, f := range []string{"name", "displayName", "vendorId", "productId", "isWendyDevice"} {
		if _, ok := usb0[f]; !ok {
			t.Errorf("usbDevices[0] missing field %q", f)
		}
	}
	if usb0["vendorId"] != "0x303a" {
		t.Errorf("usbDevices[0].vendorId = %v; want 0x303a", usb0["vendorId"])
	}

	// Verify LAN device fields.
	lanArr := parsed["lanDevices"].([]interface{})
	lan0 := lanArr[0].(map[string]interface{})
	for _, f := range []string{"id", "displayName", "hostname", "ipAddress", "port", "isWendyDevice"} {
		if _, ok := lan0[f]; !ok {
			t.Errorf("lanDevices[0] missing field %q", f)
		}
	}
	if lan0["ipAddress"] != "192.168.1.10" {
		t.Errorf("lanDevices[0].ipAddress = %v; want 192.168.1.10", lan0["ipAddress"])
	}

	// Verify Bluetooth device fields.
	btArr := parsed["bluetoothDevices"].([]interface{})
	bt0 := btArr[0].(map[string]interface{})
	for _, f := range []string{"id", "displayName", "address", "rssi", "isWendyDevice"} {
		if _, ok := bt0[f]; !ok {
			t.Errorf("bluetoothDevices[0] missing field %q", f)
		}
	}

	// Verify Ethernet interface fields.
	ethArr := parsed["ethernetDevices"].([]interface{})
	eth0 := ethArr[0].(map[string]interface{})
	for _, f := range []string{"name", "displayName"} {
		if _, ok := eth0[f]; !ok {
			t.Errorf("ethernetDevices[0] missing field %q", f)
		}
	}
}

func TestDiscoverCollectionJSON_EmptyCollection(t *testing.T) {
	collection := &models.DevicesCollection{}

	data, err := json.MarshalIndent(collection, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var parsed map[string]interface{}
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// Even empty collections should encode their keys (no omitempty on the core arrays; externalDevices may be omitted).
	for _, key := range []string{"usbDevices", "lanDevices", "bluetoothDevices", "ethernetDevices"} {
		if _, ok := parsed[key]; !ok {
			t.Errorf("empty collection missing field %q", key)
		}
	}
}

// ---------- formatBytes helper ----------

// TestFormatBytes_Units verifies the byte-size formatting used in volumes list output.
// formatBytes uses SI units (powers of 1000: kB, MB, GB).
func TestFormatBytes_Units(t *testing.T) {
	cases := []struct {
		bytes int64
		want  string
	}{
		{0, "0 B"},
		{500, "500 B"},
		{999, "999 B"},
		{1_000, "1.0 kB"},
		{1_500, "1.5 kB"},
		{1_000_000, "1.0 MB"},
		{1_500_000, "1.5 MB"},
		{1_000_000_000, "1.0 GB"},
		{2_500_000_000, "2.5 GB"},
	}

	for _, tc := range cases {
		t.Run(tc.want, func(t *testing.T) {
			got := formatBytes(tc.bytes)
			if got != tc.want {
				t.Errorf("formatBytes(%d) = %q; want %q", tc.bytes, got, tc.want)
			}
		})
	}
}
