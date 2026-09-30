package pluginmode

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	toml "github.com/BurntSushi/toml"
)

// pluginName is the Wendy plugin's name in Claude's and Codex's plugin
// registries, where installs are keyed "<name>@<marketplace>".
const pluginName = "wendy"

func isWendyPluginKey(key string) bool {
	name, _, ok := strings.Cut(key, "@")
	return ok && name == pluginName
}

// readJSON decodes the JSON file at path into v and reports success.
func readJSON(path string, v any) bool {
	data, err := os.ReadFile(path)
	return err == nil && json.Unmarshal(data, v) == nil
}

// ClaudePluginActive reports whether Claude Code has the Wendy plugin
// installed at user scope and enabled, or synced from the claude.ai directory
// (see claudeSyncedPluginPresent). A project-scoped install does not
// count: every other project still uses the user-level server `wendy mcp
// setup` writes. Anything unreadable counts as "not active", so setup keeps
// writing the user-level server rather than leaving Claude with no Wendy tools.
func ClaudePluginActive(home string) bool {
	return claudeMarketplacePluginActive(home) || claudeSyncedPluginPresent(home)
}

func claudeMarketplacePluginActive(home string) bool {
	var installed struct {
		Plugins map[string][]struct {
			Scope       string `json:"scope"`
			InstallPath string `json:"installPath"`
		} `json:"plugins"`
	}
	if !readJSON(filepath.Join(home, ".claude", "plugins", "installed_plugins.json"), &installed) {
		return false
	}
	var settings struct {
		EnabledPlugins map[string]bool `json:"enabledPlugins"`
	}
	if !readJSON(filepath.Join(home, ".claude", "settings.json"), &settings) {
		return false
	}
	for key, installs := range installed.Plugins {
		if !isWendyPluginKey(key) || !settings.EnabledPlugins[key] {
			continue
		}
		for _, in := range installs {
			if !filepath.IsAbs(in.InstallPath) {
				continue // a relative path would resolve against the working directory
			}
			if (in.Scope == "" || in.Scope == "user") && providesWendyServer(filepath.Join(in.InstallPath, ".mcp.json")) {
				return true
			}
		}
	}
	return false
}

// providesWendyServer reports whether the plugin MCP file at path declares the
// Wendy plugin's server: a "wendy" entry whose env sets WENDY_PLUGIN. Earlier
// `wendy mcp setup` runs registered a skills-only plugin also named "wendy"
// (wendy@wendy-skills); it has no MCP file, so it never counts.
func providesWendyServer(path string) bool {
	var f struct {
		MCPServers map[string]struct {
			Env map[string]string `json:"env"`
		} `json:"mcpServers"`
	}
	if !readJSON(path, &f) {
		return false
	}
	_, ok := f.MCPServers["wendy"].Env[EnvVar]
	return ok
}

// claudeSyncedPluginPresent reports whether the Wendy plugin reached Claude
// Code as a synced plugin: one added from the claude.ai directory, which Claude
// Code loads as "wendy@synced". Such a plugin has no installed_plugins.json or
// enabledPlugins entry; its root is ~/.claude/plugins/synced/<bucket>/<name>/,
// exactly one level under the bucket. It counts only when that folder's
// .mcp.json declares the Wendy server AND the bucket's manifest.json lists the
// plugin (a leftover folder alone doesn't count: doubt means not active).
// Hidden bucket entries (.staging, .marketplaces.json, ...) are skipped. The
// files appear only after a Claude Code session has started once since the
// directory install, so setup run before that keeps the user-level server. No
// per-plugin disable flag was observed. See the S3 section of
// docs/superpowers/specs/2026-09-29-wendy-plugin-spike-findings.md.
func claudeSyncedPluginPresent(home string) bool {
	synced := filepath.Join(home, ".claude", "plugins", "synced")
	buckets, err := os.ReadDir(synced)
	if err != nil {
		return false
	}
	for _, b := range buckets {
		if !b.IsDir() || strings.HasPrefix(b.Name(), ".") {
			continue
		}
		bucket := filepath.Join(synced, b.Name())
		if !providesWendyServer(filepath.Join(bucket, pluginName, ".mcp.json")) {
			continue
		}
		var manifest struct {
			Plugins []struct {
				Name string `json:"name"`
			} `json:"plugins"`
		}
		if !readJSON(filepath.Join(bucket, "manifest.json"), &manifest) {
			continue
		}
		for _, p := range manifest.Plugins {
			if p.Name == pluginName {
				return true
			}
		}
	}
	return false
}

// CodexPluginActive reports whether <home>/.codex/config.toml enables a Wendy
// plugin ([plugins."wendy@<marketplace>"] enabled = true). A missing enabled
// key counts as disabled, for the same reason as in ClaudePluginActive.
func CodexPluginActive(home string) bool {
	var cfg struct {
		Plugins map[string]struct {
			Enabled *bool `toml:"enabled"`
		} `toml:"plugins"`
	}
	if _, err := toml.DecodeFile(filepath.Join(home, ".codex", "config.toml"), &cfg); err != nil {
		return false
	}
	for key, p := range cfg.Plugins {
		if isWendyPluginKey(key) && p.Enabled != nil && *p.Enabled {
			return true
		}
	}
	return false
}

// UserLevelServer returns the config file holding a user-level "wendy" MCP
// server for client ("claude": ~/.claude.json, "codex": ~/.codex/config.toml).
// Next to the plugin's own server it lists every Wendy tool twice.
func UserLevelServer(client, home string) (string, bool) {
	switch client {
	case "claude":
		path := filepath.Join(home, ".claude.json")
		var cfg struct {
			MCPServers map[string]json.RawMessage `json:"mcpServers"`
		}
		if readJSON(path, &cfg) {
			if _, ok := cfg.MCPServers["wendy"]; ok {
				return path, true
			}
		}
	case "codex":
		path := filepath.Join(home, ".codex", "config.toml")
		var cfg struct {
			MCPServers map[string]toml.Primitive `toml:"mcp_servers"`
		}
		if _, err := toml.DecodeFile(path, &cfg); err == nil {
			if _, ok := cfg.MCPServers["wendy"]; ok {
				return path, true
			}
		}
	}
	return "", false
}

// SetupRemovesUserLevelServer reports whether `wendy mcp setup` would remove
// client's user-level "wendy" MCP server under home. Only Claude Code's is
// removed: the plugin is active (ClaudePluginActive) and the entry is one setup
// wrote, i.e. its args start with "mcp serve". Anything else — a customized
// entry, a plugin loaded only per project or with --plugin-dir, or an
// unreadable file — means setup keeps the entry. Setup never removes the Codex
// entry: a user-level "wendy" server shadows the plugin's same-name server in
// Codex, so removal cleans up nothing, and the Codex IDE extension needs it.
func SetupRemovesUserLevelServer(client, home string) bool {
	switch client {
	case "claude":
		if !ClaudePluginActive(home) {
			return false
		}
		var cfg struct {
			MCPServers map[string]struct {
				Args []any `json:"args"`
			} `json:"mcpServers"`
		}
		return readJSON(filepath.Join(home, ".claude.json"), &cfg) &&
			argsStartWithMCPServe(cfg.MCPServers["wendy"].Args)
	}
	return false
}

func argsStartWithMCPServe(args []any) bool {
	return len(args) >= 2 && args[0] == "mcp" && args[1] == "serve"
}
