package mcp

import (
	"context"
	"fmt"
	"runtime"
	"sync"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/wendylabsinc/wendy/go/internal/cli/analytics"
	"github.com/wendylabsinc/wendy/go/internal/shared/cliupdate"
	"github.com/wendylabsinc/wendy/go/internal/shared/version"
)

// Available describes a known newer release in the cache. False does not
// establish that a release check succeeded or that the CLI is up to date.
type cliUpdateInfo struct {
	Available       bool   `json:"available"`
	LatestVersion   string `json:"latest_version,omitempty"`
	LastCheckedAt   string `json:"last_checked_at,omitempty"`
	UpdateCommand   string `json:"update_command,omitempty"`
	RestartRequired bool   `json:"restart_required,omitempty"`
	Message         string `json:"message,omitempty"`
}

func (s *mcpServer) cliUpdateStatus() cliUpdateInfo {
	info := cliUpdateInfo{}
	if cliupdate.BackgroundChecksDisabled() {
		return info
	}
	cfg := s.currentConfig()
	if cfg == nil {
		return info
	}
	info.LastCheckedAt = cfg.LastCLIUpdateCheck
	// A newer CLI process can be current while this long-lived server still
	// runs an older executable. Compare the last observed release with this
	// process, independently of another process's pending-update flag.
	latest := cfg.LatestCLIRelease
	if latest == "" {
		latest = cfg.AvailableCLIUpdate
	}
	if latest == "" || version.IsDev(latest) || version.CompareVersions(latest, version.Version) <= 0 {
		return info
	}
	info.Available = true
	info.LatestVersion = latest
	info.UpdateCommand = cliupdate.Command(runtime.GOOS)
	info.RestartRequired = true
	info.Message = fmt.Sprintf("A new Wendy CLI version is available: %s (running %s). Update with: %s. Then restart the Wendy MCP server to load the new version and tools.", info.LatestVersion, version.Version, info.UpdateCommand)
	return info
}

// cliUpdateNotice asks the assistant to raise the update. Only the daily tool
// result notice carries it; wendy_status returns the plain Message on every call.
func cliUpdateNotice(info cliUpdateInfo) string {
	return fmt.Sprintf("**WENDY CLI UPDATE AVAILABLE**: %s (running %s). Tell the user about this update now and offer to download it. Update with: %s. Then restart the Wendy MCP server to load the new version and tools. If they defer, remind them tomorrow.", info.LatestVersion, version.Version, info.UpdateCommand)
}

// wallClock drops the monotonic reading, which stops while the computer
// sleeps and would stretch a daily reminder across several calendar days.
func wallClock() time.Time { return time.Now().Round(0) }

// Keep notice state separate from CLIUpdateNoticeShown: a terminal notice
// cannot tell us whether this MCP client has received the release notice.
func (s *mcpServer) cliUpdateMiddleware() server.ToolHandlerMiddleware {
	return s.cliUpdateMiddlewareWithClock(wallClock)
}

func (s *mcpServer) cliUpdateMiddlewareWithClock(now func() time.Time) server.ToolHandlerMiddleware {
	type notice struct {
		version string
		at      time.Time
	}
	var mu sync.Mutex
	announced := make(map[string]notice)
	return func(next server.ToolHandlerFunc) server.ToolHandlerFunc {
		return func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
			result, err := next(ctx, req)
			// A hosted chat is not the place for CLI upgrade steps (store policy).
			if err != nil || result == nil || ctx.Err() != nil || s.relay {
				return result, err
			}
			info := s.cliUpdateStatus()
			if !info.Available {
				return result, err
			}
			sessionID := ""
			if session := server.ClientSessionFromContext(ctx); session != nil {
				sessionID = session.SessionID()
			}
			mu.Lock()
			defer mu.Unlock()
			previous, current := announced[sessionID], now()
			if previous.version == info.LatestVersion && !current.Before(previous.at) && current.Sub(previous.at) < 24*time.Hour {
				return result, err
			}
			announced[sessionID] = notice{version: info.LatestVersion, at: current}
			analytics.TrackStage(analytics.WithEntrypoint(ctx, "mcp"), "cli_update_offered", "wendy update", "completed", "", map[string]string{
				"stage": "cli_update", "install_method": "unknown",
			})
			// Preserve the handler's structured payload and error state, and
			// avoid changing a result the handler may reuse on later calls.
			withNotice := *result
			withNotice.Content = append(append([]mcpgo.Content(nil), result.Content...), mcpgo.NewTextContent(cliUpdateNotice(info)))
			return &withNotice, nil
		}
	}
}

// SetCLIUpdateChecker supplies a best-effort, cache-aware release check.
// Call before Start. Checks run off the handshake path and stop with serving.
func (s *mcpServer) SetCLIUpdateChecker(check func(context.Context)) {
	s.cliUpdateCheckFn = check
}

func (s *mcpServer) runCLIUpdateChecks(ctx context.Context, interval time.Duration) {
	if s.cliUpdateCheckFn == nil {
		return
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			return
		}
		s.cliUpdateCheckFn(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
