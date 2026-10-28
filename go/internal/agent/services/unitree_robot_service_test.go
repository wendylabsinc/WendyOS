package services

import (
	"context"
	"math"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/unitree"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

type teleopFixtureLink struct {
	*testUnitreeLink
	mu         sync.Mutex
	calls      []string
	failMove   bool
	failStop   bool
	failAvoid  bool
	helloDelay time.Duration
	messages   chan unitree.TopicMessage
	video      chan unitree.VideoFrame
	subscribed []string
	cancelled  bool
}

func (l *teleopFixtureLink) Subscribe(topics []string, video bool) (<-chan unitree.TopicMessage, <-chan unitree.VideoFrame, func(), error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.subscribed = append([]string(nil), topics...)
	if video {
		l.subscribed = append(l.subscribed, "video")
	}
	return l.messages, l.video, func() { l.mu.Lock(); l.cancelled = true; l.mu.Unlock() }, nil
}

func (l *teleopFixtureLink) record(name string) {
	l.mu.Lock()
	l.calls = append(l.calls, name)
	l.mu.Unlock()
}
func (l *teleopFixtureLink) Move(ctx context.Context, f, s, y float64) error {
	if f == 0 && s == 0 && y == 0 {
		return l.Stop(ctx)
	}
	l.record("move")
	l.mu.Lock()
	fail := l.failMove
	l.mu.Unlock()
	if fail {
		return context.DeadlineExceeded
	}
	return nil
}
func (l *teleopFixtureLink) Stop(context.Context) error {
	l.record("stop")
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.failStop {
		return context.DeadlineExceeded
	}
	return nil
}
func (l *teleopFixtureLink) SetObstacleAvoidance(_ context.Context, enabled bool) error {
	if enabled {
		l.record("avoid-on")
	} else {
		l.record("avoid-off")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.failAvoid {
		return context.DeadlineExceeded
	}
	return nil
}
func (l *teleopFixtureLink) StandUp(context.Context) error { l.record("stand"); return nil }
func (l *teleopFixtureLink) LieDown(context.Context) error { l.record("lie"); return nil }
func (l *teleopFixtureLink) Hello(ctx context.Context) error {
	l.record("hello")
	l.mu.Lock()
	delay := l.helloDelay
	l.mu.Unlock()
	if delay == 0 {
		return nil
	}
	select {
	case <-time.After(delay):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (l *teleopFixtureLink) log() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.calls...)
}

func teleopFixture(t *testing.T, authenticated bool, lease time.Duration) (pb.WendyUnitreeRobotServiceClient, *teleopFixtureLink, *UnitreeService) {
	t.Helper()
	p := testUnitreeService(t)
	l := &teleopFixtureLink{testUnitreeLink: newTestUnitreeLink(),
		messages: make(chan unitree.TopicMessage, 4), video: make(chan unitree.VideoFrame, 4)}
	p.sessions["GO2TEST1"] = &unitreeSession{link: l, cancel: func() {}, done: make(chan struct{})}
	listener := bufconn.Listen(1 << 20)
	withPeer := func(ctx context.Context) context.Context {
		if authenticated {
			return peer.NewContext(ctx, &peer.Peer{Addr: &net.UnixAddr{Name: "fixture", Net: "unix"}})
		}
		return ctx
	}
	server := grpc.NewServer(
		grpc.UnaryInterceptor(func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, h grpc.UnaryHandler) (any, error) {
			return h(withPeer(ctx), req)
		}),
		grpc.StreamInterceptor(func(srv any, ss grpc.ServerStream, _ *grpc.StreamServerInfo, h grpc.StreamHandler) error {
			return h(srv, peerStream{ss, withPeer(ss.Context())})
		}))
	svc := NewUnitreeRobotService(p)
	svc.lease = lease
	pb.RegisterWendyUnitreeRobotServiceServer(server, svc)
	go server.Serve(listener)
	conn, err := grpc.NewClient("passthrough:///fixture", grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close(); server.Stop(); listener.Close() })
	return pb.NewWendyUnitreeRobotServiceClient(conn), l, p
}

func teleopSend(c pb.WendyUnitreeRobotServiceClient, cmd pb.UnitreeCommand, f, y float64) error {
	_, err := c.Command(context.Background(), &pb.UnitreeCommandRequest{Serial: "GO2TEST1", Command: cmd, Forward: f, Yaw: y})
	return err
}

func sameCalls(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestUnitreeRobotCommandRequiresTransport(t *testing.T) {
	c, l, _ := teleopFixture(t, false, time.Second)
	if err := teleopSend(c, pb.UnitreeCommand_UNITREE_COMMAND_HELLO, 0, 0); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("error %v", err)
	}
	if len(l.log()) != 0 {
		t.Fatal("unauthenticated caller sent robot commands")
	}
}

