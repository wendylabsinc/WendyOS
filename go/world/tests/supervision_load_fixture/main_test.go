package main

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
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/client"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
	"golang.org/x/sys/unix"
)

func fixture(t *testing.T, endpoint string) (options, client.Config, *authority.Verifier) {
	t.Helper()
	root, key, _ := ed25519.GenerateKey(rand.Reader)
	pub, secret, _ := ed25519.GenerateKey(rand.Reader)
	now := time.Now()
	g, err := authority.SignGrant(authority.Grant{Version: 1, ID: "lab/test-run/sim-load-publisher", Tenant: "lab", Site: "synthetic-office", Domain: "simulation", Kind: "app", Principal: "lab/sim-load-publisher", Source: world.Source{Device: "sim-load-publisher", App: "wendy-world", Service: "sim-load-publisher", Stream: "state"}, PublicKey: pub, Scopes: []string{"publish", "read"}, PublishAudience: "node-2", NotBeforeUnixMS: now.Add(-time.Second).UnixMilli(), NotAfterUnixMS: now.Add(10 * time.Minute).UnixMilli()}, key)
	if err != nil {
		t.Fatal(err)
	}
	rev, err := authority.SignRevocations(authority.Revocations{Version: 1, Tenant: "lab", Site: "synthetic-office", Domain: "simulation", Revision: 1, IssuedAtUnixMS: now.Add(-time.Second).UnixMilli(), ExpiresAtUnixMS: now.Add(10 * time.Minute).UnixMilli()}, key)
	if err != nil {
		t.Fatal(err)
	}
	cfg := client.Config{URL: endpoint, Audience: "node-2", Grant: g, PrivateKey: secret, AllowInsecureSimulation: true}
	dir := t.TempDir()
	o := options{credentials: filepath.Join(dir, "profile.json"), trust: filepath.Join(dir, "trust.json"), runID: "test-run", duration: time.Second, entities: 2, hz: 2}
	writeJSON(t, o.credentials, cfg)
	writeJSON(t, o.trust, client.SupervisionTrust{RootPublicKey: root, Revocations: rev})
	v, err := authority.NewVerifier(authority.Config{RootPublicKey: root, Tenant: "lab", Site: "synthetic-office", Domain: "simulation", Audience: "node-2", Now: time.Now, Replay: authority.NewMemoryReplay(1000)})
	if err != nil || v.UpdateRevocations(rev) != nil {
		t.Fatal("test trust invalid")
	}
	return o, cfg, v
}
func writeJSON(t *testing.T, path string, value any) {
	t.Helper()
	b, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(b)
	if err = os.WriteFile(path, b, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestPublisherUsesSignedSimulationRecordsAndBoundedMetrics(t *testing.T) {
	var v *authority.Verifier
	var accepted atomic.Uint64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(io.LimitReader(r.Body, 65537))
		if err != nil {
			t.Error(err)
			return
		}
		header, err := base64.StdEncoding.DecodeString(r.Header.Get("X-World-Authorization"))
		if err != nil {
			t.Error(err)
			return
		}
		proof, err := authority.DecodeRequest(header)
		if err != nil {
			t.Error(err)
			return
		}
		if _, err = v.Verify(proof, "publish", body); err != nil {
			t.Error(err)
			return
		}
		if r.URL.Path == "/v1/publishers/epoch" {
			_, _ = w.Write([]byte(`{"epoch":1}`))
			return
		}
		if r.URL.Path != "/v1/components" {
			t.Error("unexpected endpoint")
			return
		}
		var record authority.SignedRecord
		if err = authority.DecodeStrict(body, &record); err != nil {
			t.Error(err)
			return
		}
		if _, err = v.VerifyRecord(record); err != nil {
			t.Error(err)
			return
		}
		if record.Component.Provenance.Origin != world.Simulated || record.Component.Provenance.Simulation.RunID != "test-run" || record.Component.Key.Type != "wendy.load.scalar.v1" || record.Component.Version.Epoch != 1 || record.Component.Frame.Revision != "office-frame-v1" {
			t.Error("load record lost explicit synthetic lineage")
		}
		accepted.Add(1)
		_, _ = w.Write([]byte(`{"result":"applied"}`))
	}))
	defer server.Close()
	o, cfg, verifier := fixture(t, server.URL)
	v = verifier
	var output bytes.Buffer
	if err := run(context.Background(), o, &output); err != nil {
		t.Fatal(err)
	}
	var report struct {
		Attempted, Published uint64
		Domain               string
		Failures             map[string]uint64
	}
	if err := json.Unmarshal(output.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if report.Domain != "simulation" || report.Published != accepted.Load() || accepted.Load() < 3 || report.Attempted > 5 {
		t.Fatal("publication rate or result changed")
	}
	if bytes.Contains(output.Bytes(), []byte(base64.StdEncoding.EncodeToString(cfg.PrivateKey))) || bytes.Contains(output.Bytes(), []byte("signature")) {
		t.Fatal("credential/proof material in metrics")
	}
}

func TestLoadRefusesPhysicalWrongScopeAndRunBeforeNetwork(t *testing.T) {
	for _, kind := range []string{"physical", "broad", "run", "target", "root", "oversize", "fifo", "symlink", "permissions"} {
		t.Run(kind, func(t *testing.T) {
			o, cfg, _ := fixture(t, "http://127.0.0.1:1")
			switch kind {
			case "physical":
				cfg.Grant.Grant.Domain = "physical"
			case "broad":
				cfg.Grant.Grant.Scopes = append(cfg.Grant.Grant.Scopes, "mission")
			case "run":
				o.runID = "different"
			case "target":
				cfg.Audience = "node-1"
			case "root":
				writeJSON(t, o.trust, client.SupervisionTrust{RootPublicKey: make([]byte, 32)})
			}
			writeJSON(t, o.credentials, cfg)
			switch kind {
			case "oversize":
				if err := os.WriteFile(o.credentials, bytes.Repeat([]byte(" "), 65537), 0600); err != nil {
					t.Fatal(err)
				}
			case "fifo":
				o.credentials += ".pipe"
				if err := unix.Mkfifo(o.credentials, 0600); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				path := o.credentials + ".link"
				if err := os.Symlink(o.credentials, path); err != nil {
					t.Fatal(err)
				}
				o.credentials = path
			case "permissions":
				if err := os.Chmod(o.credentials, 0644); err != nil {
					t.Fatal(err)
				}
			}
			start := time.Now()
			if _, _, err := load(o); err == nil {
				t.Fatal("unsafe fixture profile accepted")
			}
			if time.Since(start) > time.Second {
				t.Fatal("file loading blocked")
			}
		})
	}
}

func TestLoadBoundsAndOpaqueErrorClasses(t *testing.T) {
	for _, o := range []options{{runID: "run", duration: 41 * time.Second, entities: 1, hz: 1}, {runID: "run", duration: time.Second, entities: 201, hz: 1}, {runID: "run", duration: time.Second, entities: 1, hz: 3}, {runID: "../../other", duration: time.Second, entities: 1, hz: 1}} {
		if _, _, err := load(o); err == nil {
			t.Fatal("unbounded workload accepted")
		}
	}
	if strings.Contains(failureClass(io.ErrUnexpectedEOF), "EOF") || failureClass(context.DeadlineExceeded) != "timeout_or_cancel" {
		t.Fatal("error details leaked")
	}
}
