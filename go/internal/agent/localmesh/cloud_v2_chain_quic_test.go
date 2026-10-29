package localmesh

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net/url"
	"testing"
	"time"

	quic "github.com/quic-go/quic-go"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
)

// Cloud v2's current leaf plus three non-root issuers totals 23,740 DER bytes.
// Genuine signed certificates with a noncritical extension reproduce that wire
// pressure without embedding production certificates or private keys. The two
// peers have separate issuer branches: their shared root is insufficient unless
// the other peer actually sends its required intermediates.
func largeChainCredentials(t *testing.T, padding int) (*Credentials, *Credentials) {
	t.Helper()
	type issued struct {
		cert *x509.Certificate
		key  *ecdsa.PrivateKey
		pem  string
	}
	var serial int64
	mint := func(name, principal string, parent *issued, pad int) issued {
		t.Helper()
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		serial++
		template := &x509.Certificate{
			SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: name},
			NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
			BasicConstraintsValid: true, IsCA: principal == "", KeyUsage: x509.KeyUsageDigitalSignature,
			ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth},
		}
		if template.IsCA {
			template.KeyUsage |= x509.KeyUsageCertSign
		} else {
			u, err := url.Parse(principal)
			if err != nil {
				t.Fatal(err)
			}
			template.URIs = []*url.URL{u}
		}
		if pad > 0 {
			value, err := asn1.Marshal(bytes.Repeat([]byte{0x5a}, pad))
			if err != nil {
				t.Fatal(err)
			}
			template.ExtraExtensions = []pkix.Extension{{Id: asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 55555, 1}, Value: value}}
		}
		issuer, signer := template, key
		if parent != nil {
			issuer, signer = parent.cert, parent.key
		}
		der, err := x509.CreateCertificate(rand.Reader, template, issuer, &key.PublicKey, signer)
		if err != nil {
			t.Fatal(err)
		}
		cert, err := x509.ParseCertificate(der)
		if err != nil {
			t.Fatal(err)
		}
		return issued{cert, key, string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))}
	}
	root := mint("root", "", nil, 0)
	makePeer := func(name, principal string) *Credentials {
		t.Helper()
		issuer := root
		chain := root.pem
		for i := range 3 {
			issuer = mint(fmt.Sprintf("%s-issuer-%d", name, i), "", &issuer, padding)
			chain = issuer.pem + chain
		}
		leaf := mint(name, principal, &issuer, padding)
		der, err := x509.MarshalECPrivateKey(leaf.key)
		if err != nil {
			t.Fatal(err)
		}
		credentials, err := NewCredentials(leaf.pem, chain, string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der})))
		if err != nil {
			t.Fatal(err)
		}
		if len(credentials.Certificate.Certificate) != 4 {
			t.Fatal("expected leaf and all three non-root issuers on wire")
		}
		return credentials
	}
	return makePeer("a", testPrincipal(testTenantUUID, testDeviceAID)), makePeer("b", testPrincipal(testTenantUUID, testDeviceBID))
}

func TestCloudV2LargeChainQUIC(t *testing.T) {
	a, b := largeChainCredentials(t, 6500)
	config := func(c *Credentials, peer meshid.Device) *tls.Config {
		t.Helper()
		cfg, err := c.PeerTLS(peer)
		if err != nil {
			t.Fatal(err)
		}
		return cfg
	}
	for _, credentials := range []*Credentials{a, b} {
		var size int
		for _, der := range credentials.Certificate.Certificate {
			size += len(der)
		}
		if size <= 24<<10 || size >= 32<<10 {
			t.Fatalf("test chain must exceed 24KiB with room below 32KiB, got %d", size)
		}
		t.Logf("authenticated chain: %d DER bytes across four certificates", size)
	}
	t.Run("full-mutual-authentication", func(t *testing.T) {
		result := rootQUICRoundtrip(config(a, b.Device), config(b, a.Device))
		if result.err != nil {
			t.Fatal(result.err)
		}
		for _, state := range []tls.ConnectionState{result.client, result.server} {
			if !state.HandshakeComplete || state.DidResume || state.Version != tls.VersionTLS13 || len(state.PeerCertificates) != 4 {
				t.Fatal("expected full mutual TLS with every required issuer")
			}
		}
	})
	t.Run("wrong-server-principal", func(t *testing.T) {
		wrong := testDevice(t, testPrincipal(testTenantUUID, testDeviceCID))
		if result := rootQUICRoundtrip(config(a, wrong), config(b, a.Device)); result.err == nil {
			t.Fatal("wrong server principal accepted")
		}
	})
	t.Run("wrong-client-principal", func(t *testing.T) {
		wrong := testDevice(t, testPrincipal(testTenantUUID, testDeviceCID))
		if result := rootQUICRoundtrip(config(a, b.Device), config(b, wrong)); result.err == nil {
			t.Fatal("wrong client principal accepted")
		}
	})
	t.Run("missing-required-intermediate", func(t *testing.T) {
		a.issuerMaterial.Clear()
		server := config(b, a.Device)
		cert := server.Certificates[0]
		cert.Certificate = cert.Certificate[:1]
		server.Certificates = []tls.Certificate{cert}
		if result := rootQUICRoundtrip(config(a, b.Device), server); result.err == nil {
			t.Fatal("peer accepted without its required issuers")
		}
	})
	t.Run("untrusted-chain", func(t *testing.T) {
		_, foreign := largeChainCredentials(t, 6500)
		if result := rootQUICRoundtrip(config(a, foreign.Device), config(foreign, a.Device)); result.err == nil {
			t.Fatal("same principal under an untrusted root accepted")
		}
	})
	t.Run("64KiB-bound-remains-enforced", func(t *testing.T) {
		a, b := largeChainCredentials(t, 17000)
		result := rootQUICRoundtrip(config(a, b.Device), config(b, a.Device))
		var transport *quic.TransportError
		if !errors.As(result.err, &transport) || transport.ErrorCode != quic.CryptoBufferExceeded {
			t.Fatalf("expected bounded CRYPTO_BUFFER_EXCEEDED, got %v", result.err)
		}
	})
}
