package commands

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestJobPlanWorksOfflineAndNeverOverwrites(t *testing.T) {
	dir := t.TempDir()
	recipe := filepath.Join(dir, "recipe.json")
	dest := filepath.Join(dir, "workflow.json")
	os.WriteFile(recipe, []byte(`{"schema":"wendy.experiment.v1","parameters":{"seed":["1","2"]},"workflow":{"schema":"wendy.workflow.v1","job":{"name":"pair","working_directory":"/owned/project","steps":[{"name":"eval-{{seed}}","argv":["python3","eval.py","{{seed}}"],"timeout_seconds":60}]},"wait_seconds":0}}`), 0600)
	var output bytes.Buffer
	c := newJobCmd()
	c.SetOut(&output)
	c.SetArgs([]string{"plan", recipe, "--output", dest})
	if err := c.Execute(); err != nil {
		t.Fatal(err)
	}
	var result map[string]any
	if err := json.Unmarshal(output.Bytes(), &result); err != nil || result["steps"] != float64(2) || result["execution_started"] != false {
		t.Fatalf("invalid plan receipt: %v %s", err, output.String())
	}
	c = newJobCmd()
	c.SetArgs([]string{"plan", recipe, "--output", dest})
	if err := c.Execute(); err == nil {
		t.Fatal("overwrote workflow")
	}
}

func TestJobInspectInvalidWaitFailsWithoutDeviceDiscovery(t *testing.T) {
	c := newJobCmd()
	c.SetArgs([]string{"inspect", "/tmp/job", "--wait", "1"})
	c.SetContext(context.Background())
	if err := c.Execute(); err == nil || !strings.Contains(err.Error(), "requires a since cursor") {
		t.Fatalf("invalid wait reached connection: %v", err)
	}
}
func TestJobCommandsDocumentAgentFacingFlags(t *testing.T) {
	c := newJobCmd()
	var out bytes.Buffer
	c.SetOut(&out)
	c.SetArgs([]string{"inspect", "--help"})
	if err := c.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "--since") || !strings.Contains(out.String(), "--wait") {
		t.Fatal(out.String())
	}
	c = newJobCmd()
	out.Reset()
	c.SetOut(&out)
	c.SetArgs([]string{"artifacts", "pull", "--help"})
	if err := c.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "--file") || !strings.Contains(out.String(), "--output") {
		t.Fatal(out.String())
	}
	c = newJobCmd()
	out.Reset()
	c.SetOut(&out)
	c.SetArgs([]string{"submit", "--help"})
	if err := c.Execute(); err != nil {
		t.Fatal(err)
	}
	for _, flag := range []string{"--wait", "--artifact", "--output", "--report-field", "--artifact-digest", "--workflow"} {
		if !strings.Contains(out.String(), flag) {
			t.Fatal(out.String())
		}
	}
}

func TestJobSubmitRejectsBadCollectionBeforeReadingSpecOrConnecting(t *testing.T) {
	c := newJobCmd()
	c.SetArgs([]string{"submit", "does-not-exist.json", "--artifact", "../escape", "--output", "/tmp/results", "--wait", "1"})
	if err := c.Execute(); err == nil || !strings.Contains(err.Error(), "safe relative paths") {
		t.Fatalf("invalid collection reached file/device access: %v", err)
	}
}

func TestCheckpointInspectValidatesRuntimeBeforeDeviceDiscovery(t *testing.T) {
	cmd := newJobCmd()
	cmd.SetArgs([]string{"checkpoint", "inspect", "/tmp/checkpoint.pt", "--sha256", strings.Repeat("a", 64), "--python", "python3"})
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "absolute remote paths") {
		t.Fatalf("invalid runtime reached device discovery: %v", err)
	}
}

func TestJobSubmitRejectsUncollectedDigestBeforeSpecOrDeviceAccess(t *testing.T) {
	c := newJobCmd()
	c.SetArgs([]string{"submit", "does-not-exist.json", "--artifact", "a", "--artifact-digest", "b", "--output", "/tmp/results", "--wait", "1"})
	if err := c.Execute(); err == nil || !strings.Contains(err.Error(), "artifact_digests") {
		t.Fatalf("invalid digest selection reached file/device access: %v", err)
	}
}
