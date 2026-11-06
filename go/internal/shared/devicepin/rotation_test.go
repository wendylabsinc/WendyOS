package devicepin_test

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/devicepin"
)

type rotationCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pem  string
}

func newRotationCA(t *testing.T) rotationCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "rotation-test CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(7 * 24 * time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return rotationCA{cert, key, string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))}
}

func (ca rotationCA) leaf(t *testing.T, identity string, expiry time.Time) *x509.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: "agx-orin"}, DNSNames: []string{"agx-orin.local"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: expiry,
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	if identity != "" {
		u, err := url.Parse(identity)
		if err != nil {
			t.Fatal(err)
		}
		template.URIs = []*url.URL{u}
	}
	der, err := x509.CreateCertificate(rand.Reader, template, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return cert
}

func TestVerifiedDeviceRotation(t *testing.T) {
	const principal = "spiffe://wendy.sh/tenant/" + renameTenant + "/device/box-01"
	for _, identity := range []string{"urn:wendy:org:7:asset:42", principal} {
		t.Run(identity, func(t *testing.T) {
			ca := newRotationCA(t)
			first := ca.leaf(t, identity, time.Now().Add(24*time.Hour))
			expected, ok, err := certs.IdentityFromCert(first)
			if err != nil || !ok {
				t.Fatalf("identity: %v, %v", ok, err)
			}
			dir := t.TempDir()
			verifyWithStore := func() func(tls.ConnectionState) error {
				t.Helper()
				store, err := devicepin.Open(dir)
				if err != nil {
					t.Fatal(err)
				}
				verify, err := certs.BuildServerVerifyConnection(certs.ServerVerifyOpts{
					ChainPEM: ca.pem, ExpectedOrgID: expected.OrgID, ExpectedIdentity: &expected, PinStore: store,
				})
				if err != nil {
					t.Fatal(err)
				}
				return verify
			}
			if err := verifyWithStore()(tls.ConnectionState{PeerCertificates: []*x509.Certificate{first}}); err != nil {
				t.Fatalf("initial connection: %v", err)
			}
			// A new process reads the old hash, then receives a valid replacement
			// with the same name, hostname and identity but a different key.
			verify := verifyWithStore()
			renewed := ca.leaf(t, identity, time.Now().Add(48*time.Hour))
			if bytes.Equal(first.RawSubjectPublicKeyInfo, renewed.RawSubjectPublicKeyInfo) {
				t.Fatal("test requires a new key")
			}
			for _, resumed := range []bool{false, true} {
				if err := verify(tls.ConnectionState{PeerCertificates: []*x509.Certificate{renewed}, DidResume: resumed}); err != nil {
					t.Fatalf("rotation with DidResume=%v: %v", resumed, err)
				}
			}
			path := filepath.Join(dir, "known_devices.json")
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			otherIdentity := "urn:wendy:org:7:asset:43"
			otherTenant := "urn:wendy:org:8:asset:42"
			if identity == principal {
				otherIdentity = "spiffe://wendy.sh/tenant/" + renameTenant + "/device/box-02"
				otherTenant = "spiffe://wendy.sh/tenant/11111111-1111-4111-8111-111111111111/device/box-01"
			}
			for name, leaf := range map[string]*x509.Certificate{
				"untrusted issuer":        newRotationCA(t).leaf(t, identity, renewed.NotAfter),
				"different device":        ca.leaf(t, otherIdentity, renewed.NotAfter),
				"different tenant or org": ca.leaf(t, otherTenant, renewed.NotAfter),
				"missing identity":        ca.leaf(t, "", renewed.NotAfter),
				"expired replacement":     ca.leaf(t, identity, time.Now().Add(-time.Minute)),
			} {
				t.Run(name, func(t *testing.T) {
					// Every rejected certificate uses the same display name and hostname.
					for _, resumed := range []bool{false, true} {
						if err := verify(tls.ConnectionState{PeerCertificates: []*x509.Certificate{leaf}, DidResume: resumed}); err == nil {
							t.Fatalf("accepted invalid replacement with DidResume=%v", resumed)
						}
					}
					after, err := os.ReadFile(path)
					if err != nil {
						t.Fatal(err)
					}
					if !bytes.Equal(before, after) {
						t.Fatal("rejected certificate changed the stored identity record")
					}
				})
			}
		})
	}
}
