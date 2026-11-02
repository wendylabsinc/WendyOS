package localmesh

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/wendylabsinc/WendyOS/go/babel"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
)

// This backend runs the production Node/Babel pipeline without TUN, netlink,
// sockets, radios, or host-clock changes. A carrier's TUN read blocks until its
// link closes; received routed IP writes are discarded, never reflected.
type staleClockPacketBackend struct {
	mu    sync.Mutex
	peers map[babel.LinkID]*staleClockTUN
}
type staleClockTUN struct {
	reader *io.PipeReader
	writer *io.PipeWriter
}

func (p *staleClockTUN) Read(b []byte) (int, error)   { return p.reader.Read(b) }
func (*staleClockTUN) Write(b []byte) (int, error)    { return len(b), nil }
func (p *staleClockTUN) Close() error                 { return errors.Join(p.reader.Close(), p.writer.Close()) }
func (*staleClockPacketBackend) ReconcileBase() error { return nil }
func (p *staleClockPacketBackend) AddPeer(id babel.LinkID, _ meshid.Device) (io.ReadWriteCloser, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.peers == nil {
		p.peers = map[babel.LinkID]*staleClockTUN{}
	}
	r, w := io.Pipe()
	tun := &staleClockTUN{r, w}
	p.peers[id] = tun
	return tun, nil
}
func (p *staleClockPacketBackend) RemovePeer(id babel.LinkID) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if tun := p.peers[id]; tun != nil {
		delete(p.peers, id)
		return tun.Close()
	}
	return nil
}
func (*staleClockPacketBackend) Apply([]babel.Route) error { return nil }
func (p *staleClockPacketBackend) Stop() error             { return p.Close() }
func (p *staleClockPacketBackend) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	for id, tun := range p.peers {
		_ = tun.Close()
		delete(p.peers, id)
	}
	return nil
}

type staleClockNodeRun struct {
	node    *Node
	cancel  context.CancelFunc
	done    chan error
	stopped bool
}

