package tasking

import (
	"encoding/json"
	"errors"
	"github.com/wendylabsinc/wendy/go/world/internal/mission"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

type rig struct {
	wall  time.Time
	mono  time.Duration
	cfg   Config
	s     *Store
	path  string
	epoch uint64
}

func setup(t *testing.T) *rig {
	t.Helper()
	r := &rig{wall: time.Unix(1700000000, 0), mono: time.Second, path: filepath.Join(t.TempDir(), "tasks.json")}
	r.cfg = Config{RunID: "run", Site: "office", Assignee: "robot", ExecutorPrincipal: "app:nav", Domain: mission.Simulation, Catalog: map[string]mission.SafetyClass{"go_to": mission.Move}, Now: func() time.Time { return r.wall }, Monotonic: func() time.Duration { return r.mono }, MaxLease: time.Second}
	var e error
	r.s, e = Open(r.path, r.cfg)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { r.s.Close() })
	r.epoch, e = r.s.BeginExecutor("app:nav")
	if e != nil {
		t.Fatal(e)
	}
	return r
}
func (r *rig) spec(id string) Spec {
	return Spec{ID: id, Type: "go_to", Site: "office", Assignee: "robot", Domain: mission.Simulation, Class: mission.Move, Mode: mission.Autonomous, Frame: world.FrameRef{ID: "office", Revision: "r1"}, NotAfterUnixMS: r.wall.Add(time.Minute).UnixMilli(), Area: Bounds{Min: [3]float64{-5, -5, -5}, Max: [3]float64{5, 5, 5}}, RequiredSensors: []string{"lidar"}, MinimumEnergy: .2, Fallback: mission.Hold, Target: [3]float64{2, 0, 0}, SpeedMPS: 1, ToleranceM: .1, StopSpeedMPS: .05}
}
func (r *rig) auth() Authorization {
	return Authorization{Principal: "operator", Operator: true, AllowAutonomy: true, ValidUntil: r.wall.Add(time.Hour), Proof: json.RawMessage(`{"signature":"test"}`)}
}
func (r *rig) telemetry() Telemetry {
	return Telemetry{Frame: world.FrameRef{ID: "office", Revision: "r1"}, SampleUnixMS: r.wall.UnixMilli(), ObstacleClear: true, Energy: .8, Sensors: map[string]bool{"lidar": true}}
}
func (r *rig) accept(t *testing.T, id string) Record {
	t.Helper()
	out, e := r.s.Accept(r.spec(id), r.auth())
	if e != nil {
		t.Fatal(e)
	}
	return out
}
func (r *rig) evaluate(t *testing.T, id string, tele Telemetry) Decision {
	t.Helper()
	d, e := r.s.Evaluate(id, "app:nav", r.epoch, tele)
	if e != nil {
		t.Fatal(e)
	}
	return d
}
func TestDurableAdmissionRestartAndDuplicate(t *testing.T) {
	r := setup(t)
	original := r.accept(t, "a")
	if d := r.evaluate(t, "a", r.telemetry()); d.Action != mission.Continue {
		t.Fatal(d)
	}
	r.mono += time.Second
	r.wall = r.wall.Add(time.Second)
	dupe, e := r.s.Accept(original.Spec, r.auth())
	if e != nil || dupe.Status.Phase != mission.Running {
		t.Fatal(dupe, e)
	}
	changed := original.Spec
	changed.Target[0] = 3
	if _, e = r.s.Accept(changed, r.auth()); e == nil {
		t.Fatal("changed task accepted")
	}
	r.s.Close()
	r.mono = 0
	r.s, e = Open(r.path, r.cfg)
	if e != nil {
		t.Fatal(e)
	}
	got, e := r.s.Get("a")
	if e != nil || got.Status.Phase != mission.Failed || got.Status.Version <= dupe.Status.Version {
		t.Fatal(got, e)
	}
	if string(got.Proof) != string(original.Proof) {
		t.Fatal("proof changed")
	}
	if got.Spec.NotAfterUnixMS != original.Spec.NotAfterUnixMS {
		t.Fatal("deadline changed")
	}
	if _, e = r.s.Accept(original.Spec, r.auth()); e != nil {
		t.Fatal("identical duplicate rejected after restart", e)
	}
}
func TestStopBeforeDeliveryPersists(t *testing.T) {
	r := setup(t)
	spec := r.spec("late")
	if _, e := r.s.Stop("late", r.auth()); e != nil {
		t.Fatal(e)
	}
	r.s.Close()
	var e error
	r.s, e = Open(r.path, r.cfg)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = r.s.Accept(spec, r.auth()); e == nil {
		t.Fatal("resurrected stopped ID")
	}
	got, e := r.s.Get("late")
	if e != nil || got.Status.Phase != mission.Canceled {
		t.Fatal(got, e)
	}
}
func TestCommitFailureNeverDispatches(t *testing.T) {
	r := setup(t)
	r.accept(t, "a")
	r.s.write = func(string, []byte) error { return errors.New("disk full") }
	d, e := r.s.Evaluate("a", "app:nav", r.epoch, r.telemetry())
	if e == nil || d.Action != mission.Stop {
		t.Fatal(d, e)
	}
	if _, e = r.s.Get("a"); e == nil {
		t.Fatal("fault not latched")
	}
	r.s.Close()
	r.s, e = Open(r.path, r.cfg)
	if e != nil {
		t.Fatal(e)
	}
	got, _ := r.s.Get("a")
	if got.Status.Phase != mission.Failed {
		t.Fatal(got)
	}
}
func TestAdmissionWriteFailure(t *testing.T) {
	r := setup(t)
	r.s.write = func(string, []byte) error { return errors.New("write failed") }
	if _, e := r.s.Accept(r.spec("a"), r.auth()); e == nil {
		t.Fatal("acknowledged failed persistence")
	}
	r.s.Close()
	var e error
	r.s, e = Open(r.path, r.cfg)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = r.s.Get("a"); e == nil {
		t.Fatal("failed admission visible")
	}
}
func TestCompletionUsesPositionAndVelocity(t *testing.T) {
	r := setup(t)
	r.accept(t, "a")
	tele := r.telemetry()
	tele.Position = [3]float64{2, 0, 0}
	tele.Velocity = [3]float64{.5, 0, 0}
	if d := r.evaluate(t, "a", tele); d.Record.Status.Phase != mission.Running {
		t.Fatal(d)
	}
	if _, e := r.s.Report("a", "app:nav", r.epoch, 99, mission.Succeeded); e == nil {
		t.Fatal("unmeasured completion accepted")
	}
	tele.Velocity = [3]float64{}
	d := r.evaluate(t, "a", tele)
	if d.Action != mission.Stop || d.Record.Status.Phase != mission.Succeeded {
		t.Fatal(d)
	}
	got, e := r.s.Stop("a", r.auth())
	if e != nil || got.Status != d.Record.Status {
		t.Fatal("terminal changed", got, e)
	}
}
func TestUnsafeTelemetryHoldsAndDoesNotResume(t *testing.T) {
	for _, bad := range []string{"frame", "sensor", "stale", "future", "position", "velocity", "energy", "area"} {
		t.Run(bad, func(t *testing.T) {
			r := setup(t)
			r.accept(t, "a")
			tele := r.telemetry()
			switch bad {
			case "frame":
				tele.Frame.Revision = "old"
			case "sensor":
				tele.Sensors = nil
			case "stale":
				tele.SampleUnixMS -= 2000
			case "future":
				tele.SampleUnixMS++
			case "position":
				tele.Position[0] = math.NaN()
			case "velocity":
				tele.Velocity[0] = math.Inf(1)
			case "energy":
				tele.Energy = .1
			case "area":
				tele.Position[0] = 100
			}
			d := r.evaluate(t, "a", tele)
			if d.Action != mission.Hold || d.Record.Status.Phase != mission.Held {
				t.Fatal(d)
			}
			if d = r.evaluate(t, "a", r.telemetry()); d.Action != mission.Hold {
				t.Fatal("auto resumed", d)
			}
		})
	}
}
func TestEpochAndActuatorArbitration(t *testing.T) {
	r := setup(t)
	r.accept(t, "a")
	r.accept(t, "b")
	r.evaluate(t, "a", r.telemetry())
	d, e := r.s.Evaluate("b", "app:nav", r.epoch, r.telemetry())
	if e == nil || d.Action != mission.Hold {
		t.Fatal(d, e)
	}
	epoch, e := r.s.BeginExecutor("app:nav")
	if e != nil || epoch <= r.epoch {
		t.Fatal(epoch, e)
	}
	if d, e = r.s.Evaluate("a", "app:nav", r.epoch, r.telemetry()); e == nil || d.Action != mission.Stop {
		t.Fatal(d, e)
	}
	for _, id := range []string{"a", "b"} {
		got, _ := r.s.Get(id)
		if got.Status.Phase != mission.Failed {
			t.Fatal(got)
		}
	}
}
func TestRollbackAndExpiry(t *testing.T) {
	for _, mode := range []string{"wall", "mono", "expiry"} {
		t.Run(mode, func(t *testing.T) {
			r := setup(t)
			r.accept(t, "a")
			r.evaluate(t, "a", r.telemetry())
			switch mode {
			case "wall":
				r.wall = r.wall.Add(-time.Second)
			case "mono":
				r.mono = 0
			case "expiry":
				r.wall = r.wall.Add(2 * time.Minute)
				r.mono += 2 * time.Minute
			}
			d, e := r.s.Evaluate("a", "app:nav", r.epoch, r.telemetry())
			if d.Action != mission.Stop {
				t.Fatal(d, e)
			}
			if mode == "expiry" && d.Record.Status.Phase != mission.Failed {
				t.Fatal(d, e)
			}
			if mode != "expiry" && e == nil {
				t.Fatal("rollback accepted")
			}
		})
	}
}
func TestAuthorityRevocation(t *testing.T) {
	r := setup(t)
	r.accept(t, "a")
	r.s.cfg.ValidateAuthority = func(Record) error { return errors.New("revoked") }
	d, e := r.s.Evaluate("a", "app:nav", r.epoch, r.telemetry())
	if e == nil || d.Action != mission.Stop || d.Record.Status.Phase != mission.Failed {
		t.Fatal(d, e)
	}
}
func TestLockAndChecksum(t *testing.T) {
	r := setup(t)
	if other, e := Open(r.path, r.cfg); e == nil {
		other.Close()
		t.Fatal("second owner opened")
	}
	r.s.Close()
	b, e := os.ReadFile(r.path)
	if e != nil {
		t.Fatal(e)
	}
	b[len(b)-5] ^= 1
	if e = os.WriteFile(r.path, b, 0600); e != nil {
		t.Fatal(e)
	}
	if _, e = Open(r.path, r.cfg); e == nil {
		t.Fatal("corrupt state accepted")
	}
}

