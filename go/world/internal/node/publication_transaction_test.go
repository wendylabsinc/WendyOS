package node

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/runtimeobs"
	"github.com/wendylabsinc/wendy/go/world/internal/store"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

func publicationMetrics(f *fixture) *runtimeobs.Collector {
	m := runtimeobs.New()
	f.cfg.Metrics, f.n.cfg.Metrics = m, m
	f.s.SetMetrics(m)
	return m
}

func publicationPath(f *fixture) string {
	return filepath.Join(filepath.Dir(f.cfg.ProofPath), "store.json")
}

func publicationCheckpoint(t *testing.T, f *fixture) world.Checkpoint {
	t.Helper()
	b, err := os.ReadFile(publicationPath(f))
	if err != nil {
		t.Fatal(err)
	}
	var saved struct {
		State struct {
			World world.Checkpoint `json:"world"`
		} `json:"state"`
	}
	if err = json.Unmarshal(b, &saved); err != nil {
		t.Fatal(err)
	}
	if saved.State.World.Version != 1 {
		t.Fatal("missing durable world checkpoint")
	}
	return saved.State.World
}

func publicationProofs(t *testing.T, f *fixture) map[string]authority.SignedRecord {
	t.Helper()
	b, err := os.ReadFile(f.cfg.ProofPath)
	if err != nil {
		t.Fatal(err)
	}
	var saved struct {
		Version int                      `json:"version"`
		Records []authority.SignedRecord `json:"records"`
	}
	if err = json.Unmarshal(b, &saved); err != nil {
		t.Fatal(err)
	}
	if saved.Version != 2 {
		t.Fatal("missing durable proof state")
	}
	out := make(map[string]authority.SignedRecord)
	for _, record := range saved.Records {
		digest, err := recordDigest(record.Component)
		if err != nil {
			t.Fatal(err)
		}
		if _, exists := out[digest]; exists {
			t.Fatal("duplicate durable proof identity")
		}
		out[digest] = record
	}
	return out
}

func reopenPublication(t *testing.T, f *fixture) {
	t.Helper()
	f.server.Close()
	if err := f.s.Close(); err != nil {
		t.Fatal(err)
	}
	var err error
	f.s, err = store.Open(publicationPath(f), f.wc)
	if err != nil {
		t.Fatal(err)
	}
	f.s.SetMetrics(f.cfg.Metrics)
	f.n, err = New(f.cfg, f.s, f.v)
	if err != nil {
		t.Fatal(err)
	}
	f.start(t)
}

func requirePublication(t *testing.T, records []authority.SignedRecord, want world.Component) {
	t.Helper()
	if len(records) != 1 {
		t.Fatalf("retained records = %d, want one", len(records))
	}
	gotDigest, err := recordDigest(records[0].Component)
	if err != nil {
		t.Fatal(err)
	}
	wantDigest, err := recordDigest(want)
	if err != nil || gotDigest != wantDigest {
		t.Fatal("exported record differs from the expected source version")
	}
}

func TestPublicationTransactionSingleCommitLocalAndRemote(t *testing.T) {
	for _, remote := range []bool{false, true} {
		name := "local"
		if remote {
			name = "remote"
		}
		t.Run(name, func(t *testing.T) {
			origin := newFixture(t, nil, "origin")
			c := component(origin, allocate(t, origin), 1)
			target := origin
			if remote {
				publish(t, origin, c)
				target = newFixture(t, origin.root, "replica")
			}
			m := publicationMetrics(target)
			if remote {
				// Measure single-record admission independently of the new
				// repair inventory's durable observation checkpoint.
				if _, err := target.n.apply(signed(t, origin, c), false); err != nil {
					t.Fatal(err)
				}
			} else {
				publish(t, origin, c)
			}
			// Inspect before an export/read adds its own observation checkpoint.
			if observedDurationCount(t, m, "world_store.commit") != 1 || observedDurationCount(t, m, "proof_store.commit") != 1 {
				t.Fatal("publication must perform exactly one world and one proof commit")
			}
			if observedCounter(t, m, "world_store.commit_failures") != 0 || observedCounter(t, m, "proof_store.commit_failures") != 0 {
				t.Fatal("successful publication recorded a failed commit")
			}
			worldBytes, err := os.ReadFile(publicationPath(target))
			if err != nil {
				t.Fatal(err)
			}
			proofBytes, err := os.ReadFile(target.cfg.ProofPath)
			if err != nil {
				t.Fatal(err)
			}
			if observedCounter(t, m, "world_store.serialized_bytes") != uint64(len(worldBytes)) || observedCounter(t, m, "proof_store.serialized_bytes") != uint64(len(proofBytes)) {
				t.Fatal("commit byte totals do not match durable snapshots")
			}
			reopenPublication(t, target)
			records, err := target.n.export()
			if err != nil {
				t.Fatal(err)
			}
			requirePublication(t, records, c)
		})
	}
}

