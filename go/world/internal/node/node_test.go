package node

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/store"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

type fixture struct {
	n           *Node
	s           *store.Store
	v           *authority.Verifier
	cfg         Config
	wc          world.Config
	root        ed25519.PrivateKey
	credentials Credentials
	audience    string
	server      *httptest.Server
	unavailable atomic.Bool
	requestNow  func() time.Time // Optional clock for deterministic HTTP qualifications.
}

func newFixture(t *testing.T, root ed25519.PrivateKey, name string) *fixture {
	t.Helper()
	if root == nil {
		_, key, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		root = key
	}
	dir := t.TempDir()
	f := &fixture{root: root, audience: name, cfg: Config{Tenant: "tenant", Site: "office", ProofPath: filepath.Join(dir, "proofs.json"), MaxRecords: 64}, wc: world.Config{Tenant: "tenant", Site: "office", ClockDomain: "simulation:live", Now: time.Now, MaxEntries: 64, MaxFutureSkew: time.Second, AllowSynthetic: true, Frames: []world.FrameRef{{ID: "floor", Revision: "1"}}}}
	var err error
	f.s, err = store.Open(filepath.Join(dir, "store.json"), f.wc)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.s.Close() })
	replay := authority.NewMemoryReplay(1000)
	f.v, err = authority.NewVerifier(authority.Config{RootPublicKey: root.Public().(ed25519.PublicKey), Tenant: "tenant", Site: "office", Domain: "simulation", Audience: name, Now: time.Now, Replay: replay})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	rev, err := authority.SignRevocations(authority.Revocations{Version: authority.Version, Tenant: "tenant", Site: "office", Domain: "simulation", Revision: 1, IssuedAtUnixMS: now.Add(-time.Second).UnixMilli(), ExpiresAtUnixMS: now.Add(30 * time.Minute).UnixMilli()}, root)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.v.UpdateRevocations(rev); err != nil {
		t.Fatal(err)
	}
	f.credentials = issueCredentials(t, root, name)
	f.n, err = New(f.cfg, f.s, f.v)
	if err != nil {
		t.Fatal(err)
	}
	f.start(t)
	return f
}
func (f *fixture) start(t *testing.T) {
	t.Helper()
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if f.unavailable.Load() {
			http.Error(w, "partition", 503)
			return
		}
		f.n.Handler().ServeHTTP(w, r)
	}))
	t.Cleanup(f.server.Close)
}
func issueCredentials(t *testing.T, root ed25519.PrivateKey, name string) Credentials {
	t.Helper()
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	grant, err := authority.SignGrant(authority.Grant{Version: authority.Version, ID: name, PublishAudience: name, Tenant: "tenant", Site: "office", Domain: "simulation", Principal: name, Kind: "app", Source: world.Source{Device: name, App: "sensors", Service: "publisher", Stream: "observations"}, PublicKey: pub, Scopes: []string{"publish", "read", "replicate"}, NotBeforeUnixMS: now.Add(-time.Minute).UnixMilli(), NotAfterUnixMS: now.Add(time.Hour).UnixMilli()}, root)
	if err != nil {
		t.Fatal(err)
	}
	return Credentials{Grant: grant, PrivateKey: key}
}
func call(t *testing.T, f *fixture, credentials Credentials, method, path, op string, body []byte) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, f.server.URL+path, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if err = AuthorizeRequest(req, credentials, f.audience, op, body); err != nil {
		t.Fatal(err)
	}
	if f.requestNow != nil {
		encoded, err := base64.StdEncoding.DecodeString(req.Header.Get(AuthorizationHeader))
		if err != nil {
			t.Fatal(err)
		}
		var original authority.SignedRequest
		if err = authority.DecodeStrict(encoded, &original); err != nil {
			t.Fatal(err)
		}
		signed, err := authority.SignRequest(credentials.Grant, credentials.PrivateKey, f.audience, op, body, original.Nonce, f.requestNow(), 30*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		encoded, err = json.Marshal(signed)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set(AuthorizationHeader, base64.StdEncoding.EncodeToString(encoded))
	}
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return response.StatusCode, data
}
func allocate(t *testing.T, f *fixture) uint64 {
	t.Helper()
	status, b := call(t, f, f.credentials, http.MethodPost, "/v1/publishers/epoch", "publish", []byte(`{}`))
	if status != 200 {
		t.Fatalf("epoch: %d %s", status, b)
	}
	var result struct {
		Epoch uint64 `json:"epoch"`
	}
	if err := json.Unmarshal(b, &result); err != nil {
		t.Fatal(err)
	}
	return result.Epoch
}
func component(f *fixture, epoch, sequence uint64) world.Component {
	return world.Component{Key: world.Key{Tenant: "tenant", Site: "office", Entity: "gas", Type: "wendy.gas.v0", Source: f.credentials.Grant.Grant.Source}, Version: world.Version{Epoch: epoch, Sequence: sequence}, Frame: world.FrameRef{ID: "floor", Revision: "1"}, Timing: world.Timing{SampleTime: time.Now(), ClockDomain: "simulation:live", ErrorKnown: true}, TTL: 5 * time.Minute, Provenance: world.Provenance{Origin: world.Simulated, AppVersion: "test", Simulation: &world.SimulationContext{RunID: "test", ScenarioVersion: "1", ClockDomain: "simulation:live"}}, Payload: json.RawMessage(`{"concentration":20,"unit":"ppm"}`)}
}
func signed(t *testing.T, f *fixture, c world.Component) authority.SignedRecord {
	t.Helper()
	r, err := authority.SignRecord(f.credentials.Grant, f.credentials.PrivateKey, c, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return r
}
func publish(t *testing.T, f *fixture, c world.Component) {
	t.Helper()
	b, _ := json.Marshal(signed(t, f, c))
	status, data := call(t, f, f.credentials, http.MethodPost, "/v1/components", "publish", b)
	if status != 200 {
		t.Fatalf("publish: %d %s", status, data)
	}
}
func query(t *testing.T, f *fixture) QueryResponse {
	t.Helper()
	status, b := call(t, f, f.credentials, http.MethodGet, "/v1/components", "read", nil)
	if status != 200 {
		t.Fatalf("query: %d %s", status, b)
	}
	var response QueryResponse
	if err := json.Unmarshal(b, &response); err != nil {
		t.Fatal(err)
	}
	return response
}

func TestPeerPartitionReconciliationTombstoneAndRestart(t *testing.T) {
	a := newFixture(t, nil, "a")
	b := newFixture(t, a.root, "b")
	c := newFixture(t, a.root, "c")
	epoch := allocate(t, a)
	first := component(a, epoch, 1)
	publish(t, a, first)
	if _, err := a.n.export(); err != nil {
		t.Fatalf("local export: %v", err)
	}
	if err := b.n.Sync(context.Background(), Peer{a.server.URL, "a"}, b.credentials); err != nil {
		t.Fatal(err)
	}
	if got := query(t, b).Components; len(got) != 1 || got[0].Version.Sequence != 1 {
		t.Fatal(got)
	}
	// B is a real relay: C verifies A's unchanged origin signature through B.
	if err := c.n.Sync(context.Background(), Peer{b.server.URL, "b"}, c.credentials); err != nil {
		t.Fatal(err)
	}
	if len(query(t, c).Components) != 1 {
		t.Fatal("relay lost origin proof")
	}
	a.unavailable.Store(true)
	if err := b.n.Sync(context.Background(), Peer{a.server.URL, "a"}, b.credentials); err == nil {
		t.Fatal("partition was not observed")
	}
	second := component(a, epoch, 2)
	// Local work remains available while the peer link is partitioned.
	if _, err := a.n.apply(signed(t, a, second), true); err != nil {
		t.Fatal(err)
	}
	if got := query(t, b).Components; len(got) != 1 || got[0].Version.Sequence != 1 {
		t.Fatal("partition leaked", got)
	}
	a.unavailable.Store(false)
	if err := b.n.Sync(context.Background(), Peer{a.server.URL, "a"}, b.credentials); err != nil {
		t.Fatal(err)
	}
	if got := query(t, b).Components; len(got) != 1 || got[0].Version.Sequence != 2 {
		t.Fatal(got)
	}
	tombstone := component(a, epoch, 3)
	tombstone.Tombstone = true
	tombstone.Payload = nil
	publish(t, a, tombstone)
	if err := b.n.Sync(context.Background(), Peer{a.server.URL, "a"}, b.credentials); err != nil {
		t.Fatal(err)
	}
	if len(query(t, b).Components) != 0 {
		t.Fatal("tombstone not applied")
	}
	b.server.Close()
	if err := b.s.Close(); err != nil {
		t.Fatal(err)
	}
	var err error
	b.s, err = store.Open(filepath.Join(filepath.Dir(b.cfg.ProofPath), "store.json"), b.wc)
	if err != nil {
		t.Fatal(err)
	}
	b.n, err = New(b.cfg, b.s, b.v)
	if err != nil {
		t.Fatal(err)
	}
	b.start(t)
	if _, err = b.n.apply(signed(t, a, first), false); err != nil {
		t.Fatal(err)
	}
	if len(query(t, b).Components) != 0 {
		t.Fatal("restart replay resurrected record")
	}
	if err = c.n.Sync(context.Background(), Peer{b.server.URL, "b"}, c.credentials); err != nil {
		t.Fatal(err)
	}
	if len(query(t, c).Components) != 0 {
		t.Fatal("relay did not forward retained tombstone after restart")
	}
}
func TestAuthenticationSourceTamperReplayAndEpoch(t *testing.T) {
	f := newFixture(t, nil, "a")
	epoch := allocate(t, f)
	c := component(f, epoch, 1)
	unauthorized, err := http.Get(f.server.URL + "/v1/components")
	if err != nil {
		t.Fatal(err)
	}
	unauthorized.Body.Close()
	if unauthorized.StatusCode != 401 {
		t.Fatal(unauthorized.StatusCode)
	}
	proof := signed(t, f, c)
	proof.Component.Payload = json.RawMessage(`{"concentration":999}`)
	body, _ := json.Marshal(proof)
	status, _ := call(t, f, f.credentials, http.MethodPost, "/v1/components", "publish", body)
	if status == 200 {
		t.Fatal("tampered origin accepted")
	}
	other := issueCredentials(t, f.root, "other")
	body, _ = json.Marshal(signed(t, f, c))
	status, _ = call(t, f, other, http.MethodPost, "/v1/components", "publish", body)
	if status == 200 {
		t.Fatal("source spoof", status)
	}
	publish(t, f, c)
	if allocate(t, f) <= epoch {
		t.Fatal("epoch did not advance")
	}
	old := component(f, epoch, 2)
	body, _ = json.Marshal(signed(t, f, old))
	status, _ = call(t, f, f.credentials, http.MethodPost, "/v1/components", "publish", body)
	if status == 200 {
		t.Fatal("old publisher epoch accepted")
	}
	req, _ := http.NewRequest(http.MethodGet, f.server.URL+"/v1/components", nil)
	if err = AuthorizeRequest(req, f.credentials, "a", "read", nil); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		response, err := http.DefaultClient.Do(req.Clone(context.Background()))
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if i == 0 && response.StatusCode != 200 || i == 1 && response.StatusCode != 401 {
			t.Fatalf("replay request %d status %d", i, response.StatusCode)
		}
	}
}
func TestProofWriteFailureDoesNotAdmitComponent(t *testing.T) {
	f := newFixture(t, nil, "a")
	epoch := allocate(t, f)
	if err := os.Mkdir(f.cfg.ProofPath, 0700); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(signed(t, f, component(f, epoch, 1)))
	status, _ := call(t, f, f.credentials, http.MethodPost, "/v1/components", "publish", body)
	if status == 200 {
		t.Fatal("failed proof persistence acknowledged")
	}
	records, err := f.s.Records()
	if err != nil || len(records) != 0 {
		t.Fatalf("failed proof entered replica: %v %v", records, err)
	}
}
func TestWatchObservesExpiryWithoutAnotherPublish(t *testing.T) {
	f := newFixture(t, nil, "a")
	epoch := allocate(t, f)
	c := component(f, epoch, 1)
	c.TTL = 200 * time.Millisecond
	publish(t, f, c)
	before := query(t, f)
	if len(before.Components) != 1 {
		t.Fatal("record already expired")
	}
	status, body := call(t, f, f.credentials, http.MethodGet, "/v1/watch?timeout_ms=1000&cursor="+before.Cursor, "read", nil)
	if status != 200 {
		t.Fatalf("watch: %d %s", status, body)
	}
	var after QueryResponse
	if err := json.Unmarshal(body, &after); err != nil {
		t.Fatal(err)
	}
	if len(after.Components) != 0 || after.Cursor == before.Cursor {
		t.Fatal("watch did not observe expiry")
	}
}
func TestMalformedAndOversizedRequestsRejected(t *testing.T) {
	f := newFixture(t, nil, "a")
	allocate(t, f)
	body := bytes.Repeat([]byte("x"), MaxBodyBytes+1)
	req, _ := http.NewRequest(http.MethodPost, f.server.URL+"/v1/components", bytes.NewReader(body))
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 413 {
		t.Fatal(response.StatusCode)
	}
	status, _ := call(t, f, f.credentials, http.MethodPost, "/v1/publishers/epoch", "publish", []byte(`{"unknown":true}`))
	if status != 400 {
		t.Fatal(status)
	}
	status, _ = call(t, f, f.credentials, http.MethodGet, "/v1/watch?timeout_ms=6000", "read", nil)
	if status != 400 {
		t.Fatal(status)
	}
}
