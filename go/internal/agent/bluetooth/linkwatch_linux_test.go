//go:build linux

package bluetooth

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/godbus/dbus/v5"
)

func TestListHCIAdapters(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"hci1", "hci0", "hci0:64"} {
		if err := os.Mkdir(filepath.Join(dir, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	old := sysClassBluetooth
	sysClassBluetooth = dir
	t.Cleanup(func() { sysClassBluetooth = old })

	got, err := listHCIAdapters()
	if err != nil || !slices.Equal(got, []int{0, 1}) {
		t.Errorf("listHCIAdapters = %v, %v; want [0 1]", got, err)
	}
}

func TestHCITransportStoredParams(t *testing.T) {
	dir := t.TempDir()
	devDir := filepath.Join(dir, "11:22:33:44:55:66", "AA:BB:CC:DD:EE:FF")
	if err := os.MkdirAll(devDir, 0o700); err != nil {
		t.Fatal(err)
	}
	info := "[ConnectionParameters]\nMinInterval=6\nMaxInterval=6\nLatency=0\nTimeout=300\n"
	if err := os.WriteFile(filepath.Join(devDir, "info"), []byte(info), 0o600); err != nil {
		t.Fatal(err)
	}
	old := bluezStateDir
	bluezStateDir = dir
	t.Cleanup(func() { bluezStateDir = old })

	tr := &hciTransport{address: "11:22:33:44:55:66"}
	got, ok := tr.StoredParams("aa:bb:cc:dd:ee:ff") // lower case, as a caller may pass it
	if want := (connUpdate{IntervalMin: 6, IntervalMax: 6, Latency: 0, Timeout: 300}); !ok || got != want {
		t.Errorf("StoredParams = %+v, %v; want %+v, true", got, ok, want)
	}
	if _, ok := tr.StoredParams("00:00:00:00:00:01"); ok {
		t.Error("StoredParams for an unknown device: ok = true; want false")
	}
}

func TestClassifyHIDProps(t *testing.T) {
	tests := []struct {
		name            string
		props           map[string]dbus.Variant
		wantHID, wantOK bool
	}{
		{"gamepad icon", map[string]dbus.Variant{"Icon": dbus.MakeVariant("input-gaming")}, true, true},
		{"HID over GATT service", map[string]dbus.Variant{"UUIDs": dbus.MakeVariant([]string{hidServiceUUID})}, true, true},
		{"resolved, not HID", map[string]dbus.Variant{
			"UUIDs":            dbus.MakeVariant([]string{a2dpSinkUUID}),
			"ServicesResolved": dbus.MakeVariant(true),
		}, false, true},
		{"services not resolved yet", map[string]dbus.Variant{"ServicesResolved": dbus.MakeVariant(false)}, false, false},
	}
	for _, tt := range tests {
		if isHID, known := classifyHIDProps(tt.props); isHID != tt.wantHID || known != tt.wantOK {
			t.Errorf("%s: classifyHIDProps = %v, %v; want %v, %v", tt.name, isHID, known, tt.wantHID, tt.wantOK)
		}
	}
}
