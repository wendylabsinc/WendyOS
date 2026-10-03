package containerd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/errdefs"
	"go.uber.org/zap"
)

// recordedKill captures one Kill call: the signal and whether the whole
// process group was targeted (containerd.WithKillAll).
type recordedKill struct {
	sig syscall.Signal
	all bool
}

// fakeTeardownTask implements teardownTask for unit tests. It records every
// Kill (signal + all-flag), closes its wait channel when killed with
// exitOnSignal, and lets tests inject Wait/Kill/Delete failures.
type fakeTeardownTask struct {
	mu    sync.Mutex
	kills []recordedKill

	// killErr, when set, is consulted for every Kill call.
	killErr func(sig syscall.Signal, all bool) error
	// exitOnSignal closes waitCh (task exits) after a successful Kill with
	// this signal. Zero means the task never exits from a signal.
	exitOnSignal syscall.Signal

	waitCh   chan containerd.ExitStatus
	waitErr  error
	exitOnce sync.Once

	deleteErr error
	// deleteErrs, when non-nil, overrides deleteErr with a per-call sequence
	// (index 0 = first Delete call, index 1 = second, ...); calls beyond the
	// slice length return nil. Lets tests model a Delete that fails once and
	// succeeds on retry, e.g. the missing-runc-state-dir recovery path.
	deleteErrs  []error
	deleteCalls int
	deleted     bool
}

func newFakeTeardownTask() *fakeTeardownTask {
	return &fakeTeardownTask{waitCh: make(chan containerd.ExitStatus, 1)}
}

func (f *fakeTeardownTask) exit() {
	f.exitOnce.Do(func() { close(f.waitCh) })
}

func (f *fakeTeardownTask) Kill(ctx context.Context, sig syscall.Signal, opts ...containerd.KillOpts) error {
	info := containerd.KillInfo{}
	for _, o := range opts {
		if err := o(ctx, &info); err != nil {
			return err
		}
	}
	f.mu.Lock()
	f.kills = append(f.kills, recordedKill{sig: sig, all: info.All})
	f.mu.Unlock()
	if f.killErr != nil {
		if err := f.killErr(sig, info.All); err != nil {
			return err
		}
	}
	if f.exitOnSignal != 0 && sig == f.exitOnSignal {
		f.exit()
	}
	return nil
}

func (f *fakeTeardownTask) Wait(ctx context.Context) (<-chan containerd.ExitStatus, error) {
	if f.waitErr != nil {
		return nil, f.waitErr
	}
	return f.waitCh, nil
}

func (f *fakeTeardownTask) Delete(ctx context.Context, opts ...containerd.ProcessDeleteOpts) (*containerd.ExitStatus, error) {
	// Do NOT run opts: the production code passes containerd.WithProcessKill,
	// which would Wait+Kill against this fake and distort the recorded kill
	// sequence the tests assert on. The fake models a delete that succeeds
	// (or fails via deleteErr/deleteErrs) regardless.
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted = true
	idx := f.deleteCalls
	f.deleteCalls++
	if f.deleteErrs != nil {
		if idx < len(f.deleteErrs) {
			return nil, f.deleteErrs[idx]
		}
		return nil, nil
	}
	return nil, f.deleteErr
}

func (f *fakeTeardownTask) recordedKills() []recordedKill {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]recordedKill(nil), f.kills...)
}

func newTeardownTestClient() *Client {
	return &Client{logger: zap.NewNop()}
}

func TestTerminateTaskKillsWholeProcessGroup(t *testing.T) {
	task := newFakeTeardownTask()
	task.exitOnSignal = syscall.SIGKILL
	c := newTeardownTestClient()

	err := c.terminateTask(context.Background(), task, "app", syscall.SIGKILL, time.Second, time.Second)
	if err != nil {
		t.Fatalf("terminateTask returned error: %v", err)
	}

	kills := task.recordedKills()
	if len(kills) != 1 {
		t.Fatalf("expected exactly 1 kill, got %d: %v", len(kills), kills)
	}
	if kills[0].sig != syscall.SIGKILL || !kills[0].all {
		t.Errorf("expected SIGKILL with all=true (whole cgroup), got sig=%v all=%v", kills[0].sig, kills[0].all)
	}
	if !task.deleted {
		t.Error("expected task to be deleted")
	}
}

func TestTerminateTaskEscalatesToSigkillAfterGrace(t *testing.T) {
	task := newFakeTeardownTask()
	task.exitOnSignal = syscall.SIGKILL // Ignores SIGTERM, dies on SIGKILL.
	c := newTeardownTestClient()

	err := c.terminateTask(context.Background(), task, "app", syscall.SIGTERM, 10*time.Millisecond, time.Second)
	if err != nil {
		t.Fatalf("terminateTask returned error: %v", err)
	}

	kills := task.recordedKills()
	if len(kills) != 2 {
		t.Fatalf("expected SIGTERM then SIGKILL, got %v", kills)
	}
	if kills[0].sig != syscall.SIGTERM || !kills[0].all {
		t.Errorf("first kill: expected SIGTERM all=true, got sig=%v all=%v", kills[0].sig, kills[0].all)
	}
	if kills[1].sig != syscall.SIGKILL || !kills[1].all {
		t.Errorf("second kill: expected SIGKILL all=true, got sig=%v all=%v", kills[1].sig, kills[1].all)
	}
	if !task.deleted {
		t.Error("expected task to be deleted")
	}
}

