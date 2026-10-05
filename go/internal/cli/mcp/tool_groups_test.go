package mcp

import (
	"context"
	"encoding/json"
	"slices"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
)

func protocolTools(t *testing.T, srv *server.MCPServer) []mcpgo.Tool {
	t.Helper()
	response := srv.HandleMessage(context.Background(), []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	encoded, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	var reply struct {
		Result mcpgo.ListToolsResult
		Error  any
	}
	if err := json.Unmarshal(encoded, &reply); err != nil {
		t.Fatal(err)
	}
	if reply.Error != nil {
		t.Fatalf("list tools: %s", encoded)
	}
	return reply.Result.Tools
}

func TestToolGroupsProtocolDiscovery(t *testing.T) {
	s := New(&config.Config{}, nil)
	srv, err := s.newProtocolServer()
	if err != nil {
		t.Fatal(err)
	}
	initial := protocolTools(t, srv)
	if !*srv.GetTool("wendy_tools").Tool.Annotations.ReadOnlyHint {
		t.Fatal("catalog selection should not require device-mutation approval")
	}
	if len(initial) != 16 {
		t.Fatalf("default count = %d", len(initial))
	}
	encoded, _ := json.Marshal(initial)
	if len(encoded) > 14000 {
		t.Fatalf("default schema grew to %d bytes", len(encoded))
	}
	for _, tool := range initial {
		if !slices.Contains(toolGroups["core"], tool.Name) {
			t.Errorf("unexpected default tool %s", tool.Name)
		}
	}
	// Every new built-in must have an intentional group. Unknown app tools
	// stay visible, so forgetting to classify a built-in would expose it.
	for name := range srv.ListTools() {
		if name == "container_attach" {
			continue
		}
		found := false
		for _, names := range toolGroups {
			found = found || slices.Contains(names, name)
		}
		if !found {
			t.Errorf("unclassified built-in %s", name)
		}
	}
	srv.AddTool(mcpgo.NewTool("app__observe"), nil)
	selectGroups := srv.GetTool("wendy_tools").Handler
	r, err := selectGroups(context.Background(), callToolReq("wendy_tools", map[string]any{"groups": []string{"robotics"}}))
	if err != nil || r.IsError {
		t.Fatalf("select: %v %v", r, err)
	}
	names := []string{}
	for _, tool := range protocolTools(t, srv) {
		names = append(names, tool.Name)
	}
	if !slices.Contains(names, "ros2_lidar_summary") || !slices.Contains(names, "app__observe") || slices.Contains(names, "os_update") {
		t.Fatalf("wrong robotics selection: %v", names)
	}
	for _, invalid := range []any{nil, "all", []any{1}, []string{"typo"}} {
		r, _ := selectGroups(context.Background(), callToolReq("wendy_tools", map[string]any{"groups": invalid}))
		if !r.IsError {
			t.Fatalf("accepted %v", invalid)
		}
		if !slices.Equal(s.selectedToolGroups(), []string{"core", "robotics"}) {
			t.Fatal("invalid selection changed groups")
		}
	}
	if err := s.SetToolGroups([]string{"all"}); err != nil {
		t.Fatal(err)
	}
	for _, tool := range protocolTools(t, srv) {
		if tool.Name == "container_attach" {
			t.Fatal("legacy restart advertised as passive attachment")
		}
	}
	if srv.GetTool("container_attach") == nil {
		t.Fatal("legacy handler removed")
	}
	if !*srv.GetTool("container_start").Tool.Annotations.DestructiveHint {
		t.Fatal("restart must disclose interruption")
	}
}

func TestDeviceConnectLegacyArgumentsRemainCallable(t *testing.T) {
	s := New(&config.Config{}, func(_ context.Context, device string) (*grpcclient.AgentConnection, error) {
		if device != "robot.local:50051" {
			t.Fatalf("unexpected target %s", device)
		}
		return &grpcclient.AgentConnection{Addr: device}, nil
	})
	srv, err := s.newProtocolServer()
	if err != nil {
		t.Fatal(err)
	}
	response := srv.HandleMessage(context.Background(), []byte(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"device_connect","arguments":{"address":"robot.local:50051"}}}`))
	encoded, _ := json.Marshal(response)
	var reply struct {
		Result mcpgo.CallToolResult
		Error  any
	}
	if err := json.Unmarshal(encoded, &reply); err != nil {
		t.Fatal(err)
	}
	if reply.Error != nil || reply.Result.IsError || s.GetConn() == nil {
		t.Fatalf("legacy address rejected: %s", encoded)
	}
}
