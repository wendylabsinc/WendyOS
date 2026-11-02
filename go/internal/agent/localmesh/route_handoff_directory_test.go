package localmesh

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/x509"
	"errors"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"math/big"
	"net/url"
	"testing"
	"time"
)

func TestRouteHandoffKeepsOneOriginalIndependentProof(t *testing.T) {
	d, chain, key, device, now := directoryFixture(t)
	old := routeManifest(device, now)
	oldWire := signRouteTestManifest(t, old, chain, key, now)
	if _, err := d.Accept(oldWire, now); err != nil {
		t.Fatal(err)
	}
	owner := oldWire.ownerKey()
	original := d.entries[owner].activeUntil
	next := cloneManifest(old)
	next.Revision++
	next.Route.Seqno++
	next.Issued = now.Add(10 * time.Second).UnixMilli()
	next.Expires = now.Add(70 * time.Second).UnixMilli()
	nextWire := signRouteTestManifest(t, next, chain, key, now.Add(10*time.Second))
	if _, err := d.Accept(nextWire, now.Add(10*time.Second)); err != nil {
		t.Fatal(err)
	}
	got := d.RouteAuthorizations(now.Add(10 * time.Second))
	if len(got) != 2 || !got[*old.Route] || !got[*next.Route] {
		t.Fatalf("old/new independent proofs missing:%v", got)
	}
	if len(d.handoffs) != 1 || d.handoffs[owner].origin != *old.Route || !d.handoffs[owner].deadline.Equal(original) {
		t.Fatalf("original compact proof changed:%+v", d.handoffs)
	}
	records := d.Records(now.Add(10 * time.Second))
	if len(records) != 1 || !bytes.Equal(records[0].Body, nextWire.Body) {
		t.Fatal("retained proof leaked as old gossip payload")
	}
	if _, ok := d.CurrentRecordChain(oldWire, now.Add(10*time.Second)); ok {
		t.Fatal("old proof became exportable")
	}
	snapshots, proofs := d.SnapshotWithRoutes(now.Add(10 * time.Second))
	if len(snapshots) != 1 || snapshots[0].Revision != next.Revision || !proofs[*old.Route].Retained || !proofs[*old.Route].Deadline.Equal(original) {
		t.Fatal("snapshot lost exact old authority or exported old presence")
	}
	third := cloneManifest(next)
	third.Revision++
	third.Route.Seqno++
	third.Issued = now.Add(20 * time.Second).UnixMilli()
	third.Expires = now.Add(80 * time.Second).UnixMilli()
	if _, err := d.Accept(signRouteTestManifest(t, third, chain, key, now.Add(20*time.Second)), now.Add(20*time.Second)); err != nil {
		t.Fatal(err)
	}
	got = d.RouteAuthorizations(now.Add(20 * time.Second))
	if len(got) != 2 || !got[*old.Route] || !got[*third.Route] || got[*next.Route] || !d.handoffs[owner].deadline.Equal(original) {
		t.Fatalf("repeated replacement chained/renewed authority:%v", got)
	}
	if d.CompleteRouteHandoff(*next.Route, now.Add(20*time.Second)) {
		t.Fatal("wrong tuple completed handoff")
	}
	if !d.CompleteRouteHandoff(*third.Route, now.Add(20*time.Second)) {
		t.Fatal("matching current tuple did not retire old")
	}
	if got := d.RouteAuthorizations(now.Add(20 * time.Second)); len(got) != 1 || got[*old.Route] || !got[*third.Route] {
		t.Fatalf("old authority survived retirement:%v", got)
	}
	if _, err := d.Accept(oldWire, now.Add(20*time.Second)); !errors.Is(err, ErrStaleManifest) {
		t.Fatalf("old wire bypassed latest revision floor:%v", err)
	}
}

