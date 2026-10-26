package meshsession

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/netip"
	"testing"
	"time"

	quic "github.com/quic-go/quic-go"
	"github.com/wendylabsinc/wendy/go/internal/agent/meshingress"
	"github.com/wendylabsinc/wendy/go/internal/agent/udpmessage"
)

func TestUDPVersionAndCapacityNegotiation(t *testing.T) {
	a, b := fixtureCredentials(t)
	r := meshingress.NewRegistry()
	port := udpLifecycleApp(t, r, "echo")
	address := lifecycleServer(t, b, testUDPAuthorization{r}, true)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cfg, err := a.PeerTLSWithTickets(b.Device, ALPN, "app-quic")
	if err != nil {
		t.Fatal(err)
	}
	conn, err := quic.DialAddr(ctx, address.String(), cfg, quicConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseWithError(0, "done")
	setup := func(request []byte) []byte {
		t.Helper()
		s, err := conn.OpenStreamSync(ctx)
		if err != nil {
			t.Fatal(err)
		}
		_ = s.SetDeadline(time.Now().Add(time.Second))
		if _, err = s.Write(request); err != nil {
			t.Fatal(err)
		}
		ack := make([]byte, 16)
		if _, err = io.ReadFull(s, ack); err != nil {
			t.Fatal(err)
		}
		s.CancelRead(0)
		_ = s.Close()
		return ack
	}
	for _, mutate := range []func([]byte){func(b []byte) { b[6] = 1 }, func(b []byte) { binary.BigEndian.PutUint16(b[12:14], 255) }, func(b []byte) { b[15] = 1 }, func(b []byte) { binary.BigEndian.PutUint32(b[8:12], 1000) }} {
		hello := udpOpenRequest(port)
		mutate(hello)
		if ack := setup(hello); !errors.Is(validateUDPAck(ack), ErrUDPUnsupported) {
			t.Fatalf("incompatible offer accepted: %x", ack)
		}
	}
	hello := udpOpenRequest(port)
	binary.BigEndian.PutUint16(hello[12:14], 512)
	ack := setup(hello)
	if err := validateUDPAck(ack); err != nil || binary.BigEndian.Uint16(ack[6:8]) != 512 {
		t.Fatalf("negotiated ACK %x: %v", ack, err)
	}
	manager := udpmessage.NewManager()
	defer manager.Close()
	endpoint, err := manager.Open(b.Device, func(send func() error) error { return send() })
	if err != nil {
		t.Fatal(err)
	}
	if err := endpoint.SetTransport(512, false, quicUDPAdmission(conn)); err != nil {
		t.Fatal(err)
	}
	flow := &DatagramFlow{conn: conn, endpoint: endpoint}
	defer flow.Close()
	// 8192 requires 17 chunks at this negotiated cap, in each direction.
	p := bytes.Repeat([]byte{0x73}, 8192)
	if err := flow.Send(p); err != nil {
		t.Fatal(err)
	}
	got, err := readUDPMessage(flow, ctx)
	if err != nil || !bytes.Equal(got, p) {
		t.Fatalf("negotiated echo %d: %v", len(got), err)
	}
}

func TestUDPRejectsMalformedSelectedCapabilities(t *testing.T) {
	b := make([]byte, 16)
	copy(b, "WUA2")
	b[4], b[5] = 1, 2
	binary.BigEndian.PutUint16(b[6:8], 1000)
	binary.BigEndian.PutUint32(b[8:12], MaxUDPMessageSize)
	if err := validateUDPAck(b); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func([]byte){func(b []byte) { b[3] = '1' }, func(b []byte) { b[5] = 1 }, func(b []byte) { binary.BigEndian.PutUint16(b[6:8], 255) }, func(b []byte) { binary.BigEndian.PutUint16(b[6:8], 1001) }, func(b []byte) { b[15] = 1 }, func(b []byte) { binary.BigEndian.PutUint32(b[8:12], 1000) }} {
		bad := bytes.Clone(b)
		mutate(bad)
		if !errors.Is(validateUDPAck(bad), ErrUDPUnsupported) {
			t.Fatalf("accepted %x", bad)
		}
	}
}

func TestUDPNewClientRejectsOldMissingCapabilityACK(t *testing.T) {
	a, b := fixtureCredentials(t)
	server, err := NewServer(b, allowPort(func(uint16) bool { return false }))
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	listener, err := quic.ListenAddr("127.0.0.1:0", server.TLSConfig(), quicConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, err := listener.Accept(ctx)
		if err != nil {
			return
		}
		defer conn.CloseWithError(0, "old fixture done")
		stream, err := conn.AcceptStream(ctx)
		if err != nil {
			return
		}
		var hello [6]byte
		if _, err = io.ReadFull(stream, hello[:]); err != nil {
			return
		}
		_ = stream.Close()
		stream.CancelRead(0)
		<-conn.Context().Done()
	}()
	address := netip.MustParseAddrPort(listener.Addr().String())
	if flow, err := DialUDP(ctx, a, b.Device, address, 7777); !errors.Is(err, ErrUDPUnsupported) {
		if flow != nil {
			flow.Close()
		}
		t.Fatalf("old missing capability ACK: %v", err)
	}
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("old-peer fixture did not retire")
	}
}

