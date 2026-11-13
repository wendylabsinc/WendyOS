package services

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/types/known/emptypb"
)

type disconnectAckNetwork struct {
	mockNetworkManager
	calls      atomic.Int32
	disconnect func(context.Context) error
}

func (n *disconnectAckNetwork) DisconnectWiFi(ctx context.Context) error {
	n.calls.Add(1)
	if n.disconnect != nil {
		return n.disconnect(ctx)
	}
	return n.disconnErr
}

type disconnectAckConn struct {
	net.Conn
	drop *atomic.Bool
}

func (c *disconnectAckConn) Write(p []byte) (int, error) {
	if c.drop.Load() {
		return len(p), nil
	}
	return c.Conn.Write(p)
}

type disconnectAckListener struct {
	net.Listener
	drop *atomic.Bool
}

func (l *disconnectAckListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return &disconnectAckConn{Conn: c, drop: l.drop}, nil
}

type disconnectAckRecvTracker struct {
	active  atomic.Int32
	started chan struct{}
	done    chan struct{}
}

type disconnectAckTrackedStream struct {
	grpc.ServerStream
	tracker *disconnectAckRecvTracker
}

func (s *disconnectAckTrackedStream) RecvMsg(message any) error {
	s.tracker.active.Add(1)
	s.tracker.started <- struct{}{}
	defer func() { s.tracker.active.Add(-1); s.tracker.done <- struct{}{} }()
	return s.ServerStream.RecvMsg(message)
}

func startDisconnectAckServer(t *testing.T, nm NetworkManager, drop *atomic.Bool, options ...func(*WiFiService)) (*grpc.ClientConn, *disconnectAckRecvTracker) {
	t.Helper()
	lis := bufconn.Listen(bufSize)
	tracker := &disconnectAckRecvTracker{started: make(chan struct{}, 20), done: make(chan struct{}, 20)}
	server := grpc.NewServer(grpc.WaitForHandlers(true), grpc.StreamInterceptor(func(srv any, stream grpc.ServerStream, _ *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		return handler(srv, &disconnectAckTrackedStream{ServerStream: stream, tracker: tracker})
	}))
	svc := NewWiFiService(zap.NewNop(), nm)
	for _, option := range options {
		option(svc)
	}
	agentpbv2.RegisterWendyWiFiServiceServer(server, svc)
	agentpb.RegisterWendyAgentServiceServer(server, NewAgentService(zap.NewNop(), nm, nil, nil, nil))
	var listener net.Listener = lis
	if drop != nil {
		listener = &disconnectAckListener{Listener: lis, drop: drop}
	}
	go func() { _ = server.Serve(listener) }()
	conn, err := grpc.NewClient("passthrough:///wifi-disconnect",
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return lis.Dial() }),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close(); server.Stop(); _ = lis.Close() })
	return conn, tracker
}

func beginDisconnectAck(t *testing.T, ctx context.Context, conn *grpc.ClientConn) grpc.BidiStreamingClient[agentpbv2.DisconnectWiFiControl, agentpbv2.DisconnectWiFiEvent] {
	t.Helper()
	stream, err := agentpbv2.NewWendyWiFiServiceClient(conn).DisconnectWiFiAcknowledged(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.Send(&agentpbv2.DisconnectWiFiControl{Control: &agentpbv2.DisconnectWiFiControl_Begin{Begin: &emptypb.Empty{}}}); err != nil {
		t.Fatal(err)
	}
	ready, err := stream.Recv()
	if err != nil || ready.GetReady() == nil {
		t.Fatalf("ready = %v, error = %v", ready, err)
	}
	return stream
}

func confirmDisconnectAck(t *testing.T, stream grpc.BidiStreamingClient[agentpbv2.DisconnectWiFiControl, agentpbv2.DisconnectWiFiEvent]) {
	t.Helper()
	if err := stream.Send(&agentpbv2.DisconnectWiFiControl{Control: &agentpbv2.DisconnectWiFiControl_Confirm{Confirm: &emptypb.Empty{}}}); err != nil {
		t.Fatal(err)
	}
}

func TestWiFiDisconnectAcknowledgedCommitsDespiteMissingConfirmation(t *testing.T) {
	for _, mode := range []string{"cancel", "half close", "grace deadline", "invalid confirmation"} {
		t.Run(mode, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				called := make(chan struct{})
				nm := &disconnectAckNetwork{disconnect: func(context.Context) error { close(called); return nil }}
				conn, tracker := startDisconnectAckServer(t, nm, nil, func(s *WiFiService) { s.disconnectConfirmationTimeout = time.Second })
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				stream := beginDisconnectAck(t, ctx, conn)
				if nm.calls.Load() != 0 {
					t.Fatal("disconnect started before confirmation or grace expiry")
				}
				switch mode {
				case "cancel":
					cancel()
				case "half close":
					if err := stream.CloseSend(); err != nil {
						t.Fatal(err)
					}
				case "invalid confirmation":
					if err := stream.Send(&agentpbv2.DisconnectWiFiControl{Control: &agentpbv2.DisconnectWiFiControl_Begin{Begin: &emptypb.Empty{}}}); err != nil {
						t.Fatal(err)
					}
				}
				event, err := stream.Recv()
				if mode == "cancel" {
					if status.Code(err) != codes.Canceled {
						t.Fatalf("canceled stream = %v", err)
					}
				} else if err != nil || !event.GetCompleted().GetSuccess() {
					t.Fatalf("committed completion = %v, error = %v", event, err)
				}
				for i := 0; i < 2; i++ {
					select {
					case <-tracker.done:
					case <-time.After(2 * time.Second):
						t.Fatal("accepted request or confirmation receive did not finish")
					}
				}
				select {
				case <-called:
				case <-time.After(2 * time.Second):
					t.Fatal("accepted request did not attempt disconnect")
				}
				if nm.calls.Load() != 1 || tracker.active.Load() != 0 {
					t.Fatalf("disconnect calls = %d, active receives = %d", nm.calls.Load(), tracker.active.Load())
				}
			})
		})
	}
}

