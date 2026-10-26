package meshsharing

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
	"io"
	"math/big"
	"net"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
)

func controlTestCredentials(t *testing.T) func(string, string) *localmesh.Credentials {
	t.Helper()
	factory, _ := controlTestCredentialsWithRoot(t)
	return factory
}

func controlTestCredentialsWithRoot(t *testing.T) (func(string, string) *localmesh.Credentials, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "lease test CA"}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign}
	der, err := x509.CreateCertificate(rand.Reader, ca, ca, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	root := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	return func(tenant, device string) *localmesh.Credentials {
		t.Helper()
		leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		uri, _ := url.Parse("spiffe://wendy.sh/tenant/" + tenant + "/device/" + device)
		leaf := &x509.Certificate{SerialNumber: big.NewInt(2), NotBefore: ca.NotBefore, NotAfter: ca.NotAfter, URIs: []*url.URL{uri}, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth}}
		der, err := x509.CreateCertificate(rand.Reader, leaf, ca, &leafKey.PublicKey, key)
		if err != nil {
			t.Fatal(err)
		}
		keyDER, err := x509.MarshalECPrivateKey(leafKey)
		if err != nil {
			t.Fatal(err)
		}
		creds, err := localmesh.NewCredentials(string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), root, string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})))
		if err != nil {
			t.Fatal(err)
		}
		return creds
	}, root
}

const controlTestTenant = "11111111-1111-4111-8111-111111111111"
const controlTestClient = "00000000-0000-4000-8000-000000000001"
const controlTestGateway = "00000000-0000-4000-8000-000000000002"

type controlTestBackend struct {
	grants    atomic.Int32
	granted   chan meshid.Device
	revoked   chan meshid.Device
	refusal   error
	leaseGone chan struct{}
}

func (b *controlTestBackend) LeaseDone(meshid.Device, tunnelLease) <-chan struct{} {
	return b.leaseGone
}

func (b *controlTestBackend) Grant(_ context.Context, peer meshid.Device) (tunnelLease, error) {
	b.grants.Add(1)
	if b.refusal != nil {
		return tunnelLease{}, b.refusal
	}
	b.granted <- peer
	return tunnelLease{Slot: 257, IPv6: true}, nil
}
func (b *controlTestBackend) Revoke(ctx context.Context, peer meshid.Device, lease tunnelLease) error {
	if ctx.Err() != nil || lease.Slot != 257 {
		return errors.New("invalid cleanup context or lease")
	}
	b.revoked <- peer
	return nil
}

type controlAddressConn struct {
	net.Conn
	peer net.Addr
}

func (c controlAddressConn) RemoteAddr() net.Addr { return c.peer }

func controlPipe(t *testing.T, server, client *localmesh.Credentials, backend *controlTestBackend, source meshid.Device) (context.CancelFunc, net.Conn, <-chan error) {
	t.Helper()
	left, right := net.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	finished := make(chan error, 1)
	go func() {
		finished <- serveGatewayLease(ctx, server, backend, controlAddressConn{right, &net.TCPAddr{IP: net.IP(source.MeshAddress().AsSlice()), Port: 12345}})
	}()
	t.Cleanup(func() { cancel(); left.Close(); right.Close() })
	return cancel, left, finished
}
func newControlBackend() *controlTestBackend {
	return &controlTestBackend{granted: make(chan meshid.Device, 1), revoked: make(chan meshid.Device, 1)}
}

