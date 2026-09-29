//go:build linux

package bleprovider

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"
	"github.com/godbus/dbus/v5/prop"
	"go.uber.org/zap"
)

func TestCandidatesRequireMatchingServiceDataAndLowerAsset(t *testing.T) {
	uuid, _ := ServiceUUID(64)
	otherUUID, _ := ServiceUUID(65)
	a, _ := NewAdvertisement(12, "field", 0x81)
	raw, _ := a.MarshalBinary()
	wrong, _ := NewAdvertisement(13, "other", 0x81)
	wrongRaw, _ := wrong.MarshalBinary()
	props := func(address, kind, serviceUUID string, data []byte) map[string]dbus.Variant {
		return map[string]dbus.Variant{
			"Address": dbus.MakeVariant(address), "AddressType": dbus.MakeVariant(kind),
			"ServiceData": dbus.MakeVariant(map[string]dbus.Variant{serviceUUID: dbus.MakeVariant(data)}),
		}
	}
	objects := managedObjects{
		"/org/bluez/hci0/dev_A": {deviceInterface: props("AA:BB:CC:DD:EE:01", "public", uuid, raw)},
		"/org/bluez/hci0/dev_B": {deviceInterface: props("AA:BB:CC:DD:EE:02", "random", otherUUID, raw)},
		"/org/bluez/hci0/dev_C": {deviceInterface: props("AA:BB:CC:DD:EE:03", "public", uuid, wrongRaw)},
		"/org/bluez/hci1/dev_D": {deviceInterface: props("AA:BB:CC:DD:EE:04", "public", uuid, raw)},
	}
	got := candidates(objects, "/org/bluez/hci0", uuid, "field", 20)
	if len(got) != 1 || got[0].asset != 12 || got[0].address != "AA:BB:CC:DD:EE:01" || got[0].path != "/org/bluez/hci0/dev_A" {
		t.Fatalf("candidates = %+v", got)
	}
	if got = candidates(objects, "/org/bluez/hci0", uuid, "field", 12); len(got) != 0 {
		t.Fatalf("higher/equal asset dialed: %+v", got)
	}
}

func TestBlueZOwnerChangeRequiresProviderRestart(t *testing.T) {
	if err := verifyBlueZOwner(":1.42", ":1.42"); err != nil {
		t.Fatalf("stable BlueZ owner: %v", err)
	}
	for _, current := range []string{":1.43", ""} {
		if err := verifyBlueZOwner(":1.42", current); !errors.Is(err, errBlueZOwnerChanged) {
			t.Fatalf("owner %q: got %v, want restart", current, err)
		}
	}
	r := &runtime{cfg: Config{Logger: zap.NewNop()}, owner: ":1.42", ownerLookup: func(context.Context, *dbus.Conn) (string, error) {
		return ":1.43", nil
	}}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := r.scanLoop(ctx, nil, "/org/bluez/hci0"); !errors.Is(err, errBlueZOwnerChanged) {
		t.Fatalf("scan loop after BlueZ restart: got %v, want immediate provider restart", err)
	}
}

func TestRuntimeClaimsTargetAndSuppressesDuplicate(t *testing.T) {
	r := &runtime{cfg: Config{TargetPeers: 3}, active: map[int32]struct{}{}, nextAttempt: map[int32]time.Time{}}
	for _, asset := range []int32{1, 2, 3} {
		if !r.claim(asset) {
			t.Fatalf("could not claim %d", asset)
		}
	}
	if r.claim(1) || r.claim(4) {
		t.Fatal("duplicate or fourth peer accepted")
	}
	r.release(1)
	if !r.claim(4) {
		t.Fatal("slot not reusable")
	}
}

