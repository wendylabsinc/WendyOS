package services

import (
	"context"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"go.uber.org/zap"

	cloudpb "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb"
)

type fakeAgentStream struct {
	ctx context.Context
	in  chan *cloudpb.TunnelData
	out chan *cloudpb.TunnelData
}

func newFakeAgentStream(ctx context.Context) *fakeAgentStream {
	return &fakeAgentStream{
		ctx: ctx,
		in:  make(chan *cloudpb.TunnelData, 16),
		out: make(chan *cloudpb.TunnelData, 16),
	}
}
func (f *fakeAgentStream) Send(d *cloudpb.TunnelData) error {
	select {
	case f.out <- d:
		return nil
	case <-f.ctx.Done():
		return f.ctx.Err()
	}
}
func (f *fakeAgentStream) Recv() (*cloudpb.TunnelData, error) {
	select {
	case d, ok := <-f.in:
		if !ok {
			return nil, io.EOF
		}
		return d, nil
	case <-f.ctx.Done():
		return nil, f.ctx.Err()
	}
}
func (f *fakeAgentStream) CloseSend() error { return nil }

// startUDPEcho starts a loopback UDP echo server and returns its port.
func startUDPEcho(t *testing.T) uint32 {
	t.Helper()
	pc, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pc.Close() })
	go func() {
		buf := make([]byte, 65535)
		for {
			n, addr, err := pc.ReadFromUDP(buf)
			if err != nil {
				return
			}
			_, _ = pc.WriteToUDP(buf[:n], addr)
		}
	}()
	return uint32(pc.LocalAddr().(*net.UDPAddr).Port)
}

func awaitFrame(t *testing.T, f *fakeAgentStream) *cloudpb.TunnelData {
	t.Helper()
	select {
	case d := <-f.out:
		return d
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for relay output frame")
		return nil
	}
}

