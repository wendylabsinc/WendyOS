package config

import (
	"encoding/json"
	"testing"
)

// TestEvaluateDevicePin covers the WDY-1149 trust anchor: a device hostname is
// pinned to its (organisation, cloud host, asset). The pin is
// renewal/re-enrollment tolerant — a routine cert rotation keeps the same
// org+cloud+asset and must never trip it.
func TestEvaluateDevicePin(t *testing.T) {
	c := &Config{}

	if v := c.EvaluateDevicePin("wendy-thor.local", 7, "grpc.wendy.dev:443", "42"); v != PinFirstUse {
		t.Fatalf("unpinned host: want PinFirstUse, got %v", v)
	}

	c.SetDevicePin("wendy-thor.local", 7, "grpc.wendy.dev:443", "42", "")

	// Same org + cloud + asset (e.g. a renewed cert) must match.
	if v := c.EvaluateDevicePin("wendy-thor.local", 7, "grpc.wendy.dev:443", "42"); v != PinMatch {
		t.Fatalf("same identity: want PinMatch, got %v", v)
	}
	// Different org → mismatch.
	if v := c.EvaluateDevicePin("wendy-thor.local", 9, "grpc.wendy.dev:443", "42"); v != PinMismatch {
		t.Fatalf("org change: want PinMismatch, got %v", v)
	}
	// Different cloud host → mismatch.
	if v := c.EvaluateDevicePin("wendy-thor.local", 7, "evil.example.com:443", "42"); v != PinMismatch {
		t.Fatalf("cloud change: want PinMismatch, got %v", v)
	}
}

// TestEvaluateDevicePinDifferentAssetIsMismatch is the device-identity half of
// the pin: same organisation and cloud, but a different asset id means a
// DIFFERENT physical device now answers at this hostname (a swap, or a wipe +
// re-enroll that minted a new cloud record). Org alone cannot catch that —
// every device in the fleet shares it.
func TestEvaluateDevicePinDifferentAssetIsMismatch(t *testing.T) {
	c := &Config{}
	c.SetDevicePin("wendy-thor.local", 7, "grpc.wendy.dev:443", "42", "")

	if v := c.EvaluateDevicePin("wendy-thor.local", 7, "grpc.wendy.dev:443", "43"); v != PinMismatch {
		t.Fatalf("asset change within same org+cloud: want PinMismatch, got %v", v)
	}
}

// TestEvaluateDevicePinAdoptsAssetForLegacyPin covers pins written before asset
// ids were recorded: org+cloud still match, so this is not an attack signal —
// the caller is told to backfill the observed asset rather than challenge.
func TestEvaluateDevicePinAdoptsAssetForLegacyPin(t *testing.T) {
	c := &Config{DevicePins: map[string]DevicePin{
		"wendy-thor": {OrgID: 7, CloudGRPC: "grpc.wendy.dev:443"},
	}}

	if v := c.EvaluateDevicePin("wendy-thor.local", 7, "grpc.wendy.dev:443", "42"); v != PinAdoptAsset {
		t.Fatalf("legacy assetless pin: want PinAdoptAsset, got %v", v)
	}
	// A legacy pin against a device whose cert carries no asset identity at all
	// has nothing to adopt and nothing to challenge.
	if v := c.EvaluateDevicePin("wendy-thor.local", 7, "grpc.wendy.dev:443", ""); v != PinMatch {
		t.Fatalf("legacy pin, no observed asset: want PinMatch, got %v", v)
	}
}

// TestEvaluateDevicePinUnknownObservedAssetMatches ensures an agent whose
// certificate carries no asset identity (older cert, legacy CN) does not read
// as a swapped device against a pin that does record one.
func TestEvaluateDevicePinUnknownObservedAssetMatches(t *testing.T) {
	c := &Config{}
	c.SetDevicePin("wendy-thor.local", 7, "grpc.wendy.dev:443", "42", "")

	if v := c.EvaluateDevicePin("wendy-thor.local", 7, "grpc.wendy.dev:443", ""); v != PinMatch {
		t.Fatalf("unidentifiable asset: want PinMatch, got %v", v)
	}
}

