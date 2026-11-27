package node

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/mission"
	"github.com/wendylabsinc/wendy/go/world/internal/store"
	"github.com/wendylabsinc/wendy/go/world/internal/tasking"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

func reviewRepairRecord(t *testing.T, record authority.SignedRecord, cap time.Time) RepairRecord {
	t.Helper()
	entry, err := repairEntry(world.CheckpointEntry{Component: record.Component, ExpiresAt: cap}, record)
	if err != nil {
		t.Fatal(err)
	}
	return RepairRecord{Entry: entry, Record: record}
}

func reviewInventory(t *testing.T, f *fixture, credentials Credentials, cursor string) (int, RepairInventoryResponse) {
	t.Helper()
	code, body := call(t, f, credentials, "POST", "/v1/repair/inventory", "replicate", jsonBody(t, RepairInventoryRequest{Version: RepairVersion, Cursor: cursor}))
	var reply RepairInventoryResponse
	if code == http.StatusOK {
		if err := json.Unmarshal(body, &reply); err != nil {
			t.Fatal(err)
		}
	}
	return code, reply
}

func TestRepairReviewCursorIdentityExpiryAndRestart(t *testing.T) {
	f := newFixture(t, nil, "origin")
	c := component(f, allocate(t, f), 1)
	publish(t, f, c)
	id, err := f.v.VerifyGrant(f.credentials.Grant, "replicate")
	if err != nil {
		t.Fatal(err)
	}
	cursor := repairCursor{Version: RepairVersion, Tenant: f.cfg.Tenant, Site: f.cfg.Site, Principal: id.Principal, KeyID: id.SigningKeySHA256, GrantID: id.GrantID, After: strings.Repeat("0", 64), Expires: time.Now().Add(time.Minute).UnixMilli()}
	token, err := f.n.encodeRepairCursor(cursor)
	if err != nil {
		t.Fatal(err)
	}
	if status, page := reviewInventory(t, f, f.credentials, token); status != 200 || len(page.Entries) != 1 {
		t.Fatalf("valid continuation: status=%d entries=%d", status, len(page.Entries))
	}
	for _, kind := range []string{"principal", "grant", "key"} {
		t.Run(kind, func(t *testing.T) {
			credentials := f.credentials
			grant := credentials.Grant.Grant
			switch kind {
			case "principal":
				grant.Principal += "-other"
			case "grant":
				grant.ID += "-renewed"
			case "key":
				public, private, err := ed25519.GenerateKey(rand.Reader)
				if err != nil {
					t.Fatal(err)
				}
				grant.PublicKey, credentials.PrivateKey = public, private
			}
			credentials.Grant, err = authority.SignGrant(grant, f.root)
			if err != nil {
				t.Fatal(err)
			}
			// The replacement credential itself works; only its borrowed cursor fails.
			if status, _ := reviewInventory(t, f, credentials, ""); status != 200 {
				t.Fatalf("replacement credential refused: %d", status)
			}
			if status, _ := reviewInventory(t, f, credentials, token); status != 400 {
				t.Fatalf("borrowed cursor accepted: %d", status)
			}
		})
	}
	for _, kind := range []string{"expired", "future", "site", "tampered", "padding"} {
		t.Run(kind, func(t *testing.T) {
			changed := cursor
			switch kind {
			case "expired":
				changed.Expires = time.Now().Add(-time.Second).UnixMilli()
			case "future":
				changed.Expires = time.Now().Add(3 * time.Minute).UnixMilli()
			case "site":
				changed.Site = "another-site"
			}
			bad, err := f.n.encodeRepairCursor(changed)
			if err != nil {
				t.Fatal(err)
			}
			if kind == "tampered" {
				if bad[0] == 'A' {
					bad = "B" + bad[1:]
				} else {
					bad = "A" + bad[1:]
				}
			} else if kind == "padding" {
				bad += "="
			}
			if status, _ := reviewInventory(t, f, f.credentials, bad); status != 400 {
				t.Fatalf("invalid cursor accepted: %d", status)
			}
		})
	}
	reopenPublication(t, f)
	if status, _ := reviewInventory(t, f, f.credentials, ""); status != 200 {
		t.Fatalf("reopened inventory unavailable: %d", status)
	}
	if status, _ := reviewInventory(t, f, f.credentials, token); status != 400 {
		t.Fatalf("cursor survived gateway process replacement: %d", status)
	}
}