func runStaleClockNode(t *testing.T, dir string, credentials *Credentials) *staleClockNodeRun {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	n, err := NewNodeWithNetwork(ctx, dir, credentials, "clock-regression", 50052, &staleClockPacketBackend{}, []AppEndpoint{{Transport: AppTLS, Port: AppSessionTLSPort}})
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	n.advertisement.Services = []ManifestService{{Mesh: "lab", AppID: "sh.wendy.fixture", ServiceID: "sensor", Type: "_sensorlink._tcp", Instance: "Clock fixture", Port: 28080}}
	r := &staleClockNodeRun{node: n, cancel: cancel, done: make(chan error, 1)}
	go func() { r.done <- n.Run() }()
	t.Cleanup(func() { r.stop(t) })
	return r
}
func (r *staleClockNodeRun) stop(t *testing.T) {
	t.Helper()
	if r.stopped {
		return
	}
	r.stopped = true
	r.cancel()
	select {
	case err := <-r.done:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("Node stopped: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Error("Node did not stop")
	}
}

// net.Pipe has no send buffer, while the symmetric production hello writes
// before reading. This bounded eight-record buffer models ordinary stream
// buffering without opening a host socket.
type staleClockBufferedConn struct {
	net.Conn
	out  chan []byte
	done chan struct{}
	once sync.Once
}

func bufferedStaleClockConn(raw net.Conn) *staleClockBufferedConn {
	c := &staleClockBufferedConn{Conn: raw, out: make(chan []byte, 8), done: make(chan struct{})}
	go func() {
		for {
			select {
			case <-c.done:
				return
			case b := <-c.out:
				if _, err := io.Copy(c.Conn, bytes.NewReader(b)); err != nil {
					_ = c.Close()
					return
				}
			}
		}
	}()
	return c
}
func (c *staleClockBufferedConn) Write(p []byte) (int, error) {
	if len(p) > 32<<10 {
		return 0, errors.New("fixture TLS record exceeds buffer bound")
	}
	b := append([]byte(nil), p...)
	select {
	case <-c.done:
		return 0, net.ErrClosed
	case c.out <- b:
		return len(p), nil
	}
}
func (c *staleClockBufferedConn) Close() error {
	var err error
	c.once.Do(func() { close(c.done); err = c.Conn.Close() })
	return err
}

// TLS runs over net.Pipe: the real pinned certificate verifier and Node stream
// hello/control/gossip/Babel readers still execute, with no OS network traffic.
func connectStaleClockNodes(t *testing.T, a, b *Node) func() {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	aConfig, err := a.Credentials.PeerTLS(b.Credentials.Device)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	bConfig, err := b.Credentials.PeerTLS(a.Credentials.Device)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	aConfig.NextProtos = []string{StreamALPN}
	bConfig.NextProtos = []string{StreamALPN}
	rawA, rawB := net.Pipe()
	bufferA, bufferB := bufferedStaleClockConn(rawA), bufferedStaleClockConn(rawB)
	client, server := tls.Client(bufferA, aConfig), tls.Server(bufferB, bConfig)
	handshakes := make(chan error, 2)
	go func() { handshakes <- client.HandshakeContext(ctx) }()
	go func() { handshakes <- server.HandshakeContext(ctx) }()
	for range 2 {
		if err := <-handshakes; err != nil {
			cancel()
			_ = bufferA.Close()
			_ = bufferB.Close()
			t.Fatal("in-memory TLS", err)
		}
	}
	ends := make(chan error, 2)
	go func() {
		err := a.AttachStream(ctx, b.Credentials.Device, client, 256)
		if ctx.Err() == nil {
			t.Log("A stream ended early", err)
		}
		ends <- err
	}()
	go func() {
		err := b.AttachStream(ctx, a.Credentials.Device, server, 256)
		if ctx.Err() == nil {
			t.Log("B stream ended early", err)
		}
		ends <- err
	}()
	var once sync.Once
	closeLink := func() {
		once.Do(func() {
			cancel()
			_ = bufferA.Close()
			_ = bufferB.Close()
			for range 2 {
				select {
				case <-ends:
				case <-time.After(5 * time.Second):
					t.Error("in-memory carrier did not stop")
				}
			}
		})
	}
	t.Cleanup(closeLink)
	return closeLink
}

func waitStaleClockSnapshot(t *testing.T, node *Node, want int) NodeSnapshot {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		s := node.Snapshot()
		if len(s.Devices) == want {
			return s
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("Node did not see %d devices: %+v", want, node.Snapshot())
	return NodeSnapshot{}
}

func waitStaleClockForwarding(t *testing.T, node *Node, peer meshid.Device) {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		s := node.Snapshot()
		for _, r := range s.ForwardingRoutes {
			if !r.Local && !r.Unreachable && r.Prefix.Addr() == peer.MeshAddress() {
				return
			}
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("signed proof did not produce a live Babel forwarding route", node.Snapshot())
}

// A 598-second old RTC and saved local floor must not stop two authenticated
// Nodes from exchanging their unchanged signed v4 announcements. No timesync
// implementation or peer-clock control protocol is installed by this test.
func TestNodesJoinWithStaleLocalClockBeforeAnyTimeCorrection(t *testing.T) {
	start := time.Now()
	wallStart := start.Add(-598 * time.Second)
	staleNow := func() time.Time { return wallStart.Add(time.Since(start)) }
	stalePolicy := certs.CertificateTimePolicy{Now: staleNow, Floor: staleNow, Uncertain: func() bool { return true }}
	healthyPolicy := certs.CertificateTimePolicy{Now: time.Now, Uncertain: func() bool { return false }}
	makeCredentials := func(principal string, policy certs.CertificateTimePolicy) *Credentials {
		notBefore := start.Add(-time.Hour)
		if principal == testPrincipal(testTenantUUID, testDeviceBID) {
			notBefore = start.Add(-4 * time.Minute)
		}
		cert, key := issuePolicyLeaf(t, principal, notBefore, start.Add(time.Hour))
		c, err := NewCredentialsWithTimePolicy(cert, testPKI().chainPEM, key, policy)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	aCredentials := makeCredentials(testPrincipal(testTenantUUID, testDeviceAID), stalePolicy)
	bCredentials := makeCredentials(testPrincipal(testTenantUUID, testDeviceBID), healthyPolicy)
	aDir := t.TempDir()
	a := runStaleClockNode(t, aDir, aCredentials)
	b := runStaleClockNode(t, t.TempDir(), bCredentials)
	waitStaleClockSnapshot(t, a.node, 1)
	waitStaleClockSnapshot(t, b.node, 1)
	closeLink := connectStaleClockNodes(t, a.node, b.node)
	for _, node := range []*Node{a.node, b.node} {
		snapshot := waitStaleClockSnapshot(t, node, 2)
		for _, m := range snapshot.Devices {
			if m.Route == nil || !snapshot.RouteAuthorizations[*m.Route] || len(m.Services) != 1 || len(m.AppEndpoints) != 1 {
				t.Fatal("authenticated announcement projection missing", m)
			}
			if remaining := m.LeaseRemaining(snapshot.CapturedAt); remaining < MaxLease-8*time.Second || remaining > MaxLease {
				t.Fatal("TTL used stale certificate UTC instead of receiver monotonic scheduling", remaining)
			}
			if m.Device == aCredentials.Device.UUID() {
				skew := time.UnixMilli(m.Issued).Sub(staleNow())
				if skew < -8*time.Second || skew > time.Second {
					t.Fatal("publisher did not preserve stale local UTC", skew)
				}
			}
		}
	}
	waitStaleClockForwarding(t, a.node, bCredentials.Device)
	waitStaleClockForwarding(t, b.node, aCredentials.Device)
	if skew := time.Since(staleNow()); skew < 597*time.Second || skew > 599*time.Second {
		t.Fatal("peer interaction corrected local UTC", skew)
	}
	closeLink()
	a.stop(t)
	previous, _, err := readPersistentState(a.node.store.path)
	if err != nil {
		t.Fatal(err)
	}
	// A process restart keeps the local durable origin revision but does not
	// restore foreign presence or invent new 150-minute RAM deadlines.
	restarted := runStaleClockNode(t, aDir, aCredentials)
	snapshot := waitStaleClockSnapshot(t, restarted.node, 1)
	if snapshot.Devices[0].Device != aCredentials.Device.UUID() || snapshot.Devices[0].Revision <= previous.Revision {
		t.Fatal("restart lost own revision or restored foreign presence", snapshot)
	}
	persisted, _, err := readPersistentState(restarted.node.store.path)
	if err != nil || persisted.Revision <= previous.Revision {
		t.Fatal("published origin revision was not durable", err)
	}
	if skew := time.Since(staleNow()); skew < 597*time.Second || skew > 599*time.Second {
		t.Fatal("restart corrected local UTC without direct time authority", skew)
	}
}
