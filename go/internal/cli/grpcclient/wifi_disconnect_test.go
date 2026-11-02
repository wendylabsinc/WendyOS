package grpcclient

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/types/known/emptypb"
)

type wifiDisconnectClientServer struct {
	agentpbv2.UnimplementedWendyWiFiServiceServer
	run func(grpc.BidiStreamingServer[agentpbv2.DisconnectWiFiControl, agentpbv2.DisconnectWiFiEvent]) error
}

func (s *wifiDisconnectClientServer) DisconnectWiFiAcknowledged(stream grpc.BidiStreamingServer[agentpbv2.DisconnectWiFiControl, agentpbv2.DisconnectWiFiEvent]) error {
	return s.run(stream)
}

type wifiDisconnectLegacyServer struct {
	agentpb.UnimplementedWendyAgentServiceServer
	run   func(context.Context) (*agentpb.DisconnectWiFiResponse, error)
	calls atomic.Int32
}

func (s *wifiDisconnectLegacyServer) DisconnectWiFi(ctx context.Context, _ *agentpb.DisconnectWiFiRequest) (*agentpb.DisconnectWiFiResponse, error) {
	s.calls.Add(1)
	return s.run(ctx)
}

func startWiFiDisconnectClients(t *testing.T, run func(grpc.BidiStreamingServer[agentpbv2.DisconnectWiFiControl, agentpbv2.DisconnectWiFiEvent]) error, legacyRun func(context.Context) (*agentpb.DisconnectWiFiResponse, error)) (*AgentConnection, *wifiDisconnectLegacyServer) {
	t.Helper()
	lis := bufconn.Listen(1024 * 1024)
	server := grpc.NewServer(grpc.WaitForHandlers(true))
	if run != nil {
		agentpbv2.RegisterWendyWiFiServiceServer(server, &wifiDisconnectClientServer{run: run})
	}
	legacy := &wifiDisconnectLegacyServer{run: legacyRun}
	if legacyRun != nil {
		agentpb.RegisterWendyAgentServiceServer(server, legacy)
	}
	go func() { _ = server.Serve(lis) }()
	conn, err := grpc.NewClient("passthrough:///wifi-disconnect-client",
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return lis.Dial() }),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close(); server.Stop(); _ = lis.Close() })
	return NewFromConn(conn), legacy
}

func acceptWiFiDisconnectClient(stream grpc.BidiStreamingServer[agentpbv2.DisconnectWiFiControl, agentpbv2.DisconnectWiFiEvent]) error {
	begin, err := stream.Recv()
	if err != nil {
		return err
	}
	if begin.GetBegin() == nil {
		return status.Error(codes.InvalidArgument, "missing begin")
	}
	if err := stream.Send(&agentpbv2.DisconnectWiFiEvent{Event: &agentpbv2.DisconnectWiFiEvent_Ready{Ready: &emptypb.Empty{}}}); err != nil {
		return err
	}
	confirm, err := stream.Recv()
	if err != nil {
		return err
	}
	if confirm.GetConfirm() == nil {
		return status.Error(codes.InvalidArgument, "missing confirm")
	}
	return nil
}

func TestWiFiDisconnectClientObservesConfirmationBeforeClosing(t *testing.T) {
	confirmed, stopped := make(chan struct{}), make(chan struct{})
	connection, _ := startWiFiDisconnectClients(t, func(stream grpc.BidiStreamingServer[agentpbv2.DisconnectWiFiControl, agentpbv2.DisconnectWiFiEvent]) error {
		defer close(stopped)
		if err := acceptWiFiDisconnectClient(stream); err != nil {
			return err
		}
		close(confirmed)
		<-stream.Context().Done()
		return stream.Context().Err()
	}, nil)
	result, err := disconnectWiFi(context.Background(), agentpbv2.NewWendyWiFiServiceClient(connection.Conn), connection.AgentService, 30*time.Millisecond)
	if err != nil || result.State != "requested" || !result.Acknowledged || result.CompletionConfirmed || result.Success != nil {
		t.Fatalf("result = %+v, error = %v", result, err)
	}
	select {
	case <-confirmed:
	default:
		t.Fatal("client closed before the server could consume confirmation")
	}
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("observation expiry did not clean up its stream")
	}
}

func TestWiFiDisconnectClientCancellationIsNeverSuccess(t *testing.T) {
	for _, afterReady := range []bool{false, true} {
		t.Run(map[bool]string{false: "before ready", true: "after confirmation"}[afterReady], func(t *testing.T) {
			reached := make(chan struct{})
			connection, _ := startWiFiDisconnectClients(t, func(stream grpc.BidiStreamingServer[agentpbv2.DisconnectWiFiControl, agentpbv2.DisconnectWiFiEvent]) error {
				if afterReady {
					if err := acceptWiFiDisconnectClient(stream); err != nil {
						return err
					}
				} else if _, err := stream.Recv(); err != nil {
					return err
				}
				close(reached)
				<-stream.Context().Done()
				return stream.Context().Err()
			}, nil)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			type response struct {
				result WiFiDisconnectResult
				err    error
			}
			done := make(chan response, 1)
			go func() { result, err := connection.DisconnectWiFi(ctx); done <- response{result, err} }()
			select {
			case <-reached:
			case <-time.After(time.Second):
				t.Fatal("request did not reach its intended phase")
			}
			cancel()
			got := <-done
			if !errors.Is(got.err, context.Canceled) || got.result.State != "unknown" || got.result.CompletionConfirmed || got.result.Success != nil || got.result.Acknowledged != afterReady {
				t.Fatalf("cancellation became success: result = %+v, error = %v", got.result, got.err)
			}
		})
	}
}

