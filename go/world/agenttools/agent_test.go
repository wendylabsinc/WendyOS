//go:build !windows

package agenttools

import (
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
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/client"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

func TestChatUsesSignedFixedProfileAndRejectsAuthorityChanges(t *testing.T) {
	root, rootKey, _ := ed25519.GenerateKey(rand.Reader)
	public, key, _ := ed25519.GenerateKey(rand.Reader)
	now := time.Now()
	grant, err := authority.SignGrant(authority.Grant{Version: 1, ID: "chat", Tenant: "test", Site: "synthetic", Domain: "simulation", Principal: "chat-agent", Kind: "agent", Source: world.Source{Device: "test-device", App: "chat", Service: "world", Stream: "proposals"}, PublicKey: public, Scopes: []string{"read", "propose"}, NotBeforeUnixMS: now.Add(-time.Minute).UnixMilli(), NotAfterUnixMS: now.Add(time.Hour).UnixMilli()}, rootKey)
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := authority.NewVerifier(authority.Config{RootPublicKey: root, Tenant: "test", Site: "synthetic", Domain: "simulation", Audience: "gateway", Now: time.Now, Replay: authority.NewMemoryReplay(100)})
	if err != nil {
		t.Fatal(err)
	}
	revocations, err := authority.SignRevocations(authority.Revocations{Version: 1, Tenant: "test", Site: "synthetic", Domain: "simulation", Revision: 1, IssuedAtUnixMS: now.Add(-time.Second).UnixMilli(), ExpiresAtUnixMS: now.Add(30 * time.Minute).UnixMilli()}, rootKey)
	if err != nil {
		t.Fatal(err)
	}
	if err = verifier.UpdateRevocations(revocations); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != "GET" || r.URL.Path != "/v1/components" && r.URL.Path != "/v1/proposals" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			http.Error(w, "unexpected", 400)
			return
		}
		encoded, err := base64.StdEncoding.DecodeString(r.Header.Get("X-World-Authorization"))
		var signed authority.SignedRequest
		body, _ := io.ReadAll(r.Body)
		if err != nil || authority.DecodeStrict(encoded, &signed) != nil {
			t.Error("missing signed request")
			http.Error(w, "denied", 401)
			return
		}
		identity, err := verifier.Verify(signed, "read", body)
		if err != nil || identity.Kind != "agent" {
			t.Errorf("invalid agent request: %v", err)
			http.Error(w, "denied", 401)
			return
		}
		if r.URL.Path == "/v1/proposals" {
			json.NewEncoder(w).Encode(map[string]any{"proposals": []any{}, "next": ""})
		} else {
			json.NewEncoder(w).Encode(client.Snapshot{Cursor: strings.Repeat("a", 64), Components: []world.Component{}})
		}
	}))
	defer endpoint.Close()
	profile := client.Config{URL: endpoint.URL, Audience: "gateway", Grant: grant, PrivateKey: key, AllowInsecureSimulation: true}
	path := filepath.Join(t.TempDir(), "agent.json")
	write := func() {
		t.Helper()
		raw, err := json.Marshal(profile)
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(path, raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	write()
	result, err := Call(context.Background(), path, "world_query", json.RawMessage(`{"limit":1}`))
	if err != nil || result.IsError || calls.Load() != 1 {
		t.Fatalf("signed query failed: %v %+v", err, result)
	}
	result, err = Call(context.Background(), path, "world_proposals", json.RawMessage(`{"limit":1}`))
	if err != nil || result.IsError || calls.Load() != 2 {
		t.Fatalf("signed proposal history read failed: %v %+v", err, result)
	}
	for _, args := range []string{`{"url":"http://elsewhere"}`, `{"credentials":"operator.json"}`, `{"limit":1000}`} {
		result, err = Call(context.Background(), path, "world_query", json.RawMessage(args))
		if err == nil && !result.IsError {
			t.Fatalf("accepted authority/schema change %s", args)
		}
	}
	if calls.Load() != 2 {
		t.Fatal("invalid arguments reached gateway")
	}
	if _, err := Call(context.Background(), path, "task_send", json.RawMessage(`{}`)); err == nil {
		t.Fatal("accepted execution operation")
	}
	profile.Grant.Grant.Kind = "operator"
	write()
	if _, err := Call(context.Background(), path, "world_query", json.RawMessage(`{}`)); err == nil {
		t.Fatal("accepted operator profile")
	}
	profile.Grant = grant
	write()
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := Call(context.Background(), path, "world_query", json.RawMessage(`{}`)); err == nil {
		t.Fatal("accepted shared signing file")
	}
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	link := path + ".link"
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, err := Call(context.Background(), link, "world_query", json.RawMessage(`{}`)); err == nil {
		t.Fatal("accepted symlink profile")
	}
	if calls.Load() != 2 {
		t.Fatal("rejected profile reached gateway")
	}
}
