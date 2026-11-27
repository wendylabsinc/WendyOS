package authority

import (
	"errors"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/annotations"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

func annotationFixture(t *testing.T) (*fixture, SignedRecord) {
	t.Helper()
	f := newFixture(t, nil)
	g := f.grant.Grant
	g.Kind = "operator"
	g.Scopes = []string{"read", "annotation-write"}
	var err error
	f.grant, err = SignGrant(g, f.root)
	if err != nil {
		t.Fatal(err)
	}
	c, err := annotations.Build(annotations.Note{ID: "12345678-1234-4123-8123-123456789abc", AuthorPrincipal: g.Principal, Text: "Check north door", AuthoredAtUnixMS: f.now.UnixMilli(), ExpiresAtUnixMS: f.now.Add(time.Hour).UnixMilli()}, annotations.Config{Tenant: g.Tenant, Site: g.Site, RunID: "run", Source: g.Source, Frame: world.FrameRef{ID: "floor", Revision: "1"}, Epoch: 1, Sequence: 1})
	if err != nil {
		t.Fatal(err)
	}
	r, err := SignAnnotation(f.grant, f.key, c)
	if err != nil {
		t.Fatal(err)
	}
	return f, r
}

func TestAnnotationGrantConstraintsAndReadOnlyDenial(t *testing.T) {
	f, r := annotationFixture(t)
	if _, err := f.verifier.VerifyRecord(r); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*Grant){
		"app":      func(g *Grant) { g.Kind = "app" },
		"physical": func(g *Grant) { g.Domain = "physical" },
		"unowned":  func(g *Grant) { g.PublishAudience = "" },
	} {
		t.Run(name, func(t *testing.T) {
			g := f.grant.Grant
			change(&g)
			if _, err := SignGrant(g, f.root); err == nil {
				t.Fatal("invalid annotation grant issued")
			}
			// Test validation even when the signer bypasses the issuance helper.
			sig, err := sign(f.root, "grant", g)
			if err != nil {
				t.Fatal(err)
			}
			bad, err := SignRecord(SignedGrant{Grant: g, Signature: sig}, f.key, r.Component, f.now)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = f.verifier.VerifyRecord(bad); err == nil {
				t.Fatal("invalid root-signed grant accepted")
			}
		})
	}
	g := f.grant.Grant
	g.Scopes = []string{"read"}
	reader, err := SignGrant(g, f.root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = SignAnnotation(reader, f.key, r.Component); err == nil {
		t.Fatal("reader prepared annotation")
	}
	bad, err := SignRecord(reader, f.key, r.Component, f.now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.verifier.VerifyRecord(bad); !errors.Is(err, ErrDenied) {
		t.Fatal("read-only origin admitted", err)
	}
}

func TestAnnotationProofCannotRefreshDraftOrEraseHistory(t *testing.T) {
	f, r := annotationFixture(t)
	refreshed, err := SignRecord(f.grant, f.key, r.Component, f.now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	f.now = f.now.Add(time.Second)
	if _, err = f.verifier.VerifyRecord(refreshed); err == nil {
		t.Fatal("new signature refreshed old draft")
	}
	if err = f.verifier.VerifyAnnotationHistory(r); err != nil {
		t.Fatal(err)
	}
	f.now = f.now.Add(25 * time.Hour)
	if _, err = f.verifier.VerifyRecord(r); err == nil {
		t.Fatal("expired proof admitted live")
	}
	if err = f.verifier.VerifyAnnotationHistory(r); err != nil {
		t.Fatal("expired retained highwater rejected", err)
	}
}

func TestAnnotationRevocationDeniesVisibilityNotRetainedHistory(t *testing.T) {
	f, r := annotationFixture(t)
	rev := f.rev.Revocations
	rev.Revision++
	rev.GrantIDs = []string{f.grant.Grant.ID}
	signed, err := SignRevocations(rev, f.root)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.verifier.UpdateRevocations(signed); err != nil {
		t.Fatal(err)
	}
	if _, err = f.verifier.VerifyRecord(r); !errors.Is(err, ErrDenied) {
		t.Fatal("revoked note remained authorized", err)
	}
	if err = f.verifier.VerifyAnnotationHistory(r); err != nil {
		t.Fatal("revoked highwater should remain valid history", err)
	}
	request := f.request(t, "annotation-write", "annotation_revoked_nonce", []byte(`{}`))
	if _, err = f.verifier.Verify(request, "annotation-write", []byte(`{}`)); !errors.Is(err, ErrDenied) {
		t.Fatal("revoked writer admitted", err)
	}
}
