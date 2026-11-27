package executor

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/mission"
	"github.com/wendylabsinc/wendy/go/world/internal/tasking"
)

func TestWaitingSupervisedTaskDoesNotStarveReadyMission(t *testing.T) {
	c, a, g := controller(t)
	waiting := record()
	waiting.Spec.ID = "a-waiting"
	waiting.Spec.Mode = mission.Supervised
	waiting.Status.Phase = mission.Accepted
	ready := record()
	ready.Spec.ID = "b-ready"
	ready.Status.Phase = mission.Accepted
	running := ready
	running.Status.Phase = mission.Running
	g.records = []tasking.Record{waiting, ready}
	g.perTask = map[string]tasking.Decision{waiting.Spec.ID: {Action: mission.Hold, Record: waiting}, ready.Spec.ID: {Action: mission.Continue, Record: running}}
	d, err := c.Tick(context.Background())
	if err != nil || d.Record.Spec.ID != ready.Spec.ID || a.commands != 1 || g.evaluations != 2 {
		t.Fatalf("ready mission starved: %+v %v commands=%d evaluations=%d", d, err, a.commands, g.evaluations)
	}
	// A held task keeps its reservation even while an earlier ID is waiting.
	held := record()
	held.Spec.ID = "z-held"
	held.Status.Phase = mission.Held
	g.records = append(g.records, held)
	g.perTask[held.Spec.ID] = tasking.Decision{Action: mission.Hold, Record: held}
	before := g.evaluations
	d, err = c.Tick(context.Background())
	if err != nil || d.Record.Spec.ID != held.Spec.ID || a.commands != 1 || g.evaluations != before+1 {
		t.Fatal("waiting task bypassed held reservation", d, err)
	}
}

func TestAcceptedSelectionIsBoundedAndRotates(t *testing.T) {
	c, a, g := controller(t)
	c.Adapter = &deadlineFakeAdapter{fakeAdapter: a}
	g.records = nil
	g.perTask = map[string]tasking.Decision{}
	for i := 0; i < 10; i++ {
		r := record()
		r.Spec.ID = fmt.Sprintf("%02d-task", i)
		r.Spec.Mode = mission.Supervised
		r.Status.Phase = mission.Accepted
		g.records = append(g.records, r)
		d := tasking.Decision{Action: mission.Hold, Record: r}
		if i == 9 {
			d.Action = mission.Continue
			d.Record.Status.Phase = mission.Running
			d = withSupervisionBudget(d, time.Now())
			g.records[len(g.records)-1].Spec = d.Record.Spec
		}
		g.perTask[r.Spec.ID] = d
	}
	if _, err := c.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if g.evaluations != 8 || a.commands != 0 {
		t.Fatal("unbounded initial scan", g.evaluations, a.commands)
	}
	d, err := c.Tick(context.Background())
	if err != nil || d.Record.Spec.ID != "09-task" || g.evaluations != 10 || a.commands != 1 {
		t.Fatal("bounded scan failed to rotate", d, err, g.evaluations, a.commands)
	}
}

func TestSupervisedDecisionAllowedButTeleopAndChangedSpecRefused(t *testing.T) {
	for _, mode := range []mission.Mode{mission.Supervised, mission.Teleop} {
		c, a, g := controller(t)
		g.records[0].Spec.Mode = mode
		g.decision.Record.Spec.Mode = mode
		if mode == mission.Supervised {
			c.Adapter = &deadlineFakeAdapter{fakeAdapter: a}
			g.decision = withSupervisionBudget(g.decision, time.Now())
			g.records[0].Spec = g.decision.Record.Spec
		}
		_, err := c.Tick(context.Background())
		if mode == mission.Supervised && (err != nil || a.commands != 1) {
			t.Fatal("qualified supervised decision refused", err)
		}
		if mode == mission.Teleop && (err == nil || a.commands != 0) {
			t.Fatal("teleoperation enabled without a command protocol")
		}
	}
	c, a, g := controller(t)
	g.decision.Record.Spec.Target[0] = -1.5
	if _, err := c.Tick(context.Background()); err == nil || a.commands != 0 {
		t.Fatal("changed immutable task reached actuator")
	}
}

type deadlineFakeAdapter struct {
	*fakeAdapter
	lease    time.Duration
	notAfter int64
}