// The subprocess exits without Close, exercising OS lock release and a real
// process-clock reset. Atomic acceptance survives abrupt process termination.
func TestAbruptProcessExit(t *testing.T) {
	if path := os.Getenv("TASKING_CRASH_PATH"); path != "" {
		now := time.Unix(1700000000, 0)
		cfg := Config{RunID: "run", Site: "office", Assignee: "robot", ExecutorPrincipal: "app:nav", Domain: mission.Simulation, Catalog: map[string]mission.SafetyClass{"go_to": mission.Move}, Now: func() time.Time { return now }, Monotonic: func() time.Duration { return time.Second }}
		s, e := Open(path, cfg)
		if e != nil {
			os.Exit(2)
		}
		r := &rig{wall: now, s: s}
		_, e = s.Accept(r.spec("crashed"), r.auth())
		if e != nil {
			os.Exit(3)
		}
		os.Exit(0)
	}
	r := setup(t)
	r.s.Close()
	cmd := exec.Command(os.Args[0], "-test.run=^TestAbruptProcessExit$")
	cmd.Env = append(os.Environ(), "TASKING_CRASH_PATH="+r.path)
	if out, e := cmd.CombinedOutput(); e != nil {
		t.Fatal(string(out), e)
	}
	var e error
	r.s, e = Open(r.path, r.cfg)
	if e != nil {
		t.Fatal(e)
	}
	got, e := r.s.Get("crashed")
	if e != nil || got.Status.Phase != mission.Failed {
		t.Fatal(got, e)
	}
}

