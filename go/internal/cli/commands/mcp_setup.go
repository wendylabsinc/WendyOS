package commands

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

func newMCPSetupCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "setup",
		Short: "Configure the Wendy MCP server in Claude Code and Claude Desktop",
		Long:  "Detects installed AI tools and adds the wendy MCP server to their configuration.",
		RunE: func(cmd *cobra.Command, args []string) error {
			results := setupMCPForAllTools()
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
			return nil
		},
	}
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

	// Codex (~/.codex/config.yaml)
	if codexPath := codexConfigPath(); codexPath != "" {
		if err := addMCPToYAMLConfig(codexPath, "mcpServers", "wendy", entry); err != nil {
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

// claudeDesktopConfigPath returns the Claude Desktop config path if the app
// directory exists, or "" if Claude Desktop is not installed.
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

// wendyBinaryPath returns the absolute path to the currently running wendy
// binary, falling back to PATH lookup.
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

// addMCPToYAMLConfig reads a YAML config file, sets cfg[topKey][name] = entry,
// and writes it back. Creates the file if it does not exist.
func addMCPToYAMLConfig(path, topKey, name string, entry any) error {
	var cfg map[string]any
	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("reading %s: %w", path, err)
	}
	if len(data) > 0 {
		if err := yaml.Unmarshal(data, &cfg); err != nil {
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
	top[name] = entry
	cfg[topKey] = top
	out, err := yaml.Marshal(cfg)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, out, 0o644)
}

// codexConfigPath returns ~/.codex/config.yaml if Codex is installed.
func codexConfigPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	dir := filepath.Join(home, ".codex")
	if _, err := os.Stat(dir); err == nil {
		return filepath.Join(dir, "config.yaml")
	}
	if _, err := exec.LookPath("codex"); err == nil {
		return filepath.Join(dir, "config.yaml")
	}
	return ""
}

// addMCPToJSONConfig reads a JSON config file, sets cfg[topKey][name] = entry,
// and writes it back. Creates the file if it does not exist.
func addMCPToJSONConfig(path, topKey, name string, entry any) error {
	var cfg map[string]any
	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("reading %s: %w", path, err)
	}
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
	top[name] = entry
	cfg[topKey] = top

	out, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, out, 0o644)
}
