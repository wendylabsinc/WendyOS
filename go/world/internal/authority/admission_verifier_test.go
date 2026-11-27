package authority

import (
	"errors"
	"path/filepath"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/runtimeobs"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

// Exposing only ReplayStore deliberately hides the optional admission method.
// Existing custom implementations must retain checkpoint-before-consume order.
type admissionLegacyStore struct {
	ReplayStore
	calls         []string
	checkpointErr error
	consumeErr    error
}

func (s *admissionLegacyStore) Checkpoint(revision uint64, digest string, now time.Time) error {
	s.calls = append(s.calls, "checkpoint")
	if s.checkpointErr != nil {
		return s.checkpointErr
	}
	return s.ReplayStore.Checkpoint(revision, digest, now)
}
func (s *admissionLegacyStore) Consume(key string, expires, now time.Time) error {
	s.calls = append(s.calls, "consume")
	if s.consumeErr != nil {
		return s.consumeErr
	}
	return s.ReplayStore.Consume(key, expires, now)
}
func admissionDiskFixture(t *testing.T, max int) (*fixture, *ReplayCache, *runtimeobs.Collector) {
	t.Helper()
	cache, err := OpenReplay(filepath.Join(t.TempDir(), "replay.json"), max)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cache.Close() })
	f := newFixture(t, cache)
	metrics := runtimeobs.New()
	cache.SetMetrics(metrics)      // Do not count initial revocation commissioning.
	f.now = f.now.Add(time.Second) // Force a changed durable clock checkpoint.
	return f, cache, metrics
}
func TestVerifierAdmissionCommitsOnceAndRetainsNonce(t *testing.T) {
	f, cache, metrics := admissionDiskFixture(t, 8)
	r := f.request(t, "read", "admission_nonce_0001", nil)
	id, err := f.verifier.Verify(r, "read", nil)
	if err != nil || id.Principal != f.grant.Grant.Principal {
		t.Fatalf("admission: %v", err)
	}
	if got := metricDuration(t, metrics, "replay.checkpoint_commit").Count; got != 1 {
		t.Fatalf("accepted request committed %d times; want 1", got)
	}
	if cache.state.LastUnixMS != f.now.UnixMilli() || len(cache.state.Nonces) != 1 {
		t.Fatal("acceptance omitted clock or nonce")
	}
	if _, err = f.verifier.Verify(r, "read", nil); !errors.Is(err, ErrReplay) {
		t.Fatalf("duplicate: %v", err)
	}
	if got := metricDuration(t, metrics, "replay.checkpoint_commit").Count; got != 1 {
		t.Fatalf("same-clock duplicate added durable work: %d", got)
	}
}
func TestVerifierAdmissionDenialClockSurvivesReopen(t *testing.T) {
	for _, denial := range []string{"replay", "quota"} {
		t.Run(denial, func(t *testing.T) {
			f, cache, metrics := admissionDiskFixture(t, 1)
			accepted := f.request(t, "read", "admission_nonce_0001", nil)
			if _, err := f.verifier.Verify(accepted, "read", nil); err != nil {
				t.Fatal(err)
			}
			acceptedAt := f.now
			f.now = f.now.Add(2 * time.Second)
			denied := accepted
			if denial == "quota" {
				denied = f.request(t, "read", "admission_nonce_0002", nil)
			}
			if _, err := f.verifier.Verify(denied, "read", nil); err == nil || (denial == "replay" && !errors.Is(err, ErrReplay)) {
				t.Fatalf("%s denial: %v", denial, err)
			}
			if got := metricDuration(t, metrics, "replay.checkpoint_commit").Count; got != 2 {
				t.Fatalf("denial must persist advanced clock once: %d", got)
			}
			path, deniedAt := cache.path, f.now
			if err := cache.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := OpenReplay(path, 1)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			if reopened.state.LastUnixMS != deniedAt.UnixMilli() || len(reopened.state.Nonces) != 1 {
				t.Fatal("denial lost clock floor or consumed rejected nonce")
			}
			f.verifier.cfg.Replay = reopened
			f.now = acceptedAt.Add(time.Second)
			if err = reopened.ValidateCheckpoint(reopened.state.Revision, reopened.state.Digest, f.now); err == nil {
				t.Fatal("reopened ledger accepted rollback behind denied request")
			}
			if _, err = f.verifier.Verify(f.request(t, "read", "admission_nonce_0003", nil), "read", nil); err == nil {
				t.Fatal("verifier accepted rollback after denied request")
			}
		})
	}
}
func TestVerifierAdmissionLegacyStoreOrderAndFailures(t *testing.T) {
	failure := errors.New("test store refusal")
	for _, stage := range []string{"success", "checkpoint", "consume"} {
		t.Run(stage, func(t *testing.T) {
			store := &admissionLegacyStore{ReplayStore: NewMemoryReplay(8)}
			f := newFixture(t, store)
			store.calls = nil
			if stage == "checkpoint" {
				store.checkpointErr = failure
			}
			if stage == "consume" {
				store.consumeErr = failure
			}
			id, err := f.verifier.Verify(f.request(t, "read", "admission_nonce_0001", nil), "read", nil)
			want := []string{"checkpoint", "consume"}
			if stage == "checkpoint" {
				want = []string{"checkpoint"}
			}
			if !reflect.DeepEqual(store.calls, want) {
				t.Fatalf("calls %v, want %v", store.calls, want)
			}
			if stage == "success" {
				if err != nil || id.Principal == "" {
					t.Fatalf("legacy success: %v", err)
				}
			} else if !errors.Is(err, failure) || id.Principal != "" {
				t.Fatalf("persistence refusal returned identity: %v", err)
			}
		})
	}
}
func TestVerifierAdmissionPolicyRefusalsDoNotWrite(t *testing.T) {
	cases := []string{"request-signature", "grant-signature", "missing-scope", "foreign-site", "wrong-publisher", "stale-revocations", "revoked-principal"}
	for _, name := range cases {
		t.Run(name, func(t *testing.T) {
			f, cache, _ := admissionDiskFixture(t, 8)
			operation := "read"
			switch name {
			case "missing-scope":
				operation = "stop"
			case "foreign-site":
				g := f.grant.Grant
				g.Site = "another-site"
				var err error
				f.grant, err = SignGrant(g, f.root)
				if err != nil {
					t.Fatal(err)
				}
			case "wrong-publisher":
				operation = "publish"
				g := f.grant.Grant
				g.PublishAudience = "node-2"
				var err error
				f.grant, err = SignGrant(g, f.root)
				if err != nil {
					t.Fatal(err)
				}
			case "grant-signature":
				f.grant.Signature[0] ^= 1
			case "stale-revocations":
				f.now = time.UnixMilli(f.rev.Revocations.ExpiresAtUnixMS)
			case "revoked-principal":
				rev := f.rev.Revocations
				rev.Revision++
				rev.Principals = []string{f.grant.Grant.Principal}
				signed, err := SignRevocations(rev, f.root)
				if err != nil {
					t.Fatal(err)
				}
				if err = f.verifier.UpdateRevocations(signed); err != nil {
					t.Fatal(err)
				}
			}
			metrics := runtimeobs.New()
			cache.SetMetrics(metrics)
			before := cache.state.LastUnixMS
			f.now = f.now.Add(time.Millisecond)
			r := f.request(t, operation, "admission_nonce_0001", nil)
			if name == "request-signature" {
				r.Signature[0] ^= 1
			}
			if id, err := f.verifier.Verify(r, operation, nil); err == nil || id.Principal != "" {
				t.Fatalf("policy refusal returned identity: %v", err)
			}
			if metricDuration(t, metrics, "replay.checkpoint_commit").Count != 0 || len(cache.state.Nonces) != 0 || cache.state.LastUnixMS != before {
				t.Fatal("rejected policy changed durable state")
			}
		})
	}
}
func TestVerifierAdmissionOngoingChecksStillCheckpoint(t *testing.T) {
	f, cache, metrics := admissionDiskFixture(t, 8)
	if _, err := f.verifier.VerifyGrant(f.grant, "read"); err != nil {
		t.Fatal(err)
	}
	f.now = f.now.Add(time.Second)
	component := world.Component{Key: world.Key{Tenant: "tenant", Site: "office", Source: f.grant.Grant.Source}, TTL: time.Second}
	record, err := SignRecord(f.grant, f.key, component, f.now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.verifier.VerifyRecord(record); err != nil {
		t.Fatal(err)
	}
	if got := metricDuration(t, metrics, "replay.checkpoint_commit").Count; got != 2 {
		t.Fatalf("ongoing checks committed %d times; want 2", got)
	}
	if len(cache.state.Nonces) != 0 || cache.state.LastUnixMS != f.now.UnixMilli() {
		t.Fatal("ongoing checks changed nonce semantics or omitted clock floor")
	}
}
func TestVerifierAdmissionSharedLedgerHasOneWinner(t *testing.T) {
	f, _, metrics := admissionDiskFixture(t, 8)
	other, err := NewVerifier(f.verifier.cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err = other.UpdateRevocations(f.rev); err != nil {
		t.Fatal(err)
	}
	// The update above advanced the clock. Move again to require admission work.
	f.now = f.now.Add(time.Second)
	before := metricDuration(t, metrics, "replay.checkpoint_commit").Count
	request := f.request(t, "read", "admission_nonce_0001", nil)
	var winners atomic.Int32
	var wg sync.WaitGroup
	for _, verifier := range []*Verifier{f.verifier, other} {
		wg.Add(1)
		go func(v *Verifier) {
			defer wg.Done()
			_, err := v.Verify(request, "read", nil)
			if err == nil {
				winners.Add(1)
			} else if !errors.Is(err, ErrReplay) {
				t.Errorf("concurrent verification: %v", err)
			}
		}(verifier)
	}
	wg.Wait()
	if winners.Load() != 1 {
		t.Fatalf("accepted %d copies", winners.Load())
	}
	if got := metricDuration(t, metrics, "replay.checkpoint_commit").Count - before; got != 1 {
		t.Fatalf("concurrent admission committed %d times", got)
	}
}

// A failed optional transaction must never trigger a legacy retry: the store
// may already have renamed its snapshot and be refusing an uncertain commit.
type admissionFailingCombinedStore struct {
	*admissionLegacyStore
	admissions int
	failure    error
}

func (s *admissionFailingCombinedStore) CheckpointAndConsume(revision uint64, digest, key string, expires, now time.Time) error {
	s.admissions++
	return s.failure
}
func TestVerifierAdmissionCombinedFailureDoesNotFallback(t *testing.T) {
	failure := errors.New("uncertain combined commit")
	store := &admissionFailingCombinedStore{admissionLegacyStore: &admissionLegacyStore{ReplayStore: NewMemoryReplay(8)}, failure: failure}
	f := newFixture(t, store)
	store.calls = nil
	id, err := f.verifier.Verify(f.request(t, "read", "admission_nonce_0001", nil), "read", nil)
	if !errors.Is(err, failure) || id.Principal != "" {
		t.Fatalf("combined failure returned identity: %v", err)
	}
	if store.admissions != 1 || len(store.calls) != 0 {
		t.Fatalf("failed combined transaction retried: admissions=%d legacy=%v", store.admissions, store.calls)
	}
}