func TestAdvertisementTxPowerCapabilities(t *testing.T) {
	manager := func(features []string, capability any) map[string]dbus.Variant {
		return map[string]dbus.Variant{
			"SupportedFeatures": dbus.MakeVariant(features),
			"SupportedCapabilities": dbus.MakeVariant(map[string]dbus.Variant{
				"MaxTxPower": dbus.MakeVariant(capability),
			}),
		}
	}
	tests := []struct {
		name    string
		props   map[string]dbus.Variant
		want    int16
		present bool
	}{
		{"manager lookup failed", nil, 0, false},
		{"jetson", manager([]string{"CanSetTxPower"}, int16(8)), 8, true},
		{"pi without feature", manager([]string{"Other"}, int16(8)), 0, false},
		{"malformed features", map[string]dbus.Variant{"SupportedFeatures": dbus.MakeVariant("CanSetTxPower")}, 0, false},
		{"missing features", map[string]dbus.Variant{"SupportedCapabilities": manager(nil, int16(8))["SupportedCapabilities"]}, 0, false},
		{"missing capabilities", map[string]dbus.Variant{"SupportedFeatures": dbus.MakeVariant([]string{"CanSetTxPower"})}, 0, false},
		{"wrong capability type", manager([]string{"CanSetTxPower"}, int32(8)), 0, false},
		{"zero is valid", manager([]string{"CanSetTxPower"}, int16(0)), 0, true},
		{"clamp bluez upper limit", manager([]string{"CanSetTxPower"}, int16(30)), 20, true},
		{"bluez lower limit", manager([]string{"CanSetTxPower"}, int16(-127)), -127, true},
		{"invalid controller maximum", manager([]string{"CanSetTxPower"}, int16(-128)), 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, present := advertisementTxPower(tt.props)
			if got != tt.want || present != tt.present {
				t.Fatalf("power=(%d,%v), want (%d,%v)", got, present, tt.want, tt.present)
			}
			properties := advertisementProperties("uuid", []byte{1}, tt.props)
			power, exists := properties["TxPower"]
			if exists != tt.present {
				t.Fatalf("TxPower exists=%v, want %v", exists, tt.present)
			}
			if exists {
				value, ok := power.Value.(int16)
				if !ok || value != tt.want {
					t.Fatalf("TxPower=%T(%v), want int16(%d)", power.Value, power.Value, tt.want)
				}
			}
			if _, ok := properties["ServiceData"]; !ok {
				t.Fatal("advertising payload removed")
			}
		})
	}
}

func TestAdvertisementRegistrationFallsBackToDefaultPower(t *testing.T) {
	properties := advertisementProperties("uuid", []byte{1}, map[string]dbus.Variant{
		"SupportedFeatures": dbus.MakeVariant([]string{"CanSetTxPower"}),
		"SupportedCapabilities": dbus.MakeVariant(map[string]dbus.Variant{
			"MaxTxPower": dbus.MakeVariant(int16(8)),
		}),
	})
	var exported []bool
	registrations := 0
	err := registerAdvertisementWithFallback(properties, func(p map[string]*prop.Prop) error {
		_, requested := p["TxPower"]
		exported = append(exported, requested)
		return nil
	}, func() error {
		registrations++
		if registrations == 1 {
			return errors.New("BlueZ rejected TxPower")
		}
		return nil
	})
	if err != nil || registrations != 2 || len(exported) != 2 || !exported[0] || exported[1] {
		t.Fatalf("fallback err=%v registrations=%d exported=%v", err, registrations, exported)
	}
	if _, ok := properties["ServiceData"]; !ok {
		t.Fatal("fallback removed service advertisement")
	}

	// A failure without the optional property must not be retried.
	registrations = 0
	err = registerAdvertisementWithFallback(advertisementProperties("uuid", []byte{1}, nil), func(map[string]*prop.Prop) error {
		return nil
	}, func() error {
		registrations++
		return errors.New("adapter unavailable")
	})
	if err == nil || registrations != 1 {
		t.Fatalf("ordinary failure err=%v registrations=%d", err, registrations)
	}
}

func TestAdvertisementRelaxedIntervalsPresent(t *testing.T) {
	properties := advertisementProperties("uuid", []byte{1}, nil)
	min, ok := properties["MinInterval"]
	if !ok {
		t.Fatal("MinInterval missing")
	}
	if v, ok := min.Value.(uint16); !ok || v != 160 {
		t.Fatalf("MinInterval=%T(%v), want uint16(160)", min.Value, min.Value)
	}
	max, ok := properties["MaxInterval"]
	if !ok {
		t.Fatal("MaxInterval missing")
	}
	if v, ok := max.Value.(uint16); !ok || v != 320 {
		t.Fatalf("MaxInterval=%T(%v), want uint16(320)", max.Value, max.Value)
	}
}
