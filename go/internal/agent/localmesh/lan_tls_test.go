package localmesh

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	quic "github.com/quic-go/quic-go"
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"
)

func TestLANUDPListenerFullChainAndIdentity(t *testing.T) {
	a, b := largeChainCredentials(t, 6500)
	server, err := lanServerTLS(b)
	if err != nil {
		t.Fatal(err)
	}
	client, err := a.PeerTLSWithTickets(b.Device, LinkALPN, LinkQUICSessionScope)
	if err != nil {
		t.Fatal(err)
	}
	result := rootQUICRoundtrip(client, server)
	if result.err != nil {
		t.Fatal(result.err)
	}
	peer, err := lanAuthenticatedPeer(b, result.server)
	if err != nil || peer != a.Device {
		t.Fatalf("actual authenticated identity: %v %v", peer, err)
	}
	if len(result.server.PeerCertificates) != 4 || len(result.client.PeerCertificates) != 4 {
		t.Fatal("Cloud v2 issuer chain missing")
	}
	// Resumption must still revalidate trust and principal, never trust a ticket alone.
	saved := b.Verify
	b.Verify = func([][]byte, time.Time) (Identity, error) { return Identity{}, errors.New("trust changed") }
	if err := server.VerifyConnection(tls.ConnectionState{DidResume: true, PeerCertificates: result.server.PeerCertificates}); err == nil {
		t.Fatal("resumed trust change accepted")
	}
	b.Verify = saved
}

func TestLANUDPListenerRejectsUntrustedIdentities(t *testing.T) {
	a, b := testCredentials(t)
	server, err := lanServerTLS(b)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name        string
		certificate *Credentials
	}{
		{"cross-tenant-same-CA", testCredentialsFor(t, testPrincipal(testOtherTenant, testDeviceAID))},
		{"untrusted-CA", testForeignCredentials(t, a.Device.Principal())},
		{"self", b},
		{"missing-certificate", nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			client, err := a.PeerTLS(b.Device)
			if err != nil {
				t.Fatal(err)
			}
			client.Certificates = nil
			client.GetClientCertificate = nil
			if test.certificate != nil {
				client.Certificates = []tls.Certificate{test.certificate.Certificate}
			}
			result := rootQUICRoundtrip(client, server)
			if result.err == nil {
				t.Fatal("untrusted inbound identity admitted")
			}
		})
	}
	client, err := a.PeerTLS(testDevice(t, testPrincipal(testTenantUUID, testDeviceCID)))
	if err != nil {
		t.Fatal(err)
	}
	if result := rootQUICRoundtrip(client, server); result.err == nil {
		t.Fatal("advertised wrong principal bypassed outbound pin")
	}
}

// Dropping real UDP sends forces QUIC's handshake/control retransmission. No
// framed stream or TCP transport is available to this fixture.
type lanLossSocket struct {
	net.PacketConn
	sent    atomic.Int64
	dropped atomic.Int64
}

func (s *lanLossSocket) WriteTo(packet []byte, address net.Addr) (int, error) {
	n := s.sent.Add(1)
	if n == 1 || n%11 == 0 {
		s.dropped.Add(1)
		return len(packet), nil
	}
	return s.PacketConn.WriteTo(packet, address)
}

func TestLANUDPRecoversLossAndKeepsSharedTransport(t *testing.T) {
	a, b := largeChainCredentials(t, 6500)
	socket, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer socket.Close()
	lossy := &lanLossSocket{PacketConn: socket}
	transport := &quic.Transport{Conn: lossy}
	defer transport.Close()
	server, err := lanServerTLS(b)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := transport.Listen(server, QUICConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	payload := bytes.Repeat([]byte("udp-control"), 4096)
	for trial := 0; trial < 2; trial++ {
		done := make(chan error, 1)
		go func() {
			conn, err := listener.Accept(ctx)
			if err != nil {
				done <- err
				return
			}
			defer conn.CloseWithError(0, "trial done")
			peer, err := lanAuthenticatedPeer(b, conn.ConnectionState().TLS)
			if err == nil && peer != a.Device {
				err = errors.New("wrong authenticated principal")
			}
			var control *quic.Stream
			if err == nil {
				control, err = OpenControl(ctx, conn, b.Device, peer)
			}
			if err == nil {
				_, err = control.Write(payload)
			}
			if err == nil {
				var ack [1]byte
				_, err = io.ReadFull(control, ack[:])
			}
			done <- err
		}()
		client, err := a.PeerTLSWithTickets(b.Device, LinkALPN, LinkQUICSessionScope)
		if err != nil {
			t.Fatal(err)
		}
		conn, err := quic.DialAddr(ctx, listener.Addr().String(), client, QUICConfig())
		if err != nil {
			t.Fatal(err)
		}
		if trial == 1 && !conn.ConnectionState().TLS.DidResume {
			conn.CloseWithError(1, "resumption missing")
			t.Fatal("UDP listener failed to resume authenticated peer")
		}
		stream, err := OpenControl(ctx, conn, a.Device, b.Device)
		if err != nil {
			conn.CloseWithError(1, "failure")
			t.Fatal(err)
		}
		got := make([]byte, len(payload))
		_, err = io.ReadFull(stream, got)
		if err != nil || !bytes.Equal(got, payload) {
			conn.CloseWithError(1, "failure")
			t.Fatalf("UDP loss corrupted reliable control: %v", err)
		}
		if _, err = stream.Write([]byte{1}); err != nil {
			t.Fatal(err)
		}
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		conn.CloseWithError(0, "trial done")
	}
	if lossy.dropped.Load() == 0 {
		t.Fatal("loss fixture dropped no UDP packets")
	}
	t.Logf("real UDP sends=%d dropped=%d", lossy.sent.Load(), lossy.dropped.Load())
}

func TestLANUDPListenerRejectsTrustedNonDevicePrincipals(t *testing.T) {
	a, b := testCredentials(t)
	server, err := lanServerTLS(b)
	if err != nil {
		t.Fatal(err)
	}
	for _, principal := range []string{
		"spiffe://wendy.sh/tenant/" + testTenantUUID + "/service/user-lan-review",
		"spiffe://wendy.sh/tenant/" + testTenantUUID + "/service/lan-review",
	} {
		t.Run(principal, func(t *testing.T) {
			certificate, key := testIssue(t, principal)
			identity, err := tls.X509KeyPair([]byte(certificate+testPKI().chainPEM), []byte(key))
			if err != nil {
				t.Fatal(err)
			}
			var chain []*x509.Certificate
			for _, raw := range identity.Certificate {
				cert, err := x509.ParseCertificate(raw)
				if err != nil {
					t.Fatal(err)
				}
				chain = append(chain, cert)
			}
			// Establish this is a current client certificate from the same
			// trusted issuer; rejection must be about its non-device principal.
			roots := x509.NewCertPool()
			roots.AddCert(testPKI().certificate)
			if _, err := chain[0].Verify(x509.VerifyOptions{Roots: roots, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err != nil {
				t.Fatal(err)
			}
			client, err := a.PeerTLS(b.Device)
			if err != nil {
				t.Fatal(err)
			}
			client.Certificates = []tls.Certificate{identity}
			if result := rootQUICRoundtrip(client, server); result.err == nil {
				t.Fatal("trusted non-device certificate authenticated on UDP listener")
			}
			for _, resumed := range []bool{false, true} {
				if err := server.VerifyConnection(tls.ConnectionState{DidResume: resumed, PeerCertificates: chain}); err == nil {
					t.Fatalf("non-device principal accepted with resumed=%v", resumed)
				}
			}
		})
	}
}
