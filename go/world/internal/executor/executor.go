// Package executor connects local mission decisions to an actuator adapter.
// This first driver is restricted to the synthetic one-axis MuJoCo robot.
package executor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/mission"
	"github.com/wendylabsinc/wendy/go/world/internal/supervision"
	"github.com/wendylabsinc/wendy/go/world/internal/tasking"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

type State struct {
	SchemaVersion           string          `json:"schema_version,omitempty"`
	RobotID                 string          `json:"robot_id,omitempty"`
	RunID                   string          `json:"run_id,omitempty"`
	Site                    string          `json:"site_id,omitempty"`
	Provenance              StateProvenance `json:"provenance,omitempty"`
	Session                 string          `json:"session"`
	BootID                  string          `json:"boot_id"`
	Sequence                uint64          `json:"sequence"`
	Position                [3]float64      `json:"position_m"`
	Velocity                [3]float64      `json:"velocity_mps"`
	CommandedVelocity       float64         `json:"commanded_velocity_mps"`
	PlanarCommandedVelocity [3]float64      `json:"-"`
	SimulationTimeS         float64         `json:"simulation_time_s"`
	SampleUnixMS            int64           `json:"sample_unix_ms"`
	Frame                   world.FrameRef  `json:"frame"`
	Sensors                 map[string]bool `json:"sensors"`
	ObstacleClear           bool            `json:"obstacle_clear"`
	Energy                  float64         `json:"energy"`
	Synthetic               bool            `json:"synthetic"`
}

type StateProvenance struct {
	Mode          string `json:"mode"`
	PackageSHA256 string `json:"package_sha256"`
}

// A native control context names the actual loaded twin, not just the gateway
// serving its tasks. Check before evaluation so a mismatched twin cannot even
// report target completion. Legacy tasks retain their existing adapter contract.
func validateControlState(s State, spec tasking.Spec) error {
	if spec.ControlContext == nil {
		return nil
	}
	c := spec.ControlContext.Context
	if s.RunID != c.RunID || s.Site != c.Site || s.Frame != c.Frame || s.Provenance.Mode != "simulated" || s.Provenance.PackageSHA256 != c.PackageSHA256 {
		return errors.New("actuator run, package or frame differs from commissioned mission context")
	}
	return nil
}

const commandLease = 300 * time.Millisecond
const maxSequence = 1<<53 - 1

func validateState(s State) error {
	if err := validateStateBase(s); err != nil {
		return err
	}
	if math.Abs(s.Position[1]-.2) > .000001 || math.Abs(s.Position[2]) > .000001 || math.Abs(s.Velocity[1]) > .000001 || math.Abs(s.Velocity[2]) > .000001 || math.Abs(s.Velocity[0]) > .501 {
		return errors.New("state does not match the synthetic one-axis robot")
	}
	return nil
}

func validateStateBase(s State) error {
	if !s.Synthetic || s.Session == "" || len(s.Session) > 128 || s.BootID == "" || len(s.BootID) > 128 || s.Sequence > maxSequence || s.SampleUnixMS <= 0 {
		return errors.New("invalid synthetic adapter identity or sample")
	}
	if math.IsNaN(s.SimulationTimeS) || math.IsInf(s.SimulationTimeS, 0) || s.SimulationTimeS < 0 || s.SimulationTimeS > (24*time.Hour).Seconds() {
		return errors.New("invalid adapter simulation time")
	}
	if err := s.Frame.Validate(); err != nil {
		return err
	}
	for i := range s.Position {
		if math.IsNaN(s.Position[i]) || math.IsInf(s.Position[i], 0) || math.IsNaN(s.Velocity[i]) || math.IsInf(s.Velocity[i], 0) {
			return errors.New("nonfinite adapter coordinates")
		}
	}
	if math.IsNaN(s.Energy) || math.IsInf(s.Energy, 0) || s.Energy < 0 || s.Energy > 1 {
		return errors.New("invalid adapter energy")
	}
	if math.IsNaN(s.CommandedVelocity) || math.IsInf(s.CommandedVelocity, 0) || math.Abs(s.CommandedVelocity) > .5 {
		return errors.New("invalid previous actuator command")
	}
	for _, name := range []string{"gas", "local-obstacles"} {
		if _, ok := s.Sensors[name]; !ok {
			return errors.New("adapter sensor availability missing")
		}
	}
	return nil
}

type Gateway interface {
	Begin(context.Context) (uint64, error)
	Tasks(context.Context) ([]tasking.Record, error)
	Evaluate(context.Context, string, uint64, tasking.Telemetry) (tasking.Decision, error)
}
type Adapter interface {
	Session(context.Context) (State, error)
	State(context.Context) (State, error)
	Command(context.Context, string, uint64, float64, time.Duration) error
	Stop(context.Context, string, uint64) error
}