func TestRepairReviewFetchExactCoverageAndDigests(t *testing.T) {
	f := newFixture(t, nil, "origin")
	c := component(f, allocate(t, f), 1)
	publish(t, f, c)
	status, inventory := reviewInventory(t, f, f.credentials, "")
	if status != 200 || len(inventory.Entries) != 1 {
		t.Fatal("missing inventory fixture")
	}
	entry := inventory.Entries[0]
	request := RepairFetchRequest{Version: RepairVersion, Entries: []RepairEntry{entry}}
	status, body := call(t, f, f.credentials, "POST", "/v1/repair/fetch", "replicate", jsonBody(t, request))
	var response RepairFetchResponse
	if status != 200 || json.Unmarshal(body, &response) != nil || validateRepairFetch(request, response) != nil || len(response.Records) != 1 {
		t.Fatalf("exact fetch failed: %d", status)
	}
	for _, kind := range []string{"component", "proof", "cap", "version"} {
		t.Run(kind, func(t *testing.T) {
			changed := entry
			switch kind {
			case "component":
				changed.ComponentSHA256 = strings.Repeat("0", 64)
			case "proof":
				changed.ProofSHA256 = strings.Repeat("0", 64)
			case "cap":
				changed.ExpiresAt = changed.ExpiresAt.Add(-time.Nanosecond)
			case "version":
				changed.Version.Sequence++
			}
			req := RepairFetchRequest{Version: RepairVersion, Entries: []RepairEntry{changed}}
			code, body := call(t, f, f.credentials, "POST", "/v1/repair/fetch", "replicate", jsonBody(t, req))
			var reply RepairFetchResponse
			if code != 200 || json.Unmarshal(body, &reply) != nil || validateRepairFetch(req, reply) != nil || len(reply.Records) != 0 || len(reply.Unavailable) != 1 {
				t.Fatal("stale fetch did not return exact unavailable coverage")
			}
		})
	}
	duplicate := RepairFetchRequest{Version: RepairVersion, Entries: []RepairEntry{entry, entry}}
	if code, _ := call(t, f, f.credentials, "POST", "/v1/repair/fetch", "replicate", jsonBody(t, duplicate)); code != 400 {
		t.Fatalf("duplicate fetch keys: %d", code)
	}
	for name, reply := range map[string]RepairFetchResponse{
		"missing":             {Version: RepairVersion},
		"duplicate":           {Version: RepairVersion, Records: []RepairRecord{response.Records[0], response.Records[0]}},
		"overlap":             {Version: RepairVersion, Records: response.Records, Unavailable: []string{repairKeyDigest(entry.Key)}},
		"foreign-unavailable": {Version: RepairVersion, Unavailable: []string{strings.Repeat("0", 64)}},
	} {
		t.Run(name, func(t *testing.T) {
			if err := validateRepairFetch(request, reply); err == nil {
				t.Fatal("invalid response coverage accepted")
			}
		})
	}
	target := newFixture(t, f.root, "receiver")
	changed := response.Records[0]
	changed.Record.Component.Payload = json.RawMessage(`{"concentration":999}`)
	if _, err := target.n.applyRepairPage([]RepairRecord{changed}); err == nil {
		t.Fatal("exact entry with altered record payload accepted")
	}
	if cp := publicationCheckpoint(t, target); len(cp.Entries) != 0 {
		t.Fatal("invalid fetched proof changed durable replica")
	}
}

