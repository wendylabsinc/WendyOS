package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/wendylabsinc/wendy/go/internal/cli/analytics"
	"github.com/wendylabsinc/wendy/go/internal/shared/cliupdate"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/internal/shared/env"
	"github.com/wendylabsinc/wendy/go/internal/shared/version"
)

func releasedCLI(t *testing.T) {
	t.Helper()
	for _, key := range env.CIEnvVars {
		t.Setenv(key, "")
	}
	t.Setenv("WENDY_NO_UPDATE_CHECK", "")
	old := version.Version
	version.Version = "2026.09.29-120000"
	t.Cleanup(func() { version.Version = old })
}

func TestCLIUpdateOptOutSuppressesCachedMCPNotice(t *testing.T) {
	for _, test := range []struct{ key, value string }{
		{"WENDY_NO_UPDATE_CHECK", "1"},
		{"WENDY_NO_UPDATE_CHECK", "true"},
		{"CI", "true"},
		{"GITHUB_ACTIONS", "true"},
	} {
		t.Run(test.key+"="+test.value, func(t *testing.T) {
			releasedCLI(t)
			t.Setenv(test.key, test.value)
			analytics.Disable()
			offers := 0
			analytics.SetTrackHookForTesting(func(event string, _ map[string]string) {
				if event == "cli_update_offered" {
					offers++
				}
			})
			t.Cleanup(func() { analytics.SetTrackHookForTesting(nil) })
			s := New(&config.Config{LatestCLIRelease: "2026.10.05-134249"}, nil)
			if info := s.cliUpdateStatus(); info.Available || info.Message != "" || info.UpdateCommand != "" {
				t.Fatalf("disabled update guidance remained visible: %+v", info)
			}
			next := s.cliUpdateMiddleware()(func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
				return okText("unchanged"), nil
			})
			result, err := next(context.Background(), mcpgo.CallToolRequest{})
			if err != nil || len(result.Content) != 1 || offers != 0 {
				t.Fatalf("disabled update reminder was emitted: result=%+v err=%v offers=%d", result, err, offers)
			}
		})
	}
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
		// The status field repeats on every call, so it informs without asking
		// the assistant to raise the update again.
		if out.Update.UpdateCommand != "wendy update" || !strings.Contains(out.Update.Message, "restart the Wendy MCP server") || strings.Contains(out.Update.Message, "brew trust") || strings.Contains(out.Update.Message, "Tell the user") {
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

func TestCLIUpdateStatusRetainsReleaseAfterNewerCLIUpdated(t *testing.T) {
	releasedCLI(t)
	useConfigDir(t)
	enableReload(t, config.Load)
	s := New(&config.Config{}, nil)
	// A newly installed CLI observed the latest release and no longer needs
	// an update itself. This server still runs its old in-memory version.
	const release = "2026.10.05-134249"
	if err := config.Save(&config.Config{LatestCLIRelease: release, LastCLIUpdateCheck: "2026-10-06T12:00:00Z"}); err != nil {
		t.Fatal(err)
	}
	info := s.cliUpdateStatus()
	if !info.Available || info.LatestVersion != release || !info.RestartRequired || info.UpdateCommand != "wendy update" {
		t.Fatalf("old MCP server lost its upgrade/restart guidance: %+v", info)
	}
	// Once the server restarts on the new executable, that same cache must
	// stop producing an update notice.
	version.Version = release
	if info := s.cliUpdateStatus(); info.Available || info.RestartRequired {
		t.Fatalf("current MCP server received a stale update: %+v", info)
	}
}

func TestCLIUpdateStatusPrefersLatestReleaseOverLegacyPendingValue(t *testing.T) {
	releasedCLI(t)
	s := New(&config.Config{LatestCLIRelease: version.Version, AvailableCLIUpdate: "2026.10.05-134249"}, nil)
	if info := s.cliUpdateStatus(); info.Available || info.RestartRequired {
		t.Fatalf("stale legacy pending version overrode the latest release cache: %+v", info)
	}
}

// A CLI the Wendy plugin's launcher installed gets the plugin advice and the
// CLI installer as its command in the notice the assistant sees, matching the
// terminal notice. wendy update recognizes no updater for that copy, so any
// other install keeps "Update with: wendy update".
func TestCLIUpdatePluginManagedAdvice(t *testing.T) {
	releasedCLI(t)
	for _, managed := range []bool{true, false} {
		old := pluginManagedCLI
		pluginManagedCLI = func() bool { return managed }
		info := New(&config.Config{AvailableCLIUpdate: "2026.09.30-120000"}, nil).cliUpdateStatus()
		pluginManagedCLI = old

		command, advice := "wendy update", "Update with: wendy update"
		if managed {
			command, advice = cliupdate.InstallCommand(runtime.GOOS), cliupdate.PluginAdvice(runtime.GOOS)
		}
		if !info.Available || info.UpdateCommand != command ||
			!strings.Contains(info.Message, advice) || !strings.Contains(info.Message, "restart the Wendy MCP server") {
			t.Errorf("managed=%v: update guidance %+v, want command %q and message with %q", managed, info, command, advice)
		}
	}
}

func TestCLIUpdateSuppressesCurrentAndDevBuilds(t *testing.T) {
	releasedCLI(t)
	for _, current := range []string{"2026.09.29-120000", "dev", "2026.09.29-120000-dev"} {
		version.Version = current
		for _, latest := range []string{"", "2026.09.28-120000", "2026.09.29-120000", "dev", "2026.10.05-134249-dev"} {
			for _, cfg := range []*config.Config{{AvailableCLIUpdate: latest}, {LatestCLIRelease: latest}} {
				info := New(cfg, nil).cliUpdateStatus()
				if info.Available || info.UpdateCommand != "" || info.RestartRequired || info.Message != "" {
					t.Fatalf("current=%s latest=%s: %+v", current, latest, info)
				}
			}
		}
		if version.IsDev(current) {
			if info := New(&config.Config{LatestCLIRelease: "2026.10.05-134249"}, nil).cliUpdateStatus(); info.Available {
				t.Fatalf("development server offered a released build: %+v", info)
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
	analytics.Disable()
	var offers []map[string]string
	var commands []map[string]string
	analytics.SetTrackHookForTesting(func(event string, props map[string]string) {
		switch event {
		case "cli_update_offered":
			offers = append(offers, props)
		case "command_executed":
			commands = append(commands, props)
		}
	})
	t.Cleanup(func() { analytics.SetTrackHookForTesting(nil) })
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
		if err := config.Save(&config.Config{LatestCLIRelease: release, CLIUpdateNoticeShown: release}); err != nil {
			t.Fatal(err)
		}
		for _, ctx := range []context.Context{a, b} {
			offerCount, commandCount := len(offers), len(commands)
			first := protocolUpdateCall(t, srv, ctx, "container_list")
			if len(offers) != offerCount+1 || len(commands) != commandCount+1 {
				t.Fatalf("notice call emitted %d offers and %d command events", len(offers)-offerCount, len(commands)-commandCount)
			}
			if offers[offerCount]["attempt_id"] != commands[commandCount]["attempt_id"] {
				t.Fatalf("notice attempt %q does not match tool attempt %q", offers[offerCount]["attempt_id"], commands[commandCount]["attempt_id"])
			}
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
	if len(offers) != 4 {
		t.Fatalf("offer telemetry count = %d, want one per session and release", len(offers))
	}
	seenAttempts := make(map[string]bool)
	for _, props := range commands {
		id := props["attempt_id"]
		if seenAttempts[id] {
			t.Fatalf("separate tool calls reused attempt %q", id)
		}
		seenAttempts[id] = true
	}
	for _, props := range offers {
		if _, err := uuid.Parse(props["attempt_id"]); err != nil {
			t.Fatalf("invalid attempt ID: %v", err)
		}
		want := map[string]string{
			"attempt_id": props["attempt_id"], "entrypoint": "mcp", "stage": "cli_update", "install_method": "unknown",
			"command_name": "wendy update", "command_root": "update", "outcome": "completed", "success": "true",
		}
		if !reflect.DeepEqual(props, want) {
			t.Fatalf("offer leaked extra properties or lost bounded context: %#v", props)
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

// A monotonic reading would make Sub ignore time the computer spent asleep.
func TestCLIUpdateNoticeClockIsWallTime(t *testing.T) {
	if now := wallClock(); now.String() != now.Round(0).String() {
		t.Fatalf("reminder clock keeps a monotonic reading: %s", now)
	}
}

func TestCLIUpdateNoticeRepeatsDailyInLongLivedSession(t *testing.T) {
	releasedCLI(t)
	s := New(&config.Config{LatestCLIRelease: "2026.10.05-134249"}, nil)
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	start := now
	original := okResult(map[string]any{"value": "unchanged"})
	next := s.cliUpdateMiddlewareWithClock(func() time.Time { return now })(func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		return original, nil
	})
	for _, tc := range []struct {
		after      time.Duration
		wantNotice bool
	}{
		{0, true},
		{23*time.Hour + 59*time.Minute, false},
		{24 * time.Hour, true},
		{24*time.Hour + time.Minute, false},
		{48 * time.Hour, true},
	} {
		now = start.Add(tc.after)
		result, err := next(context.Background(), mcpgo.CallToolRequest{})
		if err != nil || (len(result.Content) == 2) != tc.wantNotice {
			t.Fatalf("after %s: content=%+v err=%v", tc.after, result.Content, err)
		}
		if toolResultText(t, result) != toolResultText(t, original) || len(original.Content) != 1 {
			t.Fatal("reminder changed the original result")
		}
		if tc.wantNotice {
			message := result.Content[1].(mcpgo.TextContent).Text
			for _, want := range []string{"**WENDY CLI UPDATE AVAILABLE**", "Tell the user", "wendy update", "restart the Wendy MCP server", "tomorrow"} {
				if !strings.Contains(message, want) {
					t.Fatalf("notice missing %q: %s", want, message)
				}
			}
		}
	}
}
