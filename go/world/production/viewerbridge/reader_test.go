package viewerbridge

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/client"
	"github.com/wendylabsinc/wendy/go/world/internal/node"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
	appv1 "github.com/wendylabsinc/wendy/go/world/production/api/world/app/v1"
	"github.com/wendylabsinc/wendy/go/world/production/appclock"
	"github.com/wendylabsinc/wendy/go/world/production/hazardreader"
	"github.com/wendylabsinc/wendy/go/world/production/internal/derivedreader"
	"github.com/wendylabsinc/wendy/go/world/production/internal/testidentity"
	"github.com/wendylabsinc/wendy/go/world/production/peoplereader"
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

type grpcFixture struct {
	appv1.UnimplementedWorldAppServiceServer
	mu                   sync.Mutex
	cfg                  Config
	w                    trustedtime.Interval
	now                  time.Time
	raw                  map[string][][]byte
	want                 []world.Component
	queries              int
	clocks               int
	sampleClocks         int
	mode                 string
	finalAdvance         time.Duration
	finalPhysicalAdvance time.Duration
	evidence             map[EvidenceSource]testMedia
	evidenceCalls        int
	evidenceMode         string
}

func (f *grpcFixture) Now() (trustedtime.Interval, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.w, nil
}
func (f *grpcFixture) clock(q *appv1.ClockRequest, sample bool) *appv1.ClockReply {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.clocks++
	if sample {
		f.sampleClocks++
		if f.sampleClocks == 5 {
			f.now = f.now.Add(f.finalAdvance)
			f.w.Earliest = f.w.Earliest.Add(f.finalPhysicalAdvance)
			f.w.Latest = f.w.Latest.Add(f.finalPhysicalAdvance)
			f.w.Monotonic += f.finalPhysicalAdvance
		}
	}
	p := f.cfg.People
	generation := p.ExpectedGeneration
	if f.mode == "wrong-generation" {
		generation++
	}
	return &appv1.ClockReply{Nonce: bytes.Clone(q.Nonce), Physical: &appv1.PhysicalClock{EarliestUnixNs: f.w.Earliest.UnixNano(), LatestUnixNs: f.w.Latest.UnixNano(), BootId: f.w.BootID, MonotonicNs: int64(f.w.Monotonic), Source: f.w.Source}, Tenant: p.Tenant, Site: p.Site, Device: p.Device, App: p.App, Service: p.Service, ExecutionDomain: "simulation", DeploymentGeneration: generation, ServerBootId: strings.Repeat("c", 32), SampleClockDomain: "simulation:" + p.RunID, SampleUnixNs: f.now.UnixNano()}
}
func (f *grpcFixture) Clock(_ context.Context, q *appv1.ClockRequest) (*appv1.ClockReply, error) {
	return f.clock(q, true), nil
}
func (f *grpcFixture) PhysicalClock(_ context.Context, q *appv1.ClockRequest) (*appv1.PhysicalClockReply, error) {
	c := f.clock(q, false)
	return &appv1.PhysicalClockReply{Nonce: c.Nonce, Physical: c.Physical, Tenant: c.Tenant, Site: c.Site, Device: c.Device, App: c.App, Service: c.Service, ExecutionDomain: c.ExecutionDomain, DeploymentGeneration: c.DeploymentGeneration, ServerBootId: c.ServerBootId}, nil
}
func (f *grpcFixture) Query(_ context.Context, q *appv1.QueryRequest) (*appv1.QueryReply, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.queries++
	if len(q.Sources) != 1 || q.After != "" || q.Limit != 8 {
		return nil, status.Error(codes.InvalidArgument, "bounded source query required")
	}
	s := q.Sources[0]
	expected := f.cfg.People.Source
	if s.Service == f.cfg.Hazards.Source.Service {
		expected = peoplereader.Source(f.cfg.Hazards.Source)
	}
	if s.Device != expected.Device || s.App != expected.App || s.Service != expected.Service || s.Stream != expected.Stream || s.ComponentType != resultrecord.ComponentType {
		return nil, status.Error(codes.PermissionDenied, "exact source differs")
	}
	if f.mode == "denied" {
		return nil, status.Error(codes.PermissionDenied, "source denied")
	}
	if f.mode == "unavailable" {
		return nil, status.Error(codes.Unavailable, "source temporarily unavailable")
	}
	out := &appv1.QueryReply{}
	for _, raw := range f.raw[s.Service] {
		out.SignedRecordJson = append(out.SignedRecordJson, bytes.Clone(raw))
	}
	if f.mode == "partial" {
		out.Next = "unfinished"
	}
	return out, nil
}

