package backend

import (
	"context"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/data/thermal"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

type recoveringThermalAgent struct {
	pb.UnimplementedWendyVideoServiceServer
	attempts  atomic.Int32
	stall     bool
	permanent bool
}

func (a *recoveringThermalAgent) StreamVideo(_ *pb.StreamVideoRequest, s grpc.ServerStreamingServer[pb.VideoFrame]) error {
	if a.permanent {
		a.attempts.Add(1)
		return status.Error(codes.FailedPrecondition, "raw capture unavailable")
	}
	if a.attempts.Add(1) == 1 {
		if a.stall {
			<-s.Context().Done()
			return s.Context().Err()
		}
		return status.Error(codes.Unavailable, "relay interrupted")
	}
	if err := s.Send(thermalFixture()); err != nil {
		return err
	}
	<-s.Context().Done()
	return s.Context().Err()
}

func TestThermalStreamRecoversAndCancels(t *testing.T) {
	for _, mode := range []string{"interrupted", "stalled", "permanent"} {
		t.Run(mode, func(t *testing.T) {
			listener := bufconn.Listen(1 << 20)
			defer listener.Close()
			server := grpc.NewServer()
			agent := &recoveringThermalAgent{stall: mode == "stalled", permanent: mode == "permanent"}
			pb.RegisterWendyVideoServiceServer(server, agent)
			go server.Serve(listener)
			defer server.Stop()
			conn, err := grpc.NewClient("passthrough:///recovery", grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return listener.DialContext(ctx) }))
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			b := New()
			done := make(chan struct{})
			go func() {
				defer close(done)
				b.runThermalCamera(ctx, pb.NewWendyVideoServiceClient(conn), &pb.StreamVideoRequest{AdaptiveFramerate: true}, &pb.ThermalCamera{Adapter: thermal.PureThermal}, 7, 42, 100*time.Millisecond)
			}()
			select {
			case e := <-b.events:
				want := EventThermal
				if agent.permanent {
					want = EventCameraError
				}
				if e.Kind != want || e.Session != 7 || e.Stream != 42 {
					t.Fatalf("unexpected event: %+v", e)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("no recovery result")
			}
			cancel()
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("recovery did not cancel")
			}
			wantAttempts := int32(2)
			if agent.permanent {
				wantAttempts = 1
			}
			if got := agent.attempts.Load(); got != wantAttempts {
				t.Fatalf("attempts = %d, want %d", got, wantAttempts)
			}
		})
	}
}
