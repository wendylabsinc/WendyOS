//go:build linux

package bluetooth

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"
	"go.uber.org/zap"

	"github.com/wendylabsinc/wendy/go/internal/agent/audio"
)

// deviceProps builds an org.bluez.Device1 property map for tests.
func deviceProps(address string, extra map[string]dbus.Variant) map[string]dbus.Variant {
	props := map[string]dbus.Variant{"Address": dbus.MakeVariant(address)}
	for k, v := range extra {
		props[k] = v
	}
	return props
}

func TestFindDeviceByAddress(t *testing.T) {
	adapterEntry := map[string]map[string]dbus.Variant{
		adapterIface: {"Address": dbus.MakeVariant("00:00:00:00:00:00")},
	}

	tests := []struct {
		name            string
		managed         managedObjects
		address         string
		wantFound       bool
		wantDevicePath  dbus.ObjectPath
		wantAdapterPath string
	}{
		{
			name: "exact case match under hci0",
			managed: managedObjects{
				"/org/bluez/hci0": adapterEntry,
				"/org/bluez/hci0/dev_AA_BB_CC_DD_EE_FF": {
					deviceIface: deviceProps("AA:BB:CC:DD:EE:FF", nil),
				},
			},
			address:         "AA:BB:CC:DD:EE:FF",
			wantFound:       true,
			wantDevicePath:  "/org/bluez/hci0/dev_AA_BB_CC_DD_EE_FF",
			wantAdapterPath: "/org/bluez/hci0",
		},
		{
			name: "lowercase query matches uppercase address",
			managed: managedObjects{
				"/org/bluez/hci0/dev_AA_BB_CC_DD_EE_FF": {
					deviceIface: deviceProps("AA:BB:CC:DD:EE:FF", nil),
				},
			},
			address:         "aa:bb:cc:dd:ee:ff",
			wantFound:       true,
			wantDevicePath:  "/org/bluez/hci0/dev_AA_BB_CC_DD_EE_FF",
			wantAdapterPath: "/org/bluez/hci0",
		},
		{
			name: "absent address not found",
			managed: managedObjects{
				"/org/bluez/hci0": adapterEntry,
				"/org/bluez/hci0/dev_AA_BB_CC_DD_EE_FF": {
					deviceIface: deviceProps("AA:BB:CC:DD:EE:FF", nil),
				},
			},
			address:   "11:22:33:44:55:66",
			wantFound: false,
		},
		{
			name: "device on hci1, adapter path from Adapter property",
			managed: managedObjects{
				"/org/bluez/hci0": adapterEntry,
				"/org/bluez/hci1": adapterEntry,
				"/org/bluez/hci1/dev_AA_BB_CC_DD_EE_FF": {
					deviceIface: deviceProps("AA:BB:CC:DD:EE:FF", map[string]dbus.Variant{
						"Adapter": dbus.MakeVariant(dbus.ObjectPath("/org/bluez/hci1")),
					}),
				},
			},
			address:         "AA:BB:CC:DD:EE:FF",
			wantFound:       true,
			wantDevicePath:  "/org/bluez/hci1/dev_AA_BB_CC_DD_EE_FF",
			wantAdapterPath: "/org/bluez/hci1",
		},
		{
			name: "adapter path falls back to object-path parent",
			managed: managedObjects{
				"/org/bluez/hci2/dev_AA_BB_CC_DD_EE_FF": {
					deviceIface: deviceProps("AA:BB:CC:DD:EE:FF", nil),
				},
			},
			address:         "AA:BB:CC:DD:EE:FF",
			wantFound:       true,
			wantDevicePath:  "/org/bluez/hci2/dev_AA_BB_CC_DD_EE_FF",
			wantAdapterPath: "/org/bluez/hci2",
		},
		{
			name: "duplicate device on two adapters picks lowest path",
			managed: managedObjects{
				"/org/bluez/hci1/dev_AA_BB_CC_DD_EE_FF": {
					deviceIface: deviceProps("AA:BB:CC:DD:EE:FF", nil),
				},
				"/org/bluez/hci0/dev_AA_BB_CC_DD_EE_FF": {
					deviceIface: deviceProps("AA:BB:CC:DD:EE:FF", nil),
				},
			},
			address:         "AA:BB:CC:DD:EE:FF",
			wantFound:       true,
			wantDevicePath:  "/org/bluez/hci0/dev_AA_BB_CC_DD_EE_FF",
			wantAdapterPath: "/org/bluez/hci0",
		},
		{
			name: "non-device objects are ignored",
			managed: managedObjects{
				"/org/bluez/hci0": {
					adapterIface: {"Address": dbus.MakeVariant("AA:BB:CC:DD:EE:FF")},
				},
			},
			address:   "AA:BB:CC:DD:EE:FF",
			wantFound: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			devicePath, adapterPath, props, found := findDeviceByAddress(tt.managed, tt.address)
			if found != tt.wantFound {
				t.Fatalf("found = %v, want %v", found, tt.wantFound)
			}
			if !found {
				return
			}
			if devicePath != tt.wantDevicePath {
				t.Errorf("devicePath = %q, want %q", devicePath, tt.wantDevicePath)
			}
			if adapterPath != tt.wantAdapterPath {
				t.Errorf("adapterPath = %q, want %q", adapterPath, tt.wantAdapterPath)
			}
			if props == nil {
				t.Error("props = nil, want the device's property map")
			}
		})
	}
}