func TestRepairReviewMultiPageTraversalAndCurrentAuthority(t *testing.T) {
	f := newFixture(t, nil, "origin")
	f.cfg.MaxRecords, f.wc.MaxEntries = 128, 128
	reopenPublication(t, f)
	const count = MaxRepairInventoryEntries + 3
	for offset := 0; offset < count; offset += store.MaxRepairRecords {
		page := []RepairRecord{}
		for i := offset; i < count && i < offset+store.MaxRepairRecords; i++ {
			c := component(f, 1, 1)
			c.Key.Entity = fmt.Sprintf("retained-%03d", i)
			page = append(page, reviewRepairRecord(t, signed(t, f, c), c.Timing.SampleTime.Add(c.TTL)))
		}
		if _, err := f.n.applyRepairPage(page); err != nil {
			t.Fatal(err)
		}
	}
	code, first := reviewInventory(t, f, f.credentials, "")
	if code != 200 || len(first.Entries) != MaxRepairInventoryEntries || first.Cursor == "" {
		t.Fatalf("first page not bounded: %d entries=%d", code, len(first.Entries))
	}
	code, second := reviewInventory(t, f, f.credentials, first.Cursor)
	if code != 200 || len(second.Entries) != 3 || second.Cursor != "" {
		t.Fatalf("continuation incomplete: %d entries=%d", code, len(second.Entries))
	}
	seen := map[world.Key]bool{}
	previous := ""
	for _, e := range append(first.Entries, second.Entries...) {
		key := repairKeyDigest(e.Key)
		if seen[e.Key] || key <= previous {
			t.Fatal("cursor duplicated or reordered retained keys")
		}
		seen[e.Key], previous = true, key
	}
	if len(seen) != count {
		t.Fatal("cursor lost retained keys")
	}
	now := time.Now()
	rev, err := authority.SignRevocations(authority.Revocations{Version: 1, Tenant: "tenant", Site: "office", Domain: "simulation", Revision: 2, IssuedAtUnixMS: now.Add(-time.Second).UnixMilli(), ExpiresAtUnixMS: now.Add(30 * time.Minute).UnixMilli(), GrantIDs: []string{f.credentials.Grant.Grant.ID}}, f.root)
	if err != nil || f.v.UpdateRevocations(rev) != nil {
		t.Fatal("revocation setup failed", err)
	}
	if code, _ := reviewInventory(t, f, f.credentials, first.Cursor); code != http.StatusUnauthorized {
		t.Fatalf("MAC cursor substituted for current request authority: %d", code)
	}
}

func TestRepairReviewLateWorldDenialKeepsProofCoverageAndPriorWorld(t *testing.T) {
	origin := newFixture(t, nil, "origin")
	target := newFixture(t, origin.root, "receiver")
	prior := component(origin, 1, 1)
	prior.Key.Entity = "prior"
	retained := reviewRepairRecord(t, signed(t, origin, prior), prior.Timing.SampleTime.Add(prior.TTL))
	if _, err := target.n.applyRepairPage([]RepairRecord{retained}); err != nil {
		t.Fatal(err)
	}
	valid := component(origin, 1, 1)
	valid.Key.Entity = "new-valid"
	invalid := component(origin, 1, 1)
	invalid.Key.Entity = "new-unknown-freshness"
	invalid.Timing.ErrorKnown = false
	page := []RepairRecord{
		reviewRepairRecord(t, signed(t, origin, valid), valid.Timing.SampleTime.Add(valid.TTL)),
		reviewRepairRecord(t, signed(t, origin, invalid), invalid.Timing.SampleTime.Add(invalid.TTL)),
	}
	// Both proofs really authorize their origins: only the later world merge
	// rejects the second component, after the proof superset was persisted.
	if _, err := target.v.VerifyRecords([]authority.SignedRecord{page[0].Record, page[1].Record}); err != nil {
		t.Fatal("test failed before the intended merge boundary", err)
	}
	before := publicationCheckpoint(t, target)
	if results, err := target.n.applyRepairPage(page); err == nil || results != nil {
		t.Fatal("page exposed partially admitted results")
	}
	after := publicationCheckpoint(t, target)
	if len(after.Entries) != 1 || after.Entries[0].Component.Key != prior.Key || after.CapturedAt.Before(before.CapturedAt) {
		t.Fatal("late denial lost prior world or partially committed page")
	}
	reopenPublication(t, target)
	inv, err := target.n.localRepairInventory()
	if err != nil || len(inv) != 1 || !sameRepairEntry(inv[prior.Key], retained.Entry) {
		t.Fatal("proof superset changed admitted world after reopen", err)
	}
	if results, err := target.n.applyRepairPage(page[:1]); err != nil || len(results) != 1 || results[0] != world.Applied {
		t.Fatal("known denial incorrectly poisoned next valid page", results, err)
	}
}

