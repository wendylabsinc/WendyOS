package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/client"
	"github.com/wendylabsinc/wendy/go/world/internal/incidentchange"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestExpiredRetryDoesNotSubmit(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; w.WriteHeader(204) }))
	defer server.Close()
	pub, key, _ := ed25519.GenerateKey(rand.Reader)
	_, root, _ := ed25519.GenerateKey(rand.Reader)
	now := time.Now()
	grant, e := authority.SignGrant(authority.Grant{Version: authority.Version, ID: "retry", Tenant: "lab", Site: "lab", Domain: "simulation", Principal: "test", Kind: "app", Source: world.Source{Device: "test", App: "join", Service: "reader", Stream: "state"}, PublicKey: pub, Scopes: []string{"publish"}, PublishAudience: "node-2", NotBeforeUnixMS: now.Add(-time.Second).UnixMilli(), NotAfterUnixMS: now.Add(time.Minute).UnixMilli()}, root)
	if e != nil {
		t.Fatal(e)
	}
	p, e := newPublisher(client.Config{URL: server.URL, Audience: "node-2", Grant: grant, PrivateKey: key, AllowInsecureSimulation: true})
	if e != nil {
		t.Fatal(e)
	}
	c := world.Component{Timing: world.Timing{SampleTime: now.Add(-2 * time.Second)}, TTL: time.Second}
	raw, _ := json.Marshal(c)
	p.pending = &signedPending{raw, c}
	if e = p.retry(context.Background()); !errors.Is(e, errOriginalExpired) || calls != 0 || p.pending != nil {
		t.Fatalf("expired signed POST submitted calls=%d err=%v", calls, e)
	}
}

func TestExpiredPublicationReturnsCurrentUnpublishedView(t *testing.T) {
	cfg := ownerConfig(time.Now())
	view, err := incidentchange.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	r := &runtime{cfg: runtimeConfig{Public: cfg}, p: &publisher{pending: &signedPending{}}, view: view, lastInput: "previous"}
	out := map[string]any{"result": incidentchange.Result{State: "supported_rise"}, "published": true}
	reply, err := r.expiredPublication(out, nil, "original joined support expired before signing")
	if err != nil {
		t.Fatal(err)
	}
	got := reply.(map[string]any)
	current := got["result"].(incidentchange.Result)
	if current.State != "unavailable" || got["published"] != false || got["publication_error"] == nil || r.p.pending != nil || r.lastInput != "" {
		t.Fatalf("expired publication retained current output: %+v", got)
	}
	if len(current.Witnesses) != 0 || len(current.Supports) != 0 {
		t.Fatal("expired output retained current witnesses")
	}
}