func TestRestrictToAdapter(t *testing.T) {
	managed := managedObjects{
		"/org/bluez/hci0": {adapterIface: {}},
		"/org/bluez/hci0/dev_AA_BB_CC_DD_EE_FF": {
			deviceIface: deviceProps("AA:BB:CC:DD:EE:FF", nil),
		},
		"/org/bluez/hci1": {adapterIface: {}},
		"/org/bluez/hci1/dev_AA_BB_CC_DD_EE_FF": {
			deviceIface: deviceProps("AA:BB:CC:DD:EE:FF", nil),
		},
	}

	t.Run("empty restriction returns input unchanged", func(t *testing.T) {
		if got := restrictToAdapter(managed, ""); len(got) != len(managed) {
			t.Errorf("got %d objects, want %d", len(got), len(managed))
		}
	})

	t.Run("restriction pins device lookup to the given adapter", func(t *testing.T) {
		restricted := restrictToAdapter(managed, "/org/bluez/hci1")
		devicePath, adapterPath, _, found := findDeviceByAddress(restricted, "AA:BB:CC:DD:EE:FF")
		if !found {
			t.Fatal("device should be found on the restricted adapter")
		}
		if devicePath != "/org/bluez/hci1/dev_AA_BB_CC_DD_EE_FF" || adapterPath != "/org/bluez/hci1" {
			t.Errorf("got (%q, %q), want the hci1 device", devicePath, adapterPath)
		}
	})
}

func TestConnectFailureErrorPrefersNotFound(t *testing.T) {
	m := &BlueZManager{logger: zap.NewNop()}
	pairErr := dbus.Error{Name: "org.bluez.Error.Failed", Body: []any{"br-connection-unknown"}}
	goneErr := dbus.Error{
		Name: "org.freedesktop.DBus.Error.UnknownMethod",
		Body: []any{`Method "Connect" with signature "" on interface "org.bluez.Device1" doesn't exist`},
	}

	t.Run("device gone during connect reports NotFound despite pair failure", func(t *testing.T) {
		err := m.connectFailureError("AA:BB:CC:DD:EE:FF", pairErr, goneErr)
		if !errors.Is(err, ErrDeviceNotFound) {
			t.Fatalf("err = %v, want ErrDeviceNotFound wrap", err)
		}
	})

	t.Run("pair failure is primary for ordinary connect failures", func(t *testing.T) {
		connErr := dbus.Error{Name: "org.bluez.Error.Failed", Body: []any{"br-connection-refused"}}
		err := m.connectFailureError("AA:BB:CC:DD:EE:FF", pairErr, connErr)
		if !strings.Contains(err.Error(), "pairing") {
			t.Errorf("err = %q, want the pairing error reported as primary", err.Error())
		}
	})

	t.Run("no pair error reports the connect failure", func(t *testing.T) {
		connErr := dbus.Error{Name: "org.bluez.Error.Failed", Body: []any{"br-connection-refused"}}
		err := m.connectFailureError("AA:BB:CC:DD:EE:FF", nil, connErr)
		if !strings.Contains(err.Error(), "refused") {
			t.Errorf("err = %q, want the connect failure text", err.Error())
		}
	})
}