func TestRepairReviewProofRenewalExpiryAndTombstone(t *testing.T) {
	origin := newFixture(t, nil, "origin")
	target := newFixture(t, origin.root, "receiver")
	c := component(origin, 1, 1)
	c.Timing.SampleTime = time.Now().Add(-2 * time.Second)
	old, err := authority.SignRecord(origin.credentials.Grant, origin.credentials.PrivateKey, c, time.Now().Add(-time.Second))
	if err != nil {
		t.Fatal(err)
	}
	first := reviewRepairRecord(t, old, c.Timing.SampleTime.Add(c.TTL))
	if _, err := target.n.applyRepairPage([]RepairRecord{first}); err != nil {
		t.Fatal(err)
	}
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	g := old.Grant.Grant
	g.ID += "-renewed"
	g.PublicKey = public
	grant, err := authority.SignGrant(g, origin.root)
	if err != nil {
		t.Fatal(err)
	}
	renewed, err := authority.SignRecord(grant, private, c, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	shorter := reviewRepairRecord(t, renewed, time.Now().Add(30*time.Second))
	if !needsRepair(shorter.Entry, first.Entry, true) {
		t.Fatal("changed proof and shorter lifetime were skipped")
	}
	if results, err := target.n.applyRepairPage([]RepairRecord{shorter}); err != nil || len(results) != 1 || results[0] != world.Duplicate {
		t.Fatal(results, err)
	}
	now := time.Now()
	rev, err := authority.SignRevocations(authority.Revocations{Version: 1, Tenant: "tenant", Site: "office", Domain: "simulation", Revision: 2, IssuedAtUnixMS: now.Add(-time.Second).UnixMilli(), ExpiresAtUnixMS: now.Add(30 * time.Minute).UnixMilli(), GrantIDs: []string{old.Grant.Grant.ID}, KeyIDs: []string{authority.KeyID(old.Grant.Grant.PublicKey)}}, origin.root)
	if err != nil || target.v.UpdateRevocations(rev) != nil {
		t.Fatal("revocation setup failed", err)
	}
	if _, err := target.v.VerifyRecord(old); err == nil {
		t.Fatal("old proof was not actually revoked")
	}
	reopenPublication(t, target)
	inv, err := target.n.localRepairInventory()
	if err != nil || !sameRepairEntry(inv[c.Key], shorter.Entry) {
		t.Fatal("renewed proof or exact shortened cap lost on reopen", err)
	}
	if records, err := target.n.export(); err != nil || len(records) != 1 || records[0].Grant.Grant.ID != grant.Grant.ID {
		t.Fatal("renewed proof unavailable after old key revocation", err)
	}
	// Unsigned age changes neither transfer identity; a later cap cannot renew.
	ageChanged := renewed
	ageChanged.Component.Timing.AgeHint = time.Second
	stable := reviewRepairRecord(t, ageChanged, shorter.Entry.ExpiresAt)
	if needsRepair(stable.Entry, shorter.Entry, true) {
		t.Fatal("age alone caused a repeated repair")
	}
	longer := reviewRepairRecord(t, renewed, first.Entry.ExpiresAt)
	if needsRepair(longer.Entry, shorter.Entry, true) {
		t.Fatal("later peer cap requested a lifetime extension")
	}
	if _, err := target.n.applyRepairPage([]RepairRecord{longer}); err != nil {
		t.Fatal(err)
	}
	if got := publicationCheckpoint(t, target).Entries[0].ExpiresAt; !got.Equal(shorter.Entry.ExpiresAt) {
		t.Fatal("duplicate extended retained expiry")
	}
	// An already expired cap remains a repairable high-water record across reopen.
	expired := reviewRepairRecord(t, renewed, time.Now().Add(-time.Second))
	if _, err := target.n.applyRepairPage([]RepairRecord{expired}); err != nil {
		t.Fatal(err)
	}
	reopenPublication(t, target)
	if visible, err := target.s.Snapshot(); err != nil || len(visible) != 0 {
		t.Fatal("expired capped record resurrected", err)
	}
	inv, err = target.n.localRepairInventory()
	if err != nil || !sameRepairEntry(inv[c.Key], expired.Entry) {
		t.Fatal("expired high-water record omitted from repair inventory", err)
	}
	c.Version.Sequence++
	c.Tombstone, c.Payload = true, nil
	tombstone, err := authority.SignRecord(grant, private, c, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	dead := reviewRepairRecord(t, tombstone, time.Now().Add(time.Minute))
	if _, err := target.n.applyRepairPage([]RepairRecord{dead}); err != nil {
		t.Fatal(err)
	}
	if results, err := target.n.applyRepairPage([]RepairRecord{expired}); err != nil || results[0] != world.Obsolete {
		t.Fatal("obsolete live record displaced tombstone", results, err)
	}
	reopenPublication(t, target)
	inv, err = target.n.localRepairInventory()
	if err != nil || !sameRepairEntry(inv[c.Key], dead.Entry) {
		t.Fatal("tombstone high-water or cap changed", err)
	}
	if visible, err := target.s.Snapshot(); err != nil || len(visible) != 0 {
		t.Fatal("tombstone became visible", err)
	}
}

type reviewBlockedBody struct {
	entered chan struct{}
	release chan struct{}
}

func (b *reviewBlockedBody) Read([]byte) (int, error) {
	close(b.entered)
	<-b.release
	return 0, io.EOF
}
func (*reviewBlockedBody) Close() error { return nil }

func TestRepairReviewAdmittedBulkDoesNotGateSignedStop(t *testing.T) {
	f, operator, _, _, _ := taskFixture(t)
	spec := taskSpec("control-during-repair")
	if code, _ := call(t, f, operator, "POST", "/v1/tasks", "mission", instructionBody(t, f, operator, "mission", spec)); code != 200 {
		t.Fatal("mission setup failed")
	}
	body := &reviewBlockedBody{entered: make(chan struct{}), release: make(chan struct{})}
	done := make(chan struct{})
	req := httptest.NewRequest("POST", "/v1/repair/inventory", nil)
	req.Body = body
	go func() {
		defer close(done)
		f.n.Handler().ServeHTTP(httptest.NewRecorder(), req)
	}()
	defer func() {
		close(body.release)
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("blocked repair did not exit")
		}
	}()
	select {
	case <-body.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("bulk handler did not acquire its slot")
	}
	if code, _ := reviewInventory(t, f, f.credentials, ""); code != 429 {
		t.Fatalf("second bulk handler queued rather than refusing: %d", code)
	}
	stop := instructionBody(t, f, operator, "stop", StopRequest{TaskID: spec.ID})
	request := httptest.NewRequest("POST", "/v1/tasks/stop", bytes.NewReader(stop))
	if err := AuthorizeRequest(request, operator, f.audience, "stop", stop); err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	stopDone := make(chan struct{})
	go func() { f.n.Handler().ServeHTTP(response, request); close(stopDone) }()
	select {
	case <-stopDone:
	case <-time.After(2 * time.Second):
		t.Fatal("signed stop waited for unrelated bulk admission slot")
	}
	var result tasking.Record
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &result) != nil || result.Status.Phase != mission.Canceled {
		t.Fatalf("signed stop did not retain terminal state: status=%d", response.Code)
	}
	// This tests routing/admission independence, not preemption of already
	// running storage I/O or a 250 ms service latency guarantee.
}

