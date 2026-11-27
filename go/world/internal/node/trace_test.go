package node

import (
	"bytes"
	"context"
	"github.com/wendylabsinc/wendy/go/world/internal/diagnostics"
	"github.com/wendylabsinc/wendy/go/world/internal/mission"
	"github.com/wendylabsinc/wendy/go/world/internal/tasking"
	"net/http/httptest"
	"testing"
	"time"
)

func nodeTrace(t *testing.T) *diagnostics.Collector {
	t.Helper()
	c, e := diagnostics.New("0123456789abcdef0123456789abcdef", diagnostics.Limits{WindowMS: 3000, GraceMS: 3000, MaxRequests: 128, MaxEvents: 4096})
	if e != nil {
		t.Fatal(e)
	}
	if e = c.Arm("0123456789abcdef0123456789abcdef"); e != nil {
		t.Fatal(e)
	}
	return c
}
func TestTraceSignedSupervisionKeepsNestedCorrelation(t *testing.T) {
	f, _, ex, a, _, spec, epoch := supervisedFixtureConfigured(t, func(*tasking.Config) {})
	c := nodeTrace(t)
	f.n.cfg.Trace = c
	ack := challenge(t, f, a, spec)
	acknowledge(t, f, a, ack)
	if d := evaluateSupervised(t, f, ex, spec, epoch); d.Action != mission.Continue {
		t.Fatal(d)
	}
	snap := c.Snapshot()
	if len(snap.Requests) != 3 {
		t.Fatal("unexpected request capture", snap.Requests)
	}
	for i, request := range snap.Requests {
		expected := []string{"challenge", "ack", "evaluate"}[i]
		if request.Class != expected || request.Status != 200 || request.EndNS < 0 {
			t.Fatal(request)
		}
		phases := map[string]bool{}
		for _, e := range snap.Events {
			if e.RequestID == request.ID {
				phases[e.Phase] = true
				if e.StartNS < request.StartNS || e.EndNS > request.EndNS || e.EndNS < 0 {
					t.Fatal(e, request)
				}
			}
		}
		for _, name := range []string{"outer_authorization", "verifier_lock", "verifier_hold", "replay_lock", "task_lock", "task_hold", "mission_check", "response"} {
			if !phases[name] {
				t.Fatal(expected, "missing", name)
			}
		}
		if expected != "challenge" && !phases["supervisor_check"] {
			t.Fatal(expected, "supervisor context missing")
		}
		if expected == "evaluate" && !phases["task_commit"] {
			t.Fatal("commit missing")
		}
	}
}
func TestTraceCanceledSignedRequestCompletesWithoutChangingCancellationPolicy(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	f, _, _, a, _, spec, _ := supervisedFixtureConfigured(t, func(cfg *tasking.Config) {
		old := cfg.ValidateAuthorityContext
		cfg.ValidateAuthorityContext = func(ctx context.Context, r tasking.Record) error { close(entered); <-release; return old(ctx, r) }
	})
	f.n.cfg.Trace = nodeTrace(t)
	body := jsonBody(t, SupervisionChallengeRequest{TaskID: spec.ID, TaskDigest: tasking.SpecDigest(spec), RequestID: "request-00000000-canceled"})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := httptest.NewRequest("POST", "/v1/tasks/supervision/challenge", bytes.NewReader(body)).WithContext(ctx)
	if e := AuthorizeRequest(r, a, f.audience, "supervise", body); e != nil {
		t.Fatal(e)
	}
	w := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { defer close(done); f.n.Handler().ServeHTTP(w, r) }()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		close(release)
		t.Fatal("callback not reached")
	}
	cancel()
	close(release)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not complete")
	}
	if w.Code != 200 {
		t.Fatal("instrumentation changed existing completion policy", w.Code, w.Body.String())
	}
	snap := f.n.cfg.Trace.Snapshot()
	if len(snap.Requests) != 1 || !snap.Requests[0].Canceled || snap.Requests[0].EndNS < 0 {
		t.Fatal(snap)
	}
}
func TestTraceMethodClassesAndDisabledMiddleware(t *testing.T) {
	f, _, _, _, _, _, _ := supervisedFixture(t)
	f.n.cfg.Trace = nil
	if f.n.traced(nil) != nil {
		t.Fatal("disabled wrapper not passthrough")
	}
	c := nodeTrace(t)
	f.n.cfg.Trace = c
	for _, pair := range [][2]string{{"GET", "/v1/components"}, {"POST", "/v1/components"}, {"GET", "/v1/tasks"}, {"POST", "/v1/tasks"}} {
		r := httptest.NewRequest(pair[0], pair[1], nil)
		w := httptest.NewRecorder()
		f.n.Handler().ServeHTTP(w, r)
	}
	expected := []string{"query", "publication", "task_list", "other"}
	for i, r := range c.Snapshot().Requests {
		if r.Class != expected[i] {
			t.Fatal(r)
		}
	}
}
