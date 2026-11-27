package go2executor

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/go2nav"
	"github.com/wendylabsinc/wendy/go/world/internal/tasking"
)

const MaxFaultJSONBytes = 256 << 10

// FaultSnapshot is an unsigned, bounded simulation diagnostic. It records the
// first local failure before braking/reporting changes state. It does not prove
// physical clearance, current freshness, instruction authority or dispatch.
// FaultTiming contains local monotonic observations only. Null means unknown,
// including faults before decision start or a detected clock fault. Evaluate
// time includes client/network/server work; it does not identify server latency.
type FaultTiming struct {
	ConfiguredBudgetMS        int64    `json:"configured_budget_ms"`
	ClockValid                bool     `json:"clock_valid"`
	BeforeEvaluateMS          *float64 `json:"before_evaluate_ms"`
	EvaluateElapsedMS         *float64 `json:"evaluate_elapsed_ms"`
	EffectiveEvaluateBudgetMS *float64 `json:"effective_evaluate_budget_ms"`
	FaultElapsedMS            *float64 `json:"fault_elapsed_ms"`
}

func copyTiming(in FaultTiming) FaultTiming {
	cp := func(p *float64) *float64 {
		if p == nil {
			return nil
		}
		v := *p
		return &v
	}
	in.BeforeEvaluateMS = cp(in.BeforeEvaluateMS)
	in.EvaluateElapsedMS = cp(in.EvaluateElapsedMS)
	in.EffectiveEvaluateBudgetMS = cp(in.EffectiveEvaluateBudgetMS)
	in.FaultElapsedMS = cp(in.FaultElapsedMS)
	return in
}
func millis(d time.Duration) *float64 { v := float64(d) / float64(time.Millisecond); return &v }
func (c *Controller) diagnosticSample() (time.Duration, bool) {
	if !c.timingStarted || c.timingInvalid {
		return 0, false
	}
	now := c.cfg.Monotonic()
	if now < c.timingStart || now < c.timingLast || now < c.lastMono {
		c.timingInvalid = true
		return 0, false
	}
	c.timingLast = now
	return now, true
}
func (c *Controller) beginEvaluateTiming(ctx context.Context) time.Duration {
	now, ok := c.diagnosticSample()
	if !ok {
		return -1
	}
	c.faultTiming.BeforeEvaluateMS = millis(now - c.timingStart)
	if d, ok := ctx.Deadline(); ok {
		c.faultTiming.EffectiveEvaluateBudgetMS = millis(max(0, time.Until(d)))
	}
	return now
}
func (c *Controller) endEvaluateTiming(start time.Duration) {
	if now, ok := c.diagnosticSample(); ok && start >= 0 {
		c.faultTiming.EvaluateElapsedMS = millis(now - start)
	}
}
func (c *Controller) freezeTiming() FaultTiming {
	timing := copyTiming(c.faultTiming)
	timing.ConfiguredBudgetMS = c.profile.DecisionMaxMS
	if now, ok := c.diagnosticSample(); ok {
		timing.ClockValid = true
		timing.FaultElapsedMS = millis(now - c.timingStart)
	} else {
		timing = FaultTiming{ConfiguredBudgetMS: c.profile.DecisionMaxMS}
	}
	return timing
}

type FaultSnapshot struct {
	Selection               *SelectionDiagnostic  `json:"selection,omitempty"`
	Gait                    *GaitDiagnostic       `json:"gait,omitempty"`
	BridgeDiagnostics       *BridgeDiagnostics    `json:"bridge_diagnostics,omitempty"`
	Timing                  FaultTiming           `json:"timing"`
	Schema                  string                `json:"schema"`
	Simulation              bool                  `json:"simulation"`
	OperationalAuthority    bool                  `json:"operational_authority"`
	Complete                bool                  `json:"complete"`
	TaskID                  string                `json:"task_id"`
	SpecSHA256              string                `json:"spec_sha256"`
	Spec                    tasking.Spec          `json:"spec"`
	ExecutorEpoch           uint64                `json:"executor_epoch"`
	Stage                   string                `json:"stage"`
	Failure                 string                `json:"failure"`
	State                   State                 `json:"-"`
	Alignment               Alignment             `json:"alignment"`
	Profile                 Profile               `json:"profile"`
	Route                   []go2nav.Point        `json:"route_ros"`
	Waypoint                int                   `json:"waypoint"`
	PreviousCommand         [3]float64            `json:"controller_previous_command"`
	LastAttempt             *CommandRequest       `json:"last_attempt,omitempty"`
	LastAcknowledged        *CommandRequest       `json:"last_acknowledged,omitempty"`
	LastAcknowledgedReceipt *BridgeCommandReceipt `json:"last_acknowledged_bridge_receipt,omitempty"`
	LastCommandDiagnostic   *CommandDiagnostic    `json:"last_command_diagnostic,omitempty"`
	DecisionStartedUnixMS   int64                 `json:"decision_started_unix_ms"`
}

