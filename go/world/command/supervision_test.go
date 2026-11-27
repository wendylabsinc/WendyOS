package command

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/client"
	"github.com/wendylabsinc/wendy/go/world/internal/mission"
	"github.com/wendylabsinc/wendy/go/world/internal/runtimeobs"
	"github.com/wendylabsinc/wendy/go/world/internal/tasking"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
	"golang.org/x/sys/unix"
)

func TestSupervisionCadenceWaitsAfterDelayedSuccess(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	refused := errors.New("intentional next-round refusal")
	var completed time.Time
	calls := 0
	renew := func(ctx context.Context) (client.SupervisionLease, error) {
		calls++
		if calls == 1 {
			// Cross the old ticker's first scheduled event before succeeding.
			delay := time.NewTimer(client.SupervisionRefresh + 50*time.Millisecond)
			defer delay.Stop()
			select {
			case <-ctx.Done():
				return client.SupervisionLease{}, ctx.Err()
			case <-delay.C:
			}
			completed = time.Now()
			return client.SupervisionLease{Schema: "test-acknowledged-lease"}, nil
		}
		if elapsed := time.Since(completed); elapsed < client.SupervisionRefresh {
			t.Errorf("next exchange began %s after completion, want at least %s", elapsed, client.SupervisionRefresh)
		}
		return client.SupervisionLease{}, refused
	}
	var output bytes.Buffer
	if err := supervisionLoop(ctx, renew, &output); !errors.Is(err, refused) {
		t.Fatalf("refusal was not terminal: %v", err)
	}
	if calls != 2 || bytes.Count(output.Bytes(), []byte("\n")) != 1 {
		t.Fatal("refused round retried or emitted an acknowledged lease")
	}
}

func TestSupervisionCLIExplicitInputsAndDispatch(t *testing.T) {
	root, key, _ := ed25519.GenerateKey(rand.Reader)
	pub, private, _ := ed25519.GenerateKey(rand.Reader)
	now := time.Now()
	grant, err := authority.SignGrant(authority.Grant{Version: 1, ID: "operator", Tenant: "lab", Site: "office", Domain: "simulation", Principal: "operator", Kind: "operator", Source: world.Source{Device: "laptop", App: "world", Service: "supervisor", Stream: "presence"}, PublicKey: pub, Scopes: []string{"supervise", "stop", "read"}, NotBeforeUnixMS: now.Add(-time.Minute).UnixMilli(), NotAfterUnixMS: now.Add(time.Hour).UnixMilli()}, key)
	if err != nil {
		t.Fatal(err)
	}
	rev, err := authority.SignRevocations(authority.Revocations{Version: 1, Tenant: "lab", Site: "office", Domain: "simulation", Revision: 1, IssuedAtUnixMS: now.Add(-time.Second).UnixMilli(), ExpiresAtUnixMS: now.Add(time.Minute).UnixMilli()}, key)
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/v1/tasks/supervision/challenge" {
			t.Error("unexpected supervision route")
		}
		http.Error(w, "test refusal", 409)
	}))
	defer server.Close()
	dir := t.TempDir()
	write := func(name string, value any) string {
		t.Helper()
		path := filepath.Join(dir, name)
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	credentials := write("credentials.json", client.Config{URL: server.URL, Audience: "node", Grant: grant, PrivateKey: private, AllowInsecureSimulation: true})
	trust := write("trust.json", client.SupervisionTrust{RootPublicKey: root, Revocations: rev})
	spec := write("spec.json", tasking.Spec{ID: "task", Site: "office", Assignee: "robot/nav", Domain: mission.Simulation, Mode: mission.Supervised, Frame: world.FrameRef{ID: "floor", Revision: "1"}, NotAfterUnixMS: now.Add(time.Minute).UnixMilli()})
	args := []string{"task", "supervise", "--credentials", credentials, "--trust", trust, "--spec", spec, "--run-id", "run", "--assignee", "robot/nav", "--gateway-principal", "gateway", "--duration", "1s", "--json"}
	var out bytes.Buffer
	if err = runTask(args, &out); err == nil {
		t.Fatal("gateway refusal accepted")
	}
	if calls.Load() != 1 || out.Len() != 0 {
		t.Fatal("refusal retried or falsely reported lease")
	}
	diagnostics := filepath.Join(dir, "timing.json")
	if err = runSuperviseContext(context.Background(), append(append([]string{}, args[2:]...), "--diagnostics-file", diagnostics), &out); err == nil {
		t.Fatal("diagnostic mode accepted gateway refusal")
	}
	data, err := os.ReadFile(diagnostics)
	if err != nil {
		t.Fatal(err)
	}
	var observed runtimeobs.Snapshot
	if err = json.Unmarshal(data, &observed); err != nil {
		t.Fatal(err)
	}
	var exchanges, failures uint64
	for _, item := range observed.Durations {
		if item.Name == "supervisor.exchange" {
			exchanges = item.Count
		}
	}
	for _, item := range observed.Counters {
		if item.Name == "supervisor.exchange_failures" {
			failures = item.Value
		}
	}
	if !observed.Enabled || exchanges != 1 || failures != 1 {
		t.Fatal("failed exchange missing from optional timing snapshot")
	}
	before := calls.Load()
	if err = runSuperviseContext(context.Background(), append(append([]string{}, args[2:]...), "--diagnostics-file", diagnostics), &out); err == nil || calls.Load() != before {
		t.Fatal("existing diagnostics output was replaced or reached network")
	}
	for _, extra := range [][]string{{"--duration", "11m"}, {"--assignee", "other"}, {"--spec", ""}, {"--trust", ""}, {"--run-id", ""}} {
		before := calls.Load()
		err = runSuperviseContext(context.Background(), append(append([]string{}, args[2:]...), extra...), &out)
		if err == nil || calls.Load() != before {
			t.Fatal("invalid explicit input reached network")
		}
	}
}
func TestSupervisionInputRejectsSymlinkFIFOAndPermissions(t *testing.T) {
	dir := t.TempDir()
	regular := filepath.Join(dir, "input.json")
	if err := os.WriteFile(regular, []byte(`{"root_public_key":null,"revocations":{}}`), 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(regular, link); err != nil {
		t.Fatal(err)
	}
	fifo := filepath.Join(dir, "pipe")
	if err := unix.Mkfifo(fifo, 0600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{link, fifo, dir} {
		done := make(chan error, 1)
		go func() { var value any; done <- supervisionInput(path, false, &value) }()
		select {
		case err := <-done:
			if err == nil {
				t.Fatal("nonregular input accepted")
			}
		case <-time.After(time.Second):
			t.Fatal("input blocks command")
		}
	}
	if err := os.Chmod(regular, 0644); err != nil {
		t.Fatal(err)
	}
	var value any
	if err := supervisionInput(regular, true, &value); err == nil {
		t.Fatal("public-readable credential accepted")
	}
	if err := os.WriteFile(regular, make([]byte, authority.MaxEnvelopeBytes+1), 0600); err != nil {
		t.Fatal(err)
	}
	if err := supervisionInput(regular, false, &value); err == nil {
		t.Fatal("oversized input accepted")
	}
}
