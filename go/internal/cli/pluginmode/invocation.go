package pluginmode

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// executable and lookPath are os.Executable and exec.LookPath; variables so
// tests can pose as any install.
var (
	executable = os.Executable
	lookPath   = exec.LookPath
)

// releaseDir matches the versioned directory the plugin's launcher installs
// a release into: <root>/cli/<YYYY.MM.DD-HHMMSS>/.
var releaseDir = regexp.MustCompile(`^\d{4}\.\d{2}\.\d{2}-\d{6}$`)

// CLIInvocation returns how a person or an assistant should invoke this CLI
// in a terminal. It is "wendy" unless the running binary is a
// launcher-managed install that PATH does not reach; then it is that
// install's absolute path — the stable <root>/bin pointer when present, since
// versioned directories are pruned — double-quoted if it contains spaces.
// Homebrew, apt, winget and dev builds always get "wendy".
func CLIInvocation() string {
	exe, err := executable()
	if err != nil {
		return "wendy"
	}
	resolved := resolvePath(exe)
	versionDir := filepath.Dir(resolved)
	cliDir := filepath.Dir(versionDir)
	if !releaseDir.MatchString(filepath.Base(versionDir)) || filepath.Base(cliDir) != "cli" {
		return "wendy"
	}
	pointer := filepath.Join(filepath.Dir(cliDir), "bin", filepath.Base(resolved))
	if onPath, err := lookPath("wendy"); err == nil && (sameFile(onPath, resolved) || sameFile(onPath, pointer)) {
		return "wendy"
	}
	path := resolved
	if fileExists(pointer) {
		path = pointer
	}
	if strings.ContainsAny(path, " \t") {
		return `"` + path + `"`
	}
	return path
}

// RewriteCLIHints rewrites the 'wendy …' and `wendy …` commands quoted in msg
// to use CLIInvocation, so a hint stays runnable when wendy is not on PATH.
// Everything else in msg is unchanged.
func RewriteCLIHints(msg string) string {
	if !strings.Contains(msg, "'wendy ") && !strings.Contains(msg, "`wendy ") {
		return msg
	}
	inv := CLIInvocation()
	if inv == "wendy" {
		return msg
	}
	return strings.NewReplacer("'wendy ", "'"+inv+" ", "`wendy ", "`"+inv+" ").Replace(msg)
}

// sameFile reports whether a and b name the same existing file, following
// symlinks.
func sameFile(a, b string) bool {
	ai, err := os.Stat(a)
	if err != nil {
		return false
	}
	bi, err := os.Stat(b)
	return err == nil && os.SameFile(ai, bi)
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
