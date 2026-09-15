package commands

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
)

// Cross-repository acceptance: set WENDY_TEMPLATES_CHECKOUT to the matching
// templates checkout to exercise the CLI renderer against the delivered files.
func TestMacLLMTemplateScaffold(t *testing.T) {
	root := os.Getenv("WENDY_TEMPLATES_CHECKOUT")
	if root == "" {
		t.Skip("set WENDY_TEMPLATES_CHECKOUT for the Mac template acceptance fixture")
	}
	source := filepath.Join(root, "mojo", "mac-llm")
	entries, err := os.ReadDir(source)
	if err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{}
	for _, entry := range entries {
		if entry.IsDir() || entry.Name() == "template.json" {
			continue
		}
		files[entry.Name()], err = os.ReadFile(filepath.Join(source, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
	}
	destination := t.TempDir()
	if err := renderAndWriteTemplate(files, destination, "sh.wendy.NativeChat", "mac-llm", map[string]interface{}{"APP_ID": "sh.wendy.NativeChat", "PORT": 8080, "MAX_MODEL": "HuggingFaceTB/SmolLM2-135M-Instruct"}); err != nil {
		t.Fatal(err)
	}
	cfg, err := appconfig.LoadFromFile(filepath.Join(destination, "wendy.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if cfg.Run.Command != "/usr/bin/python3" || cfg.Run.Cwd != "." || cfg.Platform != "darwin" || cfg.Readiness.TimeoutSeconds != 600 {
		t.Fatalf("incorrect native configuration: %+v", cfg)
	}
	launcher, err := os.ReadFile(filepath.Join(destination, "launcher.py"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(launcher), "{{.") {
		t.Fatal("launcher retains template tokens")
	}
	if _, err := assembleNativeCommandSyncEntries(destination, cfg); err != nil {
		t.Fatal(err)
	}
}
