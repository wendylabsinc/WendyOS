package authority

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/annotations"
	"github.com/wendylabsinc/wendy/go/world/internal/runtimeobs"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

// Expose only the required ReplayStore interface. Batch verification must not
// depend on the concrete journal or the optional HTTP nonce-admission API.
type batchReplay struct {
	ReplayStore
	checkpoints, validations, consumes int
	checkpointError, validationError   error
	afterCheckpoint, afterValidation   func()
}

func (s *batchReplay) Checkpoint(revision uint64, digest string, now time.Time) error {
	s.checkpoints++
	if s.checkpointError != nil {
		return s.checkpointError
	}
	if err := s.ReplayStore.Checkpoint(revision, digest, now); err != nil {
		return err
	}
	if s.afterCheckpoint != nil {
		s.afterCheckpoint()
	}
	return nil
}

func (s *batchReplay) ValidateCheckpoint(revision uint64, digest string, now time.Time) error {
	s.validations++
	if s.validationError != nil {
		return s.validationError
	}
	if err := s.ReplayStore.ValidateCheckpoint(revision, digest, now); err != nil {
		return err
	}
	if s.afterValidation != nil {
		s.afterValidation()
	}
	return nil
}

func (s *batchReplay) Consume(key string, expires, now time.Time) error {
	s.consumes++
	return s.ReplayStore.Consume(key, expires, now)
}

func batchFixture(t *testing.T) (*fixture, *batchReplay, *ReplayCache, *runtimeobs.Collector) {
	t.Helper()
	cache, err := OpenReplay(filepath.Join(t.TempDir(), "ledger"), 100)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cache.Close() })
	replay := &batchReplay{ReplayStore: cache}
	f := newFixture(t, replay)
	f.now = f.now.Add(time.Second)
	replay.checkpoints, replay.validations = 0, 0
	metrics := runtimeobs.New()
	cache.SetMetrics(metrics)
	f.verifier.cfg.Metrics = metrics
	return f, replay, cache, metrics
}

