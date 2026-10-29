package localmesh

import (
	"crypto/tls"
	"net"
	"testing"
	"time"
)

// Exercise actual certificate messages, rather than merely testing callbacks.
// Both shared and distinct issuing paths are ordinary valid enrollments.
func TestNormalKnownIssuersTLSWire(t *testing.T) {
	root := mintIssued(t, "root", "", nil)
	top := mintIssued(t, "top", "", &root)
	issuing := mintIssued(t, "issuing", "", &top)
	other := mintIssued(t, "other", "", &top)
	a := mintIssued(t, "a", testPrincipal(testTenantUUID, testDeviceAID), &issuing)
	b := mintIssued(t, "b", testPrincipal(testTenantUUID, testDeviceBID), &issuing)
	c := mintIssued(t, "c", testPrincipal(testTenantUUID, testDeviceCID), &other)
	ca := issuedCredentials(t, a, a.pem+issuing.pem+top.pem, root.pem)
	cb := issuedCredentials(t, b, b.pem+issuing.pem+top.pem, root.pem)
	cc := issuedCredentials(t, c, c.pem+other.pem+top.pem, root.pem)
	t.Run("shared-QUIC", func(t *testing.T) {
		client, err := ca.PeerTLSWithTickets(cb.Device, LinkALPN, LinkQUICSessionScope)
		if err != nil {
			t.Fatal(err)
		}
		server, err := lanServerTLS(cb)
		if err != nil {
			t.Fatal(err)
		}
		result := rootQUICRoundtrip(client, server)
		if result.err != nil {
			t.Fatal(result.err)
		}
		if len(result.client.PeerCertificates) != 1 || len(result.server.PeerCertificates) != 1 {
			t.Fatalf("QUIC sent known intermediates: client=%d server=%d", len(result.client.PeerCertificates), len(result.server.PeerCertificates))
		}
	})
	for _, pair := range []struct {
		name           string
		client, server *Credentials
		count          int
		turnover       bool
	}{
		{"shared-a-b", ca, cb, 1, false}, {"shared-b-a", cb, ca, 1, false},
		{"distinct-a-c", ca, cc, 2, false}, {"cached-c-a", cc, ca, 1, false}, {"cache-turnover-during-setup", ca, cc, 1, true},
	} {
		t.Run(pair.name, func(t *testing.T) {
			clientConfig, err := pair.client.PeerTLS(pair.server.Device)
			if err != nil {
				t.Fatal(err)
			}
			serverConfig, err := lanServerTLS(pair.server)
			if err != nil {
				t.Fatal(err)
			}
			if pair.turnover {
				forget := func(c *Credentials) { c.issuerMaterial.Clear() }
				forget(pair.client) // Its offered inventory is already frozen.
				previous := serverConfig.GetConfigForClient
				serverConfig.GetConfigForClient = func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
					selected, err := previous(hello)
					forget(pair.server) // Its acknowledged inventory is frozen too.
					return selected, err
				}
			}
			left, right := net.Pipe()
			defer left.Close()
			defer right.Close()
			left.SetDeadline(time.Now().Add(5 * time.Second))
			right.SetDeadline(time.Now().Add(5 * time.Second))
			server := tls.Server(right, serverConfig)
			client := tls.Client(left, clientConfig)
			done := make(chan error, 1)
			go func() { done <- server.Handshake() }()
			if err := client.Handshake(); err != nil {
				t.Fatal(err)
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			for role, state := range map[string]tls.ConnectionState{"client": client.ConnectionState(), "server": server.ConnectionState()} {
				if ApplicationProtocol(state.NegotiatedProtocol) != LinkALPN || len(state.PeerCertificates) != pair.count {
					t.Fatalf("%s: protocol %q, certificates %d, want %d", role, state.NegotiatedProtocol, len(state.PeerCertificates), pair.count)
				}
				bytes := 0
				for _, cert := range state.PeerCertificates {
					bytes += len(cert.Raw)
				}
				t.Logf("%s received %d certificates, %d DER bytes", role, pair.count, bytes)
			}
		})
	}
}
