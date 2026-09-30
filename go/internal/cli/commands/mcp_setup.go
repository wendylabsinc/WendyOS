package commands

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/spf13/cobra"
	"github.com/wendylabsinc/wendy/go/internal/cli/pluginmode"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/internal/shared/version"
)

func newMCPSetupCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "setup",
		Short: "Configure the Wendy MCP server in supported AI tools",
		Long:  "Detects installed AI tools and adds the wendy MCP server to their configuration.",
		RunE: func(cmd *cobra.Command, args []string) error {
			mcpResults := setupMCPForAllTools()
			results := append(append([]mcpSetupResult(nil), mcpResults...), installSkillsForAllTools()...)
			for _, r := range results {
				if r.err != nil {
					fmt.Fprintf(cmd.OutOrStdout(), "✗ %s: %v\n", r.tool, r.err)
				} else {
					fmt.Fprintf(cmd.OutOrStdout(), "✓ %s: configured at %s\n", r.tool, r.path)
				}
			}
			if len(results) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "No supported AI tools detected.")
				fmt.Fprintln(cmd.OutOrStdout(), "Install Claude Code: npm install -g @anthropic-ai/claude-code")
			}
			fmt.Fprint(cmd.OutOrStdout(), mcpRestartNotice(mcpResults))
			if os.Getenv("SUDO_UID") != "" {
				// The config writers keep file owners, but the skill
				// installers and the wendy settings file do not.
				fmt.Fprintln(cmd.OutOrStdout(), "\n⚠ This ran under sudo: skill files and wendy settings it created may now be owned by root. Run `wendy mcp setup` without sudo.")
			}
			// Record the CLI version so the root command can auto-refresh the
			// configuration and skills after a later upgrade.
			recordMCPSetupVersion()
			return nil
		},
	}
}

// recordMCPSetupVersion stores the running CLI version as the last version that
// performed MCP setup. Failures are non-fatal: at worst auto-refresh re-runs the
// (idempotent) setup again on the next invocation.
func recordMCPSetupVersion() {
	cfg, err := config.Load()
	if err != nil {
		return
	}
	cfg.LastMCPSetupVersion = version.Version
	_ = config.Save(cfg)
}

// shouldRefreshMCPSetup reports whether the root command should silently re-run
// MCP setup. It refreshes only when the user has previously run `wendy mcp
// setup` (non-empty lastSetupVersion) and the running CLI differs from the
// version that last set it up. A dev build never auto-refreshes, and the
// comparison is an exact mismatch so downgrades re-install the matching skills
// too. It never opts a user into the MCP server on its own.
func shouldRefreshMCPSetup(lastSetupVersion, currentVersion string) bool {
	if version.IsDev(currentVersion) {
		return false
	}
	if lastSetupVersion == "" {
		return false
	}
	return lastSetupVersion != currentVersion
}

// mcpRefreshEUID is os.Geteuid; a variable so tests can run as any user.
var mcpRefreshEUID = os.Geteuid

// mcpRefreshAsForeignUser reports whether the process runs as someone other
// than the owner of HOME: under sudo (sudoUID is $SUDO_UID; sudo keeps $HOME
// on macOS) or with a HOME another user owns. Files a refresh wrote then would
// belong to the wrong user. Root in its own HOME — a container or a root-only
// machine — is fine. ownerKnown is false where files have no POSIX owner.
func mcpRefreshAsForeignUser(sudoUID string, euid, homeOwner int, ownerKnown bool) bool {
	return sudoUID != "" || (ownerKnown && homeOwner != euid)
}

// mcpRefreshRunsAsForeignUser is mcpRefreshAsForeignUser for this process.
func mcpRefreshRunsAsForeignUser() bool {
	owner, known := -1, false
	if home, err := os.UserHomeDir(); err == nil {
		if fi, err := os.Stat(home); err == nil {
			owner, _, known = fileOwner(fi)
		}
	}
	return mcpRefreshAsForeignUser(os.Getenv("SUDO_UID"), mcpRefreshEUID(), owner, known)
}

