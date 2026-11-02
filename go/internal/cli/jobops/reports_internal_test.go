package jobops

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestProjectionNeverReturnsChangedOrMalformedReportAsFact(t *testing.T) {
	root := t.TempDir()
	original := []byte(`{"successes":3}`)
	manifest, _ := json.Marshal(map[string]any{"files": []map[string]string{{"path": "result.json", "sha256": digest(original)}}})
	os.WriteFile(filepath.Join(root, ".wendy-artifacts-manifest.json"), manifest, 0600)
	os.WriteFile(filepath.Join(root, "result.json"), []byte(`{"successes":999}`), 0600)
	artifacts := map[string]any{"manifest_sha256": digest(manifest)}
	r, err := summarizeReports(root, artifacts, map[string][]string{"result.json": {"/successes"}})
	if err != nil {
		t.Fatal(err)
	}
	report := r["result.json"].(map[string]any)
	if len(report["fields"].(map[string]any)) != 0 || report["unavailable"].(map[string]string)["/successes"] != "report_hash_mismatch" {
		t.Fatalf("unverified fact returned: %+v", r)
	}
	os.WriteFile(filepath.Join(root, ".wendy-artifacts-manifest.json"), []byte(`{}`), 0600)
	if _, err = summarizeReports(root, artifacts, map[string][]string{"result.json": {"/successes"}}); err == nil {
		t.Fatal("unverified manifest accepted")
	}
}

func TestStepCheckDoesNotTurnMissingOrInconsistentExitsIntoSuccess(t *testing.T) {
	spec := Spec{Steps: []Step{{Name: "evaluate"}}}
	for _, obs := range []map[string]any{
		{"reported_state": "completed"},
		{"reported_state": "completed", "step_outcomes": []any{map[string]any{"label": "other", "returncode": float64(0), "timed_out": false}}, "step_outcomes_omitted": float64(0)},
		{"reported_state": "completed", "step_outcomes": []any{map[string]any{"label": "evaluate", "returncode": float64(0)}}, "step_outcomes_omitted": float64(0)},
	} {
		if checkStepOutcomes(obs, spec) == "passed" {
			t.Fatalf("invented successful exits: %+v", obs)
		}
	}
}
