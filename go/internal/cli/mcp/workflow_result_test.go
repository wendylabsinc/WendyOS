package mcp

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestCompactSuccessPreservesDecisionsAndDefersOnlySavedProvenance(t *testing.T) {
	full := map[string]any{
		"reported_state": "completed", "job_path": "/owned/job", "target": "pinned", "reused": false,
		"runner_pid": 123, "runner_sha256": strings.Repeat("a", 64), "spec_sha256": strings.Repeat("b", 64),
		"receipt_path": "/local/receipt.json", "receipt_sha256": strings.Repeat("c", 64),
		"source_check": map[string]any{"status": "passed", "scope": "original_submission"},
		"observation": map[string]any{"reported_state": "completed", "step_check": "passed", "expected_steps": 1,
			"step_outcomes":     []any{map[string]any{"label": "evaluate", "returncode": 0, "timed_out": false, "duration_seconds": 1.5}},
			"execution_seconds": 1.6, "transport_retries": 2, "warnings": []string{"preserve this warning"}, "process": map[string]any{"pid": 123}, "report": map[string]any{"path": "/owned/job/status.json", "sha256": strings.Repeat("d", 64)}},
		"artifacts": map[string]any{"complete": true, "verified_files": 2, "unchanged_files": 0, "manifest_path": "/local/manifest", "manifest_sha256": strings.Repeat("e", 64)},
		"reports":   map[string]any{"evaluate/result.json": map[string]any{"sha256": strings.Repeat("f", 64), "fields": map[string]any{"/successes": 7}, "unavailable": map[string]string{"/optional": "missing_pointer"}}},
	}
	before, _ := json.Marshal(full)
	compact := compactWorkflowResult(full)
	after, _ := json.Marshal(full)
	if string(before) != string(after) {
		t.Fatal("compaction mutated the saved full evidence")
	}
	for _, key := range []string{"reported_state", "job_path", "target", "receipt_path", "receipt_sha256", "source_check"} {
		if !reflect.DeepEqual(compact[key], full[key]) {
			t.Fatalf("lost decision/reference: %s", key)
		}
	}
	obs := compact["observation"].(map[string]any)
	if !reflect.DeepEqual(obs["step_outcomes"], full["observation"].(map[string]any)["step_outcomes"]) || obs["warnings"] == nil || obs["transport_retries"] != 2 {
		t.Fatal("lost exits, durations or warnings")
	}
	r := compact["reports"].(map[string]any)["evaluate/result.json"].(map[string]any)
	if r["fields"] == nil || !reflect.DeepEqual(r["unavailable"], map[string]string{"/optional": "missing_pointer"}) || r["sha256"] != nil {
		t.Fatal("lost explicit unknowns or repeated deferred provenance")
	}
	encoded, _ := json.Marshal(compact)
	if len(encoded) >= len(before) {
		t.Fatal("reply did not shrink")
	}
	for _, change := range []func(){
		func() { full["reported_state"] = "failed" },
		func() { full["reported_state"] = "completed"; full["artifact_error"] = "missing report" },
		func() { delete(full, "artifact_error"); full["report_error"] = "changed manifest" },
		func() { delete(full, "report_error"); full["receipt_sha256"] = "" },
	} {
		change()
		if compactWorkflowResult(full)["runner_pid"] != 123 {
			t.Fatal("incomplete/error evidence was compacted")
		}
	}
}
