package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/client"
	"github.com/wendylabsinc/wendy/go/world/internal/mission"
	"github.com/wendylabsinc/wendy/go/world/internal/tasking"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
	"golang.org/x/sys/unix"
)

func privateJSON(t *testing.T, path string, v any) {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, b, 0600); err != nil {
		t.Fatal(err)
	}
}

func fixture(t *testing.T, operator string) (options, client.Config) {
	t.Helper()
	root, key, _ := ed25519.GenerateKey(rand.Reader)
	pub, private, _ := ed25519.GenerateKey(rand.Reader)
	now := time.Now()
	name := "supervisor-" + operator
	grant, err := authority.SignGrant(authority.Grant{Version: 1, ID: "lab/test-run/" + name, Tenant: "lab", Site: "synthetic-office", Domain: "simulation", Kind: "operator", Principal: "lab/" + name, Source: world.Source{Device: name, App: "wendy-world", Service: name, Stream: "state"}, PublicKey: pub, Scopes: []string{"supervise", "stop", "read"}, NotBeforeUnixMS: now.Add(-time.Second).UnixMilli(), NotAfterUnixMS: now.Add(time.Minute).UnixMilli()}, key)
	if err != nil {
		t.Fatal(err)
	}
	rev, err := authority.SignRevocations(authority.Revocations{Version: 1, Tenant: "lab", Site: "synthetic-office", Domain: "simulation", Revision: 1, IssuedAtUnixMS: now.Add(-time.Second).UnixMilli(), ExpiresAtUnixMS: now.Add(time.Minute).UnixMilli()}, key)
	if err != nil {
		t.Fatal(err)
	}
	cfg := client.Config{URL: "http://world-node-1:8080", Audience: "node-1", Grant: grant, PrivateKey: private, AllowInsecureSimulation: true}
	if operator == "a" {
		cfg.URL = "http://supervision-proxy:8099"
	}
	d := t.TempDir()
	o := options{credentials: filepath.Join(d, "client.json"), trust: filepath.Join(d, "trust.json"), spec: filepath.Join(d, "mission.json"), runID: "test-run", operator: operator, release: filepath.Join(d, "release.json"), report: filepath.Join(d, "gate.json"), diagnostics: filepath.Join(d, "diagnostics.json")}
	privateJSON(t, o.credentials, cfg)
	privateJSON(t, o.trust, client.SupervisionTrust{RootPublicKey: root, Revocations: rev})
	privateJSON(t, o.spec, tasking.Spec{ID: "load-mission", Type: "wendy.nav.go_to.v1", Site: "synthetic-office", Assignee: "sim-robot/navigation", Domain: mission.Simulation, Mode: mission.Supervised, Class: mission.Move, Frame: world.FrameRef{ID: "office-floor", Revision: "office-frame-v1"}, Fallback: mission.Hold})
	return o, cfg
}

func released(o options, at time.Time) release {
	n := at.UnixMilli()
	return release{Schema: "wendy.world.supervision-gate-release.v1", RunID: o.runID, ReleaseUnixMS: n, WindowEndUnixMS: n + 24000}
}

func TestGateRunsOnlyFixedSupervisorAtRelease(t *testing.T) {
	for _, operator := range []string{"a", "b"} {
		t.Run(operator, func(t *testing.T) {
			o, cfg := fixture(t, operator)
			target := time.Now().Add(40 * time.Millisecond)
			privateJSON(t, o.release, released(o, target))
			calls := 0
			err := run(context.Background(), o, func(path string, args, env []string) error {
				calls++
				if time.Now().UnixMilli() < target.UnixMilli() {
					t.Error("early release")
				}
				if path != "/world" || !reflect.DeepEqual(args, command(o)) {
					t.Error("unexpected executable or arguments")
				}
				var r report
				if readPrivate(o.report, &r) != nil || r.Stage != "exec" || r.ExecAtUnixMS == nil || r.ReleaseUnixMS == nil || *r.ReleaseUnixMS != target.UnixMilli() || r.ReadyAtUnixMS > *r.ExecAtUnixMS {
					t.Error("missing release timing")
				}
				return nil
			})
			if err != nil || calls != 1 {
				t.Fatal(err, calls)
			}
			b, err := os.ReadFile(o.report)
			if err != nil {
				t.Fatal(err)
			}
			if bytes.Contains(b, []byte(base64.StdEncoding.EncodeToString(cfg.PrivateKey))) || bytes.Contains(b, []byte("signature")) {
				t.Fatal("private data in gate report")
			}
		})
	}
}

