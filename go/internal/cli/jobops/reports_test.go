package jobops_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/cli/jobops"
)

func TestWorkflowReturnsCompleteBoundedEvidenceAndSavedReceipt(t *testing.T) {
	c := connection(t, &host{})
	spec := jobops.Spec{Name: "evidence", WorkingDirectory: t.TempDir(), Steps: []jobops.Step{
		{Name: "check", Argv: []string{"python3", "-c", "print('checked')"}, TimeoutSeconds: 2},
		{Name: "evaluate", Argv: []string{"python3", "-c", `import json,os,pathlib;pathlib.Path(os.environ['WENDY_STEP_DIR'],'result.json').write_text(json.dumps({'successes':7,'episodes':8,'a/b':{'~key':True},'array':[3],'private_payload':'x'*10000}))`}, TimeoutSeconds: 2},
	}}
	opts := jobops.SubmitOptions{WaitSeconds: 3, ArtifactFiles: []string{"status.json", "evaluate/result.json"}, ArtifactDestination: filepath.Join(t.TempDir(), "reports"), ReportFields: map[string][]string{"evaluate/result.json": {"/successes", "/episodes", "/a~1b/~0key", "/array/0", "/missing", "/array", "/private_payload"}}}
	r, err := jobops.SubmitWorkflow(context.Background(), c, spec, opts)
	if err != nil {
		t.Fatal(err)
	}
	obs := r["observation"].(map[string]any)
	steps := obs["step_outcomes"].([]any)
	if obs["reported_state"] != "completed" || len(steps) != 2 || obs["step_outcomes_omitted"] != float64(0) || obs["step_check"] != "passed" {
		t.Fatalf("missing complete step evidence: %+v", r)
	}
	for _, step := range steps {
		if step.(map[string]any)["returncode"] != float64(0) {
			t.Fatalf("wrong exit: %+v", step)
		}

		if duration, ok := step.(map[string]any)["duration_seconds"].(float64); !ok || duration <= 0 {
			t.Fatalf("missing monotonic step timing: %+v", step)
		}
	}
	encoded, _ := json.Marshal(r["reports"])
	if strings.Contains(string(encoded), strings.Repeat("x", 100)) || len(encoded) > 2000 {
		t.Fatalf("raw report leaked into context: %.200s", encoded)
	}
	var reports map[string]struct {
		SHA256      string            `json:"sha256"`
		Fields      map[string]any    `json:"fields"`
		Unavailable map[string]string `json:"unavailable"`
	}
	if err = json.Unmarshal(encoded, &reports); err != nil {
		t.Fatal(err)
	}
	q := reports["evaluate/result.json"]
	if q.Fields["/successes"] != float64(7) || q.Fields["/episodes"] != float64(8) || q.Fields["/a~1b/~0key"] != true || q.Fields["/array/0"] != float64(3) {
		t.Fatalf("incorrect field projection: %s", encoded)
	}
	for _, field := range []string{"/missing", "/array", "/private_payload"} {
		if q.Unavailable[field] == "" {
			t.Fatalf("silently invented or omitted unknown field: %s", encoded)
		}
	}
	raw, err := os.ReadFile(filepath.Join(opts.ArtifactDestination, "evaluate/result.json"))
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(raw)
	if q.SHA256 != hex.EncodeToString(digest[:]) {
		t.Fatal("summary not tied to retrieved report hash")
	}
	receipt, err := os.ReadFile(r["receipt_path"].(string))
	if err != nil {
		t.Fatal(err)
	}
	digest = sha256.Sum256(receipt)
	if r["receipt_sha256"] != hex.EncodeToString(digest[:]) {
		t.Fatal("saved receipt checksum mismatch")
	}
	var saved map[string]any
	if json.Unmarshal(receipt, &saved) != nil || saved["reports"] == nil || saved["observation"] == nil {
		t.Fatal("saved evidence is incomplete")
	}
	before, _ := os.Stat(filepath.Join(opts.ArtifactDestination, "evaluate/result.json"))
	again, err := jobops.SubmitWorkflow(context.Background(), c, spec, opts)
	after, _ := os.Stat(filepath.Join(opts.ArtifactDestination, "evaluate/result.json"))
	if err != nil || again["reused"] != true || again["runner_pid"] != r["runner_pid"] || !before.ModTime().Equal(after.ModTime()) {
		t.Fatalf("evidence retry re-executed or recopied: %v %+v", err, again)
	}
}

func TestReportSelectionRefusedBeforeHostWork(t *testing.T) {
	h := &host{}
	c := connection(t, h)
	base := jobops.SubmitOptions{WaitSeconds: 1, ArtifactFiles: []string{"result.json"}, ArtifactDestination: t.TempDir()}
	for _, fields := range []map[string][]string{
		{"../escape": {"/x"}}, {"not-selected.json": {"/x"}}, {"result.json": {"invalid"}}, {"result.json": {"/bad~2pointer"}}, {"result.json": {"/x", "/x"}},
	} {
		opts := base
		opts.ReportFields = fields
		if _, err := jobops.SubmitWorkflow(context.Background(), c, jobops.Spec{}, opts); err == nil {
			t.Fatalf("invalid report selection accepted: %+v", fields)
		}
	}
	if h.calls.Load() != 0 {
		t.Fatal("invalid report selection reached host")
	}
}

func TestFailedWorkflowReturnsBoundedDiagnosticWithoutExtraLogCall(t *testing.T) {
	c := connection(t, &host{})
	spec := jobops.Spec{Name: "diagnostic", WorkingDirectory: t.TempDir(), Steps: []jobops.Step{{Name: "resume", Argv: []string{"python3", "-c", "print('x'*10000);raise AssertionError('Resume source mismatch')"}, TimeoutSeconds: 2}}}
	r, err := jobops.SubmitWorkflow(context.Background(), c, spec, jobops.SubmitOptions{WaitSeconds: 3})
	if err != nil {
		t.Fatal(err)
	}
	obs := r["observation"].(map[string]any)
	diagnostic, ok := obs["diagnostic"].(map[string]any)
	if !ok {
		t.Fatalf("missing failure diagnosis: %+v", r)
	}
	excerpt, _ := diagnostic["excerpt"].(string)
	if r["reported_state"] != "failed" || diagnostic["step"] != "resume" || !strings.Contains(excerpt, "Resume source mismatch") || len(excerpt) > 2048 || diagnostic["excerpt_sha256"] == "" || !strings.HasSuffix(diagnostic["log_path"].(string), "resume.log") {
		t.Fatalf("unbounded/missing diagnostic: %+v", r)
	}
}
