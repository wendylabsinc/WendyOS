//go:build linux

package bluez

import (
	"errors"
	"fmt"
	"testing"

	"github.com/godbus/dbus/v5"
)

// tree builds a ManagedObjects fixture from a compact description.
func tree(entries ...objectEntry) ManagedObjects {
	managed := ManagedObjects{}
	for _, e := range entries {
		if managed[e.path] == nil {
			managed[e.path] = map[string]map[string]dbus.Variant{}
		}
		managed[e.path][e.iface] = e.props
	}
	return managed
}

type objectEntry struct {
	path  dbus.ObjectPath
	iface string
	props map[string]dbus.Variant
}

func adapter(path string) objectEntry {
	return objectEntry{dbus.ObjectPath(path), AdapterIface, map[string]dbus.Variant{}}
}

func device(path, address string, extra ...map[string]dbus.Variant) objectEntry {
	props := map[string]dbus.Variant{"Address": dbus.MakeVariant(address)}
	for _, m := range extra {
		for k, v := range m {
			props[k] = v
		}
	}
	return objectEntry{dbus.ObjectPath(path), DeviceIface, props}
}

func TestResolveAdapterPath(t *testing.T) {
	t.Run("lowest path wins", func(t *testing.T) {
		// Map order is randomized, so an unstable choice would show up as a
		// flake rather than a consistent failure. Run it enough to catch that.
		managed := tree(adapter("/org/bluez/hci1"), adapter("/org/bluez/hci0"), adapter("/org/bluez/hci2"))
		for i := 0; i < 50; i++ {
			got, err := ResolveAdapterPath(managed)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != "/org/bluez/hci0" {
				t.Fatalf("ResolveAdapterPath = %q, want /org/bluez/hci0", got)
			}
		}
	})

	t.Run("no adapter is an error", func(t *testing.T) {
		if _, err := ResolveAdapterPath(tree(device("/org/bluez/hci0/dev_AA", "AA:BB:CC:DD:EE:FF"))); err == nil {
			t.Fatal("expected an error when no object implements Adapter1")
		}
	})

	t.Run("env override by bare name", func(t *testing.T) {
		t.Setenv(AdapterEnvVar, "hci1")
		managed := tree(adapter("/org/bluez/hci0"), adapter("/org/bluez/hci1"))
		got, err := ResolveAdapterPath(managed)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != "/org/bluez/hci1" {
			t.Fatalf("ResolveAdapterPath = %q, want /org/bluez/hci1", got)
		}
	})

	t.Run("env override by full path", func(t *testing.T) {
		t.Setenv(AdapterEnvVar, "/org/bluez/hci1")
		managed := tree(adapter("/org/bluez/hci0"), adapter("/org/bluez/hci1"))
		got, err := ResolveAdapterPath(managed)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != "/org/bluez/hci1" {
			t.Fatalf("ResolveAdapterPath = %q, want /org/bluez/hci1", got)
		}
	})

	t.Run("env override naming a missing adapter is an error", func(t *testing.T) {
		// The agent's older copy returns the override verbatim here, which
		// defers the failure to a worse message later.
		t.Setenv(AdapterEnvVar, "hci9")
		if _, err := ResolveAdapterPath(tree(adapter("/org/bluez/hci0"))); err == nil {
			t.Fatal("expected an error for an override that matches no adapter")
		}
	})

	t.Run("env override does not match a non-adapter object", func(t *testing.T) {
		t.Setenv(AdapterEnvVar, "dev_AA")
		managed := tree(adapter("/org/bluez/hci0"), device("/org/bluez/hci0/dev_AA", "AA:BB:CC:DD:EE:FF"))
		if _, err := ResolveAdapterPath(managed); err == nil {
			t.Fatal("expected an error: the override names a device, not an adapter")
		}
	})
}

