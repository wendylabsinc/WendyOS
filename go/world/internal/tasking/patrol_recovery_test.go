package tasking

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/mission"
)

func patrolRig(t *testing.T) (*rig, Spec, Telemetry) {
	t.Helper()
	r := setup(t)
	r.s.Close()
	r.cfg.Catalog[PatrolType] = mission.Move
	var err error
	r.s, err = Open(r.path, r.cfg)
	if err != nil {
		t.Fatal(err)
	}
	r.epoch, err = r.s.BeginExecutor("app:nav")
	if err != nil {
		t.Fatal(err)
	}
	s := r.spec("patrol-recovery")
	s.Type, s.SpeedMPS, s.MinimumEnergy = PatrolType, .55, 0
	s.RequiredSensors = []string{"lidar", "odometry", "localization"}
	s.Patrol = &PatrolPolicy{Rooms: []string{"room"}, DurationSeconds: 60, ObservationPolicy: "observe-and-report", DwellSeconds: 2, NoProgressSeconds: 15, DeferSeconds: 30}
	if _, err = r.s.Accept(s, r.auth()); err != nil {
		t.Fatal(err)
	}
	tele := r.telemetry()
	tele.Sensors = map[string]bool{"lidar": true, "odometry": true, "localization": true}
	return r, s, tele
}

func patrolEvaluate(t *testing.T, r *rig, s Spec, tele Telemetry) Decision {
	t.Helper()
	d, err := r.s.EvaluatePatrol(s.ID, "app:nav", r.epoch, tele)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestPatrolTransientRecoveryKeepsSingleSignedTask(t *testing.T) {
	for _, loss := range []string{"readiness", "lidar", "stale"} {
		t.Run(loss, func(t *testing.T) {
			r, s, fresh := patrolRig(t)
			if d := patrolEvaluate(t, r, s, fresh); d.Action != mission.Continue {
				t.Fatal(d)
			}
			bad := fresh
			switch loss {
			case "readiness":
				bad.ObstacleClear, bad.Sensors = false, nil
			case "lidar":
				bad.Sensors = map[string]bool{"odometry": true, "localization": true}
			case "stale":
				bad.SampleUnixMS -= 2000
			}
			held := patrolEvaluate(t, r, s, bad)
			if held.Action != mission.Hold || held.Record.Status.Phase != mission.Held {
				t.Fatal(held)
			}
			// The ordinary API still latches even a patrol until its executor
			// explicitly selects the patrol recovery contract.
			if d := r.evaluate(t, s.ID, fresh); d.Action != mission.Hold {
				t.Fatal("ordinary Evaluate resumed", d)
			}
			resumed := patrolEvaluate(t, r, s, fresh)
			if resumed.Action != mission.Continue || resumed.Record.Status.Phase != mission.Running || resumed.Record.Status.Version <= held.Record.Status.Version {
				t.Fatal(resumed)
			}
			before, _ := digest(s)
			after, _ := digest(resumed.Record.Spec)
			if before != after {
				t.Fatal("recovery changed immutable signed task")
			}
			records, _ := r.s.List()
			if len(records) != 1 {
				t.Fatal("recovery created another mission", len(records))
			}
		})
	}
}

func TestPatrolRecoveryDoesNotReleaseFaultOrAreaHold(t *testing.T) {
	for _, cause := range []string{"fault", "area"} {
		t.Run(cause, func(t *testing.T) {
			r, s, fresh := patrolRig(t)
			bad := fresh
			if cause == "fault" {
				bad.LocalFault = true
			} else {
				bad.Position[0] = 100
			}
			if d := patrolEvaluate(t, r, s, bad); d.Record.Status.Phase != mission.Held {
				t.Fatal(d)
			}
			if d := patrolEvaluate(t, r, s, fresh); d.Action != mission.Hold {
				t.Fatal("nontransient hold resumed", d)
			}
		})
	}
}

func TestPatrolRecoveryRequiresRetainedAuthorityAndEpoch(t *testing.T) {
	for _, loss := range []string{"expired", "revoked", "restart", "stop", "wrong-epoch", "canceled-context", "expires-during-authority"} {
		t.Run(loss, func(t *testing.T) {
			r, s, fresh := patrolRig(t)
			bad := fresh
			bad.Sensors = nil
			patrolEvaluate(t, r, s, bad)
			ctx := context.Background()
			epoch := r.epoch
			switch loss {
			case "expired":
				r.wall = r.wall.Add(2 * time.Minute)
				r.mono += 2 * time.Minute
				fresh.SampleUnixMS = r.wall.UnixMilli()
			case "revoked":
				r.s.cfg.ValidateAuthority = func(Record) error { return errors.New("revoked") }
			case "restart":
				var err error
				epoch, err = r.s.BeginExecutor("app:nav")
				if err != nil {
					t.Fatal(err)
				}
			case "stop":
				if _, err := r.s.Stop(s.ID, r.auth()); err != nil {
					t.Fatal(err)
				}
			case "wrong-epoch":
				epoch++
			case "canceled-context":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			case "expires-during-authority":
				r.s.cfg.ValidateAuthority = func(Record) error { r.wall = r.wall.Add(2 * time.Minute); r.mono += 2 * time.Minute; return nil }
			}
			d, _ := r.s.EvaluatePatrolContext(ctx, s.ID, "app:nav", epoch, fresh)
			if d.Action == mission.Continue {
				t.Fatal("invalid recovery continued", d)
			}
		})
	}
}

func TestPatrolRecoveryRejectsOtherCatalog(t *testing.T) {
	r := setup(t)
	r.accept(t, "ordinary")
	bad := r.telemetry()
	bad.Sensors = nil
	r.evaluate(t, "ordinary", bad)
	if d, err := r.s.EvaluatePatrol("ordinary", "app:nav", r.epoch, r.telemetry()); err == nil || d.Action == mission.Continue {
		t.Fatal("ordinary mission recovered through patrol API", d, err)
	}
	if d := r.evaluate(t, "ordinary", r.telemetry()); d.Action != mission.Hold {
		t.Fatal("ordinary mission was mutated", d)
	}
}
