//go:build !windows

package sessionbroker

import (
	"context"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
)

type disconnectWiFiProxyServer struct {
	agentpbv2.UnimplementedWendyWiFiServiceServer
	loseCompletion bool
	confirmations  atomic.Int32
}

func (s *disconnectWiFiProxyServer) DisconnectWiFiAcknowledged(stream grpc.BidiStreamingServer[agentpbv2.DisconnectWiFiControl, agentpbv2.DisconnectWiFiEvent]) error {
	begin, err := stream.Recv()
	if err != nil || begin.GetBegin() == nil {
		return status.Error(codes.InvalidArgument, "missing begin")
	}
	if err := stream.Send(&agentpbv2.DisconnectWiFiEvent{Event: &agentpbv2.DisconnectWiFiEvent_Ready{Ready: &emptypb.Empty{}}}); err != nil {
		return err
	}
	confirm, err := stream.Recv()
	if err != nil || confirm.GetConfirm() == nil {
		return status.Error(codes.InvalidArgument, "missing confirmation")
	}
	s.confirmations.Add(1)
	if s.loseCompletion {
		return status.Error(codes.Unavailable, "WiFi path removed")
	}
	return stream.Send(&agentpbv2.DisconnectWiFiEvent{Event: &agentpbv2.DisconnectWiFiEvent_Completed{Completed: &agentpbv2.DisconnectWiFiResponse{Success: true}}})
}

func TestSessionBrokerForwardsWiFiDisconnectAcknowledgement(t *testing.T) {
	for _, loseCompletion := range []bool{false, true} {
		t.Run(map[bool]string{false: "completed", true: "lost completion"}[loseCompletion], func(t *testing.T) {
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			server := grpc.NewServer()
			wifi := &disconnectWiFiProxyServer{loseCompletion: loseCompletion}
			agentpbv2.RegisterWendyWiFiServiceServer(server, wifi)
			agentpb.RegisterWendyAgentServiceServer(server, testAgentServer{})
			go func() { _ = server.Serve(listener) }()
			defer server.Stop()
			defer listener.Close()
			upstream, err := grpc.NewClient(listener.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
			if err != nil {
				t.Fatal(err)
			}
			defer upstream.Close()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			spec := Spec{Key: "wifi-disconnect.local", Host: "wifi-disconnect.local", Addr: listener.Addr().String(), CertFingerprint: "fixture", Expected: certs.WendyIdentity{OrgID: 7, EntityType: "asset", EntityID: "42"}}
			socket, _, done := startServing(t, ctx, shortTempDir(t), spec, upstream, time.Minute)
			defer func() {
				cancel()
				select {
				case <-done:
				case <-time.After(time.Second):
					t.Error("test broker did not stop")
				}
			}()
			proxy, err := grpcclient.ConnectSessionProxy(context.Background(), socket, spec.Host, spec.Addr, &config.CertificateInfo{OrganizationID: 7}, spec.Expected)
			if err != nil {
				t.Fatal(err)
			}
			defer proxy.Close()
			call, cancelCall := context.WithTimeout(context.Background(), time.Second)
			defer cancelCall()
			result, err := proxy.DisconnectWiFi(call)
			if err != nil || !result.Acknowledged || result.CompletionConfirmed == loseCompletion || wifi.confirmations.Load() != 1 {
				t.Fatalf("proxied result = %+v, error = %v, confirmations = %d", result, err, wifi.confirmations.Load())
			}
			if loseCompletion && (result.Success != nil || result.State != "requested") {
				t.Fatalf("proxy fabricated completion: %+v", result)
			}
		})
	}
}
