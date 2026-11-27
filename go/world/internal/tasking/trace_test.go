package tasking

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/wendylabsinc/wendy/go/world/internal/diagnostics"
	"github.com/wendylabsinc/wendy/go/world/internal/mission"
	"github.com/wendylabsinc/wendy/go/world/internal/supervision"
	"testing"
)

func TestTraceContextOnlyAuthorityStillDenies(t *testing.T) {
	r := setup(t)
	r.s.Close()
	denied := errors.New("denied")
	r.cfg.ValidateAuthority = nil
	r.cfg.ValidateAuthorityContext = func(context.Context, Record) error { return denied }
	var err error
	r.s, err = Open(r.path, r.cfg)
	if err != nil {
		t.Fatal(err)
	}
	r.epoch, err = r.s.BeginExecutor("app:nav")
	if err != nil {
		t.Fatal(err)
	}
	spec := r.spec("trace-denied")
	if _, err = r.s.Accept(spec, r.auth()); err != nil {
		t.Fatal(err)
	}
	d, err := r.s.EvaluateContext(context.Background(), spec.ID, "app:nav", r.epoch, r.telemetry())
	if !errors.Is(err, denied) || d.Action != mission.Stop || d.Record.Status.Phase != mission.Failed {
		t.Fatal(d, err)
	}
}
func TestTraceContextOnlySupervisorAndCanceledContextPreservePolicy(t *testing.T) {
	r := setupSupervision(t)
	old := r.cfg.ValidateSupervisor
	r.s.Close()
	r.cfg.ValidateSupervisor = nil
	seen := false
	r.cfg.ValidateSupervisorContext = func(ctx context.Context, p json.RawMessage) (supervision.Ack, supervision.Operator, error) {
		seen = diagnostics.Enabled(ctx)
		return old(p)
	}
	var err error
	r.s, err = Open(r.path, r.cfg)
	if err != nil {
		t.Fatal(err)
	}
	r.epoch, err = r.s.BeginExecutor("app:nav")
	if err != nil {
		t.Fatal(err)
	}
	record := r.acceptSupervised(t, "traced")
	c, err := diagnostics.New("0123456789abcdef0123456789abcdef", diagnostics.Limits{WindowMS: 3000, GraceMS: 3000, MaxRequests: 8, MaxEvents: 128})
	if err != nil {
		t.Fatal(err)
	}
	c.Arm("0123456789abcdef0123456789abcdef")
	ctx, cancel := context.WithCancel(context.Background())
	ctx, request := c.Start(ctx, diagnostics.Ack)
	challenge, err := r.s.SupervisionChallengeContext(ctx, record.Spec.ID, SpecDigest(record.Spec), "request-00000000-traced", r.operators["operator-a"])
	if err != nil {
		t.Fatal(err)
	}
	cancel() // Diagnostics must observe cancellation, not introduce a new policy decision.
	if _, err = r.s.AcknowledgeSupervisionContext(ctx, proof(challenge, "operator-a")); err != nil {
		t.Fatal(err)
	}
	d, err := r.s.EvaluateContext(ctx, record.Spec.ID, "app:nav", r.epoch, r.telemetry())
	if err != nil || d.Action != mission.Continue || !seen {
		t.Fatal(d, err, seen)
	}
	request.Finish(200)
	snap := c.Snapshot()
	if !snap.Requests[0].Canceled {
		t.Fatal("cancellation lost")
	}
	phases := map[string]bool{}
	for _, e := range snap.Events {
		phases[e.Phase] = true
		if e.RequestID != 1 || e.EndNS < 0 {
			t.Fatal(e)
		}
	}
	for _, name := range []string{"task_lock", "task_hold", "supervisor_check", "task_commit"} {
		if !phases[name] {
			t.Fatal("missing phase", name)
		}
	}
}
