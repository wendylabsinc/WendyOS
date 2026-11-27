package authority

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/runtimeobs"
	"github.com/wendylabsinc/wendy/go/world/internal/supervision"
)

type supervisorBatchFixture struct {
	*supervisionFixture
	replay  *batchReplay
	cache   *ReplayCache
	metrics *runtimeobs.Collector
	path    string
}

func newSupervisorBatchFixture(t *testing.T) *supervisorBatchFixture {
	t.Helper()
	f := &supervisorBatchFixture{supervisionFixture: newSupervisionFixture(t), path: filepath.Join(t.TempDir(), "ledger")}
	var err error
	f.cache, err = OpenReplay(f.path, 128)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.cache.Close() })
	f.replay = &batchReplay{ReplayStore: f.cache}
	f.verifier.cfg.Replay = f.replay
	if err = f.verifier.UpdateRevocations(f.rev); err != nil {
		t.Fatal(err)
	}
	f.now = f.now.Add(time.Millisecond) // Deliberately advancing: an actual write, not an equal-ms no-op.
	f.replay.checkpoints = 0
	f.replay.validations = 0
	f.metrics = runtimeobs.New()
	f.cache.SetMetrics(f.metrics)
	return f
}
func supervisorRaw(t *testing.T, s SignedSupervisionAck) json.RawMessage {
	t.Helper()
	b, e := json.Marshal(s)
	if e != nil {
		t.Fatal(e)
	}
	return b
}
func (f *supervisorBatchFixture) operatorProof(t *testing.T, index int) SignedSupervisionAck {
	t.Helper()
	if index == 0 {
		return f.ack(t)
	}
	g := f.operator.Grant
	g.ID = fmt.Sprintf("operator-grant-%d", index)
	g.Principal = fmt.Sprintf("operator-%d", index+1)
	pub, key, e := ed25519.GenerateKey(rand.Reader)
	if e != nil {
		t.Fatal(e)
	}
	g.PublicKey = pub
	grant, e := SignGrant(g, f.root)
	if e != nil {
		t.Fatal(e)
	}
	c := f.c
	c.OperatorPrincipal = g.Principal
	c.OperatorGrantID = g.ID
	c.OperatorKeySHA256 = KeyID(pub)
	s, e := SignSupervisionAck(grant, key, supervision.Ack{Challenge: c})
	if e != nil {
		t.Fatal(e)
	}
	return s
}
func (f *supervisorBatchFixture) check(t *testing.T, proofs ...json.RawMessage) []supervision.Verification {
	t.Helper()
	result, e := f.verifier.VerifySupervisionAcksContext(context.Background(), f.gateway, proofs)
	if e != nil {
		t.Fatal(e)
	}
	if len(result) != len(proofs) {
		t.Fatal("wrong cardinality")
	}
	for i, r := range result {
		if r.InputSHA256 != hash(proofs[i]) {
			t.Fatal("input digest/order")
		}
		if r.Denied && (!reflect.DeepEqual(r.Ack, supervision.Ack{}) || !reflect.DeepEqual(r.Operator, supervision.Operator{})) {
			t.Fatal("denied result leaked authority")
		}
	}
	return result
}
func noSupervisionResults(t *testing.T, result []supervision.Verification, err error) {
	t.Helper()
	if err == nil || result != nil {
		t.Fatalf("partial authority: %+v %v", result, err)
	}
}
func TestSupervisionBatchOneCommitPerFreshChunkAndCanonicalBinding(t *testing.T) {
	for _, size := range []int{1, 2, 8} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			f := newSupervisorBatchFixture(t)
			proofs := make([]json.RawMessage, size)
			signed := make([]SignedSupervisionAck, size)
			for i := range proofs {
				signed[i] = f.operatorProof(t, i)
				proofs[i], _ = json.MarshalIndent(signed[i], "", " ")
			}
			results := f.check(t, proofs...)
			for i, r := range results {
				if r.Denied || r.Ack != signed[i].Ack || r.Operator.Principal != signed[i].Grant.Grant.Principal || !bytes.Equal(r.Operator.Proof, supervisorRaw(t, signed[i])) {
					t.Fatal(r)
				}
				if hash(r.Operator.Proof) == r.InputSHA256 {
					t.Fatal("canonical hash confused with exact input")
				}
			}
			if f.replay.checkpoints != 1 || f.replay.validations != 1 || f.replay.consumes != 0 || metricDuration(t, f.metrics, "replay.checkpoint_commit").Count != 1 || len(f.cache.state.Nonces) != 0 {
				t.Fatal("not one durable nonnonce checkpoint", f.replay, f.cache.state)
			}
			// Same clock can no-op but every call must reverify current signatures/policy.
			f.check(t, proofs...)
			if metricDuration(t, f.metrics, "replay.checkpoint_commit").Count != 1 {
				t.Fatal("equal-ms checkpoint wrote")
			}
			results[0].Operator.Proof[0] = 'x'
			proofs[0][0] = 'x'
			again := f.check(t, proofs...)
			if !again[0].Denied {
				t.Fatal("cached signature result")
			}
		})
	}
}
func TestSupervisionBatchSelectiveDenialsAndGatewayBinding(t *testing.T) {
	for _, change := range []string{"malformed", "forged", "wrong-root", "revoked-principal", "revoked-key", "revoked-grant", "gateway-principal", "gateway-assignee", "gateway-audience", "expired"} {
		t.Run(change, func(t *testing.T) {
			f := newSupervisorBatchFixture(t)
			bad := f.operatorProof(t, 0)
			good := f.operatorProof(t, 1)
			switch change {
			case "forged":
				bad.Signature[0] ^= 1
			case "wrong-root":
				_, other, _ := ed25519.GenerateKey(rand.Reader)
				bad.Grant, _ = SignGrant(bad.Grant.Grant, other)
				bad.Signature, _ = sign(f.operatorKey, "supervision-ack", supervisionPayload(bad.Version, bad.Grant, bad.Ack))
			case "gateway-principal":
				bad.Ack.Challenge.GatewayPrincipal = "another-gateway"
			case "gateway-assignee":
				bad.Ack.Challenge.Assignee = "another/robot"
			case "gateway-audience":
				bad.Ack.Challenge.Audience = "another-node"
			case "expired":
				bad.Ack.Challenge.ExpiresAtUnixMS = f.now.UnixMilli()
			}
			if strings.HasPrefix(change, "gateway-") || change == "expired" {
				bad.Signature, _ = sign(f.operatorKey, "supervision-ack", supervisionPayload(bad.Version, bad.Grant, bad.Ack))
			}
			if strings.HasPrefix(change, "revoked-") {
				rev := f.rev.Revocations
				rev.Revision++
				switch change {
				case "revoked-principal":
					rev.Principals = []string{bad.Grant.Grant.Principal}
				case "revoked-key":
					rev.KeyIDs = []string{KeyID(bad.Grant.Grant.PublicKey)}
				case "revoked-grant":
					rev.GrantIDs = []string{bad.Grant.Grant.ID}
				}
				signed, e := SignRevocations(rev, f.root)
				if e != nil {
					t.Fatal(e)
				}
				if e = f.verifier.UpdateRevocations(signed); e != nil {
					t.Fatal(e)
				}
				f.now = f.now.Add(time.Millisecond)
			}
			raw := supervisorRaw(t, bad)
			if change == "malformed" {
				raw = json.RawMessage(`{"version":1,"version":1}`)
			}
			result := f.check(t, raw, supervisorRaw(t, good))
			if !result[0].Denied || result[1].Denied {
				t.Fatal("denial was not selective", result)
			}
		})
	}
}
func TestSupervisionBatchDeniedOperatorPreservesFloorAcrossReopen(t *testing.T) {
	f := newSupervisorBatchFixture(t)
	result := f.check(t, json.RawMessage(`not-json`))
	if !result[0].Denied || f.replay.checkpoints != 1 {
		t.Fatal(result)
	}
	floor := f.now
	revision, digest := f.cache.state.Revision, f.cache.state.Digest
	f.cache.Close()
	var err error
	f.cache, err = OpenReplay(f.path, 128)
	if err != nil {
		t.Fatal(err)
	}
	if f.cache.state.LastUnixMS != floor.UnixMilli() || len(f.cache.state.Nonces) != 0 {
		t.Fatal("denial floor not durable")
	}
	if err = f.cache.ValidateCheckpoint(revision, digest, floor.Add(-time.Millisecond)); err == nil {
		t.Fatal("rollback accepted")
	}
}
func TestSupervisionBatchInvalidGatewayDoesNotWrite(t *testing.T) {
	for _, mode := range []string{"signature", "scope", "expired", "revoked"} {
		t.Run(mode, func(t *testing.T) {
			f := newSupervisorBatchFixture(t)
			raw := supervisorRaw(t, f.ack(t))
			switch mode {
			case "signature":
				f.gateway.Signature[0] ^= 1
			case "scope":
				g := f.gateway.Grant
				g.Scopes = []string{"task-status"}
				f.gateway, _ = SignGrant(g, f.root)
			case "expired":
				g := f.gateway.Grant
				g.NotAfterUnixMS = f.now.UnixMilli()
				f.gateway, _ = SignGrant(g, f.root)
			case "revoked":
				rev := f.rev.Revocations
				rev.Revision++
				rev.Principals = []string{f.gateway.Grant.Principal}
				r, _ := SignRevocations(rev, f.root)
				if e := f.verifier.UpdateRevocations(r); e != nil {
					t.Fatal(e)
				}
				f.replay.checkpoints = 0
			}
			writes := metricDuration(t, f.metrics, "replay.checkpoint_commit").Count
			result := f.check(t, raw)
			if !result[0].Denied || f.replay.checkpoints != 0 || metricDuration(t, f.metrics, "replay.checkpoint_commit").Count != writes {
				t.Fatal("invalid gateway gained a write", result)
			}
		})
	}
}
func TestSupervisionBatchCompletionChecksAndGlobalErrors(t *testing.T) {
	for _, mode := range []string{"one-expired", "gateway-expired", "stale-policy", "gateway-expired-and-stale", "max-policy-age", "rollback", "checkpoint-error", "validation-error", "later-floor", "later-policy", "missing-policy"} {
		t.Run(mode, func(t *testing.T) {
			f := newSupervisorBatchFixture(t)
			a := f.operatorProof(t, 0)
			b := f.operatorProof(t, 1)
			if mode == "one-expired" {
				a.Ack.Challenge.ExpiresAtUnixMS = f.now.Add(20 * time.Millisecond).UnixMilli()
				a.Signature, _ = sign(f.operatorKey, "supervision-ack", supervisionPayload(a.Version, a.Grant, a.Ack))
			}
			if strings.Contains(mode, "gateway-expired") {
				g := f.gateway.Grant
				g.NotAfterUnixMS = f.now.Add(20 * time.Millisecond).UnixMilli()
				f.gateway, _ = SignGrant(g, f.root)
				a.Ack.Challenge.ExpiresAtUnixMS = g.NotAfterUnixMS
				a.Signature, _ = sign(f.operatorKey, "supervision-ack", supervisionPayload(a.Version, a.Grant, a.Ack))
				b = a
			}
			if strings.Contains(mode, "stale") {
				rev := f.rev.Revocations
				rev.Revision++
				rev.ExpiresAtUnixMS = f.now.Add(20 * time.Millisecond).UnixMilli()
				r, _ := SignRevocations(rev, f.root)
				if e := f.verifier.UpdateRevocations(r); e != nil {
					t.Fatal(e)
				}
				f.now = f.now.Add(time.Millisecond)
			}
			if mode == "max-policy-age" {
				f.verifier.cfg.MaxRevocationAge = 1020 * time.Millisecond
			}
			injected := errors.New("injected replay failure")
			initial := f.now
			switch mode {
			case "checkpoint-error":
				f.replay.checkpointError = injected
			case "validation-error":
				f.replay.validationError = injected
			case "later-floor":
				f.replay.afterCheckpoint = func() {
					if e := f.cache.Checkpoint(f.cache.state.Revision, f.cache.state.Digest, initial.Add(time.Millisecond)); e != nil {
						t.Fatal(e)
					}
				}
			case "later-policy":
				f.replay.afterCheckpoint = func() {
					if e := f.cache.Checkpoint(f.cache.state.Revision+1, "other-installed-policy", initial); e != nil {
						t.Fatal(e)
					}
				}
			case "rollback":
				f.replay.afterValidation = func() { f.now = initial.Add(-time.Millisecond) }
			case "missing-policy":
				f.verifier.rev = nil
			default:
				f.replay.afterValidation = func() { f.now = initial.Add(25 * time.Millisecond) }
			}
			result, err := f.verifier.VerifySupervisionAcksContext(context.Background(), f.gateway, []json.RawMessage{supervisorRaw(t, a), supervisorRaw(t, b)})
			if mode == "one-expired" {
				if err != nil || !result[0].Denied || result[1].Denied {
					t.Fatal(result, err)
				}
				return
			}
			if mode == "gateway-expired" {
				if err != nil || !result[0].Denied || !result[1].Denied {
					t.Fatal(result, err)
				}
				return
			}
			noSupervisionResults(t, result, err)
			if strings.Contains(mode, "stale") || mode == "max-policy-age" || mode == "missing-policy" {
				if !errors.Is(err, ErrStaleRevocations) {
					t.Fatal(err)
				}
			}
			if mode == "rollback" && !errors.Is(err, ErrSupervisionClockRollback) {
				t.Fatal(err)
			}
			if (mode == "checkpoint-error" || mode == "validation-error") && !errors.Is(err, injected) {
				t.Fatal(err)
			}
		})
	}
}
func TestSupervisionBatchBoundsBeforeAuthorityWork(t *testing.T) {
	f := newSupervisorBatchFixture(t)
	raw := supervisorRaw(t, f.ack(t))
	for _, proofs := range [][]json.RawMessage{nil, make([]json.RawMessage, 9), {bytes.Repeat([]byte{' '}, supervision.MaxVerificationProofBytes+1)}} {
		result, err := f.verifier.VerifySupervisionAcksContext(context.Background(), f.gateway, proofs)
		noSupervisionResults(t, result, err)
		if !errors.Is(err, ErrSupervisionBatchBounds) {
			t.Fatal(err)
		}
	}
	gateway := f.gateway
	gateway.Signature = make([]byte, MaxEnvelopeBytes+1)
	result, err := f.verifier.VerifySupervisionAcksContext(context.Background(), gateway, []json.RawMessage{raw})
	noSupervisionResults(t, result, err)
	if f.replay.checkpoints != 0 {
		t.Fatal("bounds refusal wrote")
	}
	// Empty proof is an ordinary individual denial, not a batch-wide fault.
	r := f.check(t, nil, raw)
	if !r[0].Denied || r[1].Denied {
		t.Fatal(r)
	}
}
func TestSupervisionBatchCopiesInputsBeforeVerifierLockWait(t *testing.T) {
	f := newSupervisorBatchFixture(t)
	raw := supervisorRaw(t, f.ack(t))
	original := bytes.Clone(raw)
	gateway := f.gateway
	ctx, c, request := tracedContext(t)
	defer request.Finish(200)
	f.verifier.mu.Lock()
	locked := true
	defer func() {
		if locked {
			f.verifier.mu.Unlock()
		}
	}()
	type outcome struct {
		r []supervision.Verification
		e error
	}
	done := make(chan outcome, 1)
	go func() {
		r, e := f.verifier.VerifySupervisionAcksContext(ctx, gateway, []json.RawMessage{raw})
		done <- outcome{r, e}
	}()
	timeout := time.NewTimer(2 * time.Second)
	defer timeout.Stop()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	ready := false
	for !ready {
		for _, e := range c.Snapshot().Events {
			if e.Phase == "verifier_lock" {
				ready = true
			}
		}
		if ready {
			break
		}
		select {
		case <-tick.C:
		case <-timeout.C:
			t.Fatal("verifier wait not reached")
		}
	}
	raw[0] = 'x'
	gateway.Signature[0] ^= 1
	gateway.Grant.PublicKey[0] ^= 1
	gateway.Grant.Scopes[0] = "read"
	f.verifier.mu.Unlock()
	locked = false
	select {
	case out := <-done:
		if out.e != nil || len(out.r) != 1 || out.r[0].Denied || out.r[0].InputSHA256 != hash(original) {
			t.Fatal(out)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("batch blocked")
	}
}
func TestSupervisionBatchUncertainSyncPoisonsAndRetainsCompleteFloor(t *testing.T) {
	f := newSupervisorBatchFixture(t)
	raw := supervisorRaw(t, f.ack(t))
	injected := errors.New("injected journal sync failure")
	calls := 0
	f.cache.syncFile = func(*os.File) error { calls++; return injected }
	result, err := f.verifier.VerifySupervisionAcksContext(context.Background(), f.gateway, []json.RawMessage{raw})
	noSupervisionResults(t, result, err)
	if !errors.Is(err, injected) || !f.cache.poisoned || calls != 1 {
		t.Fatal(err, calls)
	}
	result, err = f.verifier.VerifySupervisionAcksContext(context.Background(), f.gateway, []json.RawMessage{raw})
	noSupervisionResults(t, result, err)
	if calls != 1 || metricDuration(t, f.metrics, "replay.checkpoint_commit").Count != 1 {
		t.Fatal("failed admission retried")
	}
	f.cache.Close()
	f.cache, err = OpenReplay(f.path, 128)
	if err != nil {
		t.Fatal(err)
	}
	// The complete append preceded the injected sync error. Reopen must retain it;
	// this controlled failure is not a claim about power-loss durability.
	if f.cache.state.LastUnixMS != f.now.UnixMilli() || len(f.cache.state.Nonces) != 0 {
		t.Fatal("complete authority floor lost")
	}
	if f.cache.ValidateCheckpoint(f.cache.state.Revision, f.cache.state.Digest, f.now.Add(-time.Millisecond)) == nil {
		t.Fatal("reopen rollback accepted")
	}
}

// Assert this diagnostic-only context does not add a cancellation policy.
func TestSupervisionBatchCanceledContextDoesNotSkipDurability(t *testing.T) {
	f := newSupervisorBatchFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result, err := f.verifier.VerifySupervisionAcksContext(ctx, f.gateway, []json.RawMessage{supervisorRaw(t, f.ack(t))})
	if err != nil || result[0].Denied || metricDuration(t, f.metrics, "replay.checkpoint_commit").Count != 1 {
		t.Fatal(result, err)
	}
}

func TestSupervisionBatchCurrentPolicyIsNotCachedAcrossCalls(t *testing.T) {
	f := newSupervisorBatchFixture(t)
	a := f.operatorProof(t, 0)
	b := f.operatorProof(t, 1)
	proofs := []json.RawMessage{supervisorRaw(t, a), supervisorRaw(t, b)}
	for _, r := range f.check(t, proofs...) {
		if r.Denied {
			t.Fatal(r)
		}
	}
	rev := f.rev.Revocations
	rev.Revision++
	rev.GrantIDs = []string{a.Grant.Grant.ID}
	signed, err := SignRevocations(rev, f.root)
	if err != nil {
		t.Fatal(err)
	}
	f.now = f.now.Add(time.Millisecond)
	if err = f.verifier.UpdateRevocations(signed); err != nil {
		t.Fatal(err)
	}
	f.now = f.now.Add(time.Millisecond)
	results := f.check(t, proofs...)
	if !results[0].Denied || results[1].Denied {
		t.Fatal("previous valid result survived current revocation", results)
	}
}
func TestSupervisionBatchStalePolicyCannotHideBehindDeniedGateway(t *testing.T) {
	f := newSupervisorBatchFixture(t)
	raw := supervisorRaw(t, f.ack(t))
	f.gateway.Signature[0] ^= 1
	f.verifier.rev = nil
	results, err := f.verifier.VerifySupervisionAcksContext(context.Background(), f.gateway, []json.RawMessage{raw})
	noSupervisionResults(t, results, err)
	if !errors.Is(err, ErrStaleRevocations) || f.replay.checkpoints != 0 {
		t.Fatal(err)
	}
}
func TestSupervisionBatchGatewayIdentityBeforeShapeDenialKeepsFloor(t *testing.T) {
	f := newSupervisorBatchFixture(t)
	g := f.gateway.Grant
	g.TaskStatusAudience = "another-node"
	var err error
	f.gateway, err = SignGrant(g, f.root)
	if err != nil {
		t.Fatal(err)
	}
	results := f.check(t, supervisorRaw(t, f.ack(t)))
	if !results[0].Denied || f.replay.checkpoints != 1 || f.cache.state.LastUnixMS != f.now.UnixMilli() {
		t.Fatal(results)
	}
}

func TestSupervisionBatchInitialGatewayDenialDoesNotProbeReplayHealth(t *testing.T) {
	for _, mode := range []string{"poisoned", "future-floor"} {
		t.Run(mode, func(t *testing.T) {
			f := newSupervisorBatchFixture(t)
			raw := supervisorRaw(t, f.ack(t))
			f.gateway.Signature[0] ^= 1
			if mode == "poisoned" {
				f.cache.poisoned = true
			} else {
				if e := f.cache.Checkpoint(f.cache.state.Revision, f.cache.state.Digest, f.now.Add(time.Second)); e != nil {
					t.Fatal(e)
				}
			}
			writes := metricDuration(t, f.metrics, "replay.checkpoint_commit").Count
			result := f.check(t, raw)
			if !result[0].Denied || f.replay.checkpoints != 0 || f.replay.validations != 0 || metricDuration(t, f.metrics, "replay.checkpoint_commit").Count != writes {
				t.Fatal("identity denial touched replay health", result)
			}
		})
	}
}
