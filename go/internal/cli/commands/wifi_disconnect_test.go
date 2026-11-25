package commands

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/types/known/emptypb"
)

type commandWiFiDisconnectServer struct {
	agentpbv2.UnimplementedWendyWiFiServiceServer
	mode string
}

func (s *commandWiFiDisconnectServer) DisconnectWiFiAcknowledged(stream grpc.BidiStreamingServer[agentpbv2.DisconnectWiFiControl, agentpbv2.DisconnectWiFiEvent]) error {
	if _, err := stream.Recv(); err != nil {
		return err
	}
	if s.mode == "unacknowledged" {
		return status.Error(codes.FailedPrecondition, "WiFi unavailable")
	}
	if err := stream.Send(&agentpbv2.DisconnectWiFiEvent{Event: &agentpbv2.DisconnectWiFiEvent_Ready{Ready: &emptypb.Empty{}}}); err != nil {
		return err
	}
	if confirm, err := stream.Recv(); err != nil || confirm.GetConfirm() == nil {
		return status.Error(codes.InvalidArgument, "missing confirmation")
	}
	if s.mode == "requested" {
		return status.Error(codes.Unavailable, "WiFi path removed")
	}
	return stream.Send(&agentpbv2.DisconnectWiFiEvent{Event: &agentpbv2.DisconnectWiFiEvent_Completed{Completed: &agentpbv2.DisconnectWiFiResponse{Success: true}}})
}

func TestWiFiDisconnectCommandReportsOutcome(t *testing.T) {
	for _, mode := range []string{"completed", "requested", "unacknowledged"} {
		t.Run(mode, func(t *testing.T) {
			listener := bufconn.Listen(1024 * 1024)
			server := grpc.NewServer()
			agentpbv2.RegisterWendyWiFiServiceServer(server, &commandWiFiDisconnectServer{mode: mode})
			go func() { _ = server.Serve(listener) }()
			defer server.Stop()
			defer listener.Close()
			connection, err := grpc.NewClient("passthrough:///wifi-command", grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }))
			if err != nil {
				t.Fatal(err)
			}
			defer connection.Close()
			stubWifiCommandTarget(t, &SelectedDevice{Agent: grpcclient.NewFromConn(connection)}, nil)
			oldJSON := jsonOutput
			jsonOutput = true
			t.Cleanup(func() { jsonOutput = oldJSON })
			var output bytes.Buffer
			cmd := newWifiCmd()
			cmd.SilenceUsage = true // The real root command sets this policy.
			cmd.SetOut(&output)
			cmd.SetErr(io.Discard)
			cmd.SetArgs([]string{"disconnect"})
			err = cmd.ExecuteContext(context.Background())
			var result grpcclient.WiFiDisconnectResult
			if parseErr := json.Unmarshal(output.Bytes(), &result); parseErr != nil {
				t.Fatalf("invalid outcome JSON %q: %v", output.String(), parseErr)
			}
			if mode == "unacknowledged" {
				if err == nil || result.State != "unknown" || result.Acknowledged || result.Success != nil {
					t.Fatalf("unacknowledged = %+v, error = %v", result, err)
				}
			} else if err != nil || result.State != mode || !result.Acknowledged || result.CompletionConfirmed != (mode == "completed") {
				t.Fatalf("outcome = %+v, error = %v", result, err)
			}
			if mode == "requested" && result.Success != nil {
				t.Fatal("unknown completion was marked successful")
			}
		})
	}
}
