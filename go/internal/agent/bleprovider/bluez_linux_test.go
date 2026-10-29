//go:build linux

package bleprovider

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"
	"github.com/godbus/dbus/v5/prop"
	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"go.uber.org/zap"
)

func TestRuntimeRetryHistoryBoundsAndExpires(t *testing.T) {
	r := &runtime{}
	for i := 0; i < 4096; i++ {
		r.failedCandidate(candidate{address: fmt.Sprintf("02:00:00:%02X:%02X:%02X", i>>16&255, i>>8&255, i&255), addressType: "public"})
	}
	if len(r.radioRetry) > maxPeerRetryHistory {
		t.Fatal("unbounded failure history")
	}
	for key := range r.radioRetry {
		r.radioRetry[key] = time.Now().Add(-time.Second)
	}
	r.expireRadioRetries(time.Now())
	if len(r.radioRetry) != 0 {
		t.Fatal("expired retries retained")
	}
}

func TestCandidateAdmissionKeepsEqualHintsDistinctAndBoundsRetries(t *testing.T) {
	r := &runtime{cfg: Config{Credentials: &localmesh.Credentials{Device: selfFixture}}}
	first := candidate{hint: deviceHint(lowDevice), address: "02:00:00:00:00:01", addressType: "public"}
	second := first
	second.address = "02:00:00:00:00:02"
	a := r.reserveCandidate(first)
	if a == nil || r.reserveCandidate(second) != nil {
		t.Fatal("outgoing attempt budget not enforced")
	}
	if !a.Ready(lowFixture, func() {}) {
		t.Fatal("ready failed")
	}
	b := r.reserveCandidate(second)
	if b == nil {
		t.Fatal("equal prefixes merged distinct addresses")
	}
	b.Release()
	a.Release()
	r.failedCandidate(first)
	if r.reserveCandidate(first) != nil {
		t.Fatal("failure bypassed cooldown")
	}
	if l := r.reserveCandidate(second); l == nil {
		t.Fatal("failed peer blocked another address")
	} else {
		l.Release()
	}
	r.known[first.key()] = lowFixture
	first.hint[0]++
	for key := range r.radioRetry {
		r.radioRetry[key] = time.Now().Add(-time.Second)
	}
	if l := r.reserveCandidate(first); l == nil {
		t.Fatal("changed prefix could not authenticate")
	} else {
		l.Release()
	}
	if _, exists := r.known[first.key()]; exists {
		t.Fatal("changed prefix retained old principal pin")
	}
}

func TestCandidatesRequireMatchingServiceDataAndLowerDevice(t *testing.T) {
	uuid := ServiceUUID(testTenant)
	otherUUID := ServiceUUID(otherTenant)
	a, _ := NewAdvertisement(lowDevice, "field", 0x81)
	raw, _ := a.MarshalBinary()
	wrong, _ := NewAdvertisement(peerDevice, "other", 0x81)
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
	got := candidates(objects, "/org/bluez/hci0", uuid, "field", selfFixture)
	if len(got) != 1 || got[0].hint != deviceHint(lowDevice) || got[0].address != "AA:BB:CC:DD:EE:01" || got[0].path != "/org/bluez/hci0/dev_A" {
		t.Fatalf("candidates = %+v", got)
	}
	if got = candidates(objects, "/org/bluez/hci0", uuid, "field", lowFixture); len(got) != 1 {
		t.Fatalf("equal hint must reach authentication: %+v", got)
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
	r := &runtime{cfg: Config{Credentials: &localmesh.Credentials{Device: selfFixture}}}
	p := r.connectionPolicy()
	var leases []*localmesh.ConnectionLease
	for i := 0; i < localmesh.BLEMaxConnections; i++ {
		l := p.Reserve(fmt.Sprint(i), numberedBLEDevice(i+1), false)
		if l == nil {
			t.Fatal("incoming capacity denied")
		}
		leases = append(leases, l)
	}
	if p.Reserve("fourth", lowFixture, false) != nil {
		t.Fatal("fourth incoming accepted")
	}
	leases[0].Release()
	if p.Reserve("replacement", lowFixture, false) == nil {
		t.Fatal("released slot not reusable")
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

	// A failure with interval props (e.g. a future re-introduction) must
	// also fall back to defaults: strip intervals and retry once.
	withIntervals := advertisementProperties("uuid", []byte{1}, nil)
	withIntervals["MinInterval"] = &prop.Prop{Value: uint16(160)}
	withIntervals["MaxInterval"] = &prop.Prop{Value: uint16(320)}
	registrations = 0
	err = registerAdvertisementWithFallback(withIntervals, func(map[string]*prop.Prop) error {
		return nil
	}, func() error {
		registrations++
		if registrations == 1 {
			return errors.New("BlueZ rejected intervals")
		}
		return nil
	})
	if err != nil || registrations != 2 {
		t.Fatalf("interval fallback err=%v registrations=%d", err, registrations)
	}
	if _, ok := withIntervals["MinInterval"]; ok {
		t.Fatal("fallback left MinInterval in place")
	}
	if _, ok := withIntervals["MaxInterval"]; ok {
		t.Fatal("fallback left MaxInterval in place")
	}
}

func TestAdvertisementRelaxedIntervalsPresent(t *testing.T) {
	// golden16: intervals reverted — BlueZ 5.87 rejects them outright.
	// Advertisement must carry only required props; fallback must strip
	// any optional interval/power props on registration failure.
	properties := advertisementProperties("uuid", []byte{1}, nil)
	if _, ok := properties["MinInterval"]; ok {
		t.Fatal("MinInterval present (reverted in golden16)")
	}
	if _, ok := properties["MaxInterval"]; ok {
		t.Fatal("MaxInterval present (reverted in golden16)")
	}
}

func TestCandidatesDoNotDeduplicateCollidingHints(t *testing.T) {
	adv, _ := NewAdvertisement(lowDevice, "field", DefaultPSM)
	raw, _ := adv.MarshalBinary()
	first := meshDeviceProps(ServiceUUID(testTenant), raw)
	second := meshDeviceProps(ServiceUUID(testTenant), raw)
	second["Address"] = dbus.MakeVariant("AA:BB:CC:DD:EE:02")
	objects := managedObjects{
		"/org/bluez/hci0/dev_A": {deviceInterface: first},
		"/org/bluez/hci0/dev_B": {deviceInterface: second},
	}
	if peers := candidates(objects, "/org/bluez/hci0", ServiceUUID(testTenant), "field", selfFixture); len(peers) != 2 {
		t.Fatalf("colliding candidates lost: %+v", peers)
	}
}

func TestVersionedBinaryAdvertisementHasNoNameOrUUIDList(t *testing.T) {
	a, err := NewAdvertisement(lowDevice, "field", DefaultPSM)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := a.MarshalBinary()
	props := advertisementProperties(ServiceUUID(testTenant), raw, nil)
	if len(props) != 2 || props["Type"].Value != "peripheral" {
		t.Fatalf("unexpected advertised properties: %+v", props)
	}
	data := props["ServiceData"].Value.(map[string]dbus.Variant)
	if len(data) != 1 || !bytes.Equal(data[ServiceUUID(testTenant)].Value().([]byte), raw) {
		t.Fatal("versioned payload changed in registration")
	}
	if len(raw) != 9 || 3+2+16+len(raw) != 30 {
		t.Fatal("binary advertisement exceeds primary packet budget")
	}
}
