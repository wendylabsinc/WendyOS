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
	"fmt"
	"io"
	"math/big"
	"net"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/bleprovider"
	"github.com/wendylabsinc/wendy/go/internal/cli/netmesh/carriers"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshalpn"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshoperatorlink"
)

type idleBLEPacketEndpoint struct{}

func (idleBLEPacketEndpoint) ReadPacket(ctx context.Context) ([]byte, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}
func (idleBLEPacketEndpoint) Inject([]byte) error { return nil }

// The native radio is substituted by a stream. TLS, primary identity, signed
// operator proof, userspace endpoint and same-TLS management are production.
func TestOperatorBLEAttachmentUsesPrimaryProofAndSharedTLS(t *testing.T) {
	for _, algorithm := range []string{"P256", "MLDSA65", "P256-auto", "MLDSA65-auto", "P256-auto-known-issuer"} {
		t.Run(algorithm, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			key := func() *ecdsa.PrivateKey {
				k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
				if err != nil {
					t.Fatal(err)
				}
				return k
			}
			now := time.Now()
			rootKey := key()
			root := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
			rootDER, err := x509.CreateCertificate(rand.Reader, root, root, rootKey.Public(), rootKey)
			if err != nil {
				t.Fatal(err)
			}
			root, err = x509.ParseCertificate(rootDER)
			if err != nil {
				t.Fatal(err)
			}
			issue := func(serial int64, principal string, signer crypto.Signer) tls.Certificate {
				uri, err := url.Parse(principal)
				if err != nil {
					t.Fatal(err)
				}
				leaf := &x509.Certificate{SerialNumber: big.NewInt(serial), NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour), URIs: []*url.URL{uri}, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth}}
				der, err := x509.CreateCertificate(rand.Reader, leaf, root, signer.Public(), rootKey)
				if err != nil {
					t.Fatal(err)
				}
				return tls.Certificate{Certificate: [][]byte{der, rootDER}, PrivateKey: signer}
			}
			tenant, _ := meshid.ParseTenant("11111111-1111-4111-8111-111111111111")
			router, _ := meshid.ParseDevice(tenant, "22222222-2222-4222-8222-222222222222")
			principal := "spiffe://wendy.sh/tenant/" + tenant.String() + "/operator/alice%7Cble"
			var signer crypto.Signer = key()
			if strings.HasPrefix(algorithm, "MLDSA65") {
				signer, err = mldsa.GenerateKey(mldsa.MLDSA65())
				if err != nil {
					t.Fatal(err)
				}
			}
			certificate := issue(2, principal, signer)
			chainPEM := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: rootDER}))
			knownIssuer := strings.Contains(algorithm, "known-issuer")
			if knownIssuer {
				// A deployed router can have cached this operator's intermediate
				// from an earlier installation. Its TLS verifier supports omission,
				// but its router-role verifier still needs the full wire chain.
				caKey := key()
				ca := &x509.Certificate{SerialNumber: big.NewInt(4), Subject: pkix.Name{CommonName: "Operator Authority"}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
				caDER, err := x509.CreateCertificate(rand.Reader, ca, root, caKey.Public(), rootKey)
				if err != nil {
					t.Fatal(err)
				}
				ca, err = x509.ParseCertificate(caDER)
				if err != nil {
					t.Fatal(err)
				}
				leaf, err := x509.ParseCertificate(certificate.Certificate[0])
				if err != nil {
					t.Fatal(err)
				}
				leafDER, err := x509.CreateCertificate(rand.Reader, leaf, ca, signer.Public(), caKey)
				if err != nil {
					t.Fatal(err)
				}
				certificate.Certificate = [][]byte{leafDER, caDER, rootDER}
				chainPEM = string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})) + chainPEM
			}
			serverCertificate := issue(3, router.Principal(), key())
			privateDER, err := x509.MarshalPKCS8PrivateKey(signer)
			if err != nil {
				t.Fatal(err)
			}
			certificateInfo := config.CertificateInfo{PrincipalURI: principal, PemCertificate: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificate.Certificate[0]})), PemCertificateChain: chainPEM, PemPrivateKey: string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privateDER}))}
			left, right := net.Pipe()
			defer left.Close()
			defer right.Close()
			previous := dialOperatorBLE
			t.Cleanup(func() { dialOperatorBLE = previous })
			dialOperatorBLE = func(_ context.Context, address string, psm uint16) (net.Conn, error) {
				if address != "peripheral.fixture" || psm != 0x81 {
					return nil, fmt.Errorf("wrong selected BLE endpoint")
				}
				return left, nil
			}
			serverDone := make(chan error, 1)
			admitted := make(chan *meshoperatorlink.RouterLink, 1)
			go func() {
				serverConfig := &tls.Config{MinVersion: tls.VersionTLS13, MaxVersion: tls.VersionTLS13, Certificates: []tls.Certificate{serverCertificate}, ClientAuth: tls.RequireAnyClientCert, NextProtos: []string{meshoperatorlink.RouterALPN}, CurvePreferences: []tls.CurveID{tls.X25519}, SessionTicketsDisabled: true}
				if knownIssuer {
					material := meshalpn.NewCertificates(serverCertificate, []*x509.Certificate{root})
					material.Remember(certificate.Certificate, now)
					material.ConfigureServer(serverConfig)
				}
				secure := tls.Server(right, serverConfig)
				if err := secure.HandshakeContext(ctx); err != nil {
					serverDone <- err
					return
				}
				link, err := meshoperatorlink.AcceptRouter(ctx, secure, tenant, []*x509.Certificate{root}, func(_ context.Context, c net.Conn) error {
					var b [4]byte
					if _, err := io.ReadFull(c, b[:]); err != nil {
						return err
					}
					_, err := c.Write(b[:])
					return err
				})
				if err != nil {
					serverDone <- err
					return
				}
				defer func() { _ = link.Close(); link.Wait() }()
				admitted <- link
				select {
				case <-ctx.Done():
					serverDone <- ctx.Err()
				case <-link.Done():
					serverDone <- nil
				}
			}()
			options := Options{CarrierSources: []carriers.Source{}, Certificate: certificateInfo, StateDirectory: t.TempDir(), RouterAddress: "peripheral.fixture", RouterUUID: router.UUID().String(), Transport: "ble"}
			if strings.Contains(algorithm, "-auto") {
				id := router.UUID()
				options.Automatic = true
				options.RouterAddress = ""
				options.RouterUUID = ""
				options.Transport = ""
				options.CarrierSources = []carriers.Source{func(ctx context.Context, emit func([]carriers.Candidate)) error {
					emit([]carriers.Candidate{{Transport: carriers.BLE, Address: "peripheral.fixture", PSM: 0x81, Hint: [4]byte(id[:4]), Cost: bleprovider.LinkCost}})
					<-ctx.Done()
					return ctx.Err()
				}}
			}
			session, err := Open(ctx, options)
			if err != nil {
				t.Fatal(err)
			}
			defer session.Close()
			if options.Automatic && (session.initialRouter != nil || !session.Node.Credentials.Device.IsOperator()) {
				t.Fatal("cold automatic join required an offered identity or seed")
			}
			if _, err = session.Start(ctx); err != nil {
				t.Fatal(err)
			}
			if _, err = session.AgentConnection(ctx); err != nil {
				t.Fatal(err)
			}
			admission := <-admitted
			if admission.Peer().Principal() != principal || admission.Address() != session.Endpoint.Address() {
				t.Fatal("BLE changed the primary operator or local router address")
			}
			setup, stop := context.WithCancel(ctx)
			agent, err := session.openAgent(setup, "")
			if err != nil {
				t.Fatal(err)
			}
			stop()
			if _, err = agent.Write([]byte("ping")); err != nil {
				t.Fatal(err)
			}
			var reply [4]byte
			if _, err = io.ReadFull(agent, reply[:]); err != nil || string(reply[:]) != "ping" {
				t.Fatalf("same-TLS management %q: %v", reply, err)
			}
			agent.Close()
			session.Close()
			cancel()
			select {
			case <-serverDone:
			case <-time.After(time.Second):
				t.Fatal("BLE attachment driver did not join")
			}
		})
	}
}
