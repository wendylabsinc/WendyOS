package go2executor

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/go2nav"
	"github.com/wendylabsinc/wendy/go/world/internal/mission"
	"github.com/wendylabsinc/wendy/go/world/internal/tasking"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

type testClock struct {
	wall time.Time
	mono time.Duration
}

func (t *testClock) advance(d time.Duration) { t.wall = t.wall.Add(d); t.mono += d }

type fakeGateway struct {
	record              tasking.Record
	epoch               uint64
	begins, evaluations int
	hook                func(tasking.Telemetry)
	changed             bool
	beginErr            error
	stop                bool
}

func (g *fakeGateway) Begin(context.Context) (uint64, error) {
	g.begins++
	g.epoch++
	return g.epoch, g.beginErr
}

func TestGenerationRequiresSuccessfulDurableBegin(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "idle-ready", true: "failed-begin"}[fail], func(t *testing.T) {
			c, g, a, _ := fixture(t)
			g.record = tasking.Record{}
			if c.Generation() != 0 {
				t.Fatal("generation existed before startup")
			}
			if fail {
				g.beginErr = errors.New("uncertain durable acknowledgment")
			}
			err := c.Start(context.Background())
			if fail {
				if err == nil || c.Generation() != 0 || a.stops == 0 {
					t.Fatalf("failed Start exposed readiness: generation=%d err=%v", c.Generation(), err)
				}
			} else {
				if err != nil || c.Generation() != 1 {
					t.Fatalf("successful Begin not exposed: %d %v", c.Generation(), err)
				}
				if d, e := c.Tick(context.Background()); e != nil || d.Action != mission.Stop || len(a.commands) != 0 || c.Generation() != 1 {
					t.Fatal("idle readiness changed authority or moved")
				}
			}
		})
	}
}
func (g *fakeGateway) Tasks(context.Context) ([]tasking.Record, error) {
	return []tasking.Record{copyRecord(g.record)}, nil
}
func (g *fakeGateway) Evaluate(_ context.Context, _ string, _ uint64, t tasking.Telemetry) (tasking.Decision, error) {
	g.evaluations++
	if g.hook != nil {
		g.hook(t)
	}
	d := tasking.Decision{Action: mission.Continue, Record: copyRecord(g.record)}
	d.Record.Status.Phase = mission.Running
	if !t.ObstacleClear {
		d.Action = mission.Stop
		d.Record.Status.Phase = mission.Failed
	}
	if g.stop {
		d.Action = mission.Stop
		d.Record.Status.Phase = mission.Canceled
	}
	if g.changed {
		d.Record.Spec.Target[0] += .1
	}
	g.record = copyRecord(d.Record)
	return d, nil
}

type fakeAdapter struct {
	clock        *testClock
	state        State
	commands     []CommandRequest
	stops        int
	sessionCalls int
	commandErr   error
	stateHook    func(*State)
	beginErr     error
}

