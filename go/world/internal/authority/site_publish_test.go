package authority

import (
	"strings"
	"testing"
	"time"
)

func TestSitePublicationRequiresOperatorAndOwnedPublishGrant(t *testing.T) {
	f := newFixture(t, nil)
	valid := f.grant.Grant
	valid.Kind = "operator"
	valid.Scopes = []string{"publish", "site-publish", "read"}
	for _, tt := range []struct {
		name string
		edit func(*Grant)
	}{
		{"app", func(g *Grant) { g.Kind = "app" }},
		{"device", func(g *Grant) { g.Kind = "device" }},
		{"agent", func(g *Grant) { g.Kind = "agent" }},
		{"proposal agent", func(g *Grant) { g.Kind = "agent"; g.Scopes = []string{"propose", "publish", "site-publish"} }},
		{"no publish", func(g *Grant) { g.Scopes = []string{"read", "site-publish"} }},
		{"missing owner", func(g *Grant) { g.PublishAudience = "" }},
		{"long owner", func(g *Grant) { g.PublishAudience = strings.Repeat("a", 257) }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			g := valid
			tt.edit(&g)
			if _, err := SignGrant(g, f.root); err == nil {
				t.Fatal("issuer accepted invalid site publisher")
			}
			// A compromised or older issuer can sign bytes without running its
			// constructor. Receiving verifiers must enforce the rule too.
			signature, err := sign(f.root, "grant", g)
			if err != nil {
				t.Fatal(err)
			}
			signed := SignedGrant{Grant: g, Signature: signature}
			if _, err := f.verifier.VerifyGrant(signed, "site-publish"); err == nil {
				t.Fatal("verifier accepted invalid root-signed site publisher")
			}
			if _, err := f.verifier.VerifyGrant(signed, "publish"); err == nil {
				t.Fatal("invalid site publisher bypassed validation through publish")
			}
		})
	}
	signed, err := SignGrant(valid, f.root)
	if err != nil {
		t.Fatal(err)
	}
	id, err := f.verifier.VerifyGrant(signed, "site-publish")
	if err != nil || id.Kind != "operator" || !id.Allows("publish") || !id.Allows("site-publish") {
		t.Fatalf("valid site publisher rejected: %+v %v", id, err)
	}
}

func TestSitePublishRequestKeepsOwnerScopeAndRevocation(t *testing.T) {
	f := newFixture(t, nil)
	g := f.grant.Grant
	g.Kind = "operator"
	g.Scopes = []string{"publish", "site-publish"}
	for _, owner := range []string{"node-2", "node-1"} {
		g.PublishAudience = owner
		signed, err := SignGrant(g, f.root)
		if err != nil {
			t.Fatal(err)
		}
		r, err := SignRequest(signed, f.key, "node-1", "site-publish", nil, "site_publish_nonce_"+owner, f.now, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		_, err = f.verifier.Verify(r, "site-publish", nil)
		if (err == nil) != (owner == "node-1") {
			t.Fatalf("owner %s: %v", owner, err)
		}
	}
	g.PublishAudience = "node-1"
	signed, err := SignGrant(g, f.root)
	if err != nil {
		t.Fatal(err)
	}
	rev := f.rev.Revocations
	rev.Revision++
	rev.GrantIDs = []string{g.ID}
	update, err := SignRevocations(rev, f.root)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.verifier.UpdateRevocations(update); err != nil {
		t.Fatal(err)
	}
	if _, err = f.verifier.VerifyGrant(signed, "site-publish"); err == nil {
		t.Fatal("revoked publisher accepted")
	}
}

func TestOrdinaryPublishAndMissionGrantsRemainValid(t *testing.T) {
	f := newFixture(t, nil)
	if _, err := f.verifier.VerifyGrant(f.grant, "publish"); err != nil {
		t.Fatal(err)
	}
	g := f.grant.Grant
	g.Kind = "operator"
	g.Scopes = []string{"mission", "stop", "read"}
	g.PublishAudience = ""
	signed, err := SignGrant(g, f.root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.verifier.VerifyGrant(signed, "mission"); err != nil {
		t.Fatal(err)
	}
	if _, err = f.verifier.VerifyGrant(signed, "site-publish"); err == nil {
		t.Fatal("ordinary operator gained site publication")
	}
}