func TestUnitreeRobotCommandCommandsMapToRobot(t *testing.T) {
	c, l, _ := teleopFixture(t, true, time.Second)
	for _, cmd := range []pb.UnitreeCommand{
		pb.UnitreeCommand_UNITREE_COMMAND_STAND_UP,
		pb.UnitreeCommand_UNITREE_COMMAND_HELLO,
		pb.UnitreeCommand_UNITREE_COMMAND_LIE_DOWN,
		pb.UnitreeCommand_UNITREE_COMMAND_STOP,
	} {
		if err := teleopSend(c, cmd, 0, 0); err != nil {
			t.Fatal(err)
		}
	}
	if got := l.log(); !sameCalls(got, []string{"stand", "hello", "lie", "stop"}) {
		t.Fatalf("calls %v", got)
	}
}

func TestUnitreeRobotCommandAllowsSlowPostureAcknowledgement(t *testing.T) {
	c, l, _ := teleopFixture(t, true, time.Second)
	l.helloDelay = 600 * time.Millisecond
	if err := teleopSend(c, pb.UnitreeCommand_UNITREE_COMMAND_HELLO, 0, 0); err != nil {
		t.Fatalf("slow Hello acknowledgement failed: %v", err)
	}
}

func TestUnitreeRobotCommandMoveExpiresUnlessRefreshed(t *testing.T) {
	c, l, _ := teleopFixture(t, true, 150*time.Millisecond)
	for range 4 {
		if err := teleopSend(c, pb.UnitreeCommand_UNITREE_COMMAND_MOVE, .2, 0); err != nil {
			t.Fatal(err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if got := l.log(); !sameCalls(got, []string{"move", "move", "move", "move"}) {
		t.Fatalf("refreshed move stopped early: %v", got)
	}
	time.Sleep(300 * time.Millisecond)
	if got := l.log(); !sameCalls(got, []string{"move", "move", "move", "move", "stop"}) {
		t.Fatalf("move did not expire into one stop: %v", got)
	}
}

func TestUnitreeRobotCommandStopCancelsPendingExpiry(t *testing.T) {
	c, l, _ := teleopFixture(t, true, 100*time.Millisecond)
	if err := teleopSend(c, pb.UnitreeCommand_UNITREE_COMMAND_MOVE, 0, .5); err != nil {
		t.Fatal(err)
	}
	if err := teleopSend(c, pb.UnitreeCommand_UNITREE_COMMAND_STOP, 0, 0); err != nil {
		t.Fatal(err)
	}
	time.Sleep(250 * time.Millisecond)
	if got := l.log(); !sameCalls(got, []string{"move", "stop"}) {
		t.Fatalf("calls %v", got)
	}
}

func TestUnitreeRobotCommandRejectsInvalidInput(t *testing.T) {
	c, l, _ := teleopFixture(t, true, time.Second)
	for _, v := range [][3]float64{{.81, 0, 0}, {0, .51, 0}, {0, 0, -1.01}, {math.NaN(), 0, 0}, {0, math.Inf(1), 0}} {
		_, err := c.Command(context.Background(), &pb.UnitreeCommandRequest{Serial: "GO2TEST1", Command: pb.UnitreeCommand_UNITREE_COMMAND_MOVE, Forward: v[0], Lateral: v[1], Yaw: v[2]})
		if status.Code(err) != codes.InvalidArgument {
			t.Fatalf("velocity %v accepted: %v", v, err)
		}
	}
	if err := teleopSend(c, pb.UnitreeCommand_UNITREE_COMMAND_UNSPECIFIED, 0, 0); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("unspecified command accepted: %v", err)
	}
	_, err := c.Command(context.Background(), &pb.UnitreeCommandRequest{Serial: "OTHER123", Command: pb.UnitreeCommand_UNITREE_COMMAND_HELLO})
	if status.Code(err) != codes.Unavailable {
		t.Fatalf("unpaired serial accepted: %v", err)
	}
	if got := l.log(); len(got) != 0 {
		t.Fatalf("invalid input sent robot commands: %v", got)
	}
}

func TestUnitreeRobotCommandFailedMoveStops(t *testing.T) {
	c, l, _ := teleopFixture(t, true, time.Second)
	l.failMove = true
	if err := teleopSend(c, pb.UnitreeCommand_UNITREE_COMMAND_MOVE, .1, 0); status.Code(err) != codes.Unavailable {
		t.Fatalf("error %v", err)
	}
	if got := l.log(); !sameCalls(got, []string{"move", "stop"}) {
		t.Fatalf("calls %v", got)
	}
}

func TestUnitreeRobotCommandRejectsForeignTLSOwner(t *testing.T) {
	_, l, p := teleopFixture(t, true, time.Second)
	p.opMu.Lock()
	p.owner = unitree.PairingOwner{OrgID: 42}
	p.opMu.Unlock()
	ctx := unitreeTLSContext(t, "urn:wendy:org:43:user:fixture")
	_, err := NewUnitreeRobotService(p).Command(ctx, &pb.UnitreeCommandRequest{Serial: "GO2TEST1", Command: pb.UnitreeCommand_UNITREE_COMMAND_HELLO})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("foreign TLS owner accepted: %v", err)
	}
	if len(l.log()) != 0 {
		t.Fatal("foreign owner sent robot commands")
	}
}

type peerStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (s peerStream) Context() context.Context { return s.ctx }

func TestUnitreeRobotStreamDeliversTopicsAndVideo(t *testing.T) {
	c, l, _ := teleopFixture(t, true, time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stream, err := c.Stream(ctx, &pb.UnitreeStreamRequest{Serial: "GO2TEST1", Topics: []string{"rt/lf/lowstate"}, Video: true})
	if err != nil {
		t.Fatal(err)
	}
	l.messages <- unitree.TopicMessage{Topic: "rt/lf/lowstate", Data: []byte(`{"x":1}`), Received: time.Unix(0, 42)}
	l.video <- unitree.VideoFrame{AnnexB: []byte{0, 0, 0, 1, 0x65}, RTPTimestamp: 9}
	var topic *pb.UnitreeTopicMessage
	var frame *pb.UnitreeVideoFrame
	for topic == nil || frame == nil {
		m, err := stream.Recv()
		if err != nil {
			t.Fatal(err)
		}
		if m.GetTopic() != nil {
			topic = m.GetTopic()
		}
		if m.GetVideo() != nil {
			frame = m.GetVideo()
		}
	}
	if topic.Topic != "rt/lf/lowstate" || string(topic.Data) != `{"x":1}` || topic.ReceivedUnixNanos != 42 {
		t.Fatalf("topic %v", topic)
	}
	if frame.RtpTimestamp != 9 || len(frame.Annexb) != 5 {
		t.Fatalf("frame %v", frame)
	}
	l.testUnitreeLink.Close()
	if _, err := stream.Recv(); status.Code(err) != codes.Unavailable {
		t.Fatalf("robot disconnect did not end the stream: %v", err)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if !sameCalls(l.subscribed, []string{"rt/lf/lowstate", "video"}) || !l.cancelled {
		t.Fatalf("subscribed %v cancelled %v", l.subscribed, l.cancelled)
	}
}

func TestUnitreeRobotStreamRejectsBadRequests(t *testing.T) {
	for _, tc := range []struct {
		auth bool
		req  *pb.UnitreeStreamRequest
		code codes.Code
	}{
		{false, &pb.UnitreeStreamRequest{Serial: "GO2TEST1", Video: true}, codes.Unauthenticated},
		{true, &pb.UnitreeStreamRequest{Serial: "GO2TEST1", Topics: []string{"../etc"}}, codes.InvalidArgument},
		{true, &pb.UnitreeStreamRequest{Serial: "OTHER123", Video: true}, codes.Unavailable},
	} {
		c, _, _ := teleopFixture(t, tc.auth, time.Second)
		stream, err := c.Stream(context.Background(), tc.req)
		if err == nil {
			_, err = stream.Recv()
		}
		if status.Code(err) != tc.code {
			t.Fatalf("%v: %v", tc.req, err)
		}
	}
}

func TestAvoidanceModeStopsFirst(t *testing.T) {
	for _, fault := range []string{"none", "stop", "avoid"} {
		t.Run(fault, func(t *testing.T) {
			client, link, _ := teleopFixture(t, true, time.Second)
			link.failStop = fault == "stop"
			link.failAvoid = fault == "avoid"
			_, err := client.Command(context.Background(), &pb.UnitreeCommandRequest{
				Serial: "GO2TEST1", Command: pb.UnitreeCommand_UNITREE_COMMAND_SET_OBSTACLE_AVOIDANCE,
				ObstacleAvoidanceEnabled: false,
			})
			if (err != nil) != (fault != "none") {
				t.Fatalf("fault %s: %v", fault, err)
			}
			calls := link.log()
			if len(calls) == 0 || calls[0] != "stop" {
				t.Fatalf("did not stop first: %v", calls)
			}
			if fault == "stop" {
				if len(calls) != 1 {
					t.Fatalf("changed mode after failed stop: %v", calls)
				}
			} else if len(calls) != 2 || calls[1] != "avoid-off" {
				t.Fatalf("wrong mode commands: %v", calls)
			}
		})
	}
}
