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
	"github.com/wendylabsinc/wendy/go/world/internal/mission"
	"github.com/wendylabsinc/wendy/go/world/internal/tasking"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
	"golang.org/x/sys/unix"
)

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

func fixture(t *testing.T, endpoint string) (options, client.Config, tasking.Spec, *authority.Verifier) {
	t.Helper()
	root, rootKey, _ := ed25519.GenerateKey(rand.Reader)
	pub, key, _ := ed25519.GenerateKey(rand.Reader)
	now := time.Now()
	g, err := authority.SignGrant(authority.Grant{Version: 1, ID: "lab/test-run/supervisor-a", Tenant: "lab", Site: "synthetic-office", Domain: "simulation", Kind: "operator", Principal: "lab/supervisor-a", Source: world.Source{Device: "supervisor-a", App: "wendy-world", Service: "supervisor-a", Stream: "state"}, PublicKey: pub, Scopes: []string{"supervise", "stop", "read"}, NotBeforeUnixMS: now.Add(-time.Second).UnixMilli(), NotAfterUnixMS: now.Add(2 * time.Minute).UnixMilli()}, rootKey)
	if err != nil {
		t.Fatal(err)
	}
	rev, err := authority.SignRevocations(authority.Revocations{Version: 1, Tenant: "lab", Site: "synthetic-office", Domain: "simulation", Revision: 1, IssuedAtUnixMS: now.Add(-time.Second).UnixMilli(), ExpiresAtUnixMS: now.Add(2 * time.Minute).UnixMilli()}, rootKey)
	if err != nil {
		t.Fatal(err)
	}
	cfg := client.Config{URL: endpoint, Audience: "node-1", Grant: g, PrivateKey: key, AllowInsecureSimulation: true}
	spec := tasking.Spec{ID: "load-mission", Type: "wendy.nav.go_to.v1", Site: "synthetic-office", Assignee: "sim-robot/navigation", Domain: mission.Simulation, Mode: mission.Supervised, Class: mission.Move, Frame: world.FrameRef{ID: "office-floor", Revision: "office-frame-v1"}, Fallback: mission.Hold, NotAfterUnixMS: now.Add(time.Minute).UnixMilli()}
	dir := t.TempDir()
	o := options{credentials: filepath.Join(dir, "client.json"), trust: filepath.Join(dir, "trust.json"), spec: filepath.Join(dir, "spec.json"), runID: "test-run"}
	writeJSON(t, o.credentials, cfg)
	writeJSON(t, o.trust, client.SupervisionTrust{RootPublicKey: root, Revocations: rev})
	writeJSON(t, o.spec, spec)
	v, err := authority.NewVerifier(authority.Config{RootPublicKey: root, Tenant: "lab", Site: "synthetic-office", Domain: "simulation", Audience: "node-1", Now: time.Now, Replay: authority.NewMemoryReplay(8)})
	if err != nil || v.UpdateRevocations(rev) != nil {
		t.Fatal("invalid test trust")
	}
	return o, cfg, spec, v
}

func parsedReport(t *testing.T, output *bytes.Buffer) report {
	t.Helper()
	var r report
	if err := json.Unmarshal(output.Bytes(), &r); err != nil {
		t.Fatal(err)
	}
	if r.TotalNS < r.InitializationNS+r.InstructionNS+r.RequestNS+r.ResponseValidationNS || r.RequestDeadlineNS != int64(requestDeadline) {
		t.Fatal("invalid duration accounting")
	}
	if bytes.Contains(output.Bytes(), []byte("private-marker")) || bytes.Contains(output.Bytes(), []byte("signature")) {
		t.Fatal("sensitive data in report")
	}
	return r
}

func TestOneSignedStopAndTerminalResponseTiming(t *testing.T) {
	var verifier *authority.Verifier
	var spec tasking.Spec
	var attempts atomic.Uint32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		if r.URL.Path != "/v1/tasks/stop" || r.Method != "POST" {
			t.Error("unexpected operation")
			w.WriteHeader(400)
			return
		}
		b, _ := io.ReadAll(io.LimitReader(r.Body, authority.MaxEnvelopeBytes+1))
		header, _ := base64.StdEncoding.DecodeString(r.Header.Get("X-World-Authorization"))
		outer, err := authority.DecodeRequest(header)
		if err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		if _, err = verifier.Verify(outer, "stop", b); err != nil {
			t.Error(err)
			w.WriteHeader(401)
			return
		}
		var instruction authority.SignedInstruction
		if authority.DecodeStrict(b, &instruction) != nil {
			t.Error("invalid instruction")
			return
		}
		if _, err = verifier.VerifyInstruction(instruction, "stop"); err != nil {
			t.Error(err)
			return
		}
		if string(instruction.Payload) != `{"task_id":"load-mission"}` {
			t.Error("unexpected stop payload")
			return
		}
		time.Sleep(30 * time.Millisecond)
		_ = json.NewEncoder(w).Encode(tasking.Record{Spec: spec, Status: mission.Status{Version: 2, Phase: mission.Canceled}})
	}))
	defer server.Close()
	o, cfg, s, v := fixture(t, server.URL)
	spec, verifier = s, v
	var output bytes.Buffer
	if err := run(context.Background(), o, &output); err != nil {
		t.Fatal(err)
	}
	r := parsedReport(t, &output)
	if !r.RequestAttempted || !r.ResponseMatchesTask || r.ReturnedPhase != "canceled" || r.Outcome != "terminal_response" || r.RequestNS < int64(30*time.Millisecond) || attempts.Load() != 1 {
		t.Fatalf("unexpected public result: %+v", r)
	}
	if bytes.Contains(output.Bytes(), []byte(base64.StdEncoding.EncodeToString(cfg.PrivateKey))) {
		t.Fatal("private key in report")
	}
}