func TestFindAdapterByInterface(t *testing.T) {
	managed := tree(
		adapter("/org/bluez/hci0"),
		objectEntry{"/org/bluez/hci1", AdapterIface, map[string]dbus.Variant{}},
		objectEntry{"/org/bluez/hci1", "org.bluez.GattManager1", map[string]dbus.Variant{}},
	)
	// An adapter that can scan is not necessarily one that can serve GATT.
	if got := FindAdapterByInterface(managed, "org.bluez.GattManager1"); got != "/org/bluez/hci1" {
		t.Errorf("GattManager1 lookup = %q, want /org/bluez/hci1", got)
	}
	if got := FindAdapterByInterface(managed, AdapterIface); got != "/org/bluez/hci0" {
		t.Errorf("Adapter1 lookup = %q, want /org/bluez/hci0 (lowest)", got)
	}
	if got := FindAdapterByInterface(managed, "org.bluez.LEAdvertisingManager1"); got != "" {
		t.Errorf("missing interface = %q, want empty", got)
	}
}

func TestFindDeviceByAddress(t *testing.T) {
	t.Run("case-insensitive match, adapter from the Adapter property", func(t *testing.T) {
		managed := tree(
			adapter("/org/bluez/hci0"),
			device("/org/bluez/hci0/dev_AA_BB_CC_DD_EE_FF", "AA:BB:CC:DD:EE:FF", map[string]dbus.Variant{
				"Adapter": dbus.MakeVariant(dbus.ObjectPath("/org/bluez/hci0")),
			}),
		)
		path, adapterPath, props, found := FindDeviceByAddress(managed, "aa:bb:cc:dd:ee:ff")
		if !found {
			t.Fatal("device not found for a lowercase address")
		}
		if path != "/org/bluez/hci0/dev_AA_BB_CC_DD_EE_FF" {
			t.Errorf("path = %q", path)
		}
		if adapterPath != "/org/bluez/hci0" {
			t.Errorf("adapter = %q", adapterPath)
		}
		if addr, _ := StringProp(props, "Address"); addr != "AA:BB:CC:DD:EE:FF" {
			t.Errorf("props carried Address %q", addr)
		}
	})

	t.Run("adapter falls back to the parent path", func(t *testing.T) {
		managed := tree(device("/org/bluez/hci2/dev_AA_BB_CC_DD_EE_FF", "AA:BB:CC:DD:EE:FF"))
		_, adapterPath, _, found := FindDeviceByAddress(managed, "AA:BB:CC:DD:EE:FF")
		if !found {
			t.Fatal("device not found")
		}
		if adapterPath != "/org/bluez/hci2" {
			t.Errorf("adapter = %q, want the parent path /org/bluez/hci2", adapterPath)
		}
	})

	t.Run("lowest path wins when two adapters see one device", func(t *testing.T) {
		managed := tree(
			device("/org/bluez/hci1/dev_AA_BB_CC_DD_EE_FF", "AA:BB:CC:DD:EE:FF"),
			device("/org/bluez/hci0/dev_AA_BB_CC_DD_EE_FF", "AA:BB:CC:DD:EE:FF"),
		)
		for i := 0; i < 50; i++ {
			path, _, _, found := FindDeviceByAddress(managed, "AA:BB:CC:DD:EE:FF")
			if !found || path != "/org/bluez/hci0/dev_AA_BB_CC_DD_EE_FF" {
				t.Fatalf("path = %q (found=%v), want the hci0 one every time", path, found)
			}
		}
	})

	t.Run("absent device", func(t *testing.T) {
		managed := tree(device("/org/bluez/hci0/dev_11", "11:22:33:44:55:66"))
		if _, _, _, found := FindDeviceByAddress(managed, "AA:BB:CC:DD:EE:FF"); found {
			t.Error("found a device that is not in the tree")
		}
	})
}

