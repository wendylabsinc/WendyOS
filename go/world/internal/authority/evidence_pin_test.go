package authority

import (
	"errors"
	"testing"
	"time"
)

func pinGrant(t *testing.T, f *fixture, audience string) SignedGrant {
	t.Helper()
	g := f.grant.Grant
	g.Kind, g.Scopes, g.PublishAudience = "operator", []string{"evidence-pin"}, audience
	signed, err := SignGrant(g, f.root)
	if err != nil {
		t.Fatal(err)
	}
	return signed
}

func TestEvidencePinRequiresExplicitNonAgentScopeAndDestination(t *testing.T) {
	f := newFixture(t, nil)
	body := []byte(`{"id":"investigation-1","sha256":["digest"],"retain_until_unix_ms":123}`)
	for _, kind := range []string{"agent"} {
		g := f.grant.Grant
		g.Kind, g.Scopes = kind, []string{"evidence-pin"}
		if _, err := SignGrant(g, f.root); !errors.Is(err, ErrDenied) {
			t.Fatalf("%s received investigation pin grant: %v", kind, err)
		}
	}
	for _, kind := range []string{"operator", "app", "device"} {
		g := f.grant.Grant
		g.Kind, g.Scopes = kind, []string{"evidence-pin"}
		var err error
		f.grant, err = SignGrant(g, f.root)
		if err != nil {
			t.Fatalf("explicit %s pin grant refused: %v", kind, err)
		}
		if _, err = f.verifier.Verify(f.request(t, "evidence-pin", "pin-kind-nonce-123456-"+kind, body), "evidence-pin", body); err != nil {
			t.Fatal(kind, err)
		}
	}
	g := f.grant.Grant
	g.Kind, g.Scopes, g.PublishAudience = "operator", []string{"evidence-pin"}, ""
	if _, err := SignGrant(g, f.root); !errors.Is(err, ErrDenied) {
		t.Fatal("pin grant omitted its capture node", err)
	}
	for _, scope := range []string{"read", "evidence-read", "evidence-write"} {
		g.Kind, g.Scopes, g.PublishAudience = "operator", []string{scope}, "node-1"
		f.grant, _ = SignGrant(g, f.root)
		request := f.request(t, "evidence-pin", "pin-scope-nonce-123456", body)
		if _, err := f.verifier.Verify(request, "evidence-pin", body); !errors.Is(err, ErrDenied) {
			t.Fatalf("%s implicitly authorized pin: %v", scope, err)
		}
	}
	f.grant = pinGrant(t, f, "node-2")
	if _, err := f.verifier.Verify(f.request(t, "evidence-pin", "pin-wrong-node-123456", body), "evidence-pin", body); !errors.Is(err, ErrDenied) {
		t.Fatal("pin escaped its capture node", err)
	}
	f.grant = pinGrant(t, f, "node-1")
	request := f.request(t, "evidence-pin", "pin-valid-nonce-123456", body)
	if _, err := f.verifier.Verify(request, "evidence-pin", []byte(`{"id":"changed"}`)); !errors.Is(err, ErrDenied) {
		t.Fatal("unsigned pin mutation accepted", err)
	}
	identity, err := f.verifier.Verify(request, "evidence-pin", body)
	if err != nil || identity.Kind != "operator" || identity.PublishAudience != "node-1" {
		t.Fatal("valid pin authority refused", identity, err)
	}
	if _, err = f.verifier.Verify(request, "evidence-pin", body); !errors.Is(err, ErrReplay) {
		t.Fatal("pin request replay accepted", err)
	}
	// Pin authority is not media-read or media-write authority.
	if _, err = f.verifier.Verify(f.request(t, "evidence-read", "pin-read-nonce-123456", nil), "evidence-read", nil); !errors.Is(err, ErrDenied) {
		t.Fatal("pin grant disclosed media", err)
	}
}

func TestEvidencePinDoesNotInheritExpiredStopException(t *testing.T) {
	for _, denied := range []string{"expired", "future", "revoked"} {
		t.Run(denied, func(t *testing.T) {
			f := newFixture(t, nil)
			f.grant = pinGrant(t, f, "node-1")
			g := f.grant.Grant
			switch denied {
			case "expired":
				g.NotBeforeUnixMS, g.NotAfterUnixMS = f.now.Add(-time.Hour).UnixMilli(), f.now.Add(-time.Second).UnixMilli()
			case "future":
				g.NotBeforeUnixMS = f.now.Add(time.Second).UnixMilli()
			case "revoked":
				r := f.rev.Revocations
				r.Revision, r.Principals = 2, []string{g.Principal}
				signed, _ := SignRevocations(r, f.root)
				if err := f.verifier.UpdateRevocations(signed); err != nil {
					t.Fatal(err)
				}
			}
			f.grant, _ = SignGrant(g, f.root)
			request := f.request(t, "evidence-pin", "pin-invalid-time-123456", []byte(`{"id":"pin"}`))
			if _, err := f.verifier.Verify(request, "evidence-pin", []byte(`{"id":"pin"}`)); !errors.Is(err, ErrDenied) {
				t.Fatal("invalid pin authority accepted", err)
			}
		})
	}
}
