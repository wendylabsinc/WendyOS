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
	cache := filepath.Join(root, "user-cache")
	if err := os.Mkdir(cache, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(cache, uid, gid); err != nil {
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
	for _, name := range []string{path, path + ".lock"} {
		info, err := os.Stat(name)
		if err != nil {
			t.Fatal(err)
		}
		if info.Sys().(*syscall.Stat_t).Uid != 0 {
			t.Fatal("fixture did not create root-owned stall state")
		}
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
	}{{path, uint32(uid)}, {path + ".lock", 0}} {
		info, err := os.Stat(item.path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Sys().(*syscall.Stat_t).Uid != item.owner || info.Mode().Perm() != 0644 {
			t.Fatalf("wrong final owner/mode for %s: %v", item.path, info)
		}
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
