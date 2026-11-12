//go:build linux

package commands

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/internal/shared/wendyconf"
)

func requireInstallHostFixture(t *testing.T) {
	t.Helper()
	if os.Getenv("WENDY_INSTALL_HOST_TESTS") != "1" {
		t.Skip("set WENDY_INSTALL_HOST_TESTS=1 on a disposable Linux runner with passwordless sudo")
	}
}

func installFixtureCommand(t *testing.T, stdin, name string, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdin = strings.NewReader(stdin)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v: %v\n%s", name, args, err, out)
	}
	return strings.TrimSpace(string(out))
}

type installLoopFixture struct {
	t     *testing.T
	image string
	disk  string
}

func newInstallLoopFixture(t *testing.T) *installLoopFixture {
	t.Helper()
	requireInstallHostFixture(t)
	f := &installLoopFixture{t: t, image: filepath.Join(t.TempDir(), "disk.img")}
	t.Cleanup(func() {
		if f.disk != "" {
			f.detach()
		}
	})
	image, err := os.Create(f.image)
	if err != nil {
		t.Fatal(err)
	}
	if err := image.Truncate(128 << 20); err != nil {
		t.Fatal(err)
	}
	image.Close()
	installFixtureCommand(t, "label: gpt\n, , U\n", "sfdisk", f.image)
	f.attach("")
	installFixtureCommand(t, "", "sudo", "-n", "mkfs.vfat", "-F", "32", "-n", "config", f.disk+"p1")
	installFixtureCommand(t, "", "sudo", "-n", "udevadm", "trigger", "--action=change", f.disk+"p1")
	installFixtureCommand(t, "", "sudo", "-n", "udevadm", "settle", "--timeout=10")
	return f
}

func (f *installLoopFixture) attach(at string) {
	f.t.Helper()
	if at == "" {
		f.disk = installFixtureCommand(f.t, "", "sudo", "-n", "losetup", "--find", "--show", "--partscan", f.image)
	} else {
		// This can only claim an unused loop node. A competing attachment fails.
		installFixtureCommand(f.t, "", "sudo", "-n", "losetup", "--partscan", at, f.image)
		f.disk = at
	}
	if !strings.HasPrefix(f.disk, "/dev/loop") || strings.ContainsAny(f.disk, "\n ") {
		f.t.Fatalf("unexpected disposable device: %q", f.disk)
	}
	installFixtureCommand(f.t, "", "sudo", "-n", "udevadm", "settle", "--timeout=10")
}

func (f *installLoopFixture) detach() {
	f.t.Helper()
	backing := installFixtureCommand(f.t, "", "sudo", "-n", "losetup", "--noheadings", "--output", "BACK-FILE", f.disk)
	if backing != f.image {
		f.t.Fatalf("refusing to detach %s: backing file %q is not fixture %q", f.disk, backing, f.image)
	}
	installFixtureCommand(f.t, "", "sudo", "-n", "losetup", "--detach", f.disk)
	f.disk = ""
}

