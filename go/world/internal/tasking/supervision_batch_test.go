package tasking

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/mission"
	"github.com/wendylabsinc/wendy/go/world/internal/supervision"
)

func batchRig(t *testing.T, count int) *supervisionRig {
	t.Helper()
	r := setupSupervision(t)
	r.s.cfg.MaxSupervisors = 32
	r.acceptSupervised(t, "batch")
	for i := 0; i < count; i++ {
		name := fmt.Sprintf("batch-operator-%02d", i)
		key := sha256.Sum256([]byte(name))
		r.operators[name] = supervision.Operator{Principal: name, KeySHA256: hex.EncodeToString(key[:]), GrantID: "grant-" + name, ValidUntilUnixMS: r.wall.Add(time.Hour).UnixMilli()}
		r.ack(t, r.challenge(t, "batch", name, name))
	}
	return r
}

// The callback fixture delegates to the pre-existing test single-proof verifier.
// Real signatures and replay persistence are tested at the authority/node boundary.
func batchVerify(r *supervisionRig, proofs []json.RawMessage) []supervision.Verification {
	results := make([]supervision.Verification, len(proofs))
	for i, raw := range proofs {
		digest := sha256.Sum256(raw)
		results[i].InputSHA256 = hex.EncodeToString(digest[:])
		ack, operator, err := r.cfg.ValidateSupervisor(raw)
		if err != nil {
			results[i].Denied = true
		} else {
			results[i].Ack, results[i].Operator = ack, operator
		}
	}
	return results
}

func TestSupervisorBatchCountsOrderingCopiesAndStages(t *testing.T) {
	for _, count := range []int{0, 1, 8, 9, 32} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			r := batchRig(t, count)
			calls, writes := 0, 0
			writer := r.s.write
			r.s.write = func(path string, data []byte) error { writes++; return writer(path, data) }
			marker := &struct{}{}
			ctx := context.WithValue(context.Background(), marker, "trace-context")
			r.s.cfg.ValidateSupervisorsContext = func(got context.Context, proofs []json.RawMessage) ([]supervision.Verification, error) {
				if got.Value(marker) != "trace-context" {
					t.Fatal("context lost")
				}
				chunks := (count + 7) / 8
				if len(proofs) < 1 || len(proofs) > 8 || writes != calls/chunks {
					t.Fatal("chunk bounds or commit stage", calls, writes, len(proofs))
				}
				start := (calls % chunks) * 8
				results := batchVerify(r, proofs)
				for i, result := range results {
					if result.Operator.Principal != fmt.Sprintf("batch-operator-%02d", start+i) {
						t.Fatal("nondeterministic proof order")
					}
					// Mutation cannot touch active stored proofs or protected digest copies.
					proofs[i][0] = '!'
				}
				calls++
				return results, nil
			}
			d, err := r.s.EvaluateContext(ctx, "batch", "app:nav", r.epoch, r.telemetry())
			if err != nil {
				t.Fatal(err)
			}
			wantCalls := 2 * ((count + 7) / 8)
			if calls != wantCalls || writes != 1 {
				t.Fatal(calls, wantCalls, writes)
			}
			if count == 0 {
				if d.Action != mission.Hold || d.Record.Status.Phase != mission.Accepted {
					t.Fatal(d)
				}
			} else if d.Action != mission.Continue || d.Supervision == nil || d.Supervision.RemainingMS != 500 {
				t.Fatal("batch renewed or lost lease", d)
			}
			for _, slot := range r.s.supervision {
				if slot.active != nil && !json.Valid(slot.active.proof) {
					t.Fatal("callback mutated retained proof")
				}
			}
		})
	}
}

func TestSupervisorBatchRejectsBatchOnlyConfiguration(t *testing.T) {
	r := setup(t)
	if err := r.s.Close(); err != nil {
		t.Fatal(err)
	}
	r.cfg.ValidateSupervisorsContext = func(context.Context, []json.RawMessage) ([]supervision.Verification, error) { return nil, nil }
	if s, err := Open(r.path, r.cfg); err == nil {
		s.Close()
		t.Fatal("batch-only configuration accepted")
	}
}

