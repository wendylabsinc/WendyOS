package readservice

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/world"
	"github.com/wendylabsinc/wendy/go/world/production/admission"
	"github.com/wendylabsinc/wendy/go/world/production/authorizer"
	"github.com/wendylabsinc/wendy/go/world/production/internal/testidentity"
	"github.com/wendylabsinc/wendy/go/world/production/jws"
	"github.com/wendylabsinc/wendy/go/world/production/request"
	"github.com/wendylabsinc/wendy/go/world/production/trustedtime"
	"github.com/wendylabsinc/wendy/go/world/production/wendycert"
)

type fixedClock struct{ w trustedtime.Interval }

func (c fixedClock) Now() (trustedtime.Interval, error) { return c.w, nil }

type readerFunc func(context.Context, Query) (Snapshot, error)

func (f readerFunc) Query(c context.Context, q Query) (Snapshot, error) { return f(c, q) }

type env struct {
	t        *testing.T
	now      time.Time
	identity *testidentity.Fixture
	auth     *authorizer.Authorizer
	ac       authorizer.Config
	path     string
	cfg      Config
	snapshot Snapshot
	calls    atomic.Int64
}

func setup(t *testing.T) *env {
	t.Helper()
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	e := &env{t: t, now: now, path: filepath.Join(t.TempDir(), "ledger.json")}
	e.identity = testidentity.New(t, now)
	policy := testidentity.NewWith(t, now, "spiffe://wendy.sh/tenant/"+testidentity.Tenant+"/operator/policy")
	revoker := testidentity.NewWith(t, now, "spiffe://wendy.sh/tenant/"+testidentity.Tenant+"/operator/revoker")
	v, err := wendycert.New(wendycert.Config{Tenant: testidentity.Tenant, Roots: [][]byte{e.identity.RootDER, policy.RootDER, revoker.RootDER}})
	if err != nil {
		t.Fatal(err)
	}
	clock := fixedClock{trustedtime.Interval{Earliest: now.Add(-time.Millisecond), Latest: now.Add(time.Millisecond), BootID: "test-boot", Monotonic: time.Second, Domain: "physical", Authenticated: true, Source: "test-injection-no-time-proof"}}
	pin := func(f *testidentity.Fixture) admission.Pin {
		id, err := v.Verify(f.ChainDER, clock.w.Earliest, clock.w.Latest)
		if err != nil {
			t.Fatal(err)
		}
		return admission.Pin{Principal: id.Principal, SigningKeySHA256: id.SigningKeySHA256}
	}
	e.ac = authorizer.Config{Clock: clock, Admission: admission.Config{Tenant: testidentity.Tenant, Site: "office", Audience: "gateway/world", Certificates: v, PolicyIssuer: pin(policy), RevocationIssuer: pin(revoker), MaxRevocationAge: 10 * time.Minute}}
	e.auth, err = authorizer.Open(e.path, e.ac)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if e.auth != nil {
			_ = e.auth.Close()
		}
	})
	p := admission.Policy{Version: 1, Domain: "physical", Tenant: testidentity.Tenant, Site: "office", Audience: "gateway/world", IssuerPrincipal: policy.Principal, IssuerKeySHA256: e.ac.Admission.PolicyIssuer.SigningKeySHA256, PolicyID: "read-policy", Revision: 1, IssuedAtUnixMS: now.Add(-time.Second).UnixMilli(), ExpiresAtUnixMS: now.Add(time.Hour).UnixMilli(), Entries: []admission.PolicyEntry{{Principal: e.identity.Principal, Scopes: []string{admission.ReadScope}}}}
	r := admission.Revocations{Version: 1, Domain: "physical", Tenant: p.Tenant, Site: p.Site, Audience: p.Audience, IssuerPrincipal: revoker.Principal, IssuerKeySHA256: e.ac.Admission.RevocationIssuer.SigningKeySHA256, Revision: 1, IssuedAtUnixMS: p.IssuedAtUnixMS, ExpiresAtUnixMS: now.Add(time.Minute).UnixMilli(), RevokedPrincipals: []string{}, RevokedKeySHA256: []string{}, RevokedCertificateSHA256: []string{}, RevokedPolicyIDs: []string{}}
	sign := func(typ string, value any, f *testidentity.Fixture) string {
		b, _ := json.Marshal(value)
		s, err := jws.Sign(typ, b, f.ChainDER, f.Key)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	if err = e.auth.ApplyPolicy(sign(admission.PolicyTyp, p, policy)); err != nil {
		t.Fatal(err)
	}
	if err = e.auth.ApplyRevocations(sign(admission.RevocationsTyp, r, revoker)); err != nil {
		t.Fatal(err)
	}
	dataset := Dataset{ID: "office-simulation", Domain: "simulation", SourceTenant: "lab", SourceSite: "virtual-office", Frame: FrameRef{ID: "office-floor", Revision: "survey-v1"}, RunID: "test-run"}
	component := world.Component{Key: world.Key{Tenant: "lab", Site: dataset.SourceSite, Entity: "gas-sensor", Type: "wendy.gas.v1", Source: world.Source{Device: "sim-gateway", App: "gas", Service: "sensor", Stream: "gas"}}, Version: world.Version{Epoch: 1, Sequence: 1}, Frame: world.FrameRef{ID: dataset.Frame.ID, Revision: dataset.Frame.Revision}, Timing: world.Timing{SampleTime: now, ClockDomain: "simulation:test-run", ErrorKnown: true}, TTL: time.Minute, Provenance: world.Provenance{Origin: world.Simulated, AppVersion: "1", Simulation: &world.SimulationContext{RunID: dataset.RunID, ScenarioVersion: "scenario-v2", ClockDomain: "simulation:test-run", Seed: 7}}, Payload: json.RawMessage(`{"value":4.2,"unit":"ppm"}`)}
	raw, _ := json.Marshal(component)
	e.snapshot = Snapshot{Dataset: dataset, Cursor: strings.Repeat("a", 64), Components: []json.RawMessage{raw}}
	if err := validateSnapshot(e.snapshot, dataset, 16); err != nil {
		t.Fatal(err)
	}
	e.cfg = Config{Tenant: p.Tenant, Site: p.Site, Audience: p.Audience, Dataset: dataset, Authorizer: e.auth, Reader: readerFunc(func(ctx context.Context, q Query) (Snapshot, error) { e.calls.Add(1); return e.snapshot, nil })}
	return e
}
func (e *env) body() []byte {
	b, _ := json.Marshal(Query{Version: 1, DatasetID: e.cfg.Dataset.ID, Limit: 16})
	return b
}
func (e *env) envelope(body []byte) []byte {
	e.t.Helper()
	nonce := make([]byte, 24)
	if _, err := rand.Read(nonce); err != nil {
		e.t.Fatal(err)
	}
	digest, err := request.BodyDigest(body)
	if err != nil {
		e.t.Fatal(err)
	}
	d := request.Descriptor{Version: 1, Domain: "physical", Tenant: e.cfg.Tenant, Site: e.cfg.Site, Audience: e.cfg.Audience, Principal: e.identity.Principal, Operation: request.OperationRead, Method: request.Method, Path: request.Path, BodySHA256: digest, PolicyRevision: 1, IssuedAtUnixMS: e.now.Add(-time.Second).UnixMilli(), ExpiresAtUnixMS: e.now.Add(30 * time.Second).UnixMilli(), Nonce: base64.RawURLEncoding.EncodeToString(nonce)}
	proof, err := request.Sign(d, body, e.identity.ChainDER, e.identity.Key)
	if err != nil {
		e.t.Fatal(err)
	}
	b, _ := json.Marshal(Envelope{Proof: proof, BodyBase64: base64.StdEncoding.EncodeToString(body)})
	return b
}
func (e *env) server() *httptest.Server {
	e.t.Helper()
	h, err := New(e.cfg)
	if err != nil {
		e.t.Fatal(err)
	}
	s := httptest.NewUnstartedServer(h)
	s.TLS = &tls.Config{MinVersion: tls.VersionTLS13}
	s.StartTLS()
	e.t.Cleanup(s.Close)
	return s
}
func send(t *testing.T, s *httptest.Server, method, path string, body []byte) (int, []byte) {
	t.Helper()
	r, err := http.NewRequest(method, s.URL+path, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Content-Type", "application/json")
	c := s.Client()
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := c.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, b
}
func TestTLSReadPreservesSimulationAndDurableReplay(t *testing.T) {
	e := setup(t)
	s := e.server()
	wire := e.envelope(e.body())
	status, b := send(t, s, "POST", request.Path, wire)
	if status != 200 {
		t.Fatalf("%d %s", status, b)
	}
	var response Response
	if err := json.Unmarshal(b, &response); err != nil {
		t.Fatal(err)
	}
	if response.Authority.Domain != "physical" || response.Authority.Tenant == response.Dataset.SourceTenant || response.Dataset != e.snapshot.Dataset || !bytes.Equal(response.Components[0], e.snapshot.Components[0]) {
		t.Fatalf("origin changed: %s", b)
	}
	if status, _ = send(t, s, "POST", request.Path, wire); status != 403 {
		t.Fatal(status)
	}
	s.Close()
	if err := e.auth.Close(); err != nil {
		t.Fatal(err)
	}
	var err error
	e.auth, err = authorizer.Open(e.path, e.ac)
	if err != nil {
		t.Fatal(err)
	}
	e.cfg.Authorizer = e.auth
	s = e.server()
	if status, _ = send(t, s, "POST", request.Path, wire); status != 403 {
		t.Fatal(status)
	}
	if e.calls.Load() != 1 {
		t.Fatal("replay reached backend")
	}
	if status, b = send(t, s, "POST", request.Path, e.envelope(e.body())); status != 200 {
		t.Fatalf("fresh proof after restart: %d %s", status, b)
	}
}
func TestPreauthorizationRejectsAmbiguousRequestsWithoutSpendingNonce(t *testing.T) {
	e := setup(t)
	s := e.server()
	wire := e.envelope(e.body())
	cases := []struct {
		name, method, path string
		body               []byte
		want               int
	}{
		{"method", "GET", request.Path, wire, 405}, {"query", "POST", request.Path + "?x=1", wire, 404}, {"empty-query", "POST", request.Path + "?", wire, 404}, {"escaped", "POST", "/v1/world/%71uery", wire, 404}, {"slash", "POST", request.Path + "/", wire, 404}, {"dot", "POST", "/v1/world/../world/query", wire, 404}, {"duplicate", "POST", request.Path, append([]byte(`{"proof":"x",`), wire[1:]...), 400}, {"unknown", "POST", request.Path, []byte(`{"Proof":"x","body_base64":"e30="}`), 400}, {"huge", "POST", request.Path, bytes.Repeat([]byte("x"), MaxEnvelopeBytes+1), 413},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, b := send(t, s, tc.method, tc.path, tc.body)
			if status != tc.want {
				t.Fatalf("%d %s", status, b)
			}
		})
	}
	if e.calls.Load() != 0 {
		t.Fatal("invalid route reached backend")
	}
	if status, b := send(t, s, "POST", request.Path, wire); status != 200 {
		t.Fatalf("nonce spent before validation: %d %s", status, b)
	}
}
func TestQuerySchemaAndExactBodyBinding(t *testing.T) {
	e := setup(t)
	s := e.server()
	for _, body := range []string{`{"version":1,"dataset_id":"office-simulation","limit":1,"limit":2}`, `{"version":1,"dataset_id":"other","limit":1}`, `{"version":1,"dataset_id":"office-simulation","limit":257}`, `{"Version":1,"dataset_id":"office-simulation","limit":1}`, `{"version":1,"dataset_id":"office-simulation","limit":1,"url":"http://evil"}`} {
		// Sign a valid body and substitute malformed query; syntax refusal precedes proof verification.
		var envelope Envelope
		_ = json.Unmarshal(e.envelope(e.body()), &envelope)
		envelope.BodyBase64 = base64.StdEncoding.EncodeToString([]byte(body))
		wire, _ := json.Marshal(envelope)
		if status, _ := send(t, s, "POST", request.Path, wire); status != 400 {
			t.Fatal(status)
		}
	}
	var envelope Envelope
	_ = json.Unmarshal(e.envelope(e.body()), &envelope)
	envelope.BodyBase64 = base64.StdEncoding.EncodeToString(append(e.body(), ' '))
	wire, _ := json.Marshal(envelope)
	if status, _ := send(t, s, "POST", request.Path, wire); status != 403 {
		t.Fatal(status)
	}
	if e.calls.Load() != 0 {
		t.Fatal("invalid proof reached backend")
	}
}
func TestSnapshotRejectsScopeAndOriginChanges(t *testing.T) {
	e := setup(t)
	original := e.snapshot
	s := e.server()
	cases := map[string]func(*Snapshot){"dataset": func(s *Snapshot) { s.Dataset.Domain = "physical" }, "cursor": func(s *Snapshot) { s.Cursor = "" }, "nil": func(s *Snapshot) { s.Components = nil }, "duplicate": func(s *Snapshot) { s.Components = append(s.Components, s.Components[0]) }}
	for _, field := range []string{"tenant", "frame", "run", "origin", "unknown-clock", "tombstone", "mixed"} {
		field := field
		cases[field] = func(s *Snapshot) {
			var c world.Component
			_ = json.Unmarshal(s.Components[0], &c)
			switch field {
			case "tenant":
				c.Key.Tenant = e.cfg.Tenant
			case "frame":
				c.Frame.Revision = "other"
			case "run":
				c.Provenance.Simulation.RunID = "other"
			case "origin":
				c.Provenance.Origin = world.Measured
				c.Provenance.Simulation = nil
			case "unknown-clock":
				c.Timing.ErrorKnown = false
			case "tombstone":
				c.Tombstone = true
			case "mixed":
				c.Provenance = world.Provenance{Origin: world.Inferred, AppVersion: "1", ModelID: "m", ModelVersion: "1", Inputs: []world.Input{{ID: "physical", Origin: world.Measured}}}
			}
			b, _ := json.Marshal(c)
			s.Components = []json.RawMessage{b}
		}
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			e.snapshot = original
			e.snapshot.Components = append([]json.RawMessage{}, original.Components...)
			change(&e.snapshot)
			status, b := send(t, s, "POST", request.Path, e.envelope(e.body()))
			if status != 503 || bytes.Contains(b, []byte("gas-sensor")) {
				t.Fatalf("%d %s", status, b)
			}
		})
	}
}
func TestTransportAndBudget(t *testing.T) {
	e := setup(t)
	h, err := New(e.cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, version := range []uint16{0, tls.VersionTLS12} {
		r := httptest.NewRequest("POST", request.Path, bytes.NewReader(e.envelope(e.body())))
		if version != 0 {
			r.TLS = &tls.ConnectionState{Version: version}
		}
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 426 {
			t.Fatal(w.Code)
		}
	}
	e.cfg.MaxResponseBytes = 1024
	s := e.server()
	var c world.Component
	_ = json.Unmarshal(e.snapshot.Components[0], &c)
	c.Payload = json.RawMessage(`{"value":"` + strings.Repeat("a", 2000) + `"}`)
	e.snapshot.Components[0], _ = json.Marshal(c)
	status, b := send(t, s, "POST", request.Path, e.envelope(e.body()))
	if status != 503 || bytes.Contains(b, []byte("gas-sensor")) {
		t.Fatalf("%d %s", status, b)
	}
}
func TestConcurrentReaderLimits(t *testing.T) {
	for _, global := range []bool{false, true} {
		t.Run(map[bool]string{false: "principal", true: "global"}[global], func(t *testing.T) {
			e := setup(t)
			entered := make(chan struct{}, 1)
			release := make(chan struct{})
			defer close(release)
			e.cfg.MaxConcurrent = 2
			e.cfg.MaxPerPrincipal = 1
			if global {
				e.cfg.MaxConcurrent = 1
			}
			e.cfg.Reader = readerFunc(func(ctx context.Context, q Query) (Snapshot, error) {
				entered <- struct{}{}
				select {
				case <-release:
					return e.snapshot, nil
				case <-ctx.Done():
					return Snapshot{}, ctx.Err()
				}
			})
			s := e.server()
			first := e.envelope(e.body())
			done := make(chan int, 1)
			go func() { status, _ := send(t, s, "POST", request.Path, first); done <- status }()
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("reader not entered")
			}
			if status, _ := send(t, s, "POST", request.Path, e.envelope(e.body())); status != 429 {
				t.Fatal(status)
			}
			release <- struct{}{}
			select {
			case status := <-done:
				if status != 200 {
					t.Fatal(status)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("reader stuck")
			}
		})
	}
}
func TestBackendCancellation(t *testing.T) {
	e := setup(t)
	entered := make(chan time.Duration, 1)
	cancelled := make(chan struct{})
	e.cfg.Reader = readerFunc(func(ctx context.Context, q Query) (Snapshot, error) {
		deadline, ok := ctx.Deadline()
		remaining := time.Duration(-1)
		if ok {
			remaining = time.Until(deadline)
		}
		entered <- remaining
		<-ctx.Done()
		close(cancelled)
		return Snapshot{}, ctx.Err()
	})
	h, err := New(e.cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	r := httptest.NewRequest("POST", request.Path, bytes.NewReader(e.envelope(e.body()))).WithContext(ctx)
	r.TLS = &tls.ConnectionState{Version: tls.VersionTLS13}
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { defer close(done); h.ServeHTTP(w, r) }()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("handler failed to exit after cancellation")
		}
	}()
	select {
	case remaining := <-entered:
		if remaining <= 0 || remaining > 5*time.Second {
			t.Fatalf("backend context lacks a bounded live deadline: %s", remaining)
		}
	case <-done:
		t.Fatal("handler exited before backend entry")
	case <-time.After(10 * time.Second):
		t.Fatal("backend was not entered")
	}
	// Authorization has completed. Cancellation now tests the Reader contract,
	// independently of how long ML-DSA verification or durable admission took.
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("handler did not finish")
	}
	if w.Code != 504 {
		t.Fatal(w.Code)
	}
	select {
	case <-cancelled:
	default:
		t.Fatal("backend context was not cancelled")
	}
}

