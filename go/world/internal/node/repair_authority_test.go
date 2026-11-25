package node

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/authority"
)

// Only the origin verifier's clock moves in these tests. The node's durable
// world clock continues running normally, including when it retains old state.
func repairAuthorityClock(t *testing.T, f *fixture, offset *atomic.Int64, replay authority.ReplayStore) {
	t.Helper()
	v, err := authority.NewVerifier(authority.Config{
		RootPublicKey: f.root.Public().(ed25519.PublicKey), Tenant: f.cfg.Tenant,
		Site: f.cfg.Site, Domain: "simulation", Audience: f.audience,
		Now: func() time.Time { return time.Now().Add(time.Duration(offset.Load())) }, Replay: replay,
	})
	if err != nil {
		t.Fatal(err)
	}
	f.v, f.n.verifier = v, v
}

func repairAuthorityRevocations(t *testing.T, f *fixture, now time.Time, revision uint64, principals ...string) {
	t.Helper()
	rev, err := authority.SignRevocations(authority.Revocations{
		Version: authority.Version, Tenant: f.cfg.Tenant, Site: f.cfg.Site, Domain: "simulation",
		Revision: revision, IssuedAtUnixMS: now.Add(-time.Second).UnixMilli(),
		ExpiresAtUnixMS: now.Add(30 * time.Minute).UnixMilli(), Principals: principals,
	}, f.root)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.v.UpdateRevocations(rev); err != nil {
		t.Fatal(err)
	}
}

func repairAuthorityRecord(t *testing.T, f *fixture, credentials Credentials, entity string, stamp time.Time) authority.SignedRecord {
	t.Helper()
	c := component(f, 1, 1)
	c.Key.Entity, c.Key.Source, c.Timing.SampleTime = entity, credentials.Grant.Grant.Source, stamp
	record, err := authority.SignRecord(credentials.Grant, credentials.PrivateKey, c, stamp)
	if err != nil {
		t.Fatal(err)
	}
	return record
}

func repairAuthorityRetain(t *testing.T, f *fixture, record authority.SignedRecord) {
	t.Helper()
	if _, err := f.n.applyRepairPage([]RepairRecord{reviewRepairRecord(t, record, record.Component.Timing.SampleTime.Add(record.Component.TTL))}); err != nil {
		t.Fatal(err)
	}
}

