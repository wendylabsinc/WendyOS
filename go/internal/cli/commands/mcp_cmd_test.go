package commands

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	toml "github.com/BurntSushi/toml"
)

func TestCursorConfigPath_ReturnsDirBasedPath(t *testing.T) {
	home, _ := os.UserHomeDir()
	want := filepath.Join(home, ".cursor", "mcp.json")
	if got := cursorConfigPath(); got != "" && got != want {
		t.Fatalf("cursorConfigPath() = %q, want %q or empty", got, want)
	}
}

func TestWindsurfConfigPath_ReturnsDirBasedPath(t *testing.T) {
	home, _ := os.UserHomeDir()
	want := filepath.Join(home, ".codeium", "windsurf", "mcp_config.json")
	if got := windsurfConfigPath(); got != "" && got != want {
		t.Fatalf("windsurfConfigPath() = %q, want %q or empty", got, want)
	}
}

func TestAddMCPToTOMLConfig_CreatesFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	entry := map[string]any{"command": "wendy", "args": []string{"mcp", "serve"}}
	if err := addMCPToTOMLConfig(path, "mcp_servers", "wendy", entry); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading file: %v", err)
	}
	var out map[string]any
	if _, err := toml.Decode(string(data), &out); err != nil {
		t.Fatalf("parsing TOML: %v", err)
	}
	servers, ok := out["mcp_servers"].(map[string]any)
	if !ok {
		t.Fatalf("expected mcp_servers map, got: %T", out["mcp_servers"])
	}
	wendyEntry, ok := servers["wendy"].(map[string]any)
	if !ok {
		t.Fatalf("expected wendy entry, got: %T %v", servers["wendy"], servers["wendy"])
	}
	if wendyEntry["command"] != "wendy" {
		t.Errorf("expected command=wendy, got: %v", wendyEntry["command"])
	}
}

func TestAddMCPToTOMLConfig_PreservesExisting(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	existing := "[mcp_servers.other]\ncommand = \"other\"\n"
	if err := os.WriteFile(path, []byte(existing), 0o644); err != nil {
		t.Fatal(err)
	}
	entry := map[string]any{"command": "wendy", "args": []string{"mcp", "serve"}}
	if err := addMCPToTOMLConfig(path, "mcp_servers", "wendy", entry); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading file: %v", err)
	}
	var out map[string]any
	if _, err := toml.Decode(string(data), &out); err != nil {
		t.Fatalf("parsing TOML: %v", err)
	}
	servers, ok := out["mcp_servers"].(map[string]any)
	if !ok {
		t.Fatalf("expected mcp_servers map, got %T", out["mcp_servers"])
	}
	if _, ok := servers["other"]; !ok {
		t.Error("expected 'other' entry to be preserved")
	}
	if _, ok := servers["wendy"]; !ok {
		t.Error("expected 'wendy' entry to be present")
	}
}

func TestCodexConfigPath_ReturnsDirBasedPath(t *testing.T) {
	home, _ := os.UserHomeDir()
	want := filepath.Join(home, ".codex", "config.toml")
	if got := codexConfigPath(); got != "" && got != want {
		t.Fatalf("codexConfigPath() = %q, want %q or empty", got, want)
	}
}

func TestMCPCmd_HelpText(t *testing.T) {
	cmd := newMCPCmd()
	buf := new(bytes.Buffer)
	cmd.SetOut(buf)
	cmd.SetErr(buf)
	cmd.SetArgs([]string{"--help"})
	_ = cmd.Execute()
	out := buf.String()
	if !strings.Contains(out, "serve") {
		t.Fatalf("expected help to mention 'serve', got: %s", out)
	}
}