func TestGatewayLeaseMutualTLSHeartbeatAndWithdrawal(t *testing.T) {
	makeCreds := controlTestCredentials(t)
	client := makeCreds(controlTestTenant, controlTestClient)
	gateway := makeCreds(controlTestTenant, controlTestGateway)
	backend := newControlBackend()
	stopServer, raw, done := controlPipe(t, gateway, client, backend, client.Device)
	dialCtx, cancelDial := context.WithCancel(context.Background())
	conn, lease, err := negotiateGatewayLease(dialCtx, client, gateway.Device, raw)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	cancelDial() // A short reconcile context must not own the returned lease.
	if !lease.IPv6 || lease.Slot != 257 || <-backend.granted != client.Device {
		t.Fatal("wrong lease identity or addresses")
	}
	conn.SetDeadline(time.Now().Add(time.Second))
	if err := writeControl(conn, []byte{leaseHeartbeat}); err != nil {
		t.Fatal(err)
	}
	var ack [1]byte
	if _, err := io.ReadFull(conn, ack[:]); err != nil || ack[0] != leaseHeartbeat {
		t.Fatalf("heartbeat: %v %v", ack, err)
	}
	stopServer()
	select {
	case peer := <-backend.revoked:
		if peer != client.Device {
			t.Fatal("wrong revoked peer")
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled server retained lease")
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestGatewayLeaseRejectsIdentityAndEndpointConfusion(t *testing.T) {
	for _, mode := range []string{"wrong tenant", "self", "wrong gateway pin", "wrong source", "wrong ALPN", "disabled gateway"} {
		t.Run(mode, func(t *testing.T) {
			makeCreds := controlTestCredentials(t)
			client := makeCreds(controlTestTenant, controlTestClient)
			gateway := makeCreds(controlTestTenant, controlTestGateway)
			source, target := client.Device, gateway.Device
			backend := newControlBackend()
			switch mode {
			case "wrong tenant":
				client = makeCreds("22222222-2222-4222-8222-222222222222", controlTestClient)
				source = client.Device
			case "self":
				client = gateway
				source = client.Device
			case "wrong gateway pin":
				target = makeCreds(controlTestTenant, "00000000-0000-4000-8000-000000000003").Device
			case "wrong source":
				source = gateway.Device
			case "disabled gateway":
				backend.refusal = errors.New("sharing disabled")
			}
			_, raw, done := controlPipe(t, gateway, client, backend, source)
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			var err error
			if mode == "wrong ALPN" {
				cfg, e := client.PeerTLS(gateway.Device)
				if e != nil {
					t.Fatal(e)
				}
				cfg.NextProtos = []string{"unrelated/1"}
				err = tls.Client(raw, cfg).HandshakeContext(ctx)
			} else {
				_, _, err = negotiateGatewayLease(ctx, client, target, raw)
			}
			if err == nil {
				t.Fatal("untrusted control session admitted")
			}
			raw.Close()
			select {
			case e := <-done:
				if e != nil {
					t.Fatal(e)
				}
			case <-time.After(time.Second):
				t.Fatal("failed handshake leaked handler")
			}
			expected := int32(0)
			if mode == "disabled gateway" {
				expected = 1
			}
			if backend.grants.Load() != expected {
				t.Fatalf("unexpected allocation attempts: %d", backend.grants.Load())
			}
			if len(backend.revoked) != 0 {
				t.Fatal("ungranted lease revoked")
			}
		})
	}
}

func TestGatewayLeaseExpiresEvenWhenCallerExtendsDeadline(t *testing.T) {
	left, right := net.Pipe()
	defer left.Close()
	defer right.Close()
	conn := &expiryConn{Conn: left, expires: time.Now().Add(30 * time.Millisecond)}
	if err := conn.SetDeadline(time.Time{}); err != nil {
		t.Fatal(err)
	}
	if err := conn.SetReadDeadline(time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	var b [1]byte
	_, err := conn.Read(b[:])
	if e, ok := err.(net.Error); !ok || !e.Timeout() {
		t.Fatalf("credential lifetime did not cap read: %v", err)
	}
}

func TestGatewayLeasePolicyWithdrawalClosesIdleControl(t *testing.T) {
	makeCreds := controlTestCredentials(t)
	client := makeCreds(controlTestTenant, controlTestClient)
	gateway := makeCreds(controlTestTenant, controlTestGateway)
	backend := newControlBackend()
	backend.leaseGone = make(chan struct{})
	_, raw, done := controlPipe(t, gateway, client, backend, client.Device)
	conn, _, err := negotiateGatewayLease(context.Background(), client, gateway.Device, raw)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	close(backend.leaseGone)
	conn.SetReadDeadline(time.Now().Add(time.Second))
	var b [1]byte
	if _, err := conn.Read(b[:]); err == nil {
		t.Fatal("withdrawn gateway retained control connection")
	} else if e, ok := err.(net.Error); ok && e.Timeout() {
		t.Fatal("withdrawal waited for heartbeat timeout")
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("withdrawn lease did not clean up")
	}
	if len(backend.revoked) != 1 {
		t.Fatal("withdrawn control session skipped revoke")
	}
}

// A mesh leaf may need more than the LAN's old eight-second allowance just
// to transmit certificates. Delay the client without weakening mTLS or grants.
func TestGatewayLeaseSlowRadioHandshake(t *testing.T) {
	makeCreds := controlTestCredentials(t)
	client := makeCreds(controlTestTenant, controlTestClient)
	gateway := makeCreds(controlTestTenant, controlTestGateway)
	backend := newControlBackend()
	stop, raw, done := controlPipe(t, gateway, client, backend, client.Device)
	time.Sleep(9 * time.Second)
	if backend.grants.Load() != 0 {
		t.Fatal("lease granted before authentication")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	secure, lease, err := negotiateGatewayLease(ctx, client, gateway.Device, raw)
	if err != nil {
		t.Fatal(err)
	}
	defer secure.Close()
	if !lease.valid() {
		t.Fatal("invalid lease")
	}
	if backend.grants.Load() != 1 {
		t.Fatal("authenticated leaf received no lease")
	}
	stop()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancellation did not close the lease")
	}
	if <-backend.revoked != client.Device {
		t.Fatal("wrong revoked identity")
	}
}
