package tasking

import (
	"encoding/json"
	"errors"
	"github.com/wendylabsinc/wendy/go/world/internal/runtimeobs"
	"strings"
	"testing"
)

func taskCommitMetric(t *testing.T, c *runtimeobs.Collector) runtimeobs.DurationSnapshot {
	t.Helper()
	for _, v := range c.Snapshot().Durations {
		if v.Name == "task_store.commit" {
			return v
		}
	}
	t.Fatal("missing task commit metric")
	return runtimeobs.DurationSnapshot{}
}
func taskFailureMetric(t *testing.T, c *runtimeobs.Collector) uint64 {
	t.Helper()
	for _, v := range c.Snapshot().Counters {
		if v.Name == "task_store.commit_failures" {
			return v.Value
		}
	}
	t.Fatal("missing task failure counter")
	return 0
}
func TestTaskCommitMetricsDistinguishReadDuplicateAndWrite(t *testing.T) {
	r := setup(t)
	metrics := runtimeobs.New()
	r.s.cfg.Metrics = metrics
	r.accept(t, "private-task-id")
	if _, err := r.s.Get("private-task-id"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.s.List(); err != nil {
		t.Fatal(err)
	}
	if _, err := r.s.AuditLog(); err != nil {
		t.Fatal(err)
	}
	r.accept(t, "private-task-id")
	if got := taskCommitMetric(t, metrics).Count; got != 1 {
		t.Fatal("read or duplicate counted as commit", got)
	}
	r.evaluate(t, "private-task-id", r.telemetry())
	r.evaluate(t, "private-task-id", r.telemetry())
	// Measure the current full-checkpoint baseline even for unchanged decisions.
	if got := taskCommitMetric(t, metrics); got.Count != 3 || got.MaxNS == 0 {
		t.Fatal(got)
	}
	raw, _ := json.Marshal(metrics.Snapshot())
	for _, secret := range []string{"private-task-id", r.path, r.cfg.ExecutorPrincipal} {
		if strings.Contains(string(raw), secret) {
			t.Fatal("private task data entered metrics")
		}
	}
	r.s.cfg.Metrics = nil
	r.evaluate(t, "private-task-id", r.telemetry())
	if taskCommitMetric(t, metrics).Count != 3 {
		t.Fatal("disabled metrics changed")
	}
}
func TestTaskCommitFailureMetricsPreserveFailClosedStore(t *testing.T) {
	r := setup(t)
	metrics := runtimeobs.New()
	r.s.cfg.Metrics = metrics
	r.s.write = func(string, []byte) error { return errors.New("injected private-path failure") }
	if _, err := r.s.Accept(r.spec("not-accepted"), r.auth()); err == nil {
		t.Fatal("write failure accepted")
	}
	if taskCommitMetric(t, metrics).Count != 1 || taskFailureMetric(t, metrics) != 1 {
		t.Fatal(metrics.Snapshot())
	}
	if _, exists := r.s.state.Records["not-accepted"]; exists {
		t.Fatal("failed commit changed admitted state")
	}
	if _, err := r.s.Accept(r.spec("second"), r.auth()); err == nil {
		t.Fatal("faulted store admitted work")
	}
	if taskCommitMetric(t, metrics).Count != 1 {
		t.Fatal("pre-admission refusal counted as commit")
	}
}
