package commands

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
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

func TestAddMCPToYAMLConfig_CreatesFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	entry := map[string]any{"type": "stdio", "command": "wendy", "args": []string{"mcp", "serve"}}
	if err := addMCPToYAMLConfig(path, "mcpServers", "wendy", entry); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading file: %v", err)
	}
	if !strings.Contains(string(data), "wendy") {
		t.Fatalf("expected 'wendy' in output, got: %s", data)
	}
}

func TestAddMCPToYAMLConfig_PreservesExisting(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	existing := "mcpServers:\n  other:\n    type: stdio\n    command: other\n"
	if err := os.WriteFile(path, []byte(existing), 0o644); err != nil {
		t.Fatal(err)
	}
	entry := map[string]any{"type": "stdio", "command": "wendy", "args": []string{"mcp", "serve"}}
	if err := addMCPToYAMLConfig(path, "mcpServers", "wendy", entry); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading file: %v", err)
	}
	if !strings.Contains(string(data), "other") {
		t.Fatalf("expected 'other' to be preserved, got: %s", data)
	}
	if !strings.Contains(string(data), "wendy") {
		t.Fatalf("expected 'wendy' entry, got: %s", data)
	}
}

func TestCodexConfigPath_ReturnsDirBasedPath(t *testing.T) {
	home, _ := os.UserHomeDir()
	want := filepath.Join(home, ".codex", "config.yaml")
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
