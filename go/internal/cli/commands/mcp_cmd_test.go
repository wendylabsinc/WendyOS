package commands

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	toml "github.com/BurntSushi/toml"
	"github.com/wendylabsinc/wendy/go/internal/cli/pluginmode"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/internal/shared/version"
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
	if err := addMCPToTOMLConfig(path, "mcp_servers", "wendy", "wendy", []string{"mcp", "serve"}); err != nil {
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
	if err := addMCPToTOMLConfig(path, "mcp_servers", "wendy", "wendy", []string{"mcp", "serve"}); err != nil {
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

// Setup owns only type and command of the wendy entry, plus args unless they
// already start with "mcp serve"; anything else the user added to the entry
// (env, timeouts, a pinned --device) must survive setup and the upgrade
// refresh.
func TestAddMCPToJSONConfig_KeepsUserKeysInWendyEntry(t *testing.T) {
	tests := []struct {
		name, in, want string
	}{
		{
			name: "user keys kept, stale args replaced",
			in: `{"numStartups": 3, "mcpServers": {
  "github": {"command": "npx"},
  "wendy": {"type": "stdio", "command": "/old/wendy", "args": ["serve", "--old"], "env": {"HTTPS_PROXY": "http://proxy.local:3128"}}
}}`,
			want: `{"numStartups": 3, "mcpServers": {
  "github": {"command": "npx"},
  "wendy": {"type": "stdio", "command": "/new/wendy", "args": ["mcp", "serve"], "env": {"HTTPS_PROXY": "http://proxy.local:3128"}}
}}`,
		},
		{
			name: "pinned --device kept",
			in: `{"mcpServers": {
  "wendy": {"type": "stdio", "command": "/old/wendy", "args": ["mcp", "serve", "--device", "my-pi.local"]}
}}`,
			want: `{"mcpServers": {
  "wendy": {"type": "stdio", "command": "/new/wendy", "args": ["mcp", "serve", "--device", "my-pi.local"]}
}}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "mcp.json")
			if err := os.WriteFile(path, []byte(tt.in), 0o644); err != nil {
				t.Fatal(err)
			}
			entry := map[string]any{"type": "stdio", "command": "/new/wendy", "args": []string{"mcp", "serve"}}
			if err := addMCPToJSONConfig(path, "mcpServers", "wendy", entry); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var got, want map[string]any
			if err := json.Unmarshal(data, &got); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal([]byte(tt.want), &want); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("got:\n%s", data)
			}
		})
	}
}

// A wendy entry that is not an object (a hand-edit gone wrong) is replaced by
// a working one instead of failing setup or keeping the broken value.
func TestAddMCPToJSONConfig_NonObjectWendyEntry(t *testing.T) {
	for name, wendy := range map[string]string{
		"string": `"broken"`,
		"array":  `["mcp", "serve"]`,
		"null":   `null`,
		"number": `42`,
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "mcp.json")
			in := `{"mcpServers": {"github": {"command": "npx"}, "wendy": ` + wendy + `}}`
			if err := os.WriteFile(path, []byte(in), 0o644); err != nil {
				t.Fatal(err)
			}
			entry := map[string]any{"type": "stdio", "command": "/new/wendy", "args": []string{"mcp", "serve"}}
			if err := addMCPToJSONConfig(path, "mcpServers", "wendy", entry); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var got, want map[string]any
			if err := json.Unmarshal(data, &got); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal([]byte(`{"mcpServers": {"github": {"command": "npx"}, "wendy": {"type": "stdio", "command": "/new/wendy", "args": ["mcp", "serve"]}}}`), &want); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("got:\n%s", data)
			}
		})
	}
}

// End to end: the silent refresh after a CLI upgrade updates the command in
// every configured client but keeps a device the user pinned in args.
func TestMaybeRefreshMCPSetup_KeepsPinnedDevice(t *testing.T) {
	home := setupMCPRefreshTest(t)
	claudePath := filepath.Join(home, ".claude.json")
	if err := os.WriteFile(claudePath, []byte(`{"mcpServers": {"wendy": {"type": "stdio", "command": "/old/wendy", "args": ["mcp", "serve", "--device", "my-pi.local"]}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	codexPath := filepath.Join(home, ".codex", "config.toml")
	if err := os.WriteFile(codexPath, []byte("[mcp_servers.wendy]\ncommand = \"/old/wendy\"\nargs = [\"mcp\", \"serve\", \"--device\", \"my-pi.local\"]\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{LastMCPSetupVersion: "9.9.8"}
	maybeRefreshMCPSetup(cfg)
	if cfg.LastMCPSetupVersion != "9.9.9" {
		t.Fatalf("refresh did not run: LastMCPSetupVersion = %q", cfg.LastMCPSetupVersion)
	}

	wantArgs := []any{"mcp", "serve", "--device", "my-pi.local"}
	var claude map[string]any
	data, err := os.ReadFile(claudePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &claude); err != nil {
		t.Fatal(err)
	}
	entry := claude["mcpServers"].(map[string]any)["wendy"].(map[string]any)
	if entry["command"] != wendyBinaryPath() || !reflect.DeepEqual(entry["args"], wantArgs) {
		t.Errorf("Claude Code entry after refresh = %v", entry)
	}
	var codex map[string]any
	if _, err := toml.DecodeFile(codexPath, &codex); err != nil {
		t.Fatal(err)
	}
	entry = codex["mcp_servers"].(map[string]any)["wendy"].(map[string]any)
	if entry["command"] != wendyBinaryPath() || !reflect.DeepEqual(entry["args"], wantArgs) {
		t.Errorf("Codex entry after refresh = %v", entry)
	}
}

// Skill installers can leave root-owned files behind under sudo, so
// `sudo wendy mcp setup` says once to run it as the user instead.
func TestMCPSetupCmd_WarnsUnderSudo(t *testing.T) {
	for _, sudoUID := range []string{"501", ""} {
		setupMCPRefreshTest(t)
		t.Setenv("SUDO_UID", sudoUID)
		cmd := newMCPSetupCmd()
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetErr(&out)
		cmd.SetArgs(nil)
		if err := cmd.Execute(); err != nil {
			t.Fatal(err)
		}
		n := strings.Count(out.String(), "without sudo")
		if want := map[bool]int{true: 1, false: 0}[sudoUID != ""]; n != want {
			t.Errorf("SUDO_UID=%q: warning printed %d times, want %d:\n%s", sudoUID, n, want, out.String())
		}
	}
}

// Tests that run the real MCP writers must never reach the developer's own AI
// tool configs, on any OS.
func TestSetupMCPRefreshTest_IsolatesConfigRoots(t *testing.T) {
	home := setupMCPRefreshTest(t)
	for _, env := range []string{"HOME", "USERPROFILE", "APPDATA", "LOCALAPPDATA", "XDG_CONFIG_HOME", "WENDY_CONFIG_DIR"} {
		if v := os.Getenv(env); !strings.HasPrefix(v, home) {
			t.Errorf("%s = %q, want a path under the test HOME %s", env, v, home)
		}
	}
	for name, p := range map[string]string{
		"Claude Code":    claudeCodeConfigPath(),
		"Claude Desktop": claudeDesktopConfigPath(),
		"Cursor":         cursorConfigPath(),
		"Windsurf":       windsurfConfigPath(),
		"Codex":          codexConfigPath(),
	} {
		if p != "" && !strings.HasPrefix(p, home) {
			t.Errorf("%s config path %q is outside the test HOME %s", name, p, home)
		}
	}
}

// isolateAIToolHome points HOME and every per-OS config root the MCP setup
// writers can read (USERPROFILE and APPDATA on Windows, XDG_CONFIG_HOME) at a
// temporary directory, PATH at an empty one (no claude/cursor/windsurf/codex
// binaries) and the CLI config directory inside it, so a test running the
// real writers never touches the developer's own AI tool configs. It returns
// the temporary HOME.
func isolateAIToolHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("APPDATA", filepath.Join(home, "AppData", "Roaming"))
	t.Setenv("LOCALAPPDATA", filepath.Join(home, "AppData", "Local"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("PATH", t.TempDir())
	t.Setenv("WENDY_CONFIG_DIR", filepath.Join(home, ".wendy"))
	return home
}

// setupMCPRefreshTest isolates HOME, PATH and the config roots, makes
// ~/.codex exist so Codex counts as installed, and pretends the running CLI is
// release 9.9.9 so maybeRefreshMCPSetup has something to refresh. It returns
// the temporary HOME.
func setupMCPRefreshTest(t *testing.T) string {
	t.Helper()
	home := isolateAIToolHome(t)
	if err := os.MkdirAll(filepath.Join(home, ".codex"), 0o755); err != nil {
		t.Fatal(err)
	}
	oldVersion := version.Version
	version.Version = "9.9.9"
	t.Cleanup(func() { version.Version = oldVersion })
	// Run as HOME's owner, not under sudo, whoever runs the tests (CI may
	// run them as root).
	t.Setenv("SUDO_UID", "")
	owner := -1
	if fi, err := os.Stat(home); err == nil {
		if uid, _, ok := fileOwner(fi); ok {
			owner = uid
		}
	}
	oldEUID := mcpRefreshEUID
	mcpRefreshEUID = func() int { return owner }
	t.Cleanup(func() { mcpRefreshEUID = oldEUID })
	return home
}

func TestMCPRefreshAsForeignUser(t *testing.T) {
	tests := []struct {
		name       string
		sudoUID    string
		euid       int
		homeOwner  int
		ownerKnown bool
		want       bool
	}{
		{"user in own HOME", "", 501, 501, true, false},
		{"root in own HOME (container, root-only box)", "", 0, 0, true, false},
		{"sudo keeping the user's HOME (macOS)", "501", 0, 501, true, true},
		{"sudo with HOME reset to root's", "501", 0, 0, true, true},
		{"su keeping another user's HOME", "", 0, 501, true, true},
		{"no POSIX owners (Windows)", "", -1, 0, false, false},
	}
	for _, tt := range tests {
		if got := mcpRefreshAsForeignUser(tt.sudoUID, tt.euid, tt.homeOwner, tt.ownerKnown); got != tt.want {
			t.Errorf("%s: mcpRefreshAsForeignUser = %v, want %v", tt.name, got, tt.want)
		}
	}
}

// `sudo wendy …` right after an upgrade keeps $HOME on macOS: a refresh then
// would leave root-owned AI tool configs the user's tools cannot read. The
// refresh waits for the next run as the user instead.
func TestMaybeRefreshMCPSetup_SkipsUnderSudo(t *testing.T) {
	home := setupMCPRefreshTest(t)
	t.Setenv("SUDO_UID", "501")
	assertMCPRefreshSkipped(t, home)
}

func TestMaybeRefreshMCPSetup_SkipsWhenHomeHasAnotherOwner(t *testing.T) {
	home := setupMCPRefreshTest(t)
	fi, err := os.Stat(home)
	if err != nil {
		t.Fatal(err)
	}
	owner, _, ok := fileOwner(fi)
	if !ok {
		t.Skip("no POSIX file owners here")
	}
	mcpRefreshEUID = func() int { return owner + 1 }
	assertMCPRefreshSkipped(t, home)
}

func assertMCPRefreshSkipped(t *testing.T, home string) {
	t.Helper()
	cfg := &config.Config{LastMCPSetupVersion: "9.9.8"}
	maybeRefreshMCPSetup(cfg)
	if cfg.LastMCPSetupVersion != "9.9.8" {
		t.Errorf("LastMCPSetupVersion = %q; the refresh must wait for a run as HOME's owner", cfg.LastMCPSetupVersion)
	}
	for _, p := range []string{filepath.Join(home, ".codex", "config.toml"), filepath.Join(home, ".wendy")} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("skipped refresh wrote %s (err=%v)", p, err)
		}
	}
}

func TestCodexConfigPath_ReturnsDirBasedPath(t *testing.T) {
	home, _ := os.UserHomeDir()
	want := filepath.Join(home, ".codex", "config.toml")
	if got := codexConfigPath(); got != "" && got != want {
		t.Fatalf("codexConfigPath() = %q, want %q or empty", got, want)
	}
}

func TestShouldRefreshMCPSetup(t *testing.T) {
	tests := []struct {
		name        string
		lastVersion string
		current     string
		want        bool
	}{
		{name: "never set up", lastVersion: "", current: "0.11.0", want: false},
		{name: "dev build never refreshes", lastVersion: "0.10.0", current: "dev", want: false},
		{name: "dev branch build never refreshes", lastVersion: "0.10.0", current: "2026.06.30-1-dev", want: false},
		{name: "same version", lastVersion: "0.11.0", current: "0.11.0", want: false},
		{name: "upgraded", lastVersion: "0.10.0", current: "0.11.0", want: true},
		{name: "downgraded", lastVersion: "0.11.0", current: "0.10.0", want: true},
		{name: "set up on dev then real build", lastVersion: "dev", current: "0.11.0", want: true},
		{name: "never set up on dev build", lastVersion: "", current: "dev", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := shouldRefreshMCPSetup(tt.lastVersion, tt.current); got != tt.want {
				t.Errorf("shouldRefreshMCPSetup(%q, %q) = %v, want %v", tt.lastVersion, tt.current, got, tt.want)
			}
		})
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

func TestMCPRestartNotice(t *testing.T) {
	notice := mcpRestartNotice([]mcpSetupResult{
		{tool: "Claude Code", path: "/h/.claude.json"},
		{tool: "Cursor", path: "/h/.cursor/mcp.json", err: errors.New("parsing")},
		{tool: "Codex", path: "/h/.codex/config.toml"},
	})
	for _, want := range []string{"Restart any of these", "Claude Code: start a new session", "`/mcp`", "Codex: start a new Codex session"} {
		if !strings.Contains(notice, want) {
			t.Errorf("notice missing %q:\n%s", want, notice)
		}
	}
	if strings.Contains(notice, "Cursor") {
		t.Errorf("a tool whose setup failed must not be listed:\n%s", notice)
	}
	if got := mcpRestartNotice(nil); got != "" {
		t.Errorf("no configured tools should print nothing, got %q", got)
	}
	if got := mcpRestartNotice([]mcpSetupResult{{tool: "Codex skills", path: "/h/.codex/wendy-skills.md"}}); got != "" {
		t.Errorf("skill installs are not MCP clients, got %q", got)
	}
}

// End to end: `wendy mcp setup` in an isolated HOME keeps a hand-written Codex
// config intact apart from the wendy table, and tells the user what to restart.
func TestMCPSetupCmd_PreservesCodexConfigAndPrintsRestartNotice(t *testing.T) {
	home := isolateAIToolHome(t)
	if err := os.MkdirAll(filepath.Join(home, ".codex"), 0o755); err != nil {
		t.Fatal(err)
	}
	original := "# my codex settings\nmodel = \"o3\"  # pinned\n\n[mcp_servers.github]\ncommand = \"npx\"\n"
	codexPath := filepath.Join(home, ".codex", "config.toml")
	if err := os.WriteFile(codexPath, []byte(original), 0o600); err != nil {
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

	got, err := os.ReadFile(codexPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(got), original+"\n[mcp_servers.wendy]\n") {
		t.Fatalf("codex config not preserved:\n%s", got)
	}
	for _, want := range []string{"✓ Codex: configured at " + codexPath, "Codex: start a new Codex session"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output missing %q:\n%s", want, out.String())
		}
	}
}

// The plugin's launcher runs `wendy mcp serve` for Claude or Codex. That
// process must not rewrite AI tool configs after an upgrade; the next command
// the user runs in a terminal refreshes instead.
func TestMaybeRefreshMCPSetup_SkippedUnderPlugin(t *testing.T) {
	home := setupMCPRefreshTest(t)
	t.Setenv(pluginmode.EnvVar, "claude")
	claudePath := filepath.Join(home, ".claude.json")
	orig := []byte(`{"mcpServers": {"wendy": {"type": "stdio", "command": "/old/wendy", "args": ["mcp", "serve"]}}}`)
	if err := os.WriteFile(claudePath, orig, 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{LastMCPSetupVersion: "9.9.8"}
	maybeRefreshMCPSetup(cfg)

	if cfg.LastMCPSetupVersion != "9.9.8" {
		t.Errorf("LastMCPSetupVersion = %q, want it untouched (9.9.8)", cfg.LastMCPSetupVersion)
	}
	got, err := os.ReadFile(claudePath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, orig) {
		t.Errorf("~/.claude.json was rewritten under WENDY_PLUGIN:\n%s", got)
	}
	if _, err := os.Stat(filepath.Join(home, ".codex", "config.toml")); !os.IsNotExist(err) {
		t.Errorf("Codex config was written under WENDY_PLUGIN (stat err = %v)", err)
	}
}
