package liteclient

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestPKILANTrustAndDeviceIdentity(t *testing.T) {
	testPKITrustAndDeviceIdentity(t, func(c *WendyLiteClient, address string, cert tls.Certificate, chain, deviceID string, lite LiteDeviceCAChain) error {
		return c.ConnectWithPKIAuthentication(address, cert, chain, deviceID, lite)
	})
}

func TestPKIBLETrustAndDeviceIdentity(t *testing.T) {
	previous := openBLEStreamFn
	t.Cleanup(func() { openBLEStreamFn = previous })
	openBLEStreamFn = func(ctx context.Context, address string, psm uint16) (net.Conn, error) {
		if psm != DefaultL2CAPPSM {
			t.Errorf("PSM = %d, want %d", psm, DefaultL2CAPPSM)
		}
		return (&net.Dialer{}).DialContext(ctx, "tcp", address)
	}
	testPKITrustAndDeviceIdentity(t, func(c *WendyLiteClient, address string, cert tls.Certificate, chain, deviceID string, lite LiteDeviceCAChain) error {
		return c.ConnectViaBLEWithPKIAuthentication(address, DefaultL2CAPPSM, cert, chain, deviceID, lite)
	})
}

func testPKITrustAndDeviceIdentity(t *testing.T, connect func(*WendyLiteClient, string, tls.Certificate, string, string, LiteDeviceCAChain) error) {
	t.Helper()
	const tenant = "11111111-1111-4111-8111-111111111111"
	issue := func(serial int64, name, uri string, ca bool, parent *x509.Certificate, signer *ecdsa.PrivateKey) (*x509.Certificate, *ecdsa.PrivateKey) {
		return issueRelayCertificate(t, serial, name, uri, ca, parent, signer)
	}

	root, rootKey := issue(1, "root", "", true, nil, nil)
	intermediate, intermediateKey := issue(2, "operator authority", "", true, root, rootKey)
	operator, operatorKey := issue(3, "operator", "spiffe://wendy.sh/tenant/"+tenant+"/operator/22222222-2222-4222-8222-222222222222", false, intermediate, intermediateKey)
	client := tls.Certificate{Certificate: [][]byte{operator.Raw, intermediate.Raw}, PrivateKey: operatorKey}
	// Wendy-issued chains may contain trailing ASN.1 bytes. Both transports
	// must use the shared parser rather than the standard x509-only verifier.
	chain := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: append(append([]byte(nil), intermediate.Raw...), 0, 0)})) + string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: root.Raw}))
	if err := connect(NewWendyLiteClient(), "invalid:0", client, chain, "", nil); err == nil || !strings.Contains(err.Error(), "missing Wendy Lite device identity") {
		t.Fatalf("missing selected device identity must be rejected before dialing: %v", err)
	}
	// Only a canonical device UUID is a pin: a hardware id, a pre-UUID lite-
	// identity or a non-canonical UUID spelling is refused before dialing.
	for _, pin := range []string{"441bf6804ff8", "lite-441bf6804ff8", "lite-" + liteTestDevice, strings.ToUpper(liteTestDevice), "{" + liteTestDevice + "}"} {
		if err := connect(NewWendyLiteClient(), "invalid:0", client, chain, pin, nil); err == nil || !strings.Contains(err.Error(), "is not a device UUID") {
			t.Fatalf("pin %q must be refused before dialing: %v", pin, err)
		}
	}
	for _, uri := range []string{"", "urn:wendy:org:2:user:42", "spiffe://wendy.sh/tenant/" + tenant + "/device/other"} {
		leaf, key := issue(6, "unscoped", uri, false, root, rootKey)
		c := NewWendyLiteClient()
		err := connect(c, "invalid:0", tls.Certificate{Certificate: [][]byte{leaf.Raw}, PrivateKey: key}, chain, "441bf6804ff8", nil)
		if err == nil || !strings.Contains(err.Error(), "tenant-scoped operator") {
			t.Fatalf("identity %q must be rejected before dialing: %v", uri, err)
		}
	}
	// The Lite Device CA is a sibling under the session's root, absent from chain.
	liteCA, liteKey := issue(7, "Lite Device CA", "", true, root, rootKey)
	selfSignedLite, selfSignedLiteKey := issue(8, "Lite Device CA", "", true, nil, nil)
	const matching = "spiffe://wendy.sh/tenant/" + tenant + "/device/" + liteTestDevice
	const deviceUUID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	const uuidPrincipal = "spiffe://wendy.sh/tenant/" + tenant + "/device/" + deviceUUID
	for _, tc := range []struct {
		name, principal, deviceID string
		trusted                   bool
		lite                      *x509.Certificate
		liteKey                   *ecdsa.PrivateKey
		wantHandshake             bool
	}{
		{"matching device", matching, liteTestDevice, true, nil, nil, true},
		{"UUID device ID", uuidPrincipal, deviceUUID, true, nil, nil, true},
		{"UUID under Lite CA", uuidPrincipal, deviceUUID, false, liteCA, liteKey, true},
		{"different UUID", uuidPrincipal, "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", true, nil, nil, false},
		{"UUID in different tenant", "spiffe://wendy.sh/tenant/33333333-3333-4333-8333-333333333333/device/" + deviceUUID, deviceUUID, false, liteCA, liteKey, false},
		{"UUID under untrusted CA", uuidPrincipal, deviceUUID, false, selfSignedLite, selfSignedLiteKey, false},
		{"different device", "spiffe://wendy.sh/tenant/" + tenant + "/device/00000000-0000-4000-8000-000000000000", liteTestDevice, true, nil, nil, false},
		{"different tenant", "spiffe://wendy.sh/tenant/33333333-3333-4333-8333-333333333333/device/" + liteTestDevice, liteTestDevice, true, nil, nil, false},
		{"untrusted issuer", matching, liteTestDevice, false, nil, nil, false},
		{"Lite CA under session trust", matching, liteTestDevice, false, liteCA, liteKey, true},
		{"self-signed Lite CA", matching, liteTestDevice, false, selfSignedLite, selfSignedLiteKey, false},
		{"UUID device", "spiffe://wendy.sh/tenant/" + tenant + "/device/44444444-4444-4444-8444-444444444444", "44444444-4444-4444-8444-444444444444", true, nil, nil, true},
		{"wrong UUID device", "spiffe://wendy.sh/tenant/" + tenant + "/device/55555555-5555-4555-8555-555555555555", "44444444-4444-4444-8444-444444444444", true, nil, nil, false},
		{"UUID prefixed by legacy firmware", "spiffe://wendy.sh/tenant/" + tenant + "/device/lite-44444444-4444-4444-8444-444444444444", "44444444-4444-4444-8444-444444444444", true, nil, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			issuer, issuerKey := root, rootKey
			if !tc.trusted {
				issuer, issuerKey = issue(4, "untrusted", "", true, nil, nil)
			}
			// Every case loads the Lite chain before dialing; it only decides
			// the outcome for a leaf issued by it.
			litePEM := pemCerts(liteCA)
			if tc.lite != nil {
				issuer, issuerKey, litePEM = tc.lite, tc.liteKey, pemCerts(tc.lite)
			}
			var calls atomic.Int32
			lite := func(context.Context) (string, error) {
				calls.Add(1)
				return litePEM, nil
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
			err = connect(c, listener.Addr().String(), client, chain, tc.deviceID, lite)
			serverErr := <-completed
			// Loading starts before dialing; a Lite-issued board needs its result.
			if got := calls.Load(); got > 1 || (tc.lite != nil && got != 1) {
				t.Fatalf("Lite device CA chain loads = %d, want at most 1 (1 for a Lite-issued board)", got)
			}
			if tc.wantHandshake {
				// TLS succeeded with only the root on the server, proving the CLI sent
				// the operator intermediate. The mock then closes before WendyCom.
				if serverErr != nil || err == nil || !strings.HasPrefix(err.Error(), "handshake:") {
					t.Fatalf("TLS=%v WendyCom=%v", serverErr, err)
				}
			} else if err == nil || (!strings.HasPrefix(err.Error(), "connect (mTLS):") && !strings.HasPrefix(err.Error(), "BLE TLS handshake:")) {
				t.Fatalf("untrusted peer accepted: %v", err)
			}
		})
	}
}

