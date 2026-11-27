package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/client"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

func testPublisher(t *testing.T, url string) (*publisher, world.Component) {
	t.Helper()
	pub, key, _ := ed25519.GenerateKey(rand.Reader)
	_, root, _ := ed25519.GenerateKey(rand.Reader)
	now := time.Now()
	source := world.Source{Device: "sensor", App: "gas", Service: "gas", Stream: "ppm"}
	g, e := authority.SignGrant(authority.Grant{Version: 1, PublishAudience: "node", ID: "test", Tenant: "lab", Site: "site", Domain: "simulation", Principal: "sensor", Kind: "app", Source: source, PublicKey: pub, Scopes: []string{"publish"}, NotBeforeUnixMS: now.Add(-time.Minute).UnixMilli(), NotAfterUnixMS: now.Add(time.Hour).UnixMilli()}, root)
	if e != nil {
		t.Fatal(e)
	}
	p, e := newPublisher(client.Config{URL: url, Audience: "node", Grant: g, PrivateKey: key, AllowInsecureSimulation: true})
	if e != nil {
		t.Fatal(e)
	}
	sim := world.SimulationContext{RunID: "test", ScenarioVersion: "v1", ClockDomain: "simulation:test", Seed: 31}
	c := world.Component{Key: world.Key{Tenant: "lab", Site: "site", Entity: "sensor", Type: "test", Source: source}, Version: world.Version{Epoch: 1, Sequence: 1}, Frame: world.FrameRef{ID: "floor", Revision: "1"}, Timing: world.Timing{SampleTime: now, ClockDomain: sim.ClockDomain, ErrorKnown: true}, TTL: 4 * time.Second, Provenance: world.Provenance{Origin: world.Simulated, AppVersion: "generator", Simulation: &sim}, Payload: json.RawMessage(`{}`)}
	return p, c
}
func TestUncertainPublicationRetriesExactSignedBytes(t *testing.T) {
	var mu sync.Mutex
	var bodies [][]byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		defer mu.Unlock()
		bodies = append(bodies, raw)
		if len(bodies) == 1 {
			http.Error(w, "uncertain", 503)
			return
		}
		w.Write([]byte(`{"accepted":true}`))
	}))
	defer server.Close()
	p, c := testPublisher(t, server.URL)
	pending, e := p.prepare(c)
	if e != nil {
		t.Fatal(e)
	}
	p.pending = pending
	if e = p.retry(context.Background()); e == nil || p.pending != pending {
		t.Fatal("uncertain POST lost its exact candidate")
	}
	if e = p.retry(context.Background()); e != nil || p.pending != nil || p.last != pending {
		t.Fatal(e)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != 2 || !bytes.Equal(bodies[0], bodies[1]) {
		t.Fatal("retry changed signed evidence")
	}
}
func TestExpiredRetryDoesNotClaimPublication(t *testing.T) {
	p, c := testPublisher(t, "http://127.0.0.1:1")
	c.Timing.SampleTime = time.Now().Add(-5 * time.Second)
	pending, e := p.prepare(c)
	if e != nil {
		t.Fatal(e)
	}
	p.pending = pending
	if e = p.retry(context.Background()); e == nil || p.pending != nil || p.last != nil {
		t.Fatalf("expired input reported success: %v", e)
	}
}
func TestDurableEpochIsAllocatedOncePerProcess(t *testing.T) {
	var mu sync.Mutex
	count := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		count++
		mu.Unlock()
		if r.URL.Path != "/v1/publishers/epoch" {
			t.Errorf("wrong operation %s", r.URL.Path)
		}
		w.Write([]byte(`{"epoch":17}`))
	}))
	defer server.Close()
	p, _ := testPublisher(t, server.URL)
	for seq := uint64(1); seq <= 2; seq++ {
		v, e := p.version(context.Background())
		if e != nil || v.Epoch != 17 || v.Sequence != seq {
			t.Fatal(v, e)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if count != 1 {
		t.Fatal("epoch renewed within live process")
	}
}
func TestUncertainWithdrawalKeepsLongestOriginalLifetime(t *testing.T) {
	_, long := testPublisher(t, "http://127.0.0.1:1")
	r := new(runtime)
	r.rememberPossible(long)
	shorter := long
	shorter.Version.Sequence++
	shorter.TTL = time.Second
	r.rememberPossible(shorter)
	if r.possible.TTL != long.TTL || r.possible.Timing.SampleTime != long.Timing.SampleTime {
		t.Fatal("shorter uncertain replacement erased prior withdrawal obligation")
	}
}