func TestRetryConnect(t *testing.T) {
	m := &BlueZManager{logger: zap.NewNop()}
	const testDelay = time.Millisecond

	t.Run("succeeds on first attempt without retrying", func(t *testing.T) {
		calls := 0
		err := m.retryConnect(context.Background(), testDelay, func() error {
			calls++
			return nil
		})
		if err != nil {
			t.Fatalf("err = %v, want nil", err)
		}
		if calls != 1 {
			t.Errorf("calls = %d, want 1", calls)
		}
	})

	t.Run("retries a transient failure and succeeds", func(t *testing.T) {
		calls := 0
		err := m.retryConnect(context.Background(), testDelay, func() error {
			calls++
			if calls < 3 {
				return dbus.Error{Name: "org.bluez.Error.Failed", Body: []any{"br-connection-unknown"}}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("err = %v, want nil after retries", err)
		}
		if calls != 3 {
			t.Errorf("calls = %d, want 3", calls)
		}
	})

	t.Run("does not retry a non-transient failure", func(t *testing.T) {
		calls := 0
		wantErr := dbus.Error{Name: "org.bluez.Error.AuthenticationRejected"}
		err := m.retryConnect(context.Background(), testDelay, func() error {
			calls++
			return wantErr
		})
		if calls != 1 {
			t.Errorf("calls = %d, want 1 (no retry for a non-transient error)", calls)
		}
		if err == nil {
			t.Fatal("want the non-transient error returned")
		}
	})

	t.Run("gives up after the max attempts", func(t *testing.T) {
		calls := 0
		err := m.retryConnect(context.Background(), testDelay, func() error {
			calls++
			return dbus.Error{Name: "org.bluez.Error.InProgress"}
		})
		if calls != maxConnectAttempts {
			t.Errorf("calls = %d, want %d", calls, maxConnectAttempts)
		}
		if err == nil {
			t.Fatal("want an error after exhausting retries")
		}
	})

	t.Run("stops early when the context is done", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		calls := 0
		err := m.retryConnect(ctx, time.Hour, func() error {
			calls++
			cancel()
			return dbus.Error{Name: "org.bluez.Error.InProgress"}
		})
		if calls != 1 {
			t.Errorf("calls = %d, want 1 (context canceled before the retry delay elapses)", calls)
		}
		if err == nil {
			t.Fatal("want an error")
		}
	})
}

func TestIncludePeripheral(t *testing.T) {
	tests := []struct {
		name  string
		props map[string]dbus.Variant
		want  bool
	}{
		{"paired only", map[string]dbus.Variant{"Paired": dbus.MakeVariant(true)}, true},
		{"connected only", map[string]dbus.Variant{"Connected": dbus.MakeVariant(true)}, true},
		{"trusted only", map[string]dbus.Variant{"Trusted": dbus.MakeVariant(true)}, true},
		{"rssi present only", map[string]dbus.Variant{"RSSI": dbus.MakeVariant(int16(-60))}, true},
		{"stale cache entry", map[string]dbus.Variant{
			"Name":   dbus.MakeVariant("Old Speaker"),
			"Paired": dbus.MakeVariant(false),
		}, false},
		{"empty props", map[string]dbus.Variant{}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := includePeripheral(tt.props); got != tt.want {
				t.Errorf("includePeripheral = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestShouldListPeripheral(t *testing.T) {
	bare := map[string]dbus.Variant{"Name": dbus.MakeVariant("Mystery")}
	withRSSI := map[string]dbus.Variant{"RSSI": dbus.MakeVariant(int16(-50))}
	paired := map[string]dbus.Variant{"Paired": dbus.MakeVariant(true)}

	tests := []struct {
		name        string
		props       map[string]dbus.Variant
		preexisting bool
		want        bool
	}{
		// A device object that appeared during this discovery is always listed —
		// RSSI is an optional BlueZ property and must not gate fresh devices.
		{"new device without RSSI", bare, false, true},
		{"new device with RSSI", withRSSI, false, true},
		// Pre-existing cache entries need a presence marker.
		{"stale cache entry", bare, true, false},
		{"cached but re-seen (RSSI)", withRSSI, true, true},
		{"cached and paired", paired, true, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := shouldListPeripheral(tt.props, tt.preexisting); got != tt.want {
				t.Errorf("shouldListPeripheral(preexisting=%v) = %v, want %v", tt.preexisting, got, tt.want)
			}
		})
	}
}

func TestDevicePathsUnder(t *testing.T) {
	managed := managedObjects{
		"/org/bluez/hci0": {adapterIface: {}},
		"/org/bluez/hci0/dev_AA_BB_CC_DD_EE_FF": {
			deviceIface: deviceProps("AA:BB:CC:DD:EE:FF", nil),
		},
		"/org/bluez/hci1/dev_11_22_33_44_55_66": {
			deviceIface: deviceProps("11:22:33:44:55:66", nil),
		},
	}
	got := devicePathsUnder(managed, "/org/bluez/hci0")
	if len(got) != 1 || !got["/org/bluez/hci0/dev_AA_BB_CC_DD_EE_FF"] {
		t.Errorf("devicePathsUnder = %v, want only the hci0 device", got)
	}
}

func TestResolveAdapterPath(t *testing.T) {
	adapterEntry := map[string]map[string]dbus.Variant{adapterIface: {}}

	t.Run("env override wins verbatim", func(t *testing.T) {
		t.Setenv("WENDY_BT_ADAPTER", "/org/bluez/hci7")
		got, err := resolveAdapterPath(managedObjects{})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != "/org/bluez/hci7" {
			t.Errorf("path = %q, want /org/bluez/hci7", got)
		}
	})

	t.Run("lowest Adapter1 path wins", func(t *testing.T) {
		t.Setenv("WENDY_BT_ADAPTER", "")
		managed := managedObjects{
			"/org/bluez/hci1": adapterEntry,
			"/org/bluez/hci0": adapterEntry,
			"/org/bluez/hci0/dev_AA_BB_CC_DD_EE_FF": {
				deviceIface: deviceProps("AA:BB:CC:DD:EE:FF", nil),
			},
		}
		got, err := resolveAdapterPath(managed)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != "/org/bluez/hci0" {
			t.Errorf("path = %q, want /org/bluez/hci0", got)
		}
	})

	t.Run("no adapter is an error", func(t *testing.T) {
		t.Setenv("WENDY_BT_ADAPTER", "")
		if _, err := resolveAdapterPath(managedObjects{}); err == nil {
			t.Fatal("expected an error when no adapter implements org.bluez.Adapter1")
		}
	})
}

func TestDbusErrorInfo(t *testing.T) {
	dbErr := dbus.Error{
		Name: "org.freedesktop.DBus.Error.UnknownMethod",
		Body: []any{`Method "Pair" with signature "" on interface "org.bluez.Device1" doesn't exist`},
	}

	t.Run("plain dbus.Error", func(t *testing.T) {
		name, message, ok := dbusErrorInfo(dbErr)
		if !ok || name != dbErr.Name || message != dbErr.Body[0].(string) {
			t.Fatalf("got (%q, %q, %v)", name, message, ok)
		}
	})

	t.Run("wrapped dbus.Error", func(t *testing.T) {
		name, _, ok := dbusErrorInfo(fmt.Errorf("pairing with X: %w", dbErr))
		if !ok || name != dbErr.Name {
			t.Fatalf("got (%q, %v)", name, ok)
		}
	})

	t.Run("pointer dbus.Error", func(t *testing.T) {
		name, _, ok := dbusErrorInfo(dbus.NewError("org.bluez.Error.Failed", []any{"br-connection-unknown"}))
		if !ok || name != "org.bluez.Error.Failed" {
			t.Fatalf("got (%q, %v)", name, ok)
		}
	})

	t.Run("non-dbus error", func(t *testing.T) {
		if _, _, ok := dbusErrorInfo(errors.New("plain")); ok {
			t.Fatal("expected ok=false for a non-dbus error")
		}
	})
}

func TestWrapBluetoothError(t *testing.T) {
	m := &BlueZManager{logger: zap.NewNop()}

	t.Run("missing object wraps ErrDeviceNotFound", func(t *testing.T) {
		err := m.wrapBluetoothError("pairing with", "AA:BB:CC:DD:EE:FF", dbus.Error{
			Name: "org.freedesktop.DBus.Error.UnknownMethod",
			Body: []any{`Method "Pair" with signature "" on interface "org.bluez.Device1" doesn't exist`},
		})
		if !errors.Is(err, ErrDeviceNotFound) {
			t.Fatalf("err = %v, want ErrDeviceNotFound wrap", err)
		}
	})

	t.Run("bearer failure becomes friendly text", func(t *testing.T) {
		err := m.wrapBluetoothError("connecting to", "AA:BB:CC:DD:EE:FF", dbus.Error{
			Name: "org.bluez.Error.Failed",
			Body: []any{"br-connection-unknown"},
		})
		if errors.Is(err, ErrDeviceNotFound) {
			t.Fatal("bearer failure must not be ErrDeviceNotFound")
		}
		if msg := err.Error(); !strings.Contains(msg, "pairing mode") {
			t.Errorf("err = %q, want pairing-mode hint", msg)
		}
	})

	t.Run("unclassified error keeps raw text", func(t *testing.T) {
		raw := errors.New("connection reset by peer")
		err := m.wrapBluetoothError("connecting to", "AA:BB:CC:DD:EE:FF", raw)
		if !strings.Contains(err.Error(), "connection reset by peer") {
			t.Errorf("err = %q, want raw text preserved", err.Error())
		}
	})
}

func TestDeviceFromPropsIgnoresBlueZDefaultAlias(t *testing.T) {
	// BlueZ sets Alias to the address (':' -> '-') when a device has never
	// advertised a real name. That must not surface as a Name, or every
	// anonymous device sorts as if it were named.
	props := deviceProps("03:F8:5C:73:77:6B", map[string]dbus.Variant{
		"Alias": dbus.MakeVariant("03-F8-5C-73-77-6B"),
	})
	p := deviceFromProps(props)
	if p.Name != "" {
		t.Fatalf("Name = %q, want empty for BlueZ default alias", p.Name)
	}
}

func TestDeviceFromPropsKeepsRealAlias(t *testing.T) {
	props := deviceProps("40:C1:F6:E2:53:24", map[string]dbus.Variant{
		"Alias": dbus.MakeVariant("JBL Flip 5"),
	})
	p := deviceFromProps(props)
	if p.Name != "JBL Flip 5" {
		t.Fatalf("Name = %q, want %q", p.Name, "JBL Flip 5")
	}
}

func TestDeviceFromPropsFallsBackToNameWhenAliasIsDefault(t *testing.T) {
	props := deviceProps("AA:BB:CC:DD:EE:FF", map[string]dbus.Variant{
		"Alias": dbus.MakeVariant("AA-BB-CC-DD-EE-FF"),
		"Name":  dbus.MakeVariant("Real Advertised Name"),
	})
	p := deviceFromProps(props)
	if p.Name != "Real Advertised Name" {
		t.Fatalf("Name = %q, want fallback to Name property", p.Name)
	}
}

func TestIsDefaultAlias(t *testing.T) {
	tests := []struct {
		alias, address string
		want           bool
	}{
		{"AA-BB-CC-DD-EE-FF", "AA:BB:CC:DD:EE:FF", true},
		{"aa-bb-cc-dd-ee-ff", "AA:BB:CC:DD:EE:FF", true},
		{"JBL Flip 5", "AA:BB:CC:DD:EE:FF", false},
		{"AA-BB-CC-DD-EE-FF", "", false},
	}
	for _, tt := range tests {
		if got := isDefaultAlias(tt.alias, tt.address); got != tt.want {
			t.Errorf("isDefaultAlias(%q, %q) = %v, want %v", tt.alias, tt.address, got, tt.want)
		}
	}
}

func TestIsHIDDevice(t *testing.T) {
	tests := []struct {
		name  string
		props map[string]dbus.Variant
		want  bool
	}{
		{"gamepad icon", map[string]dbus.Variant{"Icon": dbus.MakeVariant("input-gaming")}, true},
		{"keyboard icon", map[string]dbus.Variant{"Icon": dbus.MakeVariant("input-keyboard")}, true},
		{"hid service uuid", map[string]dbus.Variant{
			"UUIDs": dbus.MakeVariant([]string{"0000180f-0000-1000-8000-00805f9b34fb", hidServiceUUID}),
		}, true},
		{"classic hid service uuid", map[string]dbus.Variant{
			"UUIDs": dbus.MakeVariant([]string{classicHIDServiceUUID}),
		}, true},
		{"uppercase uuid", map[string]dbus.Variant{
			"UUIDs": dbus.MakeVariant([]string{strings.ToUpper(hidServiceUUID)}),
		}, true},
		{"speaker", map[string]dbus.Variant{
			"Icon":  dbus.MakeVariant("audio-headset"),
			"UUIDs": dbus.MakeVariant([]string{"0000110b-0000-1000-8000-00805f9b34fb"}),
		}, false},
		{"no hints", map[string]dbus.Variant{}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isHIDDevice(tt.props); got != tt.want {
				t.Fatalf("isHIDDevice() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestAudioProfileUUID(t *testing.T) {
	const (
		hfp  = "0000111e-0000-1000-8000-00805f9b34fb"
		avrc = "0000110e-0000-1000-8000-00805f9b34fb"
	)
	tests := []struct {
		name  string
		uuids []string
		want  string
	}{
		// Speakers and headsets advertise a sink; that is the direction we want
		// even when they also advertise a source, which is what made a Bose
		// SoundLink claim our sink endpoint and land on the audio-gateway profile.
		{"sink only", []string{a2dpSinkUUID, avrc}, a2dpSinkUUID},
		{"sink and source", []string{a2dpSourceUUID, a2dpSinkUUID, hfp}, a2dpSinkUUID},
		{"sink and source, source listed first", []string{a2dpSourceUUID, a2dpSinkUUID}, a2dpSinkUUID},
		// A microphone has no sink role to offer, so the fallback picks it up
		// without needing to inspect the class of device.
		{"source only", []string{a2dpSourceUUID, avrc}, a2dpSourceUUID},
		// Non-audio peripherals fall back to a whole-device Connect.
		{"no audio profiles", []string{avrc, hfp}, ""},
		{"empty", nil, ""},
		// BlueZ reports lowercase, but the property is not guaranteed to be.
		{"uppercase", []string{strings.ToUpper(a2dpSinkUUID)}, a2dpSinkUUID},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := audioProfileUUID(tt.uuids); got != tt.want {
				t.Errorf("audioProfileUUID(%v) = %q, want %q", tt.uuids, got, tt.want)
			}
		})
	}
}

func TestWaitForInputDevice(t *testing.T) {
	root := t.TempDir()
	restore := inputDeviceUniqGlob
	inputDeviceUniqGlob = root + "/input*/uniq"
	t.Cleanup(func() { inputDeviceUniqGlob = restore })

	writeUniq := func(dir, uniq string) {
		t.Helper()
		if err := os.MkdirAll(root+"/"+dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(root+"/"+dir+"/uniq", []byte(uniq+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// Absent (only an unrelated device): the wait must time out.
	writeUniq("input1", "aa:aa:aa:aa:aa:aa")
	ctx := context.Background()
	if waitForInputDevice(ctx, "28:EA:0B:EF:B6:51", 10*time.Millisecond) {
		t.Fatal("waitForInputDevice must report false when no matching uniq exists")
	}

	// The kernel stores the address lowercase; the query arrives uppercase.
	writeUniq("input2", "28:ea:0b:ef:b6:51")
	if !waitForInputDevice(ctx, "28:EA:0B:EF:B6:51", 10*time.Millisecond) {
		t.Fatal("waitForInputDevice must match uniq case-insensitively")
	}

	// A canceled context stops the wait promptly.
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if waitForInputDevice(canceled, "bb:bb:bb:bb:bb:bb", time.Minute) {
		t.Fatal("waitForInputDevice must report false on context cancellation")
	}
}

func TestStringsProp(t *testing.T) {
	props := map[string]dbus.Variant{
		"UUIDs":  dbus.MakeVariant([]string{a2dpSinkUUID}),
		"Paired": dbus.MakeVariant(true),
	}
	if got := stringsProp(props, "UUIDs"); len(got) != 1 || got[0] != a2dpSinkUUID {
		t.Errorf("stringsProp(UUIDs) = %v", got)
	}
	// Wrong type and missing key both yield nil rather than panicking: BlueZ
	// omits UUIDs entirely for a device it has only seen advertising.
	if got := stringsProp(props, "Paired"); got != nil {
		t.Errorf("stringsProp on non-slice = %v, want nil", got)
	}
	if got := stringsProp(props, "Missing"); got != nil {
		t.Errorf("stringsProp on missing key = %v, want nil", got)
	}
}

func TestClaimBootReconnect(t *testing.T) {
	dir := t.TempDir()
	orig := bootReconnectMarker
	t.Cleanup(func() { bootReconnectMarker = orig })
	bootReconnectMarker = filepath.Join(dir, "marker")

	logger := zap.NewNop()
	// The agent restarts for upgrades and crash recovery on machines that stay
	// powered for weeks. Only the first start after a boot may walk the device
	// list; later ones must be no-ops so the radio is not paged repeatedly and
	// a deliberate disconnect is not undone.
	if !claimBootReconnect(logger) {
		t.Fatal("first claim should succeed")
	}
	for i := range 3 {
		if claimBootReconnect(logger) {
			t.Errorf("claim %d after the first should fail", i+2)
		}
	}

	// /run is tmpfs, so a real reboot clears the marker and the next start
	// claims it again.
	if err := os.Remove(bootReconnectMarker); err != nil {
		t.Fatal(err)
	}
	if !claimBootReconnect(logger) {
		t.Error("claim after reboot (marker cleared) should succeed")
	}
}

func TestClaimBootReconnectUnwritable(t *testing.T) {
	orig := bootReconnectMarker
	t.Cleanup(func() { bootReconnectMarker = orig })
	// An unwritable location must not panic or claim; the reconnect is simply
	// skipped rather than taking down agent startup.
	bootReconnectMarker = filepath.Join(t.TempDir(), "no-such-dir", "marker")
	if claimBootReconnect(zap.NewNop()) {
		t.Error("claim should fail when the marker cannot be created")
	}
}

func TestWaitForAudioSession(t *testing.T) {
	origAvailable, origTimeout := audio.Available, audioSessionTimeout
	t.Cleanup(func() { audio.Available, audioSessionTimeout = origAvailable, origTimeout })

	// Present already: returns immediately, which is the agent-restart case on
	// a machine whose audio has been up for weeks. (What counts as a session —
	// a listening socket owned by the wendy user, not a plain file — is the
	// audio package's contract, covered by its own RuntimeDir tests; here the
	// seam is stubbed because no test environment can own a socket as wendy.)
	audio.Available = func() bool { return true }
	if !waitForAudioSession(context.Background()) {
		t.Error("should report ready when the session is already up")
	}

	// Appears later: the wait polls until the session comes up.
	probes := 0
	audio.Available = func() bool { probes++; return probes >= 2 }
	audioSessionTimeout = time.Minute
	if !waitForAudioSession(context.Background()) {
		t.Error("should become ready once the session appears")
	}
	if probes < 2 {
		t.Errorf("expected repeated probes, got %d", probes)
	}

	// Cancellation is the only false: the reconnect is skipped only when the
	// agent is shutting down.
	audio.Available = func() bool { return false }
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if waitForAudioSession(ctx) {
		t.Error("cancelled context should report not-ready")
	}

	// A timeout still proceeds, so a board with no working audio stack still
	// attempts the reconnect.
	audioSessionTimeout = 0
	if !waitForAudioSession(context.Background()) {
		t.Error("timeout should proceed anyway rather than skip the reconnect")
	}
}

func TestNewManager_WithLinkReporter(t *testing.T) {
	r := fakeLinkReporter{}
	m, ok := NewManager(zap.NewNop(), WithLinkReporter(r)).(*BlueZManager)
	if !ok {
		t.Fatal("NewManager did not return a *BlueZManager")
	}
	if _, ok := m.links.(fakeLinkReporter); !ok {
		t.Errorf("manager links = %T; want the reporter passed in", m.links)
	}
}
