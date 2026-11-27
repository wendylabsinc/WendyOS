package mission

import (
	"math"
	"testing"
	"time"
)

func fixture(t *testing.T) (*Engine, *time.Duration, Task, Authority, Conditions) {
	t.Helper()
	now := time.Second
	e, err := New(Config{Assignee: "robot/app", Site: "office", Domain: Simulation, Now: func() time.Duration { return now }, MaxLease: time.Second, Catalog: map[string]SafetyClass{"inspect": Move, "capture": Observe, "relay": Actuate}})
	if err != nil {
		t.Fatal(err)
	}
	task := Task{ID: "mission-1", Type: "inspect", Assignee: "robot/app", Site: "office", Domain: Simulation, Class: Move, Mode: Autonomous, Deadline: time.Minute, Envelope: Envelope{AreaID: "floor-1", RequiredSensors: []string{"depth"}, MinimumEnergy: 0.2, Fallback: Hold}}
	a := authority(task)
	c := Conditions{InsideArea: true, ObstacleClear: true, Energy: 0.8, Sensors: map[string]bool{"depth": true}}
	return e, &now, task, a, c
}

func authority(task Task) Authority {
	return Authority{TaskDigest: Digest(task), Principal: "operator-1", Operator: true, AllowAutonomy: true, Site: task.Site, Assignee: task.Assignee, Domain: task.Domain, ValidUntil: time.Minute}
}
func stopAuthority(task Task) StopAuthority {
	return StopAuthority{TaskID: task.ID, Principal: "operator-2", Site: task.Site, Assignee: task.Assignee, Domain: task.Domain, ValidUntil: time.Minute}
}
func mustAccept(t *testing.T, e *Engine, task Task, a Authority) Status {
	t.Helper()
	s, err := e.Accept(task, a)
	if err != nil {
		t.Fatal(err)
	}
	return s
}
func mustEvaluate(t *testing.T, e *Engine, id string, c Conditions) Decision {
	t.Helper()
	d, err := e.Evaluate(id, c)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestAutonomousContinuesWithoutSupervisionButHoldsOnSensorLoss(t *testing.T) {
	e, now, task, a, c := fixture(t)
	mustAccept(t, e, task, a)
	d := mustEvaluate(t, e, task.ID, c)
	if d.Action != Continue || d.Status.Phase != Running {
		t.Fatal(d)
	}
	*now += 10 * time.Second
	if got := mustEvaluate(t, e, task.ID, c); got != d {
		t.Fatalf("partition changed autonomous status: %#v", got)
	}
	c.Sensors["depth"] = false
	d = mustEvaluate(t, e, task.ID, c)
	if d.Action != Hold || d.Status.Phase != Held {
		t.Fatal(d)
	}
	c.Sensors["depth"] = true
	if got := mustEvaluate(t, e, task.ID, c); got != d {
		t.Fatalf("held mission resumed automatically: %#v", got)
	}
}

func TestSupervisionAndTeleopNeedBoundedFreshLeases(t *testing.T) {
	for _, mode := range []Mode{Supervised, Teleop} {
		t.Run(string(mode), func(t *testing.T) {
			e, now, task, _, c := fixture(t)
			task.Mode = mode
			mustAccept(t, e, task, authority(task))
			c.SupervisionUntil = *now + 500*time.Millisecond
			c.CommandUntil = c.SupervisionUntil
			if d := mustEvaluate(t, e, task.ID, c); d.Action != Continue {
				t.Fatal(d)
			}
			*now += 500 * time.Millisecond
			if d := mustEvaluate(t, e, task.ID, c); d.Action != Hold {
				t.Fatal(d)
			}
		})
	}
	e, _, task, _, c := fixture(t)
	task.Mode = Supervised
	mustAccept(t, e, task, authority(task))
	c.SupervisionUntil = time.Hour
	if d := mustEvaluate(t, e, task.ID, c); d.Action != Hold {
		t.Fatal("unbounded lease accepted", d)
	}
}

func TestAdmissionRejectsUnsafeRequests(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Task, *Authority)
	}{
		{"class downgrade", func(x *Task, a *Authority) { x.Class = Observe; a.TaskDigest = Digest(*x) }},
		{"changed signed content", func(x *Task, a *Authority) { x.Envelope.AreaID = "other-floor" }},
		{"wrong site", func(x *Task, a *Authority) { x.Site = "other-site"; *a = authority(*x) }},
		{"wrong assignee", func(x *Task, a *Authority) { x.Assignee = "other-robot"; *a = authority(*x) }},
		{"simulation to physical", func(x *Task, a *Authority) { x.Domain = Physical; *a = authority(*x) }},
		{"nonoperator motion", func(x *Task, a *Authority) { a.Operator = false }},
		{"autonomy not granted", func(x *Task, a *Authority) { a.AllowAutonomy = false }},
		{"expired task", func(x *Task, a *Authority) { x.Deadline = time.Second; *a = authority(*x) }},
		{"expired authority", func(x *Task, a *Authority) { a.ValidUntil = time.Second }},
		{"unsupported actuation", func(x *Task, a *Authority) { x.Type = "relay"; x.Class = Actuate; *a = authority(*x) }},
		{"unknown type", func(x *Task, a *Authority) { x.Type = "unknown"; *a = authority(*x) }},
		{"invalid mode", func(x *Task, a *Authority) { x.Mode = "unrestricted"; *a = authority(*x) }},
		{"nonfinite energy", func(x *Task, a *Authority) { x.Envelope.MinimumEnergy = math.NaN(); *a = authority(*x) }},
		{"continue fallback", func(x *Task, a *Authority) { x.Envelope.Fallback = Continue; *a = authority(*x) }},
		{"observation return bypass", func(x *Task, a *Authority) {
			x.Type = "capture"
			x.Class = Observe
			x.Envelope.Fallback = Return
			*a = authority(*x)
			a.Operator = false
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e, _, task, a, _ := fixture(t)
			tc.mutate(&task, &a)
			if _, err := e.Accept(task, a); err == nil {
				t.Fatal("accepted invalid task")
			}
			if _, err := e.Status(task.ID); err == nil {
				t.Fatal("rejected request changed state")
			}
		})
	}
}

