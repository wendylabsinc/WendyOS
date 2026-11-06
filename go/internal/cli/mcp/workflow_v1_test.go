package mcp

import (
	"encoding/json"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"reflect"
	"strings"
	"testing"
)

func v1Receipt() map[string]any {
	return map[string]any{"reported_state": "completed", "job_path": "/owned/job", "runner_pid": 123, "receipt_path": "/local/receipt", "receipt_sha256": strings.Repeat("a", 64), "source_check": map[string]any{"verified_files": 3, "scope": "original_submission"}, "observation": map[string]any{"step_check": "passed", "expected_steps": 2, "step_outcomes_omitted": 0, "step_outcomes": []any{map[string]any{"label": "train", "returncode": 0, "timed_out": false}, map[string]any{"label": "evaluate", "returncode": float64(0), "timed_out": false}}, "transport_retries": 2, "execution_seconds": 3.5, "warnings": []string{"keep me"}}, "artifacts": map[string]any{"complete": true, "verified_files": 3, "manifest_path": "/local/manifest", "manifest_sha256": strings.Repeat("b", 64)}, "reports": map[string]any{"result.json": map[string]any{"fields": map[string]any{"/successes": 8}, "unavailable": map[string]string{"/missing": "missing_pointer"}}}}
}

func TestV1CatalogDoesNotChangePermissionOrPreviousProfiles(t *testing.T) {
	s := New(&config.Config{}, nil)
	if err := s.SetToolProfile("workflow-v1"); err != nil {
		t.Fatal(err)
	}
	srv, err := s.newProtocolServer()
	if err != nil {
		t.Fatal(err)
	}
	tools := protocolTools(t, srv)
	if len(tools) != 4 {
		t.Fatalf("unfocused V1 catalog: %d", len(tools))
	}
	for _, tool := range tools {
		if tool.Name == "job_submit" {
			if len(tool.InputSchema.Properties) != 1 || tool.InputSchema.Required[0] != "workflow_path" || *tool.Annotations.ReadOnlyHint {
				t.Fatal("V1 changed submission authority/schema")
			}
		}
	}
	if readOnlyTool(srv.GetTool("job_inspect").Tool) {
		t.Fatal("V1 granted automatic host read authority")
	}
	for profile, want := range map[string]int{"workflow": 9, "training": 11, "": 16} {
		if err := s.SetToolProfile(profile); err != nil {
			t.Fatal(err)
		}
		if len(protocolTools(t, srv)) != want {
			t.Fatalf("changed %s catalog", profile)
		}
	}
}

func TestV1DisconnectedStatusExplainsRequiredSetupWithoutInventingTools(t *testing.T) {
	s := New(&config.Config{}, nil)
	if err := s.SetToolProfile("workflow-v1"); err != nil {
		t.Fatal(err)
	}
	out := statusOf(t, s)
	next := out["suggested_next_step"].(string)
	if !strings.Contains(next, "--device") || strings.Contains(next, "Call device_list") || out["connected"] != false {
		t.Fatalf("V1 suggested hidden setup tools: %+v", out)
	}
}

func TestV1SummaryRequiresCompleteKnownOutcomesAndPreservesFullEvidence(t *testing.T) {
	r := v1Receipt()
	before, _ := json.Marshal(r)
	out := compactWorkflowV1Result(r)
	after, _ := json.Marshal(r)
	if string(before) != string(after) || out["candidate"] != "Wendy MCP V1" || out["runner_pid"] != nil {
		t.Fatal("lost full evidence or no concise V1 result")
	}
	obs := out["observation"].(map[string]any)
	if obs["zero_exit_steps"] != 2 || obs["timed_out_steps"] != 0 || obs["transport_retries"] != 2 || obs["warnings"] == nil || out["receipt_sha256"] != r["receipt_sha256"] {
		t.Fatalf("lost decisive facts: %+v", out)
	}
	report := out["reports"].(map[string]any)["result.json"].(map[string]any)
	if !reflect.DeepEqual(report["unavailable"], map[string]string{"/missing": "missing_pointer"}) {
		t.Fatal("lost unknown")
	}
	for name, change := range map[string]func(map[string]any){
		"failed":           func(r map[string]any) { r["reported_state"] = "failed" },
		"missing_receipt":  func(r map[string]any) { delete(r, "receipt_sha256") },
		"collection_error": func(r map[string]any) { r["artifact_error"] = "failed verification" },
		"omitted":          func(r map[string]any) { r["observation"].(map[string]any)["step_outcomes_omitted"] = 1 },
		"wrong_count":      func(r map[string]any) { r["observation"].(map[string]any)["expected_steps"] = 3 },
		"fractional_count": func(r map[string]any) { r["observation"].(map[string]any)["expected_steps"] = 2.5 },
		"unknown_code": func(r map[string]any) {
			delete(r["observation"].(map[string]any)["step_outcomes"].([]any)[0].(map[string]any), "returncode")
		},
		"nonzero": func(r map[string]any) {
			r["observation"].(map[string]any)["step_outcomes"].([]any)[0].(map[string]any)["returncode"] = 7
		},
		"unknown_timeout": func(r map[string]any) {
			delete(r["observation"].(map[string]any)["step_outcomes"].([]any)[0].(map[string]any), "timed_out")
		},
		"duplicate": func(r map[string]any) {
			r["observation"].(map[string]any)["step_outcomes"].([]any)[0].(map[string]any)["label"] = "evaluate"
		},
	} {
		t.Run(name, func(t *testing.T) {
			r := v1Receipt()
			change(r)
			if compactWorkflowV1Result(r)["runner_pid"] != 123 {
				t.Fatal("compacted unverified/inconsistent outcome")
			}
		})
	}
}
