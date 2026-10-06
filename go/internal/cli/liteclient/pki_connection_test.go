package liteclient

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestPKILANTrustAndDeviceIdentity(t *testing.T) {
	const tenant = "11111111-1111-4111-8111-111111111111"
	issue := func(serial int64, name, uri string, ca bool, parent *x509.Certificate, signer *ecdsa.PrivateKey) (*x509.Certificate, *ecdsa.PrivateKey) {
		t.Helper()
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
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
			signer = key
		}
		der, err := x509.CreateCertificate(rand.Reader, template, parent, &key.PublicKey, signer)
		if err != nil {
			t.Fatal(err)
		}
		cert, err := x509.ParseCertificate(der)
		if err != nil {
			t.Fatal(err)
		}
		return cert, key
	}
	root, rootKey := issue(1, "root", "", true, nil, nil)
	intermediate, intermediateKey := issue(2, "operator authority", "", true, root, rootKey)
	operator, operatorKey := issue(3, "operator", "spiffe://wendy.sh/tenant/"+tenant+"/operator/22222222-2222-4222-8222-222222222222", false, intermediate, intermediateKey)
	client := tls.Certificate{Certificate: [][]byte{operator.Raw}, PrivateKey: operatorKey}
	chain := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: intermediate.Raw})) + string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: root.Raw}))
	for _, uri := range []string{"", "urn:wendy:org:2:user:42", "spiffe://wendy.sh/tenant/" + tenant + "/device/other"} {
		leaf, key := issue(6, "unscoped", uri, false, root, rootKey)
		c := NewWendyLiteClient()
		err := c.ConnectWithPKIAuthentication("invalid:0", tls.Certificate{Certificate: [][]byte{leaf.Raw}, PrivateKey: key}, chain, "441bf6804ff8")
		if err == nil || !strings.Contains(err.Error(), "tenant-scoped operator") {
			t.Fatalf("identity %q must be rejected before dialing: %v", uri, err)
		}
	}
	for _, tc := range []struct {
		name, principal, deviceID string
		trusted                   bool
		wantHandshake             bool
	}{
		{"matching device", "spiffe://wendy.sh/tenant/" + tenant + "/device/lite-441bf6804ff8", "441bf6804ff8", true, true},
		{"prefixed device ID", "spiffe://wendy.sh/tenant/" + tenant + "/device/lite-441bf6804ff8", "lite-441bf6804ff8", true, true},
		{"different device", "spiffe://wendy.sh/tenant/" + tenant + "/device/lite-000000000000", "441bf6804ff8", true, false},
		{"different tenant", "spiffe://wendy.sh/tenant/33333333-3333-4333-8333-333333333333/device/lite-441bf6804ff8", "441bf6804ff8", true, false},
		{"untrusted issuer", "spiffe://wendy.sh/tenant/" + tenant + "/device/lite-441bf6804ff8", "441bf6804ff8", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			issuer, issuerKey := root, rootKey
			if !tc.trusted {
				issuer, issuerKey = issue(4, "untrusted", "", true, nil, nil)
			}
			server, serverKey := issue(5, "board", tc.principal, false, issuer, issuerKey)
			pool := x509.NewCertPool()
			pool.AddCert(root)
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			completed := make(chan error, 1)
			go func() {
				raw, err := listener.Accept()
				if err != nil {
					completed <- err
					return
				}
				defer raw.Close()
				raw.SetDeadline(time.Now().Add(5 * time.Second))
				conn := tls.Server(raw, &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{{Certificate: [][]byte{server.Raw}, PrivateKey: serverKey}}, ClientCAs: pool, ClientAuth: tls.RequireAndVerifyClientCert})
				completed <- conn.Handshake()
			}()
			c := NewWendyLiteClient()
			err = c.ConnectWithPKIAuthentication(listener.Addr().String(), client, chain, tc.deviceID)
			serverErr := <-completed
			if tc.wantHandshake {
				// TLS succeeded with only the root on the server, proving the CLI sent
				// the operator intermediate. The mock then closes before WendyCom.
				if serverErr != nil || err == nil || !strings.HasPrefix(err.Error(), "handshake:") {
					t.Fatalf("TLS=%v WendyCom=%v", serverErr, err)
				}
			} else if err == nil || !strings.HasPrefix(err.Error(), "connect (mTLS):") {
				t.Fatalf("untrusted peer accepted: %v", err)
			}
		})
	}
}
