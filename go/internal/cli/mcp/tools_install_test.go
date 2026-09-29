package mcp

import (
	"context"
	"testing"

	"github.com/mark3labs/mcp-go/server"
	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/cli/onboarding"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

func TestInstallationToolsWorkBeforeDeviceConnection(t *testing.T) {
	s := New(&config.Config{}, nil)
	s.SetInstallationBackend(onboarding.Backend{
		Plan: func(_ context.Context, o onboarding.Options) (*onboarding.Plan, error) {
			if o.DeviceType != "unitree-g1" {
				t.Fatal("lost selected hardware")
			}
			return &onboarding.Plan{DeviceType: o.DeviceType, Method: "agent"}, nil
		},
		Drives: func() ([]onboarding.Drive, error) { return []onboarding.Drive{{ID: "/dev/test"}}, nil },
	})
	srv := server.NewMCPServer("test", "test")
	s.registerInstallationTools(srv)
	for _, name := range []string{"os_install_plan", "os_list_drives", "os_install_verify"} {
		if srv.GetTool(name) == nil {
			t.Fatalf("tool %s not registered", name)
		}
	}
	p, err := s.handleInstallPlan(context.Background(), callToolReq("os_install_plan", map[string]any{"device_type": "unitree-g1"}))
	if err != nil || p.IsError {
		t.Fatalf("plan without connection: %v %v", p, err)
	}
	d, err := s.handleInstallDrives(context.Background(), callToolReq("os_list_drives", nil))
	if err != nil || d.IsError {
		t.Fatalf("drives without connection: %v %v", d, err)
	}
	if s.GetConn() != nil {
		t.Fatal("planning changed session connection")
	}
}

func TestInstallVerifyDoesNotReplaceActiveSession(t *testing.T) {
	conn, address := startFakeAgentServer(t, &fakeAgentServer{versionResp: &agentpb.GetAgentVersionResponse{Version: "test"}})
	conn.ProvisioningService = agentpb.NewWendyProvisioningServiceClient(conn.Conn)
	s := New(&config.Config{}, func(_ context.Context, target string) (*grpcclient.AgentConnection, error) {
		if target != address {
			t.Fatalf("verified %s instead of %s", target, address)
		}
		return conn, nil
	})
	active := &grpcclient.AgentConnection{Host: "other.local", Addr: "other.local:50051"}
	s.SetConn(active)
	r, err := s.handleInstallVerify(context.Background(), callToolReq("os_install_verify", map[string]any{"address": address}))
	if err != nil || r.IsError {
		t.Fatalf("verification failed: %v %v", r, err)
	}
	if s.GetConn() != active {
		t.Fatal("first-boot check replaced active session")
	}
}
