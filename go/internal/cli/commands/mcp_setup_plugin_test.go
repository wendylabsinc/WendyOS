package commands

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	toml "github.com/BurntSushi/toml"
)

// writeClaudeWendyPlugin registers the Wendy plugin for Claude Code under
// home: an install folder with the plugin's .mcp.json, installed_plugins.json
// at user scope and enabledPlugins set to enabled.
func writeClaudeWendyPlugin(t *testing.T, home string, enabled bool) {
	t.Helper()
	writeClaudePluginInstall(t, home, "wendy@wendy-agentic-coding", enabled,
		`{"mcpServers":{"wendy":{"command":"${CLAUDE_PLUGIN_ROOT}/scripts/wendy","args":["mcp","serve"],"env":{"WENDY_PLUGIN":"claude"}}}}`)
}

// writeClaudePluginInstall registers plugin key for Claude Code under home,
// with mcpJSON ("" = none) in its install folder.
func writeClaudePluginInstall(t *testing.T, home, key string, enabled bool, mcpJSON string) {
	t.Helper()
	installPath := filepath.Join(home, ".claude", "plugins", "cache", "fixture", key)
	if err := os.MkdirAll(installPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if mcpJSON != "" {
		if err := os.WriteFile(filepath.Join(installPath, ".mcp.json"), []byte(mcpJSON), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	installed, _ := json.Marshal(map[string]any{
		"version": 2,
		"plugins": map[string]any{key: []map[string]any{{"scope": "user", "installPath": installPath}}},
	})
	settings, _ := json.Marshal(map[string]any{"enabledPlugins": map[string]bool{key: enabled}})
	for path, data := range map[string][]byte{
		filepath.Join(home, ".claude", "plugins", "installed_plugins.json"): installed,
		filepath.Join(home, ".claude", "settings.json"):                     settings,
	} {
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// resultFor returns the setup result for tool.
func resultFor(t *testing.T, results []mcpSetupResult, tool string) mcpSetupResult {
	t.Helper()
	for _, r := range results {
		if r.tool == tool {
			return r
		}
	}
	t.Fatalf("no %s result in %+v", tool, results)
	return mcpSetupResult{}
}

func TestSetupMCPForAllTools_ClaudePluginRemovesOwnEntry(t *testing.T) {
	home := setupMCPRefreshTest(t)
	writeClaudeWendyPlugin(t, home, true)
	claudePath := filepath.Join(home, ".claude.json")
	orig := `{"numStartups": 12345678901234567, "lastPrompt": "a < b && c", ` +
		`"mcpServers": {"wendy": {"type": "stdio", "command": "/old/wendy", "args": ["mcp", "serve"]}, "other": {"command": "other"}}}`
	if err := os.WriteFile(claudePath, []byte(orig), 0o644); err != nil {
		t.Fatal(err)
	}

	r := resultFor(t, setupMCPForAllTools(), "Claude Code")
	if r.err != nil || r.note != mcpNoteRemovedForPlugin {
		t.Fatalf("Claude Code result = %+v, want note %q", r, mcpNoteRemovedForPlugin)
	}
	raw, err := os.ReadFile(claudePath)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"12345678901234567", "a < b && c", `"other"`} {
		if !bytes.Contains(raw, []byte(want)) {
			t.Errorf("~/.claude.json lost %s:\n%s", want, raw)
		}
	}
	var cfg map[string]any
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	if _, ok := cfg["mcpServers"].(map[string]any)["wendy"]; ok {
		t.Errorf("the user-level wendy server is still configured:\n%s", raw)
	}
}

func TestSetupMCPForAllTools_ClaudePluginWithoutEntryIsSkipped(t *testing.T) {
	home := setupMCPRefreshTest(t)
	writeClaudeWendyPlugin(t, home, true)
	claudePath := filepath.Join(home, ".claude.json")
	if err := os.WriteFile(claudePath, []byte(`{"mcpServers": {}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	r := resultFor(t, setupMCPForAllTools(), "Claude Code")
	if r.err != nil || r.note != mcpNoteSkippedForPlugin {
		t.Fatalf("Claude Code result = %+v, want note %q", r, mcpNoteSkippedForPlugin)
	}
	if got, _ := os.ReadFile(claudePath); string(got) != `{"mcpServers": {}}` {
		t.Errorf("~/.claude.json was rewritten: %s", got)
	}
}

func TestSetupMCPForAllTools_KeepsCustomizedClaudeEntry(t *testing.T) {
	home := setupMCPRefreshTest(t)
	writeClaudeWendyPlugin(t, home, true)
	claudePath := filepath.Join(home, ".claude.json")
	orig := `{"mcpServers": {"wendy": {"command": "/usr/local/bin/wendy-wrapper", "args": ["--profile", "lab"]}}}`
	if err := os.WriteFile(claudePath, []byte(orig), 0o644); err != nil {
		t.Fatal(err)
	}
	r := resultFor(t, setupMCPForAllTools(), "Claude Code")
	if r.err != nil || r.note != mcpNoteSkippedForPlugin {
		t.Fatalf("Claude Code result = %+v, want note %q", r, mcpNoteSkippedForPlugin)
	}
	if got, _ := os.ReadFile(claudePath); string(got) != orig {
		t.Errorf("a customized wendy entry was changed:\n%s", got)
	}
}

func TestSetupMCPForAllTools_DisabledClaudePluginStillConfigures(t *testing.T) {
	home := setupMCPRefreshTest(t)
	writeClaudeWendyPlugin(t, home, false)
	claudePath := filepath.Join(home, ".claude.json")
	if err := os.WriteFile(claudePath, []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	r := resultFor(t, setupMCPForAllTools(), "Claude Code")
	if r.err != nil || r.note != "" {
		t.Fatalf("Claude Code result = %+v, want a plain configured result", r)
	}
	var cfg map[string]any
	data, _ := os.ReadFile(claudePath)
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatal(err)
	}
	if _, ok := cfg["mcpServers"].(map[string]any)["wendy"]; !ok {
		t.Errorf("setup did not configure Claude Code with the plugin disabled:\n%s", data)
	}
}

// Earlier `wendy mcp setup` runs registered a skills-only plugin named
// wendy@wendy-skills; enabling it must not stop setup from configuring the
// MCP server, or Claude would have no Wendy tools at all.
func TestSetupMCPForAllTools_OldWendySkillsPluginIsNotThePlugin(t *testing.T) {
	home := setupMCPRefreshTest(t)
	writeClaudePluginInstall(t, home, "wendy@wendy-skills", true, "")
	claudePath := filepath.Join(home, ".claude.json")
	if err := os.WriteFile(claudePath, []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	r := resultFor(t, setupMCPForAllTools(), "Claude Code")
	if r.err != nil || r.note != "" {
		t.Fatalf("Claude Code result = %+v, want a plain configured result", r)
	}
}

func TestSetupMCPForAllTools_CodexPluginRemovesOwnEntry(t *testing.T) {
	home := setupMCPRefreshTest(t)
	codexPath := filepath.Join(home, ".codex", "config.toml")
	src := "# my codex config\nmodel = \"gpt-6\"\n\n" +
		"[mcp_servers.wendy]\ncommand = \"/old/wendy\"\nargs = [\"mcp\", \"serve\"]\n\n" +
		"[plugins.\"wendy@wendy-agentic-coding\"]\nenabled = true\n"
	if err := os.WriteFile(codexPath, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}

	r := resultFor(t, setupMCPForAllTools(), "Codex")
	if r.err != nil || r.note != mcpNoteRemovedForPlugin {
		t.Fatalf("Codex result = %+v, want note %q", r, mcpNoteRemovedForPlugin)
	}
	got, err := os.ReadFile(codexPath)
	if err != nil {
		t.Fatal(err)
	}
	want := "# my codex config\nmodel = \"gpt-6\"\n\n[plugins.\"wendy@wendy-agentic-coding\"]\nenabled = true\n"
	if string(got) != want {
		t.Errorf("config.toml:\n%q\nwant:\n%q", got, want)
	}
	var doc map[string]any
	if _, err := toml.Decode(string(got), &doc); err != nil {
		t.Fatalf("result is not valid TOML: %v", err)
	}
}

func TestMCPSetupCmd_ReportsPluginClients(t *testing.T) {
	home := setupMCPRefreshTest(t)
	writeClaudeWendyPlugin(t, home, true)
	if err := os.WriteFile(filepath.Join(home, ".claude.json"),
		[]byte(`{"mcpServers": {"wendy": {"command": "/old/wendy", "args": ["mcp", "serve"]}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".codex", "config.toml"),
		[]byte("[plugins.\"wendy@wendy-agentic-coding\"]\nenabled = true\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	cmd := newMCPSetupCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(nil)
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"✓ Claude Code: removed the user-level wendy server (the plugin provides it)",
		"↷ Codex: skipped — the Wendy plugin provides the MCP server",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
	for _, unwanted := range []string{"• Claude Code:", "• Codex:"} {
		if strings.Contains(out.String(), unwanted) {
			t.Errorf("restart notice lists a plugin client (%q):\n%s", unwanted, out.String())
		}
	}
}