func TestSensorSourceTLSRequiresExactSameTenantDevice(t *testing.T) {
	const tenant = "11111111-2222-4333-8444-555555555555"
	base := "spiffe://wendy.sh/tenant/" + tenant + "/"
	root, rootKey := issueRelayCertificate(t, 100, "root", "", true, nil, nil)
	ca, caKey := issueRelayCertificate(t, 101, "device authority", "", true, root, rootKey)
	consumer, key := issueRelayCertificate(t, 102, "consumer", base+"device/consumer", false, ca, caKey)
	client := tls.Certificate{Certificate: [][]byte{consumer.Raw, ca.Raw}, PrivateKey: key}
	chain := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: root.Raw}))
	for _, role := range []string{"operator/user", "service/user"} {
		leaf, k := issueRelayCertificate(t, 103, "not device", base+role, false, ca, caKey)
		err := NewWendyLiteClient().ConnectSensorSource(context.Background(), "invalid:0", tls.Certificate{Certificate: [][]byte{leaf.Raw}, PrivateKey: k}, chain, base+"device/source", nil)
		if err == nil || !strings.Contains(err.Error(), "consumer device identity") {
			t.Fatalf("accepted %s: %v", role, err)
		}
	}
	// A Lite board's issuer is its tenant's Lite Device CA: a sibling under the
	// consumer's root that the consumer's own chain does not carry.
	liteCA, liteKey := issueRelayCertificate(t, 106, "Lite Device CA", "", true, root, rootKey)
	twin, twinKey := issueRelayCertificate(t, 107, "Lite Device CA", "", true, nil, nil)
	liteChain := func(chain string, err error) func(context.Context) (string, error) {
		return func(context.Context) (string, error) { return chain, err }
	}
	for _, tc := range []struct {
		name, principal    string
		untrusted, expired bool
		issuer             *x509.Certificate
		issuerKey          *ecdsa.PrivateKey
		lite               func(context.Context) (string, error)
		wantTLS            bool
		wantErr            string
	}{
		{name: "matching", principal: base + "device/source", wantTLS: true},
		{name: "other device", principal: base + "device/other"},
		{name: "other tenant", principal: "spiffe://wendy.sh/tenant/aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee/device/source"},
		{name: "untrusted", principal: base + "device/source", untrusted: true},
		{name: "expired", principal: base + "device/source", expired: true},
		{name: "Lite CA without discovery", principal: base + "device/source", issuer: liteCA, issuerKey: liteKey},
		{name: "Lite CA under consumer trust", principal: base + "device/source", issuer: liteCA, issuerKey: liteKey, lite: liteChain(pemCerts(liteCA), nil), wantTLS: true},
		{name: "Lite CA, other device", principal: base + "device/other", issuer: liteCA, issuerKey: liteKey, lite: liteChain(pemCerts(liteCA), nil)},
		{name: "Lite CA, discovery failed", principal: base + "device/source", issuer: liteCA, issuerKey: liteKey, lite: liteChain("", errors.New("offline")), wantErr: "discovering Lite device CA chain"},
		{name: "self-signed twin only", principal: base + "device/source", issuer: twin, issuerKey: twinKey, lite: liteChain(pemCerts(twin), nil)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			issuer, issuerKey := root, rootKey
			if tc.untrusted {
				issuer, issuerKey = issueRelayCertificate(t, 104, "other root", "", true, nil, nil)
			}
			if tc.issuer != nil {
				issuer, issuerKey = tc.issuer, tc.issuerKey
			}
			server, serverKey := issueRelayCertificate(t, 105, "source", tc.principal, false, issuer, issuerKey)
			if tc.expired {
				template := *server
				template.NotBefore = time.Now().Add(-2 * time.Hour)
				template.NotAfter = time.Now().Add(-time.Hour)
				der, err := x509.CreateCertificate(rand.Reader, &template, issuer, &serverKey.PublicKey, issuerKey)
				if err != nil {
					t.Fatal(err)
				}
				server, err = x509.ParseCertificate(der)
				if err != nil {
					t.Fatal(err)
				}
			}
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
				conn := tls.Server(raw, &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{{Certificate: [][]byte{server.Raw}, PrivateKey: serverKey}}, ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: pool})
				completed <- conn.Handshake()
			}()
			c := NewWendyLiteClient()
			err = c.ConnectSensorSource(context.Background(), listener.Addr().String(), client, chain, base+"device/source", tc.lite)
			serverErr := <-completed
			if tc.wantTLS {
				if serverErr != nil || err == nil || !strings.HasPrefix(err.Error(), "SensorLink handshake:") {
					t.Fatalf("TLS=%v WendyCom=%v", serverErr, err)
				}
			} else if err == nil || !strings.HasPrefix(err.Error(), "connect SensorLink (mTLS):") {
				t.Fatalf("wrong peer accepted: %v", err)
			} else if tc.wantErr != "" && !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %v, want it to name %q", err, tc.wantErr)
			}
		})
	}
}

func issueRelayCertificate(t *testing.T, serial int64, name, uri string, ca bool, parent *x509.Certificate, signer *ecdsa.PrivateKey) (*x509.Certificate, *ecdsa.PrivateKey) {
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
