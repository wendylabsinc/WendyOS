package espidftoolchain

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const testLiteVersion = "6e2ae72d0b0433a2e65744b6f0821f09b4770541"

func TestPrepareWendyCoreVersion(t *testing.T) {
	const head = "# Dependencies of the app\ndependencies:\n  idf:\n    version: \">=6.0.3\"\n"
	const dep = "  wendy_core:\n    git: \"https://github.com/wendylabsinc/wendy-lite.git\"\n    path: \"components/wendy_core\"\n"
	for _, tc := range []struct {
		name, manifest string
		want           string // manifest after Apply, "" for no setup
		wantCurrent    string
		wantUnchecked  string
		wantErr        string
	}{
		{name: "no manifest"},
		{name: "no wendy_core", manifest: head},
		{name: "same version", manifest: head + dep + "    version: \"" + testLiteVersion + "\"\n"},
		{name: "same version as short SHA", manifest: head + dep + "    version: 6E2AE72\n"},
		{
			name:     "no version",
			manifest: head + dep + "  espressif/led_strip: ^3.0.0\n",
			want:     head + "  wendy_core:\n    git: \"https://github.com/wendylabsinc/wendy-lite.git\"\n    version: \"" + testLiteVersion + "\"\n    path: \"components/wendy_core\"\n  espressif/led_strip: ^3.0.0\n",
		},
		{
			name:        "other version, comment kept",
			manifest:    head + dep + "    version: main # the branch\n",
			want:        head + dep + "    version: \"" + testLiteVersion + "\" # the branch\n",
			wantCurrent: "main",
		},
		{
			name:        "quoted version",
			manifest:    head + dep + "    version: 'v1.0'\n",
			want:        head + dep + "    version: \"" + testLiteVersion + "\"\n",
			wantCurrent: "v1.0",
		},
		{
			name:     "empty version",
			manifest: head + dep + "    version:\n",
			want:     head + dep + "    version: \"" + testLiteVersion + "\"\n",
		},
		{
			name:        "namespaced name, SSH URL, CRLF",
			manifest:    strings.ReplaceAll(head+"  wendylabsinc/wendy_core:\n    git: git@github.com:wendylabsinc/wendy-lite.git\n    version: abc\n", "\n", "\r\n"),
			want:        strings.ReplaceAll(head+"  wendylabsinc/wendy_core:\n    git: git@github.com:wendylabsinc/wendy-lite.git\n    version: \""+testLiteVersion+"\"\n", "\n", "\r\n"),
			wantCurrent: "abc",
		},
		{name: "local path", manifest: head + "  wendy_core:\n    path: ../../wendy-lite/components/wendy_core\n", wantUnchecked: "the local path ../../wendy-lite/components/wendy_core"},
		{name: "override_path", manifest: head + dep + "    override_path: ../../core\n", wantUnchecked: "override_path ../../core"},
		{name: "other repository", manifest: head + "  wendy_core:\n    git: https://github.com/me/wendy-lite-fork.git\n", wantUnchecked: "https://github.com/me/wendy-lite-fork.git"},
		{name: "registry", manifest: head + "  wendylabsinc/wendy_core: \"^1.0\"\n", wantUnchecked: "component registry"},
		{name: "flow style", manifest: head + "  wendy_core: {git: \"https://github.com/wendylabsinc/wendy-lite\", path: components/wendy_core}\n", wantErr: "by hand"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if tc.manifest != "" {
				writeCoreFixture(t, dir, map[string]string{"main/idf_component.yml": tc.manifest})
			}
			setup, unchecked, err := PrepareWendyCoreVersion(dir, testLiteVersion)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(unchecked, tc.wantUnchecked) || (tc.wantUnchecked == "") != (unchecked == "") {
				t.Errorf("unchecked = %q, want %q", unchecked, tc.wantUnchecked)
			}
			if tc.want == "" {
				if setup != nil {
					t.Fatalf("setup for %q, want none", setup.Current)
				}
				return
			}
			if setup == nil {
				t.Fatal("no setup")
			}
			if setup.Current != tc.wantCurrent || setup.Version != testLiteVersion {
				t.Errorf("Current, Version = %q, %q", setup.Current, setup.Version)
			}
			if err := setup.Apply(); err != nil {
				t.Fatal(err)
			}
			data, _ := os.ReadFile(filepath.Join(dir, "main/idf_component.yml"))
			if string(data) != tc.want {
				t.Errorf("manifest =\n%q\nwant\n%q", data, tc.want)
			}
			if again, _, err := PrepareWendyCoreVersion(dir, testLiteVersion); err != nil || again != nil {
				t.Errorf("after Apply: %v, %v; want no setup", again, err)
			}
		})
	}

	// No device version: nothing to check.
	dir := t.TempDir()
	writeCoreFixture(t, dir, map[string]string{"main/idf_component.yml": head + dep})
	if setup, unchecked, err := PrepareWendyCoreVersion(dir, ""); setup != nil || unchecked != "" || err != nil {
		t.Errorf("empty version: %v, %q, %v; want nothing", setup, unchecked, err)
	}
}

