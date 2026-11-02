package mcp

import (
	"context"
	"encoding/json"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestExperimentPlanningIsLocalAndKeepsExecutionProfileSmall(t *testing.T) {
	s := New(&config.Config{}, nil)
	srv, err := s.newProtocolServer()
	if err != nil {
		t.Fatal(err)
	}
	tool := srv.GetTool("experiment_plan")
	if tool == nil || *tool.Tool.Annotations.ReadOnlyHint || *tool.Tool.Annotations.OpenWorldHint {
		t.Fatal("planner missing or falsely labelled passive/networked")
	}
	dir := t.TempDir()
	recipe := filepath.Join(dir, "recipe.json")
	dest := filepath.Join(dir, "workflow.json")
	raw := `{"schema":"wendy.experiment.v1","parameters":{"seed":["1","2"]},"workflow":{"schema":"wendy.workflow.v1","job":{"name":"pair","working_directory":"/owned/project","steps":[{"name":"eval-{{seed}}","argv":["python3","eval.py","{{seed}}"],"timeout_seconds":60}]},"wait_seconds":0}}`
	os.WriteFile(recipe, []byte(raw), 0600)
	r, err := s.handleExperimentPlan(context.Background(), callToolReq("experiment_plan", map[string]any{"recipe_path": recipe, "destination": dest}))
	if err != nil || r.IsError {
		t.Fatalf("local plan required a device: %v %+v", err, r)
	}
	if _, err := os.Stat(dest); err != nil {
		t.Fatal(err)
	}
	r, err = s.handleExperimentPlan(context.Background(), callToolReq("experiment_plan", map[string]any{"recipe_path": recipe, "destination": dest}))
	if err != nil || !r.IsError {
		t.Fatal("planner overwrote workflow")
	}
	for _, profile := range []string{"training", "workflow"} {
		if err := s.SetToolProfile(profile); err != nil {
			t.Fatal(err)
		}
		found := false
		for _, tool := range protocolTools(t, srv) {
			if tool.Name == "experiment_plan" {
				found = true
			}
		}
		if found != (profile == "training") {
			t.Fatalf("planner changed warm workflow catalog for %s", profile)
		}
	}
}

func TestJobInspectRequiresExplicitDirectoryAndConnection(t *testing.T) {
	s := New(&config.Config{}, nil)
	for _, args := range []map[string]any{{}, {"job_path": 1}, {"job_path": "/tmp/job", "since": 2}, {"job_path": "/tmp/job", "wait_seconds": 1.5}} {
		r, err := s.handleJobInspect(context.Background(), callToolReq("job_inspect", args))
		if err != nil || !r.IsError {
			t.Fatalf("accepted invalid arguments: %v %v", r, err)
		}
	}
	r, err := s.handleJobInspect(context.Background(), callToolReq("job_inspect", map[string]any{"job_path": "/tmp/job"}))
	if err != nil || !r.IsError {
		t.Fatalf("unconnected job read: %v %v", r, err)
	}
}
func TestJobInspectDiscoveryAndHostPolicy(t *testing.T) {
	s := New(&config.Config{}, nil)
	srv, err := s.newProtocolServer()
	if err != nil {
		t.Fatal(err)
	}
	tool := srv.GetTool("job_inspect")
	if tool == nil {
		t.Fatal("job inspection not registered")
	}
	if !*tool.Tool.Annotations.ReadOnlyHint || readOnlyTool(tool.Tool) {
		t.Fatal("host read must retain explicit relay host authorization")
	}
	if !slices.Contains(toolGroups["observability"], "job_inspect") {
		t.Fatal("tool adds default schema overhead")
	}
	for _, visible := range protocolTools(t, srv) {
		if visible.Name == "job_inspect" {
			t.Fatal("job tool eagerly loaded in default catalog")
		}
	}
}

func TestTrainingProfileBoundsCatalogWithoutChangingDefaultOrAuthority(t *testing.T) {
	s := New(&config.Config{}, nil)
	if err := s.SetToolProfile("training"); err != nil {
		t.Fatal(err)
	}
	srv, err := s.newProtocolServer()
	if err != nil {
		t.Fatal(err)
	}
	tools := protocolTools(t, srv)
	if len(tools) != 11 {
		t.Fatalf("training catalog contains %d tools", len(tools))
	}
	for _, tool := range tools {
		if !slices.Contains(trainingTools, tool.Name) {
			t.Fatal("irrelevant tool advertised", tool.Name)
		}
	}
	if err = s.SetToolProfile("typo"); err == nil {
		t.Fatal("unknown profile accepted")
	}
	if err = s.SetToolProfile(""); err != nil {
		t.Fatal(err)
	}
	if len(protocolTools(t, srv)) != 16 {
		t.Fatal("profile changed default discovery")
	}
	if readOnlyTool(srv.GetTool("job_inspect").Tool) {
		t.Fatal("catalog profile granted host authority")
	}
}

func TestWorkflowResultHasOneParseableJSONRepresentation(t *testing.T) {
	r := jobJSON(map[string]any{"reported_state": "running", "process": map[string]any{"status": "missing"}, "iteration": 64})
	if r.IsError || len(r.Content) != 1 || r.StructuredContent != nil {
		t.Fatal("duplicate or unusable workflow representation")
	}
	encoded, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	if len(encoded) > 220 || strings.Count(string(encoded), "reported_state") != 1 {
		t.Fatalf("workflow observation duplicated: %s", encoded)
	}
}

func TestSubmitFileReferenceValidatesBeforeDeviceAccess(t *testing.T) {
	s := New(&config.Config{}, nil)
	for _, args := range []map[string]any{
		{}, {"spec": map[string]any{}, "spec_path": "/tmp/spec.json"},
		{"spec_path": "relative.json"}, {"spec_path": 3},
		{"spec_path": "/tmp/spec.json", "wait_seconds": 3601},
		{"spec": map[string]any{}, "wait_seconds": 0.5},
		{"spec_path": "/tmp/spec.json", "artifact_files": []string{"status.json"}},
		{"spec_path": "/tmp/spec.json", "wait_seconds": 1, "artifact_files": []string{"../escape"}, "artifact_destination": "/tmp/results"},
		{"spec_path": "/tmp/spec.json", "wait_seconds": 1, "artifact_files": []any{3}, "artifact_destination": "/tmp/results"},
		{"spec_path": "/tmp/spec.json", "wait_seconds": 1, "artifact_destination": 3},
	} {
		r, err := s.handleJobSubmit(context.Background(), callToolReq("job_submit", args))
		encoded, _ := json.Marshal(r)
		if err != nil || !r.IsError || strings.Contains(string(encoded), "NOT_CONNECTED") {
			t.Fatalf("invalid request reached connection: %v %s", err, encoded)
		}
	}
	name := filepath.Join(t.TempDir(), "spec.json")
	os.WriteFile(name, []byte(`{"name":"test","working_directory":"/tmp/source","steps":[{"name":"check","argv":["python3"],"timeout_seconds":1}]}`), 0600)
	r, err := s.handleJobSubmit(context.Background(), callToolReq("job_submit", map[string]any{"spec_path": name, "wait_seconds": 1}))
	encoded, _ := json.Marshal(r)
	if err != nil || !strings.Contains(string(encoded), "NOT_CONNECTED") {
		t.Fatalf("valid file did not reach connection check: %v %s", err, encoded)
	}
}

func TestWorkflowProfileHasSmallSchemaAndPreservesHostAuthorization(t *testing.T) {
	s := New(&config.Config{}, nil)
	if err := s.SetToolProfile("workflow"); err != nil {
		t.Fatal(err)
	}
	srv, err := s.newProtocolServer()
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range protocolTools(t, srv) {
		if tool.Name != "job_submit" {
			continue
		}
		if len(tool.InputSchema.Properties) != 1 || len(tool.InputSchema.Required) != 1 || tool.InputSchema.Required[0] != "workflow_path" {
			t.Fatalf("workflow profile exposes full low-level schema: %+v", tool.InputSchema)
		}
		if readOnlyTool(tool) || *tool.Annotations.ReadOnlyHint {
			t.Fatal("profile granted execution authority")
		}
		encoded, _ := json.Marshal(tool)
		if len(encoded) > 1000 {
			t.Fatalf("workflow schema grew unexpectedly: %d bytes", len(encoded))
		}
	}
	for _, args := range []map[string]any{{"workflow_path": 3}, {"workflow_path": "relative.json"}, {"workflow_path": "/tmp/workflow.json", "spec": map[string]any{}}} {
		r, err := s.handleJobSubmit(context.Background(), callToolReq("job_submit", args))
		if err != nil || !r.IsError {
			t.Fatalf("accepted ambiguous workflow: %v %+v", err, r)
		}
	}
}

func TestCheckpointInspectorDoesNotGainAutomaticReadAuthority(t *testing.T) {
	s := New(&config.Config{}, nil)
	srv, err := s.newProtocolServer()
	if err != nil {
		t.Fatal(err)
	}
	tool := srv.GetTool("checkpoint_inspect").Tool
	if readOnlyTool(tool) || (&BindingPolicy{AllowRead: true}).allows(tool) {
		t.Fatal("explicit host runtime bypassed host authorization")
	}
	if err := CheckToolNames([]string{"checkpoint_inspect"}); err != nil {
		t.Fatal(err)
	}
	result, err := s.handleCheckpointInspect(context.Background(), callToolReq("checkpoint_inspect", map[string]any{"checkpoint_path": "/tmp/a.pt", "sha256": strings.Repeat("a", 64), "python": "/tmp/python", "source_directory": 7}))
	if err != nil || !result.IsError {
		t.Fatalf("invalid source directory reached a host: %v %+v", err, result)
	}
}
