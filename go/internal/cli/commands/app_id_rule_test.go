package commands

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
)

// One app id rule: init writes it, create/publish/promote refuse anything
// else with the id, the rule and the fix, and every bundled example follows it.
func TestAppIDRule(t *testing.T) {
	examples, err := filepath.Glob("../../../../Examples/*/wendy.json")
	if err != nil || len(examples) == 0 {
		t.Fatalf("no bundled examples found: %v", err)
	}
	for _, p := range examples {
		cfg, err := appconfig.LoadFromFile(p)
		if err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		if err := checkAppID(cfg.AppID); err != nil {
			t.Errorf("%s: %v", p, err)
		}
	}

	_, _, err = appDefinition([]byte(`{"appId":"HelloAudio"}`), "")
	for _, want := range []string{`"HelloAudio"`, "lowercase letters, digits, dots and hyphens", `"helloaudio"`, "Rename appId in wendy.json"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("publish error %v, want it to say %s", err, want)
		}
	}
	if err := validateNewAppID("my_app"); err == nil {
		t.Error("app create accepted my_app")
	}

	if got := appIDFromName("My_Project"); got != "my-project" {
		t.Errorf("appIDFromName(My_Project) = %q", got)
	}
	if id, err := resolveInitAppID(filepath.Join(os.TempDir(), "My_Project"), nil, initOptions{}); err != nil || id != "my-project" {
		t.Errorf("init from directory My_Project = %q, %v", id, err)
	}
	if _, err := resolveInitAppID("", []string{"MyApp"}, initOptions{}); err == nil {
		t.Error("init accepted app id MyApp")
	}
}
