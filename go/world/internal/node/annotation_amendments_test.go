package node

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/annotations"
	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

func amendRecord(t *testing.T, f *fixture, original authority.SignedRecord, prior *authority.SignedRecord, action, text string, sequence uint64) authority.SignedRecord {
	t.Helper()
	raw, _ := json.Marshal(original)
	_, note, e := annotations.Original(annotations.Amendment{OriginalRecord: raw})
	if e != nil {
		t.Fatal(e)
	}
	a := annotations.Amendment{OriginalRecord: raw, OriginalSHA256: annotations.SHA256(raw), PredecessorSHA256: annotations.SHA256(raw), Action: action, AuthorPrincipal: note.AuthorPrincipal, Text: text, PositionMM: note.PositionMM, AuthoredAtUnixMS: time.Now().UnixMilli(), ExpiresAtUnixMS: note.ExpiresAtUnixMS}
	if prior != nil {
		a.PredecessorRecord, _ = json.Marshal(prior)
		a.PredecessorSHA256 = annotations.SHA256(prior.Component.Payload)
	}
	c, e := annotations.BuildAmendment(a, annotations.Config{Tenant: f.cfg.Tenant, Site: f.cfg.Site, RunID: original.Component.Provenance.Simulation.RunID, Source: original.Component.Key.Source, Frame: original.Component.Frame, Epoch: original.Component.Version.Epoch, Sequence: sequence})
	if e != nil {
		t.Fatal(e)
	}
	r, e := authority.SignAnnotation(f.credentials.Grant, f.credentials.PrivateKey, c)
	if e != nil {
		t.Fatal(e)
	}
	return r
}
func views(t *testing.T, f *fixture) []AnnotationView {
	t.Helper()
	status, body := call(t, f, f.credentials, http.MethodPost, "/v1/annotations/views", "read", []byte(`{"revision":"","offset":0,"limit":8}`))
	if status != 200 {
		t.Fatal(status, string(body))
	}
	var out AnnotationViewsResponse
	if json.Unmarshal(body, &out) != nil || !out.Complete {
		t.Fatal(string(body))
	}
	return out.Views
}
func TestAnnotationAmendmentWithdrawalBeforeOriginalNeverResurrects(t *testing.T) {
	f := newFixture(t, nil, "owner")
	annotationOperator(t, f)
	original := annotationRecord(t, f, annotationEpoch(t, f))
	withdraw := amendRecord(t, f, original, nil, "withdraw", "", 2)
	if code, b := postAnnotation(t, f, withdraw); code != 200 {
		t.Fatal(code, string(b))
	}
	if got := views(t, f); len(got) != 1 || got[0].State != "withdrawn" || got[0].Text != "" {
		t.Fatal(got)
	}
	if code, b := postAnnotation(t, f, original); code != 200 {
		t.Fatal(code, string(b))
	}
	snapshot, e := f.n.query()
	if e != nil {
		t.Fatal(e)
	}
	for _, c := range snapshot.Components {
		if c.Key == original.Component.Key {
			t.Fatal("legacy original resurrected")
		}
	}
	if got := views(t, f); len(got) != 1 || got[0].State != "withdrawn" {
		t.Fatal(got)
	}
	f.n.lock()
	reopened, e := New(f.cfg, f.s, f.v)
	f.n.mu.Unlock()
	if e != nil {
		t.Fatal(e)
	}
	f.n = reopened
	f.n.lock()
	got, e := f.n.annotationViewsLocked(time.Now())
	f.n.mu.Unlock()
	if e != nil || len(got) != 1 || got[0].State != "withdrawn" {
		t.Fatal(got, e)
	}
}
func TestAnnotationAmendmentForkAndMissingArrival(t *testing.T) {
	f := newFixture(t, nil, "owner")
	annotationOperator(t, f)
	original := annotationRecord(t, f, annotationEpoch(t, f))
	one := amendRecord(t, f, original, nil, "correct", "Door is east", 2)
	two := amendRecord(t, f, original, &one, "correct", "Use east entry", 3)
	// Ancestor signed bytes allow this latest correction to arrive before all parents.
	if code, b := postAnnotation(t, f, two); code != 200 {
		t.Fatal(code, string(b))
	}
	got := views(t, f)
	if len(got) != 1 || got[0].State != "corrected" || got[0].Text != "Use east entry" || len(got[0].AmendmentSHA256) != 2 {
		t.Fatal(got)
	}
	fork := amendRecord(t, f, original, nil, "correct", "West entry instead", 4)
	if code, b := postAnnotation(t, f, fork); code != 200 {
		t.Fatal(code, string(b))
	}
	got = views(t, f)
	if got[0].State != "conflict" || !got[0].Conflict || got[0].Text != "" {
		t.Fatal(got)
	}
	withdraw := amendRecord(t, f, original, nil, "withdraw", "", 5)
	if code, b := postAnnotation(t, f, withdraw); code != 200 {
		t.Fatal(code, string(b))
	}
	got = views(t, f)
	if got[0].State != "withdrawn" || !got[0].Conflict {
		t.Fatal(got)
	}
}
func TestAnnotationAmendmentRejectsTamperedAncestryAndExpiry(t *testing.T) {
	for _, variant := range []string{"digest", "expiry", "predecessor", "original-proof", "frame", "run", "author", "key"} {
		t.Run(variant, func(t *testing.T) {
			f := newFixture(t, nil, "owner")
			annotationOperator(t, f)
			original := annotationRecord(t, f, annotationEpoch(t, f))
			r := amendRecord(t, f, original, nil, "correct", "Replacement", 2)
			var a annotations.Amendment
			json.Unmarshal(r.Component.Payload, &a)
			switch variant {
			case "digest":
				a.OriginalSHA256 = annotations.SHA256([]byte("other"))
			case "expiry":
				a.ExpiresAtUnixMS++
			case "predecessor":
				a.PredecessorSHA256 = annotations.SHA256([]byte("missing"))
			case "original-proof":
				var p authority.SignedRecord
				json.Unmarshal(a.OriginalRecord, &p)
				p.Signature[0] ^= 1
				a.OriginalRecord, _ = json.Marshal(p)
				a.OriginalSHA256 = annotations.SHA256(a.OriginalRecord)
				a.PredecessorSHA256 = a.OriginalSHA256
			case "frame":
				r.Component.Frame.Revision = "other"
			case "run":
				r.Component.Provenance.Simulation.RunID = "other"
			case "author":
				a.AuthorPrincipal = "other"
			case "key":
				other := newFixture(t, f.root, "other")
				annotationOperator(t, other)
				r.Grant = other.credentials.Grant
			}
			r.Component.Payload, _ = json.Marshal(a)
			r.Component.Key.Entity = annotations.AmendmentPrefix + annotations.SHA256(r.Component.Payload)
			var e error
			r, e = authority.SignRecord(r.Grant, f.credentials.PrivateKey, r.Component, time.UnixMilli(r.SignedAtUnixMS))
			if e == nil {
				if _, e = f.n.apply(r, false); e == nil {
					t.Fatal("tampered amendment admitted")
				}
			}
			if entries, _ := f.s.Records(); len(entries) != 0 {
				t.Fatal("rejected amendment wrote world records")
			}
		})
	}
}
func TestAnnotationAmendmentPreservesUnrelatedNoteAndDeadline(t *testing.T) {
	f := newFixture(t, nil, "owner")
	annotationOperator(t, f)
	original := annotationRecord(t, f, annotationEpoch(t, f))
	withdraw := amendRecord(t, f, original, nil, "withdraw", "", 2)
	if _, e := f.n.apply(withdraw, false); e != nil {
		t.Fatal(e)
	}
	other := original
	var n annotations.Note
	json.Unmarshal(other.Component.Payload, &n)
	n.ID = "12345678-1234-4123-8123-123456789abd"
	var e error
	other.Component, e = annotations.Build(n, annotations.Config{Tenant: f.cfg.Tenant, Site: f.cfg.Site, RunID: "live", Source: original.Component.Key.Source, Frame: world.FrameRef{ID: "floor", Revision: "1"}, Epoch: 1, Sequence: 3})
	if e != nil {
		t.Fatal(e)
	}
	other = resignAnnotation(t, f, other)
	if _, e = f.n.apply(other, false); e != nil {
		t.Fatal(e)
	}
	got := views(t, f)
	if len(got) != 2 {
		t.Fatal(got)
	}
	seen := false
	for _, v := range got {
		if v.State == "original" && v.Text == n.Text {
			seen = true
		}
	}
	if !seen {
		t.Fatal("unrelated note hidden", got)
	}
	f.n.lock()
	expired, e := f.n.annotationViewsLocked(time.UnixMilli(n.ExpiresAtUnixMS))
	f.n.mu.Unlock()
	if e != nil {
		t.Fatal(e)
	}
	for _, v := range expired {
		if v.State != "expired" || v.Text != "" {
			t.Fatal(v)
		}
	}
}

