package localmesh

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"math/big"
	"testing"
	"time"
)

func directoryFixture(t *testing.T) (*Directory, [][]byte, ed25519.PrivateKey, time.Time) {
	t.Helper()
	now := time.Unix(1800000000, 0)
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, template, template, pub, key)
	if err != nil {
		t.Fatal(err)
	}
	chain := [][]byte{der}
	cache, err := OpenIdentityCache("", DefaultCacheLimits(), func(_ [][]byte, _ time.Time) (Identity, error) { return Identity{64, 445, template.NotAfter}, nil }, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = cache.Put(chain, now); err != nil {
		t.Fatal(err)
	}
	d, err := NewDirectory(64, 10, cache)
	if err != nil {
		t.Fatal(err)
	}
	return d, chain, key, now
}

func TestDirectoryOriginSignatureLeaseAndReplay(t *testing.T) {
	d, chain, key, now := directoryFixture(t)
	m := Manifest{Version: 1, Org: 64, Asset: 445, Revision: 1, Issued: now.UnixMilli(), Expires: now.Add(time.Minute).UnixMilli(), Name: "Orin", AgentPort: 50052, Internet: true}
	sign := func(m Manifest) SignedManifest {
		t.Helper()
		w, err := SignManifest(m, chain, key, now)
		if err != nil {
			t.Fatal(err)
		}
		return w
	}
	w := sign(m)
	if changed, err := d.Accept(w, now); err != nil || !changed {
		t.Fatalf("accept: %v %v", changed, err)
	}
	if changed, err := d.Accept(w, now); err != nil || changed {
		t.Fatalf("duplicate: %v %v", changed, err)
	}
	got := d.Snapshot(now)
	got[0].Name = "mutation"
	if d.Snapshot(now)[0].Name != "Orin" {
		t.Fatal("snapshot aliases directory")
	}
	bad := w
	bad.Signature = append([]byte(nil), w.Signature...)
	bad.Signature[0] ^= 1
	if _, err := d.Accept(bad, now); err == nil {
		t.Fatal("invalid signature accepted")
	}
	m.Asset = 460
	if _, err := d.Accept(sign(m), now); err == nil {
		t.Fatal("origin spoof accepted")
	}
	m.Asset = 445
	m.Revision = 2
	m.Withdraw = true
	m.Internet = false
	m.Expires = now.Add(2 * time.Second).UnixMilli()
	if _, err := d.Accept(sign(m), now); err != nil {
		t.Fatal(err)
	}
	if len(d.Snapshot(now)) != 0 {
		t.Fatal("withdrawn entry visible")
	}
	if _, err := d.Accept(w, now.Add(3*time.Second)); err == nil {
		t.Fatal("expired short withdrawal resurrected prior version")
	}
	if len(d.Snapshot(now.Add(time.Minute))) != 0 {
		t.Fatal("expired presence visible")
	}
}

func TestManifestTransportAndLimits(t *testing.T) {
	_, chain, key, now := directoryFixture(t)
	m := Manifest{Version: 1, Org: 64, Asset: 445, Revision: 1, Issued: now.UnixMilli(), Expires: now.Add(time.Minute).UnixMilli(), AgentPort: 50052}
	m.Withdraw = true
	m.Internet = true
	if _, err := SignManifest(m, chain, key, now); err == nil {
		t.Fatal("withdrawal advertised internet")
	}
	m.Withdraw = false
	m.Internet = false
	m.Expires = now.Add(MaxLease + time.Second).UnixMilli()
	if _, err := SignManifest(m, chain, key, now); err == nil {
		t.Fatal("unbounded lease")
	}
}
