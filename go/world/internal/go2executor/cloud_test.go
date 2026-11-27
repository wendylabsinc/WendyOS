package go2executor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/go2nav"
	"github.com/wendylabsinc/wendy/go/world/internal/mission"
	"github.com/wendylabsinc/wendy/go/world/internal/tasking"
)

// Synthetic fixtures exercise selection and expiry, not clock calibration or
// physical clearance. Original hashes here are labels, not captured CDR proof.
type cloudAdapter struct {
	*gaitAdapter
	cloudHook func(*State)
	states    int
}

func cloudForState(s State) *CloudState {
	ranges := make([]float64, go2nav.CloudRayCount)
	for i := range ranges {
		ranges[i] = 4
	}
	copy(ranges[4320:4680], s.Scan.Ranges)
	return &CloudState{Model: go2nav.CloudModelV1, SourceDigest: SupportedOdometrySourceDigest,
		Frame: "utlidar_lidar", PublisherGID: strings.Repeat("01", 24),
		SerializedSHA256: SHA256([]byte(fmt.Sprint(s.ScanCapturedUnixNS))),
		CapturedUnixNS:   s.ScanCapturedUnixNS, Ranges: ranges, Sequence: s.CaptureSequence,
		ObservedEpoch: s.Scan.WorldEpoch, ScanSHA256: strings.Repeat("a", 64),
		OdomSHA256: strings.Repeat("b", 64), EpochSHA256: strings.Repeat("c", 64),
		MountSHA256: strings.Repeat("d", 64), CloudMountSHA256: strings.Repeat("d", 64),
		EpochPublisher: strings.Repeat("02", 24), MountPublisher: strings.Repeat("03", 24),
		CloudMountPublisher: strings.Repeat("03", 24)}
}

func (a *cloudAdapter) Session(ctx context.Context, binding tasking.Go2Binding) (State, error) {
	a.clock.advance(time.Millisecond)
	s, e := a.gaitAdapter.Session(ctx, binding)
	s.Cloud = cloudForState(s)
	return s, e
}
func (a *cloudAdapter) State(ctx context.Context, session string) (State, error) {
	a.clock.advance(time.Millisecond)
	s, e := a.gaitAdapter.State(ctx, session)
	s.Cloud = cloudForState(s)
	a.states++
	if a.cloudHook != nil {
		a.cloudHook(&s)
	}
	return s, e
}

func cloudController(t *testing.T) (*Controller, *fakeGateway, *cloudAdapter, *testClock) {
	t.Helper()
	base, g, ga, clock := gaitController(t)
	p := base.profile
	p.CloudExperiment = &CloudExperiment{Model: go2nav.CloudModelV1, Scope: "contained-simulation", CollectorDurationMS: 60000}
	cfg := base.cfg
	cfg.ProfileJSON, _ = json.Marshal(p)
	cfg.Binding.ProfileSHA256 = SHA256(cfg.ProfileJSON)
	ga.state.Binding = cfg.Binding
	g.record.Spec.Go2 = &cfg.Binding
	a := &cloudAdapter{gaitAdapter: ga}
	c, e := New(g, a, cfg)
	if e != nil {
		t.Fatal(e)
	}
	if e = c.Start(context.Background()); e != nil {
		t.Fatal(e)
	}
	return c, g, a, clock
}

func cloudStateJSON(t *testing.T, s State) []byte {
	t.Helper()
	x := s.Cloud
	s.Cloud = nil
	var fields map[string]json.RawMessage
	if e := json.Unmarshal(stateJSON(t, s), &fields); e != nil {
		t.Fatal(e)
	}
	if x != nil {
		ranges := make([]*float64, len(x.Ranges))
		for i, r := range x.Ranges {
			if finite(r) {
				r := r
				ranges[i] = &r
			}
		}
		wire := wireCloud{Model: x.Model, SourceDigest: x.SourceDigest, Frame: x.Frame,
			PublisherGID: x.PublisherGID, SerializedSHA256: x.SerializedSHA256,
			CapturedUnixNS: x.CapturedUnixNS, Ranges: ranges, Sequence: x.Sequence,
			ObservedEpoch: x.ObservedEpoch, ScanSHA256: x.ScanSHA256, OdomSHA256: x.OdomSHA256,
			EpochSHA256: x.EpochSHA256, MountSHA256: x.MountSHA256, CloudMountSHA256: x.CloudMountSHA256,
			EpochPublisher: x.EpochPublisher, MountPublisher: x.MountPublisher, CloudMountPublisher: x.CloudMountPublisher}
		fields["cloud"], _ = json.Marshal(wire)
	}
	raw, e := json.Marshal(fields)
	if e != nil {
		t.Fatal(e)
	}
	return raw
}