func batchRecord(t *testing.T, f *fixture, index int) SignedRecord {
	t.Helper()
	signedAt := f.now.Add(-time.Millisecond)
	c := world.Component{Key: world.Key{Tenant: f.grant.Grant.Tenant, Site: f.grant.Grant.Site,
		Entity: fmt.Sprintf("observation-%d", index), Type: "pose", Source: f.grant.Grant.Source},
		Version: world.Version{Epoch: 1, Sequence: uint64(index + 1)}, TTL: time.Second,
		Timing: world.Timing{SampleTime: signedAt}, Payload: json.RawMessage(`{"x":1}`)}
	r, err := SignRecord(f.grant, f.key, c, signedAt)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// Re-sign deliberately invalid records to distinguish a trusted publisher's
// late binding denial from an unauthenticated tamper.
func batchResign(t *testing.T, key ed25519.PrivateKey, r SignedRecord) SignedRecord {
	t.Helper()
	var err error
	r.Signature, err = sign(key, "record", recordPayload(r))
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func batchGrant(t *testing.T, f *fixture, r SignedRecord, change func(*Grant)) SignedRecord {
	t.Helper()
	change(&r.Grant.Grant)
	var err error
	r.Grant, err = SignGrant(r.Grant.Grant, f.root)
	if err != nil {
		t.Fatal(err)
	}
	return batchResign(t, f.key, r)
}

func batchNoAuthority(t *testing.T, ids []Identity, err error) {
	t.Helper()
	if err == nil || ids != nil {
		t.Fatalf("batch returned partial or invalid authority: identities=%d error=%v", len(ids), err)
	}
}

func TestRecordBatchOneCheckpointOrdersOriginsAndDoesNotCacheAuthority(t *testing.T) {
	f, replay, cache, metrics := batchFixture(t)
	var records []SignedRecord
	for i := 0; i < MaxRecordBatchSize; i++ {
		record := batchRecord(t, f, i)
		public, private, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		g := record.Grant.Grant
		g.ID, g.Principal, g.Source.Stream = fmt.Sprintf("grant-%d", i), fmt.Sprintf("publisher-%d", i), fmt.Sprintf("stream-%d", i)
		g.PublicKey = public
		g.NotAfterUnixMS = f.now.UnixMilli() // valid at issuance, already expired now
		record.Grant, err = SignGrant(g, f.root)
		if err != nil {
			t.Fatal(err)
		}
		record.Component.Key.Source = g.Source
		record.Component.Timing.AgeHint = 2 * time.Second // unsigned, component already stale
		record = batchResign(t, private, record)
		records = append(records, record)
	}
	ids, err := f.verifier.VerifyRecords(records)
	if err != nil || len(ids) != len(records) {
		t.Fatalf("valid repaired group refused: %v", err)
	}
	for i, id := range ids {
		if id.Principal != records[i].Grant.Grant.Principal || id.Source != records[i].Component.Key.Source || id.SigningKeySHA256 != KeyID(records[i].Grant.Grant.PublicKey) || !id.NotAfter.Equal(time.UnixMilli(records[i].ExpiresAtUnixMS)) {
			t.Fatal("lost original identity, order or proof horizon")
		}
	}
	if replay.checkpoints != 1 || replay.validations != 1 || replay.consumes != 0 || len(cache.state.Nonces) != 0 || metricDuration(t, metrics, "replay.checkpoint_commit").Count != 1 || metricDuration(t, metrics, "authority.lock_wait").Count != 1 {
		t.Fatal("group did not use one lock/checkpoint without nonces")
	}
	if _, err = f.verifier.VerifyRecords(records); err != nil || replay.checkpoints != 2 || metricDuration(t, metrics, "replay.checkpoint_commit").Count != 1 {
		t.Fatal("immutable duplicates did not retain the same clock checkpoint", err)
	}
	rev := f.rev.Revocations
	rev.Revision++
	rev.Principals = []string{records[3].Grant.Grant.Principal}
	signed, err := SignRevocations(rev, f.root)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.verifier.UpdateRevocations(signed); err != nil {
		t.Fatal(err)
	}
	replay.checkpoints = 0
	ids, err = f.verifier.VerifyRecords(records)
	batchNoAuthority(t, ids, err)
	if !errors.Is(err, ErrDenied) || replay.checkpoints != 0 {
		t.Fatal("revoked member used cached authority or wrote a denied checkpoint", err)
	}
	if ids, err = f.verifier.VerifyRecords(records[:1]); err != nil || len(ids) != 1 {
		t.Fatal("independent valid singleton lost authority", err)
	}
}

func TestRecordBatchCountAndExactEncodedBounds(t *testing.T) {
	for _, kind := range []string{"empty", "nine", "invalid_json", "over_bytes"} {
		t.Run(kind, func(t *testing.T) {
			f, replay, _, _ := batchFixture(t)
			r := batchRecord(t, f, 0)
			records := []SignedRecord{r}
			switch kind {
			case "empty":
				records = nil
			case "nine":
				records = make([]SignedRecord, 9)
				for i := range records {
					records[i] = r
				}
			case "invalid_json":
				records[0].Component.Payload = json.RawMessage(`{`)
			case "over_bytes":
				records[0].Component.Payload = json.RawMessage(`"` + strings.Repeat("a", MaxRecordBatchBytes) + `"`)
				records[0] = batchResign(t, f.key, records[0])
			}
			ids, err := f.verifier.VerifyRecords(records)
			batchNoAuthority(t, ids, err)
			if replay.checkpoints != 0 || replay.validations != 0 || replay.consumes != 0 {
				t.Fatal("bounds failure touched replay state")
			}
		})
	}
	f, replay, _, _ := batchFixture(t)
	r := batchRecord(t, f, 0)
	r.Component.Payload = json.RawMessage(`""`)
	r = batchResign(t, f.key, r)
	encoded, _ := json.Marshal([]SignedRecord{r})
	r.Component.Payload = json.RawMessage(`"` + strings.Repeat("a", MaxRecordBatchBytes-len(encoded)) + `"`)
	r = batchResign(t, f.key, r)
	encoded, _ = json.Marshal([]SignedRecord{r})
	if len(encoded) != MaxRecordBatchBytes {
		t.Fatal("fixture does not meet exact encoded limit")
	}
	if ids, err := f.verifier.VerifyRecords([]SignedRecord{r}); err != nil || len(ids) != 1 || replay.checkpoints != 1 {
		t.Fatal("exact encoded limit rejected", err)
	}
	r.Component.Payload = append(r.Component.Payload[:len(r.Component.Payload)-1], 'a', '"')
	r = batchResign(t, f.key, r)
	ids, err := f.verifier.VerifyRecords([]SignedRecord{r})
	batchNoAuthority(t, ids, err)
	if replay.checkpoints != 1 {
		t.Fatal("over-limit record advanced the admitted clock")
	}
}

func TestRecordBatchInvalidMemberNeverAdmitsValidPrefix(t *testing.T) {
	cases := []string{"record_signature", "grant_signature", "wrong_root", "missing_scope", "grant_tenant", "grant_site", "grant_domain", "future_proof", "expired_proof", "before_grant", "after_grant", "revoked_principal", "revoked_key", "revoked_grant", "stale_revocations"}
	for _, kind := range cases {
		for _, badFirst := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/bad_first_%t", kind, badFirst), func(t *testing.T) {
				f, replay, cache, _ := batchFixture(t)
				good, bad := batchRecord(t, f, 0), batchRecord(t, f, 1)
				switch kind {
				case "record_signature":
					bad.Signature[0] ^= 1
				case "grant_signature":
					bad.Grant.Signature = bytes.Clone(bad.Grant.Signature)
					bad.Grant.Signature[0] ^= 1
					bad = batchResign(t, f.key, bad)
				case "wrong_root":
					_, root, _ := ed25519.GenerateKey(rand.Reader)
					bad.Grant, _ = SignGrant(bad.Grant.Grant, root)
					bad = batchResign(t, f.key, bad)
				case "missing_scope":
					bad = batchGrant(t, f, bad, func(g *Grant) { g.Scopes = []string{"read"} })
				case "grant_tenant":
					bad = batchGrant(t, f, bad, func(g *Grant) { g.Tenant = "other" })
				case "grant_site":
					bad = batchGrant(t, f, bad, func(g *Grant) { g.Site = "other" })
				case "grant_domain":
					bad = batchGrant(t, f, bad, func(g *Grant) { g.Domain = "physical" })
				case "future_proof":
					bad.SignedAtUnixMS = f.now.Add(time.Second).UnixMilli()
					bad.ExpiresAtUnixMS = f.now.Add(time.Hour).UnixMilli()
					bad = batchResign(t, f.key, bad)
				case "expired_proof":
					bad.ExpiresAtUnixMS = f.now.UnixMilli()
					bad = batchResign(t, f.key, bad)
				case "before_grant":
					bad = batchGrant(t, f, bad, func(g *Grant) { g.NotBeforeUnixMS = f.now.UnixMilli() })
				case "after_grant":
					bad = batchGrant(t, f, bad, func(g *Grant) { g.NotAfterUnixMS = bad.SignedAtUnixMS })
				case "revoked_principal", "revoked_key", "revoked_grant", "stale_revocations":
					r := f.rev.Revocations
					r.Revision++
					switch kind {
					case "revoked_principal":
						r.Principals = []string{bad.Grant.Grant.Principal}
					case "revoked_key":
						r.KeyIDs = []string{KeyID(bad.Grant.Grant.PublicKey)}
					case "revoked_grant":
						r.GrantIDs = []string{bad.Grant.Grant.ID}
					case "stale_revocations":
						r.ExpiresAtUnixMS = f.now.Add(time.Millisecond).UnixMilli()
					}
					signed, _ := SignRevocations(r, f.root)
					if err := f.verifier.UpdateRevocations(signed); err != nil {
						t.Fatal(err)
					}
					f.now = f.now.Add(2 * time.Millisecond)
				}
				replay.checkpoints, replay.validations = 0, 0
				floor := cache.state.LastUnixMS
				records := []SignedRecord{good, bad}
				if badFirst {
					records[0], records[1] = records[1], records[0]
				}
				ids, err := f.verifier.VerifyRecords(records)
				batchNoAuthority(t, ids, err)
				if replay.checkpoints != 0 || replay.validations != 0 || cache.state.LastUnixMS != floor {
					t.Fatal("invalid group partially advanced replay state")
				}
			})
		}
	}
}

