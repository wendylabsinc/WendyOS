package espidftoolchain

import (
	"context"
	"os/exec"
	"reflect"
	"strings"
	"testing"
)

// UseIDF selects the ESP-IDF version and --preview of the EIM toolchain.
func TestProjectToolchainUseIDF(t *testing.T) {
	calls := stubExecCommandContext(t, func(ctx context.Context, name string, args ...string) *exec.Cmd {
		if len(args) > 0 && args[0] == "list" {
			return exec.CommandContext(ctx, "echo", "- "+DefaultVersion+" (selected) [/x]")
		}
		return exec.CommandContext(ctx, "true")
	})
	toolchain := &ProjectToolchain{}
	if toolchain.IDFVersion() != DefaultVersion {
		t.Errorf("default IDFVersion = %q", toolchain.IDFVersion())
	}
	toolchain.UseIDF("v6.1", true)
	if err := toolchain.Ensure(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := toolchain.Run(context.Background(), t.TempDir(), "set-target", "esp32s31"); err != nil {
		t.Fatal(err)
	}
	want := [][]string{
		{"eim", "--version"},
		{"eim", "list"},
		{"eim", "install", "-i", "v6.1", "-n", "true"},
		{"eim", "run", "idf.py --preview set-target esp32s31", "v6.1"},
	}
	if !reflect.DeepEqual(*calls, want) {
		t.Errorf("calls = %v, want %v", *calls, want)
	}

	// Without --preview.
	*calls = nil
	toolchain.UseIDF("v6.0.3", false)
	if err := toolchain.Run(context.Background(), t.TempDir(), "gen_project_binary"); err != nil {
		t.Fatal(err)
	}
	if want := [][]string{{"eim", "run", "idf.py gen_project_binary", "v6.0.3"}}; !reflect.DeepEqual(*calls, want) {
		t.Errorf("calls = %v, want %v", *calls, want)
	}
}

// A container toolchain keeps the ESP-IDF of its image.
func TestProjectToolchainUseIDFIgnoredByContainer(t *testing.T) {
	dir := t.TempDir()
	image := "espressif/idf@sha256:" + strings.Repeat("d", 64)
	writeCoreFixture(t, dir, map[string]string{"wendy-idf.json": `{"image":"` + image + `","target":"esp32c6"}`})
	toolchain, err := LoadProjectToolchain(dir, "esp32c6")
	if err != nil {
		t.Fatal(err)
	}
	toolchain.UseIDF("v6.1", true)
	calls := stubExecCommandContext(t, func(ctx context.Context, name string, args ...string) *exec.Cmd {
		return exec.CommandContext(ctx, "true")
	})
	if err := toolchain.Run(context.Background(), dir, "gen_project_binary"); err != nil {
		t.Fatal(err)
	}
	call := (*calls)[0]
	if call[0] != "docker" || !reflect.DeepEqual(call[len(call)-3:], []string{image, "idf.py", "gen_project_binary"}) {
		t.Errorf("command = %v, want docker ... %s idf.py gen_project_binary", call, image)
	}
}

func TestBuildIDFVersion(t *testing.T) {
	dir := t.TempDir()
	if got := BuildIDFVersion(dir); got != "" {
		t.Errorf("not configured: %q", got)
	}
	writeCoreFixture(t, dir, map[string]string{"build/project_description.json": `{"git_revision": "v6.0.3", "target": "esp32c61"}`})
	if got := BuildIDFVersion(dir); got != "v6.0.3" {
		t.Errorf("BuildIDFVersion = %q, want v6.0.3", got)
	}
	writeCoreFixture(t, dir, map[string]string{"build/project_description.json": `{`})
	if got := BuildIDFVersion(dir); got != "" {
		t.Errorf("unreadable description: %q", got)
	}
}

func TestSameIDFVersion(t *testing.T) {
	for _, tc := range []struct {
		configured, want string
		same             bool
	}{
		{"v6.0.3", "v6.0.3", true},
		{"v6.1-dirty", "v6.1", true},
		{"v6.1.1", "v6.1", false},
		{"v6.0.3", "v6.1", false},
		{"", "v6.1", false},
	} {
		if got := SameIDFVersion(tc.configured, tc.want); got != tc.same {
			t.Errorf("SameIDFVersion(%q, %q) = %v, want %v", tc.configured, tc.want, got, tc.same)
		}
	}
}