func TestCloudProfileExplicitOptInAndDigest(t *testing.T) {
	c, _, _, _ := cloudController(t)
	for _, name := range []string{"absent", "valid", "min-duration", "null", "missing-duration", "too-short", "too-long", "extra", "wrong-model", "wrong-scope", "legacy-motion", "v1", "changed-bytes"} {
		t.Run(name, func(t *testing.T) {
			cfg := c.cfg
			var m map[string]any
			_ = json.Unmarshal(cfg.ProfileJSON, &m)
			x := m["cloud_experiment"].(map[string]any)
			switch name {
			case "absent":
				delete(m, "cloud_experiment")
			case "min-duration":
				x["collector_duration_ms"] = 100
			case "null":
				m["cloud_experiment"] = nil
			case "missing-duration":
				delete(x, "collector_duration_ms")
			case "too-short":
				x["collector_duration_ms"] = 99
			case "too-long":
				x["collector_duration_ms"] = 60001
			case "extra":
				x["qualified_clearance"] = true
			case "wrong-model":
				x["model"] = "cloud"
			case "wrong-scope":
				x["scope"] = "physical"
			case "legacy-motion":
				m["motion_envelope"] = go2nav.IsotropicV1
				delete(m, "max_measured_lateral_mps")
			case "v1":
				m["schema"] = "wendy.world.go2-profile.v1"
			}
			cfg.ProfileJSON, _ = json.Marshal(m)
			cfg.Binding.ProfileSHA256 = SHA256(cfg.ProfileJSON)
			if name == "changed-bytes" {
				cfg.ProfileJSON = append(cfg.ProfileJSON, ' ')
			}
			_, _, e := DecodeConfiguration(cfg)
			ok := name == "absent" || name == "valid" || name == "min-duration"
			if (e == nil) != ok {
				t.Fatalf("admitted=%v want=%v err=%v", e == nil, ok, e)
			}
		})
	}
}

func TestCloudStateStrictShapeMissingReturnsAndBound(t *testing.T) {
	c, _, a, _ := cloudController(t)
	s, _ := a.State(context.Background(), c.session)
	s.Cloud.Ranges[0] = math.Inf(1)
	raw := cloudStateJSON(t, s)
	decoded, e := DecodeState(raw)
	if e != nil || decoded.Cloud == nil || !math.IsInf(decoded.Cloud.Ranges[0], 1) || decoded.Cloud.Ranges[1] != 4 {
		t.Fatalf("null lost: %v", e)
	}
	if e = c.validateState(decoded); e != nil {
		t.Fatal(e)
	}
	var top map[string]json.RawMessage
	_ = json.Unmarshal(raw, &top)
	var original map[string]json.RawMessage
	_ = json.Unmarshal(top["cloud"], &original)
	for key := range original {
		for _, mode := range []string{"missing", "null"} {
			t.Run(key+"-"+mode, func(t *testing.T) {
				var body map[string]json.RawMessage
				_ = json.Unmarshal(raw, &body)
				var cloud map[string]json.RawMessage
				_ = json.Unmarshal(body["cloud"], &cloud)
				if mode == "missing" {
					delete(cloud, key)
				} else {
					cloud[key] = json.RawMessage("null")
				}
				body["cloud"], _ = json.Marshal(cloud)
				bad, _ := json.Marshal(body)
				if _, e := DecodeState(bad); e == nil {
					t.Fatal("missing or null cloud field admitted")
				}
			})
		}
	}
	// All9000 ranges use a long shortest binary64 representation within the
	// supported range. The existing256KiB bound remains sufficient.
	v := math.Nextafter(.1, 1)
	for i := range s.Cloud.Ranges {
		s.Cloud.Ranges[i] = v
	}
	for i := range s.Scan.Ranges {
		s.Scan.Ranges[i] = v
	}
	raw = cloudStateJSON(t, s)
	t.Logf("9000 finite cloud rays plus360 scan rays: %d bytes, limit%d", len(raw), maxResponse)
	if len(raw) <= 64<<10 || len(raw) >= maxResponse {
		t.Fatal("worst-format fixture does not exercise the explicit state bound")
	}
	if _, e := DecodeState(raw); e != nil {
		t.Fatal(e)
	}
	if _, e := DecodeState(append(raw, bytes.Repeat([]byte(" "), maxResponse-len(raw)+1)...)); e == nil {
		t.Fatal("state bound expanded implicitly")
	}
}

