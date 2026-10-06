package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/internal/shared/version"
)

func releasedCLI(t *testing.T) {
	t.Helper()
	old := version.Version
	version.Version = "2026.09.29-120000"
	t.Cleanup(func() { version.Version = old })
}

func TestWendyStatusCLIUpdateReloadsCache(t *testing.T) {
	releasedCLI(t)
	useConfigDir(t)
	enableReload(t, config.Load)
	s := New(&config.Config{}, nil)
	for _, connected := range []bool{false, true} {
		if connected {
			conn, _ := startFakeAgentServer(t, &fakeAgentServer{})
			s.SetConn(conn)
		}
		cfg := &config.Config{AvailableCLIUpdate: "2026.09.30-120000", LastCLIUpdateCheck: "2026-09-30T13:00:00Z", CLIUpdateNoticeShown: "2026.09.30-120000"}
		if err := config.Save(cfg); err != nil {
			t.Fatal(err)
		}
		result, err := s.handleWendyStatus(context.Background(), callToolReq("wendy_status", nil))
		if err != nil {
			t.Fatal(err)
		}
		var out struct {
			Connected bool          `json:"connected"`
			Update    cliUpdateInfo `json:"cli_update"`
		}
		if err := json.Unmarshal([]byte(toolResultText(t, result)), &out); err != nil {
			t.Fatal(err)
		}
		if out.Connected != connected || !out.Update.Available || out.Update.LatestVersion != cfg.AvailableCLIUpdate || out.Update.LastCheckedAt != cfg.LastCLIUpdateCheck || !out.Update.RestartRequired {
			t.Fatalf("status: %+v", out)
		}
		wantCommand := "curl -fsSL https://install.wendy.dev/cli.sh | bash"
		if runtime.GOOS == "darwin" {
			wantCommand = "brew update && brew install wendy"
		} else if runtime.GOOS == "windows" {
			wantCommand = "winget upgrade WendyLabs.Wendy"
		}
		if out.Update.UpdateCommand != wantCommand || !strings.Contains(out.Update.Message, "restart the Wendy MCP server") {
			t.Fatalf("update guidance: %+v", out.Update)
		}
		// A later successful check clears a stale release without restarting.
		if err := config.Save(&config.Config{}); err != nil {
			t.Fatal(err)
		}
		if s.cliUpdateStatus().Available {
			t.Fatal("still using cached startup/update config")
		}
	}
}

func TestCLIUpdateSuppressesCurrentAndDevBuilds(t *testing.T) {
	releasedCLI(t)
	for _, current := range []string{"2026.09.29-120000", "dev", "2026.09.29-120000-dev"} {
		version.Version = current
		for _, latest := range []string{"", "2026.09.28-120000", "2026.09.29-120000"} {
			info := New(&config.Config{AvailableCLIUpdate: latest}, nil).cliUpdateStatus()
			if info.Available || info.UpdateCommand != "" || info.RestartRequired || info.Message != "" {
				t.Fatalf("current=%s latest=%s: %+v", current, latest, info)
			}
		}
	}
}

func protocolUpdateCall(t *testing.T, srv *server.MCPServer, ctx context.Context, tool string) *mcpgo.CallToolResult {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": tool, "arguments": map[string]any{}}})
	if err != nil {
		t.Fatal(err)
	}
	reply, ok := srv.HandleMessage(ctx, raw).(mcpgo.JSONRPCResponse)
	if !ok {
		t.Fatal("tool call failed")
	}
	result, ok := reply.Result.(*mcpgo.CallToolResult)
	if !ok {
		t.Fatalf("unexpected result: %+v", reply.Result)
	}
	return result
}