// TestEvaluateDevicePinNormalizesHostname ensures cosmetic hostname differences
// (case, trailing dot, .local) don't spuriously read as a different device.
func TestEvaluateDevicePinNormalizesHostname(t *testing.T) {
	c := &Config{}
	c.SetDevicePin("Wendy-Thor.local.", 7, "grpc.wendy.dev:443", "42", "")
	if v := c.EvaluateDevicePin("wendy-thor", 7, "grpc.wendy.dev:443", "42"); v != PinMatch {
		t.Fatalf("normalized host should match pin, got %v", v)
	}
}

// TestClearDevicePin covers the accepted-downgrade path: once the user confirms
// a pinned hostname is legitimately unenrolled, the stale pin must go, so the
// device's next enrollment reads as a first use.
func TestClearDevicePin(t *testing.T) {
	c := &Config{}
	c.SetDevicePin("wendy-thor.local", 7, "grpc.wendy.dev:443", "42", "")

	c.ClearDevicePin("Wendy-Thor.local.") // cosmetic variant must hit the same key
	if _, ok := c.DevicePinFor("wendy-thor"); ok {
		t.Fatal("pin still present after ClearDevicePin")
	}
	if v := c.EvaluateDevicePin("wendy-thor", 7, "grpc.wendy.dev:443", "42"); v != PinFirstUse {
		t.Fatalf("after clearing: want PinFirstUse, got %v", v)
	}
	// Clearing an absent pin (nil map included) must not panic.
	(&Config{}).ClearDevicePin("nobody")
}

func TestDevicePinRoundTripsThroughConfig(t *testing.T) {
	c := &Config{}
	c.SetDevicePin("wendy-thor.local", 7, "grpc.wendy.dev:443", "42", "")

	data, err := json.Marshal(c)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got Config
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if v := got.EvaluateDevicePin("wendy-thor.local", 7, "grpc.wendy.dev:443", "42"); v != PinMatch {
		t.Fatalf("pin did not round-trip through JSON, got %v", v)
	}
	// The asset must survive the round trip too, or every restart would silently
	// re-adopt whatever device answers.
	if v := got.EvaluateDevicePin("wendy-thor.local", 7, "grpc.wendy.dev:443", "43"); v != PinMismatch {
		t.Fatalf("asset did not round-trip through JSON, got %v", v)
	}
}

func TestDevicePinSourcePrecedence(t *testing.T) {
	const host = "wendy-thor.local"

	t.Run("cloud write overwrites a lan pin", func(t *testing.T) {
		c := &Config{}
		c.SetDevicePinFrom(host, 7, "grpc.wendy.dev:443", "42", "", PinSourceLAN)
		c.SetDevicePinFrom(host, 7, "grpc.wendy.dev:443", "99", "", PinSourceCloud)

		pin, _ := c.DevicePinFor(host)
		if pin.AssetID != "99" || pin.Source != PinSourceCloud {
			t.Fatalf("want asset 99 from cloud, got asset %q from %q", pin.AssetID, pin.Source)
		}
	})

	t.Run("lan observation conflicting with a cloud pin is a mismatch", func(t *testing.T) {
		c := &Config{}
		c.SetDevicePinFrom(host, 7, "grpc.wendy.dev:443", "42", "", PinSourceCloud)
		if v := c.EvaluateDevicePin(host, 7, "grpc.wendy.dev:443", "43"); v != PinMismatch {
			t.Fatalf("want PinMismatch, got %v", v)
		}
	})

	t.Run("lan never backfills an asset into a cloud pin", func(t *testing.T) {
		c := &Config{}
		c.SetDevicePinFrom(host, 7, "grpc.wendy.dev:443", "", "", PinSourceCloud)
		if v := c.EvaluateDevicePin(host, 7, "grpc.wendy.dev:443", "42"); v != PinMatch {
			t.Fatalf("want PinMatch without adoption, got %v", v)
		}
	})

	t.Run("legacy fieldless pin reads as lan", func(t *testing.T) {
		c := &Config{DevicePins: map[string]DevicePin{
			"wendy-thor": {OrgID: 7, CloudGRPC: "grpc.wendy.dev:443"},
		}}
		if got := c.PinSource(host); got != PinSourceLAN {
			t.Fatalf("want %q, got %q", PinSourceLAN, got)
		}
		if v := c.EvaluateDevicePin(host, 7, "grpc.wendy.dev:443", "42"); v != PinAdoptAsset {
			t.Fatalf("want PinAdoptAsset, got %v", v)
		}
	})
}
