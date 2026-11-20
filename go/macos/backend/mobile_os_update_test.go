package backend

import (
	"context"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
)

func TestMobileOSUpdatePreflight(t *testing.T) {
	ready := &pb.GetOSUpdateStatusResponse{EngineStatus: &pb.OSUpdateEngineStatus{Connector: "tegrauefi"}}
	if err := validateOSUpdateStart("https://example.test/update.wendy", ready); err != nil {
		t.Fatal(err)
	}
	for _, artifact := range []string{"", "file:///tmp/a", "https://user:pass@example.test/a", "https:///a"} {
		if err := validateOSUpdateStart(artifact, ready); err == nil {
			t.Fatalf("accepted %q", artifact)
		}
	}
	if err := validateOSUpdateStart("https://example.test/a", &pb.GetOSUpdateStatusResponse{}); err == nil {
		t.Fatal("accepted missing updater")
	}
	ready.EngineStatus.Pending = &pb.OSUpdateEngineStatus_PendingUpdate{Phase: "installed"}
	if err := validateOSUpdateStart("https://example.test/a", ready); err == nil {
		t.Fatal("accepted pending update")
	}
}
func TestMobileOSUpdateStreamRequiresExplicitCompletion(t *testing.T) {
	frames := []*pb.UpdateOSResponse{{ResponseType: &pb.UpdateOSResponse_Progress_{Progress: &pb.UpdateOSResponse_Progress{Phase: "write", Percent: 30}}}, {ResponseType: &pb.UpdateOSResponse_Completed_{Completed: &pb.UpdateOSResponse_Completed{RebootRequired: true}}}}
	recv := func() (*pb.UpdateOSResponse, error) {
		if len(frames) == 0 {
			return nil, io.EOF
		}
		next := frames[0]
		frames = frames[1:]
		return next, nil
	}
	var progress OSInstallState
	result := consumeOSUpdate(recv, OSInstallState{Active: true, Baseline: 10}, func(s OSInstallState) { progress = s })
	if progress.Percent != 30 || !progress.Active || result.Active || !result.RebootRequired || result.Error != "" || result.Baseline != 10 {
		t.Fatalf("wrong state: %+v %+v", progress, result)
	}
	result = consumeOSUpdate(recv, OSInstallState{Active: true}, func(OSInstallState) {})
	if result.Error == "" || result.RebootRequired || result.Active {
		t.Fatalf("EOF incorrectly accepted: %+v", result)
	}
}

type mobileOSAgent struct {
	pb.UnimplementedWendyOSUpdateServiceServer
	release      chan struct{}
	calls        chan string
	probeStarted chan struct{}
	probeGate    chan struct{}
}

