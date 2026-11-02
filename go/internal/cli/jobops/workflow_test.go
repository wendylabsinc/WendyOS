package jobops_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/cli/jobops"
)

func TestSubmitWorkflowCollectsVerifiedReportsAndRetriesSkipThem(t *testing.T) {
	c := connection(t, &host{})
	spec := jobops.Spec{Name: "collect", WorkingDirectory: t.TempDir(), Steps: []jobops.Step{{Name: "check", Argv: []string{"python3", "-c", "import os,pathlib;pathlib.Path(os.environ['WENDY_STEP_DIR'],'result.json').write_text('{\"passed\":true}')"}, TimeoutSeconds: 2}}}
	opts := jobops.SubmitOptions{WaitSeconds: 3, ArtifactFiles: []string{"status.json", "check/result.json"}, ArtifactDestination: filepath.Join(t.TempDir(), "artifacts")}
	first, err := jobops.SubmitWorkflow(context.Background(), c, spec, opts)
	if err != nil {
		t.Fatal(err)
	}
	if first["observation"].(map[string]any)["reported_state"] != "completed" || first["artifacts"].(map[string]any)["verified_files"] != 2 {
		t.Fatalf("incomplete workflow: %+v", first)
	}
	data, err := os.ReadFile(filepath.Join(opts.ArtifactDestination, "check/result.json"))
	if err != nil || string(data) != `{"passed":true}` {
		t.Fatalf("bad retrieved result: %v %s", err, data)
	}
	second, err := jobops.SubmitWorkflow(context.Background(), c, spec, opts)
	if err != nil || second["reused"] != true || first["runner_pid"] != second["runner_pid"] || second["artifacts"].(map[string]any)["unchanged_files"] != 2 {
		t.Fatalf("retry changed outputs: %v %+v", err, second)
	}
	if _, err = os.Stat(filepath.Join(opts.ArtifactDestination, ".wendy-artifacts-manifest.json")); err != nil {
		t.Fatal(err)
	}
}

func TestSubmitWorkflowRefusesBadCollectionBeforeLaunching(t *testing.T) {
	h := &host{}
	c := connection(t, h)
	for _, opts := range []jobops.SubmitOptions{
		{WaitSeconds: 3601}, {WaitSeconds: 1, ArtifactDestination: t.TempDir()},
		{WaitSeconds: 1, ArtifactDestination: "relative", ArtifactFiles: []string{"status.json"}},
		{ArtifactDestination: t.TempDir(), ArtifactFiles: []string{"status.json"}},
		{WaitSeconds: 1, ArtifactDestination: t.TempDir(), ArtifactFiles: []string{"../escape"}},
		{WaitSeconds: 1, ArtifactDestination: t.TempDir(), ArtifactFiles: []string{"status.json", "status.json"}},
	} {
		if _, err := jobops.SubmitWorkflow(context.Background(), c, jobops.Spec{}, opts); err == nil {
			t.Fatalf("invalid options accepted: %+v", opts)
		}
	}
	if h.calls.Load() != 0 {
		t.Fatal("bad collection started remote work")
	}
}

func TestSubmitWorkflowRetainsCompletedReceiptWhenArtifactMissing(t *testing.T) {
	c := connection(t, &host{})
	spec := jobops.Spec{Name: "missing-report", WorkingDirectory: t.TempDir(), Steps: []jobops.Step{{Name: "check", Argv: []string{"python3", "-c", "print('done')"}, TimeoutSeconds: 2}}}
	r, err := jobops.SubmitWorkflow(context.Background(), c, spec, jobops.SubmitOptions{WaitSeconds: 3, ArtifactFiles: []string{"missing.json"}, ArtifactDestination: filepath.Join(t.TempDir(), "out")})
	if err != nil || r["job_path"] == nil || r["artifact_error"] == nil || r["receipt_path"] == nil || r["receipt_sha256"] == nil || r["artifacts"] != nil || r["observation"].(map[string]any)["reported_state"] != "completed" {
		t.Fatalf("lost job identity or claimed collection success: %v %+v", err, r)
	}
	data, _ := os.ReadFile(filepath.Join(r["job_path"].(string), "status.json"))
	var state map[string]any
	json.Unmarshal(data, &state)
	if state["state"] != "completed" {
		t.Fatal("collection failure changed completed job")
	}
}

func TestSubmitWorkflowDoesNotCollectWhileJobStillRunning(t *testing.T) {
	c := connection(t, &host{})
	spec := jobops.Spec{Name: "still-running", WorkingDirectory: t.TempDir(), Steps: []jobops.Step{{Name: "check", Argv: []string{"python3", "-c", "import time;time.sleep(2)"}, TimeoutSeconds: 5}}}
	destination := filepath.Join(t.TempDir(), "must-not-exist")
	r, err := jobops.SubmitWorkflow(context.Background(), c, spec, jobops.SubmitOptions{WaitSeconds: 1, ArtifactFiles: []string{"status.json"}, ArtifactDestination: destination})
	if err != nil || r["artifacts_pending"] != true || r["artifacts"] != nil {
		t.Fatalf("collected incomplete job: %v %+v", err, r)
	}
	if _, err = os.Stat(destination); !os.IsNotExist(err) {
		t.Fatal("pending collection wrote local files")
	}
	if state := waitForJob(t, r["job_path"].(string)); state["state"] != "completed" {
		t.Fatal("wait expiry stopped job")
	}
}