func TestUnknownOutcomePreservedWithoutRetry(t *testing.T) {
	for _, kind := range []string{"timeout", "http", "response", "redirect"} {
		t.Run(kind, func(t *testing.T) {
			var attempts atomic.Uint32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				attempts.Add(1)
				switch kind {
				case "timeout":
					select {
					case <-r.Context().Done():
					case <-time.After(200 * time.Millisecond):
					}
				case "http":
					w.WriteHeader(403)
					_, _ = w.Write([]byte("private-marker"))
				case "response":
					_, _ = w.Write([]byte(`{"spec":{"id":"other"},"status":{"Version":2,"Phase":"canceled"}}`))
				case "redirect":
					w.Header().Set("Location", "/private-marker")
					w.WriteHeader(307)
				}
			}))
			defer server.Close()
			o, _, _, _ := fixture(t, server.URL)
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			var output bytes.Buffer
			if err := run(ctx, o, &output); err == nil {
				t.Fatal("unknown outcome accepted")
			}
			r := parsedReport(t, &output)
			if !r.RequestAttempted || r.ResponseMatchesTask || r.Outcome != "unknown" || r.ReturnedPhase != "" || attempts.Load() != 1 {
				t.Fatal("retry or false stop confirmation")
			}
			if kind == "timeout" && r.ErrorClass != "timeout" {
				t.Fatal("timeout misclassified")
			}
			if kind == "http" && (r.ErrorClass != "http_refusal" || r.HTTPStatus != 403) {
				t.Fatal("HTTP refusal misclassified")
			}
		})
	}
}

func TestInputsFailBeforeNetwork(t *testing.T) {
	for _, kind := range []string{"physical", "scope", "root", "run", "key", "target", "source", "spec", "oversize", "fifo", "symlink", "permissions"} {
		t.Run(kind, func(t *testing.T) {
			o, cfg, spec, _ := fixture(t, "http://127.0.0.1:1")
			switch kind {
			case "physical":
				cfg.Grant.Grant.Domain = "physical"
			case "scope":
				cfg.Grant.Grant.Scopes = append(cfg.Grant.Grant.Scopes, "publish")
			case "root":
				writeJSON(t, o.trust, client.SupervisionTrust{RootPublicKey: make([]byte, 32)})
			case "run":
				o.runID = "other"
			case "key":
				cfg.PrivateKey[63] ^= 1
			case "target":
				cfg.URL = "http://elsewhere.invalid:8080"
			case "source":
				cfg.Grant.Grant.Source.Device = "other"
			case "spec":
				spec.Domain = mission.Physical
				writeJSON(t, o.spec, spec)
			}
			writeJSON(t, o.credentials, cfg)
			switch kind {
			case "oversize":
				if err := os.WriteFile(o.credentials, []byte(strings.Repeat(" ", authority.MaxEnvelopeBytes+1)), 0600); err != nil {
					t.Fatal(err)
				}
			case "fifo":
				if err := os.Remove(o.credentials); err != nil {
					t.Fatal(err)
				}
				if err := unix.Mkfifo(o.credentials, 0600); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := os.Remove(o.credentials); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(o.trust, o.credentials); err != nil {
					t.Fatal(err)
				}
			case "permissions":
				if err := os.Chmod(o.credentials, 0644); err != nil {
					t.Fatal(err)
				}
			}
			var output bytes.Buffer
			if err := run(context.Background(), o, &output); err == nil {
				t.Fatal("invalid input accepted")
			}
			r := parsedReport(t, &output)
			if r.RequestAttempted || r.Outcome != "not_attempted" || r.ErrorClass != "input_refused" || r.RequestNS != 0 {
				t.Fatal("invalid input reached request")
			}
		})
	}
}