func TestCLIUpdateNoticePerSessionAndRelease(t *testing.T) {
	releasedCLI(t)
	useConfigDir(t)
	enableReload(t, config.Load)
	s := New(&config.Config{}, nil)
	srv, err := s.newProtocolServer()
	if err != nil {
		t.Fatal(err)
	}
	a := srv.WithContext(context.Background(), server.NewInProcessSession("a", nil))
	b := srv.WithContext(context.Background(), server.NewInProcessSession("b", nil))
	if got := protocolUpdateCall(t, srv, a, "container_list"); len(got.Content) != 1 {
		t.Fatal("notice before an update was discovered")
	}
	for _, release := range []string{"2026.09.30-120000", "2026.10.01-120000"} {
		if err := config.Save(&config.Config{AvailableCLIUpdate: release, CLIUpdateNoticeShown: release}); err != nil {
			t.Fatal(err)
		}
		for _, ctx := range []context.Context{a, b} {
			first := protocolUpdateCall(t, srv, ctx, "container_list")
			if !first.IsError || len(first.Content) != 2 || structuredMap(t, first)["error_code"] != string(errCodeNotConnected) {
				t.Fatalf("notice lost tool error: %+v", first)
			}
			if !strings.Contains(first.Content[1].(mcpgo.TextContent).Text, release) {
				t.Fatalf("notice missing release: %+v", first.Content)
			}
			if second := protocolUpdateCall(t, srv, ctx, "container_list"); len(second.Content) != 1 {
				t.Fatal("duplicate release notice")
			}
			// Explicit status retains update information after the notice.
			status := protocolUpdateCall(t, srv, ctx, "wendy_status")
			if len(status.Content) != 1 || !structuredMap(t, status)["cli_update"].(cliUpdateInfo).Available {
				t.Fatalf("status lost cached update details: %+v", status)
			}
		}
	}
}

func TestCLIUpdateNoticeConcurrentCallsPreserveResult(t *testing.T) {
	releasedCLI(t)
	s := New(&config.Config{AvailableCLIUpdate: "2026.09.30-120000"}, nil)
	srv, err := s.newProtocolServer()
	if err != nil {
		t.Fatal(err)
	}
	// Tools registered later, including container tools, also use middleware.
	original := okResult(map[string]any{"value": "unchanged"})
	srv.AddTool(mcpgo.NewTool("app_tool"), func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) { return original, nil })
	var notices atomic.Int32
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			result := protocolUpdateCall(t, srv, context.Background(), "app_tool")
			if len(result.Content) == 2 {
				notices.Add(1)
			}
			if toolResultText(t, result) != toolResultText(t, original) || structuredMap(t, result)["value"] != "unchanged" {
				t.Error("changed original payload")
			}
		})
	}
	wg.Wait()
	if notices.Load() != 1 || len(original.Content) != 1 {
		t.Fatalf("notices=%d, original content=%d", notices.Load(), len(original.Content))
	}
}

func TestCLIUpdateNoticeDoesNotConsumeFailedCalls(t *testing.T) {
	releasedCLI(t)
	s := New(&config.Config{AvailableCLIUpdate: "2026.09.30-120000"}, nil)
	mw := s.cliUpdateMiddleware()
	failure := errors.New("handler failure")
	_, err := mw(func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) { return nil, failure })(context.Background(), mcpgo.CallToolRequest{})
	if err != failure {
		t.Fatal("changed handler error")
	}
	next := mw(func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) { return okText("ok"), nil })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result, _ := next(ctx, mcpgo.CallToolRequest{})
	if len(result.Content) != 1 {
		t.Fatal("notice consumed by canceled call")
	}
	result, _ = next(context.Background(), mcpgo.CallToolRequest{})
	if len(result.Content) != 2 {
		t.Fatal("failed calls consumed the notice")
	}
}

func TestCLIUpdateChecksRepeatAndStop(t *testing.T) {
	s := New(&config.Config{}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var calls atomic.Int32
	s.SetCLIUpdateChecker(func(got context.Context) {
		if got != ctx {
			t.Error("checker lost serving context")
		}
		if calls.Add(1) == 2 {
			cancel()
		}
	})
	done := make(chan struct{})
	go func() {
		s.runCLIUpdateChecks(ctx, time.Millisecond)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("checker did not stop")
	}
	if calls.Load() != 2 {
		t.Fatalf("checks=%d", calls.Load())
	}
}
