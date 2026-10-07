package liteclient

import (
	"context"
	"net"
	"testing"
	"time"

	wendypb "github.com/wendylabsinc/wendy/go/proto/gen/litepb"
	"github.com/wendylabsinc/wendy/go/proto/gen/wcomrelaypb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/proto"
)

// relayOpen is what a fakeRelay saw when a stream opened.
type relayOpen struct {
	assetID       string
	authorization []string
}

// fakeRelay stands in for the Cloud relay and the device behind it: it
// records each stream's open message and caller metadata, then answers every
// WendyCom handshake by echoing it, as a protocol 2 device does. With refuse
// set, it ends the stream with that error once the handshake arrives: over a
// network, the client has sent its handshake by the time a refusal of the
// open message reaches it.
type fakeRelay struct {
	wcomrelaypb.UnimplementedWendyComRelayServiceServer
	refuse error
	opens  chan relayOpen
	ended  chan struct{}
}

func newFakeRelay() *fakeRelay {
	return &fakeRelay{opens: make(chan relayOpen, 1), ended: make(chan struct{}, 1)}
}

func (r *fakeRelay) WendyComRelay(stream wcomrelaypb.WendyComRelayService_WendyComRelayServer) error {
	defer func() { r.ended <- struct{}{} }()
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	md, _ := metadata.FromIncomingContext(stream.Context())
	r.opens <- relayOpen{assetID: first.GetOpen().GetAssetId(), authorization: md.Get("authorization")}
	for {
		msg, err := stream.Recv()
		if err != nil {
			return err
		}
		if r.refuse != nil {
			return r.refuse
		}
		req := &wendypb.WendyComMessage{}
		if err := proto.Unmarshal(msg.GetPayload().GetBytes(), req); err != nil {
			return err
		}
		if req.GetHandshake() != nil {
			if err := stream.Send(msg); err != nil {
				return err
			}
		}
	}
}

// dialFakeRelay serves relay in-process and returns a connection to it.
func dialFakeRelay(t *testing.T, relay *fakeRelay) *grpc.ClientConn {
	t.Helper()
	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer()
	wcomrelaypb.RegisterWendyComRelayServiceServer(srv, relay)
	go srv.Serve(lis)
	t.Cleanup(srv.Stop)
	cc, err := grpc.NewClient("passthrough:///relay",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("grpc.NewClient: %v", err)
	}
	return cc
}

const relayTestAssetID = "0b6f7a52-5d1e-4c3b-9a8e-2f4d6c8b1a3e"

func TestConnectViaRelaySendsCallerCredentialsAndOwnsConn(t *testing.T) {
	relay := newFakeRelay()
	cc := dialFakeRelay(t, relay)
	ctx := metadata.AppendToOutgoingContext(context.Background(), "authorization", "Bearer session")

	c := NewWendyLiteClient()
	if err := c.ConnectViaRelay(ctx, cc, relayTestAssetID); err != nil {
		t.Fatalf("ConnectViaRelay: %v", err)
	}
	open := <-relay.opens
	if open.assetID != relayTestAssetID {
		t.Errorf("open names asset %q, want %q", open.assetID, relayTestAssetID)
	}
	if len(open.authorization) != 1 || open.authorization[0] != "Bearer session" {
		t.Errorf("relay saw authorization %q, want the caller's", open.authorization)
	}

	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	select {
	case <-relay.ended:
	case <-time.After(5 * time.Second):
		t.Fatal("relay stream still open after Close")
	}
	if state := cc.GetState(); state != connectivity.Shutdown {
		t.Errorf("connection is %v after Close, want it closed with the client", state)
	}
}

// A relay that refuses the caller ends the stream with a status. The client
// must report that status, not a handshake timeout, and release the
// connection it was given.
func TestConnectViaRelayReportsRefusalAndClosesConn(t *testing.T) {
	relay := newFakeRelay()
	relay.refuse = status.Error(codes.PermissionDenied, "permission denied")
	cc := dialFakeRelay(t, relay)

	err := NewWendyLiteClient().ConnectViaRelay(context.Background(), cc, relayTestAssetID)
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("ConnectViaRelay error = %v, want the relay's PermissionDenied", err)
	}
	if state := cc.GetState(); state != connectivity.Shutdown {
		t.Errorf("connection is %v after a refused connect, want it closed", state)
	}
}

// floodStream is a tunnel stream whose Recv always has another payload ready,
// as a chatty device would. Only Recv is used by recvLoop.
type floodStream struct {
	wcomrelaypb.WendyComRelayService_WendyComRelayClient
	payload []byte
}

func (s *floodStream) Recv() (*wcomrelaypb.WendyComRelayMessage, error) {
	return &wcomrelaypb.WendyComRelayMessage{
		Msg: &wcomrelaypb.WendyComRelayMessage_Payload{
			Payload: &wcomrelaypb.WendyComRelayPayload{Bytes: s.payload},
		},
	}, nil
}

func TestTunnelRecvLoopExitsWhenCancelledWithNoReader(t *testing.T) {
	body, err := proto.Marshal(&wendypb.WendyComMessage{})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	l := &tunnelLink{
		stream: &floodStream{payload: body},
		ctx:    ctx,
		cancel: cancel,
		msgs:   make(chan *wendypb.WendyComMessage, 16),
	}
	exited := make(chan struct{})
	go func() {
		l.recvLoop()
		close(exited)
	}()

	// Let the channel fill with nobody reading, as after a failed handshake.
	deadline := time.Now().Add(time.Second)
	for len(l.msgs) < cap(l.msgs) && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if len(l.msgs) < cap(l.msgs) {
		t.Fatalf("msgs holds %d of %d, want it full", len(l.msgs), cap(l.msgs))
	}

	cancel()
	select {
	case <-exited:
	case <-time.After(time.Second):
		t.Fatal("recvLoop still blocked on a full channel after cancel")
	}
}
