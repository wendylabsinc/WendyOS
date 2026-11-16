package litebuild

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
)

// Seams for tests.
var (
	lookPath  = exec.LookPath
	fetchFile = FetchFile
)

// CheckGit checks that git is installed and on PATH.
func CheckGit() error {
	if _, err := lookPath("git"); err != nil {
		return fmt.Errorf("git is not installed or not on PATH; %s", GitInstallHint(runtime.GOOS))
	}
	return nil
}

// GitInstallHint names the command that installs git on goos.
func GitInstallHint(goos string) string {
	switch goos {
	case "darwin":
		return "install it via 'xcode-select --install' or 'brew install git'"
	case "windows":
		return "install it via 'winget install --id Git.Git -e --source winget'"
	}
	return "install the git package of your distribution (for example 'sudo apt install git')"
}

// Git runs git, in dir when it is not empty, and returns its output.
func Git(ctx context.Context, dir string, args ...string) (string, error) {
	if dir != "" {
		args = append([]string{"-C", dir}, args...)
	}
	cmd := exec.CommandContext(ctx, "git", args...)
	// Fail instead of waiting for credentials: the repository is public.
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if details := strings.TrimSpace(stderr.String()); details != "" {
			return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, details)
		}
		return "", fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return string(out), nil
}

// FetchFile returns the file at path of the wendy-lite repository at url,
// at ref: a tag, a branch or a commit SHA (git clone --branch does not take
// a SHA). It fetches that one commit without the content of the other
// files, into a temporary repository that it then removes: git show then
// downloads only the file.
func FetchFile(ctx context.Context, url, ref, path string) ([]byte, error) {
	dir, err := os.MkdirTemp("", "wendy-lite-fetch-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	if _, err := Git(ctx, dir, "init", "--quiet"); err != nil {
		return nil, err
	}
	if _, err := Git(ctx, dir, "remote", "add", "origin", url); err != nil {
		return nil, err
	}
	if _, err := Git(ctx, dir, "fetch", "--quiet", "--depth", "1", "--filter=blob:none", "origin", ref); err != nil {
		return nil, fmt.Errorf("fetching wendy-lite %q (check the version): %w", ref, err)
	}
	out, err := Git(ctx, dir, "show", "FETCH_HEAD:"+path)
	if err != nil {
		return nil, fmt.Errorf("reading %s of wendy-lite %s: %w", path, ref, err)
	}
	return []byte(out), nil
}

var masterConfCache = struct {
	sync.Mutex
	models map[string]*Model
}{models: map[string]*Model{}}

// FetchMasterConf returns the master configuration of the wendy-lite
// repository at url, at ref. It fetches it once per process: a `wendy run`
// needs it for each build and for the repair of sdkconfig.defaults.wendy.
// logf, when not nil, reports the fetch. A failed fetch is not kept.
func FetchMasterConf(ctx context.Context, url, ref string, logf func(format string, args ...any)) (*Model, error) {
	masterConfCache.Lock()
	defer masterConfCache.Unlock()
	key := url + "\x00" + ref
	if m := masterConfCache.models[key]; m != nil {
		return m, nil
	}
	if err := CheckGit(); err != nil {
		return nil, err
	}
	if logf != nil {
		logf("Fetching %s of wendy-lite %s from %s...", MasterConfFile, ShortVersion(ref), url)
	}
	data, err := fetchFile(ctx, url, ref, MasterConfFile)
	if err != nil {
		return nil, err
	}
	m, err := Load(data, fmt.Sprintf("wendy-lite %s:%s", ShortVersion(ref), MasterConfFile))
	if err != nil {
		return nil, err
	}
	masterConfCache.models[key] = m
	return m, nil
}

// ShortVersion returns a Wendy Lite version for the messages: a full commit
// SHA is cut to 7 hex digits.
func ShortVersion(version string) string {
	if len(version) == 40 && strings.Trim(strings.ToLower(version), "0123456789abcdef") == "" {
		return version[:7]
	}
	return version
}