func TestWiFiDisconnectClientOldServerFallback(t *testing.T) {
	for _, mode := range []string{"success", "operation error", "lost reply", "unsupported", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			called := make(chan struct{}, 1)
			connection, legacy := startWiFiDisconnectClients(t, nil, func(ctx context.Context) (*agentpb.DisconnectWiFiResponse, error) {
				called <- struct{}{}
				switch mode {
				case "operation error":
					message := "nmcli failed"
					return &agentpb.DisconnectWiFiResponse{ErrorMessage: &message}, nil
				case "lost reply":
					return nil, status.Error(codes.Unavailable, "reply path lost")
				case "unsupported":
					return nil, status.Error(codes.Unimplemented, "not supported")
				case "cancel":
					<-ctx.Done()
					return nil, ctx.Err()
				}
				return &agentpb.DisconnectWiFiResponse{Success: true}, nil
			})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if mode == "cancel" {
				go func() { <-called; cancel() }()
			}
			result, err := connection.DisconnectWiFi(ctx)
			if !result.Legacy || result.Acknowledged || result.Warning == "" || legacy.calls.Load() != 1 {
				t.Fatalf("legacy invocation = %+v, error = %v, calls = %d", result, err, legacy.calls.Load())
			}
			switch mode {
			case "success":
				if err != nil || !result.CompletionConfirmed || result.Success == nil || !*result.Success {
					t.Fatalf("legacy success = %+v, %v", result, err)
				}
			case "operation error":
				if err == nil || !strings.Contains(err.Error(), "nmcli failed") || !result.CompletionConfirmed || result.Success == nil || *result.Success {
					t.Fatalf("legacy NM error = %+v, %v", result, err)
				}
			case "cancel":
				if !errors.Is(err, context.Canceled) || result.CompletionConfirmed {
					t.Fatalf("legacy cancellation = %+v, %v", result, err)
				}
			case "unsupported":
				if status.Code(err) != codes.Unimplemented || !strings.Contains(err.Error(), "does not support") || result.CompletionConfirmed {
					t.Fatalf("legacy unsupported = %+v, %v", result, err)
				}
			default:
				if err == nil || result.State != "unknown" || result.CompletionConfirmed || result.Success != nil {
					t.Fatalf("lost legacy reply fabricated success = %+v, %v", result, err)
				}
			}
		})
	}
}

func TestWiFiDisconnectClientSkipsFutureEvents(t *testing.T) {
	connection, _ := startWiFiDisconnectClients(t, func(stream grpc.BidiStreamingServer[agentpbv2.DisconnectWiFiControl, agentpbv2.DisconnectWiFiEvent]) error {
		if _, err := stream.Recv(); err != nil {
			return err
		}
		future := &agentpbv2.DisconnectWiFiEvent{}
		future.ProtoReflect().SetUnknown(protowire.AppendBytes(protowire.AppendTag(nil, 100, protowire.BytesType), nil))
		if err := stream.Send(future); err != nil {
			return err
		}
		if err := stream.Send(&agentpbv2.DisconnectWiFiEvent{Event: &agentpbv2.DisconnectWiFiEvent_Ready{Ready: &emptypb.Empty{}}}); err != nil {
			return err
		}
		if confirm, err := stream.Recv(); err != nil || confirm.GetConfirm() == nil {
			return status.Error(codes.InvalidArgument, "missing confirmation")
		}
		if err := stream.Send(future); err != nil {
			return err
		}
		return stream.Send(&agentpbv2.DisconnectWiFiEvent{Event: &agentpbv2.DisconnectWiFiEvent_Completed{Completed: &agentpbv2.DisconnectWiFiResponse{Success: true}}})
	}, nil)
	result, err := connection.DisconnectWiFi(context.Background())
	if err != nil || !result.Acknowledged || !result.CompletionConfirmed || result.Success == nil || !*result.Success || result.Legacy {
		t.Fatalf("future event interrupted known completion: %+v, %v", result, err)
	}
}

func TestWiFiDisconnectClientDefiniteRejectionAfterReadyIsError(t *testing.T) {
	connection, _ := startWiFiDisconnectClients(t, func(stream grpc.BidiStreamingServer[agentpbv2.DisconnectWiFiControl, agentpbv2.DisconnectWiFiEvent]) error {
		if err := acceptWiFiDisconnectClient(stream); err != nil {
			return err
		}
		return status.Error(codes.InvalidArgument, "confirmation refused")
	}, nil)
	result, err := connection.DisconnectWiFi(context.Background())
	if status.Code(err) != codes.InvalidArgument || !result.Acknowledged || result.CompletionConfirmed || result.Success != nil {
		t.Fatalf("definite rejection = %+v, %v", result, err)
	}
}