type disconnectReadyFailureStream struct {
	grpc.ServerStream
	ctx        context.Context
	readyError error
	sends      int
}

func (s *disconnectReadyFailureStream) Context() context.Context { return s.ctx }
func (s *disconnectReadyFailureStream) Recv() (*agentpbv2.DisconnectWiFiControl, error) {
	return &agentpbv2.DisconnectWiFiControl{Control: &agentpbv2.DisconnectWiFiControl_Begin{Begin: &emptypb.Empty{}}}, nil
}
func (s *disconnectReadyFailureStream) Send(*agentpbv2.DisconnectWiFiEvent) error {
	s.sends++
	return s.readyError
}
func TestWiFiDisconnectAcknowledgedCommitsWhenReadyCannotBeDelivered(t *testing.T) {
	nm := &disconnectAckNetwork{}
	stream := &disconnectReadyFailureStream{ctx: context.Background(), readyError: status.Error(codes.Unavailable, "reply path lost")}
	err := NewWiFiService(zap.NewNop(), nm).DisconnectWiFiAcknowledged(stream)
	if status.Code(err) != codes.Unavailable || nm.calls.Load() != 1 || stream.sends != 1 {
		t.Fatalf("accepted action lost or concurrent completion attempted: err=%v calls=%d sends=%d", err, nm.calls.Load(), stream.sends)
	}
}
func TestWiFiDisconnectAcknowledgedDoesNotCommitWithoutBegin(t *testing.T) {
	nm := &disconnectAckNetwork{}
	conn, _ := startDisconnectAckServer(t, nm, nil)
	stream, err := agentpbv2.NewWendyWiFiServiceClient(conn).DisconnectWiFiAcknowledged(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.CloseSend(); err != nil {
		t.Fatal(err)
	}
	if _, err := stream.Recv(); status.Code(err) != codes.InvalidArgument || nm.calls.Load() != 0 {
		t.Fatalf("missing begin triggered action: err=%v calls=%d", err, nm.calls.Load())
	}
}

func TestWiFiDisconnectAcknowledgedSurvivesPostConfirmationCancel(t *testing.T) {
	entered, release, finished := make(chan context.Context, 1), make(chan struct{}), make(chan error, 1)
	var releaseOnce sync.Once
	releaseOperation := func() { releaseOnce.Do(func() { close(release) }) }
	defer releaseOperation()
	nm := &disconnectAckNetwork{disconnect: func(ctx context.Context) error {
		entered <- ctx
		<-release
		finished <- ctx.Err()
		return nil
	}}
	conn, _ := startDisconnectAckServer(t, nm, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream := beginDisconnectAck(t, ctx, conn)
	confirmDisconnectAck(t, stream)
	var operation context.Context
	select {
	case operation = <-entered:
	case <-time.After(time.Second):
		t.Fatal("confirmed disconnect did not start")
	}
	cancel()
	if _, err := stream.Recv(); status.Code(err) != codes.Canceled {
		t.Fatalf("stream cancellation = %v", err)
	}
	if operation.Err() != nil {
		t.Fatal("transport cancellation aborted the confirmed operation")
	}
	if _, ok := operation.Deadline(); !ok {
		t.Fatal("confirmed operation has no execution bound")
	}
	releaseOperation()
	if err := <-finished; err != nil {
		t.Fatalf("operation inherited canceled stream: %v", err)
	}
}

func TestWiFiDisconnectAcknowledgedReportsNetworkManagerError(t *testing.T) {
	nm := &disconnectAckNetwork{mockNetworkManager: mockNetworkManager{disconnErr: errors.New("nmcli refused disconnect")}}
	conn, _ := startDisconnectAckServer(t, nm, nil)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	result, err := grpcclient.NewFromConn(conn).DisconnectWiFi(ctx)
	if err == nil || !strings.Contains(err.Error(), "nmcli refused disconnect") {
		t.Fatalf("result = %+v, error = %v", result, err)
	}
	if !result.Acknowledged || !result.CompletionConfirmed || result.Success == nil || *result.Success || nm.calls.Load() != 1 {
		t.Fatalf("network-manager error misreported: %+v, calls = %d", result, nm.calls.Load())
	}
}

func TestWiFiDisconnectAcknowledgedCompletesAfterConfirmation(t *testing.T) {
	nm := &disconnectAckNetwork{}
	conn, _ := startDisconnectAckServer(t, nm, nil)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	result, err := grpcclient.NewFromConn(conn).DisconnectWiFi(ctx)
	if err != nil || result.State != "completed" || !result.Acknowledged || !result.CompletionConfirmed || result.Success == nil || !*result.Success || result.Legacy || nm.calls.Load() != 1 {
		t.Fatalf("ordered production completion = %+v, error = %v, disconnect calls = %d", result, err, nm.calls.Load())
	}
}

func TestWiFiDisconnectAcknowledgedDoesNotDependOnStatusRead(t *testing.T) {
	nm := &disconnectAckNetwork{mockNetworkManager: mockNetworkManager{statusErr: errors.New("status unavailable")}}
	conn, _ := startDisconnectAckServer(t, nm, nil)
	result, err := grpcclient.NewFromConn(conn).DisconnectWiFi(context.Background())
	if err != nil || !result.Acknowledged || !result.CompletionConfirmed || nm.calls.Load() != 1 {
		t.Fatalf("accepted action depended on status query: result=%+v err=%v calls=%d", result, err, nm.calls.Load())
	}
}

func TestWiFiDisconnectAcknowledgedExecutionBound(t *testing.T) {
	nm := &disconnectAckNetwork{disconnect: func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }}
	conn, _ := startDisconnectAckServer(t, nm, nil, func(s *WiFiService) { s.disconnectExecutionTimeout = 30 * time.Millisecond })
	result, err := grpcclient.NewFromConn(conn).DisconnectWiFi(context.Background())
	if err == nil || !strings.Contains(err.Error(), "context deadline exceeded") || !result.CompletionConfirmed || result.Success == nil || *result.Success {
		t.Fatalf("execution deadline misreported: result = %+v, error = %v", result, err)
	}
}

func TestWiFiDisconnectAcknowledgedSkipsFutureControl(t *testing.T) {
	nm := &disconnectAckNetwork{}
	conn, _ := startDisconnectAckServer(t, nm, nil)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	stream, err := agentpbv2.NewWendyWiFiServiceClient(conn).DisconnectWiFiAcknowledged(ctx)
	if err != nil {
		t.Fatal(err)
	}
	future := &agentpbv2.DisconnectWiFiControl{}
	future.ProtoReflect().SetUnknown(protowire.AppendBytes(protowire.AppendTag(nil, 100, protowire.BytesType), nil))
	if err := stream.Send(future); err != nil {
		t.Fatal(err)
	}
	if err := stream.Send(&agentpbv2.DisconnectWiFiControl{Control: &agentpbv2.DisconnectWiFiControl_Begin{Begin: &emptypb.Empty{}}}); err != nil {
		t.Fatal(err)
	}
	if event, err := stream.Recv(); err != nil || event.GetReady() == nil {
		t.Fatalf("ready = %v, error = %v", event, err)
	}
	if err := stream.Send(future); err != nil {
		t.Fatal(err)
	}
	confirmDisconnectAck(t, stream)
	event, err := stream.Recv()
	if err != nil || !event.GetCompleted().GetSuccess() || nm.calls.Load() != 1 {
		t.Fatalf("completion = %v, error = %v, calls = %d", event, err, nm.calls.Load())
	}
}

func TestWiFiDisconnectAcknowledgedLostCompletionStaysUnknown(t *testing.T) {
	var drop atomic.Bool
	nm := &disconnectAckNetwork{disconnect: func(context.Context) error { drop.Store(true); return nil }}
	conn, _ := startDisconnectAckServer(t, nm, &drop)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	result, err := grpcclient.NewFromConn(conn).DisconnectWiFi(ctx)
	if err != nil || !result.Acknowledged || result.CompletionConfirmed || result.Success != nil || result.State != "requested" || nm.calls.Load() != 1 {
		t.Fatalf("lost completion was not an acknowledged unknown: %+v, error = %v, calls = %d", result, err, nm.calls.Load())
	}
	if strings.Contains(result.Message, "Disconnected") {
		t.Fatal("lost completion claimed successful disconnection")
	}
}
