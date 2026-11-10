package operatormesh

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
	"math/big"
	"net"
	"net/url"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"github.com/wendylabsinc/wendy/go/internal/cli/netmesh"
	"github.com/wendylabsinc/wendy/go/internal/cli/netmesh/carriers"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshoperatorlink"
)

// Real enrollments have distinct user/device issuers under common primary
// trust. A verified peer issuer can be cached without becoming a trust anchor.
// The TLS flight may then omit it, but router admission must still recheck the
// complete path rather than treating the on-wire presentation as a full chain.
func TestAutomaticRouterWithCachedForeignIssuers(t *testing.T) {
	for _, tc := range []struct {
		name, transport string
		warm            bool
		mldsa           bool
	}{
		{"tcp/full", carriers.TCP, false, false},
		{"tcp/warm-issuer", carriers.TCP, true, false},
		{"ble/full", carriers.BLE, false, false},
		{"ble/warm-issuer", carriers.BLE, true, false},
		{"tcp/warm-issuer-mldsa", carriers.TCP, true, true},
		{"ble/warm-issuer-mldsa", carriers.BLE, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			transport := tc.transport
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			type issued struct {
				cert *x509.Certificate
				key  crypto.Signer
				pem  string
			}
			serial := int64(0)
			mint := func(name, principal string, parent *issued, key crypto.Signer) issued {
				t.Helper()
				if key == nil {
					var err error
					key, err = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
					if err != nil {
						t.Fatal(err)
					}
				}
				serial++
				cert := &x509.Certificate{SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: name}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), BasicConstraintsValid: true, IsCA: principal == "", KeyUsage: x509.KeyUsageDigitalSignature}
				if cert.IsCA {
					cert.KeyUsage |= x509.KeyUsageCertSign
				}
				if principal != "" {
					u, err := url.Parse(principal)
					if err != nil {
						t.Fatal(err)
					}
					cert.URIs = []*url.URL{u}
					cert.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth}
				}
				issuer, signer := cert, key
				if parent != nil {
					issuer, signer = parent.cert, parent.key
				}
				der, err := x509.CreateCertificate(rand.Reader, cert, issuer, key.Public(), signer)
				if err != nil {
					t.Fatal(err)
				}
				cert, err = x509.ParseCertificate(der)
				if err != nil {
					t.Fatal(err)
				}
				return issued{cert, key, string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))}
			}
			root := mint("root", "", nil, nil)
			deviceIssuer := mint("device issuer", "", &root, nil)
			operatorIssuer := mint("operator issuer", "", &root, nil)
			tenant, _ := meshid.ParseTenant("11111111-1111-4111-8111-111111111111")
			router, _ := meshid.ParseDevice(tenant, "22222222-2222-4222-8222-222222222222")
			device := mint("router", router.Principal(), &deviceIssuer, nil)
			var operatorKey crypto.Signer
			if tc.mldsa {
				var err error
				operatorKey, err = mldsa.GenerateKey(mldsa.MLDSA65())
				if err != nil {
					t.Fatal(err)
				}
			}
			operator := mint("operator", "spiffe://wendy.sh/tenant/"+tenant.String()+"/operator/auth0%7Ccached-issuers", &operatorIssuer, operatorKey)
			keyPEM := func(v issued) string {
				t.Helper()
				der, err := x509.MarshalPKCS8PrivateKey(v.key)
				if err != nil {
					t.Fatal(err)
				}
				return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
			}
			serverCredentials, err := localmesh.NewCredentials(device.pem, deviceIssuer.pem+root.pem, keyPEM(device))
			if err != nil {
				t.Fatal(err)
			}
			if tc.warm {
				if _, err = serverCredentials.Verify([][]byte{operator.cert.Raw, operatorIssuer.cert.Raw, root.cert.Raw}, time.Now()); err != nil {
					t.Fatal(err)
				}
			}
			serverEndpoint, err := netmesh.NewNetwork(serverCredentials.Device)
			if err != nil {
				t.Fatal(err)
			}
			defer serverEndpoint.Close()
			serverNode, err := localmesh.NewNodeWithNetwork(ctx, t.TempDir(), serverCredentials, "router", 50051, serverEndpoint)
			if err != nil {
				t.Fatal(err)
			}
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			serverResult := make(chan error, 1)
			presented := make(chan int, 1)
			go func() {
				raw, err := listener.Accept()
				if err != nil {
					serverResult <- err
					return
				}
				defer raw.Close()
				stop := context.AfterFunc(ctx, func() { _ = raw.Close() })
				defer stop()
				cfg := &tls.Config{MinVersion: tls.VersionTLS13, MaxVersion: tls.VersionTLS13, NextProtos: []string{meshoperatorlink.RouterALPN}, ClientAuth: tls.RequireAnyClientCert, VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
					_, err := serverCredentials.VerifyTLS(raw, time.Now())
					return err
				}}
				serverCredentials.ConfigureTLSServer(cfg)
				secure := tls.Server(raw, cfg)
				if err = secure.HandshakeContext(ctx); err != nil {
					serverResult <- err
					return
				}
				presented <- len(secure.ConnectionState().PeerCertificates)
				serverResult <- serverNode.AttachOperatorRouterStream(ctx, secure, 128)
			}()
			serverRun := make(chan error, 1)
			go func() { serverRun <- serverNode.Run() }()
			if transport == carriers.BLE {
				previous := dialOperatorBLE
				t.Cleanup(func() { dialOperatorBLE = previous })
				dialOperatorBLE = func(ctx context.Context, _ string, _ uint16) (net.Conn, error) {
					return (&net.Dialer{}).DialContext(ctx, "tcp", listener.Addr().String())
				}
			}
			candidate := carriers.Candidate{Device: router, Address: listener.Addr().String(), Transport: transport, PSM: 0x81, Cost: 128}
			if transport == carriers.TCP {
				candidate.PSM = 0
			}
			options := Options{StateDirectory: t.TempDir(), Certificate: config.CertificateInfo{PrincipalURI: operator.cert.URIs[0].String(), PemCertificate: operator.pem, PemCertificateChain: operatorIssuer.pem + root.pem, PemPrivateKey: keyPEM(operator)}, CarrierSources: []carriers.Source{func(ctx context.Context, emit func([]carriers.Candidate)) error {
				emit([]carriers.Candidate{candidate})
				<-ctx.Done()
				return ctx.Err()
			}}}
			session, err := Open(ctx, options)
			if err != nil {
				t.Fatal(err)
			}
			defer session.Close()
			if tc.warm {
				if _, err = session.Node.Credentials.VerifyTLS([][]byte{device.cert.Raw, deviceIssuer.cert.Raw, root.cert.Raw}, time.Now()); err != nil {
					t.Fatal(err)
				}
			}
			if _, err = session.Start(ctx); err != nil {
				t.Fatal(err)
			}
			until := time.Now().Add(2 * time.Second)
			for (!session.AdjacentLive() || len(serverNode.Snapshot().Links) == 0) && time.Now().Before(until) {
				time.Sleep(time.Millisecond)
			}
			if !session.AdjacentLive() || len(serverNode.Snapshot().Links) == 0 {
				select {
				case err := <-serverResult:
					t.Fatalf("hinted operator attachment failed: %v", err)
				default:
					t.Fatal("hinted operator attachment never became live")
				}
			}
			select {
			case count := <-presented:
				// Automatic operator attachments retain the full issuing path
				// even with a warm cache: older routers advertise known-issuer
				// TLS but cannot complete their subsequent role-binding check.
				expected := 2
				if count != expected {
					t.Fatalf("expected %d presented operator certificates, got %d", expected, count)
				}
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			session.Close()
			cancel()
			select {
			case <-serverResult:
			case <-time.After(time.Second):
				t.Fatal("operator server did not stop")
			}
			select {
			case <-serverRun:
			case <-time.After(time.Second):
				t.Fatal("router node did not stop")
			}
		})
	}
}
