package client

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

func credentials(t *testing.T) (Config, *authority.Verifier) {
	t.Helper()
	root, rk, e := ed25519.GenerateKey(rand.Reader)
	if e != nil {
		t.Fatal(e)
	}
	pub, pk, e := ed25519.GenerateKey(rand.Reader)
	if e != nil {
		t.Fatal(e)
	}
	now := time.Now()
	g, e := authority.SignGrant(authority.Grant{Version: 1, ID: "test", PublishAudience: "node", Tenant: "t", Site: "s", Domain: "simulation", Principal: "publisher", Kind: "app", Source: world.Source{Device: "d", App: "a", Service: "s", Stream: "x"}, PublicKey: pub, Scopes: []string{"publish", "read"}, NotBeforeUnixMS: now.Add(-time.Minute).UnixMilli(), NotAfterUnixMS: now.Add(time.Hour).UnixMilli()}, rk)
	if e != nil {
		t.Fatal(e)
	}
	v, e := authority.NewVerifier(authority.Config{RootPublicKey: root, Tenant: "t", Site: "s", Domain: "simulation", Audience: "node", Now: time.Now, Replay: authority.NewMemoryReplay(100)})
	if e != nil {
		t.Fatal(e)
	}
	r, e := authority.SignRevocations(authority.Revocations{Version: 1, Tenant: "t", Site: "s", Domain: "simulation", Revision: 1, IssuedAtUnixMS: now.Add(-time.Second).UnixMilli(), ExpiresAtUnixMS: now.Add(time.Minute).UnixMilli()}, rk)
	if e != nil {
		t.Fatal(e)
	}
	if e = v.UpdateRevocations(r); e != nil {
		t.Fatal(e)
	}
	return Config{Audience: "node", Grant: g, PrivateKey: pk, AllowInsecureSimulation: true}, v
}
func TestClientSignsDistinctRequestsAndOriginRecord(t *testing.T) {
	cfg, v := credentials(t)
	seen := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, e := io.ReadAll(r.Body)
		if e != nil {
			t.Error(e)
			return
		}
		header, e := base64.StdEncoding.DecodeString(r.Header.Get("X-World-Authorization"))
		if e != nil {
			t.Error(e)
			return
		}
		var signed authority.SignedRequest
		if e = json.Unmarshal(header, &signed); e != nil {
			t.Error(e)
			return
		}
		op := "read"
		if r.Method == http.MethodPost {
			op = "publish"
		}
		if _, e = v.Verify(signed, op, b); e != nil {
			t.Error(e)
			http.Error(w, "denied", 401)
			return
		}
		seen++
		switch r.URL.Path {
		case "/v1/publishers/epoch":
			_, _ = w.Write([]byte(`{"epoch":3}`))
		case "/v1/components":
			if r.Method == http.MethodPost {
				var record authority.SignedRecord
				if e = json.Unmarshal(b, &record); e != nil {
					t.Error(e)
				}
				if _, e = v.VerifyRecord(record); e != nil {
					t.Error(e)
				}
				_, _ = w.Write([]byte(`{"result":"applied"}`))
			} else {
				_, _ = w.Write([]byte(`{"components":[],"cursor":"test"}`))
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	cfg.URL = server.URL
	c, e := New(cfg)
	if e != nil {
		t.Fatal(e)
	}
	ctx := context.Background()
	epoch, e := c.NextEpoch(ctx)
	if e != nil || epoch != 3 {
		t.Fatal(epoch, e)
	}
	component := world.Component{Key: world.Key{Tenant: "t", Site: "s", Entity: "one", Type: "pose", Source: cfg.Grant.Grant.Source}, Version: world.Version{Epoch: 3, Sequence: 1}, TTL: time.Second}
	if e = c.Publish(ctx, component); e != nil {
		t.Fatal(e)
	}
	for i := 0; i < 2; i++ {
		s, e := c.Query(ctx)
		if e != nil || s.Cursor != "test" {
			t.Fatal(s, e)
		}
	}
	if seen != 4 {
		t.Fatal(seen)
	}
}
func TestClientRefusesRedirectAndPhysicalHTTP(t *testing.T) {
	cfg, _ := credentials(t)
	forwarded := false
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { forwarded = true }))
	defer target.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, 302) }))
	defer source.Close()
	cfg.URL = source.URL
	c, e := New(cfg)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = c.Query(context.Background()); e == nil {
		t.Fatal("followed redirect")
	}
	if forwarded {
		t.Fatal("credential sent to redirected host")
	}
	cfg.Grant.Grant.Domain = "physical"
	if _, e = New(cfg); e == nil {
		t.Fatal("physical HTTP allowed")
	}
}
