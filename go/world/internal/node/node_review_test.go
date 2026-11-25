package node

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/store"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

func reviewCredentials(t *testing.T) Credentials {
	t.Helper()
	_, root, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	grant, err := authority.SignGrant(authority.Grant{Version: authority.Version, ID: "review", Tenant: "tenant", Site: "site", Domain: "simulation", Principal: "review", Kind: "device", Source: world.Source{Device: "device", App: "world", Service: "replica", Stream: "state"}, PublicKey: pub, Scopes: []string{"replicate"}, NotBeforeUnixMS: now.Add(-time.Second).UnixMilli(), NotAfterUnixMS: now.Add(time.Hour).UnixMilli()}, root)
	if err != nil {
		t.Fatal(err)
	}
	return Credentials{Grant: grant, PrivateKey: key}
}

func TestReviewPublisherHasOneEpochAuthorityButRecordsCanRelay(t *testing.T) {
	root, rootKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	source := world.Source{Device: "gateway", App: "sensors", Service: "publisher", Stream: "gas"}
	grant, err := authority.SignGrant(authority.Grant{Version: authority.Version, ID: "publisher", Tenant: "tenant", Site: "site", Domain: "simulation", Principal: "publisher", Kind: "app", Source: source, PublicKey: pub, Scopes: []string{"publish", "read", "replicate"}, PublishAudience: "node-1", NotBeforeUnixMS: now.Add(-time.Second).UnixMilli(), NotAfterUnixMS: now.Add(30 * time.Minute).UnixMilli()}, rootKey)
	if err != nil {
		t.Fatal(err)
	}
	credentials := Credentials{Grant: grant, PrivateKey: key}
	rev, err := authority.SignRevocations(authority.Revocations{Version: authority.Version, Tenant: "tenant", Site: "site", Domain: "simulation", Revision: 1, IssuedAtUnixMS: now.Add(-time.Second).UnixMilli(), ExpiresAtUnixMS: now.Add(30 * time.Minute).UnixMilli()}, rootKey)
	if err != nil {
		t.Fatal(err)
	}
	newNode := func(audience string) (*Node, *httptest.Server) {
		dir := t.TempDir()
		s, err := store.Open(filepath.Join(dir, "world"), world.Config{Tenant: "tenant", Site: "site", ClockDomain: "simulation:review", Now: time.Now, MaxEntries: 32, AllowSynthetic: true, Frames: []world.FrameRef{{ID: "floor", Revision: "1"}}})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = s.Close() })
		v, err := authority.NewVerifier(authority.Config{RootPublicKey: root, Tenant: "tenant", Site: "site", Domain: "simulation", Audience: audience, Now: time.Now, Replay: authority.NewMemoryReplay(100)})
		if err != nil {
			t.Fatal(err)
		}
		if err = v.UpdateRevocations(rev); err != nil {
			t.Fatal(err)
		}
		n, err := New(Config{Tenant: "tenant", Site: "site", ProofPath: filepath.Join(dir, "proofs"), MaxRecords: 32}, s, v)
		if err != nil {
			t.Fatal(err)
		}
		server := httptest.NewServer(n.Handler())
		t.Cleanup(server.Close)
		return n, server
	}
	n1, s1 := newNode("node-1")
	n2, s2 := newNode("node-2")
	post := func(server *httptest.Server, audience, path string, body []byte) int {
		req, err := http.NewRequest(http.MethodPost, server.URL+path, bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		if err = AuthorizeRequest(req, credentials, audience, "publish", body); err != nil {
			t.Fatal(err)
		}
		response, err := server.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()
		return response.StatusCode
	}
	if got := post(s1, "node-1", "/v1/publishers/epoch", []byte(`{}`)); got != http.StatusOK {
		t.Fatal("owning node rejected epoch", got)
	}
	if got := post(s2, "node-2", "/v1/publishers/epoch", []byte(`{}`)); got == http.StatusOK {
		t.Fatal("publisher allocated independent epoch at another node")
	}
	c := world.Component{Key: world.Key{Tenant: "tenant", Site: "site", Entity: "sensor", Type: "gas", Source: source}, Version: world.Version{Epoch: 1, Sequence: 1}, Frame: world.FrameRef{ID: "floor", Revision: "1"}, Timing: world.Timing{SampleTime: now, ClockDomain: "simulation:review", ErrorKnown: true}, TTL: time.Minute, Provenance: world.Provenance{Origin: world.Simulated, AppVersion: "review", Simulation: &world.SimulationContext{RunID: "review", ScenarioVersion: "1", ClockDomain: "simulation:review"}}, Payload: json.RawMessage(`{"ppm":12}`)}
	proof, err := authority.SignRecord(grant, key, c, now)
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(proof)
	if err != nil {
		t.Fatal(err)
	}
	if got := post(s1, "node-1", "/v1/components", body); got != http.StatusOK {
		t.Fatal("owning publisher rejected", got)
	}
	if err = n2.Sync(context.Background(), Peer{URL: s1.URL, Audience: "node-1"}, credentials); err != nil {
		t.Fatal("record could not relay", err)
	}
	for _, n := range []*Node{n1, n2} {
		snapshot, err := n.query()
		if err != nil || len(snapshot.Components) != 1 {
			t.Fatal(snapshot, err)
		}
		if snapshot.Components[0].Key.Source != source {
			t.Fatal("relay replaced source")
		}
	}
}