// DeadlineAdapter must enforce the absolute simulation command deadline at
// arrival and throughout its independent watchdog. Older adapters cannot run
// supervised motion through the ordinary relative-lease command method.
type DeadlineAdapter interface {
	CommandUntil(context.Context, string, uint64, float64, time.Duration, int64) error
}
type Controller struct {
	// Planar is explicit opt-in. Nil preserves the original one-axis driver.
	Planar          *PlanarNavigation
	Gateway         Gateway
	Capture         Capture
	pending         *tasking.Record
	Adapter         Adapter
	epoch           uint64
	session         string
	boot            string
	sequence        uint64
	acceptedOffset  int
	decisionStarted time.Time
	gatewayBoot     string
	now             func() time.Time
	lastClock       time.Time
	clockFault      bool
}

func (c *Controller) clock() time.Time {
	now := time.Now()
	if c.now != nil {
		now = c.now()
	}
	if !c.lastClock.IsZero() && (now.Before(c.lastClock) || now.UnixMilli() < c.lastClock.UnixMilli()) {
		c.clockFault = true
	}
	c.lastClock = now
	return now
}

// Start brakes by rotating the adapter session before durably invalidating any
// previous executor generation. It must run before accepting new missions.
func (c *Controller) Start(ctx context.Context) error {
	if c.Gateway == nil || c.Adapter == nil {
		return errors.New("gateway and adapter required")
	}
	s, err := c.Adapter.Session(ctx)
	if err != nil {
		return err
	}
	if err := c.validateState(s); err != nil {
		return err
	}
	c.gatewayBoot = ""
	c.decisionStarted = time.Time{}
	c.session = s.Session
	c.boot = s.BootID
	c.sequence = s.Sequence
	c.epoch, err = c.Gateway.Begin(ctx)
	if err != nil {
		return err
	}
	if c.epoch == 0 {
		return errors.New("invalid executor epoch")
	}
	return nil
}
func (c *Controller) Brake(ctx context.Context) error {
	if c.session == "" {
		return nil
	}
	if c.sequence >= maxSequence {
		return errors.New("command sequence exhausted")
	}
	c.sequence++
	return c.Adapter.Stop(ctx, c.session, c.sequence)
}

func (c *Controller) Tick(ctx context.Context) (tasking.Decision, error) {
	c.pending = nil
	if c.epoch == 0 {
		return tasking.Decision{}, errors.New("executor has not started")
	}
	s, err := c.Adapter.State(ctx)
	if err != nil {
		return tasking.Decision{}, err
	}
	if !s.Synthetic || s.BootID != c.boot || s.Session != c.session {
		return tasking.Decision{}, errors.New("actuator session changed; explicit executor restart required")
	}
	if err := c.validateState(s); err != nil {
		return tasking.Decision{}, err
	}
	if age := c.clock().UnixMilli() - s.SampleUnixMS; age < 0 || age > 250 {
		return tasking.Decision{}, errors.New("adapter sample is stale or future-dated")
	}
	records, err := c.Gateway.Tasks(ctx)
	if err != nil {
		return tasking.Decision{}, err
	}
	// Preserve the holder of the actuator before selecting another accepted task.
	sort.Slice(records, func(i, j int) bool {
		pi, pj := priority(records[i]), priority(records[j])
		if pi != pj {
			return pi < pj
		}
		return records[i].Spec.ID < records[j].Spec.ID
	})
	telemetry := tasking.Telemetry{BootID: s.BootID, Session: s.Session, Frame: s.Frame, SampleUnixMS: s.SampleUnixMS, Position: s.Position, Velocity: s.Velocity, ObstacleClear: s.ObstacleClear, Energy: s.Energy, Sensors: s.Sensors}
	d, task, err := c.selectDecision(ctx, records, telemetry, s)
	if err != nil {
		return d, err
	}
	if task == nil {
		return tasking.Decision{}, c.Brake(ctx)
	}
	if d.Action != mission.Continue {
		return d, c.Brake(ctx)
	}
	if d.Record.Spec.ID != task.Spec.ID || d.Record.Status.Phase != mission.Running {
		return d, errors.New("decision does not match running task")
	}
	// The signed catalog type is a one-axis navigation adapter. Refuse out-of-plane
	// motion rather than silently treating a 3D instruction as an X-only target.
	if (d.Record.Spec.Type != "wendy.nav.go_to.v1" && d.Record.Spec.Type != tasking.InspectType) || d.Record.Spec.Domain != mission.Simulation || (d.Record.Spec.Mode != mission.Autonomous && d.Record.Spec.Mode != mission.Supervised) {
		return d, errors.New("unsupported task for synthetic navigation adapter")
	}
	if c.Planar != nil {
		if err := c.Planar.validateTask(d.Record.Spec); err != nil {
			return d, err
		}
	} else if d.Record.Spec.InspectionDestination != nil || math.Abs(d.Record.Spec.Target[1]-.2) > .000001 || math.Abs(d.Record.Spec.Target[2]) > .000001 || d.Record.Spec.Target[0] < -2 || d.Record.Spec.Target[0] > 2 {
		return d, errors.New("target is outside one-axis adapter kinematics")
	}
	if d.Record.Spec.Type == tasking.InspectType && settled(s, d.Record.Spec) {
		if c.Capture == nil {
			return d, errors.New("inspection capture is not configured")
		}
		if err := c.Brake(ctx); err != nil {
			return d, err
		}
		record := d.Record
		c.pending = &record
		return d, nil
	}
	if c.Planar != nil {
		return d, c.commandPlanar(ctx, s, d.Record.Spec)
	}
	errorX := d.Record.Spec.Target[0] - s.Position[0]
	velocity := math.Max(-d.Record.Spec.SpeedMPS, math.Min(d.Record.Spec.SpeedMPS, 2*errorX))
	if math.Abs(errorX) <= d.Record.Spec.ToleranceM {
		velocity = 0
	}
	velocity, brake, err := boundedVelocity(s, d.Record.Spec, velocity, c.clock())
	if err != nil {
		return d, err
	}
	if brake {
		return d, c.Brake(ctx)
	}
	if math.IsNaN(velocity) || math.IsInf(velocity, 0) || math.Abs(velocity) > 0.5 {
		return d, errors.New("invalid adapter velocity")
	}
	if c.sequence >= maxSequence {
		return d, errors.New("command sequence exhausted")
	}
	if d.Record.Spec.Mode == mission.Supervised {
		adapter, ok := c.Adapter.(DeadlineAdapter)
		if !ok {
			return d, errors.New("adapter cannot enforce supervised command deadlines")
		}
		lease, notAfter, err := c.supervisionCommandBudget(d, c.clock())
		if err != nil {
			return d, err
		}
		c.sequence++
		return d, adapter.CommandUntil(ctx, c.session, c.sequence, velocity, lease, notAfter)
	}
	c.sequence++
	return d, c.Adapter.Command(ctx, c.session, c.sequence, velocity, commandLease)
}