func TestRecordBatchLateDenialKeepsObservedFloorOnReopen(t *testing.T) {
	for _, kind := range []string{"source", "tenant", "site", "raw_payload_limit", "source_utf8_alias"} {
		t.Run(kind, func(t *testing.T) {
			f, replay, cache, metrics := batchFixture(t)
			good, bad := batchRecord(t, f, 0), batchRecord(t, f, 1)
			switch kind {
			case "source":
				bad.Component.Key.Source.Service = "other"
			case "tenant":
				bad.Component.Key.Tenant = "other"
			case "site":
				bad.Component.Key.Site = "other"
			case "raw_payload_limit":
				f.verifier.cfg.MaxBodyBytes = 16
				bad.Component.Payload = json.RawMessage("                 {\"x\":1}")
			case "source_utf8_alias":
				bad = batchGrant(t, f, bad, func(g *Grant) { g.Source.Service = "camera\xff" })
				bad.Component.Key.Source.Service = "camera\ufffd"
			}
			bad = batchResign(t, f.key, bad)
			ids, err := f.verifier.VerifyRecords([]SignedRecord{good, bad})
			batchNoAuthority(t, ids, err)
			if !errors.Is(err, ErrDenied) || replay.checkpoints != 1 || replay.validations != 0 || metricDuration(t, metrics, "replay.checkpoint_commit").Count != 1 {
				t.Fatal("authenticated late denial skipped or repeated its durable floor", err)
			}
			path, deniedAt := cache.path, f.now
			if err = cache.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := OpenReplay(path, 100)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			if reopened.state.LastUnixMS != deniedAt.UnixMilli() || len(reopened.state.Nonces) != 0 {
				t.Fatal("late denial floor was not durable")
			}
			if err = reopened.ValidateCheckpoint(reopened.state.Revision, reopened.state.Digest, deniedAt.Add(-time.Millisecond)); err == nil {
				t.Fatal("denial floor permitted clock rollback after reopen")
			}
		})
	}
}

