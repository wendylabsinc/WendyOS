package appconfig

import (
	"strings"
	"testing"
)

// TestSensorReadEntitlementAcceptsAnAllowlist covers the narrowing the grant
// requires: the sensor-read grant names the source ids it covers, the way the
// camera grant names device nodes.
func TestSensorReadEntitlementAcceptsAnAllowlist(t *testing.T) {
	raw := []byte(`{
		"appId": "sh.wendy.test.sensor-read",
		"version": "1.0.0",
		"entitlements": [{"type": "sensor-read", "allowlist": ["v4l2:/dev/video0"]}]
	}`)
	cfg, err := LoadFromBytes(raw)
	if err != nil {
		t.Fatalf("loading: %v", err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if warnings := ValidateJSON(raw); len(warnings) != 0 {
		t.Fatalf("warnings: %v", warnings)
	}
	if got := cfg.Entitlements[0].Allowlist; len(got) != 1 || got[0] != "v4l2:/dev/video0" {
		t.Fatalf("allowlist = %v", got)
	}
}

// TestSensorReadEntitlementRejectsUnknownKeys keeps the grant from silently
// accepting configuration it does not honor — a "mode" that does nothing would
// read as a restriction that is not enforced.
func TestSensorReadEntitlementRejectsUnknownKeys(t *testing.T) {
	raw := []byte(`{
		"appId": "sh.wendy.test.sensor-read",
		"version": "1.0.0",
		"entitlements": [{"type": "sensor-read", "allowlist": ["v4l2:/dev/video0"], "mode": "readonly"}]
	}`)
	if warnings := ValidateJSON(raw); len(warnings) == 0 {
		t.Fatal("an unknown key on the sensor-read entitlement produced no warning")
	}
}

// TestSensorReadEntitlementRoundTripsThroughAnnotations guards the socket-restore
// path: after an agent restart the allowlist is recovered from container labels,
// not from wendy.json, so a lost allowlist would silently widen the grant.
func TestSensorReadEntitlementRoundTripsThroughAnnotations(t *testing.T) {
	original := Entitlement{Type: EntitlementSensorRead, Allowlist: []string{"v4l2:/dev/video0", "ipcamera:200"}}
	decoded := ParseEntitlementAnnotation(EntitlementSensorRead, EntitlementAnnotationValue(original))
	if len(decoded.Allowlist) != 2 || decoded.Allowlist[0] != "v4l2:/dev/video0" || decoded.Allowlist[1] != "ipcamera:200" {
		t.Fatalf("allowlist after round trip = %v", decoded.Allowlist)
	}
}

// TestSensorReadEntitlementRequiresAnAllowlist is the whole point of making the
// allowlist mandatory: a bare grant would cover every source the device can
// multiplex today and every source kind that becomes subscribable later, so an
// app asking for a camera would silently gain microphones and the ROS 2 graph on
// an agent upgrade.
func TestSensorReadEntitlementRequiresAnAllowlist(t *testing.T) {
	for name, entitlement := range map[string]string{
		"omitted": `{"type": "sensor-read"}`,
		"empty":   `{"type": "sensor-read", "allowlist": []}`,
		"blank":   `{"type": "sensor-read", "allowlist": ["  "]}`,
	} {
		t.Run(name, func(t *testing.T) {
			raw := []byte(`{
				"appId": "sh.wendy.test.sensor-read",
				"version": "1.0.0",
				"entitlements": [` + entitlement + `]
			}`)
			cfg, err := LoadFromBytes(raw)
			if err != nil {
				t.Fatalf("loading: %v", err)
			}
			err = cfg.Validate()
			if err == nil {
				t.Fatal("a sensor-read entitlement without an allowlist was accepted")
			}
			if !strings.Contains(err.Error(), "allowlist") {
				t.Fatalf("the rejection does not name the fix: %v", err)
			}
		})
	}
}

// TestSensorReadEntitlementRequiresAnAllowlistInServices covers the per-service
// entitlement block, which is validated by a separate call and would otherwise
// be a way around the rule.
func TestSensorReadEntitlementRequiresAnAllowlistInServices(t *testing.T) {
	raw := []byte(`{
		"appId": "sh.wendy.test.sensor-read",
		"version": "1.0.0",
		"services": {
			"model": {"entitlements": [{"type": "sensor-read"}]}
		}
	}`)
	cfg, err := LoadFromBytes(raw)
	if err != nil {
		t.Fatalf("loading: %v", err)
	}
	if err := cfg.Validate(); err == nil {
		t.Fatal("a service-level sensor-read entitlement without an allowlist was accepted")
	}
}

// TestOldEntitlementSpellingsAreRejected pins the rename: "sensors" and "data"
// never shipped, and no alias for them exists, so a manifest using the old
// spelling must fail rather than quietly resolve to the new grant.
func TestOldEntitlementSpellingsAreRejected(t *testing.T) {
	for _, old := range []string{"sensors", "data"} {
		t.Run(old, func(t *testing.T) {
			raw := []byte(`{
				"appId": "sh.wendy.test.sensor-read",
				"version": "1.0.0",
				"entitlements": [{"type": "` + old + `"}]
			}`)
			cfg, err := LoadFromBytes(raw)
			if err != nil {
				t.Fatalf("loading: %v", err)
			}
			if err := cfg.Validate(); err == nil {
				t.Fatalf("the retired %q entitlement spelling was accepted", old)
			}
		})
	}
}
