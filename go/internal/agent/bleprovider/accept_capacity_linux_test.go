//go:build linux

package bleprovider

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
	"go.uber.org/zap"
	"golang.org/x/sys/unix"
)

type capacityTestNode struct{ attached chan meshid.Device }

func (n *capacityTestNode) AttachStream(ctx context.Context, peer meshid.Device, _ net.Conn, _ uint16) error {
	n.attached <- peer
	<-ctx.Done()
	return ctx.Err()
}

// Exercise the real accept loop, packet adapter, TLS admission and peer claims.
// Unix sequenced packets substitute only for the Bluetooth listener's radio;
// distinct fixture keys prove possession, while PKI trust has separate suites.
func TestAcceptLoopHonorsConfiguredPeerCapacity(t *testing.T) {
	const target = localmesh.BLEMaxConnections
	self := fixtureDevice("00000001-1234-4000-8000-000000000001")
	serverCert, _ := testTLSIdentity(t)
	certificates := make([]tls.Certificate, target)
	devices := make([]meshid.Device, target)
	identities := make(map[string]meshid.Device, target)
	for i := range target {
		certificates[i], _ = testTLSIdentity(t)
		id := self.UUID()
		id[0] = byte(i + 1)
		devices[i] = meshid.NewDevice(testTenant, id)
		identities[string(certificates[i].Certificate[0])] = devices[i]
	}
	credentials := &localmesh.Credentials{Device: self, Certificate: serverCert, Verify: func(chain [][]byte, _ time.Time) (localmesh.Identity, error) {
		if len(chain) == 1 {
			if peer, ok := identities[string(chain[0])]; ok {
				return localmesh.Identity{Device: peer}, nil
			}
		}
		return localmesh.Identity{}, errors.New("untrusted fixture certificate")
	}}
	node := &capacityTestNode{attached: make(chan meshid.Device, target)}
	r := &runtime{cfg: Config{Credentials: credentials, Node: node, NetworkName: "capacity-test", Logger: zap.NewNop()},
		advertisementRefresh: make(chan struct{}, 1)}
	if err := r.cfg.defaults(); err != nil {
		t.Fatal(err)
	}
	r.serverTLS = r.makeServerTLS()
	fd, err := unix.Socket(unix.AF_UNIX, unix.SOCK_SEQPACKET|unix.SOCK_NONBLOCK|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	listener := &l2Listener{fd: fd}
	defer listener.Close()
	address := &unix.SockaddrUnix{Name: "@wendy-ble-capacity-" + uuid.NewString()}
	if err := unix.Bind(fd, address); err != nil {
		t.Fatal(err)
	}
	if err := unix.Listen(fd, target); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	done := make(chan error, 1)
	go func() { done <- r.acceptLoop(ctx, listener) }()
	t.Cleanup(func() {
		cancel()
		_ = listener.Close()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("accept loop did not stop")
		}
		r.links.Wait()
	})
	for i := range target {
		clientFD, err := unix.Socket(unix.AF_UNIX, unix.SOCK_SEQPACKET|unix.SOCK_NONBLOCK|unix.SOCK_CLOEXEC, 0)
		if err != nil {
			t.Fatal(err)
		}
		raw := newPacketConn(clientFD, l2addr{}, l2addr{})
		t.Cleanup(func() { _ = raw.Close() })
		if err := unix.Connect(clientFD, address); err != nil {
			t.Fatal(err)
		}
		client := tls.Client(raw, &tls.Config{MinVersion: tls.VersionTLS13, MaxVersion: tls.VersionTLS13,
			Certificates: []tls.Certificate{certificates[i]}, InsecureSkipVerify: true, // Fixture server; production trust is exercised separately.
			NextProtos: []string{ALPN}, CurvePreferences: []tls.CurveID{tls.X25519}})
		if err := client.HandshakeContext(ctx); err != nil {
			t.Fatalf("configured peer %d of %d could not authenticate: %v", i+1, target, err)
		}
		select {
		case peer := <-node.attached:
			if peer != devices[i] {
				t.Fatal("wrong authenticated peer attached")
			}
		case <-ctx.Done():
			t.Fatalf("configured peer %d of %d was not attached", i+1, target)
		}
	}
}