func verifyInstallLoopContents(t *testing.T, disk string, agent []byte) {
	t.Helper()
	partition, err := findConfigPartition(disk)
	if err != nil {
		t.Fatal(err)
	}
	// Do not recursively remove a mount point if unmount fails.
	dir, err := os.MkdirTemp("", "wendy-loop-read-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(dir)
	installFixtureCommand(t, "", "sudo", "-n", "mount", "-t", "vfat", "-o", "ro", partition, dir)
	defer installFixtureCommand(t, "", "sudo", "-n", "umount", dir)
	got, err := os.ReadFile(filepath.Join(dir, "wendy-agent"))
	if err != nil || !bytes.Equal(got, agent) {
		t.Fatalf("agent bytes = %q, %v", got, err)
	}
	conf, err := os.ReadFile(filepath.Join(dir, "wendy.conf"))
	if err != nil || !bytes.Contains(conf, []byte("name = loop-fixture")) {
		t.Fatalf("provisioning = %q, %v", conf, err)
	}
}

func TestInstallLoopRetriesDelayedPartitionDiscovery(t *testing.T) {
	f := newInstallLoopFixture(t)
	realLSBLK, err := exec.LookPath("lsblk")
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	counter := filepath.Join(bin, "attempts")
	// Hide only the first two label observations, modelling stale udev state.
	// The subsequent lookup, mounts, writes, unmounts and reads are real.
	wrapper := `#!/bin/sh
if [ "$1" = -o ] && [ "$2" = NAME,LABEL ]; then
  count=0
  if [ -f "$WENDY_LSBLK_COUNTER" ]; then read -r count < "$WENDY_LSBLK_COUNTER"; fi
  count=$((count + 1))
  echo "$count" > "$WENDY_LSBLK_COUNTER"
  if [ "$count" -le 2 ]; then exit 0; fi
fi
exec "$WENDY_REAL_LSBLK" "$@"
`
	if err := os.WriteFile(filepath.Join(bin, "lsblk"), []byte(wrapper), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("WENDY_REAL_LSBLK", realLSBLK)
	t.Setenv("WENDY_LSBLK_COUNTER", counter)
	agent := []byte("loop-fixture-agent")
	if err := writeConfigPartition(drive{DevicePath: f.disk}, agent, nil, "loop-fixture", nil); err != nil {
		t.Fatal(err)
	}
	count, err := os.ReadFile(counter)
	if err != nil || strings.TrimSpace(string(count)) != "3" {
		t.Fatalf("expected two stale observations then success, got %q: %v", count, err)
	}
	verifyInstallLoopContents(t, f.disk, agent)
}

func TestInstallLoopReconnectNeverWritesReplacementDisk(t *testing.T) {
	original, replacement := newInstallLoopFixture(t), newInstallLoopFixture(t)
	before, err := os.ReadFile(replacement.image)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(before)
	oldPath := original.disk
	agent := []byte("reconnected-agent")
	first := true
	calls := withStubbedProvisioning(t, func(d drive, c []wendyconf.WifiCredential, name string, j []byte) error {
		if first {
			first = false
			original.detach()
			replacement.detach()
			replacement.attach(oldPath)
			original.attach("")
			return &os.PathError{Op: "write", Path: oldPath, Err: syscall.ENXIO}
		}
		return writeConfigPartition(d, agent, c, name, j)
	}, true, []bool{true})
	rescanPartitionTableFn, readDriveIdentityFn, drivePresentFn = rescanPartitionTable, readDriveIdentity, drivePresent
	reconnectTimeout = 5 * time.Second
	listIdentifiedDrivesFn = func() ([]identifiedDrive, error) {
		var found []identifiedDrive
		for _, f := range []*installLoopFixture{replacement, original} {
			d := drive{DevicePath: f.disk}
			id, err := readDriveIdentity(d)
			if err != nil {
				return nil, err
			}
			found = append(found, identifiedDrive{drive: d, id: id})
		}
		return found, nil
	}
	got, attached, err := provisionConfigWithRetry(context.Background(), drive{DevicePath: oldPath}, nil, "loop-fixture", nil, true, nil)
	if err != nil || !attached || got.DevicePath != original.disk || *calls != 2 {
		t.Fatalf("reconnected=%s attached=%v calls=%d error=%v", got.DevicePath, attached, *calls, err)
	}
	after, err := os.ReadFile(replacement.image)
	if err != nil || sha256.Sum256(after) != digest {
		t.Fatalf("replacement disk changed: %v", err)
	}
	verifyInstallLoopContents(t, original.disk, agent)
}

func TestSudoHandBackRealOwnership(t *testing.T) {
	requireInstallHostFixture(t)
	if base := os.Getenv("WENDY_SUDO_FIXTURE"); base != "" {
		if os.Geteuid() != 0 {
			t.Fatal("fixture child must be elevated")
		}
		for _, getDir := range []func() (string, error){config.ConfigDir, config.CacheDir} {
			dir, err := getDir()
			if err != nil || !strings.HasPrefix(dir, base+string(os.PathSeparator)) {
				t.Fatalf("unsafe fixture directory %q: %v", dir, err)
			}
			if err := os.WriteFile(filepath.Join(dir, "root-created"), []byte("fixture"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		HandBackSudoFiles()
		return
	}
	base := t.TempDir()
	if os.Geteuid() == 0 {
		t.Fatal("run the parent as the invoking user, not root")
	}
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	installFixtureCommand(t, "", "sudo", "-n", "env", "WENDY_INSTALL_HOST_TESTS=1", "WENDY_SUDO_FIXTURE="+base,
		"WENDY_CONFIG_DIR="+filepath.Join(base, "config"), "XDG_CACHE_HOME="+filepath.Join(base, "cache"),
		binary, "-test.run=^TestSudoHandBackRealOwnership$", "-test.count=1", "-test.v")
	err = filepath.Walk(base, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		uid, _ := statOwner(info)
		if uid != os.Getuid() {
			return fmt.Errorf("%s is owned by %d, want %s", path, uid, strconv.Itoa(os.Getuid()))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"config/root-created", "cache/wendy/root-created"} {
		if err := os.WriteFile(filepath.Join(base, path), []byte("user can write again"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}