func setup(t *testing.T) (*grpcFixture, appv1.WorldAppServiceClient) {
	t.Helper()
	var data struct {
		Snapshot client.Snapshot `json:"snapshot"`
		Clocks   struct {
			Sample   time.Time `json:"sample_now"`
			Physical time.Time `json:"physical_now"`
		} `json:"clocks"`
	}
	raw, err := os.ReadFile(filepath.Join("..", "..", "testdata", "native-inference-viewer-v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(raw, &data); err != nil {
		t.Fatal(err)
	}
	person, hazard := data.Snapshot.Components[0], data.Snapshot.Components[2]
	id := testidentity.NewWith(t, data.Clocks.Physical, person.Key.Source.Device)
	certs, err := wendycert.NewDevice(wendycert.Config{Tenant: id.Tenant, Roots: [][]byte{id.RootDER}})
	if err != nil {
		t.Fatal(err)
	}
	p := peoplereader.Config{Version: 1, Tenant: id.Tenant, Site: person.Key.Site, Device: strings.Replace(person.Key.Source.Device, "22222222-2222-4222-8222-222222222222", "33333333-3333-4333-8333-333333333333", 1), App: person.Key.Source.App, Service: "viewer", ExpectedGeneration: 4, RootsDER: [][]byte{id.RootDER}, RunID: "viewer-native-v1", Frame: person.Frame, Source: peoplereader.Source(person.Key.Source), DurationMS: 60000, MaxTicks: 32, PollIntervalMS: 250, RequestTimeoutMS: 3000}
	h := hazardreader.Config(p)
	h.Source = hazardreader.Source(hazard.Key.Source)
	f := &grpcFixture{cfg: Config{Version: 1, People: p, Hazards: h}, w: trustedtime.Interval{Earliest: data.Clocks.Physical, Latest: data.Clocks.Physical.Add(time.Millisecond), BootID: "viewer-grpc", Monotonic: time.Second, Domain: "physical", Authenticated: true, Source: "test-only-time"}, now: data.Clocks.Sample, raw: map[string][][]byte{}}
	for _, c := range data.Snapshot.Components {
		scope := serviceorigin.Scope{Tenant: c.Key.Tenant, Site: c.Key.Site, Device: c.Key.Source.Device, App: c.Key.Source.App, Service: c.Key.Source.Service, DeploymentGeneration: 3, ExecutionDomain: "simulation", Publish: []serviceorigin.PublishScope{{ComponentType: c.Key.Type, Stream: c.Key.Source.Stream}}}
		// One source publisher owns both person entities. Keep the exact source
		// incarnation common across every result returned by its complete Query.
		if len(f.raw[scope.Service]) > 0 {
			continue
		}
		publisher, err := serviceorigin.NewPublisher(serviceorigin.PublisherConfig{Scope: scope, Epoch: 7, Lifetime: time.Minute, Clock: f, Certificates: certs, ChainDER: id.ChainDER, DeviceKey: id.Key})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { publisher.Close() })
		for _, input := range data.Snapshot.Components {
			if input.Key.Source != c.Key.Source {
				continue
			}
			publication := serviceorigin.PublishRequest{Entity: input.Key.Entity, ComponentType: input.Key.Type, Stream: input.Key.Source.Stream, Frame: input.Frame, Timing: input.Timing, TTL: input.TTL, Provenance: input.Provenance, Payload: input.Payload}
			signed, err := publisher.Publish(context.Background(), publication)
			if err != nil {
				t.Fatal(err)
			}
			envelope, err := json.Marshal(signed)
			if err != nil {
				t.Fatal(err)
			}
			f.raw[scope.Service] = append(f.raw[scope.Service], envelope)
			var record serviceorigin.Record
			if err = json.Unmarshal(signed.Record, &record); err != nil {
				t.Fatal(err)
			}
			f.want = append(f.want, serviceorigin.VerifiedRecord{Record: record}.Component())
		}
	}
	l := bufconn.Listen(4 << 20)
	server := grpc.NewServer()
	appv1.RegisterWorldAppServiceServer(server, f)
	go func() { _ = server.Serve(l) }()
	connection, err := grpc.NewClient("passthrough:///viewer-fixture", grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return l.Dial() }))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { connection.Close(); server.Stop(); l.Close() })
	return f, appv1.NewWorldAppServiceClient(connection)
}

