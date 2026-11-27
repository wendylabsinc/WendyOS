package appconfig

import "testing"

func TestSensorReadEntitlement(t *testing.T) {
	cfg := &AppConfig{AppID: "reader", Entitlements: []Entitlement{{Type: EntitlementSensorRead}}}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if w := ValidateJSON([]byte(`{"appId":"reader","entitlements":[{"type":"sensor-read"}]}`)); len(w) != 0 {
		t.Fatal(w)
	}
	cfg.Entitlements = append(cfg.Entitlements, Entitlement{Type: EntitlementSensorRead})
	if err := cfg.Validate(); err == nil {
		t.Fatal("duplicate sensor-read accepted")
	}
}
