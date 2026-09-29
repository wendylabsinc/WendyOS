package commands

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/cli/assets"
)

func TestEndUserSkillsInstallDiscoverableDirectoriesAndReferences(t *testing.T) {
	target := t.TempDir()
	for i := 0; i < 2; i++ {
		if err := installWendySkillDirs(target); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"wendy", "wendy-device-install", "wendy-robot-deploy", "wendy-template-app", "wendy-mcp-setup"} {
		data, err := os.ReadFile(filepath.Join(target, name, "SKILL.md"))
		if err != nil {
			t.Fatal(err)
		}
		want, _ := assets.FS.ReadFile("skills/" + name + "/SKILL.md")
		if string(data) != string(want) {
			t.Fatalf("skill %s lost content", name)
		}
	}
	path := "wendy/references/wendy.json.md"
	got, err := os.ReadFile(filepath.Join(target, filepath.FromSlash(path)))
	if err != nil {
		t.Fatal(err)
	}
	want, _ := assets.FS.ReadFile("skills/" + path)
	if string(got) != string(want) {
		t.Fatal("reference content missing")
	}
	if _, err := os.Stat(filepath.Join(target, "linear")); !os.IsNotExist(err) {
		t.Fatal("installed unrelated skill")
	}
}

// Exercise the actual host installers with a clean home, including Claude's
// fresh-install case without a plugins directory. No assistant binary or
// marketplace is needed to distribute the embedded group.
func TestHostInstallersUseSameEndUserGroup(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("PATH", t.TempDir())
	if got := installSkillsForAllTools(); len(got) != 0 {
		t.Fatalf("installed for absent tools: %+v", got)
	}
	for _, dir := range []string{".codex", ".config/opencode"} {
		if err := os.MkdirAll(filepath.Join(home, dir), 0755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(home, ".claude.json"), []byte("{}"), 0644); err != nil {
		t.Fatal(err)
	}
	wantNames := []string{
		"wendy", "wendy-app-lifecycle", "wendy-device-debug", "wendy-device-install",
		"wendy-device-ops", "wendy-entitlements", "wendy-install", "wendy-mcp-setup",
		"wendy-project-setup", "wendy-robot-deploy", "wendy-template-app",
	}
	for i := 0; i < 2; i++ {
		results := installSkillsForAllTools()
		if len(results) != 3 {
			t.Fatalf("expected all three hosts, got %+v", results)
		}
		for _, result := range results {
			if result.err != nil {
				t.Fatalf("%s: %v", result.tool, result.err)
			}
			entries, err := os.ReadDir(result.path)
			if err != nil {
				t.Fatal(err)
			}
			var gotNames []string
			for _, entry := range entries {
				gotNames = append(gotNames, entry.Name())
			}
			if !slices.Equal(gotNames, wantNames) {
				t.Fatalf("%s has wrong audience: %v", result.tool, gotNames)
			}
			for _, name := range wantNames {
				err := fs.WalkDir(assets.FS, "skills/"+name, func(p string, entry fs.DirEntry, err error) error {
					if err != nil || entry.IsDir() {
						return err
					}
					want, err := assets.FS.ReadFile(p)
					if err != nil {
						return err
					}
					got, err := os.ReadFile(filepath.Join(result.path, filepath.FromSlash(strings.TrimPrefix(p, "skills/"))))
					if err != nil {
						return err
					}
					if string(got) != string(want) {
						t.Errorf("%s lost content in %s", result.tool, p)
					}
					return nil
				})
				if err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	for _, path := range []string{".claude/plugins", ".config/opencode/wendy-skills.md"} {
		if _, err := os.Stat(filepath.Join(home, path)); !os.IsNotExist(err) {
			t.Fatalf("wrote legacy distribution %s: %v", path, err)
		}
	}
}

func TestInitInstallsSameGroupAndPreservesEngineeringPlugins(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("PATH", t.TempDir())
	registry := filepath.Join(home, ".claude", "plugins", "installed_plugins.json")
	if err := os.MkdirAll(filepath.Dir(registry), 0755); err != nil {
		t.Fatal(err)
	}
	const existing = `{"version":2,"plugins":{"wendy-contributing@swift-server-skills":[{"scope":"user"}]}}`
	if err := os.WriteFile(registry, []byte(existing), 0644); err != nil {
		t.Fatal(err)
	}
	if err := installWendySkills(true); err != nil {
		t.Fatal(err)
	}
	previousConfirm := confirmDefaultNoFn
	t.Cleanup(func() { confirmDefaultNoFn = previousConfirm })
	confirmDefaultNoFn = func(string) bool {
		t.Error("asked to install an already-current end-user group")
		return false
	}
	if err := installWendySkills(false); err != nil {
		t.Fatal(err)
	}
	names, err := wendyEndUserSkillNames()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range names {
		if _, err := os.Stat(filepath.Join(home, ".claude", "skills", name, "SKILL.md")); err != nil {
			t.Fatal(err)
		}
	}
	got, err := os.ReadFile(registry)
	if err != nil || string(got) != existing {
		t.Fatalf("changed engineering plugins: %s, %v", got, err)
	}
	file := filepath.Join(home, ".claude", "skills", "wendy", "SKILL.md")
	if err := os.WriteFile(file, []byte("custom"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := installWendySkills(true); err == nil || !strings.Contains(err.Error(), "preserving") {
		t.Fatalf("init swallowed skill conflict: %v", err)
	}
}

func TestCodexSkillsPreserveExistingAndEditedContent(t *testing.T) {
	for _, installed := range []bool{false, true} {
		t.Run(fmt.Sprint(installed), func(t *testing.T) {
			target := t.TempDir()
			if installed {
				if err := installWendySkillDirs(target); err != nil {
					t.Fatal(err)
				}
			}
			file := filepath.Join(target, "wendy", "SKILL.md")
			if err := os.MkdirAll(filepath.Dir(file), 0755); err != nil {
				t.Fatal(err)
			}
			const custom = "my customized Wendy workflow"
			if err := os.WriteFile(file, []byte(custom), 0644); err != nil {
				t.Fatal(err)
			}
			if err := installWendySkillDirs(target); err == nil || !strings.Contains(err.Error(), "preserving") {
				t.Fatalf("expected preservation error: %v", err)
			}
			got, _ := os.ReadFile(file)
			if string(got) != custom {
				t.Fatal("overwrote user skill")
			}
		})
	}
}