// UDP capability negotiation must reject a TLS-authenticated QUIC peer that
// omitted DATAGRAM support, before any local endpoint or delivery socket exists.
func TestUDPRejectsPeerWithoutQUICDatagrams(t *testing.T) {
	a, b := fixtureCredentials(t)
	r := meshingress.NewRegistry()
	port := udpLifecycleApp(t, r, "echo")
	server, err := NewServer(b, testUDPAuthorization{r})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	listener, err := quic.ListenAddr("127.0.0.1:0", server.TLSConfig(), quicConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		conn, err := listener.Accept(ctx)
		if err == nil {
			server.serve(ctx, conn)
		}
	}()
	cfg, err := a.PeerTLSWithTickets(b.Device, ALPN, "app-quic")
	if err != nil {
		t.Fatal(err)
	}
	config := quicConfig()
	config.EnableDatagrams = false
	conn, err := quic.DialAddr(ctx, listener.Addr().String(), cfg, config)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseWithError(0, "done")
	if stream, err := requestAppPort(ctx, conn, port, udpOpenMagic); !errors.Is(err, ErrUDPUnsupported) {
		if stream != nil {
			stream.CancelRead(0)
			stream.Close()
		}
		t.Fatalf("peer without DATAGRAM capability accepted: %v", err)
	}
	if got := server.udpManager.Snapshot(); got.Flows != 0 || got.SocketBytes != 0 {
		t.Fatalf("unsupported peer allocated resources: %+v", got)
	}
}

func TestUDPClientRejectsServerWithoutQUICDatagrams(t *testing.T) {
	a, b := fixtureCredentials(t)
	server, err := NewServer(b, allowPort(func(uint16) bool { return false }))
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	config := quicConfig()
	config.EnableDatagrams = false
	socket, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer socket.Close()
	remoteContexts := make(chan context.Context, 1)
	transport := &quic.Transport{Conn: socket, ConnContext: func(ctx context.Context, _ *quic.ClientInfo) (context.Context, error) {
		remoteContexts <- ctx
		return ctx, nil
	}}
	defer transport.Close()
	listener, err := transport.Listen(server.TLSConfig(), config)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	// Observe the transport lifetime directly. The rejecting client can close
	// before the server queues the handshake for application Accept.
	flow, err := DialUDP(ctx, a, b.Device, netip.MustParseAddrPort(listener.Addr().String()), 7777)
	if flow != nil {
		flow.Close()
	}
	if !errors.Is(err, ErrUDPUnsupported) {
		t.Fatalf("no-DATAGRAM server not rejected at capability gate: %v", err)
	}
	var remote context.Context
	select {
	case remote = <-remoteContexts:
	case <-ctx.Done():
		t.Fatal("server did not observe the QUIC connection")
	}
	select {
	case <-remote.Done():
		cause := context.Cause(remote)
		var application *quic.ApplicationError
		var transport *quic.TransportError
		// Before 1-RTT keys are available QUIC carries an application close
		// as the APPLICATION_ERROR transport code, without its app code.
		applicationClose := errors.As(cause, &application) && application.Remote && application.ErrorCode == 1
		earlyClose := errors.As(cause, &transport) && transport.Remote && transport.ErrorCode == quic.ApplicationErrorErrorCode
		if !applicationClose && !earlyClose {
			t.Fatalf("unexpected connection retirement: %v", cause)
		}
	case <-ctx.Done():
		t.Fatal("unsupported connection not retired")
	}
}