// Charge the entire Evaluate round trip and subsequent processing against the
// returned remaining budget. This intentionally double-counts server work; it
// cannot turn delayed delivery into fresh authority. Absolute clocks are an
// additional constraint in this explicit, isolated simulation domain.
func (c *Controller) supervisionCommandBudget(d tasking.Decision, now time.Time) (time.Duration, int64, error) {
	b := d.Supervision
	fail := func() (time.Duration, int64, error) {
		return 0, 0, errors.New("supervision decision expired or mismatched")
	}
	if c.clockFault || b == nil || d.Record.Spec.Domain != mission.Simulation || b.ExecutorEpoch != c.epoch || b.GatewayBootID == "" || len(b.GatewayBootID) > 128 || b.RemainingMS <= 0 || b.RemainingMS > supervision.MaxLease.Milliseconds() || c.decisionStarted.IsZero() || now.Before(c.decisionStarted) || now.UnixMilli() < c.decisionStarted.UnixMilli() {
		return fail()
	}
	if c.gatewayBoot != "" && c.gatewayBoot != b.GatewayBootID {
		return fail()
	}
	if b.ExpiresAtUnixMS <= now.UnixMilli() || b.ExpiresAtUnixMS-now.UnixMilli() > supervision.MaxLease.Milliseconds() || d.Record.Spec.NotAfterUnixMS <= 0 || d.Record.AuthorizationUntilUnixMS <= 0 || b.ExpiresAtUnixMS > d.Record.Spec.NotAfterUnixMS || b.ExpiresAtUnixMS > d.Record.AuthorizationUntilUnixMS {
		return fail()
	}
	deadline := c.decisionStarted.Add(time.Duration(b.RemainingMS) * time.Millisecond)
	remaining := deadline.Sub(now)
	notAfter := min(deadline.UnixMilli(), b.ExpiresAtUnixMS, now.Add(commandLease).UnixMilli())
	remaining = min(remaining, time.Duration(notAfter-now.UnixMilli())*time.Millisecond, commandLease)
	if remaining < time.Millisecond || notAfter <= now.UnixMilli() {
		return fail()
	}
	c.gatewayBoot = b.GatewayBootID
	return remaining, notAfter, nil
}

