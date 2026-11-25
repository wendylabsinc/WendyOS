// Package scenario composes the World and mission prototypes in a deterministic,
// in-process fixture. It opens no network, simulator, credential or hardware API.
package scenario

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/mission"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

const domain = "simulation:partition-demo"

type Step struct {
	Name      string                       `json:"name"`
	ElapsedMS int64                        `json:"elapsed_ms"`
	Connected bool                         `json:"robot_connected"`
	Action    mission.Action               `json:"task_action"`
	Phase     mission.Phase                `json:"task_phase"`
	Reason    string                       `json:"task_reason"`
	Replicas  map[string][]world.Component `json:"replicas"`
}

type Report struct {
	Schema      string                 `json:"schema"`
	Scope       string                 `json:"scope"`
	Authority   string                 `json:"authority"`
	ClockDomain string                 `json:"clock_domain"`
	Anchor      world.GeographicAnchor `json:"illustrative_anchor"`
	Steps       []Step                 `json:"steps"`
	Checks      map[string]bool        `json:"checks"`
}

// Run demonstrates three distinct replica objects and one mission engine. The
// fixture supplies trusted authority directly; it does not verify signatures.
func Run() (Report, error) {
	var elapsed time.Duration
	base := time.Unix(1, 0).UTC() // Numerical origin for simulation time, not UTC evidence.
	frame := world.FrameRef{ID: "synthetic-office-floor", Revision: "1"}
	anchor := world.GeographicAnchor{LatitudeDegrees: 0, LongitudeDegrees: 0, AltitudeDatum: "illustrative-WGS84-ellipsoid", UncertaintyMetres: 100}
	if err := anchor.Validate(); err != nil {
		return Report{}, err
	}
	replicas := map[string]*world.Replica{}
	for _, node := range []string{"robot", "gateway", "operator"} {
		r, err := world.NewReplica(world.Config{Tenant: "fixture-tenant", Site: "synthetic-office", ClockDomain: domain, Now: func() time.Time { return base.Add(elapsed) }, MaxEntries: 32, AllowSynthetic: true, Frames: []world.FrameRef{frame}})
		if err != nil {
			return Report{}, err
		}
		replicas[node] = r
	}
	engine, err := mission.New(mission.Config{Assignee: "robot/inspection", Site: "synthetic-office", Domain: mission.Simulation, Catalog: map[string]mission.SafetyClass{"inspect": mission.Move}, Now: func() time.Duration { return elapsed }, MaxLease: time.Second})
	if err != nil {
		return Report{}, err
	}
	task := mission.Task{ID: "inspection-1", Type: "inspect", Assignee: "robot/inspection", Site: "synthetic-office", Domain: mission.Simulation, Class: mission.Move, Mode: mission.Autonomous, Deadline: 30 * time.Second, Envelope: mission.Envelope{AreaID: frame.ID, RequiredSensors: []string{"local-obstacles", "gas"}, MinimumEnergy: 0.2, Fallback: mission.Hold}}
	authority := mission.Authority{TaskDigest: mission.Digest(task), Principal: "fixture-operator", Operator: true, AllowAutonomy: true, Site: task.Site, Assignee: task.Assignee, Domain: task.Domain, ValidUntil: 30 * time.Second}
	if _, err := engine.Accept(task, authority); err != nil {
		return Report{}, err
	}
	conditions := mission.Conditions{InsideArea: true, ObstacleClear: true, Energy: 0.8, Sensors: map[string]bool{"local-obstacles": true, "gas": true}}
	robotSource := world.Source{Device: "fixture-robot", App: "fixture-sensors", Service: "publisher", Stream: "observations"}
	trackSource := world.Source{Device: "fixture-gateway", App: "fixture-camera", Service: "tracker", Stream: "people"}
	component := func(source world.Source, entity, kind string, seq uint64, ttl time.Duration, value any) (world.Component, error) {
		payload, err := json.Marshal(value)
		if err != nil {
			return world.Component{}, err
		}
		return world.Component{Key: world.Key{Tenant: "fixture-tenant", Site: "synthetic-office", Entity: entity, Type: kind, Source: source}, Version: world.Version{Epoch: 1, Sequence: seq}, Frame: frame, Timing: world.Timing{SampleTime: base.Add(elapsed), ClockDomain: domain, ErrorKnown: true}, TTL: ttl, Provenance: world.Provenance{Origin: world.Simulated, AppVersion: "fixture-v0", Simulation: &world.SimulationContext{RunID: "partition-demo", ScenarioVersion: "1", Seed: 1, ClockDomain: domain}}, Payload: payload}, nil
	}
	pose := func(seq uint64, x float64) (world.Component, error) {
		return component(robotSource, "robot", "wendy.pose.v0", seq, 5*time.Second, map[string]any{"position_m": [3]float64{x, 0, 0}, "axis_convention": "right-handed-y-up"})
	}
	gas := func(seq uint64, value float64) (world.Component, error) {
		sample := world.ScalarSample{Value: value, Unit: "ppm", SensorType: "synthetic-gas", CalibrationRevision: "fixture-1", Uncertainty: 1}
		observation := world.Observation{ID: fmt.Sprintf("gas-%d", seq), Tenant: "fixture-tenant", Site: "synthetic-office", Source: robotSource, Frame: frame, Timing: world.Timing{SampleTime: base.Add(elapsed), ClockDomain: domain, ErrorKnown: true}, Provenance: world.Provenance{Origin: world.Simulated, AppVersion: "fixture-v0", Simulation: &world.SimulationContext{RunID: "partition-demo", ScenarioVersion: "1", Seed: 1, ClockDomain: domain}}, Scalar: &sample}
		if err := observation.Validate(); err != nil {
			return world.Component{}, err
		}
		return component(robotSource, "robot-gas", "wendy.gas.v0", seq, 5*time.Second, observation)
	}
	connected := true
	// Delivery is deliberately explicit. The origin argument is fixture-trusted,
	// not inferred from a network neighbor or a caller-controlled signed envelope.
	deliver := func(from, to string, c world.Component) (bool, error) {
		if !connected && from != to && (from == "robot" || to == "robot") {
			return false, nil
		}
		_, err := replicas[to].Apply(c.Key.Source, c)
		return err == nil, err
	}
	report := Report{Schema: "wendy.scenario.partition.v0", Scope: "deterministic in-process synthetic fixture; no MuJoCo, VM, network or hardware execution", Authority: "fixture-supplied trusted policy input; no signature verification", ClockDomain: domain, Anchor: anchor, Steps: []Step{}, Checks: map[string]bool{}}
	recordStep := func(name string, d mission.Decision) error {
		s := Step{Name: name, ElapsedMS: elapsed.Milliseconds(), Connected: connected, Action: d.Action, Phase: d.Status.Phase, Reason: d.Status.Reason, Replicas: map[string][]world.Component{}}
		for _, node := range []string{"robot", "gateway", "operator"} {
			values, err := replicas[node].Snapshot()
			if err != nil {
				return err
			}
			s.Replicas[node] = values
		}
		report.Steps = append(report.Steps, s)
		return nil
	}
	p1, err := pose(1, 0)
	if err != nil {
		return Report{}, err
	}
	g1, err := gas(1, 4)
	if err != nil {
		return Report{}, err
	}
	track, err := component(trackSource, "person-short-lived", "wendy.pose.v0", 1, 2*time.Second, map[string]any{"position_m": [3]float64{1, 0, 1}})
	if err != nil {
		return Report{}, err
	}
	for _, node := range []string{"robot", "gateway", "operator"} {
		for _, c := range []world.Component{p1, g1, track} {
			if _, err := deliver("gateway", node, c); err != nil {
				return Report{}, err
			}
		}
	}
	d, err := engine.Evaluate(task.ID, conditions)
	if err != nil {
		return Report{}, err
	}
	if err := recordStep("shared-world", d); err != nil {
		return Report{}, err
	}
	if d.Action != mission.Continue {
		return Report{}, fmt.Errorf("initial mission did not start")
	}

	elapsed = time.Second
	connected = false
	p2, err := pose(2, 1)
	if err != nil {
		return Report{}, err
	}
	g2, err := gas(2, 12)
	if err != nil {
		return Report{}, err
	}
	blocked := 0
	for _, c := range []world.Component{p2, g2} {
		if _, err := deliver("robot", "robot", c); err != nil {
			return Report{}, err
		}
		for _, node := range []string{"gateway", "operator"} {
			sent, err := deliver("robot", node, c)
			if err != nil {
				return Report{}, err
			}
			if !sent {
				blocked++
			}
		}
	}
	d, err = engine.Evaluate(task.ID, conditions)
	if err != nil {
		return Report{}, err
	}
	report.Checks["partition_blocks_delivery"] = blocked == 4
	report.Checks["autonomy_continues_without_supervision"] = d.Action == mission.Continue && conditions.SupervisionUntil == 0
	if err := recordStep("partition-local-progress", d); err != nil {
		return Report{}, err
	}

	elapsed = 3 * time.Second
	conditions.Sensors["gas"] = false
	d, err = engine.Evaluate(task.ID, conditions)
	if err != nil {
		return Report{}, err
	}
	report.Checks["missing_local_sensor_holds_mission"] = d.Action == mission.Hold && d.Status.Phase == mission.Held
	if err := recordStep("partition-sensor-loss", d); err != nil {
		return Report{}, err
	}

	connected = true
	for _, node := range []string{"gateway", "operator"} {
		for _, c := range []world.Component{p2, g2, track, p1, g1} {
			if _, err := deliver("robot", node, c); err != nil {
				return Report{}, err
			}
		}
	}
	conditions.Sensors["gas"] = true
	d, err = engine.Evaluate(task.ID, conditions)
	if err != nil {
		return Report{}, err
	}
	report.Checks["reconnection_does_not_auto_resume"] = d.Action == mission.Hold
	report.Checks["expired_track_stays_absent"] = true
	report.Checks["latest_observations_reconcile"] = true
	for _, node := range []string{"robot", "gateway", "operator"} {
		if _, live, err := replicas[node].Get(track.Key); err != nil {
			return Report{}, err
		} else if live {
			report.Checks["expired_track_stays_absent"] = false
		}
		for _, key := range []world.Key{p2.Key, g2.Key} {
			c, live, err := replicas[node].Get(key)
			if err != nil {
				return Report{}, err
			}
			if !live || c.Version.Sequence != 2 {
				report.Checks["latest_observations_reconcile"] = false
			}
		}
	}
	if err := recordStep("reconnected-and-reconciled", d); err != nil {
		return Report{}, err
	}
	for name, passed := range report.Checks {
		if !passed {
			return Report{}, fmt.Errorf("scenario invariant failed: %s", name)
		}
	}
	return report, nil
}