func TestGateRejectsBadReleaseWithoutExec(t *testing.T) {
	for _, kind := range []string{"wrong_run", "stale", "future", "window", "cancelled", "symlink", "fifo", "existing_report"} {
		t.Run(kind, func(t *testing.T) {
			o, _ := fixture(t, "a")
			r := released(o, time.Now().Add(20*time.Millisecond))
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch kind {
			case "wrong_run":
				r.RunID = "another-run"
			case "stale":
				r = released(o, time.Now().Add(-3*time.Second))
			case "future":
				r = released(o, time.Now().Add(3*time.Second))
			case "window":
				r.WindowEndUnixMS++
			case "cancelled":
				cancel()
			}
			if kind == "symlink" {
				target := o.release + ".target"
				privateJSON(t, target, r)
				if err := os.Symlink(target, o.release); err != nil {
					t.Fatal(err)
				}
			} else if kind == "fifo" {
				if err := unix.Mkfifo(o.release, 0600); err != nil {
					t.Fatal(err)
				}
			} else {
				privateJSON(t, o.release, r)
			}
			if kind == "existing_report" {
				privateJSON(t, o.report, map[string]string{"untouched": "yes"})
			}
			calls := 0
			if err := run(ctx, o, func(string, []string, []string) error { calls++; return nil }); err == nil || calls != 0 {
				t.Fatal("bad release executed", err, calls)
			}
			if kind == "existing_report" {
				var existing map[string]string
				if readPrivate(o.report, &existing) != nil || existing["untouched"] != "yes" {
					t.Fatal("existing report overwritten")
				}
			}
		})
	}
}

func TestGateCancellationAndExecFailureAreRecorded(t *testing.T) {
	o, _ := fixture(t, "b")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if err := run(ctx, o, func(string, []string, []string) error { t.Error("executed absent release"); return nil }); err == nil {
		t.Fatal("missing release accepted")
	}
	var r report
	if readPrivate(o.report, &r) != nil || r.Stage != "failed" || r.ErrorClass != "cancelled_or_timeout" {
		t.Fatal("missing cancellation report")
	}
	o, _ = fixture(t, "b")
	privateJSON(t, o.release, released(o, time.Now()))
	if err := run(context.Background(), o, func(string, []string, []string) error { return errors.New("sensitive exec error") }); err == nil {
		t.Fatal("exec failure suppressed")
	}
	if readPrivate(o.report, &r) != nil || r.Stage != "failed" || r.ErrorClass != "exec_failed" {
		t.Fatal("missing exec failure report")
	}
	b, _ := os.ReadFile(o.report)
	if bytes.Contains(b, []byte("sensitive")) {
		t.Fatal("raw error exposed")
	}
}

func TestGateRejectsBroadOrMismatchedAuthority(t *testing.T) {
	for _, kind := range []string{"domain", "scope", "key", "url", "operator", "path_alias"} {
		t.Run(kind, func(t *testing.T) {
			o, cfg := fixture(t, "a")
			switch kind {
			case "domain":
				cfg.Grant.Grant.Domain = "physical"
			case "scope":
				cfg.Grant.Grant.Scopes = append(cfg.Grant.Grant.Scopes, "mission")
			case "key":
				cfg.PrivateKey[40] ^= 1
			case "url":
				cfg.URL = "http://world-node-2:8080"
			case "operator":
				o.operator = "b"
			case "path_alias":
				o.report = o.credentials
			}
			privateJSON(t, o.credentials, cfg)
			if validate(o) == nil {
				t.Fatal("invalid scope accepted")
			}
		})
	}
}

func TestGateDoesNotExecAfterCancellationDuringReportWrite(t *testing.T) {
	o, _ := fixture(t, "a")
	privateJSON(t, o.release, released(o, time.Now()))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	write := func(path string, r report, first bool) error {
		err := writeReport(path, r, first)
		if r.Stage == "exec" {
			cancel()
		}
		return err
	}
	calls := 0
	err := runWithReporter(ctx, o, func(string, []string, []string) error { calls++; return nil }, write)
	if err == nil || calls != 0 {
		t.Fatal("cancelled gate launched supervisor", err, calls)
	}
	var r report
	if readPrivate(o.report, &r) != nil || r.Stage != "failed" || r.ErrorClass != "cancelled_or_window_elapsed" {
		t.Fatal("missing final cancellation evidence")
	}
}