type repairReviewCrashFixture struct {
	Root        ed25519.PublicKey           `json:"root"`
	Revocations authority.SignedRevocations `json:"revocations"`
	Page        []RepairRecord              `json:"page"`
}

// The child does not receive private signing material. It uses actual signed
// origin records and a signed simulation revocation snapshot from the parent.
func repairReviewCrashChild(t *testing.T, dir, phase string) {
	t.Helper()
	var fixture repairReviewCrashFixture
	b, err := os.ReadFile(filepath.Join(dir, "review-child.json"))
	if err != nil || json.Unmarshal(b, &fixture) != nil {
		t.Fatal("child fixture unavailable", err)
	}
	wc := world.Config{Tenant: "tenant", Site: "office", ClockDomain: "simulation:live", Now: time.Now, MaxEntries: 8, MaxFutureSkew: time.Second, AllowSynthetic: true, Frames: []world.FrameRef{{ID: "floor", Revision: "1"}}}
	path := filepath.Join(dir, "store.json")
	s, err := store.Open(path, wc)
	if err != nil {
		t.Fatal(err)
	}
	v, err := authority.NewVerifier(authority.Config{RootPublicKey: fixture.Root, Tenant: "tenant", Site: "office", Domain: "simulation", Audience: "receiver", Now: time.Now, Replay: authority.NewMemoryReplay(1000)})
	if err != nil || v.UpdateRevocations(fixture.Revocations) != nil {
		t.Fatal("child verifier unavailable", err)
	}
	n, err := New(Config{Tenant: "tenant", Site: "office", ProofPath: filepath.Join(dir, "proofs.json"), MaxRecords: 8}, s, v)
	if err != nil {
		t.Fatal(err)
	}
	if phase == "world-failure" {
		if err := os.Rename(path, path+".previous"); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
		if results, err := n.applyRepairPage(fixture.Page); err == nil || results != nil {
			t.Fatal("failed world writer returned admitted page results")
		}
		if _, err := s.Records(); !errors.Is(err, store.ErrFaulted) {
			t.Fatal("failed world writer did not poison reads", err)
		}
		before, err := os.ReadFile(n.cfg.ProofPath)
		if err != nil {
			t.Fatal(err)
		}
		if results, err := n.applyRepairPage(fixture.Page); !errors.Is(err, store.ErrFaulted) || results != nil {
			t.Fatal("poisoned store admitted retry", err)
		}
		after, err := os.ReadFile(n.cfg.ProofPath)
		if err != nil || !bytes.Equal(before, after) {
			t.Fatal("poisoned retry changed durable proof coverage", err)
		}
	} else if phase == "after-commit" {
		if results, err := n.applyRepairPage(fixture.Page); err != nil || len(results) != 8 {
			t.Fatal("child page did not commit", results, err)
		} else {
			for _, result := range results {
				if result != world.Applied {
					t.Fatal("child did not apply every new version", results)
				}
			}
		}
	} else {
		t.Fatal("unknown child phase")
	}
	// No Store.Close or deferred cleanup runs. Exact exit73 proves that all
	// assertions above reached the intended boundary rather than setup failure.
	os.Exit(73)
}

