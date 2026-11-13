package commands

import (
	"context"
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type osDownloadClient struct {
	agentpb.WendyAgentServiceClient
	wifiConnected bool
	wifiErr       error
	wifiCalls     int
}

func (c *osDownloadClient) GetWiFiStatus(context.Context, *agentpb.GetWiFiStatusRequest, ...grpc.CallOption) (*agentpb.GetWiFiStatusResponse, error) {
	c.wifiCalls++
	return &agentpb.GetWiFiStatusResponse{Connected: c.wifiConnected}, c.wifiErr
}

func TestOSArtifactDownloadRouting(t *testing.T) {
	for _, tt := range []struct {
		name          string
		host          string
		cloud         bool
		cloudContext  bool
		addresses     []string
		wifiConnected bool
		wifiErr       error
		wantLocal     bool
		wantWiFiCalls int
	}{
		{name: "cloud default without WiFi", host: "ci-mesh-2", cloud: true},
		{name: "legacy cloud wrapper", host: "ci-mesh-2", cloudContext: true},
		{name: "Ethernet IPv4 without WiFi", host: "jetson.local", addresses: []string{"192.168.100.111"}},
		{name: "Ethernet IPv6 without WiFi", host: "jetson.local", addresses: []string{"2a02:a469:f77e:100::1"}},
		{name: "wired route through USB control", host: "fe80::1%en0", addresses: []string{"10.0.0.10"}},
		{name: "NAT simulator", host: "127.0.0.1:50051"},
		{name: "IPv6 loopback", host: "[::1]:50051"},
		{name: "WiFi without interface metadata", host: "jetson.local", wifiConnected: true, wantWiFiCalls: 1},
		{name: "isolated USB", host: "fe80::1%en0", wantLocal: true, wantWiFiCalls: 1},
		{name: "link local only", host: "jetson.local", addresses: []string{"169.254.1.2", "fe80::1"}, wantLocal: true, wantWiFiCalls: 1},
		{name: "unusable addresses", host: "jetson.local", addresses: []string{"", "invalid", "127.0.0.1", "0.0.0.0", "224.0.0.1"}, wantLocal: true, wantWiFiCalls: 1},
		{name: "older agent WiFi unsupported", host: "jetson.local", wifiErr: status.Error(codes.Unimplemented, "unsupported"), wantWiFiCalls: 1},
		{name: "WiFi probe failure leaves connectivity unknown", host: "jetson.local", wifiErr: status.Error(codes.Unavailable, "connection lost"), wantWiFiCalls: 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			client := &osDownloadClient{wifiConnected: tt.wifiConnected, wifiErr: tt.wifiErr}
			conn := &grpcclient.AgentConnection{Host: tt.host, AgentService: client}
			if tt.cloud {
				conn.Reconnect = func(context.Context) (*grpcclient.AgentConnection, error) {
					t.Fatal("download selection must not reconnect")
					return nil, nil
				}
			}
			ctx := context.Background()
			if tt.cloudContext {
				ctx = context.WithValue(ctx, cloudDeviceContextKey{}, cloudDeviceConfig{})
			}
			version := &agentpb.GetAgentVersionResponse{
				NetworkInterfaces: []*agentpb.NetworkInterface{{Name: "eth0", IpAddresses: tt.addresses}},
			}
			if got := shouldServeOSArtifactLocally(ctx, conn, version); got != tt.wantLocal {
				t.Errorf("shouldServeOSArtifactLocally = %v, want %v", got, tt.wantLocal)
			}
			if client.wifiCalls != tt.wantWiFiCalls {
				t.Errorf("WiFi status calls = %d, want %d", client.wifiCalls, tt.wantWiFiCalls)
			}
		})
	}
}

func TestLocalOSArtifactRequiresDirectConnection(t *testing.T) {
	for _, tt := range []struct {
		name    string
		host    string
		cloud   bool
		wantErr string
	}{
		{name: "cloud display name", host: "ci-mesh-2", cloud: true, wantErr: "--artifact-url"},
		{name: "loopback", host: "127.0.0.1:50051", wantErr: "loopback"},
		{name: "LAN", host: "192.168.100.111"},
		{name: "USB", host: "fe80::1%en0"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			conn := &grpcclient.AgentConnection{Host: tt.host}
			if tt.cloud {
				conn.Reconnect = func(context.Context) (*grpcclient.AgentConnection, error) { return nil, nil }
			}
			err := validateLocalOSArtifactConnection(context.Background(), conn)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %v, want %q", err, tt.wantErr)
			}
		})
	}
}

func TestCloudOSUpdateDoesNotPromptForWiFi(t *testing.T) {
	conn := &grpcclient.AgentConnection{
		Host:      "ci-mesh-2",
		Reconnect: func(context.Context) (*grpcclient.AgentConnection, error) { return nil, nil },
	}
	// A default cloud selection has no cloud command context. No WiFi RPC or
	// interactive picker should run for this already connected cloud device.
	ensureDeviceWiFiForOSUpdate(context.Background(), conn)
}