func TestRecordBatchLateDenialDoesNotAuthenticateAnotherMember(t *testing.T) {
	f, replay, _, _ := batchFixture(t)
	late, forged := batchRecord(t, f, 0), batchRecord(t, f, 1)
	late.Component.Key.Source.Service = "wrong-source"
	late = batchResign(t, f.key, late)
	forged.Signature[0] ^= 1
	ids, err := f.verifier.VerifyRecords([]SignedRecord{late, forged})
	batchNoAuthority(t, ids, err)
	if replay.checkpoints != 0 {
		t.Fatal("late denial wrote before authenticating the whole group")
	}
}

func TestRecordBatchCompletionAfterBlockingReplay(t *testing.T) {
	for _, boundary := range []string{"proof_expiry", "revocation_expiry", "wall_rollback", "submillisecond_rollback", "current_grant_expiry_allowed"} {
		for _, stage := range []string{"checkpoint", "validation"} {
			t.Run(boundary+"/"+stage, func(t *testing.T) {
				f, replay, cache, _ := batchFixture(t)
				record := batchRecord(t, f, 0)
				advance := 100 * time.Millisecond
				switch boundary {
				case "proof_expiry":
					record.ExpiresAtUnixMS = f.now.Add(advance).UnixMilli()
					record = batchResign(t, f.key, record)
				case "revocation_expiry":
					r := f.rev.Revocations
					r.Revision++
					r.ExpiresAtUnixMS = f.now.Add(advance).UnixMilli()
					signed, _ := SignRevocations(r, f.root)
					if err := f.verifier.UpdateRevocations(signed); err != nil {
						t.Fatal(err)
					}
					f.now = f.now.Add(time.Millisecond)
				case "wall_rollback":
					advance = -time.Millisecond
				case "submillisecond_rollback":
					f.now = f.now.Add(500 * time.Microsecond)
					advance = -time.Nanosecond
				case "current_grant_expiry_allowed":
					record = batchGrant(t, f, record, func(g *Grant) { g.NotAfterUnixMS = f.now.Add(time.Millisecond).UnixMilli() })
				}
				admittedAt := f.now
				replay.checkpoints, replay.validations = 0, 0
				shift := func() { f.now = f.now.Add(advance) }
				if stage == "checkpoint" {
					replay.afterCheckpoint = shift
				} else {
					replay.afterValidation = shift
				}
				ids, err := f.verifier.VerifyRecords([]SignedRecord{record})
				if boundary == "current_grant_expiry_allowed" {
					if err != nil || len(ids) != 1 || !ids[0].NotAfter.Equal(time.UnixMilli(record.ExpiresAtUnixMS)) {
						t.Fatal("valid issuance repair lost authority at current grant expiry", err)
					}
				} else {
					batchNoAuthority(t, ids, err)
					if boundary == "revocation_expiry" && !errors.Is(err, ErrStaleRevocations) {
						t.Fatal("revocation expiry returned wrong classification", err)
					}
					if (boundary == "wall_rollback" || boundary == "submillisecond_rollback") &&
						(!errors.Is(err, ErrRecordClockRollback) || errors.Is(err, ErrDenied)) {
						t.Fatal("clock rollback must stop repair, not classify an origin for omission", err)
					}
				}
				if replay.checkpoints != 1 || replay.validations != 1 || cache.state.LastUnixMS != admittedAt.UnixMilli() {
					t.Fatal("completion performed another write or changed durable floor")
				}
			})
		}
	}
}

