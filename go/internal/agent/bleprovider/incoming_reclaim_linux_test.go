//go:build linux

package bleprovider

import (
	"context"
	"crypto/tls"
	"errors"
	"github.com/google/uuid"
	"github.com/wendylabsinc/WendyOS/go/babel"
	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
	"go.uber.org/zap"
	"golang.org/x/sys/unix"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type incomingReclaimNode struct {
	mu       sync.Mutex
	links    []localmesh.PeerLink
	next     atomic.Uint64
	attached chan meshid.Device
	retired  atomic.Int32
}

func (n *incomingReclaimNode) Snapshot() localmesh.NodeSnapshot {
	n.mu.Lock()
	defer n.mu.Unlock()
	return localmesh.NodeSnapshot{Links: append([]localmesh.PeerLink(nil), n.links...)}
}
func (n *incomingReclaimNode) AttachStream(ctx context.Context, peer meshid.Device, conn net.Conn, cost uint16) error {
	if !localmesh.ConnectionReady(ctx, peer, func() { conn.Close() }) {
		return nil
	}
	id := babel.LinkID(n.next.Add(1))
	retire := make(chan struct{})
	var once sync.Once
	localmesh.BindConnectionLink(ctx, id, cost, func() { once.Do(func() { n.retired.Add(1); close(retire) }) })
	n.mu.Lock()
	n.links = append(n.links, localmesh.PeerLink{ID: id, Device: peer, Cost: cost, Transport: "ble"})
	n.mu.Unlock()
	n.attached <- peer
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-retire:
		return nil
	}
}

func TestBLEIncomingFullBudgetReachesSafeOwnedReclamation(t *testing.T) {
	const target = localmesh.BLEMaxConnections
	self := fixtureDevice("00000001-1234-4000-8000-000000000001")
	serverCert, _ := testTLSIdentity(t)
	certificates := make([]tls.Certificate, target+1)
	devices := make([]meshid.Device, target+1)
	ids := map[string]meshid.Device{}
	for i := range certificates {
		certificates[i], _ = testTLSIdentity(t)
		id := self.UUID()
		id[0] = byte(i + 1)
		devices[i] = meshid.NewDevice(testTenant, id)
		ids[string(certificates[i].Certificate[0])] = devices[i]
	}
	creds := &localmesh.Credentials{Device: self, Certificate: serverCert, Verify: func(chain [][]byte, _ time.Time) (localmesh.Identity, error) {
		if len(chain) == 1 {
			if peer, ok := ids[string(chain[0])]; ok {
				return localmesh.Identity{Device: peer}, nil
			}
		}
		return localmesh.Identity{}, errors.New("untrusted fixture")
	}}
	node := &incomingReclaimNode{attached: make(chan meshid.Device, target+1), links: []localmesh.PeerLink{{ID: 100, Device: devices[0], Cost: 128, ForwardingReady: true}}}
	r := &runtime{cfg: Config{Credentials: creds, Node: node, NetworkName: "incoming-reclaim-fixture", Logger: zap.NewNop()}, advertisementRefresh: make(chan struct{}, 1)}
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
	address := &unix.SockaddrUnix{Name: "@wendy-review-reclaim-" + uuid.NewString()}
	if err = unix.Bind(fd, address); err != nil {
		t.Fatal(err)
	}
	if err = unix.Listen(fd, target+1); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	done := make(chan error, 1)
	go func() { done <- r.acceptLoop(ctx, listener) }()
	t.Cleanup(func() {
		cancel()
		listener.Close()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("accept loop did not stop")
		}
		r.links.Wait()
	})
	dial := func(i int, deadline context.Context) error {
		clientfd, err := unix.Socket(unix.AF_UNIX, unix.SOCK_SEQPACKET|unix.SOCK_NONBLOCK|unix.SOCK_CLOEXEC, 0)
		if err != nil {
			return err
		}
		raw := newPacketConn(clientfd, l2addr{}, l2addr{})
		t.Cleanup(func() { raw.Close() })
		if err = unix.Connect(clientfd, address); err != nil {
			return err
		}
		client := tls.Client(raw, &tls.Config{MinVersion: tls.VersionTLS13, MaxVersion: tls.VersionTLS13, Certificates: []tls.Certificate{certificates[i]}, InsecureSkipVerify: true, NextProtos: []string{ALPN}, CurvePreferences: []tls.CurveID{tls.X25519}})
		return client.HandshakeContext(deadline)
	}
	for i := 0; i < target; i++ {
		if err = dial(i, ctx); err != nil {
			t.Fatalf("initial handshake%d: %v", i, err)
		}
		select {
		case <-node.attached:
		case <-ctx.Done():
			t.Fatal("initial attach timeout")
		}
	}
	if counts := r.connectionPolicy().Counts(); counts.Total != target || counts.Ready != target {
		t.Fatalf("setup counts %+v", counts)
	}
	if !r.connectionPolicy().IncomingAvailable() {
		t.Fatal("fixture has no safe reclaimable incoming capacity")
	}
	probe, stop := context.WithTimeout(ctx, 500*time.Millisecond)
	defer stop()
	_ = dial(target, probe)
	deadline := time.Now().Add(300 * time.Millisecond)
	for node.retired.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if node.retired.Load() != 1 {
		t.Fatalf("advertised safe incoming capacity could not reach reclamation: retired=%d", node.retired.Load())
	}
	// The first probe never receives a fourth worker/lease. Only the old
	// incoming owner completing can make the next authenticated retry fit.
	deadline = time.Now().Add(time.Second)
	for r.connectionPolicy().Counts().Total == target && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if counts := r.connectionPolicy().Counts(); counts.Total != target-1 {
		t.Fatalf("retired incoming owner did not normally release: %+v", counts)
	}
	retried := false
	deadline = time.Now().Add(time.Second)
	for !retried && time.Now().Before(deadline) {
		attempt, stop := context.WithTimeout(ctx, 200*time.Millisecond)
		err = dial(target, attempt)
		stop()
		retried = err == nil
		if !retried {
			time.Sleep(5 * time.Millisecond)
		}
	}
	if !retried {
		t.Fatal("new peer never used the normally released incoming worker")
	}
	select {
	case got := <-node.attached:
		if got != devices[target] {
			t.Fatalf("wrong replacement peer: %v", got)
		}
	case <-ctx.Done():
		t.Fatal("replacement was not authenticated and attached")
	}
	if counts := r.connectionPolicy().Counts(); counts.Total != target || counts.Ready != target || node.retired.Load() != 1 {
		t.Fatalf("replacement exceeded caps or churned another owner: %+v retires=%d", counts, node.retired.Load())
	}
}
