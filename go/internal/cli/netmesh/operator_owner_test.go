package netmesh

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"math/big"
	"net/netip"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/wendylabsinc/WendyOS/go/babel"
	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshoperator"
)

// Normal simultaneous installations of one real operator credential have
// independent origin revisions, without duplicating certificate cache entries.
func TestOperatorInstallationsKeepIndependentManifestLeases(t *testing.T) {
	var authority fixtureAuthority
	c := fixtureCredentials(t, &authority)
	operator := c[1]
	now := time.Now()
	cache, err := localmesh.OpenIdentityCache("", localmesh.DefaultCacheLimits(), c[0].Verify, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = cache.Put(operator.Certificate.Certificate, now); err != nil {
		t.Fatal(err)
	}
	directory, err := localmesh.NewDirectory(operator.Device.Tenant(), 64, cache)
	if err != nil {
		t.Fatal(err)
	}
	var receipts []localmesh.DirectoryReceipt
	if err = directory.SetPersistence(nil, func(r []localmesh.DirectoryReceipt) error { receipts = r; return nil }, now); err != nil {
		t.Fatal(err)
	}
	second, err := operator.Device.WithInstance(uuid.MustParse("bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"))
	if err != nil {
		t.Fatal(err)
	}
	secondKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	secondLeaf, err := x509.ParseCertificate(operator.Certificate.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	secondLeaf.SerialNumber = big.NewInt(900)
	secondLeaf.NotAfter = secondLeaf.NotAfter.Add(-time.Minute)
	secondDER, err := x509.CreateCertificate(rand.Reader, secondLeaf, authority.certificate, secondKey.Public(), authority.key)
	if err != nil {
		t.Fatal(err)
	}
	secondChain := [][]byte{secondDER}
	if _, err = cache.Put(secondChain, now); err != nil {
		t.Fatal(err)
	}
	for i, peer := range []struct {
		instance uuid.UUID
		revision uint64
	}{{operator.Device.UUID(), 1}, {second.UUID(), 1}, {operator.Device.UUID(), 2}} {
		owner, err := operator.Device.WithInstance(peer.instance)
		if err != nil {
			t.Fatal(err)
		}
		parsed, err := meshoperator.ParsePrincipal(owner.Principal())
		if err != nil {
			t.Fatal(err)
		}
		ownerAddress := meshoperator.Endpoint{Operator: parsed, Instance: meshoperator.InstanceID(owner.UUID())}
		if owner.MeshAddress() != ownerAddress.Address() {
			t.Fatal("installation address derivation changed")
		}
		m := localmesh.Manifest{Version: 4, Owner: &localmesh.OperatorOwner{Principal: owner.Principal(), Instance: owner.UUID()}, Revision: peer.revision, Issued: now.UnixMilli(), Expires: now.Add(15 * time.Minute).UnixMilli(), Route: &localmesh.RouteOrigin{Prefix: netip.PrefixFrom(owner.MeshAddress(), 128), RouterID: babel.RouterID(100 + i), Seqno: 1}}
		chain, signer := operator.Certificate.Certificate, operator.Signer
		if owner.UUID() == second.UUID() {
			chain, signer = secondChain, secondKey
		}
		wire, err := localmesh.SignManifest(m, chain, signer, now)
		if err != nil {
			t.Fatal(err)
		}
		if accepted, err := directory.Accept(wire, now); err != nil || !accepted {
			t.Fatalf("normal instance update: %v %v", accepted, err)
		}
	}
	if got := len(directory.Snapshot(now)); got != 2 {
		t.Fatalf("got %d installations", got)
	}
	if len(receipts) != 2 {
		t.Fatalf("got %d replay owners", len(receipts))
	}
	restored, err := localmesh.NewDirectory(operator.Device.Tenant(), 64, cache)
	if err != nil {
		t.Fatal(err)
	}
	if err = restored.SetPersistence(receipts, func([]localmesh.DirectoryReceipt) error { return nil }, now); err != nil {
		t.Fatal(err)
	}
	for _, wire := range directory.Records(now) {
		if _, err = restored.Accept(wire, now); err != nil {
			t.Fatal(err)
		}
	}
	if len(restored.RouteAuthorizations(now)) != 2 {
		t.Fatal("installation origins lost across normal restore")
	}
}