func diagnosticState(s State) (State, bool) {
	complete := true
	if s.Cloud != nil {
		cloud := *s.Cloud
		if len(cloud.Ranges) > go2nav.CloudRayCount {
			cloud.Ranges = cloud.Ranges[:go2nav.CloudRayCount]
			complete = false
		}
		s.Cloud = copyCloud(&cloud)
	}
	if len(s.Scan.Ranges) > 8192 {
		s.Scan.Ranges = s.Scan.Ranges[:8192]
		complete = false
	}
	s.Scan.Ranges = append([]float64(nil), s.Scan.Ranges...)
	if s.Scan.TiltedPose != nil {
		t := *s.Scan.TiltedPose
		s.Scan.TiltedPose = &t
	}
	return s, complete
}
func copyCommand(r *CommandRequest) *CommandRequest {
	if r == nil {
		return nil
	}
	out := *r
	return &out
}
func copyFault(in FaultSnapshot) FaultSnapshot {
	out := in
	out.Gait = copyGaitDiagnostic(in.Gait)
	out.Selection = copySelection(in.Selection)
	out.BridgeDiagnostics = copyBridgeDiagnostics(in.BridgeDiagnostics)
	if in.Profile.TiltedScan != nil {
		v := *in.Profile.TiltedScan
		out.Profile.TiltedScan = &v
	}
	if in.Profile.MaxMeasuredLateralMPS != nil {
		v := *in.Profile.MaxMeasuredLateralMPS
		out.Profile.MaxMeasuredLateralMPS = &v
	}
	if in.Profile.CloudExperiment != nil {
		v := *in.Profile.CloudExperiment
		out.Profile.CloudExperiment = &v
	}
	out.Timing = copyTiming(in.Timing)
	out.State, _ = diagnosticState(in.State)
	out.Spec = copyRecord(tasking.Record{Spec: in.Spec}).Spec
	out.Route = append([]go2nav.Point(nil), in.Route...)
	out.LastAttempt = copyCommand(in.LastAttempt)
	out.LastAcknowledged = copyCommand(in.LastAcknowledged)
	out.LastAcknowledgedReceipt = copyReceipt(in.LastAcknowledgedReceipt)
	out.LastCommandDiagnostic = copyCommandDiagnostic(in.LastCommandDiagnostic)
	return out
}
func (c *Controller) freezeFault(err error) {
	if c.faultSnapshot != nil {
		return
	}
	s, complete := diagnosticState(c.observedState)
	path := c.path
	if len(path) > 256 {
		path = path[:256]
		complete = false
	}
	failure := err.Error()
	if len(failure) > 512 {
		failure = failure[:512]
		complete = false
	}
	shot := FaultSnapshot{Timing: c.freezeTiming(), Schema: "wendy.world.go2-executor-fault.v1", Simulation: true, Complete: complete, TaskID: c.cfg.TaskID, SpecSHA256: tasking.SpecDigest(c.record.Spec), Spec: copyRecord(c.record).Spec, ExecutorEpoch: c.epoch, Stage: c.diagnosticStage, Failure: failure, State: s, Alignment: c.alignment, Profile: c.profile, Route: append([]go2nav.Point(nil), path...), Waypoint: c.waypoint, PreviousCommand: c.lastCommand, LastAttempt: copyCommand(c.lastAttempt), LastAcknowledged: copyCommand(c.lastAcknowledged), DecisionStartedUnixMS: c.decisionStartedUnixMS}
	shot.LastAcknowledgedReceipt = copyReceipt(c.lastAcknowledgedReceipt)
	shot.LastCommandDiagnostic = copyCommandDiagnostic(c.commandDiagnostic)
	shot.Gait = c.gaitDiagnostic()
	shot.Selection = copySelection(c.selectionDiagnostic)
	c.faultSnapshot = &shot
}

// FaultSnapshot returns a deep copy. Call only from the controller owner (for
// example its Run callback). There is no background logging or file writing.
func (c *Controller) FaultSnapshot() (*FaultSnapshot, bool) {
	if c.faultSnapshot == nil {
		return nil, false
	}
	out := copyFault(*c.faultSnapshot)
	return &out, true
}

