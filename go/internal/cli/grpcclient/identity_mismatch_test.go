package grpcclient

import (
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/devicepin"
)

// TestHandshakeSinksTolerateNoDestination covers the contract every sink wired
// into BuildServerVerifyConnection shares: a nil destination means "this
// observation was not requested", not "store into nothing".
//
// These callbacks run inside VerifyConnection, on the TLS handshake path.
// verifiedIdentitySink and the OnServerIdentity callback both dereferenced
// their destination unconditionally, so a caller that passed nil — which
// newAgentTLSConfig's signature has always permitted, and which its own tests
// do — hit a nil-pointer panic mid-verification. That the verifier wraps these
// calls in a recover() is a safety net, not a licence: a recovered panic
// silently drops the observation the sink existed to make.
func TestHandshakeSinksTolerateNoDestination(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("a sink with no destination panicked during TLS verification: %v", r)
		}
	}()
	id := certs.WendyIdentity{OrgID: 7, EntityType: "asset", EntityID: "42"}
	verifiedIdentitySink(nil)(id)
	observedOrgSink(nil)(id)
	pinMismatchSink(nil)(&devicepin.PinMismatchError{Key: "urn:wendy:org:7:asset:42"})
}

// TestPinMismatchRecorded covers the sink → accessor round trip the dial ladder
// reads to tell an SPKI key rotation apart from a handshake failure. Without it
// the store's rejection reaches the user only as gRPC's wrapper text, which
// names no way out.
func TestPinMismatchRecorded(t *testing.T) {
	c := newAgentConnection(nil)
	pinMismatchSink(c.pinMismatch)(&devicepin.PinMismatchError{
		Key: "urn:wendy:org:7:asset:42", DisplayName: "thor",
		Want: "sha256:aaa", Got: "sha256:bbb",
	})

	got, ok := c.PinMismatch()
	if !ok {
		t.Fatal("want ok=true after the pin sink fired")
	}
	if got.Key != "urn:wendy:org:7:asset:42" || got.Got != "sha256:bbb" {
		t.Fatalf("recorded %+v, want the store's own key and observed fingerprint", got)
	}

	if got, ok := (&AgentConnection{}).PinMismatch(); ok || got != nil {
		t.Fatalf("a connection that never installed the sink: want (nil, false), got (%v, %v)", got, ok)
	}
}

// TestIdentityMismatchNilConnection covers the accessor's contract on a
// connection that never installed the sink (plaintext / NewFromConn).
func TestIdentityMismatchNilConnection(t *testing.T) {
	c := &AgentConnection{}
	if got, ok := c.IdentityMismatch(); ok || got != nil {
		t.Fatalf("want (nil, false), got (%v, %v)", got, ok)
	}
}

// TestIdentityMismatchRecorded covers the sink → accessor round trip that the
// dial ladder relies on to distinguish "wrong device" from "handshake failed".
func TestIdentityMismatchRecorded(t *testing.T) {
	c := newAgentConnection(nil)
	sink := identityMismatchSink(c.identityMismatch)
	sink(&certs.IdentityMismatchError{WantOrg: 7, WantAsset: "42", GotOrg: 7, GotAsset: "43"})

	got, ok := c.IdentityMismatch()
	if !ok {
		t.Fatal("want ok=true after sink fired")
	}
	if got.WantAsset != "42" || got.GotAsset != "43" {
		t.Fatalf("want 42→43, got %s→%s", got.WantAsset, got.GotAsset)
	}
}

// TestVerifyConnectionNotVerifyPeerCertificate locks in the resumption-safety
// invariant: a resumed TLS 1.3 handshake skips VerifyPeerCertificate entirely
// and calls only VerifyConnection, so the identity check must live there or it
// silently stops running after the first connect.
func TestVerifyConnectionNotVerifyPeerCertificate(t *testing.T) {
	cfg, err := newAgentTLSConfig("dev.local:50052", testCertInfo(t), nil, nil, nil, &certs.WendyIdentity{OrgID: 7, EntityType: certs.EntityAsset, EntityID: "42"}, nil, nil)
	if err != nil {
		t.Fatalf("newAgentTLSConfig: %v", err)
	}
	if cfg.VerifyConnection == nil {
		t.Error("VerifyConnection must be set")
	}
	if cfg.VerifyPeerCertificate != nil {
		t.Error("VerifyPeerCertificate must stay nil: a resumed handshake skips it")
	}
}
