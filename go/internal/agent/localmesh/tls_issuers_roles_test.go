package localmesh

import (
	"crypto/tls"
	"crypto/x509"
	"sync/atomic"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/shared/meshalpn"
)

// A known issuer is only a chain-completion hint. It must never authorize a
// different certificate role, tenant, or device on either side of the link.
func TestKnownIssuersDeviceAdmissionMatrix(t *testing.T) {
	root := mintIssued(t, "root", "", nil)
	issuer := mintIssued(t, "issuer", "", &root)
	a := mintIssued(t, "a", testPrincipal(testTenantUUID, testDeviceAID), &issuer)
	b := mintIssued(t, "b", testPrincipal(testTenantUUID, testDeviceBID), &issuer)
	for _, transport := range []string{"LAN", "QUIC"} {
		for _, direction := range []string{"client", "server"} {
			for _, role := range []string{"device", "operator", "user", "service", "other-tenant", "self"} {
				t.Run(transport+"/"+direction+"/"+role, func(t *testing.T) {
					ca := issuedCredentials(t, a, a.pem, issuer.pem+root.pem)
					cb := issuedCredentials(t, b, b.pem, issuer.pem+root.pem)
					ca.SetTLSRecovery(new(meshalpn.Recovery))
					cb.SetTLSRecovery(new(meshalpn.Recovery))
					principal, self := ca.Device.Principal(), cb.Device.Principal()
					if direction == "server" {
						principal, self = cb.Device.Principal(), ca.Device.Principal()
					}
					switch role {
					case "operator", "user", "service":
						principal = "spiffe://wendy.sh/tenant/" + testTenantUUID + "/" + role + "/fixture"
					case "other-tenant":
						principal = testPrincipal("ffffffff-ffff-4fff-8fff-ffffffffffff", testDeviceCID)
					case "self":
						principal = self
					}
					leaf := mintIssued(t, "presented", principal, &issuer)
					presented := tls.Certificate{Certificate: [][]byte{leaf.cert.Raw}, PrivateKey: leaf.key}
					client, err := ca.PeerTLS(cb.Device)
					if err != nil {
						t.Fatal(err)
					}
					server, err := lanServerTLS(cb)
					if err != nil {
						t.Fatal(err)
					}
					var seen atomic.Int32
					if direction == "client" {
						client.GetClientCertificate = func(*tls.CertificateRequestInfo) (*tls.Certificate, error) { return &presented, nil }
						verify := server.VerifyConnection
						server.VerifyConnection = func(state tls.ConnectionState) error {
							seen.Store(int32(len(state.PeerCertificates)))
							return verify(state)
						}
					} else {
						selectConfig := server.GetConfigForClient
						server.GetConfigForClient = func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
							selected, err := selectConfig(hello)
							if err == nil {
								selected.GetCertificate = func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return &presented, nil }
							}
							return selected, err
						}
						verify := client.VerifyPeerCertificate
						client.VerifyPeerCertificate = func(raw [][]byte, chains [][]*x509.Certificate) error {
							seen.Store(int32(len(raw)))
							return verify(raw, chains)
						}
					}
					run := rootTLSRoundtrip
					if transport == "QUIC" {
						run = rootQUICRoundtrip
					}
					result := run(client, server)
					if seen.Load() != 1 {
						t.Fatalf("verifier received %d certificates, want the leaf with omitted issuer", seen.Load())
					}
					if role == "device" {
						if result.err != nil || len(result.client.PeerCertificates) != 1 || len(result.server.PeerCertificates) != 1 {
							t.Fatalf("valid device exchange: %v", result.err)
						}
					} else if result.err == nil {
						t.Fatal("non-peer identity completed an application roundtrip")
					}
				})
			}
		}
	}
}