func TestRestrictToAdapter(t *testing.T) {
	managed := tree(
		adapter("/org/bluez/hci0"),
		device("/org/bluez/hci0/dev_AA", "AA:BB:CC:DD:EE:FF"),
		adapter("/org/bluez/hci1"),
		device("/org/bluez/hci1/dev_BB", "BB:BB:CC:DD:EE:FF"),
	)

	got := RestrictToAdapter(managed, "/org/bluez/hci0")
	if len(got) != 2 {
		t.Fatalf("restricted tree has %d objects, want 2", len(got))
	}
	if _, ok := got["/org/bluez/hci1/dev_BB"]; ok {
		t.Error("restricted tree leaked an object from the other adapter")
	}
	if _, _, _, found := FindDeviceByAddress(got, "BB:BB:CC:DD:EE:FF"); found {
		t.Error("a device on the other adapter is still findable")
	}

	// An empty adapter path means "no override", not "match nothing".
	if len(RestrictToAdapter(managed, "")) != len(managed) {
		t.Error("an empty adapter path should pass the tree through unchanged")
	}

	// hci1 must not match by prefix against hci10.
	wide := tree(adapter("/org/bluez/hci1"), device("/org/bluez/hci10/dev_CC", "CC:BB:CC:DD:EE:FF"))
	if len(RestrictToAdapter(wide, "/org/bluez/hci1")) != 1 {
		t.Error("hci1 matched an object under hci10")
	}
}

func TestErrorInfo(t *testing.T) {
	const name = "org.bluez.Error.NotPermitted"

	t.Run("value", func(t *testing.T) {
		err := dbus.Error{Name: name, Body: []any{"nope"}}
		gotName, gotMsg, ok := ErrorInfo(err)
		if !ok || gotName != name || gotMsg != "nope" {
			t.Fatalf("ErrorInfo = (%q, %q, %v)", gotName, gotMsg, ok)
		}
	})

	t.Run("pointer", func(t *testing.T) {
		err := &dbus.Error{Name: name, Body: []any{"nope"}}
		gotName, _, ok := ErrorInfo(err)
		if !ok || gotName != name {
			t.Fatalf("ErrorInfo = (%q, _, %v)", gotName, ok)
		}
	})

	t.Run("wrapped", func(t *testing.T) {
		err := fmt.Errorf("reading characteristic: %w", dbus.Error{Name: name})
		gotName, _, ok := ErrorInfo(err)
		if !ok || gotName != name {
			t.Fatalf("ErrorInfo through a wrap = (%q, _, %v)", gotName, ok)
		}
	})

	t.Run("non-bus error", func(t *testing.T) {
		if _, _, ok := ErrorInfo(errors.New("plain")); ok {
			t.Error("a plain error was reported as a bus error")
		}
	})

	t.Run("body without a string", func(t *testing.T) {
		_, gotMsg, ok := ErrorInfo(dbus.Error{Name: name, Body: []any{42}})
		if !ok || gotMsg != "" {
			t.Fatalf("message = %q, want empty for a non-string body", gotMsg)
		}
	})
}

func TestIsErrorName(t *testing.T) {
	err := fmt.Errorf("wrapped: %w", dbus.Error{Name: "org.bluez.Error.InProgress"})
	if !IsErrorName(err, "org.bluez.Error.AlreadyConnected", "org.bluez.Error.InProgress") {
		t.Error("IsErrorName missed a name in the list")
	}
	if IsErrorName(err, "org.bluez.Error.Failed") {
		t.Error("IsErrorName matched the wrong name")
	}
	if IsErrorName(errors.New("plain"), "org.bluez.Error.Failed") {
		t.Error("IsErrorName matched a non-bus error")
	}
}

