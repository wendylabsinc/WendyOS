package node

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/annotations"
	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/selection"
	"github.com/wendylabsinc/wendy/go/world/internal/sitepub"
	"github.com/wendylabsinc/wendy/go/world/internal/subscription"
)

func TestAnnotationWithdrawalInvalidatesSelectedQueryAndWatch(t *testing.T) {
	f := newFixture(t, nil, "annotation-selected")
	annotationOperator(t, f)
	original := annotationRecord(t, f, annotationEpoch(t, f))
	if code, body := postAnnotation(t, f, original); code != http.StatusOK {
		t.Fatal(code, string(body))
	}
	request := selection.Request{Selection: selection.Selection{Types: []string{annotations.Type, annotations.TypeV2}}}
	before := selectedOK(t, f, request)
	if len(before.Components) != 1 {
		t.Fatal("original note missing before withdrawal")
	}
	withdraw := amendRecord(t, f, original, nil, "withdraw", "", 2)
	// A separate grant lets us revoke the amendment's current authority while
	// keeping the original note and the reader independently authorized.
	grant := f.credentials.Grant.Grant
	grant.ID += "/amendment"
	signedGrant, err := authority.SignGrant(grant, f.root)
	if err != nil {
		t.Fatal(err)
	}
	withdraw, err = authority.SignAnnotation(signedGrant, f.credentials.PrivateKey, withdraw.Component)
	if err != nil {
		t.Fatal(err)
	}
	if code, body := postAnnotation(t, f, withdraw); code != http.StatusOK {
		t.Fatal(code, string(body))
	}
	if after := selectedOK(t, f, request); len(after.Components) != 0 {
		t.Fatal("selected query resurrected withdrawn original")
	}
	status, watched, raw := selectedCall(t, f, f.credentials, "/v1/watch/selected", selection.WatchRequest{
		Selection: request.Selection, Revision: before.Revision, WaitMS: 0,
	})
	if status != http.StatusOK || len(watched.Components) != 0 || watched.Revision == before.Revision || watched.TimedOut {
		t.Fatalf("withdrawal did not invalidate selected watch: %d %s", status, raw)
	}
	now := time.Now()
	rev, err := authority.SignRevocations(authority.Revocations{Version: 1, Tenant: f.cfg.Tenant, Site: f.cfg.Site, Domain: "simulation", Revision: 2, IssuedAtUnixMS: now.Add(-time.Second).UnixMilli(), ExpiresAtUnixMS: now.Add(time.Minute).UnixMilli(), GrantIDs: []string{grant.ID}}, f.root)
	if err != nil {
		t.Fatal(err)
	}
	f.n.lock()
	err = f.v.UpdateRevocations(rev)
	f.n.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if after := selectedOK(t, f, request); len(after.Components) != 0 {
		t.Fatal("revoked amendment restored an obsolete original projection")
	}
	if got := views(t, f); len(got) != 1 || got[0].State != "historical_withdrawn" || got[0].Text != "" {
		t.Fatal("historical withdrawal gained current authority", got)
	}
	if retained, err := f.s.Records(); err != nil || len(retained) != 2 {
		t.Fatal("projection filtering removed immutable repair evidence", retained, err)
	}
}

func TestAnnotationSelectiveRepairPreservesWithdrawalThroughRestart(t *testing.T) {
	a := newFixture(t, nil, "annotation-origin")
	annotationOperator(t, a)
	policy := &subscription.Policy{Version: 1, Tenant: a.cfg.Tenant, Site: a.cfg.Site,
		Selection: selection.Selection{Types: []string{annotations.Type, annotations.TypeV2, annotations.AmendmentType}}, RequiredTypes: []string{sitepub.Type}}
	b := subscribedFixture(t, a.root, "annotation-reader", 32, policy)
	original := annotationRecord(t, a, annotationEpoch(t, a))
	if code, body := postAnnotation(t, a, original); code != http.StatusOK {
		t.Fatal(code, string(body))
	}
	sync := func() error { return b.n.Sync(context.Background(), Peer{a.server.URL, a.audience}, b.credentials) }
	if err := sync(); err != nil {
		t.Fatal(err)
	}
	request := selection.Request{Selection: selection.Selection{Types: []string{annotations.Type, annotations.TypeV2}}}
	first := selectedOK(t, b, request)
	if len(first.Components) != 1 || first.ReplicaScope == nil || !first.ReplicaScope.Partial {
		t.Fatal("subscribed reader did not disclose its local partial view", first)
	}
	a.unavailable.Store(true)
	withdraw := amendRecord(t, a, original, nil, "withdraw", "", 2)
	// Publish at the origin while its peer-facing transport is partitioned.
	if _, err := a.n.apply(withdraw, true); err != nil {
		t.Fatal(err)
	}
	if err := sync(); err == nil {
		t.Fatal("partition unexpectedly repaired")
	}
	if len(selectedOK(t, b, request).Components) != 1 {
		t.Fatal("reader invented knowledge of an unseen withdrawal")
	}
	a.unavailable.Store(false)
	if err := sync(); err != nil {
		t.Fatal(err)
	}
	if len(selectedOK(t, b, request).Components) != 0 {
		t.Fatal("repaired withdrawal was absent from selected projection")
	}
	if got := views(t, b); len(got) != 1 || got[0].State != "withdrawn" {
		t.Fatal("typed view lost terminal withdrawal", got)
	}
	if retained, err := b.s.Records(); err != nil || len(retained) != 2 {
		t.Fatal("selective repair must retain both immutable signed records", retained, err)
	}
	subscribedRestart(t, b, policy)
	if err := sync(); err != nil {
		t.Fatal(err)
	}
	if len(selectedOK(t, b, request).Components) != 0 {
		t.Fatal("restart or duplicate repair resurrected withdrawn original")
	}
}