func (a *fakeAdapter) Session(context.Context, tasking.Go2Binding) (State, error) {
	a.sessionCalls++
	a.fresh()
	return a.state, nil
}
func (a *fakeAdapter) Begin(_ context.Context, r BeginRequest) error {
	a.state.ExecutorEpoch = r.ExecutorEpoch
	return a.beginErr
}
func (a *fakeAdapter) fresh() {
	a.state.SampleUnixMS = a.clock.wall.UnixMilli()
	a.state.OdomCapturedUnixNS = a.clock.wall.UnixNano()
	a.state.ScanCapturedUnixNS = a.clock.wall.UnixNano()
	a.state.CaptureSequence++
	a.state.Scan.Sequence = a.state.CaptureSequence
	a.state.Scan.CapturedUnixNS = a.clock.wall.UnixNano()
	a.state.Scan.Robot = a.state.Pose
}
func (a *fakeAdapter) State(context.Context, string) (State, error) {
	a.fresh()
	s := a.state
	s.Scan.Ranges = append([]float64(nil), s.Scan.Ranges...)
	if a.stateHook != nil {
		a.stateHook(&s)
	}
	return s, nil
}
func (a *fakeAdapter) Command(_ context.Context, r CommandRequest) error {
	a.commands = append(a.commands, r)
	a.state.Sequence = r.Sequence
	a.state.PreviousCommand = [3]float64{r.BodyVX, r.BodyVY, r.YawRate}
	return a.commandErr
}
func (a *fakeAdapter) Stop(_ context.Context, r StopRequest) error {
	a.stops++
	a.state.Sequence = r.Sequence
	return nil
}
func fixture(t *testing.T) (*Controller, *fakeGateway, *fakeAdapter, *testClock) {
	t.Helper()
	clock := &testClock{wall: time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)}
	f := world.FrameRef{ID: "imported-floor", Revision: "frame-v1"}
	alignment := Alignment{Schema: "wendy.world.go2-alignment.v1", SourceFrame: "odom", TargetFrame: f, ReferencePoint: "base_footprint", Synthetic: true, PositionUncertaintyM: .02, YawUncertaintyRad: .02}
	profile := Profile{MotionEnvelope: "isotropic-v1", OdometryMaxEpochAgeS: 120, OdometryIntegrationAllowanceM: .05, ClockErrorMS: 50, MaxAbsRollRad: .05, MaxAbsPitchRad: .05, VerticalObstacles: true, Schema: "wendy.world.go2-profile.v1", FlatFloor: true, HalfLengthM: .425, HalfWidthM: .3, ClearanceM: .05, MaxPositionUncertaintyM: .1, MaxYawUncertaintyRad: .1, MaxForwardMPS: .35, MaxYawRateRPS: .5, ObservationMaxAgeMS: 250, SourceSkewMaxMS: 50, DecisionMaxMS: 500, ExpiryLeadMS: 450, HorizonS: 1, ExtraStopM: .1, ExtraStopRad: .1}
	ab, _ := json.Marshal(alignment)
	pb, _ := json.Marshal(profile)
	b := tasking.Go2Binding{Schema: "wendy.world.go2-execution.v1", PackageSHA256: strings.Repeat("a", 64), ConversionSHA256: strings.Repeat("b", 64), LossSHA256: strings.Repeat("c", 64), ProjectRevision: strings.Repeat("d", 64), ProjectID: strings.Repeat("e", 32), VMName: "fixture", SourceDigest: SupportedOdometrySourceDigest, PolicyBundle: "go2-moe-cts-164k-0.6715-v1", WorldEpoch: 2, RunID: "fixture-run", Frame: f, AlignmentSHA256: SHA256(ab), ProfileSHA256: SHA256(pb)}
	spec := tasking.Spec{ID: "test-task", Type: tasking.Go2GoToType, Site: "fixture-site", Assignee: "fixture-robot", Domain: mission.Simulation, Class: mission.Move, Mode: mission.Autonomous, Frame: f, Go2: &b, NotAfterUnixMS: clock.wall.Add(time.Minute).UnixMilli(), Area: tasking.Bounds{Min: [3]float64{-2, -.1, -2}, Max: [3]float64{2, .1, 2}}, RequiredSensors: []string{"odometry", "local-obstacles"}, Fallback: mission.Stop, Target: [3]float64{1, 0, 1}, SpeedMPS: .2, ToleranceM: .1, StopSpeedMPS: .05}
	g := &fakeGateway{record: tasking.Record{Spec: spec, Status: mission.Status{Phase: mission.Accepted, Version: 1}, AuthorizationUntilUnixMS: spec.NotAfterUnixMS}}
	ranges := make([]float64, 360)
	for i := range ranges {
		ranges[i] = 4
	}
	a := &fakeAdapter{clock: clock, state: State{BridgeBootID: "bridge-boot", Session: "session", Binding: b, OdomPublisher: "odom", ScanPublisher: "scan", CommandPublisher: "command", Owner: "command", Synthetic: true, Ready: true, Scan: go2nav.Scan{WorldEpoch: 2, Frame: "odom", Ranges: ranges, AngleMin: -math.Pi, AngleIncrement: math.Pi / 180, RangeMin: .1, RangeMax: 12, SensorMount: go2nav.Pose{X: .28}, MountKnown: true, PositionUncertaintyM: .07, YawUncertaintyRad: .02, UncertaintyKnown: true}}}
	c, err := New(g, a, Config{TaskID: spec.ID, Site: spec.Site, Binding: b, AlignmentJSON: ab, ProfileJSON: pb, Now: func() time.Time { return clock.wall }, Monotonic: func() time.Duration { return clock.mono }})
	if err != nil {
		t.Fatal(err)
	}
	return c, g, a, clock
}
func TestPlanarTaskUsesFreshSignedDecision(t *testing.T) {
	c, g, a, _ := fixture(t)
	if err := c.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	d, err := c.Tick(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if d.Action != mission.Continue || len(a.commands) != 1 || a.commands[0].YawRate >= 0 || a.commands[0].BodyVY != 0 || g.begins != 1 {
		t.Fatalf("unexpected two-axis route decision: %+v commands=%+v", d, a.commands)
	}
	r := a.commands[0]
	if r.SpecDigest != tasking.SpecDigest(g.record.Spec) || r.ExecutorEpoch != 1 || r.CaptureSequence == 0 || r.DecisionRemainingMS > 500 {
		t.Fatal("command lost exact authority or capture binding")
	}
}

func TestInitialIdleWaitDoesNotRearmOrInventTask(t *testing.T) {
	c, g, a, _ := fixture(t)
	saved := g.record
	g.record = tasking.Record{}
	if err := c.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		d, e := c.Tick(context.Background())
		if e != nil || d.Action != mission.Stop {
			t.Fatalf("idle %v %v", d, e)
		}
	}
	if len(a.commands) != 0 || a.sessionCalls != 1 || g.begins != 1 || c.bound {
		t.Fatal("idle renewed authority or moved")
	}
	g.record = saved
	if _, err := c.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(a.commands) != 1 || a.sessionCalls != 1 || g.begins != 1 {
		t.Fatal("newly delivered task did not use original executor generation")
	}
}

