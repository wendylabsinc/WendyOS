package derivedreader

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/world"
	appv1 "github.com/wendylabsinc/wendy/go/world/production/api/world/app/v1"
	"github.com/wendylabsinc/wendy/go/world/production/appclock"
	"github.com/wendylabsinc/wendy/go/world/production/internal/testidentity"
	"github.com/wendylabsinc/wendy/go/world/production/resultrecord"
	"github.com/wendylabsinc/wendy/go/world/production/serviceorigin"
	"github.com/wendylabsinc/wendy/go/world/production/trustedtime"
	"github.com/wendylabsinc/wendy/go/world/production/wendycert"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type continuousPayload struct{ SampleUntil, PhysicalUntil time.Time }
type continuousFixture struct {
	appv1.WorldAppServiceClient
	cfg                                   Config
	physical                              trustedtime.Interval
	sample                                time.Time
	raw                                   []byte
	publisher                             *serviceorigin.Publisher
	queries, combined, physicalCalls      int
	failQuery, failCombined, failPhysical int
	errorCode                             codes.Code
	blockPhysical                         bool
	generation                            uint64
}

func (f *continuousFixture) Now() (trustedtime.Interval, error) { return f.physical, nil }
func (f *continuousFixture) clock(q *appv1.ClockRequest) *appv1.ClockReply {
	c := f.cfg
	return &appv1.ClockReply{Nonce: bytes.Clone(q.Nonce), Physical: &appv1.PhysicalClock{EarliestUnixNs: f.physical.Earliest.UnixNano(), LatestUnixNs: f.physical.Latest.UnixNano(), BootId: f.physical.BootID, MonotonicNs: int64(f.physical.Monotonic), Source: f.physical.Source}, Tenant: c.Tenant, Site: c.Site, Device: c.Device, App: c.App, Service: c.Service, ExecutionDomain: "simulation", DeploymentGeneration: f.generation, ServerBootId: strings.Repeat("c", 32), SampleClockDomain: "simulation:" + c.RunID, SampleUnixNs: f.sample.UnixNano()}
}
func (f *continuousFixture) Clock(ctx context.Context, q *appv1.ClockRequest, _ ...grpc.CallOption) (*appv1.ClockReply, error) {
	f.combined++
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if f.combined == f.failCombined {
		return nil, status.Error(f.errorCode, "injected combined transport failure")
	}
	return f.clock(q), nil
}
func (f *continuousFixture) PhysicalClock(ctx context.Context, q *appv1.ClockRequest, _ ...grpc.CallOption) (*appv1.PhysicalClockReply, error) {
	f.physicalCalls++
	if f.blockPhysical {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if f.physicalCalls == f.failPhysical {
		return nil, status.Error(f.errorCode, "injected physical transport failure")
	}
	c := f.clock(q)
	return &appv1.PhysicalClockReply{Nonce: c.Nonce, Physical: c.Physical, Tenant: c.Tenant, Site: c.Site, Device: c.Device, App: c.App, Service: c.Service, ExecutionDomain: c.ExecutionDomain, DeploymentGeneration: c.DeploymentGeneration, ServerBootId: c.ServerBootId}, nil
}
func (f *continuousFixture) Query(ctx context.Context, _ *appv1.QueryRequest, _ ...grpc.CallOption) (*appv1.QueryReply, error) {
	f.queries++
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if f.queries == f.failQuery {
		return nil, status.Error(f.errorCode, "injected query transport failure")
	}
	r := &appv1.QueryReply{}
	if f.raw != nil {
		r.SignedRecordJson = [][]byte{bytes.Clone(f.raw)}
	}
	return r, nil
}
func continuousSetup(t *testing.T) (*continuousFixture, *Session[continuousPayload]) {
	t.Helper()
	physical := time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)
	source := "spiffe://wendy.sh/tenant/" + testidentity.Tenant + "/device/22222222-2222-4222-8222-222222222222"
	id := testidentity.NewWith(t, physical, source)
	c := Config{Version: 1, Tenant: id.Tenant, Site: "office", Device: source, App: "com.example.viewer", Service: "viewer", ExpectedGeneration: 1, RootsDER: [][]byte{id.RootDER}, RunID: "continuous", Frame: world.FrameRef{ID: "office", Revision: "v1"}, Source: Source{Device: source, App: "com.example.sensors", Service: "people", Stream: "tracks"}, DurationMS: 1, MaxTicks: 1}
	f := &continuousFixture{cfg: c, physical: trustedtime.Interval{Earliest: physical, Latest: physical.Add(time.Millisecond), BootID: "continuous-test", Monotonic: time.Second, Domain: "physical", Authenticated: true, Source: "test"}, sample: time.Unix(1700000000, 0), generation: 1, errorCode: codes.Unavailable}
	certs, err := wendycert.NewDevice(wendycert.Config{Tenant: id.Tenant, Roots: [][]byte{id.RootDER}})
	if err != nil {
		t.Fatal(err)
	}
	f.publisher, err = serviceorigin.NewPublisher(serviceorigin.PublisherConfig{Scope: serviceorigin.Scope{Tenant: c.Tenant, Site: c.Site, Device: source, App: c.Source.App, Service: c.Source.Service, DeploymentGeneration: 3, ExecutionDomain: "simulation", Publish: []serviceorigin.PublishScope{{ComponentType: resultrecord.ComponentType, Stream: c.Source.Stream}}}, Epoch: 17, Lifetime: 5 * time.Minute, Clock: f, Certificates: certs, ChainDER: id.ChainDER, DeviceKey: id.Key})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.publisher.Close() })
	s, err := NewContinuous(c, f, func(r serviceorigin.PublishRequest) (Interpretation[continuousPayload], error) {
		var p continuousPayload
		if json.Unmarshal(r.Payload, &p) != nil {
			return Interpretation[continuousPayload]{}, ErrRefused
		}
		return Interpretation[continuousPayload]{Payload: p, ID: r.Entity, RunID: c.RunID, Frame: r.Frame, SampleValidUntil: p.SampleUntil, PhysicalValidUntil: p.PhysicalUntil}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return f, s
}
func (f *continuousFixture) publish(t *testing.T, entity string, lifetime time.Duration) []byte {
	t.Helper()
	p := continuousPayload{f.sample.Add(lifetime), f.physical.Earliest.Add(30 * time.Second)}
	raw, _ := json.Marshal(p)
	r := serviceorigin.PublishRequest{Entity: entity, ComponentType: resultrecord.ComponentType, Stream: f.cfg.Source.Stream, Frame: f.cfg.Frame, Timing: world.Timing{SampleTime: f.sample, ClockDomain: "simulation:" + f.cfg.RunID}, TTL: lifetime, Payload: raw, Provenance: world.Provenance{Origin: world.Simulated, AppVersion: "continuous-test", Simulation: &world.SimulationContext{RunID: f.cfg.RunID, ScenarioVersion: "test", ClockDomain: "simulation:" + f.cfg.RunID}}}
	signed, err := f.publisher.Publish(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	raw, err = json.Marshal(signed)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
func requireReady(t *testing.T, s *Session[continuousPayload], want int) Result[continuousPayload] {
	t.Helper()
	r, err := s.Step(context.Background())
	if err != nil || r.Status != "ready" || len(r.Items) != want {
		t.Fatalf("ready: status=%s count=%d error=%v", r.Status, len(r.Items), err)
	}
	return r
}

func TestContinuousTransientRecoveryPreservesPinsAndOriginals(t *testing.T) {
	for _, failure := range []string{"discovery", "authorize-first", "authorize-second", "clock-initial", "clock-final", "physical-first", "physical-second", "physical-third", "cancel-physical"} {
		t.Run(failure, func(t *testing.T) {
			f, s := continuousSetup(t)
			f.raw = f.publish(t, "track", time.Second)
			first := requireReady(t, s, 1)
			clock, scope, epoch, delegation := s.clock, s.scope, s.epoch, s.delegation
			verifier := s.verifier
			switch failure {
			case "discovery":
				f.failQuery = f.queries + 1
			case "authorize-first":
				f.failQuery = f.queries + 2
			case "authorize-second":
				f.failQuery = f.queries + 3
			case "clock-initial":
				f.failCombined = f.combined + 1
			case "clock-final":
				f.failCombined = f.combined + 2
			case "physical-first":
				f.failPhysical = f.physicalCalls + 1
			case "physical-second":
				f.failPhysical = f.physicalCalls + 2
			case "physical-third":
				f.failPhysical = f.physicalCalls + 3
			case "cancel-physical":
				f.blockPhysical = true
			}
			ctx := context.Background()
			if f.blockPhysical {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, 20*time.Millisecond)
				defer cancel()
			}
			start := time.Now()
			r, err := s.Step(ctx)
			if err == nil || r.Status != "unavailable" || len(r.Items) != 0 || len(r.AbsentIDs) != 0 || r.OriginAttestedInference || r.Clock != nil || s.fault {
				t.Fatalf("temporary read became terminal/absence: %+v %v", r, err)
			}
			if f.blockPhysical && time.Since(start) > 300*time.Millisecond {
				t.Fatal("verification ignored caller context")
			}
			f.blockPhysical = false
			again := requireReady(t, s, 1)
			if s.clock != clock || s.scope != scope || s.epoch != epoch || s.delegation != delegation || s.water["track"].hash != first.Items[0].EnvelopeSHA256 || !bytes.Equal(again.Items[0].EnvelopeJSON, first.Items[0].EnvelopeJSON) {
				t.Fatal("recovery reset identity/history/originals")
			}
			clockFailure := strings.HasPrefix(failure, "physical-") || failure == "cancel-physical"
			if (s.verifier != verifier) != clockFailure {
				t.Fatal("wrong verifier replacement boundary")
			}
		})
	}
}

func TestContinuousFailuresRemainTerminalAndUnverifiedWindowsNeverRetire(t *testing.T) {
	t.Run("initial-denial-then-ready", func(t *testing.T) {
		f, s := continuousSetup(t)
		f.raw = f.publish(t, "track", time.Second)
		f.failQuery = 1
		f.errorCode = codes.PermissionDenied
		r, err := s.Step(context.Background())
		if !errors.Is(err, ErrUnavailable) || r.Status != "unavailable" || s.fault || s.querySeen {
			t.Fatal("normal initial source wait became terminal", r, err)
		}
		requireReady(t, s, 1)
	})
	t.Run("clock-authority-denied", func(t *testing.T) {
		f, s := continuousSetup(t)
		f.raw = f.publish(t, "track", time.Second)
		requireReady(t, s, 1)
		f.failPhysical = f.physicalCalls + 1
		f.errorCode = codes.PermissionDenied
		r, err := s.Step(context.Background())
		if status.Code(err) != codes.PermissionDenied || r.Status != "failed" || !s.fault {
			t.Fatal("clock authority refusal recovered", r, err)
		}
	})
	t.Run("authority-denied", func(t *testing.T) {
		f, s := continuousSetup(t)
		f.raw = f.publish(t, "track", time.Second)
		requireReady(t, s, 1)
		f.failQuery = f.queries + 2
		f.errorCode = codes.PermissionDenied
		r, err := s.Step(context.Background())
		if err == nil || r.Status != "failed" || !s.fault {
			t.Fatal("authority refusal recovered")
		}
	})
	t.Run("generation", func(t *testing.T) {
		f, s := continuousSetup(t)
		f.raw = f.publish(t, "track", time.Second)
		requireReady(t, s, 1)
		f.generation++
		if _, err := s.Step(context.Background()); !errors.Is(err, appclock.ErrFaulted) || !s.fault {
			t.Fatal("identity fault recovered", err)
		}
	})
	t.Run("signature", func(t *testing.T) {
		f, s := continuousSetup(t)
		raw := f.publish(t, "track", time.Second)
		var signed serviceorigin.SignedRecord
		json.Unmarshal(raw, &signed)
		signed.Signature[0] ^= 1
		f.raw, _ = json.Marshal(signed)
		if _, err := s.Step(context.Background()); err == nil || !s.fault {
			t.Fatal("bad signature recovered")
		}
		if s.water["track"].verified || !s.water["track"].sampleUntil.IsZero() {
			t.Fatal("unverified original granted eviction window")
		}
	})
	t.Run("temporary-before-verification-finishes", func(t *testing.T) {
		f, s := continuousSetup(t)
		f.raw = f.publish(t, "track", time.Second)
		f.failQuery = 2
		r, err := s.Step(context.Background())
		if err == nil || r.Status != "unavailable" {
			t.Fatal(err)
		}
		if s.water["track"].verified || !s.water["track"].sampleUntil.IsZero() {
			t.Fatal("partial verification granted eviction window")
		}
		f.sample = f.sample.Add(2 * time.Second)
		requireReady(t, s, 0)
		if s.retired != (world.Version{}) {
			t.Fatal("unverified entry was retired")
		}
		requireReady(t, s, 0)
		if len(s.water) != 0 || s.retired.Sequence != 1 {
			t.Fatal("verified expired entry not retired")
		}
	})
}

func TestContinuousTransientNewIDsDoNotLeakUnverifiedHistory(t *testing.T) {
	f, s := continuousSetup(t)
	f.raw = f.publish(t, "established", time.Minute)
	requireReady(t, s, 1)
	established := s.water["established"]
	floor := s.retired
	f.raw = f.publish(t, "established", time.Second)
	f.failQuery = f.queries + 2
	if _, err := s.Step(context.Background()); !errors.Is(err, ErrUnavailable) || s.water["established"] != established {
		t.Fatal("unverified replacement lost established mark", err)
	}
	for i := 0; i < 32; i++ {
		f.raw = f.publish(t, fmt.Sprintf("transient-%d", i), time.Second)
		f.failQuery = f.queries + 2
		r, err := s.Step(context.Background())
		if !errors.Is(err, ErrUnavailable) || r.Status != "unavailable" || s.fault {
			t.Fatal("transient new record became terminal", r, err)
		}
		if len(s.water) != 1 || s.water["established"] != established || s.retired != floor {
			t.Fatal("unverified ID retained or established history reset")
		}
		f.sample = f.sample.Add(time.Second)
	}
	f.raw = f.publish(t, "fresh", time.Second)
	requireReady(t, s, 1)
}

func TestContinuousMoreThan4096ExpiredIDsKeepsBoundedHistory(t *testing.T) {
	f, s := continuousSetup(t)
	clock := s.clock
	for i := 0; i < 4100; i++ {
		f.raw = f.publish(t, fmt.Sprintf("track-%04d", i), time.Second)
		r := requireReady(t, s, 1)
		if r.Tick != i+1 || len(s.water) > 1 {
			t.Fatal("tick/history bound", r.Tick, len(s.water))
		}
		f.sample = f.sample.Add(2 * time.Second)
	}
	if s.ticks != 4100 || s.clock != clock || s.retired.Sequence != 4099 {
		t.Fatal("continuous history reset or stopped")
	}
}

func TestContinuousRetirementKeepsLongerOldWindowAndRefusesUnseenOldLive(t *testing.T) {
	t.Run("shorter-replacement", func(t *testing.T) {
		f, s := continuousSetup(t)
		f.raw = f.publish(t, "track", 10*time.Second)
		requireReady(t, s, 1)
		until := s.water["track"].sampleUntil
		f.raw = f.publish(t, "track", time.Second)
		requireReady(t, s, 1)
		f.sample = f.sample.Add(2 * time.Second)
		requireReady(t, s, 0)
		if len(s.water) != 1 || s.water["track"].sampleUntil != until || s.retired != (world.Version{}) {
			t.Fatal("short replacement retired an older live bound")
		}
	})
	t.Run("retired-floor", func(t *testing.T) {
		f, s := continuousSetup(t)
		old := f.publish(t, "track", 10*time.Second)
		f.raw = f.publish(t, "track", time.Second)
		requireReady(t, s, 1)
		f.sample = f.sample.Add(2 * time.Second)
		f.raw = old
		r, err := s.Step(context.Background())
		if !errors.Is(err, ErrUnavailable) || r.Status != "unavailable" || len(r.AbsentIDs) != 0 || s.fault || s.retired.Sequence != 2 {
			t.Fatal("unseen old live record became visible/absence", r, err)
		}
		floor := s.retired
		f.failQuery = f.queries + 1
		if _, err = s.Step(context.Background()); err == nil || s.retired != floor {
			t.Fatal("transport failure reset floor")
		}
		f.raw = f.publish(t, "fresh", time.Second)
		requireReady(t, s, 1)
	})
}