func TestRouteHandoffOldDeadlineAndCurrentExpiryStayIndependent(t *testing.T) {
	for _, which := range []string{"old", "current"} {
		t.Run(which, func(t *testing.T) {
			d, chain, key, device, now := directoryFixture(t)
			old := routeManifest(device, now)
			if which == "old" {
				old.Expires = now.Add(2 * time.Second).UnixMilli()
			}
			if _, err := d.Accept(signRouteTestManifest(t, old, chain, key, now), now); err != nil {
				t.Fatal(err)
			}
			next := cloneManifest(old)
			next.Revision++
			next.Route.Seqno++
			next.Issued = now.Add(time.Second).UnixMilli()
			next.Expires = now.Add(61 * time.Second).UnixMilli()
			if which == "current" {
				next.Expires = now.Add(3 * time.Second).UnixMilli()
			}
			if _, err := d.Accept(signRouteTestManifest(t, next, chain, key, now.Add(time.Second)), now.Add(time.Second)); err != nil {
				t.Fatal(err)
			}
			if !d.RouteAuthorizations(now.Add(time.Second))[*old.Route] {
				t.Fatal("handoff never began")
			}
			at := now.Add(2100 * time.Millisecond)
			if which == "current" {
				at = now.Add(3100 * time.Millisecond)
			}
			got := d.RouteAuthorizations(at)
			if got[*old.Route] || len(d.handoffs) != 0 {
				t.Fatalf("expired %s authority survived:%v", which, got)
			}
			if which == "old" && !got[*next.Route] {
				t.Fatal("old proof expiry erased independent current proof")
			}
			if which == "current" && len(got) != 0 {
				t.Fatal("current expiry retained earlier proof")
			}
			if d.RouteAuthorizations(now.Add(time.Second))[*old.Route] {
				t.Fatal("clock rollback resurrected retired proof")
			}
		})
	}
}

func TestRouteHandoffNonRouteChangesRevokeImmediately(t *testing.T) {
	cases := map[string]func(*Manifest){
		"withdraw":          func(m *Manifest) { m.Withdraw = true },
		"router":            func(m *Manifest) { m.Route.RouterID++ },
		"capability":        func(m *Manifest) { m.Caps = []string{CapabilitySensors} },
		"endpoint":          func(m *Manifest) { m.AppEndpoints = []AppEndpoint{{Transport: AppTLS, Port: AppSessionTLSPort}} },
		"name":              func(m *Manifest) { m.Name = "changed" },
		"backward sequence": func(m *Manifest) { m.Route.Seqno -= 3 },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			d, chain, key, device, now := directoryFixture(t)
			old := routeManifest(device, now)
			if _, err := d.Accept(signRouteTestManifest(t, old, chain, key, now), now); err != nil {
				t.Fatal(err)
			}
			next := cloneManifest(old)
			next.Revision++
			next.Route.Seqno++
			if _, err := d.Accept(signRouteTestManifest(t, next, chain, key, now), now); err != nil {
				t.Fatal(err)
			}
			if !d.RouteAuthorizations(now)[*old.Route] {
				t.Fatal("fixture did not retain original proof")
			}
			last := cloneManifest(next)
			last.Revision++
			last.Route.Seqno++
			change(&last)
			if _, err := d.Accept(signRouteTestManifest(t, last, chain, key, now), now); err != nil {
				t.Fatal(err)
			}
			if got := d.RouteAuthorizations(now); got[*old.Route] || got[*next.Route] || len(d.handoffs) != 0 {
				t.Fatalf("%s retained superseded authority:%v", name, got)
			}
		})
	}
}

func TestRouteHandoffSigningKeyChangeCannotBorrowReplacementSignature(t *testing.T) {
	d, chain, key, device, now := directoryFixture(t)
	old := routeManifest(device, now)
	if _, err := d.Accept(signRouteTestManifest(t, old, chain, key, now), now); err != nil {
		t.Fatal(err)
	}
	next := cloneManifest(old)
	next.Revision++
	next.Route.Seqno++
	if _, err := d.Accept(signRouteTestManifest(t, next, chain, key, now), now); err != nil {
		t.Fatal(err)
	}
	newChain, newKey := addRouteTestIdentity(t, d, device, now)
	last := cloneManifest(next)
	last.Revision++
	last.Route.Seqno++
	if _, err := d.Accept(signRouteTestManifest(t, last, newChain, newKey, now), now); err != nil {
		t.Fatal(err)
	}
	got := d.RouteAuthorizations(now)
	if len(got) != 1 || !got[*last.Route] || got[*old.Route] || got[*next.Route] || len(d.handoffs) != 0 {
		t.Fatalf("new signature/key lent old proof authority:%v", got)
	}
}