func TestTurnToTranslationPublishesZeroBeforeChangingMotion(t *testing.T) {
	c, _, a, _ := fixture(t)
	if e := c.Start(context.Background()); e != nil {
		t.Fatal(e)
	}
	if _, e := c.Tick(context.Background()); e != nil {
		t.Fatal(e)
	}
	if a.commands[0].YawRate == 0 {
		t.Fatal("fixture did not turn")
	}
	a.state.Pose.Yaw = -math.Pi / 4
	if _, e := c.Tick(context.Background()); e != nil {
		t.Fatal(e)
	}
	if r := a.commands[1]; r.BodyVX != 0 || r.YawRate != 0 {
		t.Fatal("turn changed directly into translation")
	}
	if _, e := c.Tick(context.Background()); e != nil {
		t.Fatal(e)
	}
	if a.commands[2].BodyVX <= 0 || a.commands[2].YawRate != 0 {
		t.Fatal("settled zero did not allow observed straight movement")
	}
}

func TestSameFreshCaptureSkipsWithoutRenewingCommand(t *testing.T) {
	for _, next := range []string{"fresh", "stale", "lost-sensor", "lower", "stop"} {
		t.Run(next, func(t *testing.T) {
			c, g, a, clock := fixture(t)
			if e := c.Start(context.Background()); e != nil {
				t.Fatal(e)
			}
			if _, e := c.Tick(context.Background()); e != nil {
				t.Fatal(e)
			}
			original := a.commands[0]
			old := a.state
			evals := g.evaluations
			a.stateHook = func(s *State) {
				s.CaptureSequence = old.CaptureSequence
				s.Scan.Sequence = old.CaptureSequence
				s.SampleUnixMS = old.SampleUnixMS
				s.OdomCapturedUnixNS = old.OdomCapturedUnixNS
				s.ScanCapturedUnixNS = old.ScanCapturedUnixNS
				s.Scan.CapturedUnixNS = old.Scan.CapturedUnixNS
			}
			clock.advance(50 * time.Millisecond)
			if _, e := c.Tick(context.Background()); e != nil {
				t.Fatal(e)
			}
			if len(a.commands) != 1 || a.commands[0] != original || g.evaluations <= evals || c.fault != nil {
				t.Fatal("same fresh capture renewed command or skipped mission validation")
			}
			switch next {
			case "fresh":
				a.stateHook = nil
				clock.advance(50 * time.Millisecond)
			case "stale":
				clock.advance(201 * time.Millisecond)
			case "lost-sensor":
				oldHook := a.stateHook
				a.stateHook = func(s *State) { oldHook(s); s.Ready = false }
			case "lower":
				old.CaptureSequence--
			case "stop":
				g.stop = true
			}
			_, e := c.Tick(context.Background())
			if next == "stop" {
				if e != nil || len(a.commands) != 1 || a.stops == 0 || g.record.Status.Phase != mission.Canceled {
					t.Fatalf("fresh duplicate capture masked stop: %v", e)
				}
			} else if next == "fresh" {
				if e != nil || len(a.commands) != 2 || a.commands[1].CaptureSequence <= original.CaptureSequence {
					t.Fatalf("new capture failed to progress: %v", e)
				}
			} else {
				if e == nil || len(a.commands) != 1 || a.stops == 0 || g.record.Status.Phase != mission.Failed {
					t.Fatalf("bad capture did not latch: %v", e)
				}
			}
		})
	}
}
func TestFailureLatchesAndNeverRetriesPositiveDispatch(t *testing.T) {
	c, g, a, _ := fixture(t)
	if err := c.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	a.commandErr = errors.New("uncertain DDS dispatch")
	if _, err := c.Tick(context.Background()); err == nil {
		t.Fatal("uncertain dispatch accepted")
	}
	a.commandErr = nil
	_, _ = c.Tick(context.Background())
	if len(a.commands) != 1 || a.stops == 0 || a.sessionCalls != 1 || g.record.Status.Phase != mission.Failed {
		t.Fatalf("failure resumed or was not recorded: commands=%d stops=%d phase=%s", len(a.commands), a.stops, g.record.Status.Phase)
	}
	if err := c.Start(context.Background()); err == nil {
		t.Fatal("controller rearmed")
	}
}
func TestDelayedEvaluationConsumesFixedBudget(t *testing.T) {
	c, g, a, clock := fixture(t)
	if err := c.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	g.hook = func(tm tasking.Telemetry) {
		if tm.ObstacleClear {
			clock.advance(501 * time.Millisecond)
		}
	}
	if _, err := c.Tick(context.Background()); err == nil {
		t.Fatal("expired decision moved")
	}
	if len(a.commands) != 0 || g.record.Status.Phase != mission.Failed {
		t.Fatal("deadline failure did not latch")
	}
}
func TestFreshStateAfterEvaluationAndExactTask(t *testing.T) {
	for _, mode := range []string{"changed-spec", "stale-state", "changed-epoch", "changed-publisher", "auto-control", "unknown-ray", "tilt"} {
		t.Run(mode, func(t *testing.T) {
			c, g, a, _ := fixture(t)
			if err := c.Start(context.Background()); err != nil {
				t.Fatal(err)
			}
			g.hook = func(tm tasking.Telemetry) {
				if !tm.ObstacleClear {
					return
				}
				if mode == "changed-spec" {
					g.changed = true
				} else {
					a.stateHook = func(s *State) {
						switch mode {
						case "stale-state":
							s.SampleUnixMS -= 251
						case "changed-epoch":
							s.Binding.WorldEpoch++
						case "changed-publisher":
							s.ScanPublisher = "replacement"
						case "auto-control":
							s.AutoControl = true
						case "tilt":
							s.RollRad = .051
						case "unknown-ray":
							for i := range s.Scan.Ranges {
								s.Scan.Ranges[i] = math.Inf(1)
							}
						}
					}
				}
			}
			if _, err := c.Tick(context.Background()); err == nil {
				t.Fatal("invalid post-authority state moved")
			}
			if len(a.commands) != 0 {
				t.Fatal("positive command emitted")
			}
		})
	}
}
func TestUnsupportedMissionNeverBeginsMotion(t *testing.T) {
	for _, mode := range []string{"supervised", "physical", "height", "energy", "binding", "zero-speed"} {
		t.Run(mode, func(t *testing.T) {
			c, g, a, _ := fixture(t)
			switch mode {
			case "supervised":
				g.record.Spec.Mode = mission.Supervised
			case "physical":
				g.record.Spec.Domain = mission.Physical
			case "height":
				g.record.Spec.Target[1] = .05
			case "energy":
				g.record.Spec.MinimumEnergy = .1
			case "binding":
				g.record.Spec.Go2.PackageSHA256 = strings.Repeat("9", 64)
			case "zero-speed":
				g.record.Spec.SpeedMPS = 0
			}
			if err := c.Start(context.Background()); err != nil {
				t.Fatal(err)
			}
			if _, err := c.Tick(context.Background()); err == nil {
				t.Fatal("unsupported task accepted")
			}
			if len(a.commands) != 0 || a.stops == 0 {
				t.Fatal("unsupported task did not brake")
			}
		})
	}
}
func TestConfigurationIsExactAndNoSilentMissingDefaults(t *testing.T) {
	c, _, _, _ := fixture(t)
	for _, mode := range []string{"modified-hash", "missing-flat-floor", "short-translation", "null-coordinate", "small-footprint"} {
		t.Run(mode, func(t *testing.T) {
			cfg := c.cfg
			switch mode {
			case "modified-hash":
				cfg.ProfileJSON = append([]byte(" "), cfg.ProfileJSON...)
			case "missing-flat-floor":
				cfg.ProfileJSON = bytesWithout(t, cfg.ProfileJSON, "flat_floor")
				cfg.Binding.ProfileSHA256 = SHA256(cfg.ProfileJSON)
			case "short-translation", "null-coordinate":
				var m map[string]any
				_ = json.Unmarshal(cfg.AlignmentJSON, &m)
				if mode == "short-translation" {
					m["translation_xy_m"] = []float64{0}
				} else {
					m["translation_xy_m"] = []any{nil, 0}
				}
				cfg.AlignmentJSON, _ = json.Marshal(m)
				cfg.Binding.AlignmentSHA256 = SHA256(cfg.AlignmentJSON)
			case "small-footprint":
				var m map[string]any
				_ = json.Unmarshal(cfg.ProfileJSON, &m)
				m["half_width_m"] = .155
				cfg.ProfileJSON, _ = json.Marshal(m)
				cfg.Binding.ProfileSHA256 = SHA256(cfg.ProfileJSON)
			}
			if _, _, err := DecodeConfiguration(cfg); err == nil {
				t.Fatal("invalid configuration accepted")
			}
		})
	}
}
func bytesWithout(t *testing.T, b []byte, k string) []byte {
	t.Helper()
	var m map[string]json.RawMessage
	if e := json.Unmarshal(b, &m); e != nil {
		t.Fatal(e)
	}
	delete(m, k)
	out, _ := json.Marshal(m)
	return out
}

