package tasking

import (
	"github.com/wendylabsinc/wendy/go/world/internal/mission"
	"testing"
	"time"
)

func TestPatrolOneMissionDoesNotCompleteAtWaypoint(t *testing.T) {
	r := setup(t)
	r.s.Close()
	r.cfg.Catalog = map[string]mission.SafetyClass{PatrolType: mission.Move}
	var err error
	r.s, err = Open(r.path, r.cfg)
	if err != nil {
		t.Fatal(err)
	}
	r.epoch, err = r.s.BeginExecutor("app:nav")
	if err != nil {
		t.Fatal(err)
	}
	s := r.spec("patrol")
	s.Type = PatrolType
	s.SpeedMPS = .55
	s.MinimumEnergy = 0
	s.RequiredSensors = []string{"lidar", "odometry", "localization"}
	s.Patrol = &PatrolPolicy{Rooms: []string{"room"}, DurationSeconds: 60, ObservationPolicy: "observe-and-report", DwellSeconds: 2, NoProgressSeconds: 15, DeferSeconds: 30}
	if _, err = r.s.Accept(s, r.auth()); err != nil {
		t.Fatal(err)
	}
	tele := r.telemetry()
	tele.Position = s.Target
	tele.Sensors = map[string]bool{"lidar": true, "odometry": true, "localization": true}
	if d := r.evaluate(t, s.ID, tele); d.Action != mission.Continue || d.Record.Status.Phase != mission.Running {
		t.Fatal(d)
	}
	r.s.Close()
	r.s, err = Open(r.path, r.cfg)
	if err != nil {
		t.Fatal(err)
	}
	record, err := r.s.Get(s.ID)
	if err != nil || record.Status.Phase != mission.Failed {
		t.Fatal("crashed patrol resumed", record, err)
	}
}
func TestPatrolRejectsChangedPassivePolicy(t *testing.T) {
	r := setup(t)
	s := r.spec("patrol")
	s.Type = PatrolType
	s.SpeedMPS = .55
	s.MinimumEnergy = 0
	s.RequiredSensors = []string{"lidar", "odometry", "localization"}
	s.Patrol = &PatrolPolicy{Rooms: []string{"room"}, DurationSeconds: 3600, ObservationPolicy: "observe-and-report", DwellSeconds: 2, NoProgressSeconds: 15, DeferSeconds: 30}
	s.NotAfterUnixMS = r.wall.Add(time.Hour).UnixMilli()
	if err := validateSpec(s); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*Spec){func(s *Spec) { s.Domain = mission.Physical }, func(s *Spec) { s.SpeedMPS = .1 }, func(s *Spec) { s.Class = mission.Observe }, func(s *Spec) { s.Patrol.ObservationPolicy = "follow-people" }} {
		copy := copyRecord(Record{Spec: s}).Spec
		mutate(&copy)
		if validateSpec(copy) == nil {
			t.Fatal("unsafe policy accepted")
		}
	}
}

func TestPatrolScheduleAdmissionPreservesScope(t *testing.T) {
	r := setup(t)
	s := r.spec("scheduled-patrol")
	s.Type = PatrolType
	s.SpeedMPS = .55
	s.MinimumEnergy = 0
	s.RequiredSensors = []string{"lidar", "odometry", "localization"}
	s.Patrol = &PatrolPolicy{Rooms: []string{"workshop", "office"}, DurationSeconds: 60, ObservationPolicy: "observe-and-report", DwellSeconds: 2, NoProgressSeconds: 15, DeferSeconds: 30, RevisitSeconds: map[string]int{"workshop": 10, "office": 60}}
	if err := validateSpec(s); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*Spec){
		func(s *Spec) { delete(s.Patrol.RevisitSeconds, "office") },
		func(s *Spec) { s.Patrol.RevisitSeconds["outside-envelope"] = 30 },
		func(s *Spec) { s.Patrol.RevisitSeconds["office"] = 9 },
		func(s *Spec) { s.Patrol.RevisitSeconds["office"] = 86401 },
		func(s *Spec) { s.Domain = mission.Physical },
		func(s *Spec) { s.RequiredSensors = nil },
		func(s *Spec) { s.Fallback = mission.Continue },
	} {
		copied := copyRecord(Record{Spec: s}).Spec
		mutate(&copied)
		if validateSpec(copied) == nil {
			t.Fatal("invalid schedule/authority scope accepted")
		}
	}
}