func TestRepairReviewSignedPageCrashRecoveryAtCapacity(t *testing.T) {
	if dir := os.Getenv("WENDY_REPAIR_REVIEW_CHILD_DIR"); dir != "" {
		repairReviewCrashChild(t, dir, os.Getenv("WENDY_REPAIR_REVIEW_CHILD_PHASE"))
		return
	}
	for _, phase := range []string{"world-failure", "after-commit"} {
		t.Run(phase, func(t *testing.T) {
			origin := newFixture(t, nil, "origin")
			target := newFixture(t, origin.root, "receiver")
			target.cfg.MaxRecords, target.wc.MaxEntries = 8, 8
			reopenPublication(t, target)
			oldPage, newPage := []RepairRecord{}, []RepairRecord{}
			for i := 0; i < 8; i++ {
				c := component(origin, 1, 1)
				c.Key.Entity = fmt.Sprintf("capacity-%d", i)
				cap := c.Timing.SampleTime.Add(c.TTL)
				oldPage = append(oldPage, reviewRepairRecord(t, signed(t, origin, c), cap))
				c.Version.Sequence++
				c.Payload = json.RawMessage(`{"concentration":30,"unit":"ppm"}`)
				newPage = append(newPage, reviewRepairRecord(t, signed(t, origin, c), cap))
			}
			if results, err := target.n.applyRepairPage(oldPage); err != nil || len(results) != 8 {
				t.Fatal("capacity setup failed", err)
			}
			dir := filepath.Dir(publicationPath(target))
			now := time.Now()
			rev, err := authority.SignRevocations(authority.Revocations{Version: 1, Tenant: "tenant", Site: "office", Domain: "simulation", Revision: 1, IssuedAtUnixMS: now.Add(-time.Second).UnixMilli(), ExpiresAtUnixMS: now.Add(30 * time.Minute).UnixMilli()}, origin.root)
			if err != nil {
				t.Fatal(err)
			}
			fixture := repairReviewCrashFixture{Root: origin.root.Public().(ed25519.PublicKey), Revocations: rev, Page: newPage}
			if err := os.WriteFile(filepath.Join(dir, "review-child.json"), jsonBody(t, fixture), 0600); err != nil {
				t.Fatal(err)
			}
			target.server.Close()
			if err := target.s.Close(); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			child := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestRepairReviewSignedPageCrashRecoveryAtCapacity$")
			child.Env = append(os.Environ(), "WENDY_REPAIR_REVIEW_CHILD_DIR="+dir, "WENDY_REPAIR_REVIEW_CHILD_PHASE="+phase)
			output, err := child.CombinedOutput()
			var exit *exec.ExitError
			if ctx.Err() != nil || !errors.As(err, &exit) || exit.ExitCode() != 73 {
				t.Fatalf("child failed before exact crash boundary: %v, %s", err, output)
			}
			proofBytes, err := os.ReadFile(target.cfg.ProofPath)
			if err != nil {
				t.Fatal(err)
			}
			var saved struct {
				Version int                      `json:"version"`
				Records []authority.SignedRecord `json:"records"`
			}
			if json.Unmarshal(proofBytes, &saved) != nil || saved.Version != 2 || len(saved.Records) != 16 {
				t.Fatal("durable v2 proof superset must contain eight old plus eight candidate proofs")
			}
			actual := map[string]bool{}
			for _, record := range saved.Records {
				if _, err := target.v.VerifyRecord(record); err != nil {
					t.Fatal("durable superset contains an unauthenticated origin", err)
				}
				digest, err := repairProofDigest(record)
				if err != nil || actual[digest] {
					t.Fatal("duplicate or invalid durable origin proof", err)
				}
				actual[digest] = true
			}
			for _, want := range append(oldPage, newPage...) {
				if !actual[want.Entry.ProofSHA256] {
					t.Fatal("missing exact signed old-or-new proof")
				}
			}
			wantPage := newPage
			if phase == "world-failure" {
				wantPage = oldPage
				path := publicationPath(target)
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(path+".previous", path); err != nil {
					t.Fatal(err)
				}
			}
			reopenPublication(t, target)
			inventory, err := target.n.localRepairInventory()
			if err != nil || len(inventory) != 8 {
				t.Fatal("crash recovery lost admitted capacity", err)
			}
			for _, want := range wantPage {
				if !sameRepairEntry(inventory[want.Entry.Key], want.Entry) {
					t.Fatal("crash recovery mixed old/new versions, proof or expiry")
				}
			}
			// The identical 16-record payload labeled v1 must retain its old
			// MaxRecords+1 bound. Only explicit v2 permits the page superset.
			saved.Version = 1
			if err := os.WriteFile(target.cfg.ProofPath, jsonBody(t, saved), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := New(target.cfg, target.s, target.v); err == nil {
				t.Fatal("v1 reader contract accepted the v2 page superset")
			}
			if err := os.WriteFile(target.cfg.ProofPath, proofBytes, 0600); err != nil {
				t.Fatal(err)
			}
			if phase == "world-failure" {
				if results, err := target.n.applyRepairPage(newPage); err != nil || len(results) != 8 {
					t.Fatal("reopened old world could not retry exact page", results, err)
				}
				reopenPublication(t, target)
				inventory, err := target.n.localRepairInventory()
				if err != nil || len(inventory) != 8 {
					t.Fatal("retry recovery failed", err)
				}
				for _, want := range newPage {
					if !sameRepairEntry(inventory[want.Entry.Key], want.Entry) {
						t.Fatal("retry did not retain exact new page")
					}
				}
			}
		})
	}
}

func TestRepairReviewValidV1ProofMigratesOnPageWrite(t *testing.T) {
	origin := newFixture(t, nil, "origin")
	target := newFixture(t, origin.root, "receiver")
	c := component(origin, 1, 1)
	first := reviewRepairRecord(t, signed(t, origin, c), time.Now().Add(30*time.Second))
	if results, err := target.n.applyRepairPage([]RepairRecord{first}); err != nil || len(results) != 1 || results[0] != world.Applied {
		t.Fatal("initial proof fixture failed", results, err)
	}
	data, err := os.ReadFile(target.cfg.ProofPath)
	if err != nil {
		t.Fatal(err)
	}
	var saved struct {
		Version int                      `json:"version"`
		Records []authority.SignedRecord `json:"records"`
	}
	if err := json.Unmarshal(data, &saved); err != nil || saved.Version != 2 || len(saved.Records) != 1 {
		t.Fatal("initial proof snapshot invalid", err)
	}
	if digest, err := repairProofDigest(saved.Records[0]); err != nil || digest != first.Entry.ProofSHA256 {
		t.Fatal("initial snapshot changed the exact origin proof", err)
	}
	// This is a valid old-format snapshot, within MaxRecords+1, paired with
	// its unchanged committed world. Only the format version is rewritten.
	saved.Version = 1
	if err := os.WriteFile(target.cfg.ProofPath, jsonBody(t, saved), 0600); err != nil {
		t.Fatal(err)
	}
	reopenPublication(t, target)
	inventory, err := target.n.localRepairInventory()
	if err != nil || len(inventory) != 1 || !sameRepairEntry(inventory[c.Key], first.Entry) {
		t.Fatal("valid v1 proof lost exact world identity, origin proof or expiry cap", err)
	}
	if records, err := target.n.export(); err != nil || len(records) != 1 {
		t.Fatal("v1 origin proof did not authenticate after reopen", err)
	}
	c.Version.Sequence++
	c.Payload = json.RawMessage(`{"concentration":31,"unit":"ppm"}`)
	updated := reviewRepairRecord(t, signed(t, origin, c), first.Entry.ExpiresAt)
	if results, err := target.n.applyRepairPage([]RepairRecord{updated}); err != nil || len(results) != 1 || results[0] != world.Applied {
		t.Fatal("valid page failed after v1 reopen", results, err)
	}
	data, err = os.ReadFile(target.cfg.ProofPath)
	if err != nil || json.Unmarshal(data, &saved) != nil || saved.Version != 2 || len(saved.Records) != 2 {
		t.Fatal("page write did not migrate to v2 old/new proof coverage", err)
	}
	wantProofs := map[string]bool{first.Entry.ProofSHA256: true, updated.Entry.ProofSHA256: true}
	for _, proof := range saved.Records {
		digest, err := repairProofDigest(proof)
		if err != nil || !wantProofs[digest] {
			t.Fatal("migration changed or duplicated origin proof", err)
		}
		delete(wantProofs, digest)
		if _, err := target.v.VerifyRecord(proof); err != nil {
			t.Fatal("migrated proof failed real origin verification", err)
		}
	}
	if len(wantProofs) != 0 {
		t.Fatal("migration omitted required proof coverage")
	}
	reopenPublication(t, target)
	inventory, err = target.n.localRepairInventory()
	if err != nil || len(inventory) != 1 || !sameRepairEntry(inventory[c.Key], updated.Entry) {
		t.Fatal("migrated v2 reopen lost exact updated state", err)
	}
}
