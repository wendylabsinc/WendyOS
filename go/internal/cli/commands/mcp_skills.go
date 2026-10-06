package commands

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/wendylabsinc/wendy/go/internal/cli/assets"
)

// installSkillsForAllTools extracts embedded skill files into each detected AI tool.
// Errors per-tool are collected and returned so the caller can report them.
func installSkillsForAllTools() []mcpSetupResult {
	var results []mcpSetupResult

	if r := installClaudeCodeSkills(); r != nil {
		results = append(results, *r)
	}
	if r := installCodexSkills(); r != nil {
		results = append(results, *r)
	}
	if r := installOpencodeSkills(); r != nil {
		results = append(results, *r)
	}

	return results
}

// Only the end-user group is distributed by setup. The embed tree also holds
// engineering and unrelated skills for other CLI features; name prefixes are
// not an audience boundary. The manifest is generated from the plugin source.
func wendyEndUserSkillNames() ([]string, error) {
	return assets.EndUserSkillNames()
}

// ---- Claude Code ----------------------------------------------------------------

func installClaudeCodeSkills() *mcpSetupResult {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	configPath := filepath.Join(home, ".claude")
	if _, err := os.Stat(filepath.Join(home, ".claude.json")); err == nil {
		configPath = filepath.Join(home, ".claude.json")
	}
	return installDetectedToolSkills("Claude Code", "claude", configPath, filepath.Join(home, ".claude", "skills"))
}

func extractSkillFiles(skillName, target string) error {
	return fs.WalkDir(assets.FS, "skills/"+skillName, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel := strings.TrimPrefix(p, "skills/"+skillName+"/")
		dst := filepath.Join(target, rel)
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		data, err := assets.FS.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(dst, data, 0o644)
	})
}

// ---- Codex ----------------------------------------------------------------------

func installCodexSkills() *mcpSetupResult {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	return installDetectedToolSkills("Codex", "codex", filepath.Join(home, ".codex"), filepath.Join(home, ".agents", "skills"))
}

// Use each host's native skill discovery. Do not write private plugin caches or
// registries, and do not install per-skill plugins from an unrelated marketplace.
func installDetectedToolSkills(tool, binary, configDir, target string) *mcpSetupResult {
	if _, err := os.Stat(configDir); err != nil {
		if _, err := exec.LookPath(binary); err != nil {
			return nil
		}
	}
	if err := installWendySkillDirs(target); err != nil {
		return &mcpSetupResult{tool: tool + " end-user skills", err: err}
	}
	return &mcpSetupResult{tool: tool + " end-user skills", path: target}
}

func installWendySkillDirs(target string) error {
	names, err := wendyEndUserSkillNames()
	if err != nil {
		return err
	}
	for _, name := range names {
		if err := installManagedSkill(name, filepath.Join(target, name)); err != nil {
			return fmt.Errorf("installing %s: %w", name, err)
		}
	}
	return nil
}

// init should not offer installation again when setup already installed the
// current group. Compare every file, including references, before skipping.
func wendySkillsCurrent(target string) bool {
	names, err := wendyEndUserSkillNames()
	if err != nil {
		return false
	}
	for _, name := range names {
		err := fs.WalkDir(assets.FS, "skills/"+name, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			want, err := assets.FS.ReadFile(p)
			if err != nil {
				return err
			}
			got, err := os.ReadFile(filepath.Join(target, filepath.FromSlash(strings.TrimPrefix(p, "skills/"))))
			if err != nil {
				return err
			}
			if string(got) != string(want) {
				return fmt.Errorf("skill content differs: %s", p)
			}
			return nil
		})
		if err != nil {
			return false
		}
	}
	return true
}

// The shared user skill directory can contain hand-written or plugin-sourced
// Wendy skills. Only replace files from our previous install that remain
// unmodified, or files that already equal the current embedded version.
func installManagedSkill(name, target string) error {
	marker := filepath.Join(target, ".wendy-managed.json")
	previous := map[string]string{}
	if data, err := os.ReadFile(marker); err == nil {
		if err := json.Unmarshal(data, &previous); err != nil {
			return fmt.Errorf("reading skill ownership: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	next := map[string]string{}
	err := fs.WalkDir(assets.FS, "skills/"+name, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel := strings.TrimPrefix(p, "skills/"+name+"/")
		data, err := assets.FS.ReadFile(p)
		if err != nil {
			return err
		}
		next[rel] = fmt.Sprintf("%x", sha256.Sum256(data))
		path := filepath.Join(target, rel)
		existing, err := os.ReadFile(path)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		hash := fmt.Sprintf("%x", sha256.Sum256(existing))
		if hash != next[rel] && hash != previous[rel] {
			return fmt.Errorf("preserving existing or edited skill file %s; move that skill aside before reinstalling Wendy's version", path)
		}
		return nil
	})
	if err != nil {
		return err
	}
	if err := extractSkillFiles(name, target); err != nil {
		return err
	}
	data, err := json.MarshalIndent(next, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(marker, data, 0o644)
}

// ---- Opencode -------------------------------------------------------------------

func installOpencodeSkills() *mcpSetupResult {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	// OpenCode also discovers the shared agent skill directory, avoiding another
	// copy when Codex and OpenCode are installed together.
	return installDetectedToolSkills("OpenCode", "opencode", filepath.Join(home, ".config", "opencode"), filepath.Join(home, ".agents", "skills"))
}