func TestPublicationDuplicateRefreshesProofBeforeOldGrantAndKeyRevocation(t *testing.T) {
	f := newFixture(t, nil, "origin")
	c := component(f, allocate(t, f), 1)
	old := signed(t, f, c)
	if result, err := f.n.apply(old, true); err != nil || result != world.Applied {
		t.Fatal(result, err)
	}
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	g := f.credentials.Grant.Grant
	g.ID += "-renewed"
	g.PublicKey = pub
	grant, err := authority.SignGrant(g, f.root)
	if err != nil {
		t.Fatal(err)
	}
	refreshed, err := authority.SignRecord(grant, key, c, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(old.Signature, refreshed.Signature) {
		t.Fatal("test did not rotate origin proof")
	}
	m := publicationMetrics(f)
	if result, err := f.n.apply(refreshed, true); err != nil || result != world.Duplicate {
		t.Fatal(result, err)
	}
	if observedDurationCount(t, m, "world_store.commit") != 1 || observedDurationCount(t, m, "proof_store.commit") != 1 {
		t.Fatal("duplicate skipped proof refresh or repeated world persistence")
	}
	now := time.Now()
	rev, err := authority.SignRevocations(authority.Revocations{Version: 1, Tenant: "tenant", Site: "office", Domain: "simulation", Revision: 2, IssuedAtUnixMS: now.Add(-time.Second).UnixMilli(), ExpiresAtUnixMS: now.Add(30 * time.Minute).UnixMilli(), GrantIDs: []string{old.Grant.Grant.ID}, KeyIDs: []string{authority.KeyID(old.Grant.Grant.PublicKey)}}, f.root)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(rev)
	if status, response := call(t, f, f.credentials, http.MethodPost, "/v1/revocations", "replicate", body); status != http.StatusOK {
		t.Fatalf("revocation: %d %s", status, response)
	}
	if _, err = f.v.VerifyRecord(old); err == nil {
		t.Fatal("old proof survived revocation")
	}
	for _, reopen := range []bool{false, true} {
		if reopen {
			reopenPublication(t, f)
		}
		records, err := f.n.export()
		if err != nil {
			t.Fatal(err)
		}
		requirePublication(t, records, c)
		if records[0].Grant.Grant.ID != grant.Grant.ID || !bytes.Equal(records[0].Signature, refreshed.Signature) {
			t.Fatal("export did not retain the refreshed grant and signature")
		}
		if _, err = f.v.VerifyRecord(records[0]); err != nil {
			t.Fatal("refreshed exported origin proof is invalid", err)
		}
	}
}

func TestPublicationDuplicateAgeHintShortensExpiryAcrossReopen(t *testing.T) {
	f := newFixture(t, nil, "origin")
	now := time.Now().Add(time.Second)
	f.wc.Now = func() time.Time { return now }
	reopenPublication(t, f)
	c := component(f, allocate(t, f), 1)
	c.Timing.SampleTime = now
	record := signed(t, f, c)
	if result, err := f.n.apply(record, true); err != nil || result != world.Applied {
		t.Fatal(result, err)
	}
	before := publicationCheckpoint(t, f).Entries[0].ExpiresAt
	record.Component.Timing.AgeHint = 4 * time.Minute // Not signed; it may only shorten freshness.
	m := publicationMetrics(f)
	if result, err := f.n.apply(record, true); err != nil || result != world.Duplicate {
		t.Fatal(result, err)
	}
	if observedDurationCount(t, m, "world_store.commit") != 1 || observedDurationCount(t, m, "proof_store.commit") != 1 {
		t.Fatal("duplicate age update bypassed the publication transaction")
	}
	shortened := publicationCheckpoint(t, f).Entries[0].ExpiresAt
	if !shortened.Equal(now.Add(time.Minute)) || !shortened.Before(before) {
		t.Fatal("higher relay age did not shorten the absolute deadline")
	}
	reopenPublication(t, f)
	if got := publicationCheckpoint(t, f).Entries[0].ExpiresAt; !got.Equal(shortened) {
		t.Fatal("reopen changed shortened expiry")
	}
	record.Component.Timing.AgeHint = 0
	if result, err := f.n.apply(record, true); err != nil || result != world.Duplicate {
		t.Fatal(result, err)
	}
	if got := publicationCheckpoint(t, f).Entries[0].ExpiresAt; !got.Equal(shortened) {
		t.Fatal("younger duplicate renewed the deadline")
	}
	now = now.Add(2 * time.Minute)
	visible, err := f.n.query()
	if err != nil || len(visible.Components) != 0 {
		t.Fatal("expired duplicate became visible", err)
	}
	reopenPublication(t, f)
	visible, err = f.n.query()
	if err != nil || len(visible.Components) != 0 {
		t.Fatal("restart revived expired duplicate", err)
	}
	records, err := f.n.export()
	if err != nil {
		t.Fatal(err)
	}
	requirePublication(t, records, c)
	if records[0].Component.Timing.AgeHint < c.TTL {
		t.Fatal("expired retained proof lost its relay age floor")
	}
}

func TestPublicationProofFailureLeavesOldWorldRetryable(t *testing.T) {
	f := newFixture(t, nil, "origin")
	c := component(f, allocate(t, f), 1)
	if _, err := f.n.apply(signed(t, f, c), true); err != nil {
		t.Fatal(err)
	}
	before := publicationCheckpoint(t, f)
	newer := c
	newer.Version.Sequence++
	candidate := signed(t, f, newer)
	backup := f.cfg.ProofPath + ".previous"
	if err := os.Rename(f.cfg.ProofPath, backup); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(f.cfg.ProofPath, 0700); err != nil {
		t.Fatal(err)
	}
	m := publicationMetrics(f)
	if _, err := f.n.apply(candidate, true); err == nil {
		t.Fatal("proof rename failure was accepted")
	}
	if observedDurationCount(t, m, "proof_store.commit") != 1 || observedCounter(t, m, "proof_store.commit_failures") != 1 || observedDurationCount(t, m, "world_store.commit") != 1 || observedCounter(t, m, "world_store.commit_failures") != 0 {
		t.Fatal("proof failure did not retain one durable observation floor")
	}
	after := publicationCheckpoint(t, f)
	if len(after.Entries) != 1 || after.Entries[0].Component.Version != c.Version || !after.Entries[0].ExpiresAt.Equal(before.Entries[0].ExpiresAt) {
		t.Fatal("failed proof publication changed retained world state")
	}
	if after.CapturedAt.Before(before.CapturedAt) {
		t.Fatal("proof failure regressed the observed clock")
	}
	if err := os.Remove(f.cfg.ProofPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(backup, f.cfg.ProofPath); err != nil {
		t.Fatal(err)
	}
	records, err := f.n.export()
	if err != nil {
		t.Fatal("proof failure poisoned usable old world", err)
	}
	requirePublication(t, records, c)
	for _, want := range []world.Result{world.Applied, world.Duplicate} {
		if got, err := f.n.apply(candidate, true); err != nil || got != want {
			t.Fatal("exact retry failed", got, err)
		}
	}
	reopenPublication(t, f)
	records, err = f.n.export()
	if err != nil {
		t.Fatal(err)
	}
	requirePublication(t, records, newer)
}

func TestPublicationWorldFailureRetainsProofSupersetAndRequiresReopen(t *testing.T) {
	f := newFixture(t, nil, "origin")
	c := component(f, allocate(t, f), 1)
	if _, err := f.n.apply(signed(t, f, c), true); err != nil {
		t.Fatal(err)
	}
	newer := c
	newer.Version.Sequence++
	candidate := signed(t, f, newer)
	path := publicationPath(f)
	backup := path + ".previous"
	if err := os.Rename(path, backup); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	m := publicationMetrics(f)
	if _, err := f.n.apply(candidate, true); err == nil {
		t.Fatal("world rename failure was accepted")
	}
	if observedDurationCount(t, m, "proof_store.commit") != 1 || observedCounter(t, m, "proof_store.commit_failures") != 0 || observedDurationCount(t, m, "world_store.commit") != 1 || observedCounter(t, m, "world_store.commit_failures") != 1 {
		t.Fatal("world failure changed proof-first write ordering or retried a writer")
	}
	proofs := publicationProofs(t, f)
	if len(proofs) != 2 {
		t.Fatal("failed world commit lost old-or-candidate proof coverage")
	}
	for _, expected := range []world.Component{c, newer} {
		digest, _ := recordDigest(expected)
		proof, ok := proofs[digest]
		if !ok {
			t.Fatal("proof superset missing retained or candidate source version")
		}
		if _, err := f.v.VerifyRecord(proof); err != nil {
			t.Fatal("durable proof superset contains invalid origin", err)
		}
	}
	if _, err := f.n.export(); !errors.Is(err, store.ErrFaulted) {
		t.Fatal("failed world write was still readable", err)
	}
	if _, err := f.n.apply(candidate, true); !errors.Is(err, store.ErrFaulted) {
		t.Fatal("poisoned world accepted a retry without reopen", err)
	}
	if observedDurationCount(t, m, "proof_store.commit") != 1 || observedDurationCount(t, m, "world_store.commit") != 1 {
		t.Fatal("poisoned-store retry performed another write")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(backup, path); err != nil {
		t.Fatal(err)
	}
	reopenPublication(t, f)
	records, err := f.n.export()
	if err != nil {
		t.Fatal(err)
	}
	requirePublication(t, records, c)
	if result, err := f.n.apply(candidate, true); err != nil || result != world.Applied {
		t.Fatal("reopened prior state could not retry exact candidate", result, err)
	}
	reopenPublication(t, f)
	records, err = f.n.export()
	if err != nil {
		t.Fatal(err)
	}
	requirePublication(t, records, newer)
}
