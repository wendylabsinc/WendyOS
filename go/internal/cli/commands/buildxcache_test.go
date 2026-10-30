package commands

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

func TestBuildxLocalCacheDir(t *testing.T) {
	const userCache = "/home/u/.cache"
	base := filepath.Join(userCache, "wendy", "buildx")

	if got := buildxLocalCacheDir(userCache, ""); got != base {
		t.Fatalf("empty cache key = %q, want the shared base dir %q", got, base)
	}

	// A keyed build lives in its own subdir of the base, named after the key so
	// the cache stays browsable.
	for _, key := range []string{"myapp-gpu", "myapp-vui", "My/App:1"} {
		got := buildxLocalCacheDir(userCache, key)
		if filepath.Dir(got) != base {
			t.Errorf("buildxLocalCacheDir(%q) = %q, want a direct child of %q", key, got, base)
		}
		if !strings.HasPrefix(filepath.Base(got), sanitizeAppleContainerTag(key)) {
			t.Errorf("buildxLocalCacheDir(%q) = %q, want the sanitized key as its prefix", key, got)
		}
	}
}

// TestBuildxLocalCacheDirIsCollisionFree is the core WDY-1689/WDY-1711
// invariant: two concurrent service builds must never resolve to the same local
// cache dir. Sharing one dir corrupts BuildKit's cache-export ingest store —
// verified locally as `rename .../ingest/<hash>/startedat.tmp ...: no such file
// or directory`, which failed 3 of 6 concurrent builds. Sanitizing alone is not
// enough: service names are not constrained by wendy.schema.json, so distinct
// names can fold onto one sanitized string.
func TestBuildxLocalCacheDirIsCollisionFree(t *testing.T) {
	const userCache = "/home/u/.cache"

	keys := []string{
		"app-a", "app-b",
		// Pairs that share a sanitized form and would have collided.
		"ros2multi-a/b", "ros2multi-a-b", "ros2multi-a:b",
		"ros2multi-Talker", "ros2multi-talker",
		// Same sanitized 48-char prefix, different tails.
		"ros2multi-" + strings.Repeat("x", 48) + "-one",
		"ros2multi-" + strings.Repeat("x", 48) + "-two",
		// This pair has the same sanitized 48-char prefix and the same first
		// 32 SHA-256 bits, so a four-byte digest suffix would still collide.
		strings.Repeat("x", 48) + "-40818",
		strings.Repeat("x", 48) + "-102405",
	}
	seen := map[string]string{}
	for _, key := range keys {
		dir := buildxLocalCacheDir(userCache, key)
		if other, dup := seen[dir]; dup {
			t.Fatalf("cache keys %q and %q collided on %q", other, key, dir)
		}
		seen[dir] = key
	}
}

// TestBuildxLocalCacheDirStaysInsideBase guards containment: a service name is
// free-form, and a key like ".." must not walk the cache out of its own root
// (where it would both escape and re-collide with other builds).
func TestBuildxLocalCacheDirStaysInsideBase(t *testing.T) {
	const userCache = "/home/u/.cache"
	base := filepath.Join(userCache, "wendy", "buildx")

	for _, key := range []string{"..", ".", "../..", "../../etc", "a/../..", ""} {
		dir := buildxLocalCacheDir(userCache, key)
		clean := filepath.Clean(dir)
		if clean != base && !strings.HasPrefix(clean, base+string(filepath.Separator)) {
			t.Errorf("cache key %q escaped the cache root: %q", key, clean)
		}
	}
}

// TestEnsureCleanDockerConfigIsConcurrentSafe covers the other piece of shared
// state on the parallel build path (WDY-1711): every concurrent service build
// materializes the same ~/.cache/wendy/docker-config/config.json while other
// builds' buildx clients are reading it. A truncate-then-write (os.WriteFile)
// exposes an empty config to those readers; the write must be atomic.
func TestEnsureCleanDockerConfigIsConcurrentSafe(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows uses the host docker config unchanged")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("DOCKER_CONFIG", filepath.Join(home, ".docker"))

	configFile := filepath.Join(home, ".cache", "wendy", "docker-config", "config.json")

	stop := make(chan struct{})
	bad := make(chan string, 1)
	var readers sync.WaitGroup
	for range 4 {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				// A reader may find the file absent (before the first write), but
				// never half-written.
				if data, err := os.ReadFile(configFile); err == nil && string(data) != cleanDockerConfigContents {
					select {
					case bad <- string(data):
					default:
					}
					return
				}
			}
		}()
	}

	var writers sync.WaitGroup
	dirs := make([]string, 8)
	errs := make([]error, 8)
	for i := range dirs {
		writers.Add(1)
		go func() {
			defer writers.Done()
			dirs[i], errs[i] = ensureCleanDockerConfig()
		}()
	}
	writers.Wait()
	close(stop)
	readers.Wait()

	select {
	case got := <-bad:
		t.Fatalf("reader observed a partially written docker config: %q", got)
	default:
	}

	for i, err := range errs {
		if err != nil {
			t.Fatalf("ensureCleanDockerConfig (goroutine %d): %v", i, err)
		}
		if dirs[i] != filepath.Dir(configFile) {
			t.Fatalf("ensureCleanDockerConfig (goroutine %d) = %q, want %q", i, dirs[i], filepath.Dir(configFile))
		}
	}

	data, err := os.ReadFile(configFile)
	if err != nil {
		t.Fatalf("reading final config: %v", err)
	}
	if string(data) != cleanDockerConfigContents {
		t.Fatalf("final config = %q, want %q", data, cleanDockerConfigContents)
	}
	// No temp files left behind.
	entries, err := os.ReadDir(filepath.Dir(configFile))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "config.json.") {
			t.Errorf("leftover temp file %q", e.Name())
		}
	}
}