func TestStopBeforeTaskAndWrongScope(t *testing.T) {
	e, _, task, a, _ := fixture(t)
	bad := stopAuthority(task)
	bad.Site = "other"
	if _, err := e.Stop(bad); err == nil {
		t.Fatal("wrong-site stop accepted")
	}
	if _, err := e.Stop(stopAuthority(task)); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Accept(task, a); err == nil {
		t.Fatal("delayed task started after stop")
	}
	e.Restart()
	if _, err := e.Accept(task, a); err == nil {
		t.Fatal("restart lost stop")
	}
}

func TestDuplicateDoesNotExtendGrantOrAllowChangedContent(t *testing.T) {
	e, now, task, a, c := fixture(t)
	a.ValidUntil = 5 * time.Second
	s := mustAccept(t, e, task, a)
	a.ValidUntil = time.Minute
	if got := mustAccept(t, e, task, a); got != s {
		t.Fatal("duplicate changed status")
	}
	changed := task
	changed.Envelope.AreaID = "other"
	if _, err := e.Accept(changed, authority(changed)); err == nil {
		t.Fatal("same ID changed content")
	}
	*now = 5 * time.Second
	if d := mustEvaluate(t, e, task.ID, c); d.Action != Stop || d.Status.Phase != Failed {
		t.Fatal("duplicate renewed grant", d)
	}
}

func TestTerminalStatusAndAssigneeReporting(t *testing.T) {
	e, _, task, a, c := fixture(t)
	s := mustAccept(t, e, task, a)
	if _, err := e.Report(task.ID, task.Assignee, s.Version+1, Succeeded); err == nil {
		t.Fatal("unstarted task succeeded")
	}
	d := mustEvaluate(t, e, task.ID, c)
	if _, err := e.Report(task.ID, "other", d.Status.Version+1, Succeeded); err == nil {
		t.Fatal("other assignee reported")
	}
	if _, err := e.Report(task.ID, task.Assignee, d.Status.Version, Succeeded); err == nil {
		t.Fatal("nonincreasing status")
	}
	s, err := e.Report(task.ID, task.Assignee, d.Status.Version+1, Succeeded)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Report(task.ID, task.Assignee, s.Version+1, Failed); err == nil {
		t.Fatal("terminal status changed")
	}
	if d, err := e.Stop(stopAuthority(task)); err != nil || d.Status != s || d.Action != Stop {
		t.Fatal("stop changed terminal status", d, err)
	}
}

