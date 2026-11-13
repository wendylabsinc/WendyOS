package mcp

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/credentials/insecure"
)

type countingWiFiServer struct {
	fakeWiFiBluetoothServer
	connects int
}

func (s *countingWiFiServer) ConnectToWiFi(ctx context.Context, req *agentpb.ConnectToWiFiRequest) (*agentpb.ConnectToWiFiResponse, error) {
	s.connects++
	return &agentpb.ConnectToWiFiResponse{Success: true}, nil
}

func startCountingWiFiServer(t *testing.T, fake *countingWiFiServer) *grpcclient.AgentConnection {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	g := grpc.NewServer()
	agentpb.RegisterWendyAgentServiceServer(g, fake)
	go func() { _ = g.Serve(ln) }()
	t.Cleanup(g.Stop)
	conn, err := grpc.NewClient(ln.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return &grpcclient.AgentConnection{Conn: conn, AgentService: agentpb.NewWendyAgentServiceClient(conn)}
}

func resultErrorCode(result *mcpgo.CallToolResult) string {
	content, _ := result.StructuredContent.(map[string]any)
	code, _ := content["error_code"].(string)
	return code
}

// The Wi-Fi tools' device argument opens a connection for one call, reaching
// a first-boot device device_connect refuses. A password over an unverified
// setup connection needs the user's agreement first.
func TestWiFiToolsDeviceArgument(t *testing.T) {
	for _, tc := range []struct {
		name        string
		unverified  bool
		args        map[string]any
		wantCode    string
		wantConnect int
	}{
		{name: "unverified password needs confirmation", unverified: true, args: map[string]any{"password": "secret"}, wantCode: string(errCodeConfirmationRequired)},
		{name: "unverified password confirmed", unverified: true, args: map[string]any{"password": "secret", "allow_unverified_device": true}, wantConnect: 1},
		{name: "unverified open network", unverified: true, args: map[string]any{}, wantConnect: 1},
		{name: "verified password", args: map[string]any{"password": "secret"}, wantConnect: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &countingWiFiServer{}
			conn := startCountingWiFiServer(t, fake)
			srv := New(&config.Config{}, nil)
			gotDevice := ""
			srv.SetWiFiSetupConnect(func(_ context.Context, device string) (*grpcclient.AgentConnection, bool, error) {
				gotDevice = device
				return conn, tc.unverified, nil
			})
			args := map[string]any{"ssid": "Home", "device": "wendyos-robot.local"}
			for k, v := range tc.args {
				args[k] = v
			}
			result, err := srv.callTool(context.Background(), "wifi_connect", args)
			if err != nil {
				t.Fatal(err)
			}
			if gotDevice != "wendyos-robot.local" {
				t.Fatalf("connected to %q", gotDevice)
			}
			if code := resultErrorCode(result); code != tc.wantCode {
				t.Fatalf("error_code = %q, want %q: %v", code, tc.wantCode, result.Content)
			}
			if fake.connects != tc.wantConnect {
				t.Fatalf("ConnectToWiFi calls = %d, want %d", fake.connects, tc.wantConnect)
			}
			if srv.GetConn() != nil {
				t.Fatal("per-call Wi-Fi connection became the session connection")
			}
			if state := conn.Conn.GetState(); state != connectivity.Shutdown {
				t.Fatalf("per-call Wi-Fi connection left open: %v", state)
			}
		})
	}
}

func TestWiFiToolsDeviceArgumentReadTools(t *testing.T) {
	for _, tool := range []string{"wifi_list", "wifi_status", "wifi_known_networks", "wifi_disconnect"} {
		t.Run(tool, func(t *testing.T) {
			fake := &countingWiFiServer{fakeWiFiBluetoothServer: fakeWiFiBluetoothServer{wifiStatusResp: &agentpb.GetWiFiStatusResponse{}}}
			conn := startCountingWiFiServer(t, fake)
			srv := New(&config.Config{}, nil)
			srv.SetWiFiSetupConnect(func(context.Context, string) (*grpcclient.AgentConnection, bool, error) { return conn, true, nil })
			result, err := srv.callTool(context.Background(), tool, map[string]any{"device": "wendyos-robot.local"})
			if err != nil {
				t.Fatal(err)
			}
			// wifi_disconnect's fake may report an RPC error; only the routing matters here.
			if code := resultErrorCode(result); code == string(errCodeNotConnected) || code == string(errCodeConfirmationRequired) {
				t.Fatalf("%s did not use the per-call connection: %v", tool, result.Content)
			}
			if srv.GetConn() != nil || conn.Conn.GetState() != connectivity.Shutdown {
				t.Fatal("per-call Wi-Fi connection leaked")
			}
		})
	}
}

func TestWiFiToolsDeviceArgumentRefusals(t *testing.T) {
	t.Run("relay client", func(t *testing.T) {
		srv := New(&config.Config{}, nil)
		called := false
		srv.SetWiFiSetupConnect(func(context.Context, string) (*grpcclient.AgentConnection, bool, error) {
			called = true
			return nil, false, errors.New("unreachable")
		})
		srv.setBindingPolicy(&BindingPolicy{AllowAll: true})
		result, err := srv.callTool(context.Background(), "wifi_list", map[string]any{"device": "192.0.2.10"})
		if err != nil {
			t.Fatal(err)
		}
		if resultErrorCode(result) != string(errCodeDenied) || called {
			t.Fatalf("relay client reached a host through the device argument: %v", result.Content)
		}
	})
	t.Run("not configured", func(t *testing.T) {
		result, err := New(&config.Config{}, nil).callTool(context.Background(), "wifi_status", map[string]any{"device": "192.0.2.10"})
		if err != nil {
			t.Fatal(err)
		}
		if resultErrorCode(result) != string(errCodeUnsupported) {
			t.Fatalf("missing setup connector: %v", result.Content)
		}
	})
	t.Run("connect failure", func(t *testing.T) {
		srv := New(&config.Config{}, nil)
		srv.SetWiFiSetupConnect(func(context.Context, string) (*grpcclient.AgentConnection, bool, error) {
			return nil, false, errors.New("connection refused")
		})
		result, err := srv.callTool(context.Background(), "wifi_status", map[string]any{"device": "192.0.2.10"})
		if err != nil {
			t.Fatal(err)
		}
		if resultErrorCode(result) != string(errCodeDeviceUnreachable) {
			t.Fatalf("connect failure: %v", result.Content)
		}
	})
}

// device_connect points a first-boot device at the Wi-Fi tools' device argument.
func TestDeviceConnectExpectedIdentityPointsAtWiFiSetup(t *testing.T) {
	srv := New(&config.Config{}, func(context.Context, string) (*grpcclient.AgentConnection, error) {
		return nil, fmt.Errorf("dial: %w", certs.ErrExpectedDeviceIdentity)
	})
	result, err := srv.callTool(context.Background(), "device_connect", map[string]any{"device": "wendyos-robot.local"})
	if err != nil {
		t.Fatal(err)
	}
	text := result.Content[0].(mcpgo.TextContent).Text
	if !result.IsError || !strings.Contains(text, "pass this selector as device to the Wi-Fi tools") {
		t.Fatalf("device_connect did not point at Wi-Fi setup: %s", text)
	}
}
