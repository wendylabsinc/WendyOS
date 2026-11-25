package tasking

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/mission"
	"github.com/wendylabsinc/wendy/go/world/internal/supervision"
)

type supervisionRig struct {
	*rig
	operators  map[string]supervision.Operator
	denied     map[string]bool
	verifyHook func()
}
type testAckProof struct {
	Ack    supervision.Ack `json:"ack"`
	Signer string          `json:"signer"`
}

func setupSupervision(t *testing.T) *supervisionRig {
	t.Helper()
	base := setup(t)
	if err := base.s.Close(); err != nil {
		t.Fatal(err)
	}
	r := &supervisionRig{rig: base, operators: map[string]supervision.Operator{}, denied: map[string]bool{}}
	r.cfg.Tenant = "lab"
	r.cfg.Audience = "node-robot"
	r.cfg.GatewayPrincipal = "device:gateway"
	r.cfg.MaxSupervisionLease = 500 * time.Millisecond
	for i, name := range []string{"operator-a", "operator-b", "operator-c"} {
		r.operators[name] = supervision.Operator{Principal: name, KeySHA256: strings.Repeat(string(rune('a'+i)), 64), GrantID: "grant-" + name, ValidUntilUnixMS: r.wall.Add(time.Hour).UnixMilli()}
	}
	r.cfg.ValidateSupervisor = func(raw json.RawMessage) (supervision.Ack, supervision.Operator, error) {
		var p testAckProof
		if json.Unmarshal(raw, &p) != nil {
			return supervision.Ack{}, supervision.Operator{}, errors.New("invalid test proof")
		}
		operator, ok := r.operators[p.Signer]
		if !ok || r.denied[p.Signer] || operator.ValidUntilUnixMS <= r.wall.UnixMilli() {
			return supervision.Ack{}, supervision.Operator{}, errors.New("test signer revoked or expired")
		}
		if r.verifyHook != nil {
			r.verifyHook()
		}
		operator.Proof, _ = json.Marshal(p)
		return p.Ack, operator, nil
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
	return r
}
func (r *supervisionRig) advance(d time.Duration) { r.wall = r.wall.Add(d); r.mono += d }
func (r *supervisionRig) acceptSupervised(t *testing.T, id string) Record {
	t.Helper()
	spec := r.spec(id)
	spec.Mode = mission.Supervised
	record, err := r.s.Accept(spec, r.auth())
	if err != nil {
		t.Fatal(err)
	}
	return record
}
func (r *supervisionRig) challenge(t *testing.T, id, operator, request string) supervision.Challenge {
	t.Helper()
	record, err := r.s.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	c, err := r.s.SupervisionChallenge(id, SpecDigest(record.Spec), "request-00000000-"+request, r.operators[operator])
	if err != nil {
		t.Fatal(err)
	}
	return c
}
func proof(c supervision.Challenge, operator string) json.RawMessage {
	b, _ := json.Marshal(testAckProof{Ack: supervision.Ack{Challenge: c}, Signer: operator})
	return b
}
func (r *supervisionRig) ack(t *testing.T, c supervision.Challenge) supervision.Receipt {
	t.Helper()
	receipt, err := r.s.AcknowledgeSupervision(proof(c, c.OperatorPrincipal))
	if err != nil {
		t.Fatal(err)
	}
	if receipt.Challenge != c || !sha256String(receipt.AckSHA256) {
		t.Fatal(receipt)
	}
	return receipt
}
func TestSupervisionWaitTwoOperatorsAndLatchedLoss(t *testing.T) {
	r := setupSupervision(t)
	r.acceptSupervised(t, "task")
	telemetry := r.telemetry()
	telemetry.SupervisionUntilUnixMS = r.wall.Add(time.Hour).UnixMilli()
	telemetry.CommandUntilUnixMS = telemetry.SupervisionUntilUnixMS
	d := r.evaluate(t, "task", telemetry)
	if d.Action != mission.Hold || d.Record.Status.Phase != mission.Accepted {
		t.Fatalf("telemetry created authority: %+v", d)
	}
	a := r.challenge(t, "task", "operator-a", "a")
	b := r.challenge(t, "task", "operator-b", "b")
	r.ack(t, a)
	r.ack(t, b)
	if d = r.evaluate(t, "task", r.telemetry()); d.Action != mission.Continue {
		t.Fatal(d)
	}
	r.advance(300 * time.Millisecond)
	renewal := r.challenge(t, "task", "operator-b", "renew")
	r.ack(t, renewal)
	r.advance(250 * time.Millisecond)
	d = r.evaluate(t, "task", r.telemetry())
	if d.Action != mission.Continue {
		t.Fatal("first operator loss stopped other", d)
	}
	status, err := r.s.SupervisionStatus("task")
	if err != nil || len(status.ActiveOperators) != 1 || status.ActiveOperators[0].Principal != "operator-b" {
		t.Fatal(status, err)
	}
	r.advance(251 * time.Millisecond)
	d = r.evaluate(t, "task", r.telemetry())
	if d.Action != mission.Hold || d.Record.Status.Phase != mission.Held {
		t.Fatal(d)
	}
	if _, err = r.s.SupervisionChallenge("task", SpecDigest(d.Record.Spec), "request-00000000-after-held", r.operators["operator-b"]); err == nil {
		t.Fatal("held task accepted renewed challenge")
	}
	if _, err = r.s.AcknowledgeSupervision(proof(renewal, "operator-b")); err == nil {
		t.Fatal("held task resumed by ACK")
	}
	if d = r.evaluate(t, "task", r.telemetry()); d.Record.Status.Phase != mission.Held {
		t.Fatal(d)
	}
}
func TestSupervisionDuplicateDelayedAndModifiedACK(t *testing.T) {
	r := setupSupervision(t)
	r.acceptSupervised(t, "task")
	c := r.challenge(t, "task", "operator-a", "a")
	r.advance(100 * time.Millisecond)
	repeated := r.challenge(t, "task", "operator-a", "a")
	if repeated != c {
		t.Fatal("retry extended challenge")
	}
	for name, change := range map[string]func(*supervision.Challenge){"task": func(x *supervision.Challenge) { x.TaskID = "other" }, "digest": func(x *supervision.Challenge) { x.SpecSHA256 = strings.Repeat("f", 64) }, "boot": func(x *supervision.Challenge) { x.GatewayBootID = "old-boot" }, "epoch": func(x *supervision.Challenge) { x.ExecutorEpoch++ }, "run": func(x *supervision.Challenge) { x.RunID = "other" }, "scope": func(x *supervision.Challenge) { x.Audience = "other" }, "key": func(x *supervision.Challenge) { x.OperatorKeySHA256 = strings.Repeat("f", 64) }, "grant": func(x *supervision.Challenge) { x.OperatorGrantID = "other" }, "deadline": func(x *supervision.Challenge) { x.ExpiresAtUnixMS++ }} {
		t.Run(name, func(t *testing.T) {
			altered := c
			change(&altered)
			if _, err := r.s.AcknowledgeSupervision(proof(altered, "operator-a")); err == nil {
				t.Fatal("accepted modified ACK")
			}
		})
	}
	if _, err := r.s.AcknowledgeSupervision(proof(c, "operator-b")); err == nil {
		t.Fatal("another operator ACKed challenge")
	}
	r.ack(t, c)
	if _, err := r.s.AcknowledgeSupervision(proof(c, "operator-a")); err == nil {
		t.Fatal("reused ACK accepted")
	}
	if d := r.evaluate(t, "task", r.telemetry()); d.Action != mission.Continue {
		t.Fatal(d)
	}
	r.advance(401 * time.Millisecond)
	if d := r.evaluate(t, "task", r.telemetry()); d.Record.Status.Phase != mission.Held {
		t.Fatal("ACK receipt time extended lease", d)
	}
}
func TestSupervisionRevocationAndGrantExpiry(t *testing.T) {
	for _, kind := range []string{"revocation", "expiry", "key-change", "grant-change"} {
		t.Run(kind, func(t *testing.T) {
			r := setupSupervision(t)
			r.acceptSupervised(t, "task")
			a := r.challenge(t, "task", "operator-a", "a")
			b := r.challenge(t, "task", "operator-b", "b")
			r.ack(t, a)
			r.ack(t, b)
			r.evaluate(t, "task", r.telemetry())
			operator := r.operators["operator-a"]
			switch kind {
			case "revocation":
				r.denied["operator-a"] = true
			case "expiry":
				operator.ValidUntilUnixMS = r.wall.UnixMilli()
			case "key-change":
				operator.KeySHA256 = strings.Repeat("f", 64)
			case "grant-change":
				operator.GrantID = "renewed"
			}
			r.operators["operator-a"] = operator
			if d := r.evaluate(t, "task", r.telemetry()); d.Action != mission.Continue {
				t.Fatal("one operator invalidated other", d)
			}
			r.denied["operator-b"] = true
			if d := r.evaluate(t, "task", r.telemetry()); d.Record.Status.Phase != mission.Held {
				t.Fatal("revoked supervision kept moving", d)
			}
		})
	}
}
func TestSupervisionNoHeartbeatPersistenceAndRestartFence(t *testing.T) {
	r := setupSupervision(t)
	r.acceptSupervised(t, "task")
	writes := 0
	original := r.s.write
	r.s.write = func(path string, b []byte) error { writes++; return original(path, b) }
	c := r.challenge(t, "task", "operator-a", "a")
	r.ack(t, c)
	if writes != 0 {
		t.Fatal("heartbeat wrote task store")
	}
	oldBoot := c.GatewayBootID
	if _, err := r.s.BeginExecutor("app:nav"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.s.AcknowledgeSupervision(proof(c, "operator-a")); err == nil {
		t.Fatal("stale executor ACK accepted")
	}
	r.s.Close()
	var err error
	r.s, err = Open(r.path, r.cfg)
	if err != nil {
		t.Fatal(err)
	}
	if r.s.supervisionBoot == oldBoot || len(r.s.supervision) != 0 {
		t.Fatal("boot or leases survived reopen")
	}
	if _, err = r.s.AcknowledgeSupervision(proof(c, "operator-a")); err == nil {
		t.Fatal("old boot ACK accepted")
	}
}
func TestSupervisionCrashFailsRunningAndStopDominates(t *testing.T) {
	for _, stop := range []bool{false, true} {
		t.Run(map[bool]string{false: "reopen", true: "stop"}[stop], func(t *testing.T) {
			r := setupSupervision(t)
			r.acceptSupervised(t, "task")
			c := r.challenge(t, "task", "operator-a", "a")
			r.ack(t, c)
			r.evaluate(t, "task", r.telemetry())
			if stop {
				if _, err := r.s.Stop("task", r.auth()); err != nil {
					t.Fatal(err)
				}
			} else {
				r.s.Close()
				var err error
				r.s, err = Open(r.path, r.cfg)
				if err != nil {
					t.Fatal(err)
				}
			}
			record, err := r.s.Get("task")
			if err != nil || !record.Status.Phase.Terminal() {
				t.Fatal(record, err)
			}
			if _, err = r.s.AcknowledgeSupervision(proof(c, "operator-a")); err == nil {
				t.Fatal("old ACK resurrected task")
			}
		})
	}
}
func TestSupervisionExpiryDuringVerificationAndCommit(t *testing.T) {
	for _, stage := range []string{"ack", "evaluate", "commit"} {
		t.Run(stage, func(t *testing.T) {
			r := setupSupervision(t)
			r.acceptSupervised(t, "task")
			c := r.challenge(t, "task", "operator-a", "a")
			if stage == "ack" {
				r.verifyHook = func() { r.advance(501 * time.Millisecond) }
				if _, err := r.s.AcknowledgeSupervision(proof(c, "operator-a")); err == nil {
					t.Fatal("late proof activated lease")
				}
				return
			}
			r.ack(t, c)
			r.evaluate(t, "task", r.telemetry())
			if stage == "evaluate" {
				r.verifyHook = func() { r.advance(501 * time.Millisecond) }
			} else {
				original := r.s.write
				r.s.write = func(path string, b []byte) error { r.advance(501 * time.Millisecond); return original(path, b) }
			}
			d, err := r.s.Evaluate("task", "app:nav", r.epoch, r.telemetry())
			if err != nil {
				t.Fatal(err)
			}
			if d.Action == mission.Continue || d.Record.Status.Phase != mission.Held {
				t.Fatal("expired during work but continued", d)
			}
		})
	}
}
func TestSupervisionCapsAndClockFaults(t *testing.T) {
	r := setupSupervision(t)
	r.s.cfg.MaxSupervisors = 1
	r.s.cfg.MaxPendingSupervision = 1
	r.acceptSupervised(t, "task")
	c := r.challenge(t, "task", "operator-a", "a")
	record, _ := r.s.Get("task")
	if _, err := r.s.SupervisionChallenge("task", SpecDigest(record.Spec), "request-00000000-rate", r.operators["operator-a"]); !errors.Is(err, ErrSupervisionLimit) {
		t.Fatal(err)
	}
	if _, err := r.s.SupervisionChallenge("task", SpecDigest(record.Spec), "request-00000000-other", r.operators["operator-b"]); !errors.Is(err, ErrSupervisionLimit) {
		t.Fatal(err)
	}
	r.ack(t, c)
	r.evaluate(t, "task", r.telemetry())
	r.advance(100 * time.Millisecond)
	// Even without a heartbeat fsync, wall rollback below the last volatile read fails.
	r.s.SupervisionStatus("task")
	r.wall = r.wall.Add(-time.Millisecond)
	if _, err := r.s.SupervisionStatus("task"); err == nil {
		t.Fatal("volatile wall rollback accepted")
	}
	if d, err := r.s.Evaluate("task", "app:nav", r.epoch, r.telemetry()); err == nil || d.Action != mission.Stop {
		t.Fatal(d, err)
	}
}
func TestSupervisionBoundsByTaskAndGrant(t *testing.T) {
	for _, cap := range []string{"task", "task-authority", "operator"} {
		t.Run(cap, func(t *testing.T) {
			r := setupSupervision(t)
			spec := r.spec("task")
			spec.Mode = mission.Supervised
			auth := r.auth()
			limit := r.wall.Add(100 * time.Millisecond).UnixMilli()
			switch cap {
			case "task":
				spec.NotAfterUnixMS = limit
			case "task-authority":
				auth.ValidUntil = time.UnixMilli(limit)
			case "operator":
				o := r.operators["operator-a"]
				o.ValidUntilUnixMS = limit
				r.operators["operator-a"] = o
			}
			if _, err := r.s.Accept(spec, auth); err != nil {
				t.Fatal(err)
			}
			c := r.challenge(t, "task", "operator-a", "a")
			if c.ExpiresAtUnixMS != limit {
				t.Fatal(c)
			}
			r.advance(100 * time.Millisecond)
			if _, err := r.s.AcknowledgeSupervision(proof(c, "operator-a")); err == nil {
				t.Fatal("ACK beyond bound")
			}
		})
	}
}
func TestUnconfiguredSupervisionAndTeleopCannotUseTelemetry(t *testing.T) {
	r := setup(t)
	for _, mode := range []mission.Mode{mission.Supervised, mission.Teleop} {
		spec := r.spec(string(mode))
		spec.Mode = mode
		if _, err := r.s.Accept(spec, r.auth()); err == nil {
			t.Fatal("unsupported mode admitted", mode)
		}
	}
	r.accept(t, "autonomous")
	telemetry := r.telemetry()
	telemetry.SupervisionUntilUnixMS = r.wall.Add(time.Hour).UnixMilli()
	telemetry.CommandUntilUnixMS = telemetry.SupervisionUntilUnixMS
	if d := r.evaluate(t, "autonomous", telemetry); d.Action != mission.Continue {
		t.Fatal("autonomy changed", d)
	}
}

func TestPostCommitTelemetryFreshness(t *testing.T) {
	for _, mode := range []mission.Mode{mission.Autonomous, mission.Supervised} {
		t.Run(string(mode), func(t *testing.T) {
			r := setupSupervision(t)
			r.s.cfg.MaxTelemetryAge = 250 * time.Millisecond
			if mode == mission.Supervised {
				r.acceptSupervised(t, "task")
				c := r.challenge(t, "task", "operator-a", "a")
				r.ack(t, c)
			} else {
				r.accept(t, "task")
			}
			original := r.s.write
			r.s.write = func(path string, b []byte) error { r.advance(300 * time.Millisecond); return original(path, b) }
			d, err := r.s.Evaluate("task", "app:nav", r.epoch, r.telemetry())
			if err != nil || d.Action != mission.Hold || d.Record.Status.Phase != mission.Held {
				t.Fatalf("stale sensor state permitted motion: %+v %v", d, err)
			}
		})
	}
}

func TestContinueCarriesConservativeSupervisionBudget(t *testing.T) {
	r := setupSupervision(t)
	r.acceptSupervised(t, "task")
	if d := r.evaluate(t, "task", r.telemetry()); d.Supervision != nil {
		t.Fatal("initial wait has movement budget")
	}
	c := r.challenge(t, "task", "operator-a", "a")
	r.ack(t, c)
	r.advance(100 * time.Millisecond)
	original := r.s.write
	once := false
	r.s.write = func(path string, b []byte) error {
		if !once {
			once = true
			r.advance(50 * time.Millisecond)
		}
		return original(path, b)
	}
	d := r.evaluate(t, "task", r.telemetry())
	if d.Action != mission.Continue || d.Supervision == nil {
		t.Fatal(d)
	}
	budget := d.Supervision
	if budget.RemainingMS != 350 || budget.ExpiresAtUnixMS != c.ExpiresAtUnixMS || budget.ExecutorEpoch != r.epoch || budget.GatewayBootID != c.GatewayBootID {
		t.Fatalf("renewed or unbound budget: %+v", budget)
	}
	r.advance(351 * time.Millisecond)
	d = r.evaluate(t, "task", r.telemetry())
	if d.Supervision != nil || d.Action != mission.Hold {
		t.Fatal(d)
	}
	r2 := setup(t)
	r2.accept(t, "autonomous")
	if d := r2.evaluate(t, "autonomous", r2.telemetry()); d.Supervision != nil {
		t.Fatal("autonomous received supervision budget")
	}
}
func TestSupervisionMonotonicRollbackFailsClosed(t *testing.T) {
	r := setupSupervision(t)
	r.acceptSupervised(t, "task")
	c := r.challenge(t, "task", "operator-a", "a")
	r.ack(t, c)
	r.advance(50 * time.Millisecond)
	if _, err := r.s.SupervisionStatus("task"); err != nil {
		t.Fatal(err)
	}
	r.mono -= time.Millisecond
	if d, err := r.s.Evaluate("task", "app:nav", r.epoch, r.telemetry()); err == nil || d.Action != mission.Stop || d.Supervision != nil {
		t.Fatal(d, err)
	}
}

func TestSupervisionGapCannotBeBridgedBetweenExecutorPolls(t *testing.T) {
	for _, operation := range []string{"challenge", "ack", "status", "revoked"} {
		t.Run(operation, func(t *testing.T) {
			r := setupSupervision(t)
			r.acceptSupervised(t, "task")
			c := r.challenge(t, "task", "operator-a", "first")
			r.ack(t, c)
			r.evaluate(t, "task", r.telemetry())
			r.advance(400 * time.Millisecond)
			pending := r.challenge(t, "task", "operator-a", "pending")
			if operation == "revoked" {
				r.denied["operator-a"] = true
			} else {
				r.advance(101 * time.Millisecond)
			}
			switch operation {
			case "challenge", "revoked":
				record, _ := r.s.Get("task")
				if _, err := r.s.SupervisionChallenge("task", SpecDigest(record.Spec), "request-00000000-fresh", r.operators["operator-b"]); err == nil {
					t.Fatal("fresh challenge bridged supervision gap")
				}
			case "ack":
				if _, err := r.s.AcknowledgeSupervision(proof(pending, "operator-a")); err == nil {
					t.Fatal("pending ACK bridged supervision gap")
				}
			case "status":
				if _, err := r.s.SupervisionStatus("task"); err != nil {
					t.Fatal(err)
				}
			}
			record, err := r.s.Get("task")
			if err != nil || record.Status.Phase != mission.Held {
				t.Fatal("loss was not durably latched before another evaluate", record, err)
			}
			if d := r.evaluate(t, "task", r.telemetry()); d.Action != mission.Hold || d.Supervision != nil {
				t.Fatal(d)
			}
		})
	}
}
func TestOverlappingSupervisionPreservesContinuityBetweenPolls(t *testing.T) {
	r := setupSupervision(t)
	r.acceptSupervised(t, "task")
	a := r.challenge(t, "task", "operator-a", "a")
	r.ack(t, a)
	r.evaluate(t, "task", r.telemetry())
	r.advance(300 * time.Millisecond)
	b := r.challenge(t, "task", "operator-b", "b")
	r.ack(t, b)
	r.advance(201 * time.Millisecond)
	a = r.challenge(t, "task", "operator-a", "renew")
	r.ack(t, a)
	if d := r.evaluate(t, "task", r.telemetry()); d.Action != mission.Continue || d.Record.Status.Phase != mission.Running {
		t.Fatal(d)
	}
}

func TestACKCannotBridgeGapAtFinalActivationClock(t *testing.T) {
	r := setupSupervision(t)
	r.acceptSupervised(t, "task")
	first := r.challenge(t, "task", "operator-a", "first")
	r.ack(t, first)
	r.evaluate(t, "task", r.telemetry())
	r.advance(400 * time.Millisecond)
	pending := r.challenge(t, "task", "operator-a", "pending")
	r.advance(99 * time.Millisecond)
	// Old coverage is valid through proof verification. Only the final ACK
	// admission clock crosses its expiry; the pending challenge itself is live.
	reads := 0
	r.s.cfg.Now = func() time.Time {
		reads++
		if reads == 7 {
			r.advance(2 * time.Millisecond)
		}
		return r.wall
	}
	if _, err := r.s.AcknowledgeSupervision(proof(pending, "operator-a")); err == nil {
		t.Fatal("final activation clock bridged supervision gap")
	}
	record, err := r.s.Get("task")
	if err != nil || record.Status.Phase != mission.Held {
		t.Fatal(record, err)
	}
	if reads < 7 {
		t.Fatalf("test failed to reach final activation clock: %d", reads)
	}
}
