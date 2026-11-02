package jobops

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRequestedDigestsRequireVerifiedManifestAndUnchangedLocalBytes(t *testing.T) {
	root := t.TempDir()
	raw := []byte("verified snapshot")
	manifest, _ := json.Marshal(map[string]any{"files": []map[string]string{{"path": "report.json", "sha256": digest(raw)}}})
	os.WriteFile(filepath.Join(root, ".wendy-artifacts-manifest.json"), manifest, 0600)
	os.WriteFile(filepath.Join(root, "report.json"), raw, 0600)
	artifacts := map[string]any{"manifest_sha256": digest(manifest)}
	selected := []string{"report.json"}
	got, err := selectedArtifactDigests(root, artifacts, selected)
	if err != nil || got["report.json"] != digest(raw) {
		t.Fatalf("missing requested verified digest: %v %+v", err, got)
	}
	os.WriteFile(filepath.Join(root, "report.json"), []byte("changed after collection"), 0600)
	if _, err := selectedArtifactDigests(root, artifacts, selected); err == nil {
		t.Fatal("returned stale digest after local bytes changed")
	}
	os.WriteFile(filepath.Join(root, ".wendy-artifacts-manifest.json"), []byte(`{}`), 0600)
	if _, err := selectedArtifactDigests(root, artifacts, selected); err == nil {
		t.Fatal("accepted replaced manifest")
	}
}

func TestDigestSelectionIsExplicitUniqueCollectedAndBoundedBeforeExecution(t *testing.T) {
	files := []string{"a", "b"}
	for _, selected := range [][]string{{"outside"}, {"a", "a"}, make([]string, 17)} {
		if err := validateArtifactDigests(files, selected); err == nil {
			t.Fatalf("accepted invalid digest request: %q", selected)
		}
	}
	long := strings.Repeat("\n", 1000)
	if err := validateArtifactDigests([]string{long}, []string{long}); err != nil {
		t.Fatal("valid bounded JSON-escaped name refused", err)
	}
	long = strings.Repeat("\n", 2100)
	if err := validateArtifactDigests([]string{long}, []string{long}); err == nil {
		t.Fatal("escaped JSON exceeded encoded budget")
	}
	if err := ValidateSubmitOptions(SubmitOptions{ArtifactDigests: []string{"a"}}); err == nil {
		t.Fatal("digest-only request bypassed the collection contract")
	}
}