func TestRepairAuthorityHTTPFiltersExpiredProofAndRevokedOrigin(t *testing.T) {
	f := newFixture(t, nil, "repair-gateway")
	var offset atomic.Int64
	offset.Store(int64(-25 * time.Hour))
	repairAuthorityClock(t, f, &offset, authority.NewMemoryReplay(1000))
	oldNow := time.Now().Add(-25 * time.Hour)
	repairAuthorityRevocations(t, f, oldNow, 1)
	oldPublisher := issueCredentials(t, f.root, "expired-proof-origin")
	g := oldPublisher.Grant.Grant
	g.NotBeforeUnixMS, g.NotAfterUnixMS = oldNow.Add(-time.Minute).UnixMilli(), oldNow.Add(time.Hour).UnixMilli()
	var err error
	oldPublisher.Grant, err = authority.SignGrant(g, f.root)
	if err != nil {
		t.Fatal(err)
	}
	expiredProof := repairAuthorityRecord(t, f, oldPublisher, "expired-proof", oldNow)
	repairAuthorityRetain(t, f, expiredProof)

	// A retained high-water entry survives its 24-hour proof. It is an
	// inventory hint, not permission to forward that expired origin proof.
	offset.Store(0)
	now := time.Now()
	repairAuthorityRevocations(t, f, now, 2)
	if _, err := f.v.VerifyRecords([]authority.SignedRecord{expiredProof}); !errors.Is(err, authority.ErrDenied) {
		t.Fatalf("expired proof admitted: %v", err)
	}
	issuedPublisher := issueCredentials(t, f.root, "expired-grant-origin")
	g = issuedPublisher.Grant.Grant
	g.NotAfterUnixMS = now.Add(-time.Second).UnixMilli()
	issuedPublisher.Grant, err = authority.SignGrant(g, f.root)
	if err != nil {
		t.Fatal(err)
	}
	issued := repairAuthorityRecord(t, f, issuedPublisher, "valid-issuance", now.Add(-2*time.Second))
	if _, err := f.v.VerifyGrant(issuedPublisher.Grant, "publish"); !errors.Is(err, authority.ErrDenied) {
		t.Fatalf("current expired grant admitted: %v", err)
	}
	// Record repair checks grant validity at issuance, current revocation and
	// the absolute proof deadline. Current role expiry alone is not revocation.
	repairAuthorityRetain(t, f, issued)
	fresh := repairAuthorityRecord(t, f, f.credentials, "fresh-origin", now.Add(-time.Second))
	repairAuthorityRetain(t, f, fresh)
	revokedPublisher := issueCredentials(t, f.root, "revoked-origin")
	revoked := repairAuthorityRecord(t, f, revokedPublisher, "revoked-origin", now)
	repairAuthorityRetain(t, f, revoked)
	repairAuthorityRevocations(t, f, time.Now(), 3, revokedPublisher.Grant.Grant.Principal)

	status, inventory := reviewInventory(t, f, f.credentials, "")
	if status != http.StatusOK || len(inventory.Entries) != 4 || inventory.Cursor != "" {
		t.Fatalf("retained inventory: status=%d entries=%d", status, len(inventory.Entries))
	}
	wanted := RepairFetchRequest{Version: RepairVersion, Entries: inventory.Entries}
	status, body := call(t, f, f.credentials, http.MethodPost, "/v1/repair/fetch", "replicate", jsonBody(t, wanted))
	if status != http.StatusOK {
		t.Fatalf("mixed-origin fetch status=%d", status)
	}
	var response RepairFetchResponse
	if err := authority.DecodeStrictLimit(body, &response, MaxRepairPageBytes); err != nil {
		t.Fatal(err)
	}
	if err := validateRepairFetch(wanted, response); err != nil {
		t.Fatal(err)
	}
	if len(response.Records) != 2 || len(response.Unavailable) != 2 {
		t.Fatalf("filter result: records=%d unavailable=%d", len(response.Records), len(response.Unavailable))
	}
	want := map[string]authority.SignedRecord{issued.Component.Key.Entity: issued, fresh.Component.Key.Entity: fresh}
	assertOriginals := func(records []authority.SignedRecord, legacy bool) {
		t.Helper()
		seen := map[string]bool{}
		for _, record := range records {
			original, ok := want[record.Component.Key.Entity]
			if !ok || seen[record.Component.Key.Entity] {
				t.Fatal("returned an unauthorized or repeated origin")
			}
			seen[record.Component.Key.Entity] = true
			actualDigest, err := repairProofDigest(record)
			if err != nil {
				t.Fatal(err)
			}
			originalDigest, err := repairProofDigest(original)
			if err != nil || actualDigest != originalDigest || !bytes.Equal(record.Signature, original.Signature) {
				t.Fatal("export changed an original signed field")
			}
			if _, err := f.v.VerifyRecords([]authority.SignedRecord{record}); err != nil {
				t.Fatalf("exported origin signature does not verify: %v", err)
			}
			if legacy && record.Component.Timing.AgeHint <= original.Component.Timing.AgeHint {
				t.Fatal("legacy export did not carry increased unsigned age")
			}
		}
		if len(seen) != len(want) {
			t.Fatal("export omitted a currently authorized proof")
		}
	}
	returned := make([]authority.SignedRecord, len(response.Records))
	for i := range response.Records {
		returned[i] = response.Records[i].Record
	}
	assertOriginals(returned, false)

	status, body = call(t, f, f.credentials, http.MethodGet, "/v1/replica", "replicate", nil)
	if status != http.StatusOK {
		t.Fatalf("legacy export status=%d", status)
	}
	var legacy ReplicationResponse
	if err := authority.DecodeStrictLimit(body, &legacy, MaxResponseBytes); err != nil {
		t.Fatal(err)
	}
	assertOriginals(legacy.Records, true)
}