func TestRecordBatchCompletionRejectsAnotherLedgerWriterAdvance(t *testing.T) {
	f, replay, cache, _ := batchFixture(t)
	record := batchRecord(t, f, 0)
	replay.afterCheckpoint = func() {
		f.now = f.now.Add(time.Millisecond)
		if err := cache.Checkpoint(cache.state.Revision, cache.state.Digest, f.now); err != nil {
			t.Fatal(err)
		}
	}
	ids, err := f.verifier.VerifyRecords([]SignedRecord{record})
	batchNoAuthority(t, ids, err)
	if replay.checkpoints != 1 || replay.validations != 1 || cache.state.LastUnixMS != f.now.UnixMilli() {
		t.Fatal("batch replaced or retried the newer durable floor")
	}
}

func TestRecordBatchPhysicalVerifierRejectsSyntheticAncestry(t *testing.T) {
	for _, origin := range []string{"measured", "simulated", "replayed", "authored", "inferred_measured", "inferred_simulated"} {
		t.Run(origin, func(t *testing.T) {
			f, replay, cache, _ := batchFixture(t)
			// These are generated unit-test keys in an explicitly configured
			// physical namespace, not enrolled production credentials.
			g := f.grant.Grant
			g.Domain = "physical"
			var err error
			f.grant, err = SignGrant(g, f.root)
			if err != nil {
				t.Fatal(err)
			}
			config := f.verifier.cfg
			config.Domain = "physical"
			f.verifier, err = NewVerifier(config)
			if err != nil {
				t.Fatal(err)
			}
			r := f.rev.Revocations
			r.Domain, r.Revision = "physical", r.Revision+1
			f.rev, err = SignRevocations(r, f.root)
			if err != nil {
				t.Fatal(err)
			}
			if err = f.verifier.UpdateRevocations(f.rev); err != nil {
				t.Fatal(err)
			}
			f.now = f.now.Add(time.Millisecond)
			replay.checkpoints, replay.validations = 0, 0
			record := batchRecord(t, f, 0)
			record.Component.Provenance.Origin = world.Origin(origin)
			if strings.HasPrefix(origin, "inferred_") {
				record.Component.Provenance.Origin = world.Inferred
				record.Component.Provenance.Inputs = []world.Input{{ID: "input", Origin: world.Origin(strings.TrimPrefix(origin, "inferred_"))}}
			}
			record = batchResign(t, f.key, record)
			ids, err := f.verifier.VerifyRecords([]SignedRecord{record})
			allow := origin == "measured" || origin == "inferred_measured"
			if allow {
				if err != nil || len(ids) != 1 {
					t.Fatal("non-synthetic origin lost existing record authority", err)
				}
			} else {
				batchNoAuthority(t, ids, err)
			}
			if replay.checkpoints != 1 || cache.state.LastUnixMS != f.now.UnixMilli() {
				t.Fatal("physical provenance check changed the authenticated observation floor")
			}
		})
	}
}

