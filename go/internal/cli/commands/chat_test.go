package commands

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestChatVisibleWithLocalModelHelp(t *testing.T) {
	root := NewRootCmd()
	cmd, _, err := root.Find([]string{"chat"})
	if err != nil || cmd.Name() != "chat" || cmd.Hidden || cmd.GroupID != "develop" {
		t.Fatalf("chat must be a visible development command: %v, %v", cmd, err)
	}
	buf := new(bytes.Buffer)
	root.SetOut(buf)
	root.SetArgs([]string{"chat", "--help"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Just run wendy chat", "--setup", "--help-all", "--directory", "--device", "--voice", "/voice", "--prompt"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("chat help missing %q", want)
		}
	}
	for _, advanced := range []string{"--provider", "--base-url", "--model", "WENDY_CHAT_API_KEY"} {
		if strings.Contains(buf.String(), advanced) {
			t.Errorf("ordinary help overwhelms setup with %q", advanced)
		}
	}
	// --json is inherited and only meaningful with --prompt; help may name it
	// there, but must not list it as a flag of its own.
	if strings.Contains(buf.String(), "--json   ") {
		t.Errorf("ordinary help lists --json as a chat flag: %s", buf.String())
	}
	if len(strings.Split(buf.String(), "\n")) > 36 {
		t.Errorf("ordinary help should fit on one screen: %s", buf.String())
	}
	if err := UnknownSubcommandError([]string{"chat", "inspect", "my", "devices"}); err != nil {
		t.Fatalf("initial prompt rejected: %v", err)
	}
}

func TestChatAdvancedHelpKeepsConnectionFlags(t *testing.T) {
	for _, args := range [][]string{{"--help-all"}, {"--help-all", "--help"}} {
		cmd := newChatCmd()
		buf := new(bytes.Buffer)
		cmd.SetOut(buf)
		cmd.SetArgs(args)
		if err := cmd.Execute(); err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{"--provider", "--base-url", "--model", "--max-tokens", "--yes"} {
			if !strings.Contains(buf.String(), want) {
				t.Errorf("advanced help missing %q", want)
			}
		}
	}
}

func TestChatRequiresTerminalBeforeStartingTools(t *testing.T) {
	orig := isInteractiveTerminalFn
	origJSON := jsonOutput
	t.Cleanup(func() { isInteractiveTerminalFn = orig; jsonOutput = origJSON })
	isInteractiveTerminalFn = func() bool { return false }
	cmd := newChatCmd()
	cmd.SetArgs([]string{"-C", t.TempDir()})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "interactive terminal") {
		t.Fatalf("expected actionable terminal error, got %v", err)
	}
}

func TestChatRejectsJSON(t *testing.T) {
	orig := isInteractiveTerminalFn
	origJSON := jsonOutput
	t.Cleanup(func() { isInteractiveTerminalFn = orig; jsonOutput = origJSON })
	isInteractiveTerminalFn = func() bool { return true }
	jsonOutput = true
	cmd := newChatCmd()
	cmd.SetArgs([]string{"--provider", "ollama", "--model", "test-model", "-C", t.TempDir()})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "remove --json") {
		t.Fatalf("expected actionable JSON error, got %v", err)
	}
}

func TestChatWorkspace(t *testing.T) {
	dir := t.TempDir()
	want, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	got, err := chatWorkspace(dir)
	if err != nil || got != want {
		t.Fatalf("chatWorkspace = %q, %v; want %q", got, err, want)
	}
	file := filepath.Join(dir, "file.txt")
	if err := os.WriteFile(file, []byte("data"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{file, filepath.Join(dir, "missing")} {
		if _, err := chatWorkspace(path); err == nil {
			t.Errorf("accepted invalid directory %q", path)
		}
	}
}

func TestChatPromptDoesNotNeedATerminalAndAllowsJSON(t *testing.T) {
	orig := isInteractiveTerminalFn
	origJSON := jsonOutput
	t.Cleanup(func() { isInteractiveTerminalFn = orig; jsonOutput = origJSON })
	isInteractiveTerminalFn = func() bool { return false }
	jsonOutput = true
	// An unset HOME keeps the developer's saved chat setup out of the test.
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	for _, env := range []string{"WENDY_CHAT_PROVIDER", "WENDY_CHAT_MODEL", "WENDY_CHAT_BASE_URL", "WENDY_CHAT_API_KEY"} {
		t.Setenv(env, "")
	}
	cmd := newChatCmd()
	cmd.SetArgs([]string{"--prompt", "hello", "-C", t.TempDir()})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("headless chat with no saved setup must explain what to do")
	}
	for _, unwanted := range []string{"interactive terminal", "remove --json"} {
		if strings.Contains(err.Error(), unwanted) {
			t.Fatalf("--prompt must bypass the terminal and --json checks, got %v", err)
		}
	}
	if !strings.Contains(err.Error(), "--setup") {
		t.Fatalf("expected setup guidance, got %v", err)
	}
}

func TestChatPromptRefusesInteractiveOnlyFlags(t *testing.T) {
	for _, args := range [][]string{{"--prompt", "x", "--setup"}, {"--prompt", "x", "--voice"}} {
		cmd := newChatCmd()
		cmd.SetArgs(append(args, "-C", t.TempDir()))
		err := cmd.Execute()
		if err == nil || !strings.Contains(err.Error(), "--prompt") {
			t.Fatalf("%v: expected an error naming --prompt, got %v", args, err)
		}
	}
}