// diagnosticValue retains finite numbers exactly; JSON null represents missing
// or nonfinite values, including unavailable scan rays. Numeric null never
// becomes a fabricated range. This codec is diagnostic-only, not admission.
func diagnosticValue(v reflect.Value) any {
	if v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return nil
		}
		return diagnosticValue(v.Elem())
	}
	switch v.Kind() {
	case reflect.Struct:
		out := map[string]any{}
		t := v.Type()
		for i := 0; i < v.NumField(); i++ {
			f := t.Field(i)
			if !f.IsExported() {
				continue
			}
			tag := strings.Split(f.Tag.Get("json"), ",")[0]
			if tag == "-" {
				continue
			}
			if tag == "" {
				tag = f.Name
			}
			out[tag] = diagnosticValue(v.Field(i))
		}
		return out
	case reflect.Slice, reflect.Array:
		out := make([]any, v.Len())
		for i := range out {
			out[i] = diagnosticValue(v.Index(i))
		}
		return out
	case reflect.Float64, reflect.Float32:
		f := v.Float()
		if !finite(f) {
			return nil
		}
		return f
	case reflect.String:
		return v.String()
	case reflect.Bool:
		return v.Bool()
	case reflect.Uint, reflect.Uint64, reflect.Uint32:
		return v.Uint()
	case reflect.Int, reflect.Int64, reflect.Int32:
		return v.Int()
	default:
		return nil
	}
}

func (s FaultSnapshot) MarshalJSON() ([]byte, error) {
	if len(s.State.Scan.Ranges) > 8192 || s.State.Cloud != nil && len(s.State.Cloud.Ranges) > go2nav.CloudRayCount || len(s.Route) > 256 || len(s.Failure) > 512 || len(s.Stage) > 64 {
		return nil, errors.New("fault diagnostic collection exceeds bound")
	}
	type ordinary FaultSnapshot
	out := diagnosticValue(reflect.ValueOf(ordinary(s))).(map[string]any)
	if s.BridgeDiagnostics == nil {
		delete(out, "bridge_diagnostics")
	}
	if s.Gait == nil {
		delete(out, "gait")
	}
	if s.Selection == nil {
		delete(out, "selection")
	}
	state := diagnosticValue(reflect.ValueOf(s.State)).(map[string]any)
	if s.State.Cloud == nil {
		delete(state, "cloud")
	}
	state["pose"] = diagnosticValue(reflect.ValueOf(wirePose{s.State.Pose.X, s.State.Pose.Y, s.State.Pose.Yaw}))
	scan := s.State.Scan
	ranges := make([]*float64, len(scan.Ranges))
	for i, r := range scan.Ranges {
		if finite(r) {
			v := r
			ranges[i] = &v
		}
	}
	wire := wireScan{TiltedPose: scan.TiltedPose, Sequence: scan.Sequence, CapturedUnixNS: scan.CapturedUnixNS, WorldEpoch: scan.WorldEpoch, Frame: scan.Frame, Ranges: ranges, AngleMin: scan.AngleMin, AngleIncrement: scan.AngleIncrement, RangeMin: scan.RangeMin, RangeMax: scan.RangeMax, Robot: wirePose{scan.Robot.X, scan.Robot.Y, scan.Robot.Yaw}, SensorMount: wirePose{scan.SensorMount.X, scan.SensorMount.Y, scan.SensorMount.Yaw}, MountKnown: scan.MountKnown, PositionUncertaintyM: scan.PositionUncertaintyM, YawUncertaintyRad: scan.YawUncertaintyRad, UncertaintyKnown: scan.UncertaintyKnown}
	scanValue := diagnosticValue(reflect.ValueOf(wire)).(map[string]any)
	if scan.TiltedPose == nil {
		delete(scanValue, "tilted_pose")
	}
	state["scan"] = scanValue
	if s.Profile.TiltedScan == nil {
		delete(out["profile"].(map[string]any), "tilted_scan")
	}
	if s.Profile.CloudExperiment == nil {
		delete(out["profile"].(map[string]any), "cloud_experiment")
	}
	if s.Profile.MaxMeasuredLateralMPS == nil {
		delete(out["profile"].(map[string]any), "max_measured_lateral_mps")
	}
	out["state"] = state
	b, err := json.Marshal(out)
	if err != nil {
		return nil, err
	}
	if len(b) > MaxFaultJSONBytes {
		return nil, errors.New("fault diagnostic exceeds output bound")
	}
	return b, nil
}
