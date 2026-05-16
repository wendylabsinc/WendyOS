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
