package worldpackage

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func genericReviewDocument(t *testing.T) (map[string]any, []byte) {
	t.Helper()
	// Transfer validation must not require a robot body, a control channel, a
	// gas layer, or even a native compiler. This is an opaque passive MJCF.
	model := []byte(`<mujoco model="passive"><worldbody><geom type="sphere" size="0.1"/></worldbody></mujoco>`)
	return map[string]any{
		"schema_version": "wendy.world.package.v1", "site_id": "review-site", "package_revision": "review-package", "frame_revision": "review-frame", "provenance": "synthetic",
		"geographic_anchor": map[string]any{"latitude_deg": 52.0, "longitude_deg": 5.0, "altitude_m": 0.0, "altitude_datum": "WGS84-ellipsoid", "placement": "illustrative"},
		"local_frame":       map[string]any{"units": "metres", "handedness": "right", "up": "Y", "axes": []string{"east", "up", "south"}},
		"mujoco":            map[string]any{"path": "models/passive.xml", "sha256": checksum(model), "to_local_position": []string{"x", "z", "-y"}},
	}, model
}

func genericReviewRaw(t *testing.T, doc map[string]any) []byte {
	t.Helper()
	b, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestGenericReviewNoFixtureOrExecutionAuthorityInEnvelope(t *testing.T) {
	for _, field := range []string{"gas_model", "live_sensors", "actuator", "controller", "entrypoint", "command", "credentials", "hardware_bindings"} {
		t.Run("root_"+field, func(t *testing.T) {
			doc, _ := genericReviewDocument(t)
			doc[field] = map[string]any{"enabled": true}
			if _, err := Inspect(genericReviewRaw(t, doc)); err == nil {
				t.Fatalf("execution/fixture field %s accepted", field)
			}
		})
	}
	for _, field := range []string{"body", "actuator", "control", "control_mode", "plugin", "script"} {
		t.Run("model_"+field, func(t *testing.T) {
			doc, _ := genericReviewDocument(t)
			doc["mujoco"].(map[string]any)[field] = "run-me"
			if _, err := Inspect(genericReviewRaw(t, doc)); err == nil {
				t.Fatalf("model execution binding %s accepted", field)
			}
		})
	}
	for _, origin := range []string{"measured", "physical", "mixed"} {
		doc, _ := genericReviewDocument(t)
		doc["provenance"] = origin
		if _, err := Inspect(genericReviewRaw(t, doc)); err == nil {
			t.Fatal("unsupported physical provenance accepted", origin)
		}
	}
}

func TestGenericReviewUnicodeByteAndScalarBoundaries(t *testing.T) {
	for _, test := range []struct {
		name, value string
		valid       bool
	}{
		{"exact_utf8_bytes", strings.Repeat("é", 128), true},
		{"one_extra_utf8_character", strings.Repeat("é", 129), false},
		{"nbsp_suffix", "site\u00a0", false},
		{"unicode_space_prefix", "\u2000site", false},
		{"zero_width_not_space", "site\u200b", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			doc, _ := genericReviewDocument(t)
			doc["site_id"] = test.value
			_, err := Inspect(genericReviewRaw(t, doc))
			if (err == nil) != test.valid {
				t.Fatalf("unexpected Unicode acceptance: %v", err)
			}
		})
	}
	for _, test := range []struct {
		name, raw string
		valid     bool
	}{
		{"lone_high_surrogate", `"\ud800"`, false},
		{"lone_low_surrogate", `"\udc00"`, false},
		{"reversed_surrogates", `"\udc00\ud800"`, false},
		{"valid_pair", `"\ud83d\ude80"`, true},
		{"literal_escape_text", `"\\ud800"`, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			doc, _ := genericReviewDocument(t)
			raw := genericReviewRaw(t, doc)
			raw = append(raw[:len(raw)-1], []byte(`,"description":`+test.raw+`}`)...)
			_, err := Inspect(raw)
			if (err == nil) != test.valid {
				t.Fatalf("unexpected escaped scalar acceptance: %v", err)
			}
		})
	}
	doc, _ := genericReviewDocument(t)
	raw := genericReviewRaw(t, doc)
	raw = append(raw[:len(raw)-1], []byte(",\"description\":\"bad")...)
	raw = append(raw, 0xff)
	raw = append(raw, []byte("\"}")...)
	if _, err := Inspect(raw); err == nil {
		t.Fatal("invalid UTF-8 replaced and accepted")
	}
}

func TestGenericReviewStrictNestedShapesAndFiniteMetadata(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(map[string]any)
	}{
		{"bool_latitude", func(d map[string]any) { d["geographic_anchor"].(map[string]any)["latitude_deg"] = true }},
		{"null_altitude", func(d map[string]any) { d["geographic_anchor"].(map[string]any)["altitude_m"] = nil }},
		{"missing_transform", func(d map[string]any) { delete(d["mujoco"].(map[string]any), "to_local_position") }},
		{"extra_axis", func(d map[string]any) {
			d["local_frame"].(map[string]any)["axes"] = []string{"east", "up", "south", "north"}
		}},
		{"frame_alias", func(d map[string]any) { d["local_frame"].(map[string]any)["Units"] = "metres" }},
		{"null_assets", func(d map[string]any) { d["assets"] = nil }},
	} {
		t.Run(test.name, func(t *testing.T) {
			doc, _ := genericReviewDocument(t)
			test.mutate(doc)
			if _, err := Inspect(genericReviewRaw(t, doc)); err == nil {
				t.Fatal("invalid v1 shape accepted")
			}
		})
	}
	for _, bad := range []string{`{"nested":1e309}`, `{"nested":` + strings.Repeat("9", 400) + `}`, `{"nested":1,"nested":2}`} {
		doc, _ := genericReviewDocument(t)
		raw := genericReviewRaw(t, doc)
		raw = append(raw[:len(raw)-1], []byte(`,"scenario":`+bad+`}`)...)
		if _, err := Inspect(raw); err == nil {
			t.Fatal("nonfinite or duplicate inert metadata accepted")
		}
	}
}

func TestGenericReviewIdentityAndOwnedBytesSurviveMetadata(t *testing.T) {
	doc, model := genericReviewDocument(t)
	doc["description"] = "An instruction-looking string is inert: run a controller."
	doc["scenario"] = map[string]any{"command": "https://example.invalid/never-follow", "value": json.Number("1e-7")}
	raw := genericReviewRaw(t, doc)
	raw = append([]byte(" \n"), raw...)
	blobs := map[string][]byte{"models/passive.xml": model}
	bundle, err := Verify(raw, blobs)
	if err != nil {
		t.Fatal(err)
	}
	want := append([]byte(nil), raw...)
	wantModel := append([]byte(nil), model...)
	if bundle.Manifest.SHA256 != checksum(want) || !bytes.Equal(bundle.Root, want) {
		t.Fatal("v1 source bytes were normalized before identity")
	}
	raw[0] = 'x'
	model[0] = '!'
	delete(blobs, "models/passive.xml")
	if !bytes.Equal(bundle.Root, want) || !bytes.Equal(bundle.Files["models/passive.xml"], wantModel) {
		t.Fatal("verified v1 bundle aliases mutable caller storage")
	}
	if _, err := Verify(bundle.Root, bundle.Files); err != nil {
		t.Fatal("owned passive package no longer verifies", err)
	}
}