func TestSupervisorBatchSelectiveDenialAndFixedDeadlines(t *testing.T) {
	r := batchRig(t, 2)
	denied := false
	r.s.cfg.ValidateSupervisorsContext = func(_ context.Context, proofs []json.RawMessage) ([]supervision.Verification, error) {
		results := batchVerify(r, proofs)
		for i := range results {
			if results[i].Operator.Principal == "batch-operator-00" {
				results[i] = supervision.Verification{InputSHA256: results[i].InputSHA256, Denied: true}
				denied = true
			}
		}
		r.advance(20 * time.Millisecond)
		return results, nil
	}
	d := r.evaluate(t, "batch", r.telemetry())
	if !denied || d.Action != mission.Continue || d.Supervision.RemainingMS != 460 {
		t.Fatal(d)
	}
	if r.s.supervision[supervisionKey{"batch", "batch-operator-00"}].active != nil {
		t.Fatal("denied operator retained")
	}
	if d.Supervision.ExpiresAtUnixMS != r.wall.Add(460*time.Millisecond).UnixMilli() {
		t.Fatal("expiry extended")
	}
	// Shared gateway denial is represented as all ordered entries denied.
	r.s.cfg.ValidateSupervisorsContext = func(_ context.Context, proofs []json.RawMessage) ([]supervision.Verification, error) {
		results := batchVerify(r, proofs)
		for i := range results {
			results[i] = supervision.Verification{InputSHA256: results[i].InputSHA256, Denied: true}
		}
		return results, nil
	}
	d = r.evaluate(t, "batch", r.telemetry())
	if d.Action != mission.Hold || d.Record.Status.Phase != mission.Held {
		t.Fatal(d)
	}
}

func TestSupervisorBatchLaterChunkGlobalFailureClearsAllAndLatches(t *testing.T) {
	r := batchRig(t, 9)
	if d := r.evaluate(t, "batch", r.telemetry()); d.Action != mission.Continue {
		t.Fatal(d)
	}
	pending := r.challenge(t, "batch", "operator-c", "pending")
	cause := errors.New("checkpoint unavailable")
	calls := 0
	r.s.cfg.ValidateSupervisorsContext = func(_ context.Context, proofs []json.RawMessage) ([]supervision.Verification, error) {
		calls++
		if calls == 2 {
			return batchVerify(r, proofs), cause
		}
		return batchVerify(r, proofs), nil
	}
	d, err := r.s.Evaluate("batch", "app:nav", r.epoch, r.telemetry())
	if !errors.Is(err, cause) || d.Action != mission.Stop || d.Supervision != nil || d.Record.Status.Version != 0 || calls != 2 || len(r.s.supervision) != 0 {
		t.Fatal(d, err, calls, len(r.s.supervision))
	}
	record, err := r.s.Get("batch")
	if err != nil || record.Status.Phase != mission.Held {
		t.Fatal(record, err)
	}
	r.s.cfg.ValidateSupervisorsContext = func(_ context.Context, proofs []json.RawMessage) ([]supervision.Verification, error) {
		return batchVerify(r, proofs), nil
	}
	if _, err = r.s.AcknowledgeSupervision(proof(pending, pending.OperatorPrincipal)); err == nil {
		t.Fatal("pending ACK restored coverage")
	}
	if _, err = r.s.SupervisionChallenge("batch", SpecDigest(record.Spec), "fresh-request-0000", r.operators["operator-a"]); err == nil {
		t.Fatal("fresh policy resumed held task")
	}
	data, err := os.ReadFile(r.path)
	if err != nil {
		t.Fatal(err)
	}
	var disk envelope
	var saved state
	if json.Unmarshal(data, &disk) != nil || json.Unmarshal(disk.State, &saved) != nil || saved.Records["batch"].Record.Status.Phase != mission.Held {
		t.Fatal("loss was not committed before reopen")
	}
	if err = r.s.Close(); err != nil {
		t.Fatal(err)
	}
	r.s, err = Open(r.path, r.cfg)
	if err != nil {
		t.Fatal(err)
	}
	record, err = r.s.Get("batch")
	if err != nil || record.Status.Phase != mission.Failed {
		t.Fatal("interrupted motion did not fail on reopen", record, err)
	}
}

func TestSupervisorBatchMalformedResultsAreGlobal(t *testing.T) {
	changes := map[string]func([]supervision.Verification) []supervision.Verification{
		"missing": func(v []supervision.Verification) []supervision.Verification { return v[:1] },
		"order":   func(v []supervision.Verification) []supervision.Verification { v[0], v[1] = v[1], v[0]; return v },
		"digest": func(v []supervision.Verification) []supervision.Verification {
			v[0].InputSHA256 = strings.Repeat("0", 64)
			return v
		},
		"denial_payload": func(v []supervision.Verification) []supervision.Verification { v[0].Denied = true; return v },
		"task": func(v []supervision.Verification) []supervision.Verification {
			v[0].Ack.Challenge.TaskID = "other"
			return v
		},
		"run": func(v []supervision.Verification) []supervision.Verification {
			v[0].Ack.Challenge.RunID = "other"
			return v
		},
		"key": func(v []supervision.Verification) []supervision.Verification {
			v[0].Operator.KeySHA256 = strings.Repeat("f", 64)
			return v
		},
		"grant": func(v []supervision.Verification) []supervision.Verification {
			v[0].Operator.GrantID = "other"
			return v
		},
		"proof": func(v []supervision.Verification) []supervision.Verification {
			v[0].Operator.Proof = append(v[0].Operator.Proof, ' ')
			return v
		},
	}
	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			r := batchRig(t, 2)
			r.evaluate(t, "batch", r.telemetry())
			r.s.cfg.ValidateSupervisorsContext = func(_ context.Context, p []json.RawMessage) ([]supervision.Verification, error) {
				return change(batchVerify(r, p)), nil
			}
			d, err := r.s.Evaluate("batch", "app:nav", r.epoch, r.telemetry())
			if !errors.Is(err, ErrSupervisionVerification) || d.Action != mission.Stop || d.Supervision != nil || len(r.s.supervision) != 0 {
				t.Fatal(d, err)
			}
		})
	}
}