func TestRouteHandoffRetainsOriginalSelectedCertificatePathCap(t *testing.T) {
	d, chain, key, device, now := directoryFixture(t)
	selectedEnd := now.Add(30 * time.Second)
	d.cache.verify = func(_ [][]byte, _ time.Time) (Identity, error) {
		return Identity{Device: device, NotAfter: selectedEnd}, nil
	}
	old := routeManifest(device, now)
	old.Issued = now.Add(-50 * time.Second).UnixMilli()
	old.Expires = now.Add(10 * time.Second).UnixMilli() // Signed60s; original selected path only30s.
	if _, err := d.Accept(signRouteTestManifest(t, old, chain, key, now), now); err != nil {
		t.Fatal(err)
	}
	selectedEnd = now.Add(time.Hour) // Same key/path repair must not relend a longer old lifetime.
	next := cloneManifest(old)
	next.Revision++
	next.Route.Seqno++
	next.Issued = now.Add(time.Second).UnixMilli()
	next.Expires = now.Add(61 * time.Second).UnixMilli()
	if _, err := d.Accept(signRouteTestManifest(t, next, chain, key, now.Add(time.Second)), now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	_, proofs := d.SnapshotWithRoutes(now.Add(time.Second))
	if !proofs[*old.Route].Deadline.Equal(now.Add(30 * time.Second)) {
		t.Fatalf("old path cap extended:%s", proofs[*old.Route].Deadline)
	}
	if d.RouteAuthorizations(now.Add(30100 * time.Millisecond))[*old.Route] {
		t.Fatal("replacement path extended old proof beyond original selected path")
	}
}

func TestRouteHandoffCertificateFloorProjectsAndOnlyShortens(t *testing.T) {
	d, chain, key, device, now := directoryFixture(t)
	selectedEnd := now.Add(time.Hour)
	d.cache.verify = func(_ [][]byte, _ time.Time) (Identity, error) {
		return Identity{Device: device, NotAfter: selectedEnd}, nil
	}
	certificateBase := selectedEnd.Add(-30 * time.Second)
	d.certificateNow = func() time.Time { return certificateBase }
	old := routeManifest(device, now)
	if _, err := d.Accept(signRouteTestManifest(t, old, chain, key, now), now); err != nil {
		t.Fatal(err)
	}
	next := cloneManifest(old)
	next.Revision++
	next.Route.Seqno++
	if _, err := d.Accept(signRouteTestManifest(t, next, chain, key, now), now); err != nil {
		t.Fatal(err)
	}
	_, proofs := d.SnapshotWithRoutes(now)
	initial := proofs[*old.Route].Deadline
	if !initial.Equal(now.Add(30 * time.Second)) {
		t.Fatalf("certificate floor became scheduling clock:%s", initial)
	}
	certificateBase = selectedEnd.Add(-10 * time.Second)
	_, proofs = d.SnapshotWithRoutes(now.Add(5 * time.Second))
	shortened := proofs[*old.Route].Deadline
	if !shortened.Equal(now.Add(15 * time.Second)) {
		t.Fatalf("forward certificate correction did not shorten:%s", shortened)
	}
	certificateBase = selectedEnd.Add(-time.Minute)
	_, proofs = d.SnapshotWithRoutes(now.Add(6 * time.Second))
	if proofs[*old.Route].Deadline.After(shortened) {
		t.Fatal("backward certificate clock extended prior cap")
	}
	if d.RouteAuthorizations(now.Add(15100 * time.Millisecond))[*old.Route] {
		t.Fatal("shortened deadline resurrected")
	}
}

func TestRouteHandoffForwardCertificateCorrectionUsesOriginalPathAfterRenewal(t *testing.T) {
	d, chain, key, device, now := directoryFixture(t)
	selectedEnd := now.Add(30 * time.Second)
	d.cache.verify = func(_ [][]byte, _ time.Time) (Identity, error) {
		return Identity{Device: device, NotAfter: selectedEnd}, nil
	}
	base := now
	d.certificateNow = func() time.Time { return base }
	old := routeManifest(device, now)
	old.Issued = now.Add(-50 * time.Second).UnixMilli()
	old.Expires = now.Add(10 * time.Second).UnixMilli()
	if _, err := d.Accept(signRouteTestManifest(t, old, chain, key, now), now); err != nil {
		t.Fatal(err)
	}
	selectedEnd = now.Add(time.Hour)
	next := cloneManifest(old)
	next.Revision++
	next.Route.Seqno++
	next.Issued = now.Add(time.Second).UnixMilli()
	next.Expires = now.Add(61 * time.Second).UnixMilli()
	base = now.Add(time.Second)
	if _, err := d.Accept(signRouteTestManifest(t, next, chain, key, now.Add(time.Second)), now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if !d.RouteAuthorizations(now.Add(time.Second))[*old.Route] {
		t.Fatal("old proof not independently retained")
	}
	base = now.Add(31 * time.Second) // Actual receiver elapsed only2s; original selected NA is now past.
	got := d.RouteAuthorizations(now.Add(2 * time.Second))
	if got[*old.Route] || !got[*next.Route] {
		t.Fatalf("renewed path masked original expiry after forward correction:%v", got)
	}
}

func TestRouteHandoffOldLeafGraceCannotBorrowRenewedLeafAfterCertainty(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	uncertain := true
	policy := certs.CertificateTimePolicy{Now: func() time.Time { return now }, Floor: func() time.Time { return now }, Uncertain: func() bool { return uncertain }}
	principal := testPrincipal(testTenantUUID, testDeviceAID)
	certPEM, keyPEM := issuePolicyLeaf(t, principal, now.Add(time.Hour), now.Add(2*time.Hour))
	credentials, err := NewCredentialsWithTimePolicy(certPEM, testPKI().chainPEM, keyPEM, policy)
	if err != nil {
		t.Fatal(err)
	}
	cache, err := OpenIdentityCache("", DefaultCacheLimits(), credentials.Verify, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = cache.Put(credentials.Certificate.Certificate, now); err != nil {
		t.Fatal(err)
	}
	d, err := NewDirectory(credentials.Device.Tenant(), 10, cache)
	if err != nil {
		t.Fatal(err)
	}
	d.certificateNow = func() time.Time { return policy.BaseTime(policy.WallTime()) }
	d.certificateLeafTime = policy.LeafTimeAtBase
	old := routeManifest(credentials.Device, now.Add(time.Hour))
	sign := func(m Manifest, chain [][]byte) SignedManifest {
		t.Helper()
		w, err := SignManifest(m, chain, credentials.Signer, now)
		if err != nil {
			t.Fatal(err)
		}
		return w
	}
	if _, err = d.Accept(sign(old, credentials.Certificate.Certificate), now); err != nil {
		t.Fatal(err)
	}
	next := cloneManifest(old)
	next.Revision++
	next.Route.Seqno++
	if _, err = d.Accept(sign(next, credentials.Certificate.Certificate), now); err != nil {
		t.Fatal(err)
	}
	if !d.RouteAuthorizations(now)[*old.Route] {
		t.Fatal("uncertain original leaf proof not retained")
	}
	// Renew to the SAME signing key with a currently valid leaf. Current proof
	// remains trusted when certainty changes; only the old grace must disappear.
	key := credentials.Signer.(*ecdsa.PrivateKey)
	uri, _ := url.Parse(principal)
	leaf := &x509.Certificate{SerialNumber: big.NewInt(777), NotBefore: now.Add(-time.Hour), NotAfter: now.Add(2 * time.Hour), URIs: []*url.URL{uri}, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth}}
	pki := testPKI()
	der, err := x509.CreateCertificate(rand.Reader, leaf, pki.certificate, key.Public(), pki.key)
	if err != nil {
		t.Fatal(err)
	}
	renewed := [][]byte{der}
	if _, err = cache.Put(renewed, now); err != nil {
		t.Fatal(err)
	}
	next.Revision++
	if _, err = d.Accept(sign(next, renewed), now); err != nil {
		t.Fatal(err)
	}
	if !d.RouteAuthorizations(now)[*old.Route] {
		t.Fatal("same-key renewal unexpectedly discarded still-valid old proof")
	}
	uncertain = false
	got := d.RouteAuthorizations(now)
	if got[*old.Route] || !got[*next.Route] {
		t.Fatalf("current leaf lent old leaf boot grace after certainty:%v", got)
	}
}

func TestRouteHandoffObservedPathRestrictionCannotBeForgottenBeforeSupersession(t *testing.T) {
	d, chain, key, device, now := directoryFixture(t)
	selectedEnd := now.Add(time.Hour)
	d.cache.verify = func(_ [][]byte, _ time.Time) (Identity, error) {
		return Identity{Device: device, NotAfter: selectedEnd}, nil
	}
	old := routeManifest(device, now)
	old.Issued = now.Add(-50 * time.Second).UnixMilli()
	old.Expires = now.Add(10 * time.Second).UnixMilli()
	if _, err := d.Accept(signRouteTestManifest(t, old, chain, key, now), now); err != nil {
		t.Fatal(err)
	}
	selectedEnd = now.Add(20 * time.Second)
	if len(d.Snapshot(now.Add(time.Second))) != 1 {
		t.Fatal("still-valid shorter selected path rejected")
	}
	selectedEnd = now.Add(time.Hour)
	next := cloneManifest(old)
	next.Revision++
	next.Route.Seqno++
	next.Issued = now.Add(2 * time.Second).UnixMilli()
	next.Expires = now.Add(62 * time.Second).UnixMilli()
	if _, err := d.Accept(signRouteTestManifest(t, next, chain, key, now.Add(2*time.Second)), now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	_, proofs := d.SnapshotWithRoutes(now.Add(2 * time.Second))
	if !proofs[*old.Route].Deadline.Equal(now.Add(20 * time.Second)) {
		t.Fatalf("observed shorter path was forgotten:%s", proofs[*old.Route].Deadline)
	}
}

func TestRouteHandoffReplacedSignatureUsesActualSignerNotBefore(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	uncertain := true
	policy := certs.CertificateTimePolicy{Now: func() time.Time { return now }, Floor: func() time.Time { return now }, Uncertain: func() bool { return uncertain }}
	principal := testPrincipal(testTenantUUID, testDeviceAID)
	oldCert, oldKey := issuePolicyLeaf(t, principal, now.Add(time.Hour), now.Add(2*time.Hour))
	a, err := NewCredentialsWithTimePolicy(oldCert, testPKI().chainPEM, oldKey, policy)
	if err != nil {
		t.Fatal(err)
	}
	cache, err := OpenIdentityCache("", DefaultCacheLimits(), a.Verify, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = cache.Put(a.Certificate.Certificate, now); err != nil {
		t.Fatal(err)
	}
	d, err := NewDirectory(a.Device.Tenant(), 10, cache)
	if err != nil {
		t.Fatal(err)
	}
	d.certificateNow = func() time.Time { return policy.BaseTime(policy.WallTime()) }
	d.certificateLeafTime = policy.LeafTimeAtBase
	old := routeManifest(a.Device, now.Add(time.Hour))
	oldOrigin := *old.Route
	wire, err := SignManifest(old, a.Certificate.Certificate, a.Signer, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = d.Accept(wire, now); err != nil {
		t.Fatal(err)
	}
	initialUntil := d.entries[a.Device.Key()].activeUntil
	newCert, newKey := issuePolicyLeaf(t, principal, now.Add(-time.Hour), now.Add(2*time.Hour))
	b, err := NewCredentialsWithTimePolicy(newCert, testPKI().chainPEM, newKey, policy)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = cache.Put(b.Certificate.Certificate, now); err != nil {
		t.Fatal(err)
	}
	replacement, err := SignManifest(old, b.Certificate.Certificate, b.Signer, now)
	if err != nil {
		t.Fatal(err)
	}
	if changed, err := d.Accept(replacement, now); err != nil || !changed {
		t.Fatal(changed, err)
	}
	if d.entries[b.Device.Key()].activeUntil != initialUntil {
		t.Fatal("same-body replacement extended lease")
	}
	uncertain = false
	if !d.RouteAuthorizations(now)[oldOrigin] {
		t.Fatal("fresh currently-valid replacement signature was not independently trusted")
	}
	next := cloneManifest(old)
	next.Revision++
	next.Route.Seqno++
	nextWire, err := SignManifest(next, b.Certificate.Certificate, b.Signer, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = d.Accept(nextWire, now); err != nil {
		t.Fatal(err)
	}
	auth := d.RouteAuthorizations(now)
	if !auth[oldOrigin] || !auth[*next.Route] {
		t.Fatalf("pure-seq handoff borrowed replaced old leaf metadata instead of actual retained signer: %v", auth)
	}
}
