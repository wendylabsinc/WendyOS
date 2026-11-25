package go2executor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/executor"
	"github.com/wendylabsinc/wendy/go/world/internal/go2nav"
	"github.com/wendylabsinc/wendy/go/world/internal/mission"
	"github.com/wendylabsinc/wendy/go/world/internal/tasking"
)

const maxSequence = 1<<53 - 1

// Controller is single-owner: callers serialize Start, Tick and Brake. One
// explicitly selected task runs per process/session. Errors latch locally; only
// failure reporting and zero commands can continue after an ambiguous outcome.
type Controller struct {
	faultTiming                                                   FaultTiming
	timingStarted, timingInvalid                                  bool
	timingStart, timingLast                                       time.Duration
	observedState                                                 State
	diagnosticStage                                               string
	decisionStartedUnixMS                                         int64
	lastAttempt, lastAcknowledged                                 *CommandRequest
	lastAcknowledgedReceipt                                       *BridgeCommandReceipt
	commandDiagnostic                                             *CommandDiagnostic
	selectionDiagnostic                                           *SelectionDiagnostic
	gait                                                          gaitTransition
	faultSnapshot                                                 *FaultSnapshot
	gateway                                                       executor.Gateway
	adapter                                                       Adapter
	cfg                                                           Config
	alignment                                                     Alignment
	profile                                                       Profile
	started, finished, bound                                      bool
	session, boot, odomPublisher, scanPublisher, commandPublisher string
	epoch, sequence, capture, usedCapture                         uint64
	lastWall                                                      int64
	lastWallNS                                                    int64
	lastMono                                                      time.Duration
	clockSet                                                      bool
	clockWall                                                     int64
	clockMono                                                     time.Duration
	record                                                        tasking.Record
	lastState                                                     State
	lastCommand                                                   [3]float64
	path                                                          []go2nav.Point
	waypoint                                                      int
	fault                                                         error
	cloudPrevious                                                 *CloudState
	cloudOdomCapturedUnixNS                                       int64
}