func TestWendyCoreVersionApplyRefusesChangedManifest(t *testing.T) {
	dir := t.TempDir()
	manifest := "dependencies:\n  wendy_core:\n    git: https://github.com/wendylabsinc/wendy-lite.git\n"
	writeCoreFixture(t, dir, map[string]string{"main/idf_component.yml": manifest})
	setup, _, err := PrepareWendyCoreVersion(dir, testLiteVersion)
	if err != nil || setup == nil {
		t.Fatalf("setup = %v, %v", setup, err)
	}
	writeCoreFixture(t, dir, map[string]string{"main/idf_component.yml": manifest + "# edited\n"})
	if err := setup.Apply(); err == nil || !strings.Contains(err.Error(), "changed during setup") {
		t.Fatalf("Apply = %v, want a changed-file error", err)
	}
}

func TestSameVersion(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want bool
	}{
		{testLiteVersion, testLiteVersion, true},
		{"6e2ae72", testLiteVersion, true},
		{testLiteVersion, "6E2AE72D", true},
		{"6e2ae7", testLiteVersion, false}, // too short
		{"6e2ae73", testLiteVersion, false},
		{"", testLiteVersion, false},
		{"v1.2.3", "v1.2.3", true},
		{"v1.2", "v1.2.3", false},
		{"", "", true},
	} {
		if got := SameVersion(tc.a, tc.b); got != tc.want {
			t.Errorf("SameVersion(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
}

func TestIsWendyLiteGitURL(t *testing.T) {
	for url, want := range map[string]bool{
		"https://github.com/wendylabsinc/wendy-lite.git":     true,
		"https://github.com/wendylabsinc/wendy-lite":         true,
		"https://github.com/WendyLabsInc/Wendy-Lite/":        true,
		"git@github.com:wendylabsinc/wendy-lite.git":         true,
		"ssh://git@github.com/wendylabsinc/wendy-lite.git":   true,
		"https://github.com/wendylabsinc/wendy-lite-fork":    false,
		"https://notgithub.com/wendylabsinc/wendy-lite.git":  false,
		"https://gitlab.com/wendylabsinc/wendy-lite.git":     false,
		"https://github.com/someone/wendylabsinc/wendy-lite": false,
	} {
		if got := isWendyLiteGitURL(url); got != want {
			t.Errorf("isWendyLiteGitURL(%q) = %v, want %v", url, got, want)
		}
	}
}

// The added dependency is pinned to the device's Wendy Lite version.
func TestApplyWendyCorePinsVersion(t *testing.T) {
	dir := t.TempDir()
	writeCoreFixture(t, dir, map[string]string{"main/main.c": "void app_main(void) {}\n"})
	setup, err := PrepareWendyCore(dir, nil, testLiteVersion)
	if err != nil {
		t.Fatal(err)
	}
	calls := stubExecCommandContext(t, func(ctx context.Context, name string, args ...string) *exec.Cmd {
		return exec.CommandContext(ctx, "true")
	})
	if err := setup.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := [][]string{
		{"eim", "run", "idf.py add-dependency --component main --git " + WendyLiteGitURL + " --git-path components/wendy_core --git-ref " + testLiteVersion + " wendy_core", DefaultVersion},
		{"eim", "run", "idf.py reconfigure", DefaultVersion},
	}
	if !reflect.DeepEqual(*calls, want) {
		t.Fatalf("commands = %v, want %v", *calls, want)
	}
}
