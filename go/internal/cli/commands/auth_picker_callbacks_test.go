//go:build darwin || linux || windows

package commands

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
)

// Exercise the production persistence callbacks without opening a terminal.
// A successful toast must mean the selected context reached disk; a failed
// load must return an error and leave the user's original bytes intact.
func TestAuthPickerCallbacksPersistOrReportFailure(t *testing.T) {
	t.Setenv("WENDY_CONFIG_DIR", t.TempDir())
	t.Setenv("WENDY_SECRET_STORE", "file")
	cfg := sharedEndpointConfig()
	cfg.Auth[0].Name, cfg.Auth[1].Name = "first", "second"
	cfg.CurrentContext = "first"
	if err := config.Save(cfg); err != nil {
		t.Fatal(err)
	}
	picker := newAuthSessionPicker(cfg)
	item := tui.PickerItem{Name: "Second organization", Value: authSessionKey(&cfg.Auth[1])}
	dir, err := config.ConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config.json")
	read := func() config.Config {
		t.Helper()
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var saved config.Config
		if err := json.Unmarshal(data, &saved); err != nil {
			t.Fatal(err)
		}
		return saved
	}
	if message, err := picker.OnSetDefault(item); err != nil || message != "Switched to Second organization." {
		t.Fatalf("set callback: message=%q error=%v", message, err)
	}
	if saved := read(); saved.CurrentContext != "second" || len(saved.Auth) != 2 {
		t.Fatalf("wrong saved selection: %+v", saved)
	}
	if message, err := picker.OnUnsetDefault(); err != nil || message != "Current context cleared." {
		t.Fatalf("unset callback: message=%q error=%v", message, err)
	}
	if saved := read(); saved.CurrentContext != "" || len(saved.Auth) != 2 {
		t.Fatalf("clear lost sessions or retained context: %+v", saved)
	}
	for _, invalid := range []any{nil, 123, "", "removed-session"} {
		if message, err := picker.OnSetDefault(tui.PickerItem{Value: invalid}); err == nil || message != "" {
			t.Fatalf("invalid selection %v reported success: message=%q error=%v", invalid, message, err)
		}
	}
	const broken = "{not valid JSON: preserve my file"
	if err := os.WriteFile(path, []byte(broken), 0600); err != nil {
		t.Fatal(err)
	}
	for name, call := range map[string]func() (string, error){
		"switch": func() (string, error) { return picker.OnSetDefault(item) },
		"clear":  picker.OnUnsetDefault,
	} {
		message, err := call()
		if err == nil || message != "" || !strings.Contains(err.Error(), "could not "+name+" context") {
			t.Errorf("%s callback concealed config failure: message=%q error=%v", name, message, err)
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil || string(data) != broken {
			t.Fatalf("%s callback altered rejected config: %q %v", name, data, readErr)
		}
	}
}