func TestTerminateTaskGracefulExitDoesNotEscalate(t *testing.T) {
	task := newFakeTeardownTask()
	task.exitOnSignal = syscall.SIGTERM
	c := newTeardownTestClient()

	err := c.terminateTask(context.Background(), task, "app", syscall.SIGTERM, time.Second, time.Second)
	if err != nil {
		t.Fatalf("terminateTask returned error: %v", err)
	}

	kills := task.recordedKills()
	if len(kills) != 1 {
		t.Fatalf("expected exactly 1 kill (no escalation), got %v", kills)
	}
	if kills[0].sig != syscall.SIGTERM || !kills[0].all {
		t.Errorf("expected SIGTERM all=true, got sig=%v all=%v", kills[0].sig, kills[0].all)
	}
	if !task.deleted {
		t.Error("expected task to be deleted")
	}
}

func TestTerminateReplacedTaskGivesOnlyNANAppGrace(t *testing.T) {
	oldGrace := nanReplacementGracePeriod
	nanReplacementGracePeriod = 5 * time.Millisecond
	t.Cleanup(func() { nanReplacementGracePeriod = oldGrace })
	for _, tc := range []struct {
		name   string
		hadNAN bool
		exitOn syscall.Signal
		want   []syscall.Signal
	}{
		{"NAN app cleans publish on SIGTERM", true, syscall.SIGTERM, []syscall.Signal{syscall.SIGTERM}},
		{"NAN app hangs and escalates", true, syscall.SIGKILL, []syscall.Signal{syscall.SIGTERM, syscall.SIGKILL}},
		{"ordinary app preserves immediate kill", false, syscall.SIGKILL, []syscall.Signal{syscall.SIGKILL}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			task := newFakeTeardownTask()
			task.exitOnSignal = tc.exitOn
			if err := newTeardownTestClient().terminateReplacedTask(context.Background(), task, "camera", tc.hadNAN); err != nil {
				t.Fatal(err)
			}
			kills := task.recordedKills()
			if len(kills) != len(tc.want) {
				t.Fatalf("kills = %v, want %v", kills, tc.want)
			}
			for i, signal := range tc.want {
				if kills[i].sig != signal || !kills[i].all {
					t.Fatalf("kill %d = %+v, want %s on whole group", i, kills[i], signal)
				}
			}
			if !task.deleted {
				t.Fatal("old task was not deleted")
			}
		})
	}
}

func TestTerminateTaskFallsBackToInitKillWhenGroupKillFails(t *testing.T) {
	task := newFakeTeardownTask()
	task.exitOnSignal = syscall.SIGKILL
	task.killErr = func(sig syscall.Signal, all bool) error {
		if all {
			return errors.New("kill --all unsupported")
		}
		return nil
	}
	c := newTeardownTestClient()

	err := c.terminateTask(context.Background(), task, "app", syscall.SIGKILL, time.Second, time.Second)
	if err != nil {
		t.Fatalf("terminateTask returned error: %v", err)
	}

	kills := task.recordedKills()
	if len(kills) != 2 {
		t.Fatalf("expected group kill then init fallback, got %v", kills)
	}
	if !kills[0].all {
		t.Errorf("first kill should target the whole group, got %v", kills[0])
	}
	if kills[1].all {
		t.Errorf("fallback kill should target init only, got %v", kills[1])
	}
	if kills[1].sig != syscall.SIGKILL {
		t.Errorf("fallback kill should preserve the signal, got %v", kills[1].sig)
	}
	if !task.deleted {
		t.Error("expected task to be deleted")
	}
}

func TestTerminateTaskTreatsGoneTaskAsSuccess(t *testing.T) {
	for name, killErr := range map[string]error{
		"not found":           fmt.Errorf("wrapped: %w", errdefs.ErrNotFound),
		"failed precondition": fmt.Errorf("wrapped: %w", errdefs.ErrFailedPrecondition),
	} {
		t.Run(name, func(t *testing.T) {
			task := newFakeTeardownTask()
			task.exit() // Already exited.
			task.killErr = func(syscall.Signal, bool) error { return killErr }
			c := newTeardownTestClient()

			err := c.terminateTask(context.Background(), task, "app", syscall.SIGKILL, time.Second, time.Second)
			if err != nil {
				t.Fatalf("terminateTask returned error: %v", err)
			}

			kills := task.recordedKills()
			if len(kills) != 1 {
				t.Fatalf("gone task must not trigger the init-only fallback, got %v", kills)
			}
			if !task.deleted {
				t.Error("expected task to be deleted")
			}
		})
	}
}