// A running or held task retains the actuator. An accepted supervised task
// waiting for its first lease does not reserve it or starve another ready task.
// Bound the number of gateway calls per tick, rotating over a long waiting list.
func (c *Controller) selectDecision(ctx context.Context, records []tasking.Record, telemetry tasking.Telemetry, state State) (tasking.Decision, *tasking.Record, error) {
	evaluate := func(task *tasking.Record) (tasking.Decision, error) {
		if err := validateControlState(state, task.Spec); err != nil {
			return tasking.Decision{}, err
		}
		if err := ctx.Err(); err != nil {
			return tasking.Decision{}, err
		}
		c.decisionStarted = c.clock()
		d, err := c.Gateway.Evaluate(ctx, task.Spec.ID, c.epoch, telemetry)
		if err != nil {
			return d, err
		}
		expected, e1 := json.Marshal(task.Spec)
		actual, e2 := json.Marshal(d.Record.Spec)
		if e1 != nil || e2 != nil || string(expected) != string(actual) {
			return d, errors.New("decision changed the selected immutable task")
		}
		return d, nil
	}
	accepted := make([]*tasking.Record, 0)
	for i := range records {
		r := &records[i]
		if r.Status.Phase.Terminal() || r.Spec.Class != mission.Move {
			continue
		}
		if priority(*r) == 0 {
			d, err := evaluate(r)
			return d, r, err
		}
		if r.Status.Phase != mission.Accepted {
			return tasking.Decision{}, nil, errors.New("unknown active task phase")
		}
		accepted = append(accepted, r)
	}
	if len(accepted) == 0 {
		return tasking.Decision{}, nil, nil
	}
	start := c.acceptedOffset % len(accepted)
	var last tasking.Decision
	var task *tasking.Record
	for i := 0; i < min(8, len(accepted)); i++ {
		index := (start + i) % len(accepted)
		task = accepted[index]
		c.acceptedOffset = (index + 1) % len(accepted)
		d, err := evaluate(task)
		if err != nil {
			return d, task, err
		}
		last = d
		if task.Spec.Mode == mission.Supervised && d.Action == mission.Hold && d.Record.Status.Phase == mission.Accepted {
			continue
		}
		return d, task, nil
	}
	return last, task, nil
}

// Reserve distance for the full lease, a one-second adapter request timeout and
// two physics ticks. Using the larger of measured and commanded outward speed
// also covers the fixture's velocity-servo lag. This assumes the documented
// ideal simulated brake and bounded synthetic dynamics, not physical stopping.
func boundedVelocity(s State, spec tasking.Spec, velocity float64, now time.Time) (float64, bool, error) {
	age := now.UnixMilli() - s.SampleUnixMS
	if age < 0 || age > 250 {
		return 0, true, errors.New("adapter sample is stale or future-dated")
	}
	if spec.SpeedMPS <= 0 || spec.SpeedMPS > .5 || math.IsNaN(spec.SpeedMPS) || math.IsNaN(velocity) || math.IsInf(velocity, 0) {
		return 0, true, errors.New("invalid navigation speed")
	}
	lo, hi := math.Max(-2, spec.Area.Min[0]), math.Min(2, spec.Area.Max[0])
	if math.IsNaN(lo) || math.IsNaN(hi) || lo >= hi || s.Position[0] < lo || s.Position[0] > hi {
		return 0, true, errors.New("outside synthetic motion bounds")
	}
	horizon := (commandLease + time.Second + 20*time.Millisecond).Seconds() + float64(age)/1000
	left, right := math.Max(0, s.Position[0]-lo-.001), math.Max(0, hi-s.Position[0]-.001)
	if math.Max(s.Velocity[0], s.CommandedVelocity) > right/horizon || math.Max(-s.Velocity[0], -s.CommandedVelocity) > left/horizon {
		return 0, true, nil
	}
	return math.Max(-left/horizon, math.Min(right/horizon, velocity)), false, nil
}
func priority(r tasking.Record) int {
	switch r.Status.Phase {
	case mission.Running, mission.Returning, mission.Held:
		return 0
	case mission.Accepted:
		return 1
	default:
		return 2
	}
}

// Run never renews an actuator lease after a failed or stale decision. The
// independent adapter watchdog remains effective if this process is killed.
func (c *Controller) Run(ctx context.Context, report func(tasking.Decision, error)) error {
	startup, cancel := context.WithTimeout(ctx, 5*time.Second)
	err := c.Start(startup)
	cancel()
	if err != nil {
		return err
	}
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	defer func() {
		stop, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
		defer cancel()
		_ = c.Brake(stop)
	}()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			tick, cancel := context.WithTimeout(ctx, 250*time.Millisecond)
			d, err := c.Tick(tick)
			cancel()
			if err == nil && c.pending != nil {
				capture, cancel := context.WithTimeout(ctx, 3*time.Second)
				d, err = c.Inspect(capture)
				cancel()
			}
			if err != nil {
				stop, cancel := context.WithTimeout(ctx, 250*time.Millisecond)
				stopErr := c.Brake(stop)
				cancel()
				if stopErr != nil {
					err = fmt.Errorf("%w; adapter brake failed: %v", err, stopErr)
				}
			}
			if report != nil {
				report(d, err)
			}
		}
	}
}