func TestRecordBatchReplayErrorsAndPoisonedJournal(t *testing.T) {
	for _, stage := range []string{"custom_checkpoint", "custom_validation", "late_denial_checkpoint", "journal_partial", "journal_sync"} {
		t.Run(stage, func(t *testing.T) {
			f, replay, cache, metrics := batchFixture(t)
			record := batchRecord(t, f, 0)
			injected := errors.New("test replay failure")
			oldFloor, admittedAt := cache.state.LastUnixMS, f.now
			switch stage {
			case "custom_checkpoint":
				replay.checkpointError = injected
			case "custom_validation":
				replay.validationError = injected
			case "late_denial_checkpoint":
				record.Component.Key.Site = "other"
				record = batchResign(t, f.key, record)
				replay.checkpointError = injected
			case "journal_partial":
				cache.appendWrite = func(data []byte) (int, error) {
					n, err := cache.file.WriteAt(data[:journalHeaderBytes+1], cache.offset)
					if err != nil {
						return n, err
					}
					return n, io.ErrUnexpectedEOF
				}
			case "journal_sync":
				cache.syncFile = func(*os.File) error { return injected }
			}
			ids, err := f.verifier.VerifyRecords([]SignedRecord{record})
			batchNoAuthority(t, ids, err)
			if stage != "journal_partial" && !errors.Is(err, injected) {
				t.Fatal("storage error was replaced by a policy error", err)
			}
			if replay.checkpoints != 1 || replay.consumes != 0 {
				t.Fatal("storage failure was retried or consumed a nonce")
			}
			if !strings.HasPrefix(stage, "journal_") {
				return
			}
			if !cache.poisoned || cache.state.LastUnixMS != oldFloor {
				t.Fatal("uncertain journal write became in-memory authority")
			}
			ids, err = f.verifier.VerifyRecords([]SignedRecord{record})
			batchNoAuthority(t, ids, err)
			if metricDuration(t, metrics, "replay.checkpoint_commit").Count != 1 {
				t.Fatal("poisoned replay store wrote again")
			}
			path := cache.path
			if err = cache.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := OpenReplay(path, 100)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			wantFloor := oldFloor
			if stage == "journal_sync" {
				// Complete bytes survived the injected sync error. This does
				// not emulate power loss or promise those bytes survive it.
				wantFloor = admittedAt.UnixMilli()
			}
			if reopened.state.LastUnixMS != wantFloor {
				t.Fatal("reopen lost the valid journal prefix")
			}
			f.verifier.cfg.Replay = reopened
			if ids, err = f.verifier.VerifyRecords([]SignedRecord{record}); err != nil || len(ids) != 1 {
				t.Fatal("reopened journal could not verify a valid record", err)
			}
		})
	}
}