func TestMeasuredMixedDriftStopsProposalWithoutLatching(t *testing.T) {
	c, g, a, _ := fixture(t)
	if e := c.Start(context.Background()); e != nil {
		t.Fatal(e)
	}
	if _, e := c.Tick(context.Background()); e != nil {
		t.Fatal(e)
	}
	a.state.BodyVelocity = [3]float64{.052, -.015, -.112}
	if _, e := c.Tick(context.Background()); e != nil {
		t.Fatal(e)
	}
	last := a.commands[len(a.commands)-1]
	if last.BodyVX != 0 || last.BodyVY != 0 || last.YawRate != 0 || a.stops != 0 || g.record.Status.Phase != mission.Running {
		t.Fatal("clear mixed drift did not submit fixed-deadline zero")
	}
	a.state.BodyVelocity = [3]float64{}
	if _, e := c.Tick(context.Background()); e != nil {
		t.Fatal(e)
	}
	if a.commands[len(a.commands)-1].YawRate == 0 {
		t.Fatal("settled fresh capture cannot progress")
	}
}

func TestOdometryCommissioningAndRuntimeBounds(t *testing.T) {
	for _, name := range []string{"missing-age", "zero-age", "large-age", "allowance", "source"} {
		t.Run(name, func(t *testing.T) {
			c, _, _, _ := fixture(t)
			cfg := c.cfg
			var m map[string]any
			_ = json.Unmarshal(cfg.ProfileJSON, &m)
			switch name {
			case "missing-age":
				delete(m, "odometry_max_epoch_age_s")
			case "zero-age":
				m["odometry_max_epoch_age_s"] = 0
			case "large-age":
				m["odometry_max_epoch_age_s"] = 121
			case "allowance":
				m["odometry_integration_allowance_m"] = .049
			case "source":
				cfg.Binding.SourceDigest = "sha256:" + strings.Repeat("f", 64)
			}
			cfg.ProfileJSON, _ = json.Marshal(m)
			cfg.Binding.ProfileSHA256 = SHA256(cfg.ProfileJSON)
			if _, _, e := DecodeConfiguration(cfg); e == nil {
				t.Fatal("unsupported odometry commissioning accepted")
			}
		})
	}
	for _, name := range []string{"age", "position", "yaw"} {
		t.Run("runtime-"+name, func(t *testing.T) {
			c, _, a, _ := fixture(t)
			if e := c.Start(context.Background()); e != nil {
				t.Fatal(e)
			}
			a.stateHook = func(s *State) {
				s.OdometryEpochAgeS = 10
				s.Scan.PositionUncertaintyM = .07 + math.Hypot(.001, -.0005)*10
				s.Scan.YawUncertaintyRad = .022
				switch name {
				case "age":
					s.OdometryEpochAgeS = 121
				case "position":
					s.Scan.PositionUncertaintyM = .07
				case "yaw":
					s.Scan.YawUncertaintyRad = .02
				}
			}
			if _, e := c.Tick(context.Background()); e == nil || a.stops == 0 || len(a.commands) != 0 {
				t.Fatal("invalid epoch drift admitted")
			}
		})
	}
}