func TestAnnotationAmendmentRepairPreservesOriginalAndWatchSuppressesIt(t *testing.T) {
	a := newFixture(t, nil, "owner")
	annotationOperator(t, a)
	original := annotationRecord(t, a, annotationEpoch(t, a))
	withdraw := amendRecord(t, a, original, nil, "withdraw", "", 2)
	for _, r := range []authority.SignedRecord{original, withdraw} {
		if _, e := a.n.apply(r, false); e != nil {
			t.Fatal(e)
		}
	}
	exported, e := a.n.export()
	if e != nil || len(exported) != 2 {
		t.Fatal("repair lost immutable history", len(exported), e)
	}
	b := newFixture(t, a.root, "reader")
	records := []RepairRecord{}
	for _, r := range []authority.SignedRecord{withdraw, original} {
		entry, e := repairEntry(world.CheckpointEntry{Component: r.Component, ExpiresAt: r.Component.Timing.SampleTime.Add(r.Component.TTL)}, r)
		if e != nil {
			t.Fatal(e)
		}
		records = append(records, RepairRecord{Entry: entry, Record: r})
	}
	if _, e = b.n.applyRepairPage(records); e != nil {
		t.Fatal(e)
	}
	b.n.lock()
	rows, e := b.n.annotationViewsLocked(time.Now())
	b.n.mu.Unlock()
	if e != nil || len(rows) != 1 || rows[0].State != "withdrawn" {
		t.Fatal(rows, e)
	}
	status, raw := call(t, a, a.credentials, http.MethodGet, "/v1/watch?timeout_ms=0", "read", nil)
	if status != 200 {
		t.Fatal(status, string(raw))
	}
	var response QueryResponse
	if e = json.Unmarshal(raw, &response); e != nil {
		t.Fatal(e)
	}
	for _, c := range response.Components {
		if c.Key == original.Component.Key {
			t.Fatal("watch exposed obsolete original")
		}
	}
}