func (a *deadlineFakeAdapter) CommandUntil(_ context.Context, _ string, _ uint64, v float64, lease time.Duration, notAfter int64) error {
	a.commands++
	a.velocity = v
	a.lease = lease
	a.notAfter = notAfter
	return nil
}
func withSupervisionBudget(d tasking.Decision, now time.Time) tasking.Decision {
	d.Record.Spec.Mode = mission.Supervised
	if d.Record.Spec.NotAfterUnixMS == 0 {
		d.Record.Spec.NotAfterUnixMS = now.Add(time.Minute).UnixMilli()
	}
	if d.Record.AuthorizationUntilUnixMS == 0 {
		d.Record.AuthorizationUntilUnixMS = now.Add(time.Minute).UnixMilli()
	}
	d.Supervision = &tasking.SupervisionBudget{RemainingMS: 500, ExpiresAtUnixMS: now.Add(500 * time.Millisecond).UnixMilli(), ExecutorEpoch: 1, GatewayBootID: "gateway-boot"}
	return d
}
func supervisedController(t *testing.T) (*Controller, *deadlineFakeAdapter, *fakeGateway, *time.Time) {
	t.Helper()
	c, a, g := controller(t)
	now := time.Now()
	c.now = func() time.Time { return now }
	a.state.SampleUnixMS = now.UnixMilli()
	adapter := &deadlineFakeAdapter{fakeAdapter: a}
	c.Adapter = adapter
	g.decision = withSupervisionBudget(g.decision, now)
	g.records[0].Spec = g.decision.Record.Spec
	return c, adapter, g, &now
}
func TestSupervisedDecisionChargesRoundTripAndProcessing(t *testing.T) {
	c, a, g, now := supervisedController(t)
	g.decision.Supervision.RemainingMS = 20
	g.decision.Supervision.ExpiresAtUnixMS = now.Add(20 * time.Millisecond).UnixMilli()
	g.evaluateHook = func() { *now = now.Add(50 * time.Millisecond) }
	if _, err := c.Tick(context.Background()); err == nil || a.commands != 0 {
		t.Fatal("20ms lease survived 50ms response", err, a.commands)
	}
	// A valid newer decision can move; its command deadline is reduced by the
	// complete round trip rather than starting a new lease at response receipt.
	g.decision.Supervision.RemainingMS = 200
	g.decision.Supervision.ExpiresAtUnixMS = now.Add(200 * time.Millisecond).UnixMilli()
	a.state.SampleUnixMS = now.UnixMilli()
	started := *now
	if _, err := c.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if a.commands != 1 || a.lease != 150*time.Millisecond || a.notAfter != started.Add(200*time.Millisecond).UnixMilli() {
		t.Fatal("response renewed budget", a.commands, a.lease, a.notAfter)
	}
	// Further executor processing consumes the same budget.
	lease, until, err := c.supervisionCommandBudget(g.decision, now.Add(140*time.Millisecond))
	if err != nil || lease != 10*time.Millisecond || until != a.notAfter {
		t.Fatal("processing renewed budget", lease, until, err)
	}
	if _, _, err = c.supervisionCommandBudget(g.decision, now.Add(150*time.Millisecond)); err == nil {
		t.Fatal("exhausted processing budget accepted")
	}
}
func TestSupervisedDecisionRequiresCompleteCurrentBudget(t *testing.T) {
	cases := map[string]func(*tasking.Decision){
		"omitted":             func(d *tasking.Decision) { d.Supervision = nil },
		"zero":                func(d *tasking.Decision) { d.Supervision.RemainingMS = 0 },
		"negative":            func(d *tasking.Decision) { d.Supervision.RemainingMS = -1 },
		"over profile":        func(d *tasking.Decision) { d.Supervision.RemainingMS = 501 },
		"wrong epoch":         func(d *tasking.Decision) { d.Supervision.ExecutorEpoch++ },
		"missing boot":        func(d *tasking.Decision) { d.Supervision.GatewayBootID = "" },
		"missing wall expiry": func(d *tasking.Decision) { d.Supervision.ExpiresAtUnixMS = 0 },
		"wall mismatch":       func(d *tasking.Decision) { d.Supervision.ExpiresAtUnixMS += 1000 },
		"past task":           func(d *tasking.Decision) { d.Record.Spec.NotAfterUnixMS = d.Supervision.ExpiresAtUnixMS - 1 },
		"past grant":          func(d *tasking.Decision) { d.Record.AuthorizationUntilUnixMS = d.Supervision.ExpiresAtUnixMS - 1 },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			c, a, g, _ := supervisedController(t)
			change(&g.decision)
			g.records[0].Spec = g.decision.Record.Spec
			if _, err := c.Tick(context.Background()); err == nil || a.commands != 0 {
				t.Fatal("invalid decision renewed motion", err, a.commands)
			}
		})
	}
}
func TestSupervisedAdapterCapabilityClockAndBootFences(t *testing.T) {
	c, a, _, _ := supervisedController(t)
	c.Adapter = a.fakeAdapter
	if _, err := c.Tick(context.Background()); err == nil || a.commands != 0 {
		t.Fatal("legacy relative-only adapter moved")
	}
	c, a, g, now := supervisedController(t)
	g.evaluateHook = func() { *now = now.Add(-time.Millisecond) }
	if _, err := c.Tick(context.Background()); err == nil || a.commands != 0 {
		t.Fatal("clock rollback renewed motion")
	}
	c, a, g, now = supervisedController(t)
	if _, err := c.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(100 * time.Millisecond)
	a.state.SampleUnixMS = now.UnixMilli()
	g.decision = withSupervisionBudget(g.decision, *now)
	g.decision.Supervision.GatewayBootID = "replacement-boot"
	if _, err := c.Tick(context.Background()); err == nil || a.commands != 1 {
		t.Fatal("gateway boot change accepted", err, a.commands)
	}
}
func TestSupervisedClockRollbackBetweenTicksStaysLatched(t *testing.T) {
	c, a, g, now := supervisedController(t)
	if _, err := c.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(-time.Millisecond)
	a.state.SampleUnixMS = now.UnixMilli()
	if _, err := c.Tick(context.Background()); err == nil || a.commands != 1 {
		t.Fatal("between-tick rollback renewed command", err, a.commands)
	}
	*now = now.Add(101 * time.Millisecond)
	a.state.SampleUnixMS = now.UnixMilli()
	g.decision = withSupervisionBudget(g.decision, *now)
	if _, err := c.Tick(context.Background()); err == nil || a.commands != 1 {
		t.Fatal("clock recovery bypassed fault latch", err, a.commands)
	}
}

func TestLegitimateFreshSupervisionDecisionCanRenewCommand(t *testing.T) {
	c, a, g, now := supervisedController(t)
	if _, err := c.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	old := a.notAfter
	*now = now.Add(100 * time.Millisecond)
	a.state.SampleUnixMS = now.UnixMilli()
	g.decision = withSupervisionBudget(g.decision, *now)
	if _, err := c.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if a.commands != 2 || a.notAfter <= old || a.lease > commandLease {
		t.Fatal("fresh supervision not applied", a.commands, a.notAfter, old)
	}
}
