//go:build linux && regression_chunk_ownership

package commands

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestChunkStallCacheRootThenUser(t *testing.T) {
	t.Run("user-owned-directory", func(t *testing.T) { chunkStallCacheRootThenUser(t, false) })
	t.Run("root-created-directory", func(t *testing.T) { chunkStallCacheRootThenUser(t, true) })
}

// rootCreatesDirectory is a first run under sudo: the user owns only the home
// above the cache, so the MkdirAll in rememberChunkUploadStall creates the
// cache as root and only the CLI's exit hook gives it back.
func chunkStallCacheRootThenUser(t *testing.T, rootCreatesDirectory bool) {
	if os.Getenv("WENDY_CHUNK_OWNERSHIP") != "1" {
		t.Skip("hosted ownership gate only")
	}
	if os.Getenv("GITHUB_ACTIONS") != "true" || os.Getenv("RUNNER_ENVIRONMENT") != "github-hosted" || !filepath.IsAbs(os.Getenv("WENDY_OWNER_SCRATCH")) {
		t.Fatal("requires disposable hosted scratch")
	}
	uid, e1 := strconv.Atoi(os.Getenv("WENDY_OWNER_UID"))
	gid, e2 := strconv.Atoi(os.Getenv("WENDY_OWNER_GID"))
	if os.Getuid() != 0 || e1 != nil || e2 != nil || uid <= 0 || gid <= 0 {
		t.Fatal("requires root and an explicit unprivileged owner")
	}
	root, err := os.MkdirTemp(os.Getenv("WENDY_OWNER_SCRATCH"), "wendy-stall-owner-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(root)
	if err := os.Chmod(root, 0755); err != nil {
		t.Fatal(err)
	}
	owned := filepath.Join(root, "user-cache")
	cache := owned
	if rootCreatesDirectory {
		owned = filepath.Join(root, "home")
		cache = filepath.Join(owned, ".cache", "wendy")
	}
	if err := os.Mkdir(owned, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(owned, uid, gid); err != nil {
		t.Fatal(err)
	}
	chunkStallTestDir = cache
	defer func() { chunkStallTestDir = "" }()
	oldMask := syscall.Umask(0077)
	err = rememberChunkUploadStall("root-device@0.19.3", time.Now())
	syscall.Umask(oldMask)
	if err != nil {
		t.Fatal(err)
	}
	path, err := chunkStallPath()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{cache, path, path + ".lock"} {
		info, err := os.Stat(name)
		if err != nil {
			t.Fatal(err)
		}
		if owner := info.Sys().(*syscall.Stat_t).Uid; owner != 0 && (name != cache || rootCreatesDirectory) {
			t.Fatal("fixture did not create root-owned stall state")
		}
	}
	lockOwner := uint32(0)
	if rootCreatesDirectory {
		// What cmd/wendy runs as a sudo invocation exits, with the environment
		// sudo gives it when it keeps the user's HOME.
		for name, value := range map[string]string{
			"HOME": owned, "XDG_CACHE_HOME": "", "WENDY_CONFIG_DIR": "",
			"SUDO_UID": strconv.Itoa(uid), "SUDO_GID": strconv.Itoa(gid),
		} {
			t.Setenv(name, value)
		}
		HandBackSudoFiles()
		lockOwner = uint32(uid)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, executable, "-test.run=^TestChunkStallCacheUnprivilegedProcess$", "-test.v", "-test.timeout=12s")
	cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + cache, "WENDY_CHUNK_OWNER_CHILD=" + cache, "WENDY_OWNER_UID=" + strconv.Itoa(uid)}
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid), Groups: []uint32{}}}
	out, err := cmd.CombinedOutput()
	t.Logf("unprivileged cache process: %s", out)
	if err != nil || !strings.Contains(string(out), "UNPRIVILEGED_STALL_CACHE_UPDATED") {
		t.Fatalf("unprivileged cache read/lock/update failed: %v", err)
	}
	stalls := loadChunkStalls()
	if len(stalls) != 2 || stalls["root-device@0.19.3"].IsZero() || stalls["user-device@0.19.3"].IsZero() {
		t.Fatalf("cross-UID update lost remembered devices: %v", stalls)
	}
	for _, item := range []struct {
		path  string
		owner uint32
	}{{path, uint32(uid)}, {path + ".lock", lockOwner}} {
		info, err := os.Stat(item.path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Sys().(*syscall.Stat_t).Uid != item.owner || info.Mode().Perm() != 0644 {
			t.Fatalf("wrong final owner/mode for %s: %v", item.path, info)
		}
	}
	if info, err := os.Stat(cache); err != nil || info.Sys().(*syscall.Stat_t).Uid != uint32(uid) {
		t.Fatalf("cache directory is not the user's: %v %v", info, err)
	}
	entries, err := os.ReadDir(cache)
	if err != nil || len(entries) != 2 {
		t.Fatalf("temporary stall files leaked: %v %v", entries, err)
	}
	t.Log("root-written stall memory remained readable and lockable; real user replaced data without losing either preference")
}

func TestChunkStallCacheUnprivilegedProcess(t *testing.T) {
	cache := os.Getenv("WENDY_CHUNK_OWNER_CHILD")
	if cache == "" {
		t.Skip("owned subprocess only")
	}
	uid, err := strconv.Atoi(os.Getenv("WENDY_OWNER_UID"))
	if err != nil || uid <= 0 || os.Getuid() != uid || os.Geteuid() != uid {
		t.Fatal("wrong real/effective user")
	}
	chunkStallTestDir = cache
	defer func() { chunkStallTestDir = "" }()
	if !chunkUploadStalledRecently("root-device@0.19.3", time.Now()) {
		t.Fatal("root-owned stall data was not readable by its user")
	}
	target := chunkUploadTarget{deviceKey: "root-device", osVersion: "0.19.3", directLink: true}
	cfg := chooseChunkUploadConfig("auto", target, func(key string) bool { return chunkUploadStalledRecently(key, time.Now()) })
	if cfg.compressor != "gzip" {
		t.Fatal("root-written preference did not prevent repeated uncompressed upload")
	}
	if err := rememberChunkUploadStall("user-device@0.19.3", time.Now()); err != nil {
		t.Fatalf("root-owned stall lock prevented user update: %v", err)
	}
	t.Log("UNPRIVILEGED_STALL_CACHE_UPDATED")
}
