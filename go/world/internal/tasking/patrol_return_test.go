package tasking

import (
	"errors"
	"github.com/wendylabsinc/wendy/go/world/internal/mission"
	"testing"
	"time"
)

func arrivalRig(t *testing.T) (*rig, Spec, Telemetry) {
	t.Helper()
	r, s, tele := patrolRig(t)
	s.ID = "return-patrol"
	s.Patrol.Return = &PatrolReturn{Position: []float64{1, 0, 1}, BeforeExpirySeconds: 10}
	if _, err := r.s.Accept(s, r.auth()); err != nil {
		t.Fatal(err)
	}
	tele.Position = [3]float64{1, 0, 1}
	tele.SampleSimulationNS = 5000000000
	tele.PatrolArrival = &PatrolArrival{MissionID: s.ID, SettledSinceUnixMS: r.wall.Add(-3 * time.Second).UnixMilli(), ObservedAtUnixMS: r.wall.UnixMilli(), FirstSimulationNS: 2000000000, LastSimulationNS: 5000000000}
	return r, s, tele
}
func TestPatrolReturnMeasuredCompletion(t *testing.T) {
	r, s, tele := arrivalRig(t)
	d := patrolEvaluate(t, r, s, tele)
	if d.Record.Status.Phase != mission.Succeeded || d.Action != mission.Stop || d.Record.PatrolReturnResult == nil {
		t.Fatal(d)
	}
	if d.Record.PatrolReturnResult.Arrival != *tele.PatrolArrival || d.Record.PatrolReturnResult.Position != tele.Position {
		t.Fatal("arrival provenance changed")
	}
	tele.PatrolArrival.MissionID = "mutated"
	got, _ := r.s.Get(s.ID)
	if got.PatrolReturnResult.Arrival.MissionID != s.ID {
		t.Fatal("result aliases input")
	}
}
func TestPatrolReturnRejectsInvalidArrival(t *testing.T) {
	for _, failure := range []string{"stale", "replayed-scope", "wrong-point", "moving", "turning", "short-wall", "short-simulation", "sample-mismatch", "missing-sensor", "wrong-frame", "revoked", "expired", "ordinary-api"} {
		t.Run(failure, func(t *testing.T) {
			r, s, tele := arrivalRig(t)
			switch failure {
			case "stale":
				tele.SampleUnixMS -= 5000
				tele.PatrolArrival.ObservedAtUnixMS = tele.SampleUnixMS
			case "replayed-scope":
				tele.PatrolArrival.MissionID = "old-mission"
			case "wrong-point":
				tele.Position = [3]float64{2, 0, 1}
			case "moving":
				tele.Velocity[0] = .5
			case "turning":
				tele.PatrolArrival.AngularSpeed = .3
			case "short-wall":
				tele.PatrolArrival.SettledSinceUnixMS = tele.SampleUnixMS - 1999
			case "short-simulation":
				tele.PatrolArrival.FirstSimulationNS = tele.SampleSimulationNS - 1999999999
			case "sample-mismatch":
				tele.SampleSimulationNS++
			case "missing-sensor":
				delete(tele.Sensors, "lidar")
			case "wrong-frame":
				tele.Frame.Revision = "wrong"
			case "revoked":
				r.s.cfg.ValidateAuthority = func(Record) error { return errors.New("revoked") }
			case "expired":
				r.wall = r.wall.Add(time.Minute)
				r.mono += time.Minute
			}
			var d Decision
			if failure == "ordinary-api" {
				d, _ = r.s.Evaluate(s.ID, "app:nav", r.epoch, tele)
			} else {
				d, _ = r.s.EvaluatePatrol(s.ID, "app:nav", r.epoch, tele)
			}
			if d.Record.Status.Phase == mission.Succeeded || d.Record.PatrolReturnResult != nil {
				t.Fatalf("invalid arrival completed: %+v", d)
			}
		})
	}
}
func TestReturnPolicyAndOtherMove(t *testing.T) {
	r, s, tele := arrivalRig(t)
	for _, mutate := range []func(*Spec){func(s *Spec) { s.Patrol.Return.Position = nil }, func(s *Spec) { s.Patrol.Return.Position[0] = 100 }, func(s *Spec) { s.Patrol.Return.BeforeExpirySeconds = 60 }, func(s *Spec) { s.Fallback = mission.Return }, func(s *Spec) { s.Domain = mission.Physical }} {
		c := copyRecord(Record{Spec: s}).Spec
		mutate(&c)
		if validateSpec(c) == nil {
			t.Fatal("invalid return policy admitted")
		}
	}
	ordinary := r.spec("ordinary")
	if _, err := r.s.Accept(ordinary, r.auth()); err != nil {
		t.Fatal(err)
	}
	tele.PatrolArrival.MissionID = ordinary.ID
	d, err := r.s.EvaluatePatrol(ordinary.ID, "app:nav", r.epoch, tele)
	if err == nil || d.Record.Status.Phase == mission.Succeeded {
		t.Fatal("ordinary MOVE used patrol path")
	}
}
