package cloudrelay_test

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net"
	"net/netip"
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"github.com/wendylabsinc/wendy/go/internal/agent/meshingress"
	"github.com/wendylabsinc/wendy/go/internal/agent/meshsession"
	"github.com/wendylabsinc/wendy/go/internal/shared/cloudrelay"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
)

// Only the authorization service's request-signature check is a fixture seam:
// testRelay inspects the signed descriptor as plaintext and accepts fixed test
// bearer credentials. Device request policy is tested by the Cloud handler.
// Everything after authorization uses the real caller/agent protocol: signed
// grants, encrypted instructions, normal-role broker joins and challenge proofs,
// and end-to-end device mTLS followed by live application-port admission.
func TestApplicationCloudRelayEndToEnd(t *testing.T) {
	t.Run("TCP echo and live revocation", func(t *testing.T) {
		f := newApplicationRelayFixture(t)
		conn, err := meshsession.DialTLSWithDialer(f.ctx, f.source, f.target.Device, f.address(), f.tcpPort, f.dial)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		if err := conn.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
			t.Fatal(err)
		}
		payload := bytes.Repeat([]byte("application relay TCP\x00"), 4096)
		writeDone := make(chan error, 1)
		go func() { _, err := conn.Write(payload); writeDone <- err }()
		got := make([]byte, len(payload))
		if _, err := io.ReadFull(conn, got); err != nil || !bytes.Equal(got, payload) {
			t.Fatalf("TCP echo length=%d: %v", len(got), err)
		}
		if err := <-writeDone; err != nil {
			t.Fatal(err)
		}
		f.assertNormalRoles(t)
		f.registry.Release("tcp-app")
		if _, err := conn.Read(make([]byte, 1)); err == nil {
			t.Fatal("revoked TCP application connection remained usable")
		} else if timeout, ok := err.(net.Error); ok && timeout.Timeout() {
			t.Fatal("revoked TCP application connection did not close")
		}
	})
	t.Run("UDP boundaries and live revocation", func(t *testing.T) {
		f := newApplicationRelayFixture(t)
		flow, err := meshsession.DialUDPTLSWithDialer(f.ctx, f.source, f.target.Device, f.address(), f.udpPort, f.dial)
		if err != nil {
			t.Fatal(err)
		}
		defer flow.Close()
		// 8KiB is valid on Darwin as well as Linux; framing ceilings have
		// separate protocol tests independent of the host UDP socket limit.
		for _, payload := range [][]byte{nil, []byte("UDP relay"), bytes.Repeat([]byte{0xa5}, 8192), []byte("last")} {
			if err := flow.Send(payload); err != nil {
				t.Fatal(err)
			}
			reply, err := flow.Receive(f.ctx)
			if err != nil {
				t.Fatalf("UDP echo length=%d: %v", len(payload), err)
			}
			match := bytes.Equal(reply.Payload, payload)
			reply.Release()
			if !match {
				t.Fatalf("UDP message boundaries changed for length=%d", len(payload))
			}
		}
		f.assertNormalRoles(t)
		f.registry.Release("udp-app")
		receiveCtx, stop := context.WithTimeout(f.ctx, time.Second)
		defer stop()
		if message, err := flow.Receive(receiveCtx); err == nil {
			message.Release()
			t.Fatal("revoked UDP application delivered a datagram")
		} else if errors.Is(err, context.DeadlineExceeded) {
			t.Fatal("revoked UDP application flow did not close")
		}
	})
	for _, port := range []uint16{22, 50052} {
		t.Run("administrative TCP port "+strconv.Itoa(int(port)), func(t *testing.T) {
			f := newApplicationRelayFixture(t)
			conn, err := meshsession.DialTLSWithDialer(f.ctx, f.source, f.target.Device, f.address(), port, f.dial)
			if err == nil && conn != nil {
				conn.Close()
			}
			if !errors.Is(err, meshsession.ErrDenied) {
				t.Fatalf("non-application TCP port admitted: %v", err)
			}
			f.assertNormalRoles(t)
		})
	}
	t.Run("unclaimed UDP port", func(t *testing.T) {
		f := newApplicationRelayFixture(t)
		flow, err := meshsession.DialUDPTLSWithDialer(f.ctx, f.source, f.target.Device, f.address(), 22, f.dial)
		if flow != nil {
			flow.Close()
		}
		if !errors.Is(err, meshsession.ErrDenied) {
			t.Fatalf("non-application UDP port admitted: %v", err)
		}
		f.assertNormalRoles(t)
	})
	t.Run("wrong destination principal", func(t *testing.T) {
		f := newApplicationRelayFixture(t)
		wrong, err := meshid.ParseDevice(f.target.Device.Tenant(), "00000000-0000-4000-8000-000000000043")
		if err != nil {
			t.Fatal(err)
		}
		// The authorized broker still delivers to device 42. The inner TLS
		// verifier must reject it when the caller expected device 43.
		conn, err := meshsession.DialTLSWithDialer(f.ctx, f.source, wrong, netip.AddrPortFrom(wrong.MeshAddress(), meshsession.TLSPort), f.tcpPort, f.dial)
		if err == nil && conn != nil {
			conn.Close()
		}
		if err == nil {
			t.Fatal("broker delivery overrode the expected device principal")
		}
		f.assertNormalRoles(t)
	})
}