func New(g executor.Gateway, a Adapter, cfg Config) (*Controller, error) {
	if g == nil || a == nil || cfg.Site == "" || cfg.TaskID == "" || len(cfg.TaskID) > 128 {
		return nil, errors.New("explicit task, gateway and simulation adapter required")
	}
	if cfg.Binding.Validate() != nil {
		return nil, errors.New("invalid Go2 execution binding")
	}
	alignment, profile, err := DecodeConfiguration(cfg)
	if err != nil {
		return nil, err
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Monotonic == nil {
		start := time.Now()
		cfg.Monotonic = func() time.Duration { return time.Since(start) }
	}
	cfg.AlignmentJSON = append([]byte(nil), cfg.AlignmentJSON...)
	cfg.ProfileJSON = append([]byte(nil), cfg.ProfileJSON...)
	return &Controller{gateway: g, adapter: a, cfg: cfg, alignment: alignment, profile: profile}, nil
}
func (c *Controller) clock() (int64, time.Duration, error) {
	now := c.cfg.Now()
	w, m := now.UnixMilli(), c.cfg.Monotonic()
	if w <= 0 || m < 0 || w < c.lastWall || m < c.lastMono {
		c.timingInvalid = true
		return 0, 0, errors.New("controller clock regression")
	}
	if !c.clockSet {
		c.clockSet = true
		c.clockWall = w
		c.clockMono = m
	}
	if absInt((w-c.clockWall)-(m-c.clockMono).Milliseconds()) > c.profile.ClockErrorMS {
		c.timingInvalid = true
		return 0, 0, errors.New("controller wall and monotonic clocks diverged")
	}
	c.lastWall, c.lastMono = w, m
	c.lastWallNS = now.UnixNano()
	return w, m, nil
}
func (c *Controller) Start(ctx context.Context) (err error) {
	if c.started {
		return errors.New("controller cannot restart or re-arm")
	}
	c.started = true
	defer func() {
		if err != nil {
			c.freezeFault(err)
			c.fault = err
			_ = c.brakeBounded()
			c.captureStartupDiagnostics()
		}
	}()
	if _, _, err = c.clock(); err != nil {
		return err
	}
	c.diagnosticStage = "session"
	s, err := c.adapter.Session(ctx, c.cfg.Binding)
	if err != nil {
		return err
	}
	s = snapshotState(s)
	c.observedState = s
	c.session, c.boot = s.Session, s.BridgeBootID
	c.sequence = s.Sequence
	c.odomPublisher, c.scanPublisher, c.commandPublisher = s.OdomPublisher, s.ScanPublisher, s.CommandPublisher
	c.observedState = s
	if err = c.validateState(s); err != nil {
		return err
	}
	c.lastState = s
	c.diagnosticStage = "executor-begin"
	epoch, err := c.gateway.Begin(ctx)
	if err != nil {
		return err
	}
	if epoch == 0 {
		return errors.New("zero durable executor generation")
	}
	c.epoch = epoch
	return nil
}

// Generation reports the successfully acknowledged durable executor generation.
// Like the controller, it is single-owner; call it from the Run callback, not
// concurrently. A positive value is startup evidence, never motion authority.
func (c *Controller) Generation() uint64 { return c.epoch }
func copyRecord(r tasking.Record) tasking.Record {
	b, _ := json.Marshal(r)
	var x tasking.Record
	_ = json.Unmarshal(b, &x)
	return x
}
func (c *Controller) validateSpec(r tasking.Record) error {
	s := r.Spec
	if err := tasking.ValidateGo2Spec(s, &c.cfg.Binding, c.cfg.Binding.RunID); err != nil {
		return err
	}
	if s.ID != c.cfg.TaskID || s.Type != tasking.Go2GoToType || s.Go2 == nil || !same(*s.Go2, c.cfg.Binding) || s.Domain != mission.Simulation || s.Class != mission.Move || s.Mode != mission.Autonomous || s.Frame != c.cfg.Binding.Frame || s.Site != c.cfg.Site || s.Target[1] != 0 || s.MinimumEnergy != 0 || s.SpeedMPS <= 0 || s.SpeedMPS > c.profile.MaxForwardMPS || s.ToleranceM <= 0 || s.ToleranceM > .35 || s.StopSpeedMPS <= 0 || s.StopSpeedMPS > .1 || s.Fallback != mission.Stop {
		return errors.New("task differs from supported signed autonomous Go2 profile")
	}
	if len(s.RequiredSensors) != 2 {
		return errors.New("required odometry and obstacle sensors missing")
	}
	seen := map[string]bool{}
	for _, v := range s.RequiredSensors {
		seen[v] = true
	}
	if !seen["odometry"] || !seen["local-obstacles"] {
		return errors.New("unsupported required sensors")
	}
	for i := range s.Target {
		if !finite(s.Target[i]) || !finite(s.Area.Min[i]) || !finite(s.Area.Max[i]) || s.Area.Min[i] >= s.Area.Max[i] || s.Target[i] < s.Area.Min[i] || s.Target[i] > s.Area.Max[i] {
			return errors.New("invalid target or operating area")
		}
	}
	if s.Area.Max[0]-s.Area.Min[0] > 8 || s.Area.Max[2]-s.Area.Min[2] > 8 {
		return errors.New("planar operating area exceeds bound")
	}
	return nil
}

// odometryMinimum matches the bridge's separately rounded binary64 operations.
// Explicit conversions forbid fused multiply-add across the product rounding
// boundary (https://go.dev/ref/spec#Floating_point_operators). Keep this order;
// ordinary assignments alone do not require the intermediate rounding.
func odometryMinimum(position, yaw, allowance, age float64) (float64, float64) {
	basePosition := float64(position + allowance)
	positionDrift := float64(math.Hypot(.001, -.0005) * age)
	yawDrift := float64(.0002 * age)
	return float64(basePosition + positionDrift), float64(yaw + yawDrift)
}

func (c *Controller) validateState(s State) error {
	w, _, err := c.clock()
	if err != nil {
		return err
	}
	if !s.Synthetic || !s.Ready || s.AutoControl || s.Fault != "" || s.Session == "" || s.BridgeBootID == "" || len(s.Session) > 128 || len(s.BridgeBootID) > 128 || s.Session != c.session || s.BridgeBootID != c.boot || !same(s.Binding, c.cfg.Binding) || s.Sequence != c.sequence || s.Sequence > maxSequence || s.CaptureSequence == 0 || s.CaptureSequence < c.capture {
		return errors.New("adapter identity, ownership, sequence or readiness changed")
	}
	if c.bound && s.ExecutorEpoch != c.epoch {
		return errors.New("adapter executor generation changed")
	}
	for _, p := range []string{s.OdomPublisher, s.ScanPublisher, s.CommandPublisher} {
		if p == "" || len(p) > 128 {
			return errors.New("missing ROS publisher identity")
		}
	}
	if s.OdomPublisher != c.odomPublisher || s.ScanPublisher != c.scanPublisher || s.CommandPublisher != c.commandPublisher || s.Owner != s.CommandPublisher {
		return errors.New("ROS publishers or command ownership changed")
	}
	if s.SampleUnixMS <= 0 || s.SampleUnixMS > w || w-s.SampleUnixMS > c.profile.ObservationMaxAgeMS || s.Scan.CapturedUnixNS <= 0 {
		return errors.New("odometry is stale or future")
	}
	scanMS := s.Scan.CapturedUnixNS / int64(time.Millisecond)
	if scanMS > w || w-scanMS > c.profile.ObservationMaxAgeMS || absInt(scanMS-s.SampleUnixMS) > c.profile.SourceSkewMaxMS {
		return errors.New("scan freshness or odometry pairing invalid")
	}
	if s.OdomCapturedUnixNS <= 0 || min(s.OdomCapturedUnixNS, s.ScanCapturedUnixNS)/int64(time.Millisecond) != s.SampleUnixMS || s.ScanCapturedUnixNS != s.Scan.CapturedUnixNS {
		return errors.New("capture evidence differs from paired samples")
	}
	odomMS := s.OdomCapturedUnixNS / int64(time.Millisecond)
	if odomMS > w || absInt(odomMS-scanMS) > c.profile.SourceSkewMaxMS {
		return errors.New("odometry/scan capture skew invalid")
	}
	for i := range s.PoseCovariance {
		if !finite(s.PoseCovariance[i]) || !finite(s.TwistCovariance[i]) || (i%7 == 0 && (s.PoseCovariance[i] < 0 || s.TwistCovariance[i] < 0)) {
			return errors.New("invalid reported odometry covariance")
		}
	}
	if !finite(s.OdometryEpochAgeS) || s.OdometryEpochAgeS < 0 || s.OdometryEpochAgeS > c.profile.OdometryMaxEpochAgeS {
		return errors.New("odometry epoch age exceeds commissioned model")
	}
	minimumPosition, minimumYaw := odometryMinimum(c.alignment.PositionUncertaintyM, c.alignment.YawUncertaintyRad, c.profile.OdometryIntegrationAllowanceM, s.OdometryEpochAgeS)
	if s.Scan.PositionUncertaintyM < minimumPosition || s.Scan.YawUncertaintyRad < minimumYaw {
		return errors.New("odometry uncertainty omits commissioned drift budget")
	}
	if s.Scan.Sequence != s.CaptureSequence || s.Scan.WorldEpoch != c.cfg.Binding.WorldEpoch || s.Scan.Frame != "odom" || !same(s.Pose, s.Scan.Robot) || !s.Scan.UncertaintyKnown || s.Scan.PositionUncertaintyM < c.alignment.PositionUncertaintyM || s.Scan.YawUncertaintyRad < c.alignment.YawUncertaintyRad {
		return errors.New("scan lifecycle, pose or uncertainty mismatch")
	}
	if !finite(s.RollRad) || !finite(s.PitchRad) || math.Abs(s.RollRad) > c.profile.MaxAbsRollRad || math.Abs(s.PitchRad) > c.profile.MaxAbsPitchRad {
		return errors.New("scan plane exceeds commissioned tilt")
	}
	if c.profile.TiltedScan != nil {
		t := s.Scan.TiltedPose
		if t == nil || t.OdomCapturedUnixNS != s.OdomCapturedUnixNS || t.OdometryEpochAgeS != s.OdometryEpochAgeS || go2nav.ValidateTiltedPose(s.Scan, c.profile.navigation()) != nil {
			return errors.New("tilted scan pose, temporal or calibrated height evidence invalid")
		}
		r, p, _, e := go2nav.QuaternionRPY(t.QuaternionWXYZ)
		if e != nil || math.Abs(r-s.RollRad) > 1e-10 || math.Abs(p-s.PitchRad) > 1e-10 {
			return errors.New("tilt disagrees with original full orientation")
		}
	} else if s.Scan.TiltedPose != nil {
		return errors.New("legacy profile cannot accept tilted pose")
	}
	for _, v := range []float64{s.Pose.X, s.Pose.Y, s.Pose.Yaw, s.BodyVelocity[0], s.BodyVelocity[1], s.BodyVelocity[2], s.PreviousCommand[0], s.PreviousCommand[1], s.PreviousCommand[2]} {
		if !finite(v) {
			return errors.New("nonfinite robot state")
		}
	}
	if math.Abs(s.BodyVelocity[0]) > 1 || math.Abs(s.BodyVelocity[1]) > c.profile.measuredLateralLimit() || math.Abs(s.BodyVelocity[2]) > 1 || math.Abs(s.PreviousCommand[0]) > c.profile.MaxForwardMPS || s.PreviousCommand[1] != 0 || math.Abs(s.PreviousCommand[2]) > c.profile.MaxYawRateRPS {
		return errors.New("robot motion outside supported planar envelope")
	}
	// Bridge.state exposes its last accepted command, not measured velocity or
	// the receiver's target. Gait transitions require that exact local history.
	if c.profile.MotionEnvelope == go2nav.IsotropicGaitV1 && s.PreviousCommand != c.lastCommand {
		return errors.New("bridge command differs from acknowledged gait history")
	}
	if err := c.validateCloudLifecycle(s); err != nil {
		return err
	}
	if s.Cloud != nil && (c.cloudPrevious == nil || s.Cloud.Sequence != c.cloudPrevious.Sequence) {
		c.cloudPrevious = copyCloud(s.Cloud)
		c.cloudOdomCapturedUnixNS = s.OdomCapturedUnixNS
	}
	c.capture = s.CaptureSequence
	return nil
}

func cloudHex(s string, n int, nonzero bool) bool {
	if len(s) != n {
		return false
	}
	any := false
	for _, v := range s {
		if !(v >= '0' && v <= '9' || v >= 'a' && v <= 'f') {
			return false
		}
		any = any || v != '0'
	}
	return !nonzero || any
}

// validateCloudState validates the explicit wire association. It cannot verify
// a serialized digest against omitted raw CDR or authenticate a publisher.
func validateCloudState(s State) error {
	x := s.Cloud
	if x == nil || x.Model != go2nav.CloudModelV1 || x.SourceDigest != SupportedOdometrySourceDigest ||
		x.SourceDigest != s.Binding.SourceDigest || x.Frame != "utlidar_lidar" ||
		x.Sequence == 0 || x.Sequence > maxSequence || x.Sequence != s.CaptureSequence || x.Sequence != s.Scan.Sequence ||
		x.ObservedEpoch == 0 || x.ObservedEpoch != s.Binding.WorldEpoch || x.ObservedEpoch != s.Scan.WorldEpoch ||
		x.CapturedUnixNS <= 0 || x.CapturedUnixNS != s.ScanCapturedUnixNS || x.CapturedUnixNS != s.Scan.CapturedUnixNS ||
		len(x.Ranges) != go2nav.CloudRayCount || len(s.Scan.Ranges) != go2nav.CloudAzimuths ||
		x.MountPublisher != x.CloudMountPublisher {
		return errors.New("cloud source, shape or original association invalid")
	}
	for _, v := range []string{x.PublisherGID, x.EpochPublisher, x.MountPublisher, x.CloudMountPublisher} {
		if !cloudHex(v, 48, true) {
			return errors.New("cloud publisher identity invalid")
		}
	}
	for _, v := range []string{x.SerializedSHA256, x.ScanSHA256, x.OdomSHA256, x.EpochSHA256, x.MountSHA256, x.CloudMountSHA256} {
		if !cloudHex(v, 64, false) {
			return errors.New("cloud original sample digest invalid")
		}
	}
	for i, r := range x.Ranges {
		if !math.IsInf(r, 1) && (!finite(r) || r < .1-go2nav.CloudRangeQuantizationM || r > 12+go2nav.CloudRangeQuantizationM) {
			return errors.New("cloud range invalid")
		}
		if i/go2nav.CloudAzimuths == 12 {
			h := s.Scan.Ranges[i%go2nav.CloudAzimuths]
			if math.IsInf(r, 1) != math.IsInf(h, 1) || !math.IsInf(r, 1) &&
				(!finite(h) || math.Abs(r-h) > go2nav.CloudRangeQuantizationM+12.0/(1<<24)) {
				return errors.New("cloud horizontal companion differs")
			}
		}
	}
	return nil
}

func (c *Controller) validateCloudLifecycle(s State) error {
	if c.profile.CloudExperiment == nil {
		if s.Cloud != nil {
			return errors.New("legacy profile cannot accept cloud state")
		}
		return nil
	}
	if err := validateCloudState(s); err != nil {
		return err
	}
	x := s.Cloud
	for _, stamp := range []int64{x.CapturedUnixNS, s.ScanCapturedUnixNS, s.OdomCapturedUnixNS} {
		if stamp <= 0 || stamp > c.lastWallNS || c.lastWallNS-stamp >= c.profile.ObservationMaxAgeMS*int64(time.Millisecond) {
			return errors.New("original cloud tuple is stale or future")
		}
	}
	if previous := c.cloudPrevious; previous != nil {
		if x.PublisherGID != previous.PublisherGID || x.EpochPublisher != previous.EpochPublisher ||
			x.MountPublisher != previous.MountPublisher || x.CloudMountPublisher != previous.CloudMountPublisher {
			return errors.New("cloud tuple publisher changed")
		}
		if x.Sequence == previous.Sequence {
			if !reflect.DeepEqual(x, previous) || s.OdomCapturedUnixNS != c.cloudOdomCapturedUnixNS {
				return errors.New("served cloud tuple changed")
			}
		} else if x.Sequence < previous.Sequence || x.CapturedUnixNS <= previous.CapturedUnixNS || s.OdomCapturedUnixNS < c.cloudOdomCapturedUnixNS {
			return errors.New("cloud tuple sequence or original time regressed")
		}
	}
	return nil
}

func snapshotState(s State) State {
	s.Scan.Ranges = append([]float64(nil), s.Scan.Ranges...)
	if s.Scan.TiltedPose != nil {
		pose := *s.Scan.TiltedPose
		s.Scan.TiltedPose = &pose
	}
	s.Cloud = copyCloud(s.Cloud)
	return s
}

func (c *Controller) buildGrid(s State) (*go2nav.Grid, error) {
	if c.profile.CloudExperiment == nil {
		return go2nav.Build(s.Scan, c.bounds(), c.profile.navigation())
	}
	if s.Cloud == nil {
		return nil, errors.New("cloud experiment requires cloud observation")
	}
	grid, _, err := go2nav.BuildCloud(s.Cloud.observation(s.Scan), c.bounds(), c.profile.navigation())
	return grid, err
}
func absInt(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}
func (c *Controller) bounds() go2nav.Bounds {
	a := c.record.Spec.Area
	return go2nav.Bounds{Min: go2nav.Point{X: a.Min[0], Y: -a.Max[2]}, Max: go2nav.Point{X: a.Max[0], Y: -a.Min[2]}}
}
func (c *Controller) telemetry(s State, clear bool) tasking.Telemetry {
	sn, cs := math.Sincos(s.Pose.Yaw)
	return tasking.Telemetry{BootID: s.BridgeBootID, Session: s.Session, Frame: c.cfg.Binding.Frame, SampleUnixMS: s.SampleUnixMS, Position: [3]float64{s.Pose.X, 0, -s.Pose.Y}, Velocity: [3]float64{cs*s.BodyVelocity[0] - sn*s.BodyVelocity[1], 0, -sn*s.BodyVelocity[0] - cs*s.BodyVelocity[1]}, ObstacleClear: clear, Energy: 0, Sensors: map[string]bool{"odometry": clear, "local-obstacles": clear}}
}
func (c *Controller) Tick(ctx context.Context) (decision tasking.Decision, err error) {
	decision.Action = mission.Stop
	if !c.started || c.epoch == 0 {
		return decision, errors.New("controller not started")
	}
	if c.finished {
		decision.Record = copyRecord(c.record)
		return decision, nil
	}
	if c.fault != nil {
		return c.reportFault(ctx)
	}
	c.selectionDiagnostic = nil
	c.faultTiming = FaultTiming{ConfiguredBudgetMS: c.profile.DecisionMaxMS}
	c.timingStarted, c.timingInvalid = false, false
	defer func() {
		if err != nil {
			decision.Action = mission.Stop
			decision.Supervision = nil
			c.freezeFault(err)
			c.fault = err
			_ = c.brakeBounded()
			if c.record.Spec.ID != "" {
				reportCtx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
				_, _ = c.reportFault(reportCtx)
				cancel()
			}
		}
	}()
	if !c.bound {
		c.diagnosticStage = "task-binding"
		records, e := c.gateway.Tasks(ctx)
		if e != nil {
			return decision, e
		}
		found := false
		for _, r := range records {
			if r.Spec.ID == c.cfg.TaskID {
				if found {
					return decision, errors.New("duplicate task identity")
				}
				c.record = copyRecord(r)
				found = true
			}
		}
		if !found {
			return decision, nil
		}
		if e = c.validateSpec(c.record); e != nil {
			return decision, e
		}
		if c.record.Status.Phase != mission.Accepted {
			c.finished = true
			decision.Record = copyRecord(c.record)
			return decision, c.Brake(ctx)
		}
		b, _ := json.Marshal(c.cfg.Binding)
		if e = c.adapter.Begin(ctx, BeginRequest{Session: c.session, ExecutorEpoch: c.epoch, TaskID: c.record.Spec.ID, SpecDigest: tasking.SpecDigest(c.record.Spec), BindingSHA256: SHA256(b)}); e != nil {
			return decision, e
		}
		c.bound = true
	}
	c.diagnosticStage = "decision-clock"
	startWall, startMono, err := c.clock()
	if err != nil {
		return decision, err
	}
	c.decisionStartedUnixMS = startWall
	c.timingStarted = true
	c.timingStart = startMono
	c.timingLast = startMono
	c.diagnosticStage = "initial-state"
	s, err := c.adapter.State(ctx, c.session)
	if err != nil {
		return decision, err
	}
	s = snapshotState(s)
	c.observedState = s
	if err = c.validateState(s); err != nil {
		return decision, err
	}
	c.lastState = s
	c.diagnosticStage = "initial-grid"
	grid, err := c.buildGrid(s)
	if err != nil {
		return decision, err
	}
	if c.path == nil {
		c.diagnosticStage = "initial-plan"
		if math.Hypot(s.BodyVelocity[0], s.BodyVelocity[1]) > c.record.Spec.StopSpeedMPS || math.Abs(s.BodyVelocity[2]) > .05 || s.PreviousCommand != [3]float64{} {
			return decision, errors.New("initial route planning requires stopped robot")
		}
		c.path, err = grid.Plan(s.Pose, go2nav.Point{X: c.record.Spec.Target[0], Y: -c.record.Spec.Target[2]})
		if err != nil {
			return decision, err
		}
		c.waypoint = 1
	}
	c.observedState = s
	c.diagnosticStage = "pre-authority-freshness"
	if err = c.validateState(s); err != nil {
		return decision, err
	}
	c.diagnosticStage = "mission-authority"
	evaluateCtx, cancel := context.WithTimeout(ctx, 250*time.Millisecond)
	evaluateStarted := c.beginEvaluateTiming(evaluateCtx)
	decision, err = c.gateway.Evaluate(evaluateCtx, c.record.Spec.ID, c.epoch, c.telemetry(s, true))
	c.endEvaluateTiming(evaluateStarted)
	cancel()
	if err != nil {
		return decision, err
	}
	if !same(decision.Record.Spec, c.record.Spec) {
		return tasking.Decision{Action: mission.Stop}, errors.New("gateway returned another immutable task")
	}
	c.record = copyRecord(decision.Record)
	if decision.Action != mission.Continue {
		c.finished = true
		return decision, c.Brake(ctx)
	}
	if decision.Record.Status.Phase != mission.Running || decision.Supervision != nil {
		return tasking.Decision{Action: mission.Stop}, errors.New("unsupported authority decision")
	}
	c.diagnosticStage = "post-authority-state"
	s, err = c.adapter.State(ctx, c.session)
	if err != nil {
		return decision, err
	}
	s = snapshotState(s)
	c.observedState = s
	if err = c.validateState(s); err != nil {
		return decision, err
	}
	c.lastState = s
	c.diagnosticStage = "live-grid"
	grid, err = c.buildGrid(s)
	if err != nil {
		return decision, err
	}
	for c.waypoint < len(c.path) && math.Hypot(s.Pose.X-c.path[c.waypoint].X, s.Pose.Y-c.path[c.waypoint].Y) <= .04 {
		c.waypoint++
	}
	target := go2nav.Point{X: c.record.Spec.Target[0], Y: -c.record.Spec.Target[2]}
	if c.waypoint < len(c.path) {
		target = c.path[c.waypoint]
	}
	// Position alone does not complete the mission: the gateway also requires
	// measured speed <= StopSpeedMPS. Inside the signed final tolerance, request
	// zero so it can settle, rather than chasing the tighter path waypoint.
	// The current pose is only a zero-proposal input; the signed target, status
	// and retained prior motion remain unchanged and are checked below.
	if math.Hypot(s.Pose.X-c.record.Spec.Target[0], s.Pose.Y+c.record.Spec.Target[2]) <= c.record.Spec.ToleranceM {
		target = go2nav.Point{X: s.Pose.X, Y: s.Pose.Y}
	}
	limits := go2nav.MotionLimits{MaxForwardMPS: c.record.Spec.SpeedMPS, MaxYawRateRPS: c.profile.MaxYawRateRPS, HorizonS: c.profile.HorizonS, ExtraStopM: c.profile.ExtraStopM, ExtraStopRad: c.profile.ExtraStopRad, WaypointToleranceM: .04}
	gait := c.profile.MotionEnvelope == go2nav.IsotropicGaitV1
	if gait {
		limits.MaxMeasuredLateralMPS = c.profile.measuredLateralLimit()
	}
	c.diagnosticStage = "measured-motion"
	cmd, err := grid.CommandObserved(s.Pose, target, go2nav.BodyMotion{ForwardMPS: s.BodyVelocity[0], LateralMPS: s.BodyVelocity[1], YawRateRPS: s.BodyVelocity[2]}, limits)
	if err != nil {
		return decision, err
	}
	priorTargets := []go2nav.Command{{ForwardMPS: s.PreviousCommand[0], YawRateRPS: s.PreviousCommand[2]}, {ForwardMPS: c.lastCommand[0], YawRateRPS: c.lastCommand[2]}}
	if gait {
		cmd = c.gaitProposal(s, cmd)
		priorTargets, err = c.gaitPriors(s)
		if err != nil {
			return decision, err
		}
	} else {
		// Previously dispatched motion may exceed a delayed measured velocity. It
		// must independently fit the same observed stopping corridor.
		for i, prior := range [][3]float64{s.PreviousCommand, c.lastCommand} {
			c.diagnosticStage = []string{"bridge-prior-motion", "controller-prior-motion"}[i]
			if _, err = grid.Command(s.Pose, target, prior[0], prior[2], limits); err != nil {
				return decision, err
			}
		}
		// A new translational command must not combine with a still-active turn,
		// or vice versa. First publish zero, then observe settled measured motion.
		// Checking two separate straight/pivot corridors is not a curved sweep.
		if (cmd.ForwardMPS != 0 && (math.Abs(s.BodyVelocity[2]) > .01 || s.PreviousCommand[2] != 0 || c.lastCommand[2] != 0)) ||
			(cmd.YawRateRPS != 0 && (math.Abs(s.BodyVelocity[0]) > .01 || s.PreviousCommand[0] != 0 || c.lastCommand[0] != 0)) {
			cmd = go2nav.Command{}
		}
	}
	c.diagnosticStage = "joint-continuation"
	if cmd, err = c.selectCandidate(grid, s, priorTargets, cmd, limits); err != nil {
		return decision, err
	}
	c.observedState = s
	c.diagnosticStage = "final-freshness"
	if err = c.validateState(s); err != nil {
		return decision, err
	}
	c.diagnosticStage = "final-deadline"
	w, m, err := c.clock()
	if err != nil {
		return decision, err
	}
	remaining := time.Duration(c.profile.DecisionMaxMS)*time.Millisecond - (m - startMono)
	notAfter := min(startWall+c.profile.DecisionMaxMS, c.record.Spec.NotAfterUnixMS, c.record.AuthorizationUntilUnixMS)
	// The bridge conservatively timestamps DDS publication before this fixed
	// deadline by ExpiryLeadMS. A stale source timestamp causes receiver refusal.
	remaining = min(remaining, time.Duration(notAfter-w)*time.Millisecond)
	if remaining < time.Millisecond || notAfter <= w || s.CaptureSequence < c.usedCapture {
		return decision, errors.New("fixed decision deadline or capture sequence regressed")
	}
	if s.CaptureSequence == c.usedCapture {
		// Sensor and controller periods can coincide. The original observation
		// was checked fresh, including the current mission/stop decision. A
		// skipped dispatch neither consumes it again nor renews the prior DDS
		// command or its fixed deadline. Staleness still latches on later ticks.
		return decision, nil
	}
	if c.sequence >= maxSequence {
		return decision, errors.New("command sequence exhausted")
	}
	c.sequence++
	request := CommandRequest{Session: c.session, Sequence: c.sequence, ExecutorEpoch: c.epoch, TaskID: c.record.Spec.ID, SpecDigest: tasking.SpecDigest(c.record.Spec), CaptureSequence: s.CaptureSequence, BodyVX: cmd.ForwardMPS, YawRate: cmd.YawRateRPS, NotAfterUnixMS: notAfter, DecisionRemainingMS: remaining.Milliseconds()}
	c.usedCapture = s.CaptureSequence // Uncertain dispatch is never retried.
	if err = ctx.Err(); err != nil {
		return decision, err
	}
	c.diagnosticStage = "dispatch"
	c.lastAttempt = copyCommand(&request)
	err = c.adapter.Command(ctx, request)
	c.recordCommandDiagnostic(request, err)
	if err != nil {
		return decision, err
	}
	c.lastAcknowledged = copyCommand(&request)
	c.lastCommand = [3]float64{cmd.ForwardMPS, 0, cmd.YawRateRPS}
	if gait {
		c.diagnosticStage = "gait-acknowledgment"
		if err = c.acknowledgeGaitCommand(request); err != nil {
			return decision, err
		}
	}
	return decision, nil
}
func (c *Controller) Brake(ctx context.Context) error {
	if c.session == "" {
		return nil
	}
	if c.sequence >= maxSequence {
		return errors.New("stop sequence exhausted")
	}
	c.sequence++
	return c.adapter.Stop(ctx, StopRequest{Session: c.session, Sequence: c.sequence})
}
func (c *Controller) brakeBounded() error {
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	return c.Brake(ctx)
}
func (c *Controller) reportFault(ctx context.Context) (tasking.Decision, error) {
	d := tasking.Decision{Action: mission.Stop}
	if c.record.Spec.ID == "" {
		return d, c.fault
	}
	telemetry := c.telemetry(c.lastState, false)
	telemetry.LocalFault = true
	evaluateCtx, cancel := context.WithTimeout(ctx, 250*time.Millisecond)
	defer cancel()
	r, err := c.gateway.Evaluate(evaluateCtx, c.record.Spec.ID, c.epoch, telemetry)
	if err == nil && same(r.Record.Spec, c.record.Spec) && r.Action != mission.Continue {
		d = r
		c.record = copyRecord(r.Record)
		if r.Record.Status.Phase.Terminal() || r.Record.Status.Phase == mission.Held {
			c.finished = true
		}
	}
	return d, errors.Join(c.fault, err)
}
func (c *Controller) Run(ctx context.Context, report func(tasking.Decision, error)) error {
	start, cancel := context.WithTimeout(ctx, 5*time.Second)
	err := c.Start(start)
	cancel()
	if err != nil {
		return err
	}
	defer c.brakeBounded()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			tick, cancel := context.WithTimeout(ctx, time.Duration(c.profile.DecisionMaxMS)*time.Millisecond)
			d, err := c.Tick(tick)
			cancel()
			if report != nil {
				report(d, err)
			}
			if err != nil && c.fault == nil {
				return fmt.Errorf("controller failed: %w", err)
			}
		}
	}
}

// Observation returns the latest consumed adapter state without I/O. Like Tick,
// it is called only by the controller owner. It is an unsigned diagnostic and
// grants no authority or freshness beyond the captured metadata.
func (c *Controller) Observation() (State, bool) {
	if c.observedState.CaptureSequence == 0 {
		return State{}, false
	}
	s, _ := diagnosticState(c.observedState)
	return s, true
}