func TestTerminateTaskReturnsDeleteError(t *testing.T) {
	task := newFakeTeardownTask()
	task.exitOnSignal = syscall.SIGKILL
	task.deleteErr = errors.New("shim wedged")
	c := newTeardownTestClient()

	err := c.terminateTask(context.Background(), task, "app", syscall.SIGKILL, time.Second, time.Second)
	if err == nil {
		t.Fatal("expected delete error to be surfaced, got nil")
	}
	if !strings.Contains(err.Error(), "deleting task") || !strings.Contains(err.Error(), "shim wedged") {
		t.Errorf("unexpected error: %v", err)
	}
}

// TestTerminateTaskRecoversFromMissingRuncStateDir is the stop-path
// regression guard for finding 3 (F2 round 1 follow-up): the
// missing-runc-state-dir recovery previously only existed in the replace
// path's forceDeleteTask, so stopOne -> terminateTask -> task.Delete
// propagated the runc error with no recovery — the actual wedge observed on
// hardware, since `wendy device apps stop` goes through this exact call.
// Uses the literal error string captured live, and swaps runcStateDirMkdirAll
// (the seam recreateRuncStateDirAndRetry calls through) so the test doesn't
// need real /run/containerd/runc/ filesystem access to prove the wiring.
func TestTerminateTaskRecoversFromMissingRuncStateDir(t *testing.T) {
	orig := runcStateDirMkdirAll
	var mkdirCalls []string
	runcStateDirMkdirAll = func(path string, perm os.FileMode) error {
		mkdirCalls = append(mkdirCalls, path)
		return nil
	}
	t.Cleanup(func() { runcStateDirMkdirAll = orig })

	task := newFakeTeardownTask()
	task.exitOnSignal = syscall.SIGKILL
	const dir = "/run/containerd/runc/default/com.wendylabs.examples.mcp-example"
	task.deleteErrs = []error{
		errors.New("cannot open directory `" + dir + "`: No such file or directory"),
		nil, // recovery retry succeeds
	}
	c := newTeardownTestClient()

	err := c.terminateTask(context.Background(), task, "com.wendylabs.examples.mcp-example", syscall.SIGKILL, time.Second, time.Second)
	if err != nil {
		t.Fatalf("terminateTask returned error: %v", err)
	}
	if len(mkdirCalls) != 1 || mkdirCalls[0] != dir {
		t.Fatalf("mkdir calls = %v, want exactly [%s]", mkdirCalls, dir)
	}
	if task.deleteCalls != 2 {
		t.Fatalf("Delete called %d times, want 2 (initial failure + recovery retry)", task.deleteCalls)
	}
}

// TestTerminateTaskMissingRuncStateDirRecoveryFailurePropagates verifies
// that when the recovery retry ALSO fails, terminateTask still surfaces an
// error (the recovery is a best-effort extra attempt, not a guarantee).
func TestTerminateTaskMissingRuncStateDirRecoveryFailurePropagates(t *testing.T) {
	orig := runcStateDirMkdirAll
	runcStateDirMkdirAll = func(path string, perm os.FileMode) error { return nil }
	t.Cleanup(func() { runcStateDirMkdirAll = orig })

	task := newFakeTeardownTask()
	task.exitOnSignal = syscall.SIGKILL
	const dir = "/run/containerd/runc/default/com.wendylabs.examples.mcp-example"
	stillWedged := errors.New("cannot open directory `" + dir + "`: No such file or directory")
	task.deleteErrs = []error{stillWedged, stillWedged}
	c := newTeardownTestClient()

	err := c.terminateTask(context.Background(), task, "com.wendylabs.examples.mcp-example", syscall.SIGKILL, time.Second, time.Second)
	if err == nil {
		t.Fatal("expected the still-failing retry's error to be surfaced, got nil")
	}
	if task.deleteCalls != 2 {
		t.Fatalf("Delete called %d times, want 2 (initial failure + recovery retry)", task.deleteCalls)
	}
}

func TestTerminateTaskNotFoundDeleteIsSuccess(t *testing.T) {
	task := newFakeTeardownTask()
	task.exitOnSignal = syscall.SIGKILL
	task.deleteErr = fmt.Errorf("wrapped: %w", errdefs.ErrNotFound)
	c := newTeardownTestClient()

	if err := c.terminateTask(context.Background(), task, "app", syscall.SIGKILL, time.Second, time.Second); err != nil {
		t.Fatalf("NotFound delete must be treated as success, got: %v", err)
	}
}

func TestTerminateTaskWaitErrorStillKillsAndDeletes(t *testing.T) {
	task := newFakeTeardownTask()
	task.waitErr = errors.New("wait broken")
	c := newTeardownTestClient()

	err := c.terminateTask(context.Background(), task, "app", syscall.SIGTERM, time.Second, time.Second)
	if err != nil {
		t.Fatalf("terminateTask returned error: %v", err)
	}

	kills := task.recordedKills()
	if len(kills) != 2 {
		t.Fatalf("expected SIGTERM then immediate SIGKILL escalation when exit cannot be observed, got %v", kills)
	}
	if kills[0].sig != syscall.SIGTERM || kills[1].sig != syscall.SIGKILL {
		t.Errorf("unexpected kill sequence: %v", kills)
	}
	if !task.deleted {
		t.Error("expected task to be deleted despite wait failure")
	}
}
