package command

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/wendylabsinc/wendy/go/world/internal/sitepub"
	"github.com/wendylabsinc/wendy/go/world/internal/worldpackage"
)

func writeGenericPackageTest(t *testing.T, p *packageTestServer) (string, []byte, []byte) {
	t.Helper()
	// The transfer command neither interprets nor executes MJCF. Its external
	// bytes are retained exactly for the separate compiler/runner boundary.
	model := []byte(`<mujoco><worldbody><body name="arm"><joint type="hinge"/><geom type="capsule" size=".03 .1"/></body></worldbody></mujoco>`)
	digest := sha256.Sum256(model)
	value := map[string]any{
		"schema_version": worldpackage.SchemaV1, "site_id": "synthetic-office", "package_revision": "generic-publish-v1", "frame_revision": "office-frame-v1", "provenance": "synthetic",
		"geographic_anchor": map[string]any{"latitude_deg": 52, "longitude_deg": 5, "altitude_m": 0, "altitude_datum": "WGS84-ellipsoid", "placement": "illustrative"},
		"local_frame":       map[string]any{"units": "metres", "handedness": "right", "up": "Y", "axes": []string{"east", "up", "south"}},
		"mujoco":            map[string]any{"path": "model.xml", "sha256": hex.EncodeToString(digest[:]), "to_local_position": []string{"x", "z", "-y"}},
		"fidelity":          map[string]any{"physics": "unvalidated test fixture"},
	}
	raw, err := json.MarshalIndent(value, "", " ")
	if err != nil {
		t.Fatal(err)
	}
	raw = append(raw, '\n')
	dir := filepath.Join(p.dir, "generic-source")
	if err = os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string][]byte{"world.json": raw, "model.xml": model} {
		if err = os.WriteFile(filepath.Join(dir, name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	siteBytes, err := os.ReadFile(p.site)
	if err != nil {
		t.Fatal(err)
	}
	var site map[string]any
	if err = json.Unmarshal(siteBytes, &site); err != nil {
		t.Fatal(err)
	}
	rootDigest := sha256.Sum256(raw)
	site["package_sha256"] = hex.EncodeToString(rootDigest[:])
	writeSiteJSON(t, p.site, site)
	return filepath.Join(dir, "world.json"), raw, model
}

func TestGenericPackageSignedPublishFetchPreservesV1(t *testing.T) {
	p := newPackageTestServer(t)
	path, raw, model := writeGenericPackageTest(t, p)
	var published bytes.Buffer
	if err := runPackage([]string{"publish", "--credentials", p.operatorFile, "--site", p.site, "--package", path, "--run-id", "generic-test", "--ttl", "5m"}, &published); err != nil {
		t.Fatal(err)
	}
	var result struct {
		Publication sitepub.Publication `json:"publication"`
		Published   bool                `json:"published"`
	}
	if err := json.Unmarshal(published.Bytes(), &result); err != nil || !result.Published {
		t.Fatal("generic publication unavailable", err)
	}
	target := filepath.Join(p.dir, "generic-installed")
	if err := fetchPackageTest(p, result.Publication.SHA256, target, new(bytes.Buffer)); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string][]byte{"world.json": raw, "model.xml": model} {
		got, err := os.ReadFile(filepath.Join(target, name))
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("signed transfer changed %s: %v", name, err)
		}
	}
}

func TestGenericPackageSchemaRefusedBeforeUpload(t *testing.T) {
	p := newPackageTestServer(t)
	path, raw, _ := writeGenericPackageTest(t, p)
	var value map[string]any
	if err := json.Unmarshal(raw, &value); err != nil {
		t.Fatal(err)
	}
	value["mujoco"].(map[string]any)["control"] = 0
	writeSiteJSON(t, path, value)
	var out bytes.Buffer
	if err := runPackage([]string{"publish", "--credentials", p.operatorFile, "--site", p.site, "--package", path, "--run-id", "generic-test"}, &out); err == nil || p.requests != 0 || out.Len() != 0 {
		t.Fatal("invalid generic package reached upload", err, p.requests)
	}
}
