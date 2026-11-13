package cloudrelay

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"math/big"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLiteOperatorIdentityMatchesActivePrincipal(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	principal := "spiffe://wendy.sh/tenant/2558fd76-afc7-466e-9613-6b715296a526/operator/11111111-1111-4111-8111-111111111111"
	uri, _ := url.Parse(principal)
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test CA"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	caDER, err := x509.CreateCertificate(rand.Reader, ca, ca, key.Public(), key)
	if err != nil {
		t.Fatal(err)
	}
	leaf := &x509.Certificate{SerialNumber: big.NewInt(2), NotBefore: ca.NotBefore, NotAfter: ca.NotAfter, URIs: []*url.URL{uri}, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}
	der, err := x509.CreateCertificate(rand.Reader, leaf, ca, key.Public(), key)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	chain := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: raw})
	dir := t.TempDir()
	certPath := filepath.Join(dir, "cert.pem")
	keyPath := filepath.Join(dir, "key.pem")
	if err = os.WriteFile(certPath, append(certPEM, chain...), 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(keyPath, keyPEM, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WENDY_LITE_OPERATOR_CERT", certPath)
	t.Setenv("WENDY_LITE_OPERATOR_KEY", keyPath)
	got, err := LiteOperatorIdentity(config.CertificateInfo{PrincipalURI: principal})
	if err != nil {
		t.Fatal(err)
	}
	if got.PrincipalURI != principal || got.PemCertificateChain != string(chain) || got.PemPrivateKey != string(keyPEM) {
		t.Fatal("incorrect Lite identity")
	}
	if _, err = LiteOperatorIdentity(config.CertificateInfo{PrincipalURI: principal + "-different"}); err == nil {
		t.Fatal("accepted another operator")
	}
	t.Setenv("WENDY_LITE_OPERATOR_KEY", "")
	if _, err = LiteOperatorIdentity(config.CertificateInfo{PrincipalURI: principal}); err == nil {
		t.Fatal("accepted partial configuration")
	}
}