type applicationRelayFixture struct {
	ctx            context.Context
	source, target *localmesh.Credentials
	registry       *meshingress.Registry
	tcpPort        uint16
	udpPort        uint16
	dial           func(context.Context, string, string) (net.Conn, error)
	assertRoles    func(*testing.T)
}

func (f *applicationRelayFixture) address() netip.AddrPort {
	return netip.AddrPortFrom(f.target.Device.MeshAddress(), meshsession.TLSPort)
}

func (f *applicationRelayFixture) assertNormalRoles(t *testing.T) {
	t.Helper()
	f.assertRoles(t)
}

// Keep actual registry ownership/token/revocation behavior while routing private
// container endpoints to local echo sockets in this host-only protocol test.
type applicationLoopbackRegistry struct{ *meshingress.Registry }

func (r applicationLoopbackRegistry) DialAuthorized(port uint16, dial func(string) (net.Conn, error)) (net.Conn, error) {
	return r.Registry.DialAuthorized(port, func(address string) (net.Conn, error) {
		_, p, err := net.SplitHostPort(address)
		if err != nil {
			return nil, err
		}
		return dial(net.JoinHostPort("127.0.0.1", p))
	})
}

func (r applicationLoopbackRegistry) WithAuthorizedUDPToken(port uint16, token uint64, write func(netip.AddrPort) error) error {
	return r.Registry.WithAuthorizedUDPToken(port, token, func(address netip.AddrPort) error {
		return write(netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), address.Port()))
	})
}

func newApplicationRelayFixture(t *testing.T) *applicationRelayFixture {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	source, target := applicationRelayCredentials(t)
	f := &applicationRelayFixture{ctx: ctx, source: source, target: target, registry: meshingress.NewRegistry()}
	tcp, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { tcp.Close() })
	f.tcpPort = uint16(tcp.Addr().(*net.TCPAddr).Port)
	go func() {
		for {
			conn, err := tcp.Accept()
			if err != nil {
				return
			}
			go func() { defer conn.Close(); _, _ = io.Copy(conn, conn) }()
		}
	}()
	udp, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { udp.Close() })
	f.udpPort = uint16(udp.LocalAddr().(*net.UDPAddr).Port)
	go func() {
		buffer := make([]byte, meshsession.MaxUDPMessageSize)
		for {
			n, peer, err := udp.ReadFromUDP(buffer)
			if err != nil {
				return
			}
			_, _ = udp.WriteToUDP(buffer[:n], peer)
		}
	}()
	if err := f.registry.ClaimForApp("tcp-app", "com.wendy.echo", f.tcpPort, "10.3.0.2", f.tcpPort); err != nil {
		t.Fatal(err)
	}
	if err := f.registry.ClaimUDPForApp("udp-app", "com.wendy.echo", f.udpPort, "10.3.0.2", f.udpPort); err != nil {
		t.Fatal(err)
	}
	appServer, err := meshsession.NewServer(target, applicationLoopbackRegistry{f.registry})
	if err != nil {
		t.Fatal(err)
	}
	appListener := meshsession.NewApplicationListener()
	appDone := make(chan error, 1)
	go func() { appDone <- appServer.ServeTLS(ctx, appListener) }()
	t.Cleanup(func() { cancel(); appServer.Close(); <-appDone })

	f.dial, f.assertRoles = cloudrelay.NewApplicationRelayTestBroker(t, ctx, appListener.Dial)
	return f
}

func applicationRelayCredentials(t *testing.T) (*localmesh.Credentials, *localmesh.Credentials) {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature}
	caDER, err := x509.CreateCertificate(rand.Reader, ca, ca, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	rootPEM := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}))
	issue := func(serial int64, device string) *localmesh.Credentials {
		t.Helper()
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		principal, err := url.Parse("spiffe://wendy.sh/tenant/11111111-1111-4111-8111-111111111111/device/" + device)
		if err != nil {
			t.Fatal(err)
		}
		leaf := &x509.Certificate{SerialNumber: big.NewInt(serial), NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Hour), BasicConstraintsValid: true, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth}, URIs: []*url.URL{principal}}
		der, err := x509.CreateCertificate(rand.Reader, leaf, ca, &key.PublicKey, caKey)
		if err != nil {
			t.Fatal(err)
		}
		keyDER, err := x509.MarshalPKCS8PrivateKey(key)
		if err != nil {
			t.Fatal(err)
		}
		credentials, err := localmesh.NewCredentials(string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), rootPEM, string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})))
		if err != nil {
			t.Fatal(err)
		}
		return credentials
	}
	return issue(2, "00000000-0000-4000-8000-000000000041"), issue(3, "00000000-0000-4000-8000-000000000042")
}
