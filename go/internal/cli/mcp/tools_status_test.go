package mcp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/shared/config"
)

func TestWendyStatus_NotConnected(t *testing.T) {
	srv := New(&config.Config{}, nil)
	result, err := srv.handleWendyStatus(context.Background(), callToolReq("wendy_status", nil))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected tool error: %v", result.Content)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(toolResultText(t, result)), &out); err != nil {
		t.Fatalf("parsing result: %v", err)
	}
	if out["connected"] != false {
		t.Errorf("expected connected=false, got %v", out["connected"])
	}
	if out["suggested_next_step"] == "" {
		t.Error("expected non-empty suggested_next_step")
	}
}

func TestWendyStatus_Connected_Direct(t *testing.T) {
	conn, _ := startFakeAgentServer(t, &fakeAgentServer{})
	srv := New(&config.Config{}, nil)
	srv.SetConn(conn)
	srv.SetConnType("direct")

	result, err := srv.handleWendyStatus(context.Background(), callToolReq("wendy_status", nil))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(toolResultText(t, result)), &out); err != nil {
		t.Fatalf("parsing result: %v", err)
	}
	if out["connected"] != true {
		t.Errorf("expected connected=true, got %v", out["connected"])
	}
	if out["connection_type"] != "direct" {
		t.Errorf("expected connection_type=direct, got %v", out["connection_type"])
	}
}

func TestWendyStatus_PluginBlock(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	statusPlugin := func(t *testing.T) (map[string]any, bool) {
		t.Helper()
		srv := New(&config.Config{}, nil)
		result, err := srv.handleWendyStatus(context.Background(), callToolReq("wendy_status", nil))
		if err != nil {
			t.Fatal(err)
		}
		var out map[string]any
		if err := json.Unmarshal([]byte(toolResultText(t, result)), &out); err != nil {
			t.Fatal(err)
		}
		p, ok := out["plugin"].(map[string]any)
		return p, ok
	}

	t.Run("not started by the plugin", func(t *testing.T) {
		t.Setenv("WENDY_PLUGIN", "")
		if p, ok := statusPlugin(t); ok {
			t.Errorf("plugin block without WENDY_PLUGIN: %v", p)
		}
	})
	t.Run("plugin, no duplicate", func(t *testing.T) {
		t.Setenv("WENDY_PLUGIN", "claude")
		p, ok := statusPlugin(t)
		if !ok || p["client"] != "claude" {
			t.Fatalf("plugin = %v, want client claude", p)
		}
		if _, dup := p["duplicate_server"]; dup {
			t.Errorf("duplicate_server reported with no user-level server: %v", p)
		}
	})
	t.Run("plugin with a user-level wendy server", func(t *testing.T) {
		t.Setenv("WENDY_PLUGIN", "claude")
		path := filepath.Join(home, ".claude.json")
		if err := os.WriteFile(path, []byte(`{"mcpServers":{"wendy":{"command":"/usr/local/bin/wendy","args":["mcp","serve"]}}}`), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.Remove(path) })
		p, _ := statusPlugin(t)
		if p["duplicate_server"] != path {
			t.Errorf("duplicate_server = %v, want %s", p["duplicate_server"], path)
		}
		if fix, _ := p["fix"].(string); !strings.Contains(fix, "claude mcp remove wendy -s user") {
			t.Errorf("fix = %q, want it to name `claude mcp remove wendy -s user` (setup keeps the entry without the plugin)", fix)
		}
	})
	t.Run("codex: user-level server shadows the plugin's", func(t *testing.T) {
		t.Setenv("WENDY_PLUGIN", "codex")
		path := filepath.Join(home, ".codex", "config.toml")
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		src := "[mcp_servers.wendy]\ncommand = \"/usr/local/bin/wendy\"\nargs = [\"mcp\", \"serve\"]\n\n[plugins.\"wendy@wendy-agentic-coding\"]\nenabled = true\n"
		if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.Remove(path) })
		p, _ := statusPlugin(t)
		if p["duplicate_server"] != path {
			t.Errorf("duplicate_server = %v, want %s", p["duplicate_server"], path)
		}
		fix, _ := p["fix"].(string)
		for _, want := range []string{"codex mcp remove wendy", "keeps it"} {
			if !strings.Contains(fix, want) {
				t.Errorf("fix = %q, want it to contain %q", fix, want)
			}
		}
		for _, unwanted := range []string{"listed twice", "(it removes"} {
			if strings.Contains(fix, unwanted) {
				t.Errorf("fix = %q, must not contain %q", fix, unwanted)
			}
		}
	})
	t.Run("plugin registered: setup removes the user-level server", func(t *testing.T) {
		t.Setenv("WENDY_PLUGIN", "claude")
		const key = "wendy@wendy-agentic-coding"
		installPath := filepath.Join(home, ".claude", "plugins", "cache", "fixture", key)
		write := func(path, content string) {
			t.Helper()
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { os.Remove(path) })
		}
		write(filepath.Join(installPath, ".mcp.json"), `{"mcpServers":{"wendy":{"command":"x","args":["mcp","serve"],"env":{"WENDY_PLUGIN":"claude"}}}}`)
		installed, _ := json.Marshal(map[string]any{
			"version": 2,
			"plugins": map[string]any{key: []map[string]any{{"scope": "user", "installPath": installPath}}},
		})
		write(filepath.Join(home, ".claude", "plugins", "installed_plugins.json"), string(installed))
		write(filepath.Join(home, ".claude", "settings.json"), `{"enabledPlugins":{"`+key+`":true}}`)
		write(filepath.Join(home, ".claude.json"), `{"mcpServers":{"wendy":{"command":"/usr/local/bin/wendy","args":["mcp","serve"]}}}`)
		p, _ := statusPlugin(t)
		if fix, _ := p["fix"].(string); !strings.Contains(fix, "mcp setup' (it removes") {
			t.Errorf("fix = %q, want the `mcp setup` text", fix)
		}
	})
	t.Run("connected sessions report it too", func(t *testing.T) {
		t.Setenv("WENDY_PLUGIN", "codex")
		conn, _ := startFakeAgentServer(t, &fakeAgentServer{})
		srv := New(&config.Config{}, nil)
		srv.SetConn(conn)
		result, err := srv.handleWendyStatus(context.Background(), callToolReq("wendy_status", nil))
		if err != nil {
			t.Fatal(err)
		}
		var out map[string]any
		if err := json.Unmarshal([]byte(toolResultText(t, result)), &out); err != nil {
			t.Fatal(err)
		}
		if p, _ := out["plugin"].(map[string]any); p["client"] != "codex" {
			t.Errorf("connected status plugin = %v, want client codex", out["plugin"])
		}
	})
}
