package tasking

import (
	"context"
	"errors"
	"github.com/wendylabsinc/wendy/go/world/internal/mission"
	"github.com/wendylabsinc/wendy/go/world/internal/patrolenvironment"
	"github.com/wendylabsinc/wendy/go/world/internal/patroltour"
	"github.com/wendylabsinc/wendy/go/world/internal/patrolwatchpolicy"
	"github.com/wendylabsinc/wendy/go/world/internal/patrolwork"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
	"math"
)

const PatrolType = "wendy.nav.patrol.v1"

// PatrolPolicy is signed once with the mission. Destinations are internal
// progress, and observations never change this passive operating policy.
// PatrolReturn is an optional signed rendezvous in the mission's X/Y/Z frame.
// It is internal patrol progress, not the generic RETURN fallback.
type PatrolReturn struct {
	Position            []float64 `json:"position"`
	Yaw                 float64   `json:"yaw"`
	BeforeExpirySeconds int       `json:"before_expiry_seconds"`
}

// PatrolArrival is trusted executor evidence of a continuous settled interval.
// Simulation sample times are original Nav2 sensor-clock stamps, not wall time.
// Wall times describe local receipt, and never replace those sample timestamps.
type PatrolReturnResult struct {
	Arrival  PatrolArrival  `json:"arrival"`
	Position [3]float64     `json:"position"`
	Velocity [3]float64     `json:"velocity"`
	Frame    world.FrameRef `json:"frame"`
}
type PatrolArrival struct {
	Tour               *patroltour.Evidence `json:"tour,omitempty"`
	AngularSpeed       float64              `json:"angular_speed_rps"`
	MissionID          string               `json:"mission_id"`
	SettledSinceUnixMS int64                `json:"settled_since_unix_ms"`
	ObservedAtUnixMS   int64                `json:"observed_at_unix_ms"`
	FirstSimulationNS  int64                `json:"first_simulation_ns"`
	LastSimulationNS   int64                `json:"last_simulation_ns"`
}

type PatrolPolicy struct {
	Tour              *patroltour.Policy        `json:"tour,omitempty"`
	WorldWatch        *patrolwatchpolicy.Policy `json:"world_watch,omitempty"`
	Environment       *patrolenvironment.Policy `json:"environment,omitempty"`
	Offline           *PatrolOffline            `json:"offline,omitempty"`
	Work              *patrolwork.Policy        `json:"work,omitempty"`
	Exclusions        *PatrolExclusions         `json:"exclusions,omitempty"`
	Return            *PatrolReturn             `json:"return,omitempty"`
	Rooms             []string                  `json:"rooms"`
	RevisitSeconds    map[string]int            `json:"revisit_seconds,omitempty"`
	DurationSeconds   int                       `json:"duration_seconds"`
	ObservationPolicy string                    `json:"observation_policy"`
	DwellSeconds      int                       `json:"dwell_seconds"`
	NoProgressSeconds int                       `json:"no_progress_seconds"`
	DeferSeconds      int                       `json:"defer_seconds"`
}