func TestRepairAuthorityHTTPPageIncludesEnvelopeAndFramingBudget(t *testing.T) {
	f := newFixture(t, nil, "large-origin")
	for i := 0; i < authority.MaxRecordBatchSize; i++ {
		c := component(f, 1, 1)
		c.Key.Entity = fmt.Sprintf("large-%d", i)
		c.Payload = json.RawMessage(`""`)
		stamp := time.Now()
		record, err := authority.SignRecord(f.credentials.Grant, f.credentials.PrivateKey, c, stamp)
		if err != nil {
			t.Fatal(err)
		}
		base := jsonBody(t, record)
		c.Payload = json.RawMessage(`"` + strings.Repeat("x", authority.MaxEnvelopeBytes-len(base)) + `"`)
		record, err = authority.SignRecord(f.credentials.Grant, f.credentials.PrivateKey, c, stamp)
		if err != nil {
			t.Fatal(err)
		}
		if size := len(jsonBody(t, record)); size != authority.MaxEnvelopeBytes {
			t.Fatalf("envelope fixture size=%d", size)
		}
		repairAuthorityRetain(t, f, record)
	}
	status, inventory := reviewInventory(t, f, f.credentials, "")
	if status != http.StatusOK || len(inventory.Entries) != authority.MaxRecordBatchSize {
		t.Fatalf("inventory status=%d entries=%d", status, len(inventory.Entries))
	}
	wanted := RepairFetchRequest{Version: RepairVersion, Entries: inventory.Entries}
	status, body := call(t, f, f.credentials, http.MethodPost, "/v1/repair/fetch", "replicate", jsonBody(t, wanted))
	if status != http.StatusOK || len(body) > MaxRepairPageBytes || !bytes.HasSuffix(body, []byte("\n")) {
		t.Fatalf("framed fetch: status=%d bytes=%d", status, len(body))
	}
	var response RepairFetchResponse
	if err := authority.DecodeStrictLimit(body, &response, MaxRepairPageBytes); err != nil {
		t.Fatal(err)
	}
	if err := validateRepairFetch(wanted, response); err != nil {
		t.Fatal(err)
	}
	if len(response.Records) == 0 || len(response.Records) >= len(wanted.Entries) || len(response.Unavailable) == 0 {
		t.Fatalf("framing should defer at least one valid envelope: records=%d unavailable=%d", len(response.Records), len(response.Unavailable))
	}
	for _, record := range response.Records {
		if len(jsonBody(t, record.Record)) != authority.MaxEnvelopeBytes {
			t.Fatal("a returned envelope was truncated or rewritten")
		}
		if _, err := f.v.VerifyRecords([]authority.SignedRecord{record.Record}); err != nil {
			t.Fatal(err)
		}
	}
	// An unavailable entry is neither an acknowledgement nor permanent loss.
	// A later small request must retrieve the same complete signed proof.
	for _, key := range response.Unavailable {
		var entry RepairEntry
		for _, candidate := range wanted.Entries {
			if repairKeyDigest(candidate.Key) == key {
				entry = candidate
			}
		}
		single := RepairFetchRequest{Version: RepairVersion, Entries: []RepairEntry{entry}}
		status, body := call(t, f, f.credentials, http.MethodPost, "/v1/repair/fetch", "replicate", jsonBody(t, single))
		var retried RepairFetchResponse
		if status != http.StatusOK || authority.DecodeStrictLimit(body, &retried, MaxRepairPageBytes) != nil || len(retried.Records) != 1 || len(retried.Unavailable) != 0 {
			t.Fatalf("deferred exact proof unavailable: status=%d", status)
		}
		if err := validateRepairFetch(single, retried); err != nil {
			t.Fatal(err)
		}
	}
}

type repairAuthorityAdvanceAfterRequest struct {
	authority.ReplayStore
	offset *atomic.Int64
	armed  atomic.Bool
}

func (r *repairAuthorityAdvanceAfterRequest) Consume(key string, expires, now time.Time) error {
	if err := r.ReplayStore.Consume(key, expires, now); err != nil {
		return err
	}
	if r.armed.CompareAndSwap(true, false) {
		r.offset.Store(int64(31 * time.Minute))
	}
	return nil
}

func TestRepairAuthorityHTTPStalePolicyFailsWholeResponseAfterRequestAdmission(t *testing.T) {
	for _, path := range []string{"/v1/repair/fetch", "/v1/replica"} {
		t.Run(path, func(t *testing.T) {
			f := newFixture(t, nil, "policy-origin")
			record := signed(t, f, component(f, 1, 1))
			repairAuthorityRetain(t, f, record)
			status, inventory := reviewInventory(t, f, f.credentials, "")
			if status != http.StatusOK || len(inventory.Entries) != 1 {
				t.Fatal("missing initial inventory")
			}
			var offset atomic.Int64
			replay := &repairAuthorityAdvanceAfterRequest{ReplayStore: authority.NewMemoryReplay(1000), offset: &offset}
			repairAuthorityClock(t, f, &offset, replay)
			repairAuthorityRevocations(t, f, time.Now(), 1)
			replay.armed.Store(true)
			method, body := http.MethodGet, []byte(nil)
			if path == "/v1/repair/fetch" {
				method, body = http.MethodPost, jsonBody(t, RepairFetchRequest{Version: RepairVersion, Entries: inventory.Entries})
			}
			status, body = call(t, f, f.credentials, method, path, "replicate", body)
			if replay.armed.Load() || offset.Load() == 0 {
				t.Fatal("request never reached authenticated nonce admission")
			}
			if status != http.StatusServiceUnavailable || bytes.Contains(body, record.Signature) || bytes.Contains(body, []byte(`"records"`)) {
				t.Fatalf("stale policy was treated as source omission: status=%d", status)
			}
			if _, err := f.v.VerifyRecords([]authority.SignedRecord{record}); !errors.Is(err, authority.ErrStaleRevocations) {
				t.Fatalf("fixture did not reach stale current policy: %v", err)
			}
		})
	}
}
