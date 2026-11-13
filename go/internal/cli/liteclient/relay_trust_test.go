package liteclient

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/mldsa"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"net/url"
	"strings"
	"testing"
	"time"
)

// relayHierarchyCert issues a certificate under any signer, so the test can
// build the real mix: an ML-DSA tenant Intermediate over a P-256 Lite Device CA.
func relayHierarchyCert(t *testing.T, serial int64, name string, ca bool, uri string, pub crypto.PublicKey, parent *x509.Certificate, signer crypto.Signer) *x509.Certificate {
	t.Helper()
	template := &x509.Certificate{SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: name}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), BasicConstraintsValid: true, IsCA: ca, KeyUsage: x509.KeyUsageDigitalSignature}
	if ca {
		template.KeyUsage |= x509.KeyUsageCertSign
	} else {
		template.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth}
	}
	if uri != "" {
		u, err := url.Parse(uri)
		if err != nil {
			t.Fatal(err)
		}
		template.URIs = []*url.URL{u}
	}
	if parent == nil {
		parent = template
	}
	der, err := x509.CreateCertificate(rand.Reader, template, parent, pub, signer)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return cert
}

func pemCerts(certs ...*x509.Certificate) string {
	var out []byte
	for _, cert := range certs {
		out = append(out, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw})...)
	}
	return string(out)
}

func TestRelayDeviceTrustFallbackChainsToSessionTrust(t *testing.T) {
	ecKey := func() *ecdsa.PrivateKey {
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		return key
	}
	rootKey, err := mldsa.GenerateKey(mldsa.MLDSA87())
	if err != nil {
		t.Fatal(err)
	}
	intermediateKey, err := mldsa.GenerateKey(mldsa.MLDSA65())
	if err != nil {
		t.Fatal(err)
	}
	root := relayHierarchyCert(t, 1, "platform root", true, "", rootKey.Public(), nil, rootKey)
	intermediate := relayHierarchyCert(t, 2, "tenant intermediate", true, "", intermediateKey.Public(), root, rootKey)
	operatorCAKey, operatorKey := ecKey(), ecKey()
	operatorCA := relayHierarchyCert(t, 3, "operator CA", true, "", operatorCAKey.Public(), intermediate, intermediateKey)
	operatorLeaf := relayHierarchyCert(t, 4, "operator", false, "spiffe://wendy.sh/tenant/11111111-1111-4111-8111-111111111111/operator/alice", operatorKey.Public(), operatorCA, operatorCAKey)
	operator := tls.Certificate{Certificate: [][]byte{operatorLeaf.Raw, operatorCA.Raw}, PrivateKey: operatorKey}
	// pki-core issuance chains exclude the Root; the session bundle is what the
	// CLI trusts from its ML-DSA login.
	session := pemCerts(operatorCA, intermediate)

	liteKey, deviceKey := ecKey(), ecKey()
	liteCA := relayHierarchyCert(t, 10, "Lite Device CA", true, "", liteKey.Public(), intermediate, intermediateKey)
	twin := relayHierarchyCert(t, 10, "Lite Device CA", true, "", liteKey.Public(), nil, liteKey)
	device := relayHierarchyCert(t, 11, "board", false, devicePrincipal, deviceKey.Public(), liteCA, liteKey)
	primary := relayHierarchyCert(t, 12, "board", false, devicePrincipal, deviceKey.Public(), operatorCA, operatorCAKey)
	strayKey := ecKey()
	strayRoot := relayHierarchyCert(t, 20, "stray root", true, "", strayKey.Public(), nil, strayKey)
	strayLite := relayHierarchyCert(t, 21, "Lite Device CA", true, "", liteKey.Public(), strayRoot, strayKey)
	withPrincipal := func(principal string) *x509.Certificate {
		return relayHierarchyCert(t, 13, "board", false, principal, deviceKey.Public(), liteCA, liteKey)
	}
	broken := func(cert *x509.Certificate) *x509.Certificate {
		copy := *cert
		copy.Signature = append([]byte(nil), cert.Signature...)
		copy.Signature[len(copy.Signature)-1] ^= 1
		return &copy
	}
	expired := *device
	expired.NotAfter = time.Now().Add(-time.Minute)
	expiredDER, err := x509.CreateCertificate(rand.Reader, &expired, liteCA, deviceKey.Public(), liteKey)
	if err != nil {
		t.Fatal(err)
	}
	expiredLeaf, err := x509.ParseCertificate(expiredDER)
	if err != nil {
		t.Fatal(err)
	}
	full := pemCerts(liteCA, intermediate)

	// waits marks the boards that reach the Lite branch; only those may wait
	// for the fetch.
	for _, tc := range []struct {
		name      string
		leaf      *x509.Certificate
		fetched   string
		lookupErr error
		waits     bool
		wantOK    bool
	}{
		{name: "existing authority", leaf: primary, fetched: full, wantOK: true},
		{name: "existing authority, discovery fails", leaf: primary, lookupErr: errors.New("unavailable"), wantOK: true},
		{name: "Lite CA chains to session trust", leaf: device, fetched: full, waits: true, wantOK: true},
		{name: "self-signed twin only", leaf: device, fetched: pemCerts(twin), waits: true},
		{name: "Lite CA under a foreign root", leaf: device, fetched: pemCerts(strayLite, strayRoot), waits: true},
		{name: "wrong device", leaf: withPrincipal(strings.Replace(devicePrincipal, "441bf6804ff8", "other", 1)), fetched: full},
		{name: "wrong tenant", leaf: withPrincipal(strings.Replace(devicePrincipal, "11111111-1111-4111-8111-111111111111", "33333333-3333-4333-8333-333333333333", 1)), fetched: full},
		{name: "expired", leaf: expiredLeaf, fetched: full},
		{name: "known issuer bad signature", leaf: broken(primary), fetched: full},
		{name: "Lite issuer bad signature", leaf: broken(device), fetched: full, waits: true},
		{name: "discovery fails", leaf: device, lookupErr: errors.New("unavailable"), waits: true},
		{name: "malformed chain", leaf: device, fetched: "invalid", waits: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			started, release := make(chan struct{}), make(chan struct{})
			cfg, err := deviceTLSConfig(ctx, operator, session, liteTestDevice, func(got context.Context) (string, error) {
				if got != ctx {
					t.Error("discovery lost the caller's context")
				}
				close(started) // a second call panics
				<-release
				return tc.fetched, tc.lookupErr
			})
			if err != nil {
				t.Fatal(err)
			}
			// The fetch starts before dialing, and is still blocked.
			select {
			case <-started:
			case <-ctx.Done():
				t.Fatal("discovery did not start before the handshake")
			}
			// The board presents its leaf and the self-signed twin it was given.
			verified := make(chan error, 1)
			go func() {
				verified <- cfg.VerifyConnection(tls.ConnectionState{PeerCertificates: []*x509.Certificate{tc.leaf, twin}})
			}()
			if tc.waits {
				select {
				case err := <-verified:
					t.Fatalf("Lite board verified before the fetch finished: %v", err)
				case <-time.After(50 * time.Millisecond):
				}
				close(release)
			} else {
				defer close(release)
			}
			select {
			case err = <-verified:
			case <-ctx.Done():
				t.Fatal("verification waited on the Lite device CA fetch")
			}
			if (err == nil) != tc.wantOK {
				t.Fatalf("verification = %v; want accepted=%v", err, tc.wantOK)
			}
		})
	}
}
