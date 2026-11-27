package authority

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

// The decorator advances an explicit trusted clock only after the real replay
// operation returns. No sleep, simulation observation clock or signature bypass
// is needed to reproduce authority expiring inside a blocking checkpoint.
type instructionCompletionReplay struct {
	ReplayStore
	checkpoints, validations int
	afterCheckpoint          func(int)
	afterValidation          func()
	checkpointErrorAt        int
	validationError          error
}

var errInstructionCompletionIO = errors.New("instruction completion test I/O failure")

func (r *instructionCompletionReplay) Checkpoint(revision uint64, digest string, now time.Time) error {
	r.checkpoints++
	if r.checkpoints == r.checkpointErrorAt {
		return errInstructionCompletionIO
	}
	if err := r.ReplayStore.Checkpoint(revision, digest, now); err != nil {
		return err
	}
	if r.afterCheckpoint != nil {
		r.afterCheckpoint(r.checkpoints)
	}
	return nil
}

func (r *instructionCompletionReplay) ValidateCheckpoint(revision uint64, digest string, now time.Time) error {
	r.validations++
	if r.validationError != nil {
		return r.validationError
	}
	if err := r.ReplayStore.ValidateCheckpoint(revision, digest, now); err != nil {
		return err
	}
	if r.afterValidation != nil {
		r.afterValidation()
	}
	return nil
}

func completionFixture(t *testing.T) (*fixture, SignedInstruction, *instructionCompletionReplay, *ReplayCache, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "replay.json")
	cache, err := OpenReplay(path, 100)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cache.Close() })
	r := &instructionCompletionReplay{ReplayStore: cache}
	f := newFixture(t, r)
	i := operatorInstruction(t, f)
	// Ensure the first instruction checkpoint really advances durable state.
	f.now = f.now.Add(time.Millisecond)
	r.checkpoints, r.validations = 0, 0
	return f, i, r, cache, path
}

func completionRevocations(t *testing.T, f *fixture, expiry time.Time) {
	t.Helper()
	r := f.rev.Revocations
	r.Revision++
	r.IssuedAtUnixMS = f.now.UnixMilli()
	r.ExpiresAtUnixMS = expiry.UnixMilli()
	var err error
	f.rev, err = SignRevocations(r, f.root)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.verifier.UpdateRevocations(f.rev); err != nil {
		t.Fatal(err)
	}
}

