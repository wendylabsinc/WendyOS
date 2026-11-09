package meshsession

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"net"
	"net/netip"
	"sync/atomic"
	"testing"
	"time"

	quic "github.com/quic-go/quic-go"
	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"github.com/wendylabsinc/wendy/go/internal/agent/meshingress"
	"github.com/wendylabsinc/wendy/go/internal/agent/udpmessage"
)

// A UDP tuple is a flow, not another authenticated transport. Start all three
// dials together so the first UDP flow cannot race a TCP handshake into a second
// connection. The two UDP flows intentionally use the same destination port.
func TestMultiplexConcurrentTCPAndUDPReuseOneSession(t *testing.T) {
	a, b := fixtureCredentials(t)
	registry := meshingress.NewRegistry()
	udpPort := udpLifecycleApp(t, registry, "udp")
	tcpPort := lifecycleEcho(t)
	if err := registry.Claim("tcp", tcpPort, "10.3.0.2", tcpPort); err != nil {
		t.Fatal(err)
	}
	address := lifecycleServer(t, b, udpLifecycleAuthorization{testUDPAuthorization{registry}}, true)
	client := lifecycleClient(t, a)
	var dials atomic.Int32
	originalDial := client.dial
	client.dial = func(ctx context.Context, address netip.AddrPort, config *tls.Config, options *quic.Config) (*quic.Conn, error) {
		dials.Add(1)
		return originalDial(ctx, address, config, options)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	start := make(chan struct{})
	type result struct {
		tcp net.Conn
		udp *DatagramFlow
		err error
	}
	results := make(chan result, 3)
	go func() {
		<-start
		conn, err := client.Dial(ctx, b.Device, address, tcpPort)
		results <- result{tcp: conn, err: err}
	}()
	for range 2 {
		go func() {
			<-start
			flow, err := client.DialUDP(ctx, b.Device, address, udpPort)
			results <- result{udp: flow, err: err}
		}()
	}
	close(start)
	var tcp net.Conn
	var flows []*DatagramFlow
	for range 3 {
		select {
		case result := <-results:
			if result.err != nil {
				t.Fatal(result.err)
			}
			if result.tcp != nil {
				tcp = result.tcp
				t.Cleanup(func() { result.tcp.Close() })
			} else {
				flows = append(flows, result.udp)
				t.Cleanup(func() { result.udp.Close() })
			}
		case <-ctx.Done():
			t.Fatal("concurrent flow setup stalled", ctx.Err())
		}
	}
	if tcp == nil || len(flows) != 2 {
		t.Fatal("missing established flows")
	}
	connection := tcp.(*streamConn).conn
	if dials.Load() != 1 || flows[0].conn != connection || flows[1].conn != connection {
		t.Fatalf("three flows used %d QUIC handshakes or different connections", dials.Load())
	}
	if got := localmesh.ApplicationProtocol(connection.ConnectionState().TLS.NegotiatedProtocol); got != multiplexALPN {
		t.Fatalf("multiplex protocol = %q", got)
	}
	lifecycleExchange(t, tcp)
	// The sizes straddle the multiplex-adjusted payload boundary and require
	// fragmentation. Linux also proves the full IPv4 UDP message ceiling; macOS
	// has a smaller kernel UDP socket ceiling and uses the established fixture cap.
	stride := maxMuxUDPChunk - udpmessage.HeaderSize
	sizes := []int{0, 1, stride, stride + 1, 2 * stride, 2*stride + 1, localEchoMaximum()}
	for _, size := range sizes {
		for i, flow := range flows {
			payload := bytes.Repeat([]byte{byte(0x61 + i)}, size)
			if err := flow.Send(payload); err != nil {
				t.Fatalf("flow %d size %d send: %v", i, size, err)
			}
		}
		for i, flow := range flows {
			got, err := readUDPMessage(flow, ctx)
			want := bytes.Repeat([]byte{byte(0x61 + i)}, size)
			if err != nil || !bytes.Equal(got, want) {
				t.Fatalf("flow %d size %d echo %d bytes: %v", i, size, len(got), err)
			}
		}
		lifecycleExchange(t, tcp)
	}
	for _, flow := range flows {
		if err := flow.Send(make([]byte, MaxUDPMessageSize+1)); err == nil {
			t.Fatal("oversize application UDP message admitted")
		}
	}
}

func TestMultiplexUDPClosureAndRevocationLeaveOtherFlowsUsable(t *testing.T) {
	for _, action := range []string{"close", "revoke"} {
		t.Run(action, func(t *testing.T) {
			a, b := fixtureCredentials(t)
			registry := meshingress.NewRegistry()
			onePort, twoPort := udpLifecycleApp(t, registry, "one"), udpLifecycleApp(t, registry, "two")
			tcpPort := lifecycleEcho(t)
			if err := registry.Claim("tcp", tcpPort, "10.3.0.2", tcpPort); err != nil {
				t.Fatal(err)
			}
			address := lifecycleServer(t, b, udpLifecycleAuthorization{testUDPAuthorization{registry}}, true)
			client := lifecycleClient(t, a)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			tcp := lifecycleDial(t, client, b.Device, address, tcpPort)
			one, err := client.DialUDP(ctx, b.Device, address, onePort)
			if err != nil {
				t.Fatal(err)
			}
			defer one.Close()
			two, err := client.DialUDP(ctx, b.Device, address, twoPort)
			if err != nil {
				t.Fatal(err)
			}
			defer two.Close()
			if one.conn != tcp.conn || two.conn != tcp.conn {
				t.Fatal("not a shared session")
			}
			deniedPort := uint16(1)
			for registry.UDPToken(deniedPort) != 0 {
				deniedPort++
			}
			if denied, err := client.DialUDP(ctx, b.Device, address, deniedPort); !errors.Is(err, ErrDenied) {
				if denied != nil {
					denied.Close()
				}
				t.Fatalf("unpublished port: %v", err)
			}
			if action == "close" {
				if err := one.Close(); err != nil {
					t.Fatal(err)
				}
			} else {
				registry.Release("one")
			}
			select {
			case <-one.Done():
			case <-ctx.Done():
				t.Fatal("individual UDP flow did not close", ctx.Err())
			}
			if err := tcp.conn.Context().Err(); err != nil {
				t.Fatal("individual flow closure killed session", err)
			}
			lifecycleExchange(t, tcp)
			if err := two.Send([]byte("survivor")); err != nil {
				t.Fatal(err)
			}
			if got, err := readUDPMessage(two, ctx); err != nil || string(got) != "survivor" {
				t.Fatalf("other UDP flow failed: %q %v", got, err)
			}
			if action == "revoke" {
				if err := registry.ClaimUDPForApp("replacement", "replacement", onePort, "10.3.0.2", onePort); err != nil {
					t.Fatal(err)
				}
			}
			replacement, err := client.DialUDP(ctx, b.Device, address, onePort)
			if err != nil {
				t.Fatal(err)
			}
			defer replacement.Close()
			if replacement.conn != tcp.conn {
				t.Fatal("individual flow replacement repeated handshake")
			}
			if err := replacement.Send([]byte("replacement")); err != nil {
				t.Fatal(err)
			}
			if got, err := readUDPMessage(replacement, ctx); err != nil || string(got) != "replacement" {
				t.Fatalf("replacement flow failed: %q %v", got, err)
			}
			client.Invalidate(b.Device)
			requireSessionClosed(t, tcp.conn)
			for i, flow := range []*DatagramFlow{two, replacement} {
				select {
				case <-flow.Done():
				case <-ctx.Done():
					t.Fatalf("invalidation left UDP flow %d open", i)
				}
			}
		})
	}
}

// The protocol is selected during the authenticated handshake. A v1 peer still
// serves TCP; requesting mixed UDP must not silently open a second connection.
func TestMultiplexLegacyPeerTCPAndExplicitUDPRefusal(t *testing.T) {
	a, b := fixtureCredentials(t)
	registry := meshingress.NewRegistry()
	tcpPort := lifecycleEcho(t)
	if err := registry.Claim("tcp", tcpPort, "10.3.0.2", tcpPort); err != nil {
		t.Fatal(err)
	}
	server, err := NewServer(b, &loopbackRegistry{Registry: registry})
	if err != nil {
		t.Fatal(err)
	}
	config := server.TLSConfig()
	config.NextProtos = []string{ALPN}
	listener, err := quic.ListenAddr("127.0.0.1:0", config, quicConfig())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	defer listener.Close()
	defer server.Close()
	go func() {
		for {
			conn, err := listener.Accept(ctx)
			if err != nil {
				return
			}
			go server.serve(ctx, conn)
		}
	}()
	client := lifecycleClient(t, a)
	var dials atomic.Int32
	originalDial := client.dial
	client.dial = func(ctx context.Context, address netip.AddrPort, config *tls.Config, options *quic.Config) (*quic.Conn, error) {
		dials.Add(1)
		return originalDial(ctx, address, config, options)
	}
	address := netip.MustParseAddrPort(listener.Addr().String())
	tcp := lifecycleDial(t, client, b.Device, address, tcpPort)
	if protocol := localmesh.ApplicationProtocol(tcp.conn.ConnectionState().TLS.NegotiatedProtocol); protocol != ALPN {
		t.Fatalf("legacy fixture negotiated %q", protocol)
	}
	if flow, err := client.DialUDP(ctx, b.Device, address, tcpPort); !errors.Is(err, ErrUDPUnsupported) {
		if flow != nil {
			flow.Close()
		}
		t.Fatalf("legacy UDP = %v, want explicit unsupported", err)
	}
	if dials.Load() != 1 {
		t.Fatalf("legacy UDP refusal created %d sessions", dials.Load())
	}
	lifecycleExchange(t, tcp)
}

// The first client-initiated bidirectional stream has ID zero. It is a valid
// UDP flow identifier, and admitting it must not lock the session into UDP-only.
func TestMultiplexUDPFirstThenTCPKeepsSession(t *testing.T) {
	a, b := fixtureCredentials(t)
	registry := meshingress.NewRegistry()
	udpPort := udpLifecycleApp(t, registry, "udp-first")
	tcpPort := lifecycleEcho(t)
	if err := registry.Claim("tcp", tcpPort, "10.3.0.2", tcpPort); err != nil {
		t.Fatal(err)
	}
	address := lifecycleServer(t, b, udpLifecycleAuthorization{testUDPAuthorization{registry}}, true)
	client := lifecycleClient(t, a)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	flow, err := client.DialUDP(ctx, b.Device, address, udpPort)
	if err != nil {
		t.Fatal(err)
	}
	defer flow.Close()
	if err := flow.Send([]byte("first flow")); err != nil {
		t.Fatal(err)
	}
	if got, err := readUDPMessage(flow, ctx); err != nil || string(got) != "first flow" {
		t.Fatalf("first UDP flow %q: %v", got, err)
	}
	tcp := lifecycleDial(t, client, b.Device, address, tcpPort)
	if tcp.conn != flow.conn {
		t.Fatal("TCP after UDP opened another connection")
	}
	lifecycleExchange(t, tcp)
	flow.Close()
	lifecycleExchange(t, tcp)
}

func TestMultiplexLateFlowIDCannotEnterReplacement(t *testing.T) {
	a, b := fixtureCredentials(t)
	registry := meshingress.NewRegistry()
	port := udpLifecycleApp(t, registry, "udp")
	server, err := NewServer(b, testUDPAuthorization{registry})
	if err != nil {
		t.Fatal(err)
	}
	listener, err := quic.ListenAddr("127.0.0.1:0", server.TLSConfig(), quicConfig())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	defer listener.Close()
	defer server.Close()
	accepted := make(chan *quic.Conn, 1)
	go func() {
		conn, err := listener.Accept(ctx)
		if err != nil {
			return
		}
		accepted <- conn
		server.serve(ctx, conn)
	}()
	client := lifecycleClient(t, a)
	address := netip.MustParseAddrPort(listener.Addr().String())
	old, err := client.DialUDP(ctx, b.Device, address, port)
	if err != nil {
		t.Fatal(err)
	}
	defer old.Close()
	remote := <-accepted
	oldID := old.shared.id
	old.Close()
	fresh, err := client.DialUDP(ctx, b.Device, address, port)
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.Close()
	if fresh.shared.id == oldID || fresh.conn != old.conn {
		t.Fatal("replacement reused a flow ID or opened another session")
	}
	payload := []byte("late reply to retired flow")
	frame := multiplexTestFrame(oldID, payload)
	if err := remote.TrySendDatagram(frame); err != nil {
		t.Fatal(err)
	}
	readCtx, stop := context.WithTimeout(ctx, 100*time.Millisecond)
	if message, err := fresh.Receive(readCtx); !errors.Is(err, context.DeadlineExceeded) {
		if message != nil {
			message.Release()
		}
		stop()
		t.Fatalf("late retired-flow reply reached replacement: %v", err)
	}
	stop()
	if err := fresh.Send([]byte("fresh")); err != nil {
		t.Fatal(err)
	}
	if got, err := readUDPMessage(fresh, ctx); err != nil || string(got) != "fresh" {
		t.Fatalf("unknown flow ID damaged live flow: %q %v", got, err)
	}
}

func multiplexTestFrame(flowID uint64, payload []byte) []byte {
	frame := make([]byte, udpMuxHeaderSize+udpmessage.HeaderSize+len(payload))
	binary.BigEndian.PutUint64(frame[:udpMuxHeaderSize], flowID)
	fragment := frame[udpMuxHeaderSize:]
	fragment[0], fragment[1], fragment[2] = 'W', 'U', 2
	binary.BigEndian.PutUint64(fragment[4:12], 1)
	binary.BigEndian.PutUint16(fragment[12:14], uint16(len(payload)))
	binary.BigEndian.PutUint16(fragment[14:16], uint16(maxMuxUDPChunk-udpmessage.HeaderSize))
	binary.BigEndian.PutUint16(fragment[18:20], 1)
	copy(fragment[udpmessage.HeaderSize:], payload)
	return frame
}

func TestMultiplexDatagramAdmissionAccountsForFlowPrefix(t *testing.T) {
	fragment := []byte{1, 2, 3, 4}
	for _, id := range []uint64{0, 1, 0x0102030405060708} {
		send := multiplexUDPAdmissionWithSender(func(wire []byte) error {
			if len(wire) != udpMuxHeaderSize+len(fragment) || binary.BigEndian.Uint64(wire[:udpMuxHeaderSize]) != id || !bytes.Equal(wire[udpMuxHeaderSize:], fragment) {
				t.Fatalf("wrong multiplex envelope %x", wire)
			}
			return nil
		}, id)
		if err := send(fragment); err != nil {
			t.Fatal(err)
		}
	}
	for _, capacity := range []int64{-1, 0, 8, 263, 264, 512, 1000} {
		send := multiplexUDPAdmissionWithSender(func([]byte) error {
			return &quic.DatagramTooLargeError{MaxDatagramPayloadSize: capacity}
		}, 0)
		var reduced *udpmessage.ChunkTooLarge
		if err := send(fragment); !errors.As(err, &reduced) || reduced.Maximum != int(capacity)-udpMuxHeaderSize {
			t.Fatalf("QUIC capacity %d yielded %v", capacity, err)
		}
	}
	if err := multiplexUDPAdmissionWithSender(func([]byte) error { return quic.ErrDatagramQueueFull }, 0)(fragment); !errors.Is(err, udpmessage.ErrQueueFull) {
		t.Fatalf("queue-full mapping = %v", err)
	}
}

// Exercise udpmessage's actual capacity-reduction and reassembly paths without
// a local UDP socket, so the full 65507-byte message ceiling is checked on macOS
// as well as Linux. The initial fragment plus flow prefix exceeds the injected
// QUIC allowance and must be resized before any fragment is admitted.
func TestMultiplexReducedDatagramCapacityPreservesFullMessage(t *testing.T) {
	a, b := fixtureCredentials(t)
	senderManager, receiverManager := udpmessage.NewManager(), udpmessage.NewManager()
	defer senderManager.Close()
	defer receiverManager.Close()
	pass := func(send func() error) error { return send() }
	sender, err := senderManager.Open(b.Device, pass)
	if err != nil {
		t.Fatal(err)
	}
	receiver, err := receiverManager.Open(a.Device, pass)
	if err != nil {
		t.Fatal(err)
	}
	if err := receiver.SetTransport(maxMuxUDPChunk, false, func([]byte) error { return nil }); err != nil {
		t.Fatal(err)
	}
	const allowance = 512
	const flowID = 12
	var oversized atomic.Int32
	type result struct {
		payload []byte
		err     error
	}
	resultCh := make(chan result, 1)
	admit := multiplexUDPAdmissionWithSender(func(wire []byte) error {
		if len(wire) > allowance {
			oversized.Add(1)
			return &quic.DatagramTooLargeError{MaxDatagramPayloadSize: allowance}
		}
		if binary.BigEndian.Uint64(wire[:udpMuxHeaderSize]) != flowID {
			return errors.New("flow prefix changed during capacity adjustment")
		}
		message, err := receiver.ReceiveFrame(wire[udpMuxHeaderSize:])
		if err != nil {
			select {
			case resultCh <- result{err: err}:
			default:
			}
			return err
		}
		if message != nil {
			payload := bytes.Clone(message.Payload)
			message.Release()
			select {
			case resultCh <- result{payload: payload}:
			default:
			}
		}
		return nil
	}, flowID)
	if err := sender.SetTransport(maxMuxUDPChunk, false, admit); err != nil {
		t.Fatal(err)
	}
	payload := bytes.Repeat([]byte{0x5a}, MaxUDPMessageSize)
	if err := sender.Send(payload); err != nil {
		t.Fatal(err)
	}
	select {
	case result := <-resultCh:
		if result.err != nil || !bytes.Equal(result.payload, payload) {
			t.Fatalf("reassembled %d of %d bytes: %v", len(result.payload), len(payload), result.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("full message did not survive reduced datagram allowance")
	}
	if oversized.Load() != 1 {
		t.Fatalf("capacity adjustment attempted %d oversized datagrams", oversized.Load())
	}
	if stats := senderManager.Snapshot(); stats.SendChunkSeenMin != allowance-udpMuxHeaderSize {
		t.Fatalf("adjusted fragment allowance = %d, want %d", stats.SendChunkSeenMin, allowance-udpMuxHeaderSize)
	}
}

func TestMultiplexUDPBudgetRefusalPreservesTCPAndReturnsCredit(t *testing.T) {
	a, b := fixtureCredentials(t)
	registry := meshingress.NewRegistry()
	udpPort := udpLifecycleApp(t, registry, "udp-budget")
	tcpPort := lifecycleEcho(t)
	if err := registry.Claim("tcp-budget", tcpPort, "10.3.0.2", tcpPort); err != nil {
		t.Fatal(err)
	}
	address := lifecycleServer(t, b, udpLifecycleAuthorization{testUDPAuthorization{registry}}, true)
	client := lifecycleClient(t, a)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	flows := make([]*DatagramFlow, 0, udpmessage.PeerFlows)
	defer func() {
		for _, flow := range flows {
			flow.Close()
		}
	}()
	for i := range udpmessage.PeerFlows {
		flow, err := client.DialUDP(ctx, b.Device, address, udpPort)
		if err != nil {
			t.Fatalf("UDP flow %d of %d: %v", i+1, udpmessage.PeerFlows, err)
		}
		flows = append(flows, flow)
		if flow.conn != flows[0].conn {
			t.Fatal("UDP budget filled using multiple sessions")
		}
	}
	if overflow, err := client.DialUDP(ctx, b.Device, address, udpPort); !errors.Is(err, udpmessage.ErrCapacity) {
		if overflow != nil {
			overflow.Close()
		}
		t.Fatalf("over-budget UDP flow = %v", err)
	}
	tcpFlows := make([]net.Conn, 0, 8)
	defer func() {
		for _, flow := range tcpFlows {
			flow.Close()
		}
	}()
	for i := range 8 {
		flow, err := client.Dial(ctx, b.Device, address, tcpPort)
		if err != nil {
			t.Fatalf("TCP flow %d with full UDP budget: %v", i+1, err)
		}
		tcpFlows = append(tcpFlows, flow)
		if flow.(*streamConn).conn != flows[0].conn {
			t.Fatal("TCP with full UDP budget opened another session")
		}
		lifecycleExchange(t, flow)
	}
	flows[0].Close()
	var replacement *DatagramFlow
	for {
		var err error
		replacement, err = client.DialUDP(ctx, b.Device, address, udpPort)
		if err == nil {
			break
		}
		if !errors.Is(err, udpmessage.ErrCapacity) || ctx.Err() != nil {
			t.Fatalf("closing UDP did not return stream/resources: %v", err)
		}
		// The remote control FIN must be observed before its independent socket
		// and per-peer reservation can be released.
		time.Sleep(5 * time.Millisecond)
	}
	defer replacement.Close()
	if replacement.conn != flows[1].conn {
		t.Fatal("returned UDP capacity opened another session")
	}
	if err := replacement.Send([]byte("capacity returned")); err != nil {
		t.Fatal(err)
	}
	if got, err := readUDPMessage(replacement, ctx); err != nil || string(got) != "capacity returned" {
		t.Fatalf("replacement UDP %q: %v", got, err)
	}
	for _, flow := range tcpFlows {
		lifecycleExchange(t, flow)
	}
}

func TestMultiplexSlowUDPReceiverDoesNotBlockOtherFlows(t *testing.T) {
	a, b := fixtureCredentials(t)
	registry := meshingress.NewRegistry()
	udpPort := udpLifecycleApp(t, registry, "udp")
	tcpPort := lifecycleEcho(t)
	if err := registry.Claim("tcp", tcpPort, "10.3.0.2", tcpPort); err != nil {
		t.Fatal(err)
	}
	address := lifecycleServer(t, b, udpLifecycleAuthorization{testUDPAuthorization{registry}}, true)
	client := lifecycleClient(t, a)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	slow, err := client.DialUDP(ctx, b.Device, address, udpPort)
	if err != nil {
		t.Fatal(err)
	}
	defer slow.Close()
	other, err := client.DialUDP(ctx, b.Device, address, udpPort)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	tcp := lifecycleDial(t, client, b.Device, address, tcpPort)
	for i := range udpmessage.FlowRXMessages + 1 {
		if err := slow.Send([]byte{byte(i)}); err != nil {
			t.Fatal(err)
		}
	}
	for len(slow.shared.messages) < udpmessage.FlowRXMessages {
		select {
		case <-ctx.Done():
			t.Fatal("slow receiver did not fill its bounded queue")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	lifecycleExchange(t, tcp)
	if err := other.Send([]byte("not blocked")); err != nil {
		t.Fatal(err)
	}
	if got, err := readUDPMessage(other, ctx); err != nil || string(got) != "not blocked" {
		t.Fatalf("slow receiver blocked another flow: %q %v", got, err)
	}
	if other.conn != slow.conn || tcp.conn != slow.conn {
		t.Fatal("slow flow isolation relied on different sessions")
	}
}

func TestMultiplexRouteWithdrawalRetiresUDPAndDrainsTCP(t *testing.T) {
	a, b := fixtureCredentials(t)
	registry := meshingress.NewRegistry()
	udpPort := udpLifecycleApp(t, registry, "udp")
	tcpPort := lifecycleEcho(t)
	if err := registry.Claim("tcp", tcpPort, "10.3.0.2", tcpPort); err != nil {
		t.Fatal(err)
	}
	address := lifecycleServer(t, b, udpLifecycleAuthorization{testUDPAuthorization{registry}}, true)
	client := lifecycleClient(t, a)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	tcp := lifecycleDial(t, client, b.Device, address, tcpPort)
	udp, err := client.DialUDP(ctx, b.Device, address, udpPort)
	if err != nil {
		t.Fatal(err)
	}
	defer udp.Close()
	if udp.conn != tcp.conn {
		t.Fatal("not a shared session")
	}
	client.WithdrawRoute(b.Device)
	select {
	case <-udp.Done():
	case <-ctx.Done():
		t.Fatal("route withdrawal left UDP flow alive")
	}
	// The fixture calls Client directly to model a restored, freshly checked
	// route. Production resolves signed routing before every new Client dial.
	lifecycleExchange(t, tcp)
	fresh, err := client.DialUDP(ctx, b.Device, address, udpPort)
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.Close()
	if fresh.conn == tcp.conn {
		t.Fatal("restored route reused retired session")
	}
	tcp.Close()
	requireSessionClosed(t, tcp.conn)
	if err := fresh.Send([]byte("restored route")); err != nil {
		t.Fatal(err)
	}
	if got, err := readUDPMessage(fresh, ctx); err != nil || string(got) != "restored route" {
		t.Fatalf("restored UDP flow %q: %v", got, err)
	}
}
