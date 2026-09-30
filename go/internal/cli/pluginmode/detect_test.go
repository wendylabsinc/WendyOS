package pluginmode

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// wendyPluginMCPJSON is the Wendy plugin's .mcp.json (plan 4).
const wendyPluginMCPJSON = `{"mcpServers":{"wendy":{"command":"${CLAUDE_PLUGIN_ROOT}/scripts/wendy","args":["mcp","serve"],"env":{"WENDY_PLUGIN":"claude"}}}}`

// claudeFixture is one Claude Code plugin install: key "<name>@<marketplace>",
// its scope, the .mcp.json in its install folder ("" writes none) and its
// enabledPlugins value (nil writes no settings.json).
type claudeFixture struct {
	key, scope, mcpJSON string
	enabled             *bool
}

func (f claudeFixture) write(t *testing.T, home string) {
	t.Helper()
	installPath := filepath.Join(home, ".claude", "plugins", "cache", "fixture", f.key)
	if err := os.MkdirAll(installPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if f.mcpJSON != "" {
		writeFile(t, filepath.Join(installPath, ".mcp.json"), f.mcpJSON)
	}
	installed, err := json.Marshal(map[string]any{
		"version": 2,
		"plugins": map[string]any{f.key: []map[string]any{{"scope": f.scope, "installPath": installPath}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(home, ".claude", "plugins", "installed_plugins.json"), string(installed))
	if f.enabled != nil {
		settings, err := json.Marshal(map[string]any{"enabledPlugins": map[string]bool{f.key: *f.enabled}})
		if err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Join(home, ".claude", "settings.json"), string(settings))
	}
}

func TestClaudePluginActive(t *testing.T) {
	yes, no := true, false
	const key = "wendy@wendy-agentic-coding"
	tests := []struct {
		name    string
		fixture *claudeFixture
		want    bool
	}{
		{"nothing installed", nil, false},
		{"user scope and enabled", &claudeFixture{key, "user", wendyPluginMCPJSON, &yes}, true},
		{"scope omitted counts as user", &claudeFixture{key, "", wendyPluginMCPJSON, &yes}, true},
		{"disabled", &claudeFixture{key, "user", wendyPluginMCPJSON, &no}, false},
		{"no enabledPlugins entry", &claudeFixture{key, "user", wendyPluginMCPJSON, nil}, false},
		{"project scope only", &claudeFixture{key, "project", wendyPluginMCPJSON, &yes}, false},
		{"old skills-only wendy@wendy-skills, enabled", &claudeFixture{"wendy@wendy-skills", "user", "", &yes}, false},
		{"a plugin named wendy without the wendy server", &claudeFixture{key, "user", `{"mcpServers":{"other":{"command":"x"}}}`, &yes}, false},
		{"different plugin name", &claudeFixture{"wendy-engineering@wendy-agentic-coding", "user", wendyPluginMCPJSON, &yes}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			home := t.TempDir()
			if tt.fixture != nil {
				tt.fixture.write(t, home)
			}
			if got := ClaudePluginActive(home); got != tt.want {
				t.Errorf("ClaudePluginActive = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestClaudePluginActive_UnreadableRegistry(t *testing.T) {
	yes := true
	home := t.TempDir()
	claudeFixture{"wendy@wendy-agentic-coding", "user", wendyPluginMCPJSON, &yes}.write(t, home)
	writeFile(t, filepath.Join(home, ".claude", "plugins", "installed_plugins.json"), "{not json")
	if ClaudePluginActive(home) {
		t.Error("a corrupt registry must count as not active")
	}
}

func TestCodexPluginActive(t *testing.T) {
	tests := []struct {
		name, config string
		want         bool
	}{
		{"no config", "", false},
		{"enabled", "[plugins.\"wendy@wendy-agentic-coding\"]\nenabled = true\n", true},
		{"enabled under an explicit [plugins] header", "[plugins]\n  [plugins.\"wendy@wendy-agentic-coding\"]\n    enabled = true\n", true},
		{"disabled", "[plugins.\"wendy@wendy-agentic-coding\"]\nenabled = false\n", false},
		{"enabled key missing", "[plugins.\"wendy@wendy-agentic-coding\"]\n", false},
		{"different plugin", "[plugins.\"wendy-engineering@wendy-agentic-coding\"]\nenabled = true\n", false},
		{"invalid TOML", "[plugins\n", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			home := t.TempDir()
			if tt.config != "" {
				writeFile(t, filepath.Join(home, ".codex", "config.toml"), tt.config)
			}
			if got := CodexPluginActive(home); got != tt.want {
				t.Errorf("CodexPluginActive = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestUserLevelServer(t *testing.T) {
	home := t.TempDir()
	claudePath := filepath.Join(home, ".claude.json")
	codexPath := filepath.Join(home, ".codex", "config.toml")

	if _, found := UserLevelServer("claude", home); found {
		t.Error("claude: found a server with no ~/.claude.json")
	}
	writeFile(t, claudePath, `{"mcpServers":{"other":{"command":"x"}}}`)
	if _, found := UserLevelServer("claude", home); found {
		t.Error("claude: another server is not a wendy server")
	}
	writeFile(t, claudePath, `{"numStartups": 3, "mcpServers":{"wendy":{"command":"/usr/local/bin/wendy","args":["mcp","serve"]}}}`)
	if path, found := UserLevelServer("claude", home); !found || path != claudePath {
		t.Errorf("claude: UserLevelServer = %q, %v; want %q, true", path, found, claudePath)
	}

	writeFile(t, codexPath, "[mcp_servers.other]\ncommand = \"x\"\n")
	if _, found := UserLevelServer("codex", home); found {
		t.Error("codex: another server is not a wendy server")
	}
	writeFile(t, codexPath, "[mcp_servers.wendy]\ncommand = \"/usr/local/bin/wendy\"\nargs = [\"mcp\", \"serve\"]\n")
	if path, found := UserLevelServer("codex", home); !found || path != codexPath {
		t.Errorf("codex: UserLevelServer = %q, %v; want %q, true", path, found, codexPath)
	}

	if _, found := UserLevelServer("cursor", home); found {
		t.Error("unknown clients never report a server")
	}
}

func TestSetupRemovesUserLevelServer(t *testing.T) {
	yes, no := true, false
	const key = "wendy@wendy-agentic-coding"
	const ownJSON = `{"mcpServers":{"wendy":{"command":"/w","args":["mcp","serve"]}}}`
	const wrapperJSON = `{"mcpServers":{"wendy":{"command":"/w","args":["wrap","mcp","serve"]}}}`
	const ownTOML = "[mcp_servers.wendy]\ncommand = \"/w\"\nargs = [\"mcp\", \"serve\"]\n"
	tests := []struct {
		name, client string
		fixture      *claudeFixture
		claudeJSON   string
		codexTOML    string
		want         bool
	}{
		{"claude active, own entry", "claude", &claudeFixture{key, "user", wendyPluginMCPJSON, &yes}, ownJSON, "", true},
		{"claude active, wrapper args", "claude", &claudeFixture{key, "user", wendyPluginMCPJSON, &yes}, wrapperJSON, "", false},
		{"claude not active, own entry", "claude", &claudeFixture{key, "user", wendyPluginMCPJSON, &no}, ownJSON, "", false},
		{"codex enabled, own entry", "codex", nil, "", ownTOML + "[plugins.\"" + key + "\"]\nenabled = true\n", true},
		{"codex disabled, own entry", "codex", nil, "", ownTOML + "[plugins.\"" + key + "\"]\nenabled = false\n", false},
		{"unknown client", "cursor", &claudeFixture{key, "user", wendyPluginMCPJSON, &yes}, ownJSON, ownTOML + "[plugins.\"" + key + "\"]\nenabled = true\n", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			home := t.TempDir()
			if tt.fixture != nil {
				tt.fixture.write(t, home)
			}
			if tt.claudeJSON != "" {
				writeFile(t, filepath.Join(home, ".claude.json"), tt.claudeJSON)
			}
			if tt.codexTOML != "" {
				writeFile(t, filepath.Join(home, ".codex", "config.toml"), tt.codexTOML)
			}
			if got := SetupRemovesUserLevelServer(tt.client, home); got != tt.want {
				t.Errorf("SetupRemovesUserLevelServer = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestClaudePluginActive_RelativeInstallPathNeverCounts(t *testing.T) {
	for _, rel := range []string{"", "rel"} {
		t.Run("installPath="+rel, func(t *testing.T) {
			home := t.TempDir()
			cwd := t.TempDir()
			writeFile(t, filepath.Join(cwd, ".mcp.json"), wendyPluginMCPJSON)
			if rel != "" {
				writeFile(t, filepath.Join(cwd, rel, ".mcp.json"), wendyPluginMCPJSON)
			}
			t.Chdir(cwd)
			const key = "wendy@wendy-agentic-coding"
			installed, _ := json.Marshal(map[string]any{
				"version": 2,
				"plugins": map[string]any{key: []map[string]any{{"scope": "user", "installPath": rel}}},
			})
			writeFile(t, filepath.Join(home, ".claude", "plugins", "installed_plugins.json"), string(installed))
			writeFile(t, filepath.Join(home, ".claude", "settings.json"), `{"enabledPlugins":{"`+key+`":true}}`)
			if ClaudePluginActive(home) {
				t.Error("a relative installPath must not count as active")
			}
		})
	}
}