func completionResign(t *testing.T, f *fixture, i SignedInstruction) SignedInstruction {
	t.Helper()
	var err error
	f.grant, err = SignGrant(i.Grant.Grant, f.root)
	if err != nil {
		t.Fatal(err)
	}
	i, err = SignInstruction(f.grant, f.key, i.Audience, i.Operation, i.Payload,
		time.UnixMilli(i.IssuedAtUnixMS), time.Duration(i.ExpiresAtUnixMS-i.IssuedAtUnixMS)*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	return i
}

func TestInstructionCompletionRejectsElapsedAuthority(t *testing.T) {
	for _, boundary := range []string{"revocations_expired", "grant_expired", "instruction_expired", "wall_regressed", "wall_regressed_within_millisecond"} {
		for _, delayAt := range []string{"first_checkpoint", "second_checkpoint", "read_only_checkpoint"} {
			t.Run(boundary+"/"+delayAt, func(t *testing.T) {
				f, i, replay, _, _ := completionFixture(t)
				var want error
				advance := 100 * time.Millisecond
				switch boundary {
				case "revocations_expired":
					completionRevocations(t, f, f.now.Add(advance))
					want = ErrStaleRevocations
				case "grant_expired":
					i.Grant.Grant.NotAfterUnixMS = f.now.Add(advance).UnixMilli()
					i = completionResign(t, f, i)
					want = ErrDenied
				case "instruction_expired":
					i.ExpiresAtUnixMS = f.now.Add(advance).UnixMilli()
					i = completionResign(t, f, i)
					want = ErrDenied
				case "wall_regressed":
					advance = -time.Millisecond
				case "wall_regressed_within_millisecond":
					f.now = f.now.Add(500 * time.Microsecond)
					advance = -time.Nanosecond
				}
				// Leave a genuine durable checkpoint to advance after any setup.
				if boundary == "revocations_expired" {
					f.now = f.now.Add(time.Millisecond)
				}
				replay.checkpoints, replay.validations = 0, 0
				advanceClock := func() { f.now = f.now.Add(advance) }
				switch delayAt {
				case "first_checkpoint":
					replay.afterCheckpoint = func(n int) {
						if n == 1 {
							advanceClock()
						}
					}
				case "second_checkpoint":
					replay.afterCheckpoint = func(n int) {
						if n == 2 {
							advanceClock()
						}
					}
				case "read_only_checkpoint":
					replay.afterValidation = advanceClock
				}
				id, err := f.verifier.VerifyInstruction(i, "mission")
				if err == nil || (want != nil && !errors.Is(err, want)) || id.Principal != "" {
					t.Fatalf("authority survived %s during %s: identity=%q error=%v", boundary, delayAt, id.Principal, err)
				}
			})
		}
	}
}

func TestInstructionCompletionPreservesDurableAdmissionFloor(t *testing.T) {
	for _, deny := range []bool{false, true} {
		t.Run(map[bool]string{false: "allowed", true: "denied"}[deny], func(t *testing.T) {
			f, i, replay, cache, path := completionFixture(t)
			admittedAt := f.now
			advance := 2 * time.Millisecond
			if deny {
				advance = 30 * time.Minute
			}
			replay.afterCheckpoint = func(n int) {
				if n == 1 {
					f.now = f.now.Add(advance)
				}
			}
			_, err := f.verifier.VerifyInstruction(i, "mission")
			if (err != nil) != deny {
				t.Fatalf("deny=%v, error=%v", deny, err)
			}
			if replay.checkpoints != 2 || replay.validations != 1 {
				t.Fatalf("completion changed checkpoint count: writes=%d reads=%d", replay.checkpoints, replay.validations)
			}
			if err = cache.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := OpenReplay(path, 100)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			encoded, _ := json.Marshal(f.rev)
			if reopened.state.LastUnixMS != admittedAt.UnixMilli() || reopened.state.Revision != f.rev.Revocations.Revision || reopened.state.Digest != hash(encoded) || len(reopened.state.Nonces) != 0 {
				t.Fatalf("durable admission changed: %+v", reopened.state)
			}
			if err = reopened.ValidateCheckpoint(reopened.state.Revision, reopened.state.Digest, admittedAt.Add(-time.Millisecond)); err == nil {
				t.Fatal("reopened admission floor allowed rollback")
			}
		})
	}
}

func TestInstructionCompletionReplayFailureReturnsNoAuthority(t *testing.T) {
	for _, stage := range []string{"first_checkpoint", "second_checkpoint", "read_only_checkpoint"} {
		t.Run(stage, func(t *testing.T) {
			f, i, replay, _, _ := completionFixture(t)
			switch stage {
			case "first_checkpoint":
				replay.checkpointErrorAt = 1
			case "second_checkpoint":
				replay.checkpointErrorAt = 2
			case "read_only_checkpoint":
				replay.validationError = errInstructionCompletionIO
			}
			id, err := f.verifier.VerifyInstruction(i, "mission")
			if !errors.Is(err, errInstructionCompletionIO) || id.Principal != "" {
				t.Fatalf("replay failure returned authority: identity=%q error=%v", id.Principal, err)
			}
		})
	}
}

func TestInstructionCompletionRefusesConcurrentDurableFloorAdvance(t *testing.T) {
	f, i, replay, cache, _ := completionFixture(t)
	replay.afterCheckpoint = func(n int) {
		if n != 2 {
			return
		}
		// A different user of the shared ledger advances its durable floor.
		// The instruction's older admission sample must not overwrite it or
		// claim that its own checkpoint reserved that later time.
		f.now = f.now.Add(time.Millisecond)
		if err := cache.Checkpoint(cache.state.Revision, cache.state.Digest, f.now); err != nil {
			t.Fatal(err)
		}
	}
	if id, err := f.verifier.VerifyInstruction(i, "mission"); err == nil || id.Principal != "" {
		t.Fatalf("advanced durable floor accepted older admission: identity=%q error=%v", id.Principal, err)
	}
	if cache.state.LastUnixMS != f.now.UnixMilli() || replay.checkpoints != 2 || replay.validations != 1 {
		t.Fatal("completion replaced or retried the concurrent durable floor")
	}
}

func TestInstructionCompletionExpiredRoleStop(t *testing.T) {
	for _, mode := range []string{"already_expired", "expires_during_completion", "stale_revocations", "expired_stop"} {
		t.Run(mode, func(t *testing.T) {
			f, i, replay, _, _ := completionFixture(t)
			i.Operation = "stop"
			i.Grant.Grant.NotAfterUnixMS = f.now.Add(-time.Millisecond).UnixMilli()
			i.IssuedAtUnixMS = f.now.UnixMilli()
			i.ExpiresAtUnixMS = f.now.Add(time.Minute).UnixMilli()
			if mode == "expires_during_completion" {
				i.Grant.Grant.NotAfterUnixMS = f.now.Add(time.Millisecond).UnixMilli()
			}
			if mode == "stale_revocations" {
				completionRevocations(t, f, f.now.Add(time.Millisecond))
			}
			if mode == "expired_stop" {
				i.ExpiresAtUnixMS = f.now.Add(time.Millisecond).UnixMilli()
			}
			i = completionResign(t, f, i)
			replay.checkpoints, replay.validations = 0, 0
			replay.afterValidation = func() { f.now = f.now.Add(2 * time.Millisecond) }
			id, err := f.verifier.VerifyInstruction(i, "stop")
			deny := mode == "stale_revocations" || mode == "expired_stop"
			if (err != nil) != deny || (deny && id.Principal != "") || (!deny && !id.NotAfter.Equal(time.UnixMilli(i.ExpiresAtUnixMS))) {
				t.Fatalf("expired-role stop policy changed: mode=%s identity=%q expiry=%v error=%v", mode, id.Principal, id.NotAfter, err)
			}
		})
	}
}

func TestInstructionCompletionHistoryHasNoCurrentAuthorityOrReplayIO(t *testing.T) {
	f, i, replay, _, _ := completionFixture(t)
	r := f.rev.Revocations
	r.Revision++
	r.Principals = []string{i.Grant.Grant.Principal}
	revoked, err := SignRevocations(r, f.root)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.verifier.UpdateRevocations(revoked); err != nil {
		t.Fatal(err)
	}
	f.now = f.now.Add(2 * time.Hour)
	replay.checkpoints, replay.validations = 0, 0
	replay.checkpointErrorAt = 1
	replay.validationError = errInstructionCompletionIO
	if id, err := f.verifier.VerifyInstructionHistoryForAudience(i, "mission", "node-1"); err != nil || id.Principal != i.Grant.Grant.Principal {
		t.Fatalf("retained signed history lost: %v", err)
	}
	if _, err = f.verifier.VerifyInstruction(i, "mission"); err == nil {
		t.Fatal("history authorized current execution")
	}
	i.Payload = json.RawMessage(`{"changed":true}`)
	if _, err = f.verifier.VerifyInstructionHistoryForAudience(i, "mission", "node-1"); err == nil {
		t.Fatal("history accepted altered signed content")
	}
	if replay.checkpoints != 0 || replay.validations != 0 {
		t.Fatal("history acquired current durable authority")
	}
}
