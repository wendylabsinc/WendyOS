package devicepin_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"math/big"
	"net/url"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/devicepin"
)

const renameTenant = "6f1b7d3c-6b7e-4a2f-9c1e-2b4a8d5e0f31"
const renamePrincipal = "spiffe://wendy.sh/tenant/" + renameTenant + "/service/asset-42"

// certWithKey mints a leaf carrying the given SAN URIs from a caller-supplied
// key, so a test can present the SAME device key under two identity shapes —
// which is exactly what a certificate rotating onto a pki-core chain does.
func certWithKey(t *testing.T, key *ecdsa.PrivateKey, sanURIs ...string) *x509.Certificate {
	t.Helper()
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "test-device"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
	}
	for _, raw := range sanURIs {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatalf("parsing SAN %q: %v", raw, err)
		}
		tmpl.URIs = append(tmpl.URIs, u)
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("CreateCertificate: %v", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("ParseCertificate: %v", err)
	}
	return cert
}

// TestCheckAndUpdate_AdoptsLegacyPinUnderPrincipal is the WDY-2968 "device pins
// survive an ACME-enrolled device's identity shape" criterion: the store keys a
// pki-core-issued device by its principal, and the entry recorded before the
// cutover is keyed by the URN. A transitional leaf carries both, which is the
// one moment the two names are provably the same device.
func TestCheckAndUpdate_AdoptsLegacyPinUnderPrincipal(t *testing.T) {
	dir := t.TempDir()
	store, err := devicepin.Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	// Pinned before the cutover, under the URN.
	legacy := certWithKey(t, key, "urn:wendy:org:7:asset:42")
	if err := store.CheckAndUpdate(legacy, "My Device"); err != nil {
		t.Fatalf("CheckAndUpdate legacy: %v", err)
	}
	if !store.Has("urn:wendy:org:7:asset:42") {
		t.Fatal("legacy pin was not recorded")
	}

	// The same device, same key, now presenting both SANs.
	transitional := certWithKey(t, key, renamePrincipal, "urn:wendy:org:7:asset:42")
	if err := store.CheckAndUpdate(transitional, "My Device"); err != nil {
		t.Fatalf("CheckAndUpdate transitional: %v", err)
	}
	if !store.Has(renamePrincipal) {
		t.Error("pin was not carried over to the principal key")
	}
	if store.Has("urn:wendy:org:7:asset:42") {
		t.Error("legacy pin key survived the rename; unpin would have to clear two entries")
	}

	// The rename must be persisted, not just held in memory: a second process
	// that re-Opens the store would otherwise re-TOFU the device.
	reopened, err := devicepin.Open(dir)
	if err != nil {
		t.Fatalf("re-Open: %v", err)
	}
	if !reopened.Has(renamePrincipal) {
		t.Error("rename was not flushed to disk")
	}

	// And the carried-over fingerprint must still be enforced: a different key
	// under the new name, while the pinned cert is live, is a mismatch — not a
	// silent first use.
	otherKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	imposter := certWithKey(t, otherKey, renamePrincipal)
	var mismatch *devicepin.PinMismatchError
	err = reopened.CheckAndUpdate(imposter, "My Device")
	if err == nil {
		t.Error("a key change under the carried-over pin was accepted")
	} else if !errors.As(err, &mismatch) {
		t.Errorf("want PinMismatchError, got %T: %v", err, err)
	}
}

// TestCheckAndUpdate_SpiffeOnlyDeviceIsPinned covers the ACME case, where there
// never was a URN to carry over.
func TestCheckAndUpdate_SpiffeOnlyDeviceIsPinned(t *testing.T) {
	dir := t.TempDir()
	store, err := devicepin.Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	principal := "spiffe://wendy.sh/tenant/" + renameTenant + "/device/box-01"
	if err := store.CheckAndUpdate(certWithKey(t, key, principal), "Box 01"); err != nil {
		t.Fatalf("CheckAndUpdate: %v", err)
	}
	if !store.Has(principal) {
		t.Error("a SPIFFE-only device was not pinned; devicepin used to fail open on it")
	}
}