// maybeRefreshMCPSetup re-applies the MCP server configuration and re-installs
// the bundled skills when the CLI has been upgraded (or downgraded) since
// `wendy mcp setup` last ran, so users automatically pick up the latest skills.
// It runs silently and only touches tools that are already configured; the
// underlying setup helpers no-op for tools they don't detect. It never runs as
// a user other than HOME's owner: sudo keeps $HOME on macOS, so a `sudo wendy …`
// right after an upgrade would otherwise leave the user root-owned AI tool
// configs. The next run as HOME's owner refreshes instead. Nor does it run when
// the Wendy plugin's launcher started this CLI for Claude or Codex: that
// process must not rewrite AI tool configs behind the user's back, and the
// next command the user runs in a terminal refreshes instead.
func maybeRefreshMCPSetup(cfg *config.Config) {
	if pluginmode.Client() != "" || mcpRefreshRunsAsForeignUser() || !shouldRefreshMCPSetup(cfg.LastMCPSetupVersion, version.Version) {
		return
	}
	setupMCPForAllTools()
	installSkillsForAllTools()
	cfg.LastMCPSetupVersion = version.Version
	_ = config.Save(cfg)
}

// mcpRestartHints says how an already-running instance of each MCP client
// picks up a server added to its config. Keys match mcpSetupResult.tool.
var mcpRestartHints = map[string]string{
	"Claude Code":    "start a new session (exit, run `claude` again), then check `/mcp`",
	"Claude Desktop": "quit it completely (not just close the window) and reopen it",
	"Cursor":         "restart Cursor",
	"Windsurf":       "restart Windsurf",
	"Codex":          "start a new Codex session",
}

// mcpRestartNotice lists the AI tools setup configured successfully and how to
// make a running instance load the wendy MCP server — config changes are only
// read at startup. It returns "" when no MCP client was configured. Only
// `wendy mcp setup` prints it; the silent upgrade refresh stays quiet.
func mcpRestartNotice(results []mcpSetupResult) string {
	var b strings.Builder
	for _, r := range results {
		hint, ok := mcpRestartHints[r.tool]
		if !ok || r.err != nil {
			continue
		}
		fmt.Fprintf(&b, "  • %s: %s\n", r.tool, hint)
	}
	if b.Len() == 0 {
		return ""
	}
	return "\nRestart any of these that are already running so they load the wendy MCP server:\n" + b.String()
}

type mcpSetupResult struct {
	tool string
	path string
	err  error
}

func setupMCPForAllTools() []mcpSetupResult {
	wendyBin := wendyBinaryPath()
	entry := map[string]any{
		"type":    "stdio",
		"command": wendyBin,
		"args":    []string{"mcp", "serve"},
	}

	var results []mcpSetupResult

	// Claude Code (~/.claude.json)
	if claudeCodePath := claudeCodeConfigPath(); claudeCodePath != "" {
		if err := addMCPToJSONConfig(claudeCodePath, "mcpServers", "wendy", entry); err != nil {
			results = append(results, mcpSetupResult{tool: "Claude Code", path: claudeCodePath, err: err})
		} else {
			results = append(results, mcpSetupResult{tool: "Claude Code", path: claudeCodePath})
		}
	}

	// Claude Desktop
	if desktopPath := claudeDesktopConfigPath(); desktopPath != "" {
		if err := addMCPToJSONConfig(desktopPath, "mcpServers", "wendy", entry); err != nil {
			results = append(results, mcpSetupResult{tool: "Claude Desktop", path: desktopPath, err: err})
		} else {
			results = append(results, mcpSetupResult{tool: "Claude Desktop", path: desktopPath})
		}
	}

	// Cursor (~/.cursor/mcp.json)
	if cursorPath := cursorConfigPath(); cursorPath != "" {
		if err := addMCPToJSONConfig(cursorPath, "mcpServers", "wendy", entry); err != nil {
			results = append(results, mcpSetupResult{tool: "Cursor", path: cursorPath, err: err})
		} else {
			results = append(results, mcpSetupResult{tool: "Cursor", path: cursorPath})
		}
	}

	// Windsurf (~/.codeium/windsurf/mcp_config.json)
	if windsurfPath := windsurfConfigPath(); windsurfPath != "" {
		if err := addMCPToJSONConfig(windsurfPath, "mcpServers", "wendy", entry); err != nil {
			results = append(results, mcpSetupResult{tool: "Windsurf", path: windsurfPath, err: err})
		} else {
			results = append(results, mcpSetupResult{tool: "Windsurf", path: windsurfPath})
		}
	}

	// Codex (~/.codex/config.toml)
	if codexPath := codexConfigPath(); codexPath != "" {
		if err := addMCPToTOMLConfig(codexPath, "mcp_servers", "wendy", wendyBin, []string{"mcp", "serve"}); err != nil {
			results = append(results, mcpSetupResult{tool: "Codex", path: codexPath, err: err})
		} else {
			results = append(results, mcpSetupResult{tool: "Codex", path: codexPath})
		}
	}

	return results
}