func TestCloudIdentityAndTupleCoherenceRefuse(t *testing.T) {
	for _, name := range []string{"missing", "source", "frame", "hash", "zero-gid", "epoch", "sequence", "stamp", "short", "horizontal", "publisher", "epoch-publisher", "mount-publisher"} {
		t.Run(name, func(t *testing.T) {
			c, _, a, _ := cloudController(t)
			s, _ := a.State(context.Background(), c.session)
			switch name {
			case "missing":
				s.Cloud = nil
			case "source":
				s.Cloud.SourceDigest = "sha256:" + strings.Repeat("0", 64)
			case "frame":
				s.Cloud.Frame = "odom"
			case "hash":
				s.Cloud.OdomSHA256 = strings.Repeat("G", 64)
			case "zero-gid":
				s.Cloud.PublisherGID = strings.Repeat("0", 48)
			case "epoch":
				s.Cloud.ObservedEpoch++
			case "sequence":
				s.Cloud.Sequence++
			case "stamp":
				s.Cloud.CapturedUnixNS++
			case "short":
				s.Cloud.Ranges = s.Cloud.Ranges[:8999]
			case "horizontal":
				s.Cloud.Ranges[4320] = 3
			case "publisher":
				s.Cloud.PublisherGID = strings.Repeat("a", 48)
			case "epoch-publisher":
				s.Cloud.EpochPublisher = strings.Repeat("a", 48)
			case "mount-publisher":
				s.Cloud.MountPublisher, s.Cloud.CloudMountPublisher = strings.Repeat("a", 48), strings.Repeat("a", 48)
			}
			if e := c.validateState(s); e == nil {
				t.Fatal("changed cloud admitted")
			}
		})
	}
	legacy, _, _, _ := fixture(t)
	if e := legacy.Start(context.Background()); e != nil {
		t.Fatal(e)
	}
	s := snapshotState(legacy.lastState)
	s.Cloud = cloudForState(s)
	if e := legacy.validateState(s); e == nil {
		t.Fatal("cloud silently entered legacy path")
	}
}

func TestCloudRepeatedCaptureCannotChangeOrRenewOriginals(t *testing.T) {
	for _, mode := range []string{"same", "same-id-new-hash", "same-id-new-ray", "new-id-old-stamp", "future-nanosecond", "expired-new-id"} {
		t.Run(mode, func(t *testing.T) {
			c, _, a, clock := cloudController(t)
			s, _ := a.State(context.Background(), c.session)
			if e := c.validateState(s); e != nil {
				t.Fatal(e)
			}
			s = snapshotState(s)
			switch mode {
			case "same-id-new-hash":
				s.Cloud.ScanSHA256 = strings.Repeat("f", 64)
			case "same-id-new-ray":
				s.Cloud.Ranges[0] = 5
			case "new-id-old-stamp", "expired-new-id":
				s.CaptureSequence++
				s.Scan.Sequence++
				s.Cloud.Sequence++
				if mode == "expired-new-id" {
					clock.advance(251 * time.Millisecond)
				}
			case "future-nanosecond":
				s.CaptureSequence++
				s.Scan.Sequence++
				s.Cloud.Sequence++
				s.Cloud.CapturedUnixNS++
				s.Scan.CapturedUnixNS++
				s.ScanCapturedUnixNS++
			}
			e := c.validateState(s)
			if (e == nil) != (mode == "same") {
				t.Fatalf("%s: %v", mode, e)
			}
		})
	}
}