func (a *mobileOSAgent) GetOSUpdateStatus(ctx context.Context, _ *pb.GetOSUpdateStatusRequest) (*pb.GetOSUpdateStatusResponse, error) {
	if a.probeStarted != nil {
		close(a.probeStarted)
		select {
		case <-a.probeGate:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return &pb.GetOSUpdateStatusResponse{HasResult: true, CreatedAtUnix: 12, Outcome: pb.GetOSUpdateStatusResponse_OUTCOME_ROLLED_BACK, EngineStatus: &pb.OSUpdateEngineStatus{Connector: "tegrauefi"}}, nil
}
func (a *mobileOSAgent) UpdateOS(req *pb.UpdateOSRequest, stream grpc.ServerStreamingServer[pb.UpdateOSResponse]) error {
	a.calls <- req.ArtifactUrl
	stream.Send(&pb.UpdateOSResponse{ResponseType: &pb.UpdateOSResponse_Progress_{Progress: &pb.UpdateOSResponse_Progress{Phase: "download", Percent: 20}}})
	select {
	case <-a.release:
	case <-stream.Context().Done():
		return stream.Context().Err()
	}
	return stream.Send(&pb.UpdateOSResponse{ResponseType: &pb.UpdateOSResponse_Completed_{Completed: &pb.UpdateOSResponse_Completed{RebootRequired: true}}})
}
func newMobileOSBackend(t *testing.T, agent *mobileOSAgent) *Backend {
	t.Helper()
	listener := bufconn.Listen(1 << 20)
	server := grpc.NewServer()
	pb.RegisterWendyOSUpdateServiceServer(server, agent)
	go server.Serve(listener)
	conn, err := grpc.NewClient("passthrough:///os-update", grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return listener.DialContext(ctx) }))
	if err != nil {
		t.Fatal(err)
	}
	b := New()
	b.conn = grpcclient.NewFromConn(conn)
	b.session = 7
	b.life, b.stop = context.WithCancel(context.Background())
	t.Cleanup(func() { b.Shutdown(); server.Stop(); listener.Close() })
	return b
}
func TestMobileOSUpdateSessionAndConcurrentStart(t *testing.T) {
	agent := &mobileOSAgent{release: make(chan struct{}), calls: make(chan string, 4)}
	b := newMobileOSBackend(t, agent)
	for _, session := range []uint64{0, 6, 8} {
		if _, err := b.MobileOSUpdate(session, "https://example.test/os.wendy", true); err == nil {
			t.Fatalf("accepted stale session %d", session)
		}
	}
	value, err := b.MobileOSUpdate(7, "https://example.test/os.wendy", true)
	if err != nil {
		t.Fatal(err)
	}
	if !value.Install.Active || value.Install.Baseline != 12 {
		t.Fatalf("wrong start %+v", value)
	}
	if _, err = b.MobileOSUpdate(7, "https://example.test/second.wendy", true); err == nil {
		t.Fatal("accepted duplicate update")
	}
	if got := <-agent.calls; got != "https://example.test/os.wendy" {
		t.Fatal(got)
	}
	close(agent.release)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		value, err = b.MobileOSUpdate(7, "", false)
		if err != nil {
			t.Fatal(err)
		}
		if !value.Install.Active {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if value.Install.Active || !value.Install.RebootRequired || !strings.Contains(string(value.Status), "ROLLED_BACK") {
		t.Fatalf("lost progress or prior result: %+v", value)
	}
}

func TestMobileOSUpdateExpirationCancelsPreflight(t *testing.T) {
	agent := &mobileOSAgent{probeStarted: make(chan struct{}), probeGate: make(chan struct{}), calls: make(chan string, 1)}
	b := newMobileOSBackend(t, agent)
	done := make(chan error, 1)
	go func() { _, err := b.MobileOSUpdate(7, "https://example.test/os.wendy", true); done <- err }()
	select {
	case <-agent.probeStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("preflight did not start")
	}
	b.CancelMobileOSUpdate(7)
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancelled preflight started installation")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancellation blocked on request lock")
	}
	select {
	case <-agent.calls:
		t.Fatal("install RPC was called")
	default:
	}
}
func TestMobileOSUpdateExpirationCancelsLiveStream(t *testing.T) {
	agent := &mobileOSAgent{release: make(chan struct{}), calls: make(chan string, 1)}
	b := newMobileOSBackend(t, agent)
	if _, err := b.MobileOSUpdate(7, "https://example.test/os.wendy", true); err != nil {
		t.Fatal(err)
	}
	select {
	case <-agent.calls:
	case <-time.After(2 * time.Second):
		t.Fatal("install did not start")
	}
	b.CancelMobileOSUpdate(7)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		value, err := b.MobileOSUpdate(7, "", false)
		if err != nil {
			t.Fatal(err)
		}
		if !value.Install.Active {
			if value.Install.Error == "" || value.Install.RebootRequired {
				t.Fatalf("cancellation reported success: %+v", value)
			}
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("stream remained active after cancellation")
}

func TestMobileOSUpdateExpirationBeforeBridgeStarts(t *testing.T) {
	agent := &mobileOSAgent{release: make(chan struct{}), calls: make(chan string, 1)}
	b := newMobileOSBackend(t, agent)
	b.PrepareMobileOSUpdate(7)
	b.CancelMobileOSUpdate(7)
	if _, err := b.MobileOSUpdate(7, "https://example.test/os.wendy", true); err != context.Canceled {
		t.Fatalf("queued update ignored cancellation: %v", err)
	}
	b.PrepareMobileOSUpdate(7)
	if _, err := b.MobileOSUpdate(7, "https://example.test/os.wendy", true); err != nil {
		t.Fatalf("fresh request could not start: %v", err)
	}
	b.CancelMobileOSUpdate(7)
}