// Hold the same lock used by node revocation installation while repair finishes
// its first proof pass. The stack observation proves this is the lock-wait path,
// rather than a revocation that simply happened before the first verification.
func TestAnnotationAmendmentRepairRechecksRevocationAfterLockWait(t *testing.T) {
	for _, direct := range []bool{false, true} {
		t.Run(fmt.Sprint(direct), func(t *testing.T) { amendmentRevocationDuringLock(t, direct) })
	}
}
func amendmentRevocationDuringLock(t *testing.T, direct bool) {
	a := newFixture(t, nil, "owner")
	annotationOperator(t, a)
	original := annotationRecord(t, a, annotationEpoch(t, a))
	amendment := amendRecord(t, a, original, nil, "correct", "Corrected", 2)
	b := newFixture(t, a.root, "replica")
	entry, err := repairEntry(world.CheckpointEntry{Component: amendment.Component, ExpiresAt: amendment.Component.Timing.SampleTime.Add(amendment.Component.TTL)}, amendment)
	if err != nil {
		t.Fatal(err)
	}
	b.n.mu.Lock()
	locked := true
	defer func() {
		if locked {
			b.n.mu.Unlock()
		}
	}()
	done := make(chan error, 1)
	go func() {
		var e error
		if direct {
			_, e = b.n.apply(amendment, false)
		} else {
			_, e = b.n.applyRepairPage([]RepairRecord{{Entry: entry, Record: amendment}})
		}
		done <- e
	}()
	waiting := false
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		buf := make([]byte, 1<<20)
		size := runtime.Stack(buf, true)
		for _, stack := range strings.Split(string(buf[:size]), "\n\n") {
			if (strings.Contains(stack, "(*Node).applyRepairPageContext") || strings.Contains(stack, "(*Node).applyContext")) && strings.Contains(stack, "(*Node).lock") {
				waiting = true
				break
			}
		}
		if waiting {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if !waiting {
		t.Fatal("repair never reached the node lock after initial verification")
	}
	now := time.Now()
	rev, err := authority.SignRevocations(authority.Revocations{Version: 1, Tenant: b.cfg.Tenant, Site: b.cfg.Site, Domain: "simulation", Revision: 2, IssuedAtUnixMS: now.Add(-time.Second).UnixMilli(), ExpiresAtUnixMS: now.Add(time.Minute).UnixMilli(), GrantIDs: []string{amendment.Grant.Grant.ID}}, b.root)
	if err != nil {
		t.Fatal(err)
	}
	if err = b.v.UpdateRevocations(rev); err != nil {
		t.Fatal(err)
	}
	b.n.mu.Unlock()
	locked = false
	select {
	case err = <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("repair did not finish")
	}
	if !errors.Is(err, authority.ErrDenied) {
		t.Fatalf("revoked amendment admitted after lock wait: %v", err)
	}
	if records, err := b.s.Records(); err != nil || len(records) != 0 {
		t.Fatal("rejected amendment changed durable world", records, err)
	}
	b.n.lock()
	rows, err := b.n.annotationViewsLocked(time.Now())
	b.n.mu.Unlock()
	if err != nil || len(rows) != 0 {
		t.Fatal("rejected amendment created a projection", rows, err)
	}
	reopenPublication(t, b)
	if records, err := b.s.Records(); err != nil || len(records) != 0 {
		t.Fatal("reopen resurrected rejected amendment", records, err)
	}
}

func TestAnnotationAmendmentOrphanProofNeverCreatesWorldOrProjection(t *testing.T) {
	f := newFixture(t, nil, "owner")
	annotationOperator(t, f)
	original := annotationRecord(t, f, annotationEpoch(t, f))
	amendment := amendRecord(t, f, original, nil, "withdraw", "", 2)
	digest, err := recordDigest(amendment.Component)
	if err != nil {
		t.Fatal(err)
	}
	// Model the durable proof superset left if the callback deadline expires
	// immediately after saveProofs, before ApplyPreparedBatch commits its world.
	f.n.lock()
	err = f.n.saveProofs(map[string]authority.SignedRecord{digest: amendment})
	f.n.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	reopenPublication(t, f)
	if records, err := f.s.Records(); err != nil || len(records) != 0 {
		t.Fatal(records, err)
	}
	f.n.lock()
	rows, err := f.n.annotationViewsLocked(time.Now())
	f.n.mu.Unlock()
	if err != nil || len(rows) != 0 {
		t.Fatal("orphan proof admitted an amendment", rows, err)
	}
	if _, err = f.n.apply(original, false); err != nil {
		t.Fatal(err)
	}
	got := views(t, f)
	if len(got) != 1 || got[0].State != "original" {
		t.Fatal("uncommitted withdrawal suppressed original", got)
	}
}

func TestAnnotationAmendmentOverflowFailsClosedForOnlyAffectedNote(t *testing.T) {
	f := newFixture(t, nil, "owner")
	annotationOperator(t, f)
	original := annotationRecord(t, f, annotationEpoch(t, f))
	if _, err := f.n.apply(original, false); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < annotations.MaxAmendments+1; i++ {
		r := amendRecord(t, f, original, nil, "correct", fmt.Sprintf("Branch %d", i), uint64(i+2))
		if _, err := f.n.apply(r, false); err != nil {
			t.Fatal(i, err)
		}
	}
	other := original
	var note annotations.Note
	json.Unmarshal(other.Component.Payload, &note)
	note.ID = "12345678-1234-4123-8123-123456789abd"
	var err error
	other.Component, err = annotations.Build(note, annotations.Config{Tenant: f.cfg.Tenant, Site: f.cfg.Site, RunID: "live", Source: original.Component.Key.Source, Frame: original.Component.Frame, Epoch: 1, Sequence: 40})
	if err != nil {
		t.Fatal(err)
	}
	other = resignAnnotation(t, f, other)
	if _, err = f.n.apply(other, false); err != nil {
		t.Fatal(err)
	}
	got := views(t, f)
	if len(got) != 2 {
		t.Fatal(got)
	}
	sawOverflow, sawOther := false, false
	for _, v := range got {
		if v.OriginalKey == original.Component.Key {
			sawOverflow = v.State == "incomplete" && v.Text == "" && len(v.AmendmentSHA256) == annotations.MaxAmendments
		}
		if v.OriginalKey == other.Component.Key {
			sawOther = v.State == "original" && v.Text == note.Text
		}
	}
	if !sawOverflow || !sawOther {
		t.Fatal("overflow hid unrelated note or chose a partial correction", got)
	}
	status, body := call(t, f, f.credentials, http.MethodPost, "/v1/annotations/views", "read", []byte(`{"revision":"","offset":0,"limit":1}`))
	if status != 200 {
		t.Fatal(status, string(body))
	}
	var page AnnotationViewsResponse
	json.Unmarshal(body, &page)
	if page.Complete || page.NextOffset != 1 || len(page.Views) != 1 {
		t.Fatal(page)
	}
	request, _ := json.Marshal(AnnotationViewsRequest{Revision: page.Revision, Offset: 1, Limit: 1})
	status, body = call(t, f, f.credentials, http.MethodPost, "/v1/annotations/views", "read", request)
	if status != 200 {
		t.Fatal(status, string(body))
	}
	var final AnnotationViewsResponse
	json.Unmarshal(body, &final)
	if !final.Complete || len(final.Views) != 1 || final.Views[0].OriginalSHA256 == page.Views[0].OriginalSHA256 {
		t.Fatal(final)
	}
}
