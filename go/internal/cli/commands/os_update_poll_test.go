package commands

import (
	"context"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

type osUpdatePollServer struct {
	agentpb.UnimplementedWendyAgentServiceServer
	statusDelay time.Duration
	outcome     agentpb.GetOSUpdateStatusResponse_Outcome
	versionErr  error
	versionRead atomic.Int32
}

func (s *osUpdatePollServer) GetOSUpdateStatus(ctx context.Context, req *agentpb.GetOSUpdateStatusRequest) (*agentpb.GetOSUpdateStatusResponse, error) {
	if req.GetIncludeEngineStatus() {
		select {
		case <-ctx.Done():
			return nil, status.FromContextError(ctx.Err()).Err()
		case <-time.After(s.statusDelay):
		}
	}
	return &agentpb.GetOSUpdateStatusResponse{
		HasResult: true, Outcome: s.outcome, CreatedAtUnix: time.Now().Unix(),
		OldOsVersion: "WendyOS-0.19.3", NewOsVersion: "WendyOS-0.19.3",
		EngineStatus: &agentpb.OSUpdateEngineStatus{CurrentSlot: "B"},
	}, nil
}

func (s *osUpdatePollServer) GetAgentVersion(context.Context, *agentpb.GetAgentVersionRequest) (*agentpb.GetAgentVersionResponse, error) {
	s.versionRead.Add(1)
	if s.versionErr != nil {
		return nil, s.versionErr
	}
	v := "WendyOS-0.19.3"
	return &agentpb.GetAgentVersionResponse{OsVersion: &v}, nil
}

// Exercise the real polling loop over gRPC without device discovery or
// credentials. bufconn lets synctest advance the RPC deadlines without sleeps.
func startOSUpdatePollServer(t *testing.T, server *osUpdatePollServer) func() *grpcclient.AgentConnection {
	t.Helper()
	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer()
	agentpb.RegisterWendyAgentServiceServer(srv, server)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(func() { srv.Stop(); _ = lis.Close() })
	connect := func() *grpcclient.AgentConnection {
		cc, err := grpc.NewClient("passthrough:///os-update-test",
			grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return lis.Dial() }),
			grpc.WithTransportCredentials(insecure.NewCredentials()))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = cc.Close() })
		return &grpcclient.AgentConnection{Conn: cc, AgentService: agentpb.NewWendyAgentServiceClient(cc)}
	}
	t.Setenv("WENDY_AGENT_SOCKET", "")
	previous := dialAgentLadderFn
	t.Cleanup(func() { dialAgentLadderFn = previous })
	dialAgentLadderFn = func(context.Context, dialTarget) (*grpcclient.AgentConnection, error, error) {
		return connect(), nil, nil
	}
	return connect
}

func TestReportOSUpdateOutcomeWithSlowEngineStatus(t *testing.T) {
	for _, tc := range []struct {
		name       string
		delay      time.Duration
		outcome    agentpb.GetOSUpdateStatusResponse_Outcome
		versionErr error
		wantErr    string
	}{
		{"slow rollback", 6 * time.Second, agentpb.GetOSUpdateStatusResponse_OUTCOME_ROLLED_BACK, nil, "rolled back"},
		{"slow commit", 6 * time.Second, agentpb.GetOSUpdateStatusResponse_OUTCOME_COMMITTED, nil, ""},
		{"status timeout preserves version query", 20 * time.Second, agentpb.GetOSUpdateStatusResponse_OUTCOME_COMMITTED, nil, "unchanged version"},
		{"neither result nor version", 20 * time.Second, agentpb.GetOSUpdateStatusResponse_OUTCOME_COMMITTED, status.Error(codes.Unavailable, "version unavailable"), "no usable result or running OS version"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				server := &osUpdatePollServer{statusDelay: tc.delay, outcome: tc.outcome, versionErr: tc.versionErr}
				startOSUpdatePollServer(t, server)
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
				defer cancel()
				baseline := osUpdateBaseline{preVersion: "WendyOS-0.19.3", recordCleared: true, bootedSlot: "A"}
				err := reportOSUpdateOutcome(ctx, &grpcclient.AgentConnection{Host: "127.0.0.1"}, baseline.preVersion, baseline)
				if tc.wantErr == "" {
					if err != nil {
						t.Fatal(err)
					}
				} else if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error=%v; want %q", err, tc.wantErr)
				}
				if server.versionRead.Load() == 0 {
					t.Fatal("engine probe prevented the independent version query")
				}
			})
		})
	}
}

func TestReadOSUpdateBaselineWithSlowEngineStatus(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		connect := startOSUpdatePollServer(t, &osUpdatePollServer{statusDelay: 6 * time.Second, outcome: agentpb.GetOSUpdateStatusResponse_OUTCOME_COMMITTED})
		baseline := readOSUpdateBaseline(context.Background(), connect())
		if baseline.bootedSlot != "B" || baseline.resultAtUnix == 0 {
			t.Fatalf("slow engine probe lost the pre-update baseline: %+v", baseline)
		}
	})
}
