package localmesh

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"math/big"
	"net/url"
	"reflect"
	"sync"
	"testing"
	"time"
)

// Each authority is independent. Renewal may keep the signing key, but never
// borrows trust from the retired issuer or another principal's cached presence.
type manifestTestAuthority struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pem  string
}

func newManifestTestAuthority(t testing.TB, now time.Time) manifestTestAuthority {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: now.Add(-time.Hour), NotAfter: now.Add(24 * time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return manifestTestAuthority{cert, key, string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))}
}
func (a manifestTestAuthority) issue(t testing.TB, now time.Time, key *ecdsa.PrivateKey, expiry time.Time) *Credentials {
	t.Helper()
	if key == nil {
		var err error
		key, err = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
	}
	uri, _ := url.Parse(testPrincipal(testTenantUUID, testDeviceAID))
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), NotBefore: now.Add(-time.Hour), NotAfter: expiry, URIs: []*url.URL{uri}, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, a.cert, &key.PublicKey, a.key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	creds, err := NewCredentials(string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), a.pem, string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})))
	if err != nil {
		t.Fatal(err)
	}
	return creds
}

func TestDirectoryRenewedKeyCannotAuthorizeOldManifest(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	old := newManifestTestAuthority(t, now).issue(t, now, nil, now.Add(time.Hour))
	renewed := newManifestTestAuthority(t, now).issue(t, now, nil, now.Add(2*time.Hour))
	allowOld := true
	verify := func(chain [][]byte, at time.Time) (Identity, error) {
		if id, err := renewed.Verify(chain, at); err == nil {
			return id, nil
		}
		if allowOld {
			return old.Verify(chain, at)
		}
		return Identity{}, errors.New("old issuer retired")
	}
	cache, err := OpenIdentityCache("", DefaultCacheLimits(), verify, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = cache.Put(old.Certificate.Certificate, now); err != nil {
		t.Fatal(err)
	}
	d, err := NewDirectory(old.Device.Tenant(), 10, cache)
	if err != nil {
		t.Fatal(err)
	}
	m := routeManifest(old.Device, now)
	w, err := SignManifest(m, old.Certificate.Certificate, old.Signer, now)
	if err != nil {
		t.Fatal(err)
	}
	if changed, err := d.Accept(w, now); err != nil || !changed {
		t.Fatal(changed, err)
	}
	if len(d.Snapshot(now)) != 1 || len(d.Records(now)) != 1 || !d.RouteAuthorizations(now)[*m.Route] {
		t.Fatal("initial manifest or signed route absent")
	}
	allowOld = false
	if _, err = cache.Put(renewed.Certificate.Certificate, now); err != nil {
		t.Fatal(err)
	}
	if len(d.Snapshot(now)) != 0 {
		t.Error("renewed key lends authority to old signature in Snapshot")
	}
	if len(d.Records(now)) != 0 {
		t.Error("old signature remains in Records after issuer retirement")
	}
	if len(d.RouteAuthorizations(now)) != 0 {
		t.Error("renewed key lends authority to the old signed route")
	}
	s := NewSynchronizer(d, cache)
	defer s.Close()
	if len(s.Record(w, now)) != 0 {
		t.Error("queued old signature pairs with renewed certificate descriptor")
	}
}

func TestDirectoryRenewalResignRecoveryPreservesReplayAndLease(t *testing.T) {
	for _, sameKey := range []bool{false, true} {
		t.Run(map[bool]string{false: "rotated-key", true: "same-key"}[sameKey], func(t *testing.T) {
			now := time.Now().Truncate(time.Second)
			old := newManifestTestAuthority(t, now).issue(t, now, nil, now.Add(time.Hour))
			var key *ecdsa.PrivateKey
			if sameKey {
				key = old.Signer.(*ecdsa.PrivateKey)
			}
			renewed := newManifestTestAuthority(t, now).issue(t, now, key, now.Add(2*time.Hour))
			allowOld := true
			verify := func(chain [][]byte, at time.Time) (Identity, error) {
				if id, err := renewed.Verify(chain, at); err == nil {
					return id, nil
				}
				if allowOld {
					return old.Verify(chain, at)
				}
				return Identity{}, errors.New("old issuer retired")
			}
			cache, err := OpenIdentityCache("", DefaultCacheLimits(), verify, now)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = cache.Put(old.Certificate.Certificate, now); err != nil {
				t.Fatal(err)
			}
			d, err := NewDirectory(old.Device.Tenant(), 10, cache)
			if err != nil {
				t.Fatal(err)
			}
			saves := 0
			if err = d.SetPersistence(nil, func([]DirectoryReceipt) error { saves++; return nil }, now); err != nil {
				t.Fatal(err)
			}
			m := routeManifest(old.Device, now)
			m.Revision = 7
			wire, err := SignManifest(m, old.Certificate.Certificate, old.Signer, now)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = d.Accept(wire, now); err != nil {
				t.Fatal(err)
			}
			before, receipt := d.entries[wire.Principal], d.history[wire.Principal]
			allowOld = false
			if _, err = cache.Put(renewed.Certificate.Certificate, now.Add(time.Second)); err != nil {
				t.Fatal(err)
			}
			projected := len(d.Snapshot(now.Add(time.Second)))
			if sameKey && projected != 1 || !sameKey && projected != 0 {
				t.Fatalf("current key projection=%d sameKey=%v", projected, sameKey)
			}
			if authorized := d.RouteAuthorizations(now.Add(time.Second))[*m.Route]; authorized != sameKey {
				t.Fatalf("current key route authorization=%v sameKey=%v", authorized, sameKey)
			}
			resigned, err := SignManifest(m, renewed.Certificate.Certificate, renewed.Signer, now.Add(time.Second))
			if err != nil {
				t.Fatal(err)
			}
			changed, err := d.Accept(resigned, now.Add(time.Second))
			if err != nil || changed == sameKey {
				t.Fatalf("resign recovery changed=%v sameKey=%v err=%v", changed, sameKey, err)
			}
			after := d.entries[wire.Principal]
			if !d.RouteAuthorizations(now.Add(time.Second))[*m.Route] {
				t.Fatal("renewed signature did not recover its exact signed route")
			}
			if !reflect.DeepEqual(after.manifest, before.manifest) || !after.retainUntil.Equal(before.retainUntil) || d.history[wire.Principal] != receipt || saves != 1 {
				t.Fatal("re-sign extended lease/retention or changed durable receipt")
			}
			admitted := resigned
			rejected := wire
			if sameKey {
				admitted, rejected = wire, resigned
			}
			chain, ok := d.CurrentRecordChain(admitted, now.Add(time.Second))
			if !ok || !sameChain(chain, renewed.Certificate.Certificate) {
				t.Fatal("current wire not bound to the renewed descriptor")
			}
			chain[0][0] ^= 1
			if chain2, ok := d.CurrentRecordChain(admitted, now.Add(time.Second)); !ok || !sameChain(chain2, renewed.Certificate.Certificate) {
				t.Fatal("returned chain aliases the cache")
			}
			if _, ok := d.CurrentRecordChain(rejected, now.Add(time.Second)); ok {
				t.Fatal("queued alternative signature treated as exact current artifact")
			}
			sender := NewSynchronizer(d, cache)
			defer sender.Close()
			if got := sender.Record(rejected, now.Add(time.Second)); len(got) != 0 {
				t.Fatal("rejected queued wire was exported")
			}
			messages := sender.Record(admitted, now.Add(time.Second))
			if len(messages) != 2 || messages[0].Certificate.Chain.Hashes[0] != certificateHash(renewed.Certificate.Certificate[0]) || messages[1].Manifest == nil {
				t.Fatal("descriptor does not match admitted signature's chain")
			}
			coldCache, err := OpenIdentityCache("", DefaultCacheLimits(), renewed.Verify, now)
			if err != nil {
				t.Fatal(err)
			}
			cold, err := NewDirectory(renewed.Device.Tenant(), 10, coldCache)
			if err != nil {
				t.Fatal(err)
			}
			receiver := NewSynchronizer(cold, coldCache)
			defer receiver.Close()
			if changed := exchangeDirectoryMessages(t, sender, receiver, messages, now.Add(time.Second)); len(changed) != 1 || len(cold.Snapshot(now.Add(time.Second))) != 1 || !cold.RouteAuthorizations(now.Add(time.Second))[*m.Route] {
				t.Fatal("renewed signature and exact signed route did not reach cold receiver")
			}
			if len(d.Snapshot(now.Add(time.Minute))) != 0 || len(d.Records(now.Add(time.Minute))) != 0 {
				t.Fatal("renewal extended signed expiry")
			}
			// A higher revision recovers normally; current-key replays remain stale.
			// Continue after the observed expiry: the route stage remembers its
			// wall high-water mark and must not rewind time for this recovery.
			recoveryAt := now.Add(62 * time.Second)
			m.Revision++
			m.Issued = recoveryAt.UnixMilli()
			m.Expires = recoveryAt.Add(90 * time.Second).UnixMilli()
			next, err := SignManifest(m, renewed.Certificate.Certificate, renewed.Signer, recoveryAt)
			if err != nil {
				t.Fatal(err)
			}
			if changed, err := d.Accept(next, recoveryAt); err != nil || !changed {
				t.Fatal(changed, err)
			}
			stale := m
			stale.Revision--
			staleWire, err := SignManifest(stale, renewed.Certificate.Certificate, renewed.Signer, recoveryAt)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = d.Accept(staleWire, recoveryAt.Add(time.Second)); !errors.Is(err, ErrStaleManifest) {
				t.Fatalf("current-key old revision bypassed floor: %v", err)
			}
			if _, ok := d.CurrentRecordChain(admitted, recoveryAt.Add(time.Second)); ok {
				t.Fatal("superseded queued wire exported")
			}
			if d.history[wire.Principal].Revision != 8 || saves != 2 {
				t.Fatal("new revision did not preserve receipt persistence")
			}
		})
	}
}

func TestManifestSignatureBindingChecksEachLeafOnlyOnce(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	old := newManifestTestAuthority(t, now).issue(t, now, nil, now.Add(time.Hour))
	sameKey := newManifestTestAuthority(t, now).issue(t, now, old.Signer.(*ecdsa.PrivateKey), now.Add(2*time.Hour))
	rotated := newManifestTestAuthority(t, now).issue(t, now, nil, now.Add(3*time.Hour))
	w, err := SignManifest(routeManifest(old.Device, now), old.Certificate.Certificate, old.Signer, now)
	if err != nil {
		t.Fatal(err)
	}
	binding := manifestSignatureBinding{leafHash: certificateHash(old.Certificate.Certificate[0]), valid: true}
	checks := 0
	check := func(c *Credentials) bool {
		return binding.matches(certificateHash(c.Certificate.Certificate[0]), func() bool {
			checks++
			leaf, err := x509.ParseCertificate(c.Certificate.Certificate[0])
			if err != nil {
				t.Fatal(err)
			}
			return validManifestSignature(w, leaf)
		})
	}
	for range 100 {
		if !check(old) {
			t.Fatal("admitted unchanged leaf lost validity")
		}
	}
	if checks != 0 {
		t.Fatal("unchanged leaf repeated crypto", checks)
	}
	for range 100 {
		if !check(sameKey) {
			t.Fatal("same key renewal lost valid signature")
		}
	}
	if checks != 1 {
		t.Fatal("renewed leaf was not verified exactly once", checks)
	}
	for range 100 {
		if check(rotated) {
			t.Fatal("rotated key authorized old signature")
		}
	}
	if checks != 2 {
		t.Fatal("invalid unchanged leaf repeated crypto", checks)
	}
}

func TestDirectoryCurrentTrustAndCertificateLifetimePrecedeSignatureMemo(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	creds := newManifestTestAuthority(t, now).issue(t, now, nil, now.Add(time.Hour))
	allowed := true
	currentExpiry := now.Add(time.Hour)
	verify := func(chain [][]byte, at time.Time) (Identity, error) {
		if !allowed {
			return Identity{}, errors.New("issuer retired")
		}
		id, err := creds.Verify(chain, at)
		id.NotAfter = currentExpiry
		return id, err
	}
	cache, err := OpenIdentityCache("", DefaultCacheLimits(), verify, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = cache.Put(creds.Certificate.Certificate, now); err != nil {
		t.Fatal(err)
	}
	d, err := NewDirectory(creds.Device.Tenant(), 10, cache)
	if err != nil {
		t.Fatal(err)
	}
	m := routeManifest(creds.Device, now)
	m.Revision, m.Withdraw = 2, true
	w, err := SignManifest(m, creds.Certificate.Certificate, creds.Signer, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = d.Accept(w, now); err != nil {
		t.Fatal(err)
	}
	if len(d.Snapshot(now)) != 0 || len(d.Records(now)) != 1 {
		t.Fatal("withdrawal projection/export wrong")
	}
	currentExpiry = now.Add(30 * time.Second)
	if len(d.Records(now)) != 0 {
		t.Fatal("signature memo bypassed shorter current verified chain lifetime")
	}
	currentExpiry = now.Add(time.Hour)
	if len(d.Records(now)) != 1 {
		t.Fatal("current verified chain lifetime did not recover")
	}
	allowed = false
	if _, ok := d.CurrentRecordChain(w, now); ok || len(d.Records(now)) != 0 {
		t.Fatal("memo bypassed current trust rejection")
	}
	if d.history[w.Principal].Revision != 2 {
		t.Fatal("trust rejection erased replay floor")
	}
}

func TestDirectoryConcurrentRenewalExportsMatchingSignatureAndChain(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	old := newManifestTestAuthority(t, now).issue(t, now, nil, now.Add(time.Hour))
	renewed := newManifestTestAuthority(t, now).issue(t, now, nil, now.Add(time.Hour))
	verify := func(chain [][]byte, at time.Time) (Identity, error) {
		if id, err := renewed.Verify(chain, at); err == nil {
			return id, nil
		}
		return old.Verify(chain, at)
	}
	cache, err := OpenIdentityCache("", DefaultCacheLimits(), verify, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = cache.Put(old.Certificate.Certificate, now); err != nil {
		t.Fatal(err)
	}
	d, err := NewDirectory(old.Device.Tenant(), 10, cache)
	if err != nil {
		t.Fatal(err)
	}
	w, err := SignManifest(routeManifest(old.Device, now), old.Certificate.Certificate, old.Signer, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = d.Accept(w, now); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	wg.Go(func() {
		for range 32 {
			for _, c := range []*Credentials{renewed, old} {
				if _, err := cache.Put(c.Certificate.Certificate, now); err != nil {
					t.Error(err)
					return
				}
			}
		}
	})
	for range 4 {
		wg.Go(func() {
			for range 64 {
				d.Snapshot(now)
				d.Records(now)
				if chain, ok := d.CurrentRecordChain(w, now); ok {
					leaf, err := x509.ParseCertificate(chain[0])
					if err != nil || !validManifestSignature(w, leaf) {
						t.Error("export paired mismatched wire and chain")
						return
					}
				}
			}
		})
	}
	wg.Wait()
}

func TestDirectorySignatureMemoOwnsAdmittedAndExportedWire(t *testing.T) {
	d, chain, key, device, now := directoryFixture(t)
	m := routeManifest(device, now)
	m.Name = "original"
	w, err := SignManifest(m, chain, key, now)
	if err != nil {
		t.Fatal(err)
	}
	original := cloneSignedManifest(w)
	if _, err = d.Accept(w, now); err != nil {
		t.Fatal(err)
	}
	w.Body[0] ^= 1
	w.Signature[0] ^= 1
	if _, ok := d.CurrentRecordChain(w, now); ok {
		t.Fatal("caller mutation inherited a positive signature verdict")
	}
	if _, ok := d.CurrentRecordChain(original, now); !ok {
		t.Fatal("caller mutation changed the owned admitted wire")
	}
	records := d.Records(now)
	if len(records) != 1 {
		t.Fatal("admitted wire disappeared")
	}
	records[0].Body[0] ^= 1
	records[0].Signature[0] ^= 1
	if _, ok := d.CurrentRecordChain(original, now); !ok || d.Snapshot(now)[0].Name != "original" {
		t.Fatal("export aliases the admitted signature or body")
	}
}
