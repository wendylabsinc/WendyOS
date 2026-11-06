package containerd

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestHostShutdownStopsTasksInParallel(t *testing.T) {
	// Each task ignores SIGTERM, like a Python app running as its
	// container's PID 1. Stopped one after another they would take n grace
	// periods; host shutdown must take about one.
	const n = 5
	const grace = 200 * time.Millisecond
	tasks := make([]shutdownTask, n)
	fakes := make([]*fakeTeardownTask, n)
	for i := range tasks {
		fakes[i] = newFakeTeardownTask()
		fakes[i].exitOnSignal = syscall.SIGKILL
		tasks[i] = shutdownTask{containerID: fmt.Sprintf("app%d", i), task: fakes[i]}
	}
	c := newTeardownTestClient()

	start := time.Now()
	if err := c.terminateTasksConcurrently(context.Background(), tasks, grace, time.Second); err != nil {
		t.Fatalf("terminateTasksConcurrently() = %v", err)
	}
	// One after another takes at least n grace periods; allow all but one of
	// them for a slow runner.
	if elapsed := time.Since(start); elapsed >= (n-1)*grace {
		t.Errorf("stopping %d tasks took %v; want about one grace period (%v), not one each", n, elapsed, grace)
	}
	for i, f := range fakes {
		kills := f.recordedKills()
		if len(kills) != 2 || kills[0] != (recordedKill{syscall.SIGTERM, true}) || kills[1] != (recordedKill{syscall.SIGKILL, true}) {
			t.Errorf("task %d kills = %v, want SIGTERM then SIGKILL to the whole group", i, kills)
		}
		if !f.deleted {
			t.Errorf("task %d was not deleted", i)
		}
	}
}

func TestHostShutdownDoesNotWaitForAppsThatExitOnSIGTERM(t *testing.T) {
	task := newFakeTeardownTask()
	task.exitOnSignal = syscall.SIGTERM
	c := newTeardownTestClient()

	start := time.Now()
	err := c.terminateTasksConcurrently(context.Background(),
		[]shutdownTask{{containerID: "app", task: task}}, 10*time.Second, time.Second)
	if err != nil {
		t.Fatalf("terminateTasksConcurrently() = %v", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("an app that exits on SIGTERM held shutdown for %v", elapsed)
	}
	if kills := task.recordedKills(); len(kills) != 1 {
		t.Errorf("kills = %v, want SIGTERM only", kills)
	}
}

func TestHostShutdownKeepsRestartsSuppressed(t *testing.T) {
	// The restart monitor must not start an app again between its task dying
	// and the agent exiting, so the suppression is never lifted.
	sup := &fakeRestartSuppressor{}
	c := newTeardownTestClient()
	c.SetRestartSuppressor(sup)
	a, b := newFakeTeardownTask(), newFakeTeardownTask()
	a.exitOnSignal, b.exitOnSignal = syscall.SIGTERM, syscall.SIGTERM

	err := c.terminateTasksConcurrently(context.Background(), []shutdownTask{
		{containerID: "com.example.app", task: a},
		{containerID: "com.example.multi_web", task: b},
	}, time.Second, time.Second)
	if err != nil {
		t.Fatalf("terminateTasksConcurrently() = %v", err)
	}
	if got := strings.Join(sup.suppressed, ","); got != "com.example.app,com.example.multi_web" {
		t.Errorf("suppressed = %q, want both containers", got)
	}
	if len(sup.resumed) != 0 {
		t.Errorf("restarts were resumed for %v during host shutdown", sup.resumed)
	}
}

func TestHostShutdownReportsEveryFailureAndStillStopsTheRest(t *testing.T) {
	bad, good := newFakeTeardownTask(), newFakeTeardownTask()
	bad.exitOnSignal, good.exitOnSignal = syscall.SIGTERM, syscall.SIGTERM
	bad.deleteErr = errors.New("delete wedged")
	c := newTeardownTestClient()

	err := c.terminateTasksConcurrently(context.Background(), []shutdownTask{
		{containerID: "bad", task: bad},
		{containerID: "good", task: good},
	}, time.Second, time.Second)
	if err == nil || !strings.Contains(err.Error(), "delete wedged") {
		t.Fatalf("err = %v, want the failed delete reported", err)
	}
	if !good.deleted {
		t.Error("one task's failure kept another from being stopped")
	}
}

func TestNoAppStartsOnceHostShutdownBegins(t *testing.T) {
	// Neither a draining deploy RPC nor a restart already under way may
	// start an app after the stop sweep has listed the running ones.
	c := &Client{namespace: "default"}
	c.hostShutdown.Store(true)
	if _, err := c.StartContainer(context.Background(), "com.example.app", "", nil); !errors.Is(err, errHostShuttingDown) {
		t.Errorf("StartContainer error = %v; want errHostShuttingDown", err)
	}
	if _, err := c.StartContainerWithStdin(context.Background(), "com.example.app", strings.NewReader("input"), "", nil); !errors.Is(err, errHostShuttingDown) {
		t.Errorf("StartContainerWithStdin error = %v; want errHostShuttingDown", err)
	}
}

func TestHasWendyLabel(t *testing.T) {
	for _, tc := range []struct {
		name   string
		labels map[string]string
		want   bool
	}{
		{"app", map[string]string{labelKeyAppVersion: "1.0.0"}, true},
		{"ROS 2 sidecar", map[string]string{labelKeyROS2Sidecar: "humble"}, true},
		{"host inspector", map[string]string{labelKeyROS2InspectorVersion: "1"}, true},
		{"someone else's container", map[string]string{"nerdctl/name": "db"}, false},
		{"no labels", nil, false},
	} {
		if got := hasWendyLabel(tc.labels); got != tc.want {
			t.Errorf("%s: hasWendyLabel() = %v, want %v", tc.name, got, tc.want)
		}
	}
}
