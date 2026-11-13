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

// A workload that appends to WENDY_STEP_DIR/beat until it dies. It ends on its
// own, so a failing test never has to signal a PID it does not own.
const heartbeat = `
for _ in range(1000):
 with open(os.environ['WENDY_STEP_DIR']+'/beat','a') as f: f.write('.')
 time.sleep(.02)
`

func requireHeartbeatStopped(t *testing.T, path string) {
	t.Helper()
	size := func() int64 {
		info, err := os.Stat(path)
		if err != nil {
			return -1
		}
		return info.Size()
	}
	before := size()
	time.Sleep(300 * time.Millisecond)
	if before <= 0 {
		t.Fatal("workload never started")
	}
	if size() != before {
		t.Fatal("workload is still running after its job reached a terminal state")
	}
}

func TestSubmitWorkloadLeadsItsOwnProcessGroup(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip(err)
	}
	// GNU timeout as argv[0] calls setpgid(0, 0) and ignores the session
	// leader's EPERM. It must not thereby leave the group its deadline signals.
	t.Run("setpgid", func(t *testing.T) {
		source := "import os,time\ntry: os.setpgid(0,0)\nexcept OSError: pass" + heartbeat
		spec := jobops.Spec{Name: "own-group", WorkingDirectory: t.TempDir(), Steps: []jobops.Step{{Name: "slow", Argv: []string{python, "-c", source}, TimeoutSeconds: 1}}}
		receipt, err := jobops.Submit(context.Background(), connection(t, &host{}), spec)
		if err != nil {
			t.Fatal(err)
		}
		root := receipt["job_path"].(string)
		if state := waitForJob(t, root); state["state"] != "failed" || state["stages"].([]any)[0].(map[string]any)["timed_out"] != true {
			t.Fatalf("deadline not recorded: %+v", state)
		}
		requireHeartbeatStopped(t, filepath.Join(root, "slow", "beat"))
	})
	// A workload signalling its own group (kill 0) must not stop its supervisor.
	t.Run("kill-0", func(t *testing.T) {
		spec := jobops.Spec{Name: "signals-own-group", WorkingDirectory: t.TempDir(), Steps: []jobops.Step{{Name: "cleanup", Argv: []string{"sh", "-c", `trap "" TERM; kill 0; exit 0`}, TimeoutSeconds: 5}, {Name: "later", Argv: []string{"sh", "-c", "exit 0"}, TimeoutSeconds: 5}}}
		receipt, err := jobops.Submit(context.Background(), connection(t, &host{}), spec)
		if err != nil {
			t.Fatal(err)
		}
		if state := waitForJob(t, receipt["job_path"].(string)); state["state"] != "completed" || len(state["stages"].([]any)) != 2 {
			t.Fatalf("workload's own group signal failed the job: %+v", state)
		}
	})
}

func TestSubmitDeadlineSignalsWorkloadOnceAndRecordsItsStatus(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip(err)
	}
	// Checkpoint on the first SIGTERM, abort on a second one.
	source := `import os,pathlib,signal,time
terms=[]
def term(_signal,_frame):
 terms.append(1)
 if len(terms)>1: os._exit(70)
signal.signal(signal.SIGTERM,term)
while not terms: time.sleep(.02)
time.sleep(.5)
pathlib.Path(os.environ['WENDY_STEP_DIR'],'checkpoint').write_text('saved')
`
	spec := jobops.Spec{Name: "checkpoint-on-term", WorkingDirectory: t.TempDir(), Steps: []jobops.Step{{Name: "train", Argv: []string{python, "-c", source}, TimeoutSeconds: 1}}}
	receipt, err := jobops.Submit(context.Background(), connection(t, &host{}), spec)
	if err != nil {
		t.Fatal(err)
	}
	root := receipt["job_path"].(string)
	state := waitForJob(t, root)
	stage := state["stages"].([]any)[0].(map[string]any)
	if state["state"] != "failed" || stage["timed_out"] != true || stage["returncode"] != float64(0) {
		t.Fatalf("deadline did not record the workload's own exit status: %+v", state)
	}
	if data, _ := os.ReadFile(filepath.Join(root, "train", "checkpoint")); string(data) != "saved" {
		t.Fatal("workload was signalled again before its checkpoint finished")
	}
}
