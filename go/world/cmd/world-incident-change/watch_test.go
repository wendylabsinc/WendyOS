package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"github.com/wendylabsinc/wendy/go/world/internal/gaschange"
	"github.com/wendylabsinc/wendy/go/world/internal/incidentchange"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/client"
	"github.com/wendylabsinc/wendy/go/world/internal/gasreplica"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

func watchAPI(t *testing.T, url string) *client.Client {
	t.Helper()
	pub, key, _ := ed25519.GenerateKey(rand.Reader)
	_, root, _ := ed25519.GenerateKey(rand.Reader)
	now := time.Now()
	grant, err := authority.SignGrant(authority.Grant{Version: authority.Version, ID: "watch-reader", Tenant: "lab", Site: "site", Domain: "simulation", Principal: "reader", Kind: "app", Source: world.Source{Device: "reader", App: "join", Service: "view", Stream: "state"}, PublicKey: pub, Scopes: []string{"read"}, NotBeforeUnixMS: now.Add(-time.Second).UnixMilli(), NotAfterUnixMS: now.Add(time.Minute).UnixMilli()}, root)
	if err != nil {
		t.Fatal(err)
	}
	api, err := client.New(client.Config{URL: url, Audience: "node", Grant: grant, PrivateKey: key, AllowInsecureSimulation: true})
	if err != nil {
		t.Fatal(err)
	}
	return api
}
func TestWatchImmutableHandoffAndExpiredOriginalAcrossBlockedResponse(t *testing.T) {
	captured := time.Now()
	component := world.Component{Key: world.Key{Tenant: "lab", Site: "site", Entity: "run/value"}, Timing: world.Timing{SampleTime: captured}, TTL: 500 * time.Millisecond, Payload: json.RawMessage(`{"original":true}`)}
	var calls atomic.Int64
	entered := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/watch" {
			http.NotFound(w, r)
			return
		}
		if calls.Add(1) > 1 {
			select {
			case entered <- struct{}{}:
			default:
			}
			<-r.Context().Done()
			return
		}
		json.NewEncoder(w).Encode(client.Snapshot{Components: []world.Component{component}, Cursor: strings.Repeat("a", 64)})
	}))
	defer server.Close()
	stream := startSnapshotWatch(context.Background(), watchAPI(t, server.URL), "lab", "site")
	defer stream.stop()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	first, err := stream.current(ctx)
	if err != nil {
		t.Fatal(err)
	}
	first.Components[0].Payload[2] = 'x'
	second, err := stream.current(ctx)
	if err != nil || string(second.Components[0].Payload) != `{"original":true}` {
		t.Fatal("caller mutated retained snapshot", err)
	}
	<-entered
	var elapsed atomic.Int64
	stream.mu.Lock()
	stream.received = captured
	stream.now = func() time.Time { return captured.Add(time.Duration(elapsed.Load())) }
	stream.mu.Unlock()
	elapsed.Store(int64(600 * time.Millisecond))
	old, err := stream.current(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if gasreplica.Remaining(old.Components[0], captured.Add(600*time.Millisecond)) > 0 {
		t.Fatal("blocked Watch renewed expired original")
	}
	elapsed.Store(int64(800 * time.Millisecond))
	if _, err = stream.current(ctx); err == nil {
		t.Fatal("expired authentication snapshot lease remained current")
	}

}
func TestWatchDenialDisconnectCancellationAndEmptyRestart(t *testing.T) {
	var mode atomic.Int64
	var calls atomic.Int64
	entered := make(chan struct{}, 4)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		switch mode.Load() {
		case 1:
			http.Error(w, "revoked", http.StatusUnauthorized)
			return
		case 2:
			select {
			case entered <- struct{}{}:
			default:
			}
			<-r.Context().Done()
			return
		case 3:
			http.Error(w, "disconnected", http.StatusServiceUnavailable)
			return
		}
		json.NewEncoder(w).Encode(client.Snapshot{Components: []world.Component{{Key: world.Key{Tenant: "lab", Site: "site", Entity: "old-run"}}}, Cursor: strings.Repeat("b", 64)})
	}))
	defer server.Close()
	api := watchAPI(t, server.URL)
	stream := startSnapshotWatch(context.Background(), api, "lab", "site")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := stream.current(ctx); err != nil {
		t.Fatal(err)
	}
	mode.Store(1)
	end := time.Now().Add(time.Second)
	denied := false
	for time.Now().Before(end) {
		stream.mu.Lock()
		denied = stream.failure != nil && !stream.ready && len(stream.snapshot.Components) == 0
		stream.mu.Unlock()
		if denied {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !denied {
		t.Fatal("read denial retained prior snapshot")
	}
	mode.Store(0)
	end = time.Now().Add(time.Second)
	for time.Now().Before(end) {
		stream.mu.Lock()
		ready := stream.ready
		stream.mu.Unlock()
		if ready {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	mode.Store(3)
	end = time.Now().Add(time.Second)
	for time.Now().Before(end) {
		stream.mu.Lock()
		failed := stream.failure != nil && !stream.ready
		stream.mu.Unlock()
		if failed {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, err := stream.current(ctx); err == nil {
		t.Fatal("disconnect retained current snapshot")
	}
	mode.Store(2)
	<-entered
	start := time.Now()
	stream.stop()
	if time.Since(start) > 300*time.Millisecond {
		t.Fatal("Watch cancellation did not stop bounded request")
	}
	if _, err := stream.current(ctx); err == nil {
		t.Fatal("cancelled Watch returned prior snapshot")
	}
	restarted := startSnapshotWatch(context.Background(), api, "lab", "site")
	defer restarted.stop()
	short, stop := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer stop()
	if _, err := restarted.current(short); err == nil {
		t.Fatal("new watcher inherited prior-run data")
	}
	if calls.Load() > 15 {
		t.Fatal("watch response loop spun without bound", calls.Load())
	}
}

func TestUnavailableWatchClearsReaderAndEstimatorOutput(t *testing.T) {
	cfg := ownerConfig(time.Now())
	view, err := incidentchange.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	failed := &snapshotWatch{failure: errors.New("read authority unavailable"), changed: make(chan struct{}, 1), now: time.Now}
	r := &runtime{cfg: runtimeConfig{Public: cfg, Reader: "east"}, view: view, p: &publisher{}, watch: failed}
	reply, err := r.step(context.Background(), "observe")
	if err != nil {
		t.Fatal(err)
	}
	out := reply.(map[string]any)
	result := out["result"].(incidentchange.Result)
	if result.State != "unavailable" || len(result.Supports) != 0 || out["published"] != false || out["transport_error"] == nil {
		t.Fatal("failed Watch retained current reader output", out)
	}
	reply, err = r.step(context.Background(), "publish")
	if err != nil || reply.(map[string]any)["published"] != false || r.p.epoch != 0 {
		t.Fatal("failed Watch allocated publication", reply, err)
	}
	r.cfg.Gateway = "east"
	r.estimator, _ = gaschange.New(cfg.Temporal)
	prior := r.estimator
	reply, err = r.step(context.Background(), "paired")
	if err != nil {
		t.Fatal(err)
	}
	out = reply.(map[string]any)
	if out["temporal"].(gaschange.Result).Window.State != "unknown" || r.estimator == prior || out["transport_error"] == nil || len(out["publications"].([]any)) != 0 {
		t.Fatal("failed Watch retained estimator history", out)
	}
}
