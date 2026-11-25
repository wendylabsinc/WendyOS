//go:build world_simulation_faults

package tasking

import (
	"github.com/wendylabsinc/wendy/go/world/internal/mission"
	"github.com/wendylabsinc/wendy/go/world/internal/runtimeobs"
	"testing"
	"time"
)

func TestTaggedCommitDelayCannotReturnExpiredSupervision(t *testing.T) {
	r := setupSupervision(t)
	r.s.Close()
	started := time.Now()
	r.wall = started
	r.cfg.Now = time.Now
	r.cfg.Monotonic = func() time.Duration { return time.Second + time.Since(started) }
	metrics := runtimeobs.New()
	r.cfg.Metrics = metrics
	for name, o := range r.operators {
		o.ValidUntilUnixMS = started.Add(time.Hour).UnixMilli()
		r.operators[name] = o
	}
	var err error
	r.s, err = Open(r.path, r.cfg)
	if err != nil {
		t.Fatal(err)
	}
	r.epoch, err = r.s.BeginExecutor("app:nav")
	if err != nil {
		t.Fatal(err)
	}
	r.acceptSupervised(t, "delayed-task")
	challenge := r.challenge(t, "delayed-task", "operator-a", "real-clock")
	r.ack(t, challenge)
	telemetry := r.telemetry()
	telemetry.SampleUnixMS = time.Now().UnixMilli()
	if d := r.evaluate(t, "delayed-task", telemetry); d.Action != mission.Continue {
		t.Fatal(d)
	}
	if err = r.s.SetCommitDelayForSimulation(600 * time.Millisecond); err != nil {
		t.Fatal(err)
	}
	before := taskCommitMetric(t, metrics).Count
	telemetry.SampleUnixMS = time.Now().UnixMilli()
	decision, err := r.s.Evaluate("delayed-task", "app:nav", r.epoch, telemetry)
	if err != nil || decision.Action == mission.Continue || decision.Supervision != nil || decision.Record.Status.Phase != mission.Held {
		t.Fatal("delayed durable write released expired motion", decision, err)
	}
	if got := taskCommitMetric(t, metrics); got.Count < before+2 || got.MaxNS < uint64(600*time.Millisecond) {
		t.Fatal(got)
	}
	if err = r.s.SetCommitDelayForSimulation(0); err != nil || r.s.beforeCommit != nil {
		t.Fatal("delay not cleared", err)
	}
	r.s.cfg.Domain = mission.Physical
	if r.s.SetCommitDelayForSimulation(time.Millisecond) == nil {
		t.Fatal("physical store accepted delay")
	}
	r.s.cfg.Domain = mission.Simulation
	for _, bad := range []time.Duration{-1, time.Second + 1} {
		if r.s.SetCommitDelayForSimulation(bad) == nil {
			t.Fatal("invalid delay accepted")
		}
	}
}
