package operatorcert

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/pem"
	"math/big"
	"net/url"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/internal/shared/operatorpermission"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
)

func TestValidateIssuedCertificate(t *testing.T) {
	const device = "22222222-2222-4222-8222-222222222222"
	principal, _ := url.Parse("spiffe://wendy.sh/tenant/11111111-1111-4111-8111-111111111111/operator/test")
	now := time.Now().Truncate(time.Second)
	rootKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	rootTemplate := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "trusted CA"}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	rootDER, err := x509.CreateCertificate(rand.Reader, rootTemplate, rootTemplate, rootKey.Public(), rootKey)
	if err != nil {
		t.Fatal(err)
	}
	root, _ := x509.ParseCertificate(rootDER)
	rootPEM := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: rootDER}))
	loginKey, _ := certs.GenerateMLDSAKeyPair()
	freshKey, _ := certs.GenerateMLDSAKeyPair()
	mint := func(key string, tmpl *x509.Certificate) string {
		signer, err := certs.ParseSigningPrivateKeyPEM([]byte(key))
		if err != nil {
			t.Fatal(err)
		}
		der, err := x509.CreateCertificate(rand.Reader, tmpl, root, signer.Public(), rootKey)
		if err != nil {
			t.Fatal(err)
		}
		return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	}
	base := x509.Certificate{SerialNumber: big.NewInt(2), NotBefore: now.Add(-time.Second), NotAfter: now.Add(4 * time.Minute), URIs: []*url.URL{principal}, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}
	login := config.CertificateInfo{PemCertificate: mint(loginKey, &base), PemCertificateChain: rootPEM, PemPrivateKey: loginKey}
	for _, tc := range []string{"valid", "wrong key", "reused login key", "wrong principal", "wrong target", "no extension", "too long", "expired", "untrusted chain"} {
		t.Run(tc, func(t *testing.T) {
			tmpl := base
			claim := "entitlement:device:shell:device:" + device + ":allow"
			key := freshKey
			localLogin := login
			switch tc {
			case "wrong principal":
				u, _ := url.Parse("spiffe://wendy.sh/tenant/11111111-1111-4111-8111-111111111111/operator/another")
				tmpl.URIs = []*url.URL{u}
			case "wrong target":
				claim = "entitlement:device:shell:device:other:allow"
			case "too long":
				tmpl.NotAfter = now.Add(time.Hour)
			case "expired":
				tmpl.NotAfter = now.Add(-time.Second)
			case "reused login key":
				key = loginKey
			case "untrusted chain":
				localLogin.PemCertificateChain = ""
			}
			if tc != "no extension" {
				der, _ := asn1.Marshal([]asn1.RawValue{{Tag: 12, Bytes: []byte(claim)}})
				tmpl.ExtraExtensions = []pkix.Extension{{Id: operatorpermission.OID, Value: der}}
			}
			issued := mint(key, &tmpl)
			if tc == "wrong key" {
				key = loginKey
			}
			resp := &pb.RequestEntitledCertificateResponse{CertificatePem: issued, ChainPem: rootPEM, Entitlements: []string{"entitlement:device:*:allow"}}
			got, err := Validate(localLogin, key, resp, device, now)
			if (err == nil) != (tc == "valid") {
				t.Fatalf("Validate=%v", err)
			}
			if tc == "valid" && (got.PemCertificate == login.PemCertificate || got.PemPrivateKey == login.PemPrivateKey) {
				t.Fatal("reused login credential")
			}
		})
	}
}
