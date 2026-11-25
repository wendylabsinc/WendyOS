package litebuild

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestCheckGit(t *testing.T) {
	orig := lookPath
	t.Cleanup(func() { lookPath = orig })

	lookPath = func(string) (string, error) { return "", exec.ErrNotFound }
	err := CheckGit()
	if err == nil || !strings.Contains(err.Error(), "git is not installed") || !strings.Contains(err.Error(), GitInstallHint(runtime.GOOS)) {
		t.Errorf("CheckGit without git = %v, want the install hint", err)
	}
	lookPath = func(string) (string, error) { return "/usr/bin/git", nil }
	if err := CheckGit(); err != nil {
		t.Errorf("CheckGit with git = %v", err)
	}
}

// FetchMasterConf fetches a ref once per process, and keeps no failure.
func TestFetchMasterConfCache(t *testing.T) {
	origLook, origFetch := lookPath, fetchFile
	t.Cleanup(func() { lookPath, fetchFile = origLook, origFetch })
	lookPath = func(string) (string, error) { return "/usr/bin/git", nil }

	const url = "https://example.test/wendy-lite.git"
	ref := "0123456789abcdef0123456789abcdef01234567"
	fetches := 0
	fail := true
	fetchFile = func(_ context.Context, gotURL, gotRef, path string) ([]byte, error) {
		fetches++
		if gotURL != url || gotRef != ref || path != MasterConfFile {
			t.Errorf("fetch %s %s %s", gotURL, gotRef, path)
		}
		if fail {
			return nil, errors.New("offline")
		}
		return []byte(appModel), nil
	}
	var logs []string
	logf := func(format string, args ...any) { logs = append(logs, format) }

	if _, err := FetchMasterConf(context.Background(), url, ref, logf); err == nil {
		t.Fatal("want the fetch error")
	}
	fail = false
	m1, err := FetchMasterConf(context.Background(), url, ref, logf)
	if err != nil {
		t.Fatal(err)
	}
	m2, err := FetchMasterConf(context.Background(), url, ref, logf)
	if err != nil {
		t.Fatal(err)
	}
	if m1 != m2 || fetches != 2 || len(logs) != 2 {
		t.Errorf("same model %v, fetches %d, logs %d; want true, 2 (one failed), 2", m1 == m2, fetches, len(logs))
	}
	if m1.Path != "wendy-lite 0123456:master_config.yaml" {
		t.Errorf("model name = %q", m1.Path)
	}

	// No git: no fetch.
	lookPath = func(string) (string, error) { return "", exec.ErrNotFound }
	if _, err := FetchMasterConf(context.Background(), url, "other", nil); err == nil || !strings.Contains(err.Error(), "git is not installed") {
		t.Errorf("without git: %v", err)
	}
	if fetches != 2 {
		t.Errorf("fetched without git")
	}
}

func TestShortVersion(t *testing.T) {
	for in, want := range map[string]string{
		"6e2ae72d0b0433a2e65744b6f0821f09b4770541": "6e2ae72",
		"6E2AE72D0B0433A2E65744B6F0821F09B4770541": "6E2AE72",
		"v1.2.3":  "v1.2.3",
		"6e2ae72": "6e2ae72",
		"nightly": "nightly",
		"":        "",
		"zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz": "zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz",
	} {
		if got := ShortVersion(in); got != want {
			t.Errorf("ShortVersion(%q) = %q, want %q", in, got, want)
		}
	}
}

// LookupMasterConf reads the master configuration of a commit, also of a
// commit that is not the tip of a branch.
func TestLookupMasterConf(t *testing.T) {
	ctx := context.Background()
	r := newTestRepo(t)
	without := r.commit("no master configuration")
	if err := os.WriteFile(filepath.Join(r.dir, MasterConfFile), []byte(appModel), 0o644); err != nil {
		t.Fatal(err)
	}
	r.git("add", MasterConfFile)
	r.git("commit", "--quiet", "-m", "master configuration")
	with := r.git("rev-parse", "HEAD")
	r.commit("later")

	for commit, want := range map[string]bool{without: false, with: true} {
		data, found, err := LookupMasterConf(ctx, r.url, commit)
		if err != nil || found != want {
			t.Errorf("LookupMasterConf(%s) found %v, %v; want %v", ShortVersion(commit), found, err, want)
		}
		if want && string(data) != appModel {
			t.Errorf("LookupMasterConf(%s) = %q, want the file", ShortVersion(commit), data)
		}
	}
	if _, _, err := LookupMasterConf(ctx, r.url, strings.Repeat("f", 40)); err == nil || !strings.Contains(err.Error(), "check the version") {
		t.Errorf("unknown commit: %v, want an error", err)
	}
}

// FetchMasterConf of a newer format asks to update the CLI; a content that
// this code rejects asks to verify it.
func TestFetchMasterConfFormat(t *testing.T) {
	origLook, origFetch := lookPath, fetchFile
	t.Cleanup(func() { lookPath, fetchFile = origLook, origFetch })
	lookPath = func(string) (string, error) { return "/usr/bin/git", nil }
	for _, tc := range []struct {
		version, ref, want string
		format             bool // a FormatVersionError, else a ConfError
	}{
		{"2.0", "v9.0.0", "Wendy Lite v9.0.0 needs a newer version of the CLI. Update the CLI", true},
		{"0.1", "v9.0.1", "Wendy Lite v9.0.1 is not compatible with this version of the CLI", true},
		{"1.0", "v9.0.2", "Wendy Lite v9.0.2 is not compatible with this version of the CLI. Verify that the CLI is up-to-date", false},
	} {
		fetchFile = func(context.Context, string, string, string) ([]byte, error) {
			return []byte("format_version: \"" + tc.version + "\"\nattributes: {}\nnew_key: 1\n"), nil
		}
		_, err := FetchMasterConf(context.Background(), "https://example.test/wendy-lite.git", tc.ref, nil)
		var formatErr *FormatVersionError
		var confErr *ConfError
		wraps := errors.As(err, &confErr)
		if tc.format {
			wraps = errors.As(err, &formatErr)
		}
		if err == nil || err.Error() != tc.want || !wraps {
			t.Errorf("format %s: %v, want %q", tc.version, err, tc.want)
		}
	}
}