func TestRecordBatchOwnsValuesAcrossReplayCallbacks(t *testing.T) {
	f, replay, _, _ := batchFixture(t)
	records := []SignedRecord{batchRecord(t, f, 0)}
	originalSource := records[0].Grant.Grant.Source
	replay.afterCheckpoint = func() {
		records[0].Grant.Grant.Scopes[0] = "stop"
		records[0].Grant.Grant.PublicKey[0] ^= 1
		records[0].Grant.Signature[0] ^= 1
		records[0].Signature[0] ^= 1
		records[0].Component.Payload[0] = '!'
	}
	ids, err := f.verifier.VerifyRecords(records)
	if err != nil || len(ids) != 1 || ids[0].Source != originalSource || !ids[0].Allows("publish") {
		t.Fatal("caller mutation changed the already-authenticated batch snapshot", err)
	}
}

func TestRecordBatchAnnotationsKeepNarrowRules(t *testing.T) {
	for _, kind := range []string{"valid_mixed", "wrong_author", "renewed_issuance", "wrong_proof_expiry", "missing_annotation_scope", "reserved_type"} {
		t.Run(kind, func(t *testing.T) {
			f, replay, _, _ := batchFixture(t)
			ordinary := batchRecord(t, f, 0)
			g := f.grant.Grant
			g.Kind, g.ID, g.Principal = "operator", "note-grant", "human"
			g.Scopes = []string{"annotation-write", "read"}
			grant, err := SignGrant(g, f.root)
			if err != nil {
				t.Fatal(err)
			}
			note := annotations.Note{ID: "12345678-1234-4123-8123-123456789abc", AuthorPrincipal: g.Principal, Text: "Inspect the doorway", AuthoredAtUnixMS: f.now.UnixMilli(), ExpiresAtUnixMS: f.now.Add(time.Hour).UnixMilli()}
			component, err := annotations.Build(note, annotations.Config{Tenant: g.Tenant, Site: g.Site, RunID: "run", Source: g.Source, Frame: world.FrameRef{ID: "floor", Revision: "1"}, Epoch: 1, Sequence: 1})
			if err != nil {
				t.Fatal(err)
			}
			record, err := SignAnnotation(grant, f.key, component)
			if err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "wrong_author":
				note.AuthorPrincipal = "other-human"
				record.Component.Payload, _ = json.Marshal(note)
			case "renewed_issuance":
				record.SignedAtUnixMS++
				record.ExpiresAtUnixMS++
				f.now = f.now.Add(time.Millisecond)
			case "wrong_proof_expiry":
				record.ExpiresAtUnixMS--
			case "missing_annotation_scope":
				record = batchGrant(t, f, record, func(g *Grant) { g.Scopes = []string{"publish", "read"} })
			case "reserved_type":
				record.Component.Key.Type = "wendy.annotation.other.v1"
			}
			record = batchResign(t, f.key, record)
			ids, err := f.verifier.VerifyRecords([]SignedRecord{ordinary, record})
			if kind == "valid_mixed" {
				if err != nil || len(ids) != 2 || ids[1].Principal != g.Principal || !ids[1].Allows("annotation-write") || ids[1].Allows("publish") || replay.checkpoints != 1 {
					t.Fatal("narrow annotation identity lost", err)
				}
				if !reflect.DeepEqual(record.Component, component) {
					t.Fatal("batch verification changed the original note")
				}
			} else {
				batchNoAuthority(t, ids, err)
				if replay.checkpoints != 0 {
					t.Fatal("malformed annotation advanced the group's floor")
				}
			}
		})
	}
}
