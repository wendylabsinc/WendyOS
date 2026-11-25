package patrol

import (
	"github.com/wendylabsinc/wendy/go/world/internal/patrolwork"
	"strings"
	"testing"
)

func workSample(now int64, total float64) patrolwork.Sample {
	return patrolwork.Sample{Schema: patrolwork.Schema, Method: patrolwork.Method, RunID: "run", BootID: "boot", ModelSHA256: strings.Repeat("a", 64), Sequence: uint64(now), SimulationNS: now * 1e6, CapturedUnixMS: now, TotalJ: total, PowerW: 1, Valid: true}
}
func workRig(t *testing.T) (*Engine, *returnAdapter) {
	t.Helper()
	e, f := returnRig(t)
	s := workSample(1000, 50)
	if err := e.SetWork(patrolwork.Policy{LimitJ: 100, ReturnReserveJ: 30}, s, 1000, s.RunID, s.ModelSHA256); err != nil {
		t.Fatal(err)
	}
	return e, f
}
func TestWorkReserveUsesSignedReturnPath(t *testing.T) {
	e, f := workRig(t)
	selectRoom(t, e, &f.fake, 1100)
	s := workSample(1200, 125)
	e.ObserveWork(&s, 1200)
	if e.WorkMotionReady() {
		t.Fatal("outbound dispatch allowed past reserve")
	}
	if err := e.Tick(1200, rn(f, 1.2)); err != nil {
		t.Fatal(err)
	}
	if !e.State.Return.Requested || e.State.Return.Trigger != "work_reserve" || e.State.State != "returning" || f.p.X != 1 || e.State.Rooms[0].Visits != 0 {
		t.Fatal(e.State)
	}
	if err := e.SetWork(patrolwork.Policy{LimitJ: 200}, s, 1200, s.RunID, s.ModelSHA256); err == nil {
		t.Fatal("active allowance replaced")
	}
}
func TestWorkStaleRequiresExplicitResumeWithoutReplenishment(t *testing.T) {
	e, f := workRig(t)
	s := workSample(1200, 100)
	e.ObserveWork(&s, 1200)
	_ = e.Tick(2300, rn(f, 2.3))
	if e.State.State != "work_held" || e.State.Work.Valid {
		t.Fatal(e.State)
	}
	if e.Control("resume", 2301) == nil || e.Control("recall", 2301) == nil {
		t.Fatal("stale work resumed")
	}
	s = workSample(2400, 125)
	e.ObserveWork(&s, 2400)
	_ = e.Tick(2400, rn(f, 2.4))
	if e.State.State != "work_held" || e.State.Return.Requested || e.State.Work.UsedJ != 75 {
		t.Fatal("automatic resume or replenishment", e.State)
	}
	if err := e.Control("resume", 2400); err != nil {
		t.Fatal(err)
	}
	_ = e.Tick(2401, rn(f, 2.401))
	if e.State.State != "returning" || e.State.Return.Trigger != "work_reserve" || e.State.Work.UsedJ != 75 {
		t.Fatal(e.State)
	}
}
func TestWorkExhaustionResetStopDeadlineAndPausedReserve(t *testing.T) {
	for _, mode := range []string{"exhausted", "reset"} {
		t.Run(mode, func(t *testing.T) {
			e, f := workRig(t)
			s := workSample(1200, 151)
			if mode == "reset" {
				s.TotalJ = 0
				s.BootID = "new"
			}
			e.ObserveWork(&s, 1200)
			_ = e.Tick(1200, rn(f, 1.2))
			if e.State.State != "work_held" || !e.State.Work.Permanent || e.WorkMotionReady() {
				t.Fatal(e.State)
			}
			if e.Control("resume", 1201) == nil || e.Control("recall", 1201) == nil {
				t.Fatal("invalid resource resumed")
			}
			if err := e.Control("stop", 1202); err != nil || e.State.State != "stopped" {
				t.Fatal(e.State, err)
			}
		})
	}
	e, f := workRig(t)
	_ = e.Control("pause", 1100)
	s := workSample(1200, 125)
	e.ObserveWork(&s, 1200)
	_ = e.Tick(1200, rn(f, 1.2))
	if e.State.State != "paused" || !e.State.Return.Requested {
		t.Fatal(e.State)
	}
	_ = e.Tick(100000, rn(f, 100))
	if e.State.State != "expired" {
		t.Fatal(e.State)
	}
}