func TestBuildxArgsRequestPlainProgress(t *testing.T) {
	// Both buildx arg builders must request --progress=plain so the CLI can
	// parse a deterministic format. Guard against accidental removal.
	for _, f := range []string{"docker.go", "ocilayers.go"} {
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		if !strings.Contains(string(src), `"--progress", "plain"`) {
			t.Errorf("%s: expected buildx args to include --progress plain", f)
		}
	}
}

// An account with no ~/.docker yet (root on a device, say) must not get links
// to directories that do not exist: buildx's first write through one fails
// with "mkdir …/docker-config/buildx: file exists". Links an earlier run left
// dangling are repaired, and when the docker config cannot be created at all
// the link is dropped so buildx can make a real directory instead.
func TestEnsureCleanDockerConfigNeverLeavesDanglingLinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows uses the host docker config unchanged")
	}
	subdirs := []string{"buildx", "cli-plugins", "contexts"}
	for _, tc := range []struct {
		name                 string
		uncreatable, earlier bool
	}{
		{"fresh account", false, false},
		{"dangling links from an earlier run", false, true},
		{"docker config cannot be created", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			dockerConfig := filepath.Join(home, ".docker")
			if tc.uncreatable {
				blocker := filepath.Join(home, "not-a-directory")
				if err := os.WriteFile(blocker, nil, 0o644); err != nil {
					t.Fatal(err)
				}
				dockerConfig = filepath.Join(blocker, ".docker")
			}
			t.Setenv("DOCKER_CONFIG", dockerConfig)
			if tc.earlier {
				dir := filepath.Join(home, ".cache", "wendy", "docker-config")
				if err := os.MkdirAll(dir, 0o755); err != nil {
					t.Fatal(err)
				}
				for _, subdir := range subdirs {
					if err := os.Symlink(filepath.Join(dockerConfig, subdir), filepath.Join(dir, subdir)); err != nil {
						t.Fatal(err)
					}
				}
			}

			dir, err := ensureCleanDockerConfig()
			if err != nil {
				t.Fatal(err)
			}
			for _, subdir := range subdirs {
				path := filepath.Join(dir, subdir)
				if tc.uncreatable {
					if _, err := os.Lstat(path); !os.IsNotExist(err) {
						t.Fatalf("%s: kept a link to a directory that cannot exist (lstat err %v)", subdir, err)
					}
				} else if info, err := os.Stat(path); err != nil || !info.IsDir() {
					t.Fatalf("%s does not resolve to a directory: %v", subdir, err)
				}
			}
			// buildx's first write on a new account.
			if err := os.MkdirAll(filepath.Join(dir, "buildx", "instances"), 0o755); err != nil {
				t.Fatalf("buildx cannot create its state: %v", err)
			}
			if !tc.uncreatable {
				// Builder state stays shared with the account's own docker config.
				if _, err := os.Stat(filepath.Join(dockerConfig, "buildx", "instances")); err != nil {
					t.Fatalf("builder state not shared with %s: %v", dockerConfig, err)
				}
			}
		})
	}
}

// Parallel service builds call ensureCleanDockerConfig at once, after the
// build lock is released, and each build's buildx writes through the links
// straight away. A build must never remove a link another build just made:
// buildx would then create a real buildx directory in the gap, which every
// later run accepts, and the build's builder registrations would no longer
// be the account's own.
func TestEnsureCleanDockerConfigConcurrentBuildsKeepTheLinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows uses the host docker config unchanged")
	}
	for iteration := range 50 {
		home := t.TempDir()
		t.Setenv("HOME", home)
		t.Setenv("DOCKER_CONFIG", filepath.Join(home, ".docker"))

		var builds sync.WaitGroup
		errs := make([]error, 8)
		for i := range errs {
			builds.Add(1)
			go func() {
				defer builds.Done()
				dir, err := ensureCleanDockerConfig()
				if err == nil {
					err = os.MkdirAll(filepath.Join(dir, "buildx", "instances"), 0o755)
				}
				errs[i] = err
			}()
		}
		builds.Wait()
		for i, err := range errs {
			if err != nil {
				t.Fatalf("iteration %d, build %d: %v", iteration, i, err)
			}
		}
		dir := filepath.Join(home, ".cache", "wendy", "docker-config")
		for _, subdir := range []string{"buildx", "cli-plugins", "contexts"} {
			info, err := os.Lstat(filepath.Join(dir, subdir))
			if err != nil {
				t.Fatalf("iteration %d: %s: %v", iteration, subdir, err)
			}
			if info.Mode()&os.ModeSymlink == 0 {
				t.Fatalf("iteration %d: %s is a %v, not a link", iteration, subdir, info.Mode())
			}
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			if strings.Contains(e.Name(), ".link-") {
				t.Fatalf("iteration %d: leftover temporary link %q", iteration, e.Name())
			}
		}
	}
}

func TestAppleContainerBuildRequestsPlainProgress(t *testing.T) {
	// Apple Container builds must also request --progress=plain so their output
	// renders through the shared build progress UI (default --progress auto emits
	// an interactive [+] Building UI the build parser cannot read). The adjacent
	// "build", "--progress", "plain" tokens are unique to the apple-container arg
	// builders (buildx prepends "buildx", "build"). Guard against accidental removal.
	for _, f := range []string{"docker.go", "ocilayers.go"} {
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		if !strings.Contains(string(src), `"build", "--progress", "plain"`) {
			t.Errorf("%s: expected apple container build args to include --progress plain", f)
		}
	}
}