func TestRestartFailsInterruptedMotionWithoutResume(t *testing.T) {
	e, _, task, a, c := fixture(t)
	mustAccept(t, e, task, a)
	mustEvaluate(t, e, task.ID, c)
	d := e.Restart()[task.ID]
	if d.Action != Stop || d.Status.Phase != Failed {
		t.Fatal(d)
	}
	if got := mustEvaluate(t, e, task.ID, c); got != d {
		t.Fatal("restarted motion resumed", got)
	}
	if len(e.Restart()) != 0 {
		t.Fatal("restart overwrote terminal state")
	}
}

func TestReturnNeedsIndependentConditionsAndCannotBypassReservation(t *testing.T) {
	e, _, task, _, c := fixture(t)
	task.Envelope.Fallback = Return
	mustAccept(t, e, task, authority(task))
	mustEvaluate(t, e, task.ID, c)
	second := task
	second.ID = "second"
	mustAccept(t, e, second, authority(second))
	c.Sensors["depth"] = false
	c.ReturnSafe = true
	if _, err := e.Evaluate(second.ID, c); err == nil {
		t.Fatal("second task bypassed reservation through return")
	}
	if d := mustEvaluate(t, e, task.ID, c); d.Action != Return {
		t.Fatal(d)
	}
	c.ReturnSafe = false
	if d := mustEvaluate(t, e, task.ID, c); d.Action != Stop || d.Status.Phase != Failed {
		t.Fatal(d)
	}
}

func TestDeadlineAndClockRollback(t *testing.T) {
	e, now, task, a, c := fixture(t)
	mustAccept(t, e, task, a)
	mustEvaluate(t, e, task.ID, c)
	*now = 0
	if d, err := e.Evaluate(task.ID, c); err == nil || d.Action != Stop {
		t.Fatal("clock rollback not rejected", d, err)
	}
	*now = 2 * time.Second
	if d, err := e.Evaluate(task.ID, c); err == nil || d.Action != Stop || d.Status.Phase != Failed {
		t.Fatal("motion resumed after clock recovered", d)
	}
	*now = task.Deadline
	if d, err := e.Evaluate(task.ID, c); err == nil || d.Action != Stop || d.Status.Phase != Failed {
		t.Fatal(d)
	}
}

func TestCompletionAfterDeadlineCannotSucceed(t *testing.T) {
	e, now, task, a, c := fixture(t)
	mustAccept(t, e, task, a)
	d := mustEvaluate(t, e, task.ID, c)
	*now = task.Deadline
	if s, err := e.Report(task.ID, task.Assignee, d.Status.Version+1, Succeeded); err == nil || s.Phase != Failed {
		t.Fatal(s, err)
	}
}

func TestCallerMutationDoesNotChangeAcceptedSensorsOrCatalog(t *testing.T) {
	e, _, task, a, c := fixture(t)
	mustAccept(t, e, task, a)
	task.Envelope.RequiredSensors[0] = "different"
	if d := mustEvaluate(t, e, task.ID, c); d.Action != Continue {
		t.Fatal("task alias changed policy", d)
	}
	cat := map[string]SafetyClass{"inspect": Move}
	n, err := New(Config{Assignee: task.Assignee, Site: task.Site, Domain: Simulation, Now: func() time.Duration { return 0 }, MaxLease: time.Second, Catalog: cat})
	if err != nil {
		t.Fatal(err)
	}
	cat["inspect"] = Observe
	task.Class = Observe
	if _, err := n.Accept(task, authority(task)); err == nil {
		t.Fatal("catalog aliased caller map")
	}
}

func TestMissingConditionsFailClosed(t *testing.T) {
	for _, change := range []func(*Conditions){func(c *Conditions) { c.InsideArea = false }, func(c *Conditions) { c.ObstacleClear = false }, func(c *Conditions) { c.Energy = 0 }, func(c *Conditions) { c.Energy = math.NaN() }, func(c *Conditions) { c.Sensors = nil }} {
		e, _, task, a, c := fixture(t)
		mustAccept(t, e, task, a)
		change(&c)
		if d := mustEvaluate(t, e, task.ID, c); d.Action != Hold {
			t.Fatal(d)
		}
	}
}
