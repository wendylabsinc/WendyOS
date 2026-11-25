package hazardreader

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/world"
	appv1 "github.com/wendylabsinc/wendy/go/world/production/api/world/app/v1"
	"github.com/wendylabsinc/wendy/go/world/production/fusioncapture"
	"github.com/wendylabsinc/wendy/go/world/production/internal/testidentity"
	"github.com/wendylabsinc/wendy/go/world/production/resultrecord"
	"github.com/wendylabsinc/wendy/go/world/production/serviceorigin"
	"github.com/wendylabsinc/wendy/go/world/production/trustedtime"
	"github.com/wendylabsinc/wendy/go/world/production/wendycert"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

type fixture struct {
	appv1.UnimplementedWorldAppServiceServer
	mu        sync.Mutex
	cfg       Config
	w         trustedtime.Interval
	now       time.Time
	request   serviceorigin.PublishRequest
	publisher *serviceorigin.Publisher
	raw       []byte
	queries   int
	denied    int
	queryHook func(int)
}

func (f *fixture) Now() (trustedtime.Interval, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.w, nil
}
func (f *fixture) Clock(_ context.Context, q *appv1.ClockRequest) (*appv1.ClockReply, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return &appv1.ClockReply{Nonce: bytes.Clone(q.Nonce), Physical: &appv1.PhysicalClock{EarliestUnixNs: f.w.Earliest.UnixNano(), LatestUnixNs: f.w.Latest.UnixNano(), BootId: f.w.BootID, MonotonicNs: int64(f.w.Monotonic), Source: f.w.Source}, Tenant: f.cfg.Tenant, Site: f.cfg.Site, Device: f.cfg.Device, App: f.cfg.App, Service: f.cfg.Service, ExecutionDomain: "simulation", DeploymentGeneration: 9, ServerBootId: strings.Repeat("a", 32), SampleClockDomain: "simulation:" + f.cfg.RunID, SampleUnixNs: f.now.UnixNano()}, nil
}
func (f *fixture) PhysicalClock(ctx context.Context, q *appv1.ClockRequest) (*appv1.PhysicalClockReply, error) {
	c, e := f.Clock(ctx, q)
	if e != nil {
		return nil, e
	}
	return &appv1.PhysicalClockReply{Nonce: c.Nonce, Physical: c.Physical, Tenant: c.Tenant, Site: c.Site, Device: c.Device, App: c.App, Service: c.Service, ExecutionDomain: c.ExecutionDomain, DeploymentGeneration: c.DeploymentGeneration, ServerBootId: c.ServerBootId}, nil
}
func (f *fixture) Query(_ context.Context, q *appv1.QueryRequest) (*appv1.QueryReply, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.queries++
	if f.queryHook != nil {
		f.queryHook(f.queries)
	}
	if len(q.Sources) != 1 || q.Sources[0].Device != f.cfg.Source.Device || q.Sources[0].App != f.cfg.Source.App || q.Sources[0].Service != f.cfg.Source.Service || q.Sources[0].Stream != f.cfg.Source.Stream || q.Sources[0].ComponentType != resultrecord.ComponentType || q.Limit != 8 || q.After != "" {
		return nil, status.Error(codes.PermissionDenied, "scope")
	}
	if f.denied > 0 {
		f.denied--
		return nil, status.Error(codes.Unavailable, "startup")
	}
	r := &appv1.QueryReply{}
	if len(f.raw) > 0 {
		r.SignedRecordJson = [][]byte{bytes.Clone(f.raw)}
	}
	return r, nil
}
func setup(t *testing.T) (*fixture, appv1.WorldAppServiceClient) {
	t.Helper()
	raw, e := os.ReadFile("testdata/hazard.json")
	if e != nil {
		t.Fatal(e)
	}
	var p serviceorigin.PublishRequest
	if json.Unmarshal(raw, &p) != nil {
		t.Fatal("request fixture")
	}
	v, e := fusioncapture.ValidateReplicaPublication(p)
	if e != nil {
		t.Fatal(e)
	}
	physical := v.PhysicalValidUntil.Add(-4 * time.Second)
	origin := v.Result.Inputs[0].Key.Source.Device
	id := testidentity.NewWith(t, physical, origin)
	certs, e := wendycert.NewDevice(wendycert.Config{Tenant: id.Tenant, Roots: [][]byte{id.RootDER}})
	if e != nil {
		t.Fatal(e)
	}
	f := &fixture{w: trustedtime.Interval{Earliest: physical, Latest: physical.Add(time.Millisecond), BootID: "hazard-test-clock", Monotonic: time.Second, Domain: "physical", Authenticated: true, Source: "test"}, now: p.Timing.SampleTime.Add(500 * time.Millisecond), request: p}
	f.cfg = Config{Version: 1, Tenant: id.Tenant, Site: v.Result.Inputs[0].Key.Site, Device: "spiffe://wendy.sh/tenant/" + id.Tenant + "/device/33333333-3333-4333-8333-333333333333", App: "com.wendy.world.hazard", Service: "reader", ExpectedGeneration: 9, RootsDER: [][]byte{id.RootDER}, RunID: "run-a", Frame: p.Frame, Source: Source{Device: origin, App: "com.wendy.world.hazard", Service: "fusion", Stream: p.Stream}, DurationMS: 10000, MaxTicks: 8, RequestTimeoutMS: 3000}
	f.publisher, e = serviceorigin.NewPublisher(serviceorigin.PublisherConfig{Scope: serviceorigin.Scope{Tenant: id.Tenant, Site: f.cfg.Site, Device: origin, App: f.cfg.Source.App, Service: f.cfg.Source.Service, DeploymentGeneration: 7, ExecutionDomain: "simulation", Publish: []serviceorigin.PublishScope{{ComponentType: resultrecord.ComponentType, Stream: p.Stream}}}, Epoch: 17, Lifetime: time.Minute, Clock: f, Certificates: certs, ChainDER: id.ChainDER, DeviceKey: id.Key})
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { f.publisher.Close() })
	f.sign(t)
	l := bufconn.Listen(1 << 20)
	server := grpc.NewServer()
	appv1.RegisterWorldAppServiceServer(server, f)
	go func() { _ = server.Serve(l) }()
	t.Cleanup(server.Stop)
	conn, e := grpc.NewClient("passthrough:///hazard", grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return l.Dial() }), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { conn.Close() })
	return f, appv1.NewWorldAppServiceClient(conn)
}
func (f *fixture) sign(t *testing.T) {
	t.Helper()
	signed, e := f.publisher.Publish(context.Background(), f.request)
	if e != nil {
		t.Fatal(e)
	}
	f.raw, e = json.Marshal(signed)
	if e != nil {
		t.Fatal(e)
	}
}
func digest(b []byte) string { v := sha256.Sum256(b); return hex.EncodeToString(v[:]) }
func (f *fixture) fresh(t *testing.T) {
	t.Helper()
	v, e := fusioncapture.ValidateReplicaPublication(f.request)
	if e != nil {
		t.Fatal(e)
	}
	// This is a structural origin-attested fixture, not a second inference.
	// All referenced inputs receive distinct versions/hashes and source times.
	delta := 3 * time.Second
	h := v.Hazard
	r := v.Result
	for i := range h.Inputs {
		in := &h.Inputs[i]
		ref := &r.Inputs[i]
		ref.Version.Sequence++
		ref.EnvelopeSHA256 = digest([]byte(ref.EnvelopeSHA256 + "/fresh"))
		ref.RecordSHA256 = digest([]byte(ref.RecordSHA256 + "/fresh"))
		in.Version = ref.Version
		in.Capture.ProofSHA256 = ref.EnvelopeSHA256
		in.SampleTime = in.SampleTime.Add(delta)
		in.CaptureStart = in.CaptureStart.Add(delta)
		in.ValidUntil = in.ValidUntil.Add(delta)
		in.SampleID = "run-a/" + in.SensorID + "/18"
		if in.Sample != nil {
			in.Sample.Sequence = 18
			in.Sample.ID = in.SampleID
			in.Sample.SampleNS += int64(delta)
		}
		if in.LidarSummary != nil {
			in.LidarSummary.NormalizedSampleSHA256 = digest([]byte(in.LidarSummary.NormalizedSampleSHA256 + "/fresh"))
		}
		f.request.Provenance.Inputs[i].ID = ref.ID()
	}
	h.ValidUntil = h.ValidUntil.Add(delta)
	h.PhysicalValidUntil = f.w.Earliest.Add(4 * time.Second)
	h.InputDigest = digest([]byte(h.InputDigest + "/fresh"))
	r.ResultJSON, e = json.Marshal(h)
	if e != nil {
		t.Fatal(e)
	}
	f.request.Timing.SampleTime = f.request.Timing.SampleTime.Add(delta)
	f.request.Payload, e = resultrecord.Encode(r)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = fusioncapture.ValidateReplicaPublication(f.request); e != nil {
		t.Fatal("fresh structural fixture", e)
	}
	f.now = f.request.Timing.SampleTime.Add(500 * time.Millisecond)
	f.sign(t)
}

