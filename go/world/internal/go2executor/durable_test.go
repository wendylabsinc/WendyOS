package go2executor

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/mission"
	"github.com/wendylabsinc/wendy/go/world/internal/tasking"
)

// This test supplies trusted authorization directly. Signed network admission
// belongs to tasking/node tests; here the real durable generation/fallback is
// exercised through the same Gateway interface used by NodeGateway.
type storeGateway struct{ s *tasking.Store }

func (g storeGateway) Begin(ctx context.Context) (uint64, error) {
	return g.s.BeginExecutorContext(ctx, "executor")
}
func (g storeGateway) Tasks(ctx context.Context) ([]tasking.Record, error) {
	return g.s.ListContext(ctx)
}
func (g storeGateway) Evaluate(ctx context.Context, id string, epoch uint64, t tasking.Telemetry) (tasking.Decision, error) {
	return g.s.EvaluateContext(ctx, id, "executor", epoch, t)
}
func TestDurableFallbackAndRestartNeverResume(t *testing.T) {
	for _, mode := range []string{"accepted-before-start", "sensor-loss", "restart"} {
		t.Run(mode, func(t *testing.T) {
			c, g, a, clock := fixture(t)
			checks := 0
			cfg := tasking.Config{Site: c.cfg.Site, Assignee: g.record.Spec.Assignee, ExecutorPrincipal: "executor", Domain: mission.Simulation, RunID: c.cfg.Binding.RunID, Go2Binding: &c.cfg.Binding, Catalog: map[string]mission.SafetyClass{tasking.Go2GoToType: mission.Move}, Now: func() time.Time { return clock.wall }, Monotonic: func() time.Duration { return clock.mono }, MaxLease: 500 * time.Millisecond, MaxTelemetryAge: 250 * time.Millisecond, ValidateAuthority: func(tasking.Record) error { checks++; return nil }}
			path := filepath.Join(t.TempDir(), "tasks.json")
			s, err := tasking.Open(path, cfg)
			if err != nil {
				t.Fatal(err)
			}
			accept := func() {
				t.Helper()
				if _, err = s.Accept(g.record.Spec, tasking.Authorization{Principal: "operator", Operator: true, AllowAutonomy: true, ValidUntil: clock.wall.Add(time.Minute), Proof: []byte(`{"test_only":true}`)}); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "accepted-before-start" {
				accept()
			}
			c.gateway = storeGateway{s}
			if err = c.Start(context.Background()); err != nil {
				t.Fatal(err)
			}
			if mode != "accepted-before-start" {
				accept()
			}
			if _, err = c.Tick(context.Background()); err != nil {
				t.Fatal(err)
			}
			if mode == "accepted-before-start" {
				if len(a.commands) != 0 || !c.finished {
					t.Fatal("previous accepted task resumed after executor generation changed")
				}
			} else if len(a.commands) != 1 || checks < 2 {
				t.Fatal("motion bypassed fresh mission checks")
			}
			if mode == "sensor-loss" {
				a.stateHook = func(s *State) { s.Ready = false }
				if d, e := c.Tick(context.Background()); e == nil || d.Action != mission.Stop {
					t.Fatal("sensor loss returned motion")
				}
				r, e := s.Get(c.cfg.TaskID)
				if e != nil || r.Status.Phase != mission.Failed {
					t.Fatalf("fallback not durable: %+v %v", r.Status, e)
				}
			}
			if err = s.Close(); err != nil {
				t.Fatal(err)
			}
			s, err = tasking.Open(path, cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			r, err := s.Get(c.cfg.TaskID)
			if err != nil || r.Status.Phase != mission.Failed {
				t.Fatalf("reopened motion not failed: %+v %v", r.Status, err)
			}
			c2, _, a2, _ := fixture(t)
			c2.gateway = storeGateway{s}
			if err = c2.Start(context.Background()); err != nil {
				t.Fatal(err)
			}
			if d, e := c2.Tick(context.Background()); e != nil || d.Action != mission.Stop || d.Record.Status.Phase != mission.Failed {
				t.Fatalf("restarted task resumed: %+v %v", d, e)
			}
			if len(a2.commands) != 0 {
				t.Fatal("restart emitted motion")
			}
		})
	}
}
func TestClockFaultsLatchDespiteLaterRecovery(t *testing.T) {
	for _, mode := range []string{"wall-backward", "monotonic-backward", "offset-forward"} {
		t.Run(mode, func(t *testing.T) {
			c, _, a, clock := fixture(t)
			clock.advance(time.Second)
			if e := c.Start(context.Background()); e != nil {
				t.Fatal(e)
			}
			if _, e := c.Tick(context.Background()); e != nil {
				t.Fatal(e)
			}
			oldWall, oldMono := clock.wall, clock.mono
			switch mode {
			case "wall-backward":
				clock.wall = clock.wall.Add(-time.Millisecond)
			case "monotonic-backward":
				clock.mono -= time.Millisecond
			case "offset-forward":
				clock.wall = clock.wall.Add(51 * time.Millisecond)
			}
			if d, e := c.Tick(context.Background()); e == nil || d.Action != mission.Stop {
				t.Fatal("clock fault returned authority")
			}
			clock.wall, clock.mono = oldWall, oldMono
			_, _ = c.Tick(context.Background())
			if len(a.commands) != 1 {
				t.Fatal("clock recovery resumed command")
			}
		})
	}
}