func TestDatagramRelayUDPRoundTrip(t *testing.T) {
	port := startUDPEcho(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream := newFakeAgentStream(ctx)
	relay := newDatagramRelay(zap.NewNop(), stream, time.Minute)
	go relay.run(ctx)

	stream.in <- &cloudpb.TunnelData{Datagram: &cloudpb.TunnelDatagram{
		FlowId: 7, Port: port, Payload: []byte("hello"),
	}}

	reply := awaitFrame(t, stream)
	if reply.GetDatagram() == nil {
		t.Fatalf("expected datagram frame, got %+v", reply)
	}
	if got := reply.GetDatagram().GetFlowId(); got != 7 {
		t.Fatalf("flow_id = %d, want 7", got)
	}
	if got := string(reply.GetDatagram().GetPayload()); got != "hello" {
		t.Fatalf("payload = %q, want hello", got)
	}
}

func TestDatagramRelayEcho(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream := newFakeAgentStream(ctx)
	relay := newDatagramRelay(zap.NewNop(), stream, time.Minute)
	go relay.run(ctx)

	stream.in <- &cloudpb.TunnelData{IcmpRequest: &cloudpb.IcmpEchoRequest{
		Identifier: 0x1234, Sequence: 3, Payload: []byte("ping"), OriginateUnixNs: 42,
	}}

	reply := awaitFrame(t, stream)
	r := reply.GetIcmpReply()
	if r == nil {
		t.Fatalf("expected icmp_reply frame, got %+v", reply)
	}
	if r.GetIdentifier() != 0x1234 || r.GetSequence() != 3 ||
		string(r.GetPayload()) != "ping" || r.GetOriginateUnixNs() != 42 {
		t.Fatalf("echo fields not copied: %+v", r)
	}
	if r.GetAgentUnixNs() == 0 {
		t.Fatal("agent_unix_ns not stamped")
	}
}

func TestDatagramRelayIdleExpiry(t *testing.T) {
	port := startUDPEcho(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream := newFakeAgentStream(ctx)
	relay := newDatagramRelay(zap.NewNop(), stream, 50*time.Millisecond)
	go relay.run(ctx)

	stream.in <- &cloudpb.TunnelData{Datagram: &cloudpb.TunnelDatagram{
		FlowId: 1, Port: port, Payload: []byte("x"),
	}}
	awaitFrame(t, stream) // echo comes back → flow exists

	deadline := time.Now().Add(3 * time.Second)
	for relay.activeFlows() != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("flow not expired; activeFlows=%d", relay.activeFlows())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestDatagramRelayFlowCap verifies flow_id — entirely client-controlled —
// cannot open unbounded sockets/goroutines on the agent: the
// maxFlowsPerSession+1th distinct flow_id is rejected with
// errFlowCapReached (no socket dialed), while re-using an already-open
// flow_id past the cap still succeeds (it's a write, not a new fd).
func TestDatagramRelayFlowCap(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream := newFakeAgentStream(ctx)
	relay := newDatagramRelay(zap.NewNop(), stream, time.Minute)

	// UDP connect() doesn't require a listener on the far end, so distinct
	// loopback ports below are fine to dial without an echo server.
	for i := uint32(0); i < maxFlowsPerSession; i++ {
		if _, err := relay.flow(ctx, i+1, 9); err != nil {
			t.Fatalf("flow %d: unexpected error opening flow within cap: %v", i+1, err)
		}
	}
	if got := relay.activeFlows(); got != maxFlowsPerSession {
		t.Fatalf("activeFlows = %d, want %d", got, maxFlowsPerSession)
	}

	if _, err := relay.flow(ctx, maxFlowsPerSession+100, 9); !errors.Is(err, errFlowCapReached) {
		t.Fatalf("flow beyond cap: err = %v, want errFlowCapReached", err)
	}
	if got := relay.activeFlows(); got != maxFlowsPerSession {
		t.Fatalf("activeFlows after rejected flow = %d, want unchanged %d", got, maxFlowsPerSession)
	}

	// An already-open flow_id is a write to an existing socket, not a new
	// fd, so it must succeed even though the session is at the cap.
	if _, err := relay.flow(ctx, 1, 9); err != nil {
		t.Fatalf("re-using an already-open flow_id at cap: unexpected error: %v", err)
	}
}

func TestDatagramRelayDropsOversized(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream := newFakeAgentStream(ctx)
	relay := newDatagramRelay(zap.NewNop(), stream, time.Minute)
	go relay.run(ctx)

	stream.in <- &cloudpb.TunnelData{Datagram: &cloudpb.TunnelDatagram{
		FlowId: 1, Port: 9, Payload: make([]byte, maxUDPPayload+1),
	}}
	stream.in <- &cloudpb.TunnelData{IcmpRequest: &cloudpb.IcmpEchoRequest{
		Identifier: 1, Sequence: 1, Payload: make([]byte, maxUDPPayload+1),
	}}
	// Follow with a valid echo; if either oversized frame opened a flow or
	// broke the loop, this would not come back.
	stream.in <- &cloudpb.TunnelData{IcmpRequest: &cloudpb.IcmpEchoRequest{Identifier: 1, Sequence: 2}}
	reply := awaitFrame(t, stream)
	if reply.GetIcmpReply() == nil || reply.GetIcmpReply().GetSequence() != 2 {
		t.Fatalf("relay loop broken after oversized frame: %+v", reply)
	}
	if relay.activeFlows() != 0 {
		t.Fatalf("oversized datagram opened a flow")
	}
}

func TestDatagramRelayDropsInvalidPorts(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream := newFakeAgentStream(ctx)
	relay := newDatagramRelay(zap.NewNop(), stream, time.Minute)
	go relay.run(ctx)

	for i, port := range []uint32{0, 65536} {
		stream.in <- &cloudpb.TunnelData{Datagram: &cloudpb.TunnelDatagram{
			FlowId: uint32(i + 1), Port: port, Payload: []byte("invalid"),
		}}
	}
	stream.in <- &cloudpb.TunnelData{IcmpRequest: &cloudpb.IcmpEchoRequest{Identifier: 1, Sequence: 3}}

	reply := awaitFrame(t, stream)
	if reply.GetIcmpReply() == nil {
		t.Fatalf("relay loop broken after invalid ports: %+v", reply)
	}
	if relay.activeFlows() != 0 {
		t.Fatalf("invalid port opened a flow")
	}
}

func TestDatagramRelayDestinationIsIPv4Loopback(t *testing.T) {
	addr := datagramLoopbackAddr(5353)
	if !addr.IP.Equal(net.IPv4(127, 0, 0, 1)) || addr.IP.To4() == nil {
		t.Fatalf("destination IP = %v, want IPv4 loopback", addr.IP)
	}
	if addr.Port != 5353 {
		t.Fatalf("destination port = %d, want 5353", addr.Port)
	}
}
