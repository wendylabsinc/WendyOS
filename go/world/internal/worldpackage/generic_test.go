package worldpackage

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func genericFixture(t *testing.T) ([]byte, map[string][]byte) {
	t.Helper()
	// Transfer validates bytes, not model support. The model and asset may be
	// passed to a separately bounded compiler only after explicit execution.
	model := []byte(`<mujoco><include file="parts/scene.xml"/></mujoco>`)
	asset := []byte("opaque declared asset\x00\xff")
	doc := map[string]any{
		"schema_version": SchemaV1, "site_id": "office", "package_revision": "generic-1", "frame_revision": "floor-1", "provenance": "synthetic",
		"geographic_anchor": map[string]any{"latitude_deg": 52.0, "longitude_deg": 5.0, "altitude_m": 0, "altitude_datum": "WGS84-ellipsoid", "placement": "illustrative"},
		"local_frame":       map[string]any{"units": "metres", "handedness": "right", "up": "Y", "axes": []string{"east", "up", "south"}},
		"mujoco":            map[string]any{"path": "model/main.xml", "sha256": checksum(model), "to_local_position": []string{"x", "z", "-y"}},
		"assets":            []any{map[string]any{"path": "model/parts/scene.xml", "sha256": checksum(asset), "size": len(asset)}},
		"description":       "Generic transfer fixture", "fidelity": map[string]any{"physics": "unknown"}, "asset_license": nil,
		"scenario": map[string]any{"inert": []any{true, nil, "metadata is not an execution instruction"}},
	}
	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return append(raw, '\n'), map[string][]byte{"model/main.xml": model, "model/parts/scene.xml": asset}
}

func TestGenericPackageVerifyInstallPreservesOpaqueBytes(t *testing.T) {
	raw, files := genericFixture(t)
	bundle, err := Verify(raw, files)
	if err != nil {
		t.Fatal(err)
	}
	if bundle.Manifest.SHA256 != checksum(raw) || !bytes.Equal(bundle.Root, raw) {
		t.Fatal("generic root was normalized")
	}
	target := filepath.Join(t.TempDir(), "generic")
	if err = Install(bundle, target); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(filepath.Join(target, "world.json"))
	if err != nil || loaded.Manifest.SHA256 != checksum(raw) || !bytes.Equal(loaded.Root, raw) {
		t.Fatal("installed generic root changed", err)
	}
	for name, want := range files {
		if !bytes.Equal(loaded.Files[name], want) {
			t.Fatalf("asset bytes changed for %s", name)
		}
		st, err := os.Stat(filepath.Join(target, name))
		if err != nil || st.Mode().Perm() != 0600 {
			t.Fatal("installed asset is not a private non-executable file", err)
		}
	}
	// All convenience metadata is distrusted again at installation time.
	bundle.Root = alter(t, bundle.Root, func(d map[string]any) { d["gas_model"] = map[string]any{} })
	if err = Install(bundle, filepath.Join(t.TempDir(), "rejected")); err == nil {
		t.Fatal("installation bypassed the generic schema")
	}
}

func TestGenericPackagePassiveInventoryAndLegacyCompatibility(t *testing.T) {
	raw, files := genericFixture(t)
	passive := alter(t, raw, func(d map[string]any) {
		delete(d, "assets")
		delete(d, "description")
		delete(d, "fidelity")
		delete(d, "asset_license")
		delete(d, "scenario")
	})
	if _, err := Verify(passive, map[string][]byte{"model/main.xml": files["model/main.xml"]}); err != nil {
		t.Fatal("passive package incorrectly requires optional application data", err)
	}
	legacy, legacyFiles := fixture(t)
	legacy = alter(t, legacy, func(d map[string]any) {
		d["custom_legacy_metadata"] = map[string]any{"enabled": true}
		d["mujoco"].(map[string]any)["body"] = "legacy-body"
	})
	got, err := Verify(legacy, legacyFiles)
	if err != nil || !bytes.Equal(got.Root, legacy) || got.Manifest.SHA256 != checksum(legacy) {
		t.Fatal("v0 compatibility or exact identity changed", err)
	}
	if _, err = Inspect(alter(t, legacy, func(d map[string]any) { d["schema_version"] = SchemaV1 })); err == nil {
		t.Fatal("v0 data was silently treated as generic v1")
	}
}

func TestGenericPackageRejectsFixtureBindingsAndGeographicClaims(t *testing.T) {
	raw, _ := genericFixture(t)
	for name, mutate := range map[string]func(map[string]any){
		"gas":             func(d map[string]any) { d["gas_model"] = map[string]any{"kind": "analytic-gaussian-fixture"} },
		"control":         func(d map[string]any) { d["mujoco"].(map[string]any)["control"] = 0 },
		"body":            func(d map[string]any) { d["mujoco"].(map[string]any)["body"] = "puck" },
		"actuator":        func(d map[string]any) { d["mujoco"].(map[string]any)["actuator"] = "drive" },
		"measured":        func(d map[string]any) { d["provenance"] = "measured" },
		"surveyed":        func(d map[string]any) { d["geographic_anchor"].(map[string]any)["placement"] = "surveyed" },
		"wrong transform": func(d map[string]any) { d["mujoco"].(map[string]any)["to_local_position"] = []string{"x", "y", "z"} },
		"missing anchor":  func(d map[string]any) { delete(d, "geographic_anchor") },
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Inspect(alter(t, raw, mutate)); err == nil {
				t.Fatal("unsupported generic claim accepted")
			}
		})
	}
}

func TestGenericPackageUnicodeScalarEscapes(t *testing.T) {
	raw, _ := genericFixture(t)
	needle := []byte(`"Generic transfer fixture"`)
	for _, value := range []string{`"\ud800\udc00"`, `"\udbff\udfff"`, `"literal \\ud800"`, `"quote \" then \\"`} {
		if _, err := Inspect(bytes.Replace(raw, needle, []byte(value), 1)); err != nil {
			t.Fatalf("valid Unicode JSON rejected %s: %v", value, err)
		}
	}
	for _, value := range []string{`"\ud800"`, `"\udfff"`, `"\ud800x"`, `"\ud800\u0061"`, `"\ud800\ud800"`, "\"bad\xff\""} {
		if _, err := Inspect(bytes.Replace(raw, needle, []byte(value), 1)); err == nil {
			t.Fatalf("non-scalar Unicode accepted %s", value)
		}
	}
}

func TestGenericPackageGeographicCoordinatesRequireNumbers(t *testing.T) {
	raw, _ := genericFixture(t)
	for _, coordinate := range []string{"latitude_deg", "longitude_deg", "altitude_m"} {
		for label, value := range map[string]any{"null": nil, "bool": false, "string": "0"} {
			t.Run(coordinate+"/"+label, func(t *testing.T) {
				changed := alter(t, raw, func(d map[string]any) { d["geographic_anchor"].(map[string]any)[coordinate] = value })
				if _, err := Inspect(changed); err == nil {
					t.Fatal("nonnumeric coordinate accepted")
				}
			})
		}
		zero := alter(t, raw, func(d map[string]any) { d["geographic_anchor"].(map[string]any)[coordinate] = 0 })
		if _, err := Inspect(zero); err != nil {
			t.Fatalf("explicit numeric zero refused for %s: %v", coordinate, err)
		}
	}
}