func TestActualSignedGRPCOriginalProjectionAndExpiry(t *testing.T) {
	f, api := setup(t)
	s, err := New(f.cfg, api)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	first, err := s.Query(context.Background())
	if err != nil || len(first) != 3 {
		t.Fatalf("first read: %d %v", len(first), err)
	}
	want := map[world.Key]world.Component{}
	for _, c := range f.want {
		want[c.Key] = c
	}
	for _, c := range first {
		if !reflect.DeepEqual(c, want[c.Key]) {
			t.Fatal("projection changed source, payload, version, timing or provenance")
		}
	}
	again, err := s.Query(context.Background())
	if err != nil || !reflect.DeepEqual(first, again) {
		t.Fatal("repeat read changed originals", err)
	}
	f.mu.Lock()
	f.now = f.now.Add(5 * time.Second)
	f.mu.Unlock()
	empty, err := s.Query(context.Background())
	if err != nil || len(empty) != 0 {
		t.Fatal("original sample expiry was renewed", err)
	}
}

func TestExpiryAfterTwoReadersBeforeHTTPProjection(t *testing.T) {
	for _, kind := range []string{"sample", "physical"} {
		t.Run(kind, func(t *testing.T) {
			f, api := setup(t)
			if kind == "sample" {
				f.finalAdvance = 5 * time.Second
			} else {
				f.finalPhysicalAdvance = 6 * time.Second
			}
			s, err := New(f.cfg, api)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			got, err := s.Query(context.Background())
			if err != nil || len(got) != 0 {
				t.Fatalf("final combined clock did not omit expired results: %d %v", len(got), err)
			}
		})
	}
}

func TestCanceledRequestDoesNotConsumeNextNativeRead(t *testing.T) {
	f, api := setup(t)
	s, err := New(f.cfg, api)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = s.Query(ctx); err == nil {
		t.Fatal("canceled query accepted")
	}
	if got, err := s.Query(context.Background()); err != nil || len(got) != 3 {
		t.Fatal("canceled queued request faulted reader", err)
	}
}

func TestChangedOriginalSignatureDoesNotReachHTTPProjection(t *testing.T) {
	f, api := setup(t)
	service := f.cfg.People.Source.Service
	var signed serviceorigin.SignedRecord
	if err := json.Unmarshal(f.raw[service][0], &signed); err != nil {
		t.Fatal(err)
	}
	signed.Signature[0] ^= 1
	var err error
	f.raw[service][0], err = json.Marshal(signed)
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(f.cfg, api)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if got, err := s.Query(context.Background()); err == nil || got != nil {
		t.Fatal("changed native signature accepted")
	}
}

func TestExistingReaderEnvelopeBudgetPreservesExactRecord(t *testing.T) {
	f, api := setup(t)
	name := f.cfg.People.Source.Service
	// Outer whitespace changes the transport envelope hash, not the exact
	// signed Record or Delegation bytes. This is valid ordinary reader input.
	f.raw[name][0] = append(f.raw[name][0], bytes.Repeat([]byte(" "), 65<<10)...)
	if len(f.raw[name][0]) <= 64<<10 || len(f.raw[name][0]) > 256<<10 {
		t.Fatal("fixture does not exercise outer envelope bound")
	}
	s, err := New(f.cfg, api)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	got, err := s.Query(context.Background())
	if err != nil || len(got) != 3 {
		t.Fatal("valid existing reader envelope refused", err)
	}
	for _, c := range got {
		for _, original := range f.want {
			if c.Key == original.Key && !reflect.DeepEqual(c, original) {
				t.Fatal("large envelope altered original record")
			}
		}
	}
}

func TestCombiningUpperBoundsDoesNotHideBackwardsFinalClock(t *testing.T) {
	at := func(n int64) time.Time { return time.Unix(n, 0).UTC() }
	now := appclock.Observation{SampleTime: at(2), SampleClockDomain: "simulation:fixture", ServerBootID: "boot", DeploymentGeneration: 1, Physical: trustedtime.Interval{Earliest: at(101), Latest: at(104)}}
	read := func(low, high int64) *derivedreader.Clock {
		return &derivedreader.Clock{SampleTime: at(2), SampleClockDomain: now.SampleClockDomain, ServerBootID: now.ServerBootID, DeploymentGeneration: 1, PhysicalEarliest: at(low), PhysicalLatest: at(high)}
	}
	a, b := read(100, 110), read(105, 106)
	for _, order := range [][]*derivedreader.Clock{{a, b}, {b, a}} {
		if _, err := combineClocks(now, order...); err == nil {
			t.Fatal("wide earlier read masked disjoint final clock")
		}
	}
	b = read(103, 106)
	got, err := combineClocks(now, a, b)
	if err != nil || !got.Physical.Latest.Equal(at(110)) || !got.SampleTime.Equal(now.SampleTime) || !got.Physical.Earliest.Equal(now.Physical.Earliest) {
		t.Fatal("overlap did not retain conservative bound and original observation", err)
	}
}