func TestSupportedSimulationProvenance(t *testing.T) {
	e := setup(t)
	var c world.Component
	if err := json.Unmarshal(e.snapshot.Components[0], &c); err != nil {
		t.Fatal(err)
	}
	simulation := c.Provenance.Simulation
	for _, origin := range []world.Origin{world.Authored, world.Inferred} {
		c.Provenance = world.Provenance{Origin: origin, AppVersion: "1"}
		if origin == world.Authored {
			c.Provenance.Simulation = simulation
		} else {
			c.Provenance.ModelID = "fusion"
			c.Provenance.ModelVersion = "1"
			c.Provenance.Inputs = []world.Input{{ID: "input", Origin: world.Simulated, Simulation: simulation}}
		}
		b, _ := json.Marshal(c)
		snapshot := e.snapshot
		snapshot.Components = []json.RawMessage{b}
		if err := validateSnapshot(snapshot, e.cfg.Dataset, 16); err != nil {
			t.Fatalf("%s: %v", origin, err)
		}
	}
}
func TestInvalidLaterRecordDoesNotLeakEarlierRecord(t *testing.T) {
	e := setup(t)
	e.snapshot.Components = append(e.snapshot.Components, json.RawMessage(`{"unexpected":true}`))
	s := e.server()
	status, body := send(t, s, "POST", request.Path, e.envelope(e.body()))
	if status != 503 || bytes.Contains(body, []byte("gas-sensor")) {
		t.Fatalf("%d %s", status, body)
	}
}
