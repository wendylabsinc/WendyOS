package node

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/authority"
)

func selectionProofs(t *testing.T, f *fixture, tied bool) (RepairRecord, RepairRecord) {
	t.Helper()
	c := component(f, 1, 1)
	stamp := time.Now().Add(-2 * time.Second)
	a, err := authority.SignRecord(f.credentials.Grant, f.credentials.PrivateKey, c, stamp)
	if err != nil {
		t.Fatal(err)
	}
	g := f.credentials.Grant.Grant
	g.ID += "/alternate"
	grant, err := authority.SignGrant(g, f.root)
	if err != nil {
		t.Fatal(err)
	}
	if !tied {
		stamp = stamp.Add(time.Second)
	}
	b, err := authority.SignRecord(grant, f.credentials.PrivateKey, c, stamp)
	if err != nil {
		t.Fatal(err)
	}
	cap := c.Timing.SampleTime.Add(time.Minute).Round(0)
	first, second := reviewRepairRecord(t, a, cap), reviewRepairRecord(t, b, cap)
	if tied && first.Entry.ProofSHA256 > second.Entry.ProofSHA256 {
		first, second = second, first
	}
	return first, second // lower rank followed by higher rank
}

func selectionDigest(t *testing.T, f *fixture, componentDigest string) string {
	t.Helper()
	f.n.mu.Lock()
	defer f.n.mu.Unlock()
	p, ok := f.n.proofs[componentDigest]
	if !ok {
		t.Fatal("missing selected proof")
	}
	d, err := repairProofDigest(p)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestRepairProofSelectionRankAndBidirectionalConvergence(t *testing.T) {
	for _, tied := range []bool{false, true} {
		name := "signed_at"
		if tied {
			name = "digest_tie"
		}
		t.Run(name, func(t *testing.T) {
			a := newFixture(t, nil, "origin")
			b := newFixture(t, a.root, "replica")
			low, high := selectionProofs(t, a, tied)
			if _, err := a.n.applyRepairPage([]RepairRecord{low}); err != nil {
				t.Fatal(err)
			}
			if _, err := b.n.applyRepairPage([]RepairRecord{high}); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			var wg sync.WaitGroup
			failures := make(chan error, 2)
			wg.Add(2)
			round := func(local, remote *fixture) {
				defer wg.Done()
				failures <- local.n.repairRound(ctx, Peer{URL: remote.server.URL, Audience: remote.audience}, local.credentials)
			}
			go round(a, b)
			go round(b, a)
			wg.Wait()
			close(failures)
			for err := range failures {
				if err != nil {
					t.Fatal(err)
				}
			}
			// Further bounded rounds prove reverse transfers cannot swap proof
			// choice after simultaneous one-in/one-out repair has converged.
			for i := 0; i < 3; i++ {
				for _, pair := range [][2]*fixture{{a, b}, {b, a}} {
					if err := pair[0].n.repairRound(ctx, Peer{URL: pair[1].server.URL, Audience: pair[1].audience}, pair[0].credentials); err != nil {
						t.Fatal(err)
					}
				}
			}
			for _, f := range []*fixture{a, b} {
				if got := selectionDigest(t, f, high.Entry.ComponentSHA256); got != high.Entry.ProofSHA256 {
					t.Fatal("proofs did not converge", got)
				}
				if _, err := f.n.applyRepairPage([]RepairRecord{low}); err != nil {
					t.Fatal(err)
				}
				if got := selectionDigest(t, f, high.Entry.ComponentSHA256); got != high.Entry.ProofSHA256 {
					t.Fatal("lower valid proof replaced preferred proof")
				}
				shortened := low
				shortened.Entry.ExpiresAt = low.Entry.ExpiresAt.Add(-time.Second)
				if _, err := f.n.applyRepairPage([]RepairRecord{shortened}); err != nil {
					t.Fatal(err)
				}
				if selectionDigest(t, f, high.Entry.ComponentSHA256) != high.Entry.ProofSHA256 ||
					!publicationCheckpoint(t, f).Entries[0].ExpiresAt.Equal(shortened.Entry.ExpiresAt) {
					t.Fatal("proof preference discarded tighter expiry")
				}
			}
		})
	}
}

func TestRepairProofSelectionRevokedPreferredGrantDoesNotBlockReplacement(t *testing.T) {
	f := newFixture(t, nil, "origin")
	low, high := selectionProofs(t, f, false)
	if _, err := f.n.applyRepairPage([]RepairRecord{high}); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	rev, err := authority.SignRevocations(authority.Revocations{Version: authority.Version, Tenant: "tenant", Site: "office", Domain: "simulation", Revision: 2, IssuedAtUnixMS: now.Add(-time.Second).UnixMilli(), ExpiresAtUnixMS: now.Add(time.Minute).UnixMilli(), GrantIDs: []string{high.Record.Grant.Grant.ID}}, f.root)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.v.UpdateRevocations(rev); err != nil {
		t.Fatal(err)
	}
	if _, err := f.v.VerifyRecords([]authority.SignedRecord{high.Record}); !errors.Is(err, authority.ErrDenied) {
		t.Fatal("preferred proof not independently denied", err)
	}
	if _, err := f.n.applyRepairPage([]RepairRecord{low}); err != nil {
		t.Fatal(err)
	}
	if got := selectionDigest(t, f, low.Entry.ComponentSHA256); got != low.Entry.ProofSHA256 {
		t.Fatal("revoked proof blocked authorized replacement")
	}
	if result := query(t, f); len(result.Components) != 1 {
		t.Fatal("replacement is not currently visible")
	}
}

type selectionReplay struct {
	authority.ReplayStore
	calls    int
	onSecond func() error
}

func (r *selectionReplay) Checkpoint(revision uint64, digest string, now time.Time) error {
	r.calls++
	if r.calls == 2 && r.onSecond != nil {
		if err := r.onSecond(); err != nil {
			return err
		}
	}
	return r.ReplayStore.Checkpoint(revision, digest, now)
}

func TestRepairProofSelectionStorageAndStalePolicyRefuseWholePage(t *testing.T) {
	for _, kind := range []string{"storage", "stale_revocations"} {
		t.Run(kind, func(t *testing.T) {
			f := newFixture(t, nil, "origin")
			low, high := selectionProofs(t, f, false)
			if _, err := f.n.applyRepairPage([]RepairRecord{high}); err != nil {
				t.Fatal(err)
			}
			now := time.Now()
			replay := &selectionReplay{ReplayStore: authority.NewMemoryReplay(100)}
			v, err := authority.NewVerifier(authority.Config{RootPublicKey: f.root.Public().(ed25519.PublicKey), Tenant: "tenant", Site: "office", Domain: "simulation", Audience: f.audience, Now: func() time.Time { return now }, Replay: replay})
			if err != nil {
				t.Fatal(err)
			}
			rev, err := authority.SignRevocations(authority.Revocations{Version: authority.Version, Tenant: "tenant", Site: "office", Domain: "simulation", Revision: 1, IssuedAtUnixMS: now.Add(-time.Second).UnixMilli(), ExpiresAtUnixMS: now.Add(time.Second).UnixMilli()}, f.root)
			if err != nil {
				t.Fatal(err)
			}
			if err := v.UpdateRevocations(rev); err != nil {
				t.Fatal(err)
			}
			replay.calls = 0
			failure := errors.New("injected selection replay failure")
			want := failure
			if kind == "stale_revocations" {
				want = authority.ErrStaleRevocations
				replay.onSecond = func() error { now = now.Add(2 * time.Second); return nil }
			} else {
				replay.onSecond = func() error { return failure }
			}
			f.n.verifier = v
			beforeProof, err := os.ReadFile(f.cfg.ProofPath)
			if err != nil {
				t.Fatal(err)
			}
			beforeWorld, err := os.ReadFile(publicationPath(f))
			if err != nil {
				t.Fatal(err)
			}
			results, err := f.n.applyRepairPage([]RepairRecord{low})
			if !errors.Is(err, want) || results != nil || replay.calls != 2 {
				t.Fatal("failed verification became preference", results, err, replay.calls)
			}
			afterProof, err := os.ReadFile(f.cfg.ProofPath)
			if err != nil {
				t.Fatal(err)
			}
			afterWorld, err := os.ReadFile(publicationPath(f))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(beforeProof, afterProof) || !bytes.Equal(beforeWorld, afterWorld) {
				t.Fatal("failed preference wrote partial page")
			}
			if selectionDigest(t, f, high.Entry.ComponentSHA256) != high.Entry.ProofSHA256 {
				t.Fatal("failed preference changed memory")
			}
		})
	}
}