func TestIncompleteDeniedOrChangedIdentityIsNotEmptySuccess(t *testing.T) {
	for _, mode := range []string{"partial", "denied", "wrong-generation"} {
		t.Run(mode, func(t *testing.T) {
			f, api := setup(t)
			f.mode = mode
			s, err := New(f.cfg, api)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			if got, err := s.Query(context.Background()); err == nil || got != nil {
				t.Fatal("unavailable read claimed absence", err)
			}
		})
	}
}

func TestReaderConfigurationKeepsOneMountedIdentity(t *testing.T) {
	f, api := setup(t)
	for _, mutate := range []func(*Config){func(c *Config) { c.Hazards.Service = "other-reader" }, func(c *Config) { c.Hazards.ExpectedGeneration++ }, func(c *Config) { c.Hazards.RunID = "other-run" }, func(c *Config) { c.Hazards.Source = hazardreader.Source(c.People.Source) }} {
		c := f.cfg
		mutate(&c)
		if s, err := New(c, api); err == nil {
			s.Close()
			t.Fatal("inconsistent mounted scope accepted")
		}
	}
}

func TestHTTPExistingReadAuthorizationAndNoWriteRoutes(t *testing.T) {
	f, api := setup(t)
	rootPublic, root, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	public, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := f.w.Earliest
	rev, err := authority.SignRevocations(authority.Revocations{Version: 1, Tenant: f.cfg.People.Tenant, Site: f.cfg.People.Site, Domain: "simulation", Revision: 1, IssuedAtUnixMS: now.Add(-time.Second).UnixMilli(), ExpiresAtUnixMS: now.Add(time.Hour - time.Second).UnixMilli()}, root)
	if err != nil {
		t.Fatal(err)
	}
	c := HTTPConfig{Version: 1, Listen: "127.0.0.1:8080", AllowInsecureSimulation: true, Audience: "native-viewer", RootPublicKey: rootPublic, Revocations: rev, Reader: f.cfg}
	b, err := newHTTP(c, api, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	grant, err := authority.SignGrant(authority.Grant{Version: 1, ID: "viewer-read", Tenant: f.cfg.People.Tenant, Site: f.cfg.People.Site, Domain: "simulation", Principal: "viewer", Kind: "app", Source: world.Source{Device: "operator", App: "viewer", Service: "http", Stream: "read"}, PublicKey: public, Scopes: []string{"read"}, NotBeforeUnixMS: now.Add(-time.Minute).UnixMilli(), NotAfterUnixMS: now.Add(time.Hour).UnixMilli()}, root)
	if err != nil {
		t.Fatal(err)
	}
	unsigned := httptest.NewRequest(http.MethodGet, "/v1/components", nil)
	response := httptest.NewRecorder()
	b.Handler.ServeHTTP(response, unsigned)
	if response.Code != http.StatusUnauthorized || f.queries != 0 {
		t.Fatal("unauthenticated read reached native socket")
	}
	request, err := authority.SignRequest(grant, key, c.Audience, "read", nil, "viewer_nonce_123456789", now, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodGet, "/v1/components", nil)
	r.Header.Set(node.AuthorizationHeader, base64.StdEncoding.EncodeToString(raw))
	response = httptest.NewRecorder()
	b.Handler.ServeHTTP(response, r)
	var snapshot client.Snapshot
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &snapshot) != nil || len(snapshot.Components) != 3 || len(snapshot.Cursor) != 64 {
		t.Fatalf("authenticated HTTP read: %d %s", response.Code, response.Body.String())
	}
	for _, route := range []string{"/v1/evidence/get", "/v1/tasks", "/v1/sites", "/v1/task-relay", "/v1/watch"} {
		res := httptest.NewRecorder()
		b.Handler.ServeHTTP(res, httptest.NewRequest(http.MethodGet, route, nil))
		if res.Code != http.StatusNotFound {
			t.Fatal("unexpected extra route", route, res.Code)
		}
	}
	res := httptest.NewRecorder()
	b.Handler.ServeHTTP(res, httptest.NewRequest(http.MethodPost, "/v1/components", nil))
	if res.Code != http.StatusMethodNotAllowed {
		t.Fatal("write route exposed", res.Code)
	}
}
