package node

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/annotations"
	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/store"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

func annotationOperator(t *testing.T, f *fixture) {
	t.Helper()
	g := f.credentials.Grant.Grant
	g.Kind = "operator"
	g.Scopes = []string{"read", "annotation-write"}
	var err error
	f.credentials.Grant, err = authority.SignGrant(g, f.root)
	if err != nil {
		t.Fatal(err)
	}
}
func annotationEpoch(t *testing.T, f *fixture) uint64 {
	t.Helper()
	status, body := call(t, f, f.credentials, http.MethodPost, "/v1/annotations/epoch", "annotation-write", []byte(`{}`))
	if status != 200 {
		t.Fatal(status, string(body))
	}
	var response struct {
		Epoch uint64 `json:"epoch"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		t.Fatal(err)
	}
	return response.Epoch
}
func annotationRecord(t *testing.T, f *fixture, epoch uint64) authority.SignedRecord {
	t.Helper()
	now := time.Now().Truncate(time.Millisecond)
	c, err := annotations.Build(annotations.Note{ID: "12345678-1234-4123-8123-123456789abc", AuthorPrincipal: f.credentials.Grant.Grant.Principal, Text: "Check north door", PositionMM: [3]int64{1200, 0, -500}, AuthoredAtUnixMS: now.UnixMilli(), ExpiresAtUnixMS: now.Add(time.Minute).UnixMilli()}, annotations.Config{Tenant: f.cfg.Tenant, Site: f.cfg.Site, RunID: "live", Source: f.credentials.Grant.Grant.Source, Frame: world.FrameRef{ID: "floor", Revision: "1"}, Epoch: epoch, Sequence: 1})
	if err != nil {
		t.Fatal(err)
	}
	r, err := authority.SignAnnotation(f.credentials.Grant, f.credentials.PrivateKey, c)
	if err != nil {
		t.Fatal(err)
	}
	return r
}
func postAnnotation(t *testing.T, f *fixture, r authority.SignedRecord) (int, []byte) {
	t.Helper()
	body, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	return call(t, f, f.credentials, http.MethodPost, "/v1/annotations", "annotation-write", body)
}
func resignAnnotation(t *testing.T, f *fixture, r authority.SignedRecord) authority.SignedRecord {
	t.Helper()
	r, err := authority.SignRecord(f.credentials.Grant, f.credentials.PrivateKey, r.Component, time.UnixMilli(r.SignedAtUnixMS))
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestAnnotationNarrowScopeAndImmutableRetry(t *testing.T) {
	f := newFixture(t, nil, "owner")
	annotationOperator(t, f)
	epoch := annotationEpoch(t, f)
	r := annotationRecord(t, f, epoch)
	for i, want := range []world.Result{world.Applied, world.Duplicate} {
		status, body := postAnnotation(t, f, r)
		if status != 200 {
			t.Fatal(i, status, string(body))
		}
		var response struct {
			Result    world.Result    `json:"result"`
			Component world.Component `json:"component"`
		}
		if err := json.Unmarshal(body, &response); err != nil {
			t.Fatal(err)
		}
		if response.Result != want || response.Component.TTL != r.Component.TTL || !response.Component.Timing.SampleTime.Equal(r.Component.Timing.SampleTime) {
			t.Fatal("retry changed immutable note", response)
		}
	}
	for _, route := range []string{"/v1/publishers/epoch", "/v1/components"} {
		body := []byte(`{}`)
		if route == "/v1/components" {
			body, _ = json.Marshal(r)
		}
		status, _ := call(t, f, f.credentials, http.MethodPost, route, "publish", body)
		if status != http.StatusUnauthorized {
			t.Fatal("annotation grant acquired general publish", status)
		}
	}
	changed := r
	changed.Component.Version.Sequence++
	changed = resignAnnotation(t, f, changed)
	status, _ := postAnnotation(t, f, changed)
	if status != http.StatusConflict {
		t.Fatal("immutable version accepted", status)
	}
	annotationEpoch(t, f)
	status, _ = postAnnotation(t, f, r)
	if status != http.StatusConflict {
		t.Fatal("old pending epoch silently rebased", status)
	}
	if got, _ := f.s.Snapshot(); len(got) != 1 || got[0].Version.Epoch != epoch {
		t.Fatal("conflict changed note")
	}
}

func TestAnnotationOwnerBindingAndInvalidSchemaAllAdmissions(t *testing.T) {
	a := newFixture(t, nil, "a")
	b := newFixture(t, a.root, "b")
	annotationOperator(t, a)
	if status, _ := call(t, b, a.credentials, http.MethodPost, "/v1/annotations/epoch", "annotation-write", []byte(`{}`)); status != http.StatusUnauthorized {
		t.Fatal("nonowner allocated annotation epoch", status)
	}
	r := annotationRecord(t, a, annotationEpoch(t, a))
	for name, change := range map[string]func(*authority.SignedRecord){
		"unknown payload": func(r *authority.SignedRecord) {
			r.Component.Payload = append(r.Component.Payload[:len(r.Component.Payload)-1], []byte(`,"sensor":true}`)...)
		},
		"tombstone":  func(r *authority.SignedRecord) { r.Component.Tombstone = true; r.Component.Payload = nil },
		"wrong type": func(r *authority.SignedRecord) { r.Component.Key.Type = "wendy.sensor.v1" },
		"wrong run":  func(r *authority.SignedRecord) { r.Component.Timing.ClockDomain = "simulation:another" },
		"spoof author": func(r *authority.SignedRecord) {
			var n annotations.Note
			_ = json.Unmarshal(r.Component.Payload, &n)
			n.AuthorPrincipal = "someone-else"
			r.Component.Payload, _ = json.Marshal(n)
		},
		"measured": func(r *authority.SignedRecord) {
			r.Component.Provenance = world.Provenance{Origin: world.Measured, AppVersion: annotations.AppVersion}
		},
	} {
		t.Run(name, func(t *testing.T) {
			raw, _ := json.Marshal(r)
			var invalid authority.SignedRecord
			_ = json.Unmarshal(raw, &invalid)
			change(&invalid)
			invalid = resignAnnotation(t, a, invalid)
			for _, local := range []bool{true, false} {
				if _, err := a.n.apply(invalid, local); err == nil {
					t.Fatal("invalid origin admitted", local)
				}
			}
		})
	}
	// A broad publish grant still lacks the dedicated annotation scope.
	g := a.credentials.Grant.Grant
	g.Scopes = []string{"read", "publish"}
	a.credentials.Grant, _ = authority.SignGrant(g, a.root)
	r = resignAnnotation(t, a, r)
	body, _ := json.Marshal(r)
	status, _ := call(t, a, a.credentials, http.MethodPost, "/v1/components", "publish", body)
	if status == 200 {
		t.Fatal("general publisher bypassed annotation scope")
	}
	if _, err := b.n.apply(r, false); err == nil {
		t.Fatal("relay bypassed annotation scope")
	}
}

func TestAnnotationPeersSameUUIDIndependentSourcesAndRestart(t *testing.T) {
	a := newFixture(t, nil, "a")
	b := newFixture(t, a.root, "b")
	aPeer, bPeer := a.credentials, b.credentials
	annotationOperator(t, a)
	annotationOperator(t, b)
	ra, rb := annotationRecord(t, a, annotationEpoch(t, a)), annotationRecord(t, b, annotationEpoch(t, b))
	for _, x := range []struct {
		f *fixture
		r authority.SignedRecord
	}{{a, ra}, {b, rb}} {
		if status, body := postAnnotation(t, x.f, x.r); status != 200 {
			t.Fatal(status, string(body))
		}
	}
	if err := a.n.Sync(context.Background(), Peer{b.server.URL, "b"}, aPeer); err != nil {
		t.Fatal(err)
	}
	if err := b.n.Sync(context.Background(), Peer{a.server.URL, "a"}, bPeer); err != nil {
		t.Fatal(err)
	}
	for _, f := range []*fixture{a, b} {
		q, err := f.n.query()
		if err != nil || len(q.Components) != 2 {
			t.Fatal("source owned notes did not converge", len(q.Components), err)
		}
	}
	b.server.Close()
	if err := b.s.Close(); err != nil {
		t.Fatal(err)
	}
	var err error
	b.s, err = store.Open(filepath.Join(filepath.Dir(b.cfg.ProofPath), "store.json"), b.wc)
	if err != nil {
		t.Fatal(err)
	}
	b.n, err = New(b.cfg, b.s, b.v)
	if err != nil {
		t.Fatal(err)
	}
	q, err := b.n.query()
	if err != nil || len(q.Components) != 2 {
		t.Fatal("notes lost on reopen", err)
	}
	changed := ra
	changed.Component.Version.Sequence++
	changed = resignAnnotation(t, a, changed)
	if _, err = b.n.apply(changed, false); !errors.Is(err, ErrAnnotationConflict) {
		t.Fatal("relay changed immutable note", err)
	}
}

func TestAnnotationExpiryDoesNotRenewAcrossReopen(t *testing.T) {
	f := newFixture(t, nil, "a")
	annotationOperator(t, f)
	r := annotationRecord(t, f, annotationEpoch(t, f))
	if status, body := postAnnotation(t, f, r); status != 200 {
		t.Fatal(status, string(body))
	}
	f.server.Close()
	if err := f.s.Close(); err != nil {
		t.Fatal(err)
	}
	f.wc.Now = func() time.Time { return time.Now().Add(2 * time.Minute) }
	var err error
	f.s, err = store.Open(filepath.Join(filepath.Dir(f.cfg.ProofPath), "store.json"), f.wc)
	if err != nil {
		t.Fatal(err)
	}
	f.n, err = New(f.cfg, f.s, f.v)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.n.apply(r, false); err != nil {
		t.Fatal("expired retained proof should remain idempotent", err)
	}
	q, err := f.n.query()
	if err != nil || len(q.Components) != 0 {
		t.Fatal("expired note resurrected", err)
	}
	all, err := f.s.Records()
	if err != nil || len(all) != 1 {
		t.Fatal("immutable highwater lost", err)
	}
}

func TestAnnotationStoredMalformedProofFailsReopenAndQuery(t *testing.T) {
	for _, malformed := range []bool{false, true} {
		t.Run(map[bool]string{false: "forged signature", true: "schema bypass"}[malformed], func(t *testing.T) {
			f := newFixture(t, nil, "a")
			annotationOperator(t, f)
			r := annotationRecord(t, f, annotationEpoch(t, f))
			if malformed {
				r.Component.Payload = append(r.Component.Payload[:len(r.Component.Payload)-1], []byte(`,"zone":"safe"}`)...)
				r = resignAnnotation(t, f, r)
			} else {
				r.Signature = make([]byte, 64)
			}
			// Simulate persisted state from an older, nonvalidating producer. The
			// low-level opaque replica deliberately does not know this schema.
			if _, err := f.s.Apply(r.Component.Key.Source, r.Component); err != nil {
				t.Fatal(err)
			}
			digest, _ := recordDigest(r.Component)
			f.n.proofs = map[string]authority.SignedRecord{digest: r}
			if err := f.n.saveProofs(f.n.proofs); err != nil {
				t.Fatal(err)
			}
			q, err := f.n.query()
			if err != nil || len(q.Components) != 0 {
				t.Fatal("query exposed unchecked annotation", err)
			}
			if _, err = New(f.cfg, f.s, f.v); err == nil {
				t.Fatal("reopen trusted unchecked annotation")
			}
		})
	}
}

func TestAnnotationExpiredDraftAndStorageFailureStatuses(t *testing.T) {
	f := newFixture(t, nil, "a")
	annotationOperator(t, f)
	r := annotationRecord(t, f, annotationEpoch(t, f))
	n, _ := annotations.Validate(r.Component)
	n.AuthoredAtUnixMS -= 30_000
	n.ExpiresAtUnixMS = n.AuthoredAtUnixMS + 10_000
	c, err := annotations.Build(n, annotations.Config{Tenant: f.cfg.Tenant, Site: f.cfg.Site, RunID: "live", Source: r.Component.Key.Source, Frame: r.Component.Frame, Epoch: r.Component.Version.Epoch, Sequence: 1})
	if err != nil {
		t.Fatal(err)
	}
	expired, err := authority.SignAnnotation(f.credentials.Grant, f.credentials.PrivateKey, c)
	if err != nil {
		t.Fatal(err)
	}
	if status, _ := postAnnotation(t, f, expired); status != http.StatusGone {
		t.Fatal("expired draft did not fail terminally", status)
	}
	if err = f.s.Close(); err != nil {
		t.Fatal(err)
	}
	if status, _ := postAnnotation(t, f, r); status != http.StatusServiceUnavailable {
		t.Fatal("storage error not retryable", status)
	}
}

func TestAnnotationInactiveFrameRefusesLocalAndQuarantinesRelay(t *testing.T) {
	f := newFixture(t, nil, "a")
	annotationOperator(t, f)
	r := annotationRecord(t, f, annotationEpoch(t, f))
	r.Component.Frame.Revision = "next"
	r.Component.Provenance.Simulation.ScenarioVersion = "next"
	r = resignAnnotation(t, f, r)
	if status, _ := postAnnotation(t, f, r); status != http.StatusConflict {
		t.Fatal("inactive local frame accepted", status)
	}
	if _, err := f.n.apply(r, false); err != nil {
		t.Fatal("relay should retain revision-held note", err)
	}
	q, err := f.n.query()
	if err != nil || len(q.Components) != 0 {
		t.Fatal("inactive frame displayed", err)
	}
	if err = f.s.SetFrame(r.Component.Frame); err != nil {
		t.Fatal(err)
	}
	q, err = f.n.query()
	if err != nil || len(q.Components) != 1 {
		t.Fatal("activated frame not visible", err)
	}
}

func TestAnnotationRejectedReadOnlyAndRevokedOperator(t *testing.T) {
	f := newFixture(t, nil, "a")
	annotationOperator(t, f)
	r := annotationRecord(t, f, annotationEpoch(t, f))
	if status, body := postAnnotation(t, f, r); status != 200 {
		t.Fatal(status, string(body))
	}
	contributor := f.credentials
	g := contributor.Grant.Grant
	g.Scopes = []string{"read"}
	var err error
	f.credentials.Grant, err = authority.SignGrant(g, f.root)
	if err != nil {
		t.Fatal(err)
	}
	if status, _ := postAnnotation(t, f, r); status != http.StatusUnauthorized {
		t.Fatal("read-only request wrote note", status)
	}
	f.credentials = contributor
	now := time.Now()
	rev, err := authority.SignRevocations(authority.Revocations{Version: 1, Tenant: f.cfg.Tenant, Site: f.cfg.Site, Domain: "simulation", Revision: 2, IssuedAtUnixMS: now.Add(-time.Second).UnixMilli(), ExpiresAtUnixMS: now.Add(time.Minute).UnixMilli(), GrantIDs: []string{g.ID}}, f.root)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.v.UpdateRevocations(rev); err != nil {
		t.Fatal(err)
	}
	if status, _ := postAnnotation(t, f, r); status != http.StatusUnauthorized {
		t.Fatal("revoked request wrote note", status)
	}
	q, err := f.n.query()
	if err != nil || len(q.Components) != 0 {
		t.Fatal("revoked annotation visible", err)
	}
	if _, err = New(f.cfg, f.s, f.v); err != nil {
		t.Fatal("revoked retained proof prevented restart", err)
	}
}
