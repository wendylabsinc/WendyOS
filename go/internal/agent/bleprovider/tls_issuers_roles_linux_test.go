//go:build linux

package bleprovider

import (
	"crypto/tls"
	"crypto/x509"
	"sync/atomic"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
)

func TestBLEKnownIssuersDeviceAdmissionMatrix(t *testing.T) {
	for _, known := range []bool{false, true} {
		contact := "first-contact"
		if known {
			contact = "pinned-reconnect"
		}
		for _, direction := range []string{"client", "server"} {
			for _, role := range []string{"device", "operator", "user", "service", "other-tenant", "self"} {
				t.Run(contact+"/"+direction+"/"+role, func(t *testing.T) {
					high, low, issue := issuerCredentialsWithLeaves(t, true, false, ecdsaDeviceKey)
					principal, self := high.Device.Principal(), low.Device.Principal()
					if direction == "server" {
						principal, self = low.Device.Principal(), high.Device.Principal()
					}
					switch role {
					case "operator", "user", "service":
						principal = "spiffe://wendy.sh/tenant/" + testTenant.String() + "/" + role + "/fixture"
					case "other-tenant":
						principal = "spiffe://wendy.sh/tenant/ffffffff-ffff-4fff-8fff-ffffffffffff/device/" + high.Device.UUID().String()
					case "self":
						principal = self
					}
					presented := issue(principal)
					clientRuntime := &runtime{cfg: Config{Credentials: high}}
					if known {
						clientRuntime.known = map[string]meshid.Device{issuerPeer(low).key(): low.Device}
					}
					client, authenticated := bleClientConfig(t, clientRuntime, issuerPeer(low))
					server := (&runtime{cfg: Config{Credentials: low}}).makeServerTLS()
					var seen atomic.Int32
					receiver := server
					if direction == "client" {
						client.GetClientCertificate = func(*tls.CertificateRequestInfo) (*tls.Certificate, error) { return &presented, nil }
					} else {
						receiver = client
						selectConfig := server.GetConfigForClient
						server.GetConfigForClient = func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
							selected, err := selectConfig(hello)
							if err == nil {
								selected.GetCertificate = func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return &presented, nil }
							}
							return selected, err
						}
					}
					verify := receiver.VerifyPeerCertificate
					receiver.VerifyPeerCertificate = func(raw [][]byte, chains [][]*x509.Certificate) error {
						seen.Store(int32(len(raw)))
						if verify != nil {
							return verify(raw, chains)
						}
						return nil // The production VerifyConnection still performs admission.
					}
					result := handshakeOverAir(t, slowBLE, client, server)
					if seen.Load() != 1 {
						t.Fatalf("verifier received %d certificates, want one leaf with omitted issuers", seen.Load())
					}
					if role == "device" {
						if result.err() != nil || *authenticated != low.Device || len(result.client.PeerCertificates) != 1 || len(result.server.PeerCertificates) != 1 {
							t.Fatalf("valid device handshake: %v identity=%v", result.err(), authenticated)
						}
					} else {
						if result.err() == nil {
							t.Fatal("non-peer identity completed a BLE handshake")
						}
						if direction == "server" && *authenticated != (meshid.Device{}) {
							t.Fatal("rejected server populated the authenticated peer")
						}
					}
				})
			}
		}
	}
}