func TestSignedHazardReadExpiryAndFreshRecovery(t *testing.T) {
	for _, mode := range []string{"sample", "physical", "empty-authority-query"} {
		t.Run(mode, func(t *testing.T) {
			f, api := setup(t)
			s, e := New(f.cfg, api)
			if e != nil {
				t.Fatal(e)
			}
			defer s.Close()
			first, e := s.Step(context.Background())
			if e != nil || first.Status != "ready" || len(first.Hazards) != 1 {
				t.Fatalf("first hazard count=%d err=%v", len(first.Hazards), e)
			}
			if !bytes.Equal(first.Hazards[0].EnvelopeJSON, f.raw) || first.Hazards[0].EnvelopeSHA256 != digest(f.raw) || !first.OriginAttestedInference || first.RawInputsIndependentlyVerified || first.Hazards[0].Payload.Spatial == nil {
				t.Fatal("exact origin-attested receipt differs")
			}
			f.mu.Lock()
			at := f.queries + 3
			f.queryHook = func(n int) {
				if n != at {
					return
				}
				if mode == "sample" {
					f.now = f.now.Add(3 * time.Second)
				} else {
					f.w.Earliest = f.w.Earliest.Add(6 * time.Second)
					f.w.Latest = f.w.Latest.Add(6 * time.Second)
					f.w.Monotonic += 6 * time.Second
				}
				if mode == "empty-authority-query" {
					f.raw = nil
				}
			}
			f.mu.Unlock()
			empty, e := s.Step(context.Background())
			if e != nil || empty.Status != "ready" || len(empty.Hazards) != 0 || len(empty.AbsentHazardIDs) != 1 || empty.AbsentHazardIDs[0] != f.request.Entity {
				t.Fatalf("expiry did not omit: %+v %v", empty, e)
			}
			f.fresh(t)
			fresh, e := s.Step(context.Background())
			if e != nil || len(fresh.Hazards) != 1 || fresh.Hazards[0].Version != (world.Version{Epoch: 17, Sequence: 2}) || !bytes.Equal(fresh.Hazards[0].EnvelopeJSON, f.raw) {
				t.Fatalf("fresh count=%d err=%v", len(fresh.Hazards), e)
			}
		})
	}
}
func TestHazardStartupAndFixedScopeRefusals(t *testing.T) {
	for _, mode := range []string{"startup", "scope", "model", "missing-physical-cap", "signature"} {
		t.Run(mode, func(t *testing.T) {
			f, api := setup(t)
			if mode == "startup" {
				f.denied = 1
			}
			if mode == "scope" {
				f.cfg.Source.Service = "other"
			}
			if mode == "model" {
				f.request.Provenance.ModelVersion = "other"
				f.sign(t)
			}
			if mode == "missing-physical-cap" {
				r, _ := resultrecord.Decode(f.request.Payload)
				var h map[string]json.RawMessage
				json.Unmarshal(r.ResultJSON, &h)
				delete(h, "physical_valid_until")
				r.ResultJSON, _ = json.Marshal(h)
				f.request.Payload, _ = resultrecord.Encode(r)
				f.sign(t)
			}
			if mode == "signature" {
				var signed serviceorigin.SignedRecord
				json.Unmarshal(f.raw, &signed)
				signed.Signature[0] ^= 1
				f.raw, _ = json.Marshal(signed)
			}
			s, e := New(f.cfg, api)
			if e != nil {
				t.Fatal(e)
			}
			defer s.Close()
			r, e := s.Step(context.Background())
			if mode == "startup" {
				raw, _ := json.Marshal(r)
				if e == nil || r.Status != "waiting_for_source" || len(r.Hazards) != 0 || bytes.Contains(raw, []byte("absent_hazard_ids")) || r.OriginAttestedInference {
					t.Fatal("startup invented ready absence")
				}
				r, e = s.Step(context.Background())
				if e != nil || r.Status != "ready" || len(r.Hazards) != 1 {
					t.Fatal("startup did not become ready", e)
				}
				return
			}
			if e == nil || r.Status != "failed" || len(r.Hazards) != 0 {
				t.Fatal("invalid hazard became current", fmt.Sprint(e))
			}
		})
	}
}
