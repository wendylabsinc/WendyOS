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
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/client"
	"github.com/wendylabsinc/wendy/go/world/internal/node"
	"github.com/wendylabsinc/wendy/go/world/internal/store"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
	"golang.org/x/sys/unix"
)

type fixture struct {
	dir               string
	publisher, reader client.Config
	trust             trustBundle
	options           options
	server            *httptest.Server
	store             *store.Store
	posts             atomic.Int32
	requests          atomic.Int32
	alter             func(http.ResponseWriter, *http.Request, http.Handler)
}

func save(t *testing.T, path string, value any) {
	t.Helper()
	b, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, b, 0600); err != nil {
		t.Fatal(err)
	}
}
func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{dir: t.TempDir()}
	root, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	f.options = options{tenant: "lab", site: "synthetic-office", node: "wendy-sim-node", runID: "probe-test", frame: world.FrameRef{ID: "office-floor", Revision: "office-frame-v1"}, deadline: 5 * time.Second}
	now := time.Now()
	rev, err := authority.SignRevocations(authority.Revocations{Version: 1, Tenant: f.options.tenant, Site: f.options.site, Domain: "simulation", Revision: 1, IssuedAtUnixMS: now.Add(-time.Second).UnixMilli(), ExpiresAtUnixMS: now.Add(50 * time.Minute).UnixMilli()}, key)
	if err != nil {
		t.Fatal(err)
	}
	f.trust = trustBundle{RootPublicKey: root, Revocations: rev}
	issue := func(name, kind string, scopes []string) client.Config {
		pub, private, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		owner := ""
		if kind == "app" {
			owner = f.options.node
		}
		grant, err := authority.SignGrant(authority.Grant{Version: 1, ID: "lab/" + f.options.runID + "/" + name, PublishAudience: owner, Tenant: f.options.tenant, Site: f.options.site, Domain: "simulation", Principal: "lab/" + name, Kind: kind, Source: world.Source{Device: name, App: "wendy-world", Service: name, Stream: "state"}, PublicKey: pub, Scopes: scopes, NotBeforeUnixMS: now.Add(-time.Minute).UnixMilli(), NotAfterUnixMS: now.Add(50 * time.Minute).UnixMilli()}, key)
		if err != nil {
			t.Fatal(err)
		}
		return client.Config{URL: "http://127.0.0.1:18881", Audience: f.options.node, Grant: grant, PrivateKey: private, AllowInsecureSimulation: true}
	}
	f.publisher = issue("sim-wendy-publisher", "app", []string{"publish", "read"})
	f.reader = issue("sim-wendy-reader", "operator", []string{"read"})
	save(t, filepath.Join(f.dir, "publisher.json"), f.publisher)
	save(t, filepath.Join(f.dir, "reader.json"), f.reader)
	save(t, filepath.Join(f.dir, "trust.json"), f.trust)
	f.start(t)
	t.Cleanup(func() {
		if f.server != nil {
			f.server.Close()
		}
		if f.store != nil {
			_ = f.store.Close()
		}
	})
	return f
}
func (f *fixture) start(t *testing.T) {
	t.Helper()
	var err error
	f.store, err = store.Open(filepath.Join(f.dir, "world.state"), world.Config{Tenant: f.options.tenant, Site: f.options.site, ClockDomain: "simulation:" + f.options.runID, Now: time.Now, MaxFutureSkew: time.Second, MaxEntries: 64, AllowSynthetic: true, Frames: []world.FrameRef{f.options.frame}})
	if err != nil {
		t.Fatal(err)
	}
	v, err := makeVerifier(f.options, f.trust)
	if err != nil {
		t.Fatal(err)
	}
	n, err := node.New(node.Config{Tenant: f.options.tenant, Site: f.options.site, ProofPath: filepath.Join(f.dir, "proofs.json"), MaxRecords: 64}, f.store, v)
	if err != nil {
		t.Fatal(err)
	}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.requests.Add(1)
		if r.Method == http.MethodPost {
			f.posts.Add(1)
		}
		if f.alter != nil {
			f.alter(w, r, n.Handler())
		} else {
			n.Handler().ServeHTTP(w, r)
		}
	}))
}
func (f *fixture) args(phase string) []string {
	args := []string{"--phase", phase, "--reader", filepath.Join(f.dir, "reader.json"), "--trust", filepath.Join(f.dir, "trust.json"), "--endpoint", f.server.URL, "--tenant", f.options.tenant, "--site", f.options.site, "--node", f.options.node, "--run-id", f.options.runID, "--frame-id", f.options.frame.ID, "--frame-revision", f.options.frame.Revision, "--checkpoint", filepath.Join(f.dir, "checkpoint.json"), "--deadline", "5s"}
	if phase == "publish" {
		args = append(args, "--publisher", filepath.Join(f.dir, "publisher.json"))
	}
	return args
}
func runReport(t *testing.T, args []string) report {
	t.Helper()
	var output bytes.Buffer
	if err := run(context.Background(), args, &output); err != nil {
		t.Fatal(err)
	}
	var result report
	if err := json.Unmarshal(output.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(output.String(), "private_key") || strings.Contains(output.String(), "signature") {
		t.Fatal("report exposed credential envelope")
	}
	return result
}
func TestPublishThenVerifyAfterStoreAndNodeRestart(t *testing.T) {
	f := newFixture(t)
	first := runReport(t, f.args("publish"))
	if !first.SignedFieldsExact || !first.OriginProofVerified || first.DeniedStatus != 401 || first.Version.Sequence != 2 || first.CheckpointReadback || first.WendyTransportIdentityVerified {
		t.Fatalf("unexpected publication report: %+v", first)
	}
	posts := f.posts.Load()
	f.server.Close()
	if err := f.store.Close(); err != nil {
		t.Fatal(err)
	}
	f.start(t)
	second := runReport(t, f.args("verify"))
	if !second.CheckpointReadback || second.ComponentSHA256 != first.ComponentSHA256 || second.Entity != first.Entity || second.Source != first.Source || second.Frame != first.Frame || f.posts.Load() != posts {
		t.Fatal("restart verification changed or rewrote saved state")
	}
	if second.Transport != "raw-simulation-http-via-explicit-loopback-mapping" || second.WendyTransportIdentityVerified {
		t.Fatal("unexpected production transport assertion")
	}
}
func TestRejectUnsafeEndpoints(t *testing.T) {
	for _, raw := range []string{"http://localhost:1234", "http://192.0.2.1:1234", "https://127.0.0.1:1234", "http://user@127.0.0.1:1234", "http://127.0.0.1:1234/path", "http://127.0.0.1:1234?x=1", "http://127.0.0.1", "http://[::1%25lo0]:1234"} {
		if _, err := loopbackEndpoint(raw); err == nil {
			t.Errorf("accepted unsafe endpoint %q", raw)
		}
	}
	for _, raw := range []string{"http://127.0.0.1:18881", "http://[::1]:18881/"} {
		if _, err := loopbackEndpoint(raw); err != nil {
			t.Fatal(err)
		}
	}
}
func TestScopeAndCredentialFailuresHappenBeforeNetwork(t *testing.T) {
	cases := []struct {
		name   string
		change func(*client.Config)
	}{
		{"physical", func(c *client.Config) { c.Grant.Grant.Domain = "physical" }},
		{"wrong-site", func(c *client.Config) { c.Grant.Grant.Site = "another-site" }},
		{"wrong-node", func(c *client.Config) { c.Audience = "another-node" }},
		{"wrong-run", func(c *client.Config) { c.Grant.Grant.ID = "lab/another-run/reader" }},
		{"motion-authority", func(c *client.Config) { c.Grant.Grant.Scopes = append(c.Grant.Grant.Scopes, "mission") }},
		{"wrong-private-seed", func(c *client.Config) { c.PrivateKey[0] ^= 1 }},
		{"no-http-opt-in", func(c *client.Config) { c.AllowInsecureSimulation = false }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			tc.change(&f.reader)
			save(t, filepath.Join(f.dir, "reader.json"), f.reader)
			if err := run(context.Background(), f.args("publish"), io.Discard); err == nil {
				t.Fatal("invalid credential accepted")
			}
			if f.requests.Load() != 0 {
				t.Fatal("made a network request before validating scope")
			}
		})
	}
}
func TestBadAcknowledgementNeverCreatesSuccessCheckpoint(t *testing.T) {
	for _, fault := range []string{"obsolete", "wrong-frame", "wrong-source"} {
		t.Run(fault, func(t *testing.T) {
			f := newFixture(t)
			f.alter = func(w http.ResponseWriter, r *http.Request, next http.Handler) {
				if r.Method != http.MethodPost || r.URL.Path != "/v1/components" {
					next.ServeHTTP(w, r)
					return
				}
				recorder := httptest.NewRecorder()
				next.ServeHTTP(recorder, r)
				var ack struct {
					Result    world.Result    `json:"result"`
					Component world.Component `json:"component"`
				}
				if json.Unmarshal(recorder.Body.Bytes(), &ack) != nil {
					t.Error("bad server fixture")
					return
				}
				if fault == "obsolete" {
					ack.Result = world.Obsolete
				}
				if fault == "wrong-frame" {
					ack.Component.Frame.Revision = "different"
				}
				if fault == "wrong-source" {
					ack.Component.Key.Source.Device = "other"
				}
				_ = json.NewEncoder(w).Encode(ack)
			}
			if err := run(context.Background(), f.args("publish"), io.Discard); err == nil {
				t.Fatal("bad acknowledgement accepted")
			}
			if _, err := os.Stat(filepath.Join(f.dir, "checkpoint.json")); !os.IsNotExist(err) {
				t.Fatal("failed probe wrote a success checkpoint")
			}
		})
	}
}
func TestRedirectNeverForwardsCredential(t *testing.T) {
	f := newFixture(t)
	var followed atomic.Int32
	sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { followed.Add(1); w.WriteHeader(200) }))
	defer sink.Close()
	f.alter = func(w http.ResponseWriter, r *http.Request, next http.Handler) {
		http.Redirect(w, r, sink.URL, http.StatusTemporaryRedirect)
	}
	if err := run(context.Background(), f.args("publish"), io.Discard); err == nil {
		t.Fatal("redirect accepted")
	}
	if followed.Load() != 0 {
		t.Fatal("redirect forwarded an authorization header")
	}
}
func TestResponseBoundAndDeadline(t *testing.T) {
	t.Run("response", func(t *testing.T) {
		f := newFixture(t)
		f.alter = func(w http.ResponseWriter, r *http.Request, next http.Handler) {
			_, _ = io.WriteString(w, strings.Repeat("x", maxResponse+1))
		}
		if err := run(context.Background(), f.args("publish"), io.Discard); err == nil {
			t.Fatal("oversized response accepted")
		}
	})
	t.Run("deadline", func(t *testing.T) {
		f := newFixture(t)
		f.alter = func(w http.ResponseWriter, r *http.Request, next http.Handler) { <-r.Context().Done() }
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		started := time.Now()
		if err := run(ctx, f.args("publish"), io.Discard); err == nil {
			t.Fatal("deadline ignored")
		}
		if time.Since(started) > time.Second {
			t.Fatal("probe exceeded bounded deadline")
		}
	})
}
func TestCheckpointAlterationRejectedBeforeReconnect(t *testing.T) {
	f := newFixture(t)
	runReport(t, f.args("publish"))
	var cp checkpoint
	path := filepath.Join(f.dir, "checkpoint.json")
	if err := readJSON(path, &cp, false); err != nil {
		t.Fatal(err)
	}
	cp.Record.Component.Key.Source.Device = "other-source"
	save(t, path, cp)
	requests := f.requests.Load()
	if err := run(context.Background(), f.args("verify"), io.Discard); err == nil {
		t.Fatal("altered signed checkpoint accepted")
	}
	if f.requests.Load() != requests {
		t.Fatal("contacted endpoint with altered checkpoint")
	}
}
func TestReadbackChangedProvenanceFails(t *testing.T) {
	f := newFixture(t)
	runReport(t, f.args("publish"))
	f.alter = func(w http.ResponseWriter, r *http.Request, next http.Handler) {
		rec := httptest.NewRecorder()
		next.ServeHTTP(rec, r)
		var s client.Snapshot
		if json.Unmarshal(rec.Body.Bytes(), &s) != nil || len(s.Components) != 1 {
			t.Error("invalid fixture snapshot")
			return
		}
		s.Components[0].Provenance.AppVersion = "different"
		_ = json.NewEncoder(w).Encode(s)
	}
	if err := run(context.Background(), f.args("verify"), io.Discard); err == nil {
		t.Fatal("changed readback provenance accepted")
	}
}
func TestPrivateFilePermissionsDuplicateJSONAndExistingCheckpoint(t *testing.T) {
	t.Run("permissions", func(t *testing.T) {
		f := newFixture(t)
		if err := os.Chmod(filepath.Join(f.dir, "reader.json"), 0644); err != nil {
			t.Fatal(err)
		}
		if err := run(context.Background(), f.args("publish"), io.Discard); err == nil {
			t.Fatal("public private-key file accepted")
		}
	})
	t.Run("duplicate-json", func(t *testing.T) {
		f := newFixture(t)
		path := filepath.Join(f.dir, "reader.json")
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		b = append([]byte(`{"audience":"wrong",`), b[1:]...)
		if err = os.WriteFile(path, b, 0600); err != nil {
			t.Fatal(err)
		}
		if err := run(context.Background(), f.args("publish"), io.Discard); err == nil {
			t.Fatal("duplicate JSON accepted")
		}
		if f.requests.Load() != 0 {
			t.Fatal("unvalidated configuration used")
		}
	})
	t.Run("existing-checkpoint", func(t *testing.T) {
		f := newFixture(t)
		runReport(t, f.args("publish"))
		before := f.posts.Load()
		if err := run(context.Background(), f.args("publish"), io.Discard); err == nil {
			t.Fatal("existing checkpoint overwritten")
		}
		if f.posts.Load() != before {
			t.Fatal("republished before refusing checkpoint overwrite")
		}
	})
}

func TestSpecialInputFilesCannotBlockOrFollowLinks(t *testing.T) {
	for _, kind := range []string{"fifo", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			f := newFixture(t)
			path := filepath.Join(f.dir, "reader.json")
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			if kind == "fifo" {
				if err := unix.Mkfifo(path, 0600); err != nil {
					t.Fatal(err)
				}
			} else {
				target := filepath.Join(f.dir, "reader-target.json")
				save(t, target, f.reader)
				if err := os.Symlink(target, path); err != nil {
					t.Fatal(err)
				}
			}
			done := make(chan error, 1)
			go func() { done <- run(context.Background(), f.args("publish"), io.Discard) }()
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("special input was accepted")
				}
			case <-time.After(time.Second):
				t.Fatal("probe blocked opening a special file")
			}
			if f.requests.Load() != 0 {
				t.Fatal("special input triggered network access")
			}
		})
	}
}
