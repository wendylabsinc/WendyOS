package localmesh

import (
	"bytes"
	"crypto/ecdsa"
	"errors"
	"testing"
	"time"
)

func TestDirectoryCertificateReuseKeepsExportsDetached(t *testing.T) {
	d, chain, key, device, now := directoryFixture(t)
	w, err := SignManifest(routeManifest(device, now), chain, key, now)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := d.Accept(w, now); err != nil || !ok {
		t.Fatalf("accept: %t %v", ok, err)
	}
	first, ok := d.CurrentRecordChain(w, now)
	if !ok {
		t.Fatal("initial export rejected")
	}
	before := cloneChain(first)
	for _, der := range first {
		clear(der)
	}
	second, ok := d.CurrentRecordChain(w, now)
	if !ok || !sameChain(second, before) {
		t.Fatal("export mutation reached retained chain or parsed leaf")
	}
	if bytes.Equal(second[0], first[0]) {
		t.Fatal("fixture did not mutate exported bytes")
	}
}

func TestDirectoryCertificateReuseStillRevalidatesEveryExport(t *testing.T) {
	d, chain, key, device, now := directoryFixture(t)
	w, err := SignManifest(routeManifest(device, now), chain, key, now)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := d.Accept(w, now); err != nil || !ok {
		t.Fatalf("accept: %t %v", ok, err)
	}
	verify := d.cache.verify
	checks := 0
	revoked := false
	d.cache.verify = func(chain [][]byte, now time.Time) (Identity, error) {
		checks++
		if revoked {
			return Identity{}, errors.New("test trust withdrawn")
		}
		return verify(chain, now)
	}
	for range 3 {
		if _, ok := d.RecordDeadline(w, now); !ok {
			t.Fatal("warm export failed")
		}
	}
	if checks != 3 {
		t.Fatalf("warm exports performed %d trust checks, want 3", checks)
	}
	revoked = true
	if _, ok := d.RecordDeadline(w, now); ok || checks != 4 {
		t.Fatal("parsed-leaf reuse bypassed current trust")
	}
}

func TestDirectoryCertificateReuseRejectsRenewedLeafWindow(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	authority := newManifestTestAuthority(t, now)
	old := authority.issue(t, now, nil, now.Add(time.Hour))
	// Same signing key, but this leaf starts after the signed manifest date.
	renewed := authority.issue(t, now.Add(45*time.Minute), old.Signer.(*ecdsa.PrivateKey), now.Add(2*time.Hour))
	cache, err := OpenIdentityCache("", DefaultCacheLimits(), old.Verify, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cache.Put(old.Certificate.Certificate, now); err != nil {
		t.Fatal(err)
	}
	d, err := NewDirectory(old.Device.Tenant(), 2, cache)
	if err != nil {
		t.Fatal(err)
	}
	manifest := routeManifest(old.Device, now.Add(-30*time.Minute))
	w, err := SignManifest(manifest, old.Certificate.Certificate, old.Signer, now)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := d.Accept(w, now); err != nil || !ok {
		t.Fatalf("accept: %t %v", ok, err)
	}
	if _, ok := d.RecordDeadline(w, now); !ok {
		t.Fatal("old leaf did not export")
	}
	if _, err := cache.Put(renewed.Certificate.Certificate, now); err != nil {
		t.Fatal(err)
	}
	if _, ok := d.RecordDeadline(w, now); ok {
		t.Fatal("same-key renewal reused old leaf's date window")
	}
}
