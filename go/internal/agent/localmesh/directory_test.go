package localmesh

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"github.com/wendylabsinc/wendy/go/internal/agent/netwire"
	"math/big"
	"net/url"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
)

// directoryFixture is one admitted device's own directory: the cached chain is
// that device's leaf, so every manifest here is signed by the device it names.
func directoryFixture(t *testing.T) (*Directory, [][]byte, ed25519.PrivateKey, meshid.Device, time.Time) {
	t.Helper()
	now := time.Unix(1800000000, 0)
	device := testDevice(t, testPrincipal(testTenantUUID, testDeviceAID))
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	uri, err := url.Parse(device.Principal())
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour), URIs: []*url.URL{uri}}
	der, err := x509.CreateCertificate(rand.Reader, template, template, pub, key)
	if err != nil {
		t.Fatal(err)
	}
	chain := [][]byte{der}
	cache, err := OpenIdentityCache("", DefaultCacheLimits(), func(_ [][]byte, _ time.Time) (Identity, error) {
		return Identity{Device: device, NotAfter: template.NotAfter}, nil
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = cache.Put(chain, now); err != nil {
		t.Fatal(err)
	}
	d, err := NewDirectory(testTenant(t), 10, cache)
	if err != nil {
		t.Fatal(err)
	}
	return d, chain, key, device, now
}

func TestDirectoryOriginSignatureLeaseAndReplay(t *testing.T) {
	d, chain, key, device, now := directoryFixture(t)
	m := routeManifest(device, now)
	m.Name = "Orin"
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
	// A manifest correctly signed by an admitted device but naming a different
	// device UUID is refused: the advertisement is about its signer, and the
	// principal is the key it is stored under.
	other := testDevice(t, testPrincipal(testTenantUUID, testDeviceBID))
	mismatched := m
	mismatched.Device = other.UUID()
	mismatched.Revision = 3
	body, err := netwire.Marshal(mismatched)
	if err != nil {
		t.Fatal(err)
	}
	input, _ := signingInput(key.Public(), device.Principal(), body)
	if _, err = d.Accept(SignedManifest{Principal: device.Principal(), Body: body, Signature: ed25519.Sign(key, input)}, now); err == nil {
		t.Fatal("advertisement for another device accepted")
	}
	// A principal from another tenant never reaches signature or cache checks.
	foreign := w
	foreign.Principal = testPrincipal(testOtherTenant, testDeviceAID)
	if _, err = d.Accept(foreign, now); err == nil {
		t.Fatal("manifest from another tenant accepted")
	}
	m.Revision = 2
	m.Withdraw = true
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
	_, chain, key, device, now := directoryFixture(t)
	manifest := func(mutate func(*Manifest)) SignedManifest {
		t.Helper()
		m := routeManifest(device, now)
		mutate(&m)
		w, err := SignManifest(m, chain, key, now)
		if err == nil {
			t.Fatal("invalid manifest signed without error")
		}
		return w
	}
	manifest(func(m *Manifest) { m.Version = 2 })
	manifest(func(m *Manifest) { m.Device = uuid.Nil })
	manifest(func(m *Manifest) { m.Revision = 0 })
	manifest(func(m *Manifest) { m.AgentPort = 0 })
	manifest(func(m *Manifest) { m.Expires = now.Add(MaxLease + time.Second).UnixMilli() })
	manifest(func(m *Manifest) { m.Expires = now.Add(-time.Second).UnixMilli() })
	manifest(func(m *Manifest) { m.Name = string(bytes.Repeat([]byte("n"), 256)) })
}

func TestManifestAllowsExplicitNilUUID(t *testing.T) {
	now := time.Now()
	m := routeManifest(testDevice(t, testPrincipal(testTenantUUID, uuid.Nil.String())), now)
	if err := m.validate(now); err != nil {
		t.Fatalf("nil UUID is still a UUID: %v", err)
	}
	// SignManifest/Accept separately require that the complete principal's device
	// UUID equals this field, including for a certificate with an explicit nil UUID.
}
