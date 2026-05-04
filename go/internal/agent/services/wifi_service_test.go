package services

import (
	"context"
	"net"
	"testing"

	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	agentpb "github.com/wendylabsinc/wendy/proto/gen/agentpb"
	agentpbv2 "github.com/wendylabsinc/wendy/proto/gen/agentpb/v2"
)

func startWiFiServer(t *testing.T, nm NetworkManager) (agentpbv2.WendyWiFiServiceClient, func()) {
	t.Helper()
	lis := bufconn.Listen(bufSize)
	srv := grpc.NewServer()
	svc := NewWiFiService(zap.NewNop(), nm)
	agentpbv2.RegisterWendyWiFiServiceServer(srv, svc)
	go func() { _ = srv.Serve(lis) }()
	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return lis.Dial() }),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("grpc.NewClient: %v", err)
	}
	return agentpbv2.NewWendyWiFiServiceClient(conn), func() {
		conn.Close()
		srv.Stop()
		lis.Close()
	}
}

func TestWiFiService_ListWiFiNetworks(t *testing.T) {
	nets := []*agentpb.ListWiFiNetworksResponse_WiFiNetwork{
		{Ssid: "HomeWiFi", IsConnected: true},
		{Ssid: "OfficeWiFi"},
	}
	client, cleanup := startWiFiServer(t, &mockNetworkManager{networks: nets})
	defer cleanup()

	resp, err := client.ListWiFiNetworks(context.Background(), &agentpbv2.ListWiFiNetworksRequest{})
	if err != nil {
		t.Fatalf("ListWiFiNetworks: %v", err)
	}
	if len(resp.Networks) != 2 {
		t.Fatalf("len(networks) = %d; want 2", len(resp.Networks))
	}
	if resp.Networks[0].Ssid != "HomeWiFi" {
		t.Errorf("networks[0].ssid = %q; want HomeWiFi", resp.Networks[0].Ssid)
	}
	if !resp.Networks[0].IsConnected {
		t.Errorf("networks[0].is_connected = false; want true")
	}
}

func TestWiFiService_ListWiFiNetworks_Unavailable(t *testing.T) {
	client, cleanup := startWiFiServer(t, nil)
	defer cleanup()

	_, err := client.ListWiFiNetworks(context.Background(), &agentpbv2.ListWiFiNetworksRequest{})
	if status.Code(err) != codes.Unavailable {
		t.Errorf("error code = %v; want Unavailable", status.Code(err))
	}
}

func TestWiFiService_ConnectToWiFi(t *testing.T) {
	client, cleanup := startWiFiServer(t, &mockNetworkManager{})
	defer cleanup()

	resp, err := client.ConnectToWiFi(context.Background(), &agentpbv2.ConnectToWiFiRequest{
		Ssid:     "TestNet",
		Password: "secret",
	})
	if err != nil {
		t.Fatalf("ConnectToWiFi: %v", err)
	}
	if !resp.Success {
		t.Errorf("success = false; want true")
	}
}

func TestWiFiService_GetWiFiStatus(t *testing.T) {
	ssid := "HomeWiFi"
	client, cleanup := startWiFiServer(t, &mockNetworkManager{connected: true, ssid: ssid})
	defer cleanup()

	resp, err := client.GetWiFiStatus(context.Background(), &agentpbv2.GetWiFiStatusRequest{})
	if err != nil {
		t.Fatalf("GetWiFiStatus: %v", err)
	}
	if !resp.Connected {
		t.Errorf("connected = false; want true")
	}
	if resp.Ssid == nil || *resp.Ssid != ssid {
		t.Errorf("ssid = %v; want %q", resp.Ssid, ssid)
	}
}