func TestReviewPeerSyncDoesNotForwardCredentialsAcrossRedirect(t *testing.T) {
	var forwarded atomic.Bool
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		forwarded.Store(r.Header.Get(AuthorizationHeader) != "")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"records":[]}`))
	}))
	defer other.Close()
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL, http.StatusTemporaryRedirect)
	}))
	defer peer.Close()
	n := &Node{cfg: Config{MaxRecords: 10}, client: newPeerClient()}
	err := n.Sync(context.Background(), Peer{URL: peer.URL, Audience: "peer"}, reviewCredentials(t))
	if forwarded.Load() {
		t.Fatal("signed authorization forwarded to redirect target")
	}
	if err == nil {
		t.Fatal("peer redirect accepted")
	}
}

func TestReviewAcknowledgedRevocationsRecoverWithOldConfig(t *testing.T) {
	dir := t.TempDir()
	root, rootKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	rev1, err := authority.SignRevocations(authority.Revocations{Version: authority.Version, Tenant: "tenant", Site: "site", Domain: "simulation", Revision: 1, IssuedAtUnixMS: now.Add(-time.Second).UnixMilli(), ExpiresAtUnixMS: now.Add(30 * time.Minute).UnixMilli()}, rootKey)
	if err != nil {
		t.Fatal(err)
	}
	replayPath := filepath.Join(dir, "replay")
	revocationPath := filepath.Join(dir, "revocations")
	replay, err := authority.OpenReplay(replayPath, 100)
	if err != nil {
		t.Fatal(err)
	}
	newVerifier := func(r authority.ReplayStore) *authority.Verifier {
		v, e := authority.NewVerifier(authority.Config{RootPublicKey: root, Tenant: "tenant", Site: "site", Domain: "simulation", Audience: "node", Now: time.Now, Replay: r})
		if e != nil {
			t.Fatal(e)
		}
		return v
	}
	v := newVerifier(replay)
	if err = v.UpdateRevocations(rev1); err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(filepath.Join(dir, "state"), world.Config{Tenant: "tenant", Site: "site", ClockDomain: "simulation:review", Now: time.Now, MaxEntries: 10, AllowSynthetic: true})
	if err != nil {
		t.Fatal(err)
	}
	n, err := New(Config{Tenant: "tenant", Site: "site", ProofPath: filepath.Join(dir, "proofs"), RevocationPath: revocationPath, MaxRecords: 10}, s, v)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(n.Handler())
	r2 := rev1.Revocations
	r2.Revision = 2
	r2.Principals = []string{"revoked-operator"}
	rev2, err := authority.SignRevocations(r2, rootKey)
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(rev2)
	if err != nil {
		t.Fatal(err)
	}
	response, err := server.Client().Post(server.URL+"/v1/revocations", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, response.Body)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatal("update not acknowledged", response.StatusCode)
	}
	server.Close()
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	if err = replay.Close(); err != nil {
		t.Fatal(err)
	}
	replay, err = authority.OpenReplay(replayPath, 100)
	if err != nil {
		t.Fatal(err)
	}
	defer replay.Close()
	loaded, err := LoadRevocations(revocationPath, rev1)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Revocations.Revision != 2 || len(loaded.Revocations.Principals) != 1 {
		t.Fatal("acknowledged signed snapshot lost", loaded)
	}
	v = newVerifier(replay)
	if err = v.UpdateRevocations(loaded); err != nil {
		t.Fatal("could not restart with old bootstrap config", err)
	}
	if err = v.UpdateRevocations(rev1); err == nil {
		t.Fatal("rollback accepted after restart")
	}
}
