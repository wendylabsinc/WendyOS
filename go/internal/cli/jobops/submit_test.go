package jobops_test

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/jobops"
)

func TestSubmitRejectsReservedStepNameBeforeCreatingJob(t *testing.T) {
	working := t.TempDir()
	spec := jobops.Spec{Name: "reserved-step", WorkingDirectory: working, Steps: []jobops.Step{{Name: "runner", Argv: []string{"python3", "-c", "print('must not run')"}, TimeoutSeconds: 2}}}
	c := connection(t, &host{})
	receipt, err := jobops.Submit(context.Background(), c, spec)
	if err == nil {
		state := waitForJob(t, receipt["job_path"].(string))
		t.Fatalf("reserved step reached execution instead of validation: %+v", state)
	}
	if !strings.Contains(err.Error(), "reserved") {
		t.Fatalf("want reserved-name error, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(working, ".wendy")); !os.IsNotExist(err) {
		t.Fatal("reserved step created job storage")
	}
	// The reservation applies only to steps, not the enclosing job name.
	spec.Name, spec.Steps[0].Name = "runner", "train"
	if err := jobops.ValidateSpec(spec); err != nil {
		t.Fatalf("valid job name rejected: %v", err)
	}
}

func waitForJob(t *testing.T, root string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(6 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(filepath.Join(root, "status.json"))
		var state map[string]any
		if err == nil && json.Unmarshal(data, &state) == nil && (state["state"] == "completed" || state["state"] == "failed" || state["state"] == "stopped") {
			return state
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("job did not reach a terminal state")
	return nil
}
func TestSubmitRunsOwnedStepsAndRetriesDoNotRestart(t *testing.T) {
	c := connection(t, &host{})
	working := t.TempDir()
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip(err)
	}
	spec := jobops.Spec{Name: "paired-pilot", WorkingDirectory: working, Steps: []jobops.Step{{Name: "check", Argv: []string{python, "-c", "print('checked')"}, TimeoutSeconds: 2}, {Name: "evaluate", Argv: []string{python, "-c", "print('evaluated')"}, TimeoutSeconds: 2}}}
	first, err := jobops.Submit(context.Background(), c, spec)
	if err != nil {
		t.Fatal(err)
	}
	state := waitForJob(t, first["job_path"].(string))
	if state["state"] != "completed" || len(state["stages"].([]any)) != 2 {
		t.Fatalf("wrong job result: %+v", state)
	}
	again, err := jobops.Submit(context.Background(), c, spec)
	if err != nil {
		t.Fatal(err)
	}
	if again["runner_pid"] != first["runner_pid"] || again["reused"] != true {
		t.Fatalf("retry restarted job: %v %v", first, again)
	}
	spec.Steps[0].Argv = []string{python, "-c", "print('different')"}
	if _, err = jobops.Submit(context.Background(), c, spec); err == nil {
		t.Fatal("existing job overwritten")
	}
}
func TestSubmitSourceMismatchFailsBeforeCreatingJob(t *testing.T) {
	c := connection(t, &host{})
	working := t.TempDir()
	os.WriteFile(filepath.Join(working, "checkpoint.bin"), []byte("preserved"), 0600)
	spec := jobops.Spec{Name: "wrong-checkpoint", WorkingDirectory: working, SourceHashes: map[string]string{"checkpoint.bin": "0000000000000000000000000000000000000000000000000000000000000000"}, Steps: []jobops.Step{{Name: "check", Argv: []string{"python3", "-c", "print('should not run')"}, TimeoutSeconds: 1}}}
	if _, err := jobops.Submit(context.Background(), c, spec); err == nil {
		t.Fatal("source mismatch accepted")
	}
	if _, err := os.Stat(filepath.Join(working, ".wendy")); !os.IsNotExist(err) {
		t.Fatal("source mismatch created a job")
	}
	data, _ := os.ReadFile(filepath.Join(working, "checkpoint.bin"))
	if string(data) != "preserved" {
		t.Fatal("checkpoint changed")
	}
}
func TestSubmitTimeoutRecordsFailureAndDoesNotRunNextStep(t *testing.T) {
	c := connection(t, &host{})
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip(err)
	}
	spec := jobops.Spec{Name: "bounded", WorkingDirectory: t.TempDir(), Steps: []jobops.Step{{Name: "slow", Argv: []string{python, "-c", "import time;time.sleep(10)"}, TimeoutSeconds: 1}, {Name: "later", Argv: []string{python, "-c", "print('must not run')"}, TimeoutSeconds: 1}}}
	receipt, err := jobops.Submit(context.Background(), c, spec)
	if err != nil {
		t.Fatal(err)
	}
	root := receipt["job_path"].(string)
	state := waitForJob(t, root)
	if state["state"] != "failed" || len(state["stages"].([]any)) != 1 {
		t.Fatalf("timeout not recorded: %+v", state)
	}
	if _, err = os.Stat(filepath.Join(root, "later.log")); !os.IsNotExist(err) {
		t.Fatal("dependent step ran after timeout")
	}
}

func TestSubmitBackgroundDescendantsCannotLookLikeCompletion(t *testing.T) {
	c := connection(t, &host{})
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip(err)
	}
	source := "import subprocess;subprocess.Popen([" + "'" + python + "'" + ",'-c','import time;time.sleep(30)'])"
	spec := jobops.Spec{Name: "background-child", WorkingDirectory: t.TempDir(), Steps: []jobops.Step{{Name: "unexpected-daemon", Argv: []string{python, "-c", source}, TimeoutSeconds: 2}}}
	receipt, err := jobops.Submit(context.Background(), c, spec)
	if err != nil {
		t.Fatal(err)
	}
	state := waitForJob(t, receipt["job_path"].(string))
	if state["state"] != "failed" {
		t.Fatalf("background workload claimed completed: %+v", state)
	}
}
