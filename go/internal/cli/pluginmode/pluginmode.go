// Package pluginmode tells the CLI when the Wendy plugin for Claude or Codex
// started it, whether the running binary is one the plugin's launcher
// installed, and whether a client has the plugin installed.
package pluginmode

import (
	"os"
	"path/filepath"
	"strings"
)

// EnvVar is set by the Wendy plugin's MCP entries (.mcp.json and
// codex.mcp.json) to the client that started the CLI.
const EnvVar = "WENDY_PLUGIN"

// Client returns "claude" or "codex" when the Wendy plugin started this CLI,
// else "". Any other value is ignored, so a typo never switches modes.
func Client() string {
	switch v := os.Getenv(EnvVar); v {
	case "claude", "codex":
		return v
	default:
		return ""
	}
}

// ManagedInstall reports whether exePath is a CLI the plugin's launcher
// installed: a binary under <configDir>/cli/ or the <configDir>/bin/ pointer
// to it (a symlink on POSIX, a copy on Windows). Both paths are resolved
// first, so a symlinked HOME or config directory still matches.
func ManagedInstall(exePath, configDir string) bool {
	if exePath == "" || configDir == "" {
		return false
	}
	exe := resolvePath(exePath)
	root := resolvePath(configDir)
	for _, sub := range []string{"cli", "bin"} {
		rel, err := filepath.Rel(filepath.Join(root, sub), exe)
		if err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// resolvePath returns p with symlinks evaluated, or cleaned when it cannot
// be resolved (for example because it does not exist).
func resolvePath(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return filepath.Clean(p)
}