func TestPropReaders(t *testing.T) {
	props := map[string]dbus.Variant{
		"Address":     dbus.MakeVariant("AA:BB:CC:DD:EE:FF"),
		"Connected":   dbus.MakeVariant(true),
		"UUIDs":       dbus.MakeVariant([]string{"a", "b"}),
		"Adapter":     dbus.MakeVariant(dbus.ObjectPath("/org/bluez/hci0")),
		"Value":       dbus.MakeVariant([]byte{1, 2, 3}),
		"MTU":         dbus.MakeVariant(uint16(517)),
		"RSSI":        dbus.MakeVariant(int16(-42)),
		"WrongType":   dbus.MakeVariant(int32(7)),
		"EmptyString": dbus.MakeVariant(""),
	}

	if s, ok := StringProp(props, "Address"); !ok || s != "AA:BB:CC:DD:EE:FF" {
		t.Errorf("StringProp = (%q, %v)", s, ok)
	}
	// A present-but-empty string is distinguishable from an absent one, which
	// is the whole reason StringProp returns two values.
	if s, ok := StringProp(props, "EmptyString"); !ok || s != "" {
		t.Errorf("StringProp(EmptyString) = (%q, %v), want (\"\", true)", s, ok)
	}
	if _, ok := StringProp(props, "Missing"); ok {
		t.Error("StringProp reported a missing key as present")
	}
	if _, ok := StringProp(props, "WrongType"); ok {
		t.Error("StringProp reported a type mismatch as present")
	}

	if !BoolProp(props, "Connected") {
		t.Error("BoolProp(Connected) = false")
	}
	if BoolProp(props, "Missing") || BoolProp(props, "WrongType") {
		t.Error("BoolProp should be false for a missing key or a type mismatch")
	}

	if got := StringsProp(props, "UUIDs"); len(got) != 2 || got[0] != "a" {
		t.Errorf("StringsProp = %v", got)
	}
	if StringsProp(props, "Missing") != nil || StringsProp(props, "WrongType") != nil {
		t.Error("StringsProp should be nil for a missing key or a type mismatch")
	}

	if got := ObjectPathProp(props, "Adapter"); got != "/org/bluez/hci0" {
		t.Errorf("ObjectPathProp = %q", got)
	}
	if ObjectPathProp(props, "Missing") != "" || ObjectPathProp(props, "WrongType") != "" {
		t.Error("ObjectPathProp should be empty for a missing key or a type mismatch")
	}

	if got := BytesProp(props, "Value"); len(got) != 3 || got[2] != 3 {
		t.Errorf("BytesProp = %v", got)
	}
	if BytesProp(props, "Missing") != nil {
		t.Error("BytesProp should be nil for a missing key")
	}

	if got := Uint16Prop(props, "MTU"); got != 517 {
		t.Errorf("Uint16Prop = %d", got)
	}
	// BlueZ before 5.62 omits MTU entirely, and 0 is how callers detect that.
	if Uint16Prop(props, "Missing") != 0 || Uint16Prop(props, "WrongType") != 0 {
		t.Error("Uint16Prop should be 0 for a missing key or a type mismatch")
	}

	if got := RSSIProp(props); got != -42 {
		t.Errorf("RSSIProp = %d", got)
	}
	if RSSIProp(map[string]dbus.Variant{}) != 0 {
		t.Error("RSSIProp should be 0 when BlueZ omits it")
	}
}

func TestIsDefaultAlias(t *testing.T) {
	tests := []struct {
		alias, address string
		want           bool
	}{
		{"AA-BB-CC-DD-EE-FF", "AA:BB:CC:DD:EE:FF", true},
		{"aa-bb-cc-dd-ee-ff", "AA:BB:CC:DD:EE:FF", true},
		{"wendy-5f2c", "AA:BB:CC:DD:EE:FF", false},
		{"AA-BB-CC-DD-EE-FF", "", false},
		{"", "", false},
	}
	for _, tc := range tests {
		if got := IsDefaultAlias(tc.alias, tc.address); got != tc.want {
			t.Errorf("IsDefaultAlias(%q, %q) = %v, want %v", tc.alias, tc.address, got, tc.want)
		}
	}
}