func validatePatrolShape(s Spec) error {
	if s.Type != PatrolType {
		if s.Patrol != nil {
			return errors.New("patrol policy requires patrol catalog type")
		}
		return nil
	}
	p := s.Patrol
	if p == nil || s.Domain != mission.Simulation || s.Class != mission.Move || s.Mode != mission.Autonomous || s.Fallback != mission.Hold || s.Trigger != nil || s.Go2 != nil || s.InspectionDestination != nil || s.SpeedMPS != .55 || s.MinimumEnergy != 0 || p.ObservationPolicy != "observe-and-report" || p.DwellSeconds != 2 || p.NoProgressSeconds != 15 || p.DeferSeconds != 30 || p.DurationSeconds < 1 || p.DurationSeconds > 86400 || len(p.Rooms) == 0 || len(p.Rooms) > 32 {
		return errors.New("patrol requires a bounded passive autonomous Go2 simulation policy")
	}
	if p.Tour != nil {
		if err := p.Tour.Validate(); err != nil {
			return err
		}
		if p.Return == nil || len(p.RevisitSeconds) != 0 || len(p.Rooms) != len(p.Tour.Stops) {
			return errors.New("finite tour requires return, exact selected rooms and no revisit schedule")
		}
		for i, stop := range p.Tour.Stops {
			if stop.RoomID != p.Rooms[i] {
				return errors.New("tour room order differs")
			}
			for j, v := range stop.Position {
				if v < s.Area.Min[j] || v > s.Area.Max[j] {
					return errors.New("tour stop outside signed area")
				}
			}
		}
	}
	if p.WorldWatch != nil {
		if err := p.WorldWatch.Validate(); err != nil {
			return err
		}
		if p.WorldWatch.Contract.Field.Site != s.Site || p.WorldWatch.Contract.Field.Frame != s.Frame {
			return errors.New("World watch must use the mission site and frame")
		}
		for i, axis := range []int{0, 2} {
			if p.WorldWatch.MinXZ[i] < s.Area.Min[axis] || p.WorldWatch.MaxXZ[i] > s.Area.Max[axis] {
				return errors.New("World watch rectangle lies outside signed mission area")
			}
		}
	}
	if err := p.Offline.Validate(s); err != nil {
		return err
	}
	if p.Environment != nil {
		if err := p.Environment.Validate(); err != nil {
			return err
		}
	}
	if p.Work != nil {
		if err := p.Work.Validate(p.Return != nil); err != nil {
			return err
		}
	}
	if err := p.Exclusions.Validate(s.Frame, s.Area); err != nil {
		return err
	}
	if p.Return != nil {
		r := p.Return
		if len(r.Position) != 3 || !finite(r.Yaw) || math.Abs(r.Yaw) > math.Pi || r.BeforeExpirySeconds < 0 || r.BeforeExpirySeconds >= p.DurationSeconds {
			return errors.New("invalid signed patrol rendezvous or return window")
		}
		for i, v := range r.Position {
			if !finite(v) || v < s.Area.Min[i] || v > s.Area.Max[i] {
				return errors.New("rendezvous lies outside signed mission area")
			}
		}
	}
	seen := map[string]bool{}
	for _, id := range p.Rooms {
		if !go2Token.MatchString(id) || seen[id] {
			return errors.New("invalid or duplicate patrol room")
		}
		seen[id] = true
	}
	if len(p.RevisitSeconds) != 0 {
		if len(p.RevisitSeconds) != len(p.Rooms) {
			return errors.New("revisit schedule must cover every permitted room")
		}
		for id, seconds := range p.RevisitSeconds {
			if !seen[id] || seconds < 10 || seconds > 86400 {
				return errors.New("revisit intervals require permitted rooms and 10..86400 seconds")
			}
		}
	}
	seen = map[string]bool{}
	for _, id := range s.RequiredSensors {
		seen[id] = true
	}
	if len(s.RequiredSensors) != 3 || !seen["lidar"] || !seen["odometry"] || !seen["localization"] {
		return errors.New("patrol requires LiDAR, odometry and localization")
	}
	return nil
}

// EvaluatePatrol opts an admitted simulation patrol into transient observation
// recovery. It uses the same authority, freshness, persistence and executor
// epoch fences as Evaluate; it never creates or re-signs a waypoint task.
func (s *Store) EvaluatePatrol(id, actor string, epoch uint64, t Telemetry) (Decision, error) {
	return s.EvaluatePatrolContext(context.Background(), id, actor, epoch, t)
}

func (s *Store) EvaluatePatrolContext(ctx context.Context, id, actor string, epoch uint64, t Telemetry) (Decision, error) {
	return s.evaluateContext(ctx, id, actor, epoch, t, true, true)
}

func patrolArrived(s Spec, t Telemetry, wall int64) bool {
	a := t.PatrolArrival
	if s.Type != PatrolType || s.Patrol == nil || s.Patrol.Return == nil || validatePatrolShape(s) != nil || a == nil || a.MissionID != s.ID || !finite(a.AngularSpeed) || math.Abs(a.AngularSpeed) > .15 || a.ObservedAtUnixMS != t.SampleUnixMS || a.ObservedAtUnixMS > wall || a.SettledSinceUnixMS <= 0 || a.ObservedAtUnixMS-a.SettledSinceUnixMS < 2000 || a.ObservedAtUnixMS-a.SettledSinceUnixMS > 30000 || a.FirstSimulationNS <= 0 || a.LastSimulationNS <= a.FirstSimulationNS || a.LastSimulationNS-a.FirstSimulationNS < 2000000000 || a.LastSimulationNS != t.SampleSimulationNS {
		return false
	}
	if s.Patrol.Tour != nil && !s.Patrol.Tour.Complete(a.Tour, a.FirstSimulationNS) {
		return false
	}
	if s.Patrol.Tour == nil && a.Tour != nil {
		return false
	}
	copy := s
	copy.Target = [3]float64{s.Patrol.Return.Position[0], s.Patrol.Return.Position[1], s.Patrol.Return.Position[2]}
	return reached(copy, t)
}