func TestObservationCopiesLatestConsumedCapture(t *testing.T) {
	c, _, a, _ := fixture(t)
	if _, ok := c.Observation(); ok {
		t.Fatal("observation invented before startup")
	}
	if e := c.Start(context.Background()); e != nil {
		t.Fatal(e)
	}
	if _, e := c.Tick(context.Background()); e != nil {
		t.Fatal(e)
	}
	s, ok := c.Observation()
	if !ok || s.CaptureSequence == 0 || s.Scan.Sequence != s.CaptureSequence {
		t.Fatal("latest consumed capture absent")
	}
	commands := len(a.commands)
	original := s.Scan.Ranges[0]
	s.Scan.Ranges[0] = -1
	again, _ := c.Observation()
	if again.Scan.Ranges[0] != original || len(a.commands) != commands {
		t.Fatal("observation mutated controller or did I/O")
	}
}

func TestMotionEnvelopeProfileIsExplicitAndBound(t *testing.T) {
	for _, model := range []string{"missing", "", "unknown", "isotropic-v1", "directional-v1"} {
		t.Run(model, func(t *testing.T) {
			c, g, a, clock := fixture(t)
			cfg := c.cfg
			var m map[string]any
			_ = json.Unmarshal(cfg.ProfileJSON, &m)
			if model == "missing" {
				delete(m, "motion_envelope")
			} else {
				m["motion_envelope"] = model
			}
			cfg.ProfileJSON, _ = json.Marshal(m)
			if _, _, e := DecodeConfiguration(cfg); e == nil {
				t.Fatal("changed profile reused original binding")
			}
			cfg.Binding.ProfileSHA256 = SHA256(cfg.ProfileJSON)
			_, p, e := DecodeConfiguration(cfg)
			if model != "isotropic-v1" && model != "directional-v1" {
				if e == nil {
					t.Fatal("missing/unknown model implicitly migrated")
				}
				return
			}
			if e != nil || p.MotionEnvelope != model || p.navigation().MotionEnvelope != model {
				t.Fatalf("model lost: %+v %v", p, e)
			}
			a.state.Binding = cfg.Binding
			g.record.Spec.Go2 = &cfg.Binding
			fresh, e := New(g, a, cfg)
			if e != nil {
				t.Fatal(e)
			}
			if e = fresh.Start(context.Background()); e != nil {
				t.Fatal(e)
			}
			if _, e = fresh.Tick(context.Background()); e != nil {
				t.Fatal(e)
			}
			if len(a.commands) != 1 || a.commands[0].NotAfterUnixMS > clock.wall.UnixMilli()+500 {
				t.Fatal("model changed dispatch/deadline contract")
			}
		})
	}
}
