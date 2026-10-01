package mcp

import (
	"context"
	"fmt"
	"runtime"
	"sync"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
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
	cfg := s.currentConfig()
	if cfg == nil {
		return info
	}
	info.LastCheckedAt = cfg.LastCLIUpdateCheck
	if cfg.AvailableCLIUpdate == "" || version.CompareVersions(cfg.AvailableCLIUpdate, version.Version) <= 0 {
		return info
	}
	info.Available = true
	info.LatestVersion = cfg.AvailableCLIUpdate
	info.UpdateCommand = cliupdate.Command(runtime.GOOS)
	info.RestartRequired = true
	info.Message = fmt.Sprintf("A new Wendy CLI version is available: %s (running %s). Update with: %s. Then restart the Wendy MCP server to load the new version and tools.", info.LatestVersion, version.Version, info.UpdateCommand)
	if runtime.GOOS == "darwin" {
		info.Message += " If the tap is untrusted, run: brew trust wendylabsinc/tap."
	}
	return info
}

// Keep notice state separate from CLIUpdateNoticeShown: a terminal notice
// cannot tell us whether this MCP client has received the release notice.
func (s *mcpServer) cliUpdateMiddleware() server.ToolHandlerMiddleware {
	var mu sync.Mutex
	announced := make(map[string]map[string]bool)
	return func(next server.ToolHandlerFunc) server.ToolHandlerFunc {
		return func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
			result, err := next(ctx, req)
			if err != nil || result == nil || ctx.Err() != nil {
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
			if announced[sessionID][info.LatestVersion] {
				return result, err
			}
			if announced[sessionID] == nil {
				announced[sessionID] = make(map[string]bool)
			}
			announced[sessionID][info.LatestVersion] = true
			// Preserve the handler's structured payload and error state, and
			// avoid changing a result the handler may reuse on later calls.
			withNotice := *result
			withNotice.Content = append(append([]mcpgo.Content(nil), result.Content...), mcpgo.NewTextContent(info.Message))
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
