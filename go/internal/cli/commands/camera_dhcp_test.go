package commands

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type cameraDHCPClient struct {
	agentpb.WendyVideoServiceClient
	request *agentpb.SetCameraDHCPRequest
	result  *agentpb.CameraDHCPStatus
	err     error
	listed  bool
}

func (c *cameraDHCPClient) GetCameraDHCPStatus(context.Context, *agentpb.GetCameraDHCPStatusRequest, ...grpc.CallOption) (*agentpb.CameraDHCPStatus, error) {
	c.listed = true
	return c.result, c.err
}

func (c *cameraDHCPClient) SetCameraDHCP(_ context.Context, req *agentpb.SetCameraDHCPRequest, _ ...grpc.CallOption) (*agentpb.CameraDHCPStatus, error) {
	c.request = req
	return c.result, c.err
}

func TestCameraDHCPCommands(t *testing.T) {
	for _, tc := range []struct {
		name   string
		args   []string
		json   bool
		rpcErr error
		want   string
	}{
		{"bare status", nil, false, nil, "waiting"},
		{"status", []string{"status"}, false, nil, "eth1"},
		{"enable", []string{"enable", "eth1"}, false, nil, "enabled on eth1"},
		{"disable", []string{"disable", "eth1"}, false, nil, "disabled on eth1"},
		{"json", []string{"status"}, true, nil, "interfaces"},
		{"old agent", []string{"status"}, false, status.Error(codes.Unimplemented, "unknown method"), "update the agent"},
		{"write failed", []string{"enable", "eth1"}, false, status.Error(codes.FailedPrecondition, "disk full"), "disk full"},
		{"missing interface", []string{"enable"}, false, nil, "accepts 1 arg"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := &cameraDHCPClient{result: &agentpb.CameraDHCPStatus{Interfaces: []*agentpb.CameraDHCPInterfaceStatus{
				{InterfaceName: "eth1", Enabled: true, Present: true, Carrier: true, Up: true, State: "waiting", Detail: "Waiting for unanswered camera DHCP requests"},
			}}, err: tc.rpcErr}
			previousResolve, previousJSON := resolveCameraTargetFn, jsonOutput
			t.Cleanup(func() { resolveCameraTargetFn, jsonOutput = previousResolve, previousJSON })
			jsonOutput = tc.json
			resolveCameraTargetFn = func(context.Context, ...resolveOption) (*SelectedDevice, error) {
				return &SelectedDevice{Agent: &grpcclient.AgentConnection{VideoService: client}}, nil
			}
			var out bytes.Buffer
			cmd := newCameraDHCPCmd()
			cmd.SetOut(&out)
			cmd.SetErr(&out)
			cmd.SetArgs(tc.args)
			err := cmd.ExecuteContext(context.Background())
			if tc.rpcErr != nil || tc.name == "missing interface" {
				if err == nil || !strings.Contains(err.Error(), tc.want) {
					t.Fatalf("error = %v, want %q", err, tc.want)
				}
				if strings.Contains(out.String(), "Camera DHCP enabled on") {
					t.Fatal("failed update printed success")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(out.String(), tc.want) {
				t.Fatalf("output %q lacks %q", out.String(), tc.want)
			}
			if tc.name == "enable" || tc.name == "disable" {
				if client.request == nil || client.request.GetInterfaceName() != "eth1" || client.request.GetEnabled() != (tc.name == "enable") {
					t.Fatalf("request: %v", client.request)
				}
			} else if !client.listed {
				t.Fatal("did not request live status")
			}
			if tc.json && !json.Valid(out.Bytes()) {
				t.Fatalf("invalid JSON: %s", out.String())
			}
		})
	}
}

func TestCameraDHCPStatusSurfacesInspectionErrors(t *testing.T) {
	previous := jsonOutput
	t.Cleanup(func() { jsonOutput = previous })
	jsonOutput = false
	cmd := newCameraDHCPCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	if err := renderCameraDHCPStatus(cmd, &agentpb.CameraDHCPStatus{
		ConfigurationError: "invalid configuration", InterfaceError: "netlink unavailable",
	}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "invalid configuration") || !strings.Contains(out.String(), "netlink unavailable") || strings.Contains(out.String(), "No wired") {
		t.Fatalf("inspection failure masked: %s", out.String())
	}
}