func TestAdmissionAndReadCopiesAreImmutable(t *testing.T) {
	r := setup(t)
	spec := r.spec("immutable")
	auth := r.auth()
	accepted, e := r.s.Accept(spec, auth)
	if e != nil {
		t.Fatal(e)
	}
	spec.RequiredSensors[0] = "tampered"
	auth.Proof[0] = '!'
	accepted.Spec.RequiredSensors[0] = "tampered-output"
	accepted.Proof[0] = '!'
	got, e := r.s.Get("immutable")
	if e != nil {
		t.Fatal(e)
	}
	if got.Spec.RequiredSensors[0] != "lidar" || !json.Valid(got.Proof) {
		t.Fatal("admission aliased caller", got)
	}
	got.Spec.RequiredSensors[0] = "tampered-get"
	again, _ := r.s.Get("immutable")
	if again.Spec.RequiredSensors[0] != "lidar" {
		t.Fatal("read aliased state")
	}
	d := r.evaluate(t, "immutable", r.telemetry())
	if d.Action != mission.Continue {
		t.Fatal(d)
	}
}
func TestWallDeadlineExpiresWithoutMonotonicAdvance(t *testing.T) {
	r := setup(t)
	r.accept(t, "wall-expiry")
	r.wall = r.wall.Add(2 * time.Minute)
	d, e := r.s.Evaluate("wall-expiry", "app:nav", r.epoch, r.telemetry())
	if d.Action != mission.Stop || d.Record.Status.Phase != mission.Failed {
		t.Fatal(d, e)
	}
}
func TestPendingStopsAreListedAndAudited(t *testing.T) {
	r := setup(t)
	if _, e := r.s.Stop("missing", r.auth()); e != nil {
		t.Fatal(e)
	}
	records, e := r.s.List()
	if e != nil || len(records) != 1 || records[0].Spec.ID != "missing" {
		t.Fatal(records, e)
	}
	log, e := r.s.AuditLog()
	if e != nil || len(log) != 2 || log[1].Event != "stop" || !json.Valid(log[1].Proof) {
		t.Fatal(log, e)
	}
	r.s.Close()
	r.s, e = Open(r.path, r.cfg)
	if e != nil {
		t.Fatal(e)
	}
	again, e := r.s.AuditLog()
	if e != nil || len(again) != len(log) || again[1].Sequence != 2 {
		t.Fatal(again, e)
	}
}