func TestSupervisorBatchGlobalLatchWriterFailureJoinsAndPoisons(t *testing.T) {
	r := batchRig(t, 2)
	r.evaluate(t, "batch", r.telemetry())
	before, err := os.ReadFile(r.path)
	if err != nil {
		t.Fatal(err)
	}
	authorityErr, writeErr := errors.New("global policy unavailable"), errors.New("task write failed")
	r.s.cfg.ValidateSupervisorsContext = func(context.Context, []json.RawMessage) ([]supervision.Verification, error) { return nil, authorityErr }
	r.s.write = func(string, []byte) error { return writeErr }
	d, err := r.s.Evaluate("batch", "app:nav", r.epoch, r.telemetry())
	if !errors.Is(err, authorityErr) || !errors.Is(err, writeErr) || d.Action != mission.Stop || d.Record.Status.Version != 0 || len(r.s.supervision) != 0 || r.s.fault == nil {
		t.Fatal(d, err)
	}
	after, _ := os.ReadFile(r.path)
	if !bytes.Equal(before, after) {
		t.Fatal("failed latch claimed disk state")
	}
	if err = r.s.Close(); err != nil {
		t.Fatal(err)
	}
	r.s, err = Open(r.path, r.cfg)
	if err != nil {
		t.Fatal(err)
	}
	record, err := r.s.Get("batch")
	if err != nil || record.Status.Phase != mission.Failed {
		t.Fatal(record, err)
	}
}

func TestSupervisorBatchGlobalLatchQuotaFailureCannotBridgeAfterRecovery(t *testing.T) {
	r := batchRig(t, 2)
	r.evaluate(t, "batch", r.telemetry())
	pending := r.challenge(t, "batch", "operator-c", "pending")
	before, err := os.ReadFile(r.path)
	if err != nil {
		t.Fatal(err)
	}
	// Valid serializable audit pressure reaches the real prewrite quota branch,
	// which leaves the store usable (unlike the deliberately poisoned writer).
	audit := r.s.state.Audit
	r.s.state.Audit = append(append([]Audit(nil), audit...), Audit{Event: strings.Repeat("x", maxBytes)})
	cause := errors.New("global policy unavailable")
	writes := 0
	writer := r.s.write
	r.s.write = func(path string, data []byte) error { writes++; return writer(path, data) }
	r.s.cfg.ValidateSupervisorsContext = func(context.Context, []json.RawMessage) ([]supervision.Verification, error) { return nil, cause }
	d, err := r.s.Evaluate("batch", "app:nav", r.epoch, r.telemetry())
	joined, joinedOK := err.(interface{ Unwrap() []error })
	if !errors.Is(err, cause) || !joinedOK || len(joined.Unwrap()) != 2 || writes != 0 || d.Action != mission.Stop || d.Record.Status.Version != 0 || len(r.s.supervision) != 0 || r.s.fault != nil || r.s.state.Records["batch"].Record.Status.Phase != mission.Running {
		t.Fatal(d, err)
	}
	after, _ := os.ReadFile(r.path)
	if !bytes.Equal(before, after) {
		t.Fatal("quota refusal changed disk")
	}
	r.s.state.Audit = audit // Remove only this test's injected quota pressure.
	r.s.cfg.ValidateSupervisorsContext = func(_ context.Context, p []json.RawMessage) ([]supervision.Verification, error) {
		return batchVerify(r, p), nil
	}
	if _, err = r.s.AcknowledgeSupervision(proof(pending, pending.OperatorPrincipal)); err == nil {
		t.Fatal("old pending challenge survived global failure")
	}
	record, _ := r.s.Get("batch")
	if _, err = r.s.SupervisionChallenge("batch", SpecDigest(record.Spec), "fresh-request-0000", r.operators["operator-a"]); !errors.Is(err, ErrSupervisionUnavailable) {
		t.Fatal("renewal bridged uncommitted loss", err)
	}
	record, err = r.s.Get("batch")
	if err != nil || record.Status.Phase != mission.Held {
		t.Fatal("fresh challenge did not latch loss", record, err)
	}
}

