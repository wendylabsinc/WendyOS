package meshsession

import (
	"bytes"
	"context"
	"crypto/tls"
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wendylabsinc/WendyOS/go/babel"
	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"github.com/wendylabsinc/wendy/go/internal/shared/directstream"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
)

// The actor sees bounded, copy-owning carrier queues, with independent real TLS
// exporters per incarnation. Dropped records model accepted writes lost on air.
type directTestCarrier struct {
	tx, rx      chan []byte
	drop        atomic.Uint32
	dropRest    atomic.Bool
	droppedRest atomic.Bool
	dropped     chan struct{}
}

func (c *directTestCarrier) TrySendDirectRecord(b []byte) (bool, error) {
	if c.droppedRest.Load() {
		return true, nil
	}
	if c.drop.CompareAndSwap(uint32(b[0]), 0) {
		if c.dropRest.Load() {
			c.droppedRest.Store(true)
		}
		select {
		case c.dropped <- struct{}{}:
		default:
		}
		return true, nil
	}
	select {
	case c.tx <- bytes.Clone(b):
		return true, nil
	default:
		return false, nil
	}
}
func (c *directTestCarrier) ReceiveDirectRecord(ctx context.Context) ([]byte, error) {
	select {
	case b := <-c.rx:
		return b, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
func directTestTLS(t *testing.T) (tls.ConnectionState, tls.ConnectionState) {
	t.Helper()
	a, b := fixtureCredentials(t)
	return directTestTLSWithCredentials(t, a, b)
}
func directTestTLSWithCredentials(t *testing.T, a, b *localmesh.Credentials) (tls.ConnectionState, tls.ConnectionState) {
	t.Helper()
	cc, err := a.PeerTLS(b.Device)
	if err != nil {
		t.Fatal(err)
	}
	sc, err := b.PeerTLS(a.Device)
	if err != nil {
		t.Fatal(err)
	}
	cc.NextProtos = []string{localmesh.StreamALPN}
	sc.NextProtos = cc.NextProtos
	x, y := net.Pipe()
	defer x.Close()
	defer y.Close()
	client, server := tls.Client(x, cc), tls.Server(y, sc)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- server.HandshakeContext(ctx) }()
	if err = client.HandshakeContext(ctx); err != nil {
		t.Fatal(err)
	}
	if err = <-done; err != nil {
		t.Fatal(err)
	}
	return client.ConnectionState(), server.ConnectionState()
}
func directTestAttach(t *testing.T, a, b *DirectSession, initialDrop ...[2]uint32) (*directTestCarrier, *directTestCarrier, func()) {
	t.Helper()
	return directTestAttachWithCost(t, a, b, 8192, 1, initialDrop...)
}
func directTestAttachWithCost(t *testing.T, a, b *DirectSession, cost uint16, link babel.LinkID, initialDrop ...[2]uint32) (*directTestCarrier, *directTestCarrier, func()) {
	return directTestAttachNegotiated(t, a, b, cost, link, directstream.CapabilityVersion|directstream.CapabilityTLS, initialDrop...)
}

func directTestAttachNegotiated(t *testing.T, a, b *DirectSession, cost uint16, link babel.LinkID, selected uint32, initialDrop ...[2]uint32) (*directTestCarrier, *directTestCarrier, func()) {
	t.Helper()
	cs, ss := directTestTLS(t)
	x, y := make(chan []byte, 2), make(chan []byte, 2)
	ca, cb := &directTestCarrier{tx: x, rx: y, dropped: make(chan struct{}, 1)}, &directTestCarrier{tx: y, rx: x, dropped: make(chan struct{}, 1)}
	if len(initialDrop) > 0 {
		ca.drop.Store(initialDrop[0][0])
		cb.drop.Store(initialDrop[0][1])
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 2)
	for i, s := range []*DirectSession{a, b} {
		state, carrier := cs, ca
		if i == 1 {
			state, carrier = ss, cb
		}
		admission := localmesh.DirectCarrierAdmission{Cost: cost, Link: link, Peer: s.cfg.Peer, IdentityDigest: s.cfg.IdentityDigest, Role: s.cfg.Role, Selected: selected, TLS: state, Records: carrier}
		go func() { done <- s.Attach(ctx, admission) }()
	}
	stopped := false
	stop := func() {
		if stopped {
			return
		}
		stopped = true
		cancel()
		for range 2 {
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				t.Error("carrier reader did not join")
			}
		}
	}
	t.Cleanup(stop)
	return ca, cb, stop
}
func directTestSessions(t *testing.T, backend DirectBackend, timeout time.Duration, datagramBytes ...int) (*DirectSession, *DirectSession) {
	t.Helper()
	var sessions [2]*DirectSession
	for i, peer := range []string{fixtureDeviceB, fixtureDeviceA} {
		budget := 0
		if len(datagramBytes) != 0 {
			budget = datagramBytes[0]
		}
		s, err := NewDirectSession(context.Background(), DirectSessionConfig{DatagramBytes: budget, DatagramMaximum: 1024, Peer: fixtureDevice(t, peer), Role: byte(i), IdentityDigest: [32]byte{7}, Deadline: time.Now().Add(time.Minute), Backend: backend, RecoveryTimeout: timeout, Slots: 4, TXBytes: 8192, RXBytes: 8192, Window: 4096, Initial: 1024})
		if err != nil {
			t.Fatal(err)
		}
		sessions[i] = s
		t.Cleanup(s.Close)
	}
	return sessions[0], sessions[1]
}
func TestDirectSessionRecoversSameApplicationSocket(t *testing.T) {
	for _, kind := range []byte{byte(directstream.FrameData), byte(directstream.FrameState)} {
		t.Run(map[byte]string{byte(directstream.FrameData): "lost-data", byte(directstream.FrameState): "lost-ack"}[kind], func(t *testing.T) {
			var opens atomic.Int32
			endpoints := make(chan net.Conn, 1)
			backend := func(ctx context.Context, _ meshid.Device, r directstream.OpenRequest) (net.Conn, error) {
				opens.Add(1)
				x, y := net.Pipe()
				endpoints <- y
				return x, nil
			}
			a, b := directTestSessions(t, backend, time.Second*3)
			ca, cb, cut := directTestAttach(t, a, b)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			app, err := a.Dial(ctx, directstream.OpenRequest{Service: directstream.ServiceTCP, Port: 28080})
			if err != nil {
				t.Fatal(err)
			}
			defer app.Close()
			endpoint := <-endpoints
			defer endpoint.Close()
			endpoint.SetDeadline(time.Now().Add(5 * time.Second))
			drop := ca
			if kind == byte(directstream.FrameState) {
				drop = cb
			}
			drop.drop.Store(uint32(kind))
			prefix := []byte("before carrier loss")
			if _, err = app.Write(prefix); err != nil {
				t.Fatal(err)
			}
			got := make([]byte, len(prefix))
			if kind == byte(directstream.FrameState) {
				if _, err = io.ReadFull(endpoint, got); err != nil {
					t.Fatal(err)
				}
			}
			select {
			case <-drop.dropped:
			case <-ctx.Done():
				t.Fatal("record never sent")
			}
			cut()
			suffix := []byte(" while disconnected")
			if _, err = app.Write(suffix); err != nil {
				t.Fatal(err)
			}
			directTestAttach(t, a, b)
			want := suffix
			if kind == byte(directstream.FrameData) {
				want = append(prefix, suffix...)
			}
			got = make([]byte, len(want))
			if _, err = io.ReadFull(endpoint, got); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, want) {
				t.Fatalf("replay = %q want %q", got, want)
			}
			if opens.Load() != 1 {
				t.Fatalf("backend dialled %d times", opens.Load())
			}
			// A response on the original socket also crosses the recovered attachment.
			response := bytes.Repeat([]byte("reply"), 3000)
			sent := make(chan error, 1)
			go func() { _, err := endpoint.Write(response); sent <- err }()
			app.SetReadDeadline(time.Now().Add(5 * time.Second))
			got = make([]byte, len(response))
			if _, err = io.ReadFull(app, got); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, response) {
				t.Fatal("response corrupt")
			}
			if err = <-sent; err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestDirectSessionPendingBackendSurvivesCarrierLoss(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	var opens atomic.Int32
	endpoints := make(chan net.Conn, 1)
	backend := func(ctx context.Context, _ meshid.Device, _ directstream.OpenRequest) (net.Conn, error) {
		opens.Add(1)
		close(started)
		select {
		case <-release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		x, y := net.Pipe()
		endpoints <- y
		return x, nil
	}
	a, b := directTestSessions(t, backend, 3*time.Second)
	_, _, cut := directTestAttach(t, a, b)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result := make(chan directReply, 1)
	go func() {
		c, err := a.Dial(ctx, directstream.OpenRequest{Service: directstream.ServiceTCP, Port: 28080})
		result <- directReply{c, err}
	}()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("backend not started")
	}
	cut()
	close(release)
	directTestAttach(t, a, b)
	var reply directReply
	select {
	case reply = <-result:
	case <-ctx.Done():
		t.Fatal("dial not recovered")
	}
	if reply.err != nil {
		t.Fatal(reply.err)
	}
	defer reply.conn.Close()
	endpoint := <-endpoints
	defer endpoint.Close()
	if opens.Load() != 1 {
		t.Fatal("pending backend redialled")
	}
	endpoint.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := reply.conn.Write([]byte("recovered")); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, 9)
	if _, err := io.ReadFull(endpoint, got); err != nil {
		t.Fatal(err)
	}
	if string(got) != "recovered" {
		t.Fatal(string(got))
	}
}

func TestDirectSessionOutageDeadlineClosesExistingSocket(t *testing.T) {
	endpoints := make(chan net.Conn, 1)
	backend := func(context.Context, meshid.Device, directstream.OpenRequest) (net.Conn, error) {
		x, y := net.Pipe()
		endpoints <- y
		return x, nil
	}
	a, b := directTestSessions(t, backend, 80*time.Millisecond)
	_, _, cut := directTestAttach(t, a, b)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	app, err := a.Dial(ctx, directstream.OpenRequest{Service: directstream.ServiceTCP, Port: 28080})
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	endpoint := <-endpoints
	defer endpoint.Close()
	cut()
	select {
	case <-a.done:
	case <-ctx.Done():
		t.Fatal("outage did not expire")
	}
	if _, err = app.Write([]byte("after deadline")); err == nil {
		t.Fatal("expired session still writable")
	}
	endpoint.SetReadDeadline(time.Now().Add(time.Second))
	if _, err = endpoint.Read(make([]byte, 1)); err != io.EOF {
		t.Fatalf("backend not closed: %v", err)
	}
}

func TestDirectSessionHalfClosePreservesResponse(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	backend := func(ctx context.Context, _ meshid.Device, _ directstream.OpenRequest) (net.Conn, error) {
		var dial net.Dialer
		return dial.DialContext(ctx, "tcp", listener.Addr().String())
	}
	a, b := directTestSessions(t, backend, time.Second)
	directTestAttach(t, a, b)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	app, err := a.Dial(ctx, directstream.OpenRequest{Service: directstream.ServiceTCP, Port: 28080})
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	endpoint, err := listener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer endpoint.Close()
	app.SetDeadline(time.Now().Add(2 * time.Second))
	endpoint.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err = app.Write([]byte("request")); err != nil {
		t.Fatal(err)
	}
	if err = app.(interface{ CloseWrite() error }).CloseWrite(); err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(endpoint)
	if err != nil || string(got) != "request" {
		t.Fatalf("request FIN: %q %v", got, err)
	}
	if _, err = endpoint.Write([]byte("response after request FIN")); err != nil {
		t.Fatal(err)
	}
	if err = endpoint.(*net.TCPConn).CloseWrite(); err != nil {
		t.Fatal(err)
	}
	got, err = io.ReadAll(app)
	if err != nil || string(got) != "response after request FIN" {
		t.Fatalf("response FIN: %q %v", got, err)
	}
}

type revokedDirectBackend struct {
	net.Conn
	revoked <-chan struct{}
}

func (c revokedDirectBackend) Done() <-chan struct{} { return c.revoked }
func TestDirectSessionUDPRecoveryKeepsAssociation(t *testing.T) {
	listener, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	var opens atomic.Int32
	backend := func(ctx context.Context, _ meshid.Device, r directstream.OpenRequest) (net.Conn, error) {
		if r.Service != directstream.ServiceUDP || r.DatagramSize != 1024 {
			return nil, ErrDenied
		}
		opens.Add(1)
		return net.DialUDP("udp4", nil, listener.LocalAddr().(*net.UDPAddr))
	}
	a, b := directTestSessions(t, backend, 3*time.Second, 6*1024)
	ca, _, cut := directTestAttach(t, a, b)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	app, err := a.Dial(ctx, directstream.OpenRequest{Service: directstream.ServiceUDP, Port: 28080, DatagramSize: 1024})
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	var endpoint string
	round := func(payload []byte) {
		t.Helper()
		app.SetDeadline(time.Now().Add(time.Second))
		listener.SetDeadline(time.Now().Add(time.Second))
		if _, err := app.Write(payload); err != nil {
			t.Fatal(err)
		}
		buffer := make([]byte, 2048)
		n, peer, err := listener.ReadFromUDP(buffer)
		if err != nil || !bytes.Equal(buffer[:n], payload) {
			t.Fatalf("backend %q %v", buffer[:n], err)
		}
		if endpoint != "" && endpoint != peer.String() {
			t.Fatal("backend association replaced")
		}
		endpoint = peer.String()
		if _, err := listener.WriteToUDP(payload, peer); err != nil {
			t.Fatal(err)
		}
		n, err = app.Read(buffer)
		if err != nil || !bytes.Equal(buffer[:n], payload) {
			t.Fatalf("reply %q %v", buffer[:n], err)
		}
	}
	round(nil)
	round(bytes.Repeat([]byte("a"), 900))
	ca.drop.Store(uint32(directstream.FrameDatagram))
	if _, err := app.Write(bytes.Repeat([]byte("lost"), 200)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ca.dropped:
	case <-ctx.Done():
		t.Fatal("no datagram frame")
	}
	cut()
	directTestAttach(t, a, b)
	// OPEN's refusal is a round-trip fence after recovery commits. UDP writes
	// during the exchange intentionally drop rather than queue for replay.
	if fence, err := a.Dial(ctx, directstream.OpenRequest{Service: directstream.ServiceTCP, Port: 1}); err == nil {
		fence.Close()
		t.Fatal("unexpected fence admission")
	}
	round([]byte("after recovery"))
	round(nil)
	if opens.Load() != 1 {
		t.Fatalf("backend opened %d times", opens.Load())
	}
}
