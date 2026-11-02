package mcp

import (
	"context"
	"encoding/json"
	"net"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/types/known/emptypb"
)

type mcpWiFiDisconnectServer struct {
	agentpbv2.UnimplementedWendyWiFiServiceServer
	mode   string
	cancel context.CancelFunc
}

func (s *mcpWiFiDisconnectServer) DisconnectWiFiAcknowledged(stream grpc.BidiStreamingServer[agentpbv2.DisconnectWiFiControl, agentpbv2.DisconnectWiFiEvent]) error {
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
	if s.mode == "cancel" {
		s.cancel()
		<-stream.Context().Done()
		return stream.Context().Err()
	}
	if s.mode == "requested" {
		return status.Error(codes.Unavailable, "WiFi path removed")
	}
	completed := &agentpbv2.DisconnectWiFiResponse{Success: true}
	if s.mode == "operation error" {
		completed.Success = false
		message := "nmcli failed"
		completed.ErrorMessage = &message
	}
	return stream.Send(&agentpbv2.DisconnectWiFiEvent{Event: &agentpbv2.DisconnectWiFiEvent_Completed{Completed: completed}})
}

func TestMCPWiFiDisconnectReportsOutcome(t *testing.T) {
	for _, mode := range []string{"completed", "requested", "unacknowledged", "operation error", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			listener := bufconn.Listen(1024 * 1024)
			server := grpc.NewServer()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			agentpbv2.RegisterWendyWiFiServiceServer(server, &mcpWiFiDisconnectServer{mode: mode, cancel: cancel})
			go func() { _ = server.Serve(listener) }()
			defer server.Stop()
			defer listener.Close()
			connection, err := grpc.NewClient("passthrough:///wifi-mcp", grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }))
			if err != nil {
				t.Fatal(err)
			}
			defer connection.Close()
			s := New(&config.Config{}, nil)
			s.SetConn(grpcclient.NewFromConn(connection))
			result, err := s.callTool(ctx, "wifi_disconnect", nil)
			if err != nil {
				t.Fatal(err)
			}
			raw, err := json.Marshal(result.StructuredContent)
			if err != nil {
				t.Fatal(err)
			}
			var outcome grpcclient.WiFiDisconnectResult
			if result.IsError {
				var envelope struct {
					Code   string                          `json:"error_code"`
					Result grpcclient.WiFiDisconnectResult `json:"result"`
				}
				if err := json.Unmarshal(raw, &envelope); err != nil {
					t.Fatal(err)
				}
				outcome = envelope.Result
				if mode == "cancel" && envelope.Code != string(errCodeCancelled) {
					t.Fatalf("cancellation error code = %s", envelope.Code)
				}
			} else if err := json.Unmarshal(raw, &outcome); err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "completed":
				if result.IsError || !outcome.CompletionConfirmed || outcome.Success == nil || !*outcome.Success {
					t.Fatalf("completed = %+v, error = %v", outcome, result.IsError)
				}
			case "requested":
				if result.IsError || !outcome.Acknowledged || outcome.CompletionConfirmed || outcome.Success != nil || outcome.State != "requested" {
					t.Fatalf("requested = %+v, error = %v", outcome, result.IsError)
				}
			case "operation error":
				if !result.IsError || !outcome.CompletionConfirmed || outcome.Success == nil || *outcome.Success {
					t.Fatalf("operation error = %+v, error = %v", outcome, result.IsError)
				}
			default:
				if !result.IsError || outcome.State != "unknown" || outcome.CompletionConfirmed || outcome.Success != nil {
					t.Fatalf("unknown/canceled = %+v, error = %v", outcome, result.IsError)
				}
			}
		})
	}
}