func TestCloudControllerSelectsBuilderAtBothGridStages(t *testing.T) {
	for _, stage := range []string{"initial-grid", "live-grid"} {
		t.Run(stage, func(t *testing.T) {
			c, g, a, _ := cloudController(t)
			a.cloudHook = func(s *State) {
				if stage == "initial-grid" || a.states == 2 {
					// Legacy tilted Build accepts a rotated360ray scan. The
					// pinned Cloud builder requires its original -pi azimuth.
					s.Scan.AngleMin = 0
				}
			}
			_, e := c.Tick(context.Background())
			if e == nil || c.diagnosticStage != stage || len(a.commands) != 0 || a.stops == 0 {
				t.Fatalf("cloud builder not selected at%s: stage=%s err=%v commands=%d", stage, c.diagnosticStage, e, len(a.commands))
			}
			if stage == "live-grid" && g.evaluations == 0 {
				t.Fatal("live grid refusal preceded authority")
			}
		})
	}
}

func TestCloudControllerKeepsAuthorityAndFinalDeadline(t *testing.T) {
	for _, mode := range []string{"success", "stop", "authority-delay", "final-expiry"} {
		t.Run(mode, func(t *testing.T) {
			c, g, a, clock := cloudController(t)
			if mode == "stop" {
				g.stop = true
			}
			if mode == "authority-delay" {
				g.hook = func(tasking.Telemetry) { clock.advance(501 * time.Millisecond) }
			}
			if mode == "final-expiry" {
				originalClock := c.cfg.Now
				advanced := false
				c.cfg.Now = func() time.Time {
					if c.diagnosticStage == "final-freshness" && !advanced {
						clock.advance(251 * time.Millisecond)
						advanced = true
					}
					return originalClock()
				}
			}
			d, e := c.Tick(context.Background())
			if mode == "success" {
				if e != nil || d.Action != mission.Continue || len(a.commands) != 1 {
					t.Fatalf("cloud fixture failed: stage=%s err=%v", c.diagnosticStage, e)
				}
			} else if len(a.commands) != 0 || a.stops == 0 || mode != "stop" && e == nil {
				t.Fatalf("authority/expiry bypass: err=%v commands=%d stops=%d", e, len(a.commands), a.stops)
			}
			if mode == "final-expiry" && (c.diagnosticStage != "final-freshness" || c.timingInvalid ||
				c.lastWallNS-c.observedState.Cloud.CapturedUnixNS < c.profile.ObservationMaxAgeMS*int64(time.Millisecond)) {
				t.Fatalf("test did not exercise final original expiry: stage=%s err=%v", c.diagnosticStage, e)
			}
		})
	}
}

func TestCloudSnapshotsPreserveTraceAndOwnRanges(t *testing.T) {
	c, _, a, _ := cloudController(t)
	s, _ := a.State(context.Background(), c.session)
	c.observedState = snapshotState(s)
	copy, ok := c.Observation()
	if !ok || copy.Cloud == nil {
		t.Fatal("cloud trace absent")
	}
	copy.Cloud.Ranges[0] = 11
	copy.Cloud.OdomSHA256 = "changed"
	if c.observedState.Cloud.Ranges[0] != 4 || c.observedState.Cloud.OdomSHA256 == "changed" {
		t.Fatal("observation cloud aliases state")
	}
	s.Cloud.Ranges[0] = math.Inf(1)
	shot := FaultSnapshot{State: s, Profile: c.profile}
	second := copyFault(shot)
	second.State.Cloud.Ranges[1] = 11
	second.Profile.CloudExperiment.Scope = "changed"
	if shot.State.Cloud.Ranges[1] != 4 || shot.Profile.CloudExperiment.Scope != "contained-simulation" {
		t.Fatal("fault snapshot aliases cloud or profile")
	}
	raw, e := json.Marshal(shot)
	if e != nil {
		t.Fatal(e)
	}
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(raw, &fields)
	decoded, e := DecodeState(fields["state"])
	if e != nil || decoded.Cloud == nil || !math.IsInf(decoded.Cloud.Ranges[0], 1) || decoded.Cloud.OdomSHA256 != s.Cloud.OdomSHA256 {
		t.Fatalf("diagnostic lost cloud trace: %v", e)
	}
}
