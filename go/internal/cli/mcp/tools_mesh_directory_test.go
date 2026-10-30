package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/wendylabsinc/wendy/go/internal/cli/sessionbroker"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
)

func TestMCPNearbyListsRoutedLiteAndWendyOSWithoutCloudInventory(t *testing.T) {
	t.Setenv("WENDY_AGENT_SOCKET", "")
	srv := New(&config.Config{}, nil)
	const tenant = "11111111-1111-4111-8111-111111111111"
	lite := "mesh://" + tenant + "/33333333-3333-4333-8333-333333333333"
	os := "mesh://" + tenant + "/44444444-4444-4444-8444-444444444444"
	calls := 0
	srv.meshDirectoryFn = func(context.Context) (sessionbroker.OperatorDirectory, error) {
		calls++
		return sessionbroker.OperatorDirectory{Version: 1, State: "ready", Captured: time.Now().UnixMilli(), Source: "authenticated-router", Truncated: true, Devices: []sessionbroker.OperatorTargetInfo{{Expires: time.Now().Add(time.Minute).UnixMilli(), Selector: lite, Platform: "Lite", Name: "C6", Caps: []string{"sensors", "future-optional"}}, {Expires: time.Now().Add(time.Minute).UnixMilli(), Selector: os, Platform: "WendyOS", Name: "Pi5"}}}, nil
	}
	result, err := srv.callTool(context.Background(), "device_list", nil)
	if err != nil || result.IsError {
		t.Fatal(result, err)
	}
	text := result.Content[0].(mcpgo.TextContent).Text
	var value map[string]any
	if err = json.Unmarshal([]byte(text), &value); err != nil {
		t.Fatal(text, err)
	}
	for _, want := range []string{lite, os, "future-optional", "authenticated-router-observation", "truncated"} {
		if !strings.Contains(text, want) {
			t.Fatal("MCP feed missing", want, text)
		}
	}
	if calls != 1 {
		t.Fatal("unexpected source calls", calls)
	}
}
