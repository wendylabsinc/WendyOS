package node

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/mission"
	"github.com/wendylabsinc/wendy/go/world/internal/tasking"
)

// The holder is a real verifier call blocked inside its replay checkpoint.
// This leaves the verifier mutex held while an executor's mission check queues.
type liveGateReplay struct {
	authority.ReplayStore
	armed            atomic.Bool
	entered, release chan struct{}
}

func (g *liveGateReplay) Checkpoint(revision uint64, digest string, now time.Time) error {
	if g.armed.CompareAndSwap(true, false) {
		close(g.entered)
		<-g.release
	}
	return g.ReplayStore.Checkpoint(revision, digest, now)
}
func awaitLiveGate(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(3 * time.Second):
		t.Fatal("live request did not reach gate")
	}
}

func TestHTTPLiveEvaluationCanceledBehindAuthorityDoesNotLatchHold(t *testing.T) {
	f, op, ex, path, cfg := taskFixture(t)
	if err := f.n.cfg.Tasks.Close(); err != nil {
		t.Fatal(err)
	}
	var wall atomic.Int64
	wall.Store(time.Now().UnixMilli())
	cfg.Now = func() time.Time { return time.UnixMilli(wall.Load()) }
	cfg.MaxTelemetryAge = 250 * time.Millisecond
	replay := &liveGateReplay{ReplayStore: authority.NewMemoryReplay(100), entered: make(chan struct{}), release: make(chan struct{})}
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(replay.release) }) }
	defer release()
	v, err := authority.NewVerifier(authority.Config{RootPublicKey: f.root.Public().(ed25519.PublicKey), Tenant: "tenant", Site: "office", Domain: "simulation", Audience: f.audience, Now: time.Now, Replay: replay})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	rev, err := authority.SignRevocations(authority.Revocations{Version: 1, Tenant: "tenant", Site: "office", Domain: "simulation", Revision: 1, IssuedAtUnixMS: now.Add(-time.Second).UnixMilli(), ExpiresAtUnixMS: now.Add(59 * time.Minute).UnixMilli()}, f.root)
	if err != nil {
		t.Fatal(err)
	}
	if err = v.UpdateRevocations(rev); err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	var enabled atomic.Bool
	var enteredOnce sync.Once
	cfg.ValidateAuthority = MissionAuthority(v)
	cfg.ValidateAuthorityContext = func(ctx context.Context, r tasking.Record) error {
		if enabled.Load() {
			enteredOnce.Do(func() { close(entered) })
		}
		return MissionAuthorityContext(v)(ctx, r)
	}
	tasks, err := tasking.Open(path, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer tasks.Close()
	f.n.cfg.Tasks = tasks
	epoch := startExecutor(t, f, ex)
	spec := taskSpec("live-authority-queue")
	spec.Fallback = mission.Hold
	body := instructionBody(t, f, op, "mission", spec)
	if code, b := call(t, f, op, "POST", "/v1/tasks", "mission", body); code != 200 {
		t.Fatalf("admission: %d %s", code, b)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	replay.armed.Store(true)
	holder := make(chan error, 1)
	go func() { _, err := v.VerifyGrant(ex.Grant, "read"); holder <- err }()
	awaitLiveGate(t, replay.entered)
	enabled.Store(true)
	tm := telemetry()
	tm.SampleUnixMS = wall.Load()
	requestBody := jsonBody(t, EvaluateRequest{TaskID: spec.ID, ExecutorEpoch: epoch, Telemetry: tm})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := httptest.NewRequest(http.MethodPost, "/v1/tasks/evaluate", bytes.NewReader(requestBody)).WithContext(ctx)
	if err = AuthorizeRequest(r, ex, f.audience, "execute", requestBody); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { defer close(done); f.n.Handler().ServeHTTP(w, r) }()
	awaitLiveGate(t, entered)
	cancel()
	wall.Add(1000) // The queued observation is now stale, but its delivery is abandoned.
	release()
	awaitLiveGate(t, done)
	if err = <-holder; err != nil {
		t.Fatal(err)
	}
	if w.Code != http.StatusConflict || !bytes.Contains(w.Body.Bytes(), []byte(context.Canceled.Error())) {
		t.Fatalf("abandoned HTTP evaluation: %d %s", w.Code, w.Body.String())
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("canceled authority wait mutated task state/audit", err)
	}
	tm.SampleUnixMS = wall.Load()
	code, b := call(t, f, ex, "POST", "/v1/tasks/evaluate", "execute", jsonBody(t, EvaluateRequest{TaskID: spec.ID, ExecutorEpoch: epoch, Telemetry: tm}))
	var decision tasking.Decision
	if code != 200 || json.Unmarshal(b, &decision) != nil || decision.Action != mission.Continue || decision.Record.Status.Phase != mission.Running {
		t.Fatalf("fresh request inherited a HOLD: %d %s", code, b)
	}
}

func TestHTTPAlreadyCanceledEvaluationDoesNotEnterTaskEngine(t *testing.T) {
	f, op, ex, path, _ := taskFixture(t)
	epoch := startExecutor(t, f, ex)
	spec := taskSpec("canceled-before-arrival")
	body := instructionBody(t, f, op, "mission", spec)
	if code, b := call(t, f, op, "POST", "/v1/tasks", "mission", body); code != 200 {
		t.Fatalf("admission: %d %s", code, b)
	}
	before, _ := os.ReadFile(path)
	tm := telemetry()
	tm.SampleUnixMS -= 1000
	body = jsonBody(t, EvaluateRequest{TaskID: spec.ID, ExecutorEpoch: epoch, Telemetry: tm})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := httptest.NewRequest(http.MethodPost, "/v1/tasks/evaluate", bytes.NewReader(body)).WithContext(ctx)
	if err := AuthorizeRequest(r, ex, f.audience, "execute", body); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	f.n.Handler().ServeHTTP(w, r)
	after, _ := os.ReadFile(path)
	if w.Code != http.StatusConflict || !bytes.Equal(before, after) {
		t.Fatal("already canceled evaluation changed state", w.Code)
	}
}