// claudeCodeConfigPath returns ~/.claude.json if it exists.
func claudeCodeConfigPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	p := filepath.Join(home, ".claude.json")
	if _, err := os.Stat(p); err == nil {
		return p
	}
	// Also detect claude binary presence even without a config file yet.
	if _, err := exec.LookPath("claude"); err == nil {
		return p
	}
	return ""
}

func claudeDesktopConfigPath() string {
	var dir string
	switch runtime.GOOS {
	case "darwin":
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		dir = filepath.Join(home, "Library", "Application Support", "Claude")
	case "linux":
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		dir = filepath.Join(home, ".config", "Claude")
	case "windows":
		appdata := os.Getenv("APPDATA")
		if appdata == "" {
			return ""
		}
		dir = filepath.Join(appdata, "Claude")
	default:
		return ""
	}
	if _, err := os.Stat(dir); err != nil {
		return ""
	}
	return filepath.Join(dir, "claude_desktop_config.json")
}

func wendyBinaryPath() string {
	if p, err := os.Executable(); err == nil {
		return p
	}
	if p, err := exec.LookPath("wendy"); err == nil {
		return p
	}
	return "wendy"
}

// cursorConfigPath returns ~/.cursor/mcp.json if Cursor is installed.
func cursorConfigPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	dir := filepath.Join(home, ".cursor")
	if _, err := os.Stat(dir); err == nil {
		return filepath.Join(dir, "mcp.json")
	}
	if _, err := exec.LookPath("cursor"); err == nil {
		return filepath.Join(dir, "mcp.json")
	}
	return ""
}

// windsurfConfigPath returns ~/.codeium/windsurf/mcp_config.json if Windsurf is installed.
func windsurfConfigPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	dir := filepath.Join(home, ".codeium", "windsurf")
	if _, err := os.Stat(dir); err == nil {
		return filepath.Join(dir, "mcp_config.json")
	}
	if _, err := exec.LookPath("windsurf"); err == nil {
		return filepath.Join(dir, "mcp_config.json")
	}
	return ""
}

// mcpArgsStartWith reports whether args, a decoded JSON or TOML array, starts
// with want. Args that already start with the ones setup writes ("mcp serve")
// belong to the user — the wendy-mcp-setup skill tells users to pin a device
// with `mcp serve --device <host>` — so setup leaves them alone.
func mcpArgsStartWith(args any, want []string) bool {
	list, ok := args.([]any)
	if !ok || len(list) < len(want) {
		return false
	}
	for i, w := range want {
		if s, ok := list[i].(string); !ok || s != w {
			return false
		}
	}
	return true
}

// addMCPToJSONConfig sets the keys in entry on cfg[topKey][name] in the JSON
// file at path. Keys the user added to an existing entry (env, timeouts) are
// kept, and so are existing args that already start with entry's args (a
// pinned --device); only the other keys setup writes are replaced.
func addMCPToJSONConfig(path, topKey, name string, entry map[string]any) error {
	var cfg map[string]any
	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("reading %s: %w", path, err)
	}
	isNew := os.IsNotExist(err)
	if len(data) > 0 {
		if err := json.Unmarshal(data, &cfg); err != nil {
			return fmt.Errorf("parsing %s: %w", path, err)
		}
	}
	if cfg == nil {
		cfg = map[string]any{}
	}
	top, _ := cfg[topKey].(map[string]any)
	if top == nil {
		top = map[string]any{}
	}
	merged := make(map[string]any, len(entry))
	if old, ok := top[name].(map[string]any); ok {
		for k, v := range old {
			merged[k] = v
		}
	}
	for k, v := range entry {
		if want, ok := v.([]string); ok && k == "args" && mcpArgsStartWith(merged["args"], want) {
			continue
		}
		merged[k] = v
	}
	top[name] = merged
	cfg[topKey] = top

	out, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	if err := mkdirAllLikeParent(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(path, out, 0o644); err != nil {
		return err
	}
	if isNew {
		copyOwnerFromParent(path) // an existing file is written in place and keeps its owner
	}
	return nil
}

// codexConfigPath returns ~/.codex/config.toml if Codex is installed.
func codexConfigPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	dir := filepath.Join(home, ".codex")
	if _, err := os.Stat(dir); err == nil {
		return filepath.Join(dir, "config.toml")
	}
	if _, err := exec.LookPath("codex"); err == nil {
		return filepath.Join(dir, "config.toml")
	}
	return ""
}
