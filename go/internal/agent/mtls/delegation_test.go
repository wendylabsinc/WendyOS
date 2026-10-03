package mtls

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

	"github.com/wendylabsinc/wendy/go/internal/agent/delegation"
)

func TestDelegatedCertificateOnlyAcceptedByScopedGRPCVerifier(t *testing.T) {
	caPEM, keyPEM := testCACertificate(t, "issuer")
	block, _ := pem.Decode([]byte(caPEM))
	ca, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	block, _ = pem.Decode([]byte(keyPEM))
	key, err := x509.ParseECPrivateKey(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	owner := "spiffe://wendy.sh/tenant/11111111-1111-4111-8111-111111111111/operator/user"
	device := "spiffe://wendy.sh/tenant/11111111-1111-4111-8111-111111111111/device/robot"
	raw := func(s string) asn1.RawValue { return asn1.RawValue{Tag: asn1.TagUTF8String, Bytes: []byte(s)} }
	wire := struct {
		Version       int
		ID            string `asn1:"utf8"`
		Owner         string `asn1:"utf8"`
		Devices, Apps []asn1.RawValue
	}{1, "22222222-2222-4222-8222-222222222222", owner, []asn1.RawValue{raw(device)}, []asn1.RawValue{raw("demo")}}
	scope, _ := asn1.Marshal(wire)
	ent, _ := asn1.Marshal([]asn1.RawValue{raw("entitlement:wendy.agent.services.v2.WendyContainerService:StopContainer:allow")})
	u, _ := url.Parse(owner)
	clientKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	leaf := &x509.Certificate{SerialNumber: big.NewInt(42), NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, URIs: []*url.URL{u}, ExtraExtensions: []pkix.Extension{{Id: delegation.ScopeOID, Critical: true, Value: scope}, {Id: delegation.EntitlementsOID, Value: ent}}}
	der, err := x509.CreateCertificate(rand.Reader, leaf, ca, &clientKey.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	if err := buildVerifyPeerCertificate(pool, []*x509.Certificate{ca}, nil, time.Time{})([][]byte{der}, nil); err == nil {
		t.Fatal("generic verifier accepted delegated cert, exposing registry/BLE bypass")
	}
	if err := buildVerifyPeerCertificateWithDelegation(pool, []*x509.Certificate{ca}, nil, time.Time{}, true)([][]byte{der}, nil); err != nil {
		t.Fatalf("scoped verifier rejected valid delegated cert: %v", err)
	}
	leaf.ExtraExtensions[0].Critical = false
	der, err = x509.CreateCertificate(rand.Reader, leaf, ca, &clientKey.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	if err := buildVerifyPeerCertificateWithDelegation(pool, []*x509.Certificate{ca}, nil, time.Time{}, true)([][]byte{der}, nil); err == nil {
		t.Fatal("noncritical scope accepted")
	}
}
