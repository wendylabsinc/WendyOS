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
	release chan struct{}
	calls   chan string
}

func (a *mobileOSAgent) GetOSUpdateStatus(context.Context, *pb.GetOSUpdateStatusRequest) (*pb.GetOSUpdateStatusResponse, error) {
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
func TestMobileOSUpdateSessionAndConcurrentStart(t *testing.T) {
	listener := bufconn.Listen(1 << 20)
	server := grpc.NewServer()
	agent := &mobileOSAgent{release: make(chan struct{}), calls: make(chan string, 4)}
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