func TestSupervisorBatchPostwriteFailureAndMissionLast(t *testing.T) {
	for _, global := range []bool{false, true} {
		t.Run(fmt.Sprint(global), func(t *testing.T) {
			r := batchRig(t, 2)
			r.evaluate(t, "batch", r.telemetry())
			calls, missions, writes := 0, 0, 0
			cause := errors.New("authority changed after task write")
			writer := r.s.write
			r.s.write = func(path string, data []byte) error { writes++; return writer(path, data) }
			r.s.cfg.ValidateSupervisorsContext = func(_ context.Context, p []json.RawMessage) ([]supervision.Verification, error) {
				calls++
				if calls == 2 && writes != 1 {
					t.Fatal("postwrite stage skipped commit")
				}
				if calls == 2 && global {
					return nil, cause
				}
				return batchVerify(r, p), nil
			}
			r.s.cfg.ValidateAuthority = func(Record) error {
				missions++
				if calls == 2 {
					return cause
				}
				return nil
			}
			d, err := r.s.Evaluate("batch", "app:nav", r.epoch, r.telemetry())
			if !errors.Is(err, cause) || d.Action != mission.Stop || d.Supervision != nil || calls != 2 || writes != 2 {
				t.Fatal(d, err, calls, writes)
			}
			if global {
				if missions != 1 || len(r.s.supervision) != 0 || d.Record.Status.Version != 0 {
					t.Fatal("global failure kept coverage or claimed status")
				}
			} else if missions != 2 || d.Record.Status.Phase != mission.Failed {
				t.Fatal("mission-last fence lost", d, missions)
			}
		})
	}
}

func TestSupervisorBatchExpiryAndRollbackAfterBlockingCallback(t *testing.T) {
	for _, rollback := range []bool{false, true} {
		t.Run(fmt.Sprint(rollback), func(t *testing.T) {
			r := batchRig(t, 9)
			r.evaluate(t, "batch", r.telemetry())
			calls := 0
			r.s.cfg.ValidateSupervisorsContext = func(_ context.Context, p []json.RawMessage) ([]supervision.Verification, error) {
				results := batchVerify(r, p)
				calls++
				if calls == 2 {
					if rollback {
						r.advance(-time.Millisecond)
					} else {
						r.advance(501 * time.Millisecond)
					}
				}
				return results, nil
			}
			d, err := r.s.Evaluate("batch", "app:nav", r.epoch, r.telemetry())
			if d.Action == mission.Continue || d.Supervision != nil {
				t.Fatal("delayed callback retained motion", d, err)
			}
			if rollback && (err == nil || len(r.s.supervision) != 0) {
				t.Fatal("clock failure retained slots", err)
			}
			if !rollback && (err != nil || d.Record.Status.Phase != mission.Held) {
				t.Fatal(d, err)
			}
		})
	}
}

func TestSupervisorBatchProofBoundFailsBeforeCallback(t *testing.T) {
	r := batchRig(t, 1)
	r.evaluate(t, "batch", r.telemetry())
	active := r.s.supervision[supervisionKey{"batch", "batch-operator-00"}].active
	active.proof = json.RawMessage(`"` + strings.Repeat("x", supervision.MaxVerificationProofBytes) + `"`)
	calls := 0
	r.s.cfg.ValidateSupervisorsContext = func(context.Context, []json.RawMessage) ([]supervision.Verification, error) {
		calls++
		return nil, nil
	}
	d, err := r.s.Evaluate("batch", "app:nav", r.epoch, r.telemetry())
	if !errors.Is(err, ErrSupervisionVerification) || calls != 0 || d.Action != mission.Stop || len(r.s.supervision) != 0 {
		t.Fatal("oversized retained proof reached verifier", d, err, calls)
	}
}

func TestSupervisorBatchCompletionExpiryIsSelective(t *testing.T) {
	r := batchRig(t, 2)
	limit := r.wall.Add(time.Millisecond).UnixMilli()
	calls := 0
	r.s.cfg.ValidateSupervisorsContext = func(_ context.Context, p []json.RawMessage) ([]supervision.Verification, error) {
		results := batchVerify(r, p)
		for i := range results {
			if results[i].Operator.Principal == "batch-operator-00" {
				results[i].Operator.ValidUntilUnixMS = limit
			}
		}
		calls++
		if calls == 1 {
			r.advance(2 * time.Millisecond)
		}
		return results, nil
	}
	d := r.evaluate(t, "batch", r.telemetry())
	if d.Action != mission.Continue || d.Supervision == nil || d.Supervision.RemainingMS != 498 || calls != 2 {
		t.Fatal("one completion-time expiry suppressed current operator", d, calls)
	}
	if r.s.supervision[supervisionKey{"batch", "batch-operator-00"}].active != nil {
		t.Fatal("operator expiring during callback retained coverage")
	}
}
