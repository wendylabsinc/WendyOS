package services

import (
	"context"
	"io"
	"net"
	"testing"
	"time"

	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
)

type fakeDeviceDatagramStream struct {
	agentpbv2.WendyTunnelService_DatagramTunnelServer
	ctx context.Context
	in  chan *agentpbv2.DeviceDatagramFrame
	out chan *agentpbv2.DeviceDatagramFrame
}

func newFakeDeviceDatagramStream(ctx context.Context) *fakeDeviceDatagramStream {
	return &fakeDeviceDatagramStream{
		ctx: ctx,
		in:  make(chan *agentpbv2.DeviceDatagramFrame, 16),
		out: make(chan *agentpbv2.DeviceDatagramFrame, 16),
	}
}

func (f *fakeDeviceDatagramStream) Context() context.Context { return f.ctx }

func (f *fakeDeviceDatagramStream) Send(msg *agentpbv2.DeviceDatagramFrame) error {
	select {
	case f.out <- msg:
		return nil
	case <-f.ctx.Done():
		return f.ctx.Err()
	}
}

func (f *fakeDeviceDatagramStream) Recv() (*agentpbv2.DeviceDatagramFrame, error) {
	select {
	case msg, ok := <-f.in:
		if !ok {
			return nil, io.EOF
		}
		return msg, nil
	case <-f.ctx.Done():
		return nil, f.ctx.Err()
	}
}

func TestTunnelServiceDatagramTunnelEchoesICMP(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream := newFakeDeviceDatagramStream(ctx)
	svc := NewTunnelService(zap.NewNop())

	go func() { _ = svc.DatagramTunnel(stream) }()

	stream.in <- &agentpbv2.DeviceDatagramFrame{
		Content: &agentpbv2.DeviceDatagramFrame_IcmpRequest{
			IcmpRequest: &agentpbv2.DeviceIcmpEchoRequest{
				Identifier: 9, Sequence: 1, Payload: []byte("ping"), OriginateUnixNs: 42,
			},
		},
	}

	select {
	case reply := <-stream.out:
		echo := reply.GetIcmpReply()
		if echo == nil {
			t.Fatalf("expected icmp_reply, got %+v", reply)
		}
		if echo.GetIdentifier() != 9 || echo.GetSequence() != 1 || string(echo.GetPayload()) != "ping" {
			t.Fatalf("echo fields not copied: %+v", echo)
		}
		if echo.GetOriginateUnixNs() != 42 {
			t.Fatalf("originate_unix_ns = %d, want 42", echo.GetOriginateUnixNs())
		}
		if echo.GetAgentUnixNs() == 0 {
			t.Fatal("agent_unix_ns not stamped")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for icmp reply")
	}
}

func TestTunnelServiceLimitsConcurrentDatagramSessions(t *testing.T) {
	svc := NewTunnelService(zap.NewNop())
	svc.sessions = make(chan struct{}, 1)

	firstCtx, cancelFirst := context.WithCancel(context.Background())
	defer cancelFirst()
	first := newFakeDeviceDatagramStream(firstCtx)
	firstDone := make(chan error, 1)
	go func() { firstDone <- svc.DatagramTunnel(first) }()

	deadline := time.Now().Add(5 * time.Second)
	for len(svc.sessions) != 1 {
		if time.Now().After(deadline) {
			t.Fatal("first session did not acquire its slot")
		}
		time.Sleep(time.Millisecond)
	}

	second := newFakeDeviceDatagramStream(context.Background())
	if got := status.Code(svc.DatagramTunnel(second)); got != codes.ResourceExhausted {
		t.Fatalf("second session status = %v, want %v", got, codes.ResourceExhausted)
	}

	cancelFirst()
	select {
	case err := <-firstDone:
		if err != nil {
			t.Fatalf("first session returned error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("first session did not release its slot")
	}

	third := newFakeDeviceDatagramStream(context.Background())
	close(third.in)
	if err := svc.DatagramTunnel(third); err != nil {
		t.Fatalf("session after release returned error: %v", err)
	}
}

func TestTunnelServiceDatagramTunnelUDPRoundTrip(t *testing.T) {
	packetConn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = packetConn.Close() })
	go func() {
		buf := make([]byte, 65535)
		for {
			n, addr, readErr := packetConn.ReadFromUDP(buf)
			if readErr != nil {
				return
			}
			_, _ = packetConn.WriteToUDP(buf[:n], addr)
		}
	}()
	port := uint32(packetConn.LocalAddr().(*net.UDPAddr).Port)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream := newFakeDeviceDatagramStream(ctx)
	svc := NewTunnelService(zap.NewNop())

	go func() { _ = svc.DatagramTunnel(stream) }()

	stream.in <- &agentpbv2.DeviceDatagramFrame{
		Content: &agentpbv2.DeviceDatagramFrame_Datagram{
			Datagram: &agentpbv2.DeviceDatagram{FlowId: 3, Port: port, Payload: []byte("hello")},
		},
	}

	select {
	case reply := <-stream.out:
		datagram := reply.GetDatagram()
		if datagram == nil || datagram.GetFlowId() != 3 || datagram.GetPort() != port || string(datagram.GetPayload()) != "hello" {
			t.Fatalf("unexpected datagram reply: %+v", reply)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for UDP reply")
	}
}
