//go:build linux

package devcover

import (
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// startNamespace runs a child in its own private mount namespace and returns
// its pid once the namespace has been entered.
func startNamespace(t *testing.T) uint32 {
	t.Helper()
	requireRoot(t)
	cmd := exec.Command("unshare", "--mount", "--propagation", "private", "sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start child: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })

	self, err := os.Readlink("/proc/self/ns/mnt")
	if err != nil {
		t.Fatal(err)
	}
	child := "/proc/" + strconv.Itoa(cmd.Process.Pid) + "/ns/mnt"
	deadline := time.Now().Add(5 * time.Second)
	for {
		// unshare enters the namespace, makes it private, and only then
		// execs sleep; wait for the exec so a cover cannot propagate to the
		// host while the namespace is still shared.
		ns, err := os.Readlink(child)
		comm, _ := os.ReadFile("/proc/" + strconv.Itoa(cmd.Process.Pid) + "/comm")
		if err == nil && ns != self && strings.TrimSpace(string(comm)) == "sleep" {
			return uint32(cmd.Process.Pid)
		}
		if time.Now().After(deadline) {
			t.Fatal("child never entered its own mount namespace")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func readIn(t *testing.T, pid uint32, path string) string {
	t.Helper()
	b, err := os.ReadFile("/proc/" + strconv.FormatUint(uint64(pid), 10) + "/root" + path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func openNS(t *testing.T, pid uint32) *Namespace {
	t.Helper()
	ns, err := Open(pid)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ns.Close() })
	return ns
}

func TestCover_BindsSourceInsideTargetNamespaceOnly(t *testing.T) {
	pid := startNamespace(t)
	dir := t.TempDir()
	src := writeFile(t, dir, "src", "share")
	dst := writeFile(t, dir, "dst", "real")

	if err := Cover(openNS(t, pid), src, dst, time.Second); err != nil {
		t.Fatalf("Cover: %v", err)
	}
	if got, _ := os.ReadFile(dst); string(got) != "real" {
		t.Fatal("cover reached the agent's own mount namespace")
	}
	if got := readIn(t, pid, dst); got != "share" {
		t.Fatalf("inside the container %s reads %q, want %q", dst, got, "share")
	}
}

func TestCover_MissingTargetFailsWithNotExist(t *testing.T) {
	pid := startNamespace(t)
	src := writeFile(t, t.TempDir(), "src", "share")
	ns := openNS(t, pid)

	start := time.Now()
	err := Cover(ns, src, filepath.Join(t.TempDir(), "missing"), time.Second)
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("err = %v, want fs.ErrNotExist", err)
	}
	if !strings.Contains(err.Error(), "move_mount(-> ") {
		t.Fatalf("err %q does not name the move_mount step", err)
	}
	if d := time.Since(start); d > 50*time.Millisecond {
		t.Fatalf("took %v, want < 50ms", d)
	}
}

func TestCover_SetnsFailureIsReportedByStep(t *testing.T) {
	requireRoot(t)
	ns, err := openPath("/proc/self/ns/net", uint32(os.Getpid()))
	if err != nil {
		t.Fatal(err)
	}
	defer ns.Close()
	src := writeFile(t, t.TempDir(), "src", "share")

	err = Cover(ns, src, src, time.Second)
	if err == nil || !strings.Contains(err.Error(), "setns(CLONE_NEWNS)") {
		t.Fatalf("err = %v, want it to name setns(CLONE_NEWNS)", err)
	}
	if !errors.Is(err, unix.EINVAL) {
		t.Fatalf("err = %v, want EINVAL", err)
	}
}

func TestCover_TimesOutAndNeverAppliesLate(t *testing.T) {
	pid := startNamespace(t)
	dir := t.TempDir()
	src := writeFile(t, dir, "src", "share")
	dst := writeFile(t, dir, "dst", "real")
	ns := openNS(t, pid)

	inNamespaceHook = func() { time.Sleep(300 * time.Millisecond) }
	defer func() { inNamespaceHook = func() {} }()

	start := time.Now()
	err := Cover(ns, src, dst, 50*time.Millisecond)
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("err = %v, want ErrTimeout", err)
	}
	if d := time.Since(start); d > 150*time.Millisecond {
		t.Fatalf("took %v, want < 150ms", d)
	}
	time.Sleep(500 * time.Millisecond)
	if got := readIn(t, pid, dst); got != "real" {
		t.Fatalf("a timed-out cover was applied late: %s reads %q", dst, got)
	}
}

func TestNamespace_CloseIsIdempotentAndClosedIsRefused(t *testing.T) {
	pid := startNamespace(t)
	dir := t.TempDir()
	src := writeFile(t, dir, "src", "share")
	dst := writeFile(t, dir, "dst", "real")
	ns, err := Open(pid)
	if err != nil {
		t.Fatal(err)
	}
	if err := ns.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := ns.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if err := Cover(ns, src, dst, time.Second); !errors.Is(err, ErrClosed) {
		t.Fatalf("Cover on closed = %v, want ErrClosed", err)
	}
	if got := readIn(t, pid, dst); got != "real" {
		t.Fatalf("Cover on a closed namespace mounted: %s reads %q", dst, got)
	}
	if _, err := Covered(ns, "/dev/null", dst); !errors.Is(err, ErrClosed) {
		t.Fatalf("Covered on closed = %v, want ErrClosed", err)
	}
	if _, err := ns.Inode(); !errors.Is(err, ErrClosed) {
		t.Fatalf("Inode on closed = %v, want ErrClosed", err)
	}
}

func TestCover_NilNamespaceIsRefused(t *testing.T) {
	src := writeFile(t, t.TempDir(), "src", "share")
	if err := Cover(nil, src, src, time.Second); !errors.Is(err, ErrClosed) {
		t.Fatalf("Cover(nil) = %v, want ErrClosed", err)
	}
	if _, err := Covered(nil, src, src); !errors.Is(err, ErrClosed) {
		t.Fatalf("Covered(nil) = %v, want ErrClosed", err)
	}
	if _, err := (*Namespace)(nil).Inode(); !errors.Is(err, ErrClosed) {
		t.Fatalf("Inode(nil) = %v, want ErrClosed", err)
	}
}

func TestCovered_ReportsWhetherDstIsSrc(t *testing.T) {
	pid := startNamespace(t)
	ns := openNS(t, pid)
	dst := writeFile(t, t.TempDir(), "dst", "")
	const src = "/dev/null"

	if ok, err := Covered(ns, src, dst); err != nil || ok {
		t.Fatalf("before cover: Covered = %v, %v; want false, nil", ok, err)
	}
	if err := Cover(ns, src, dst, time.Second); err != nil {
		t.Fatalf("Cover: %v", err)
	}
	if ok, err := Covered(ns, src, dst); err != nil || !ok {
		t.Fatalf("after cover: Covered = %v, %v; want true, nil", ok, err)
	}
	if ok, err := Covered(ns, src, filepath.Join(t.TempDir(), "gone")); err != nil || ok {
		t.Fatalf("missing dst: Covered = %v, %v; want false, nil", ok, err)
	}
}

func TestNamespace_InodeIdentifiesNamespace(t *testing.T) {
	pid := startNamespace(t)
	child, err := openNS(t, pid).Inode()
	if err != nil {
		t.Fatal(err)
	}
	self, err := openPath("/proc/self/ns/mnt", uint32(os.Getpid()))
	if err != nil {
		t.Fatal(err)
	}
	defer self.Close()
	mine, err := self.Inode()
	if err != nil {
		t.Fatal(err)
	}
	if child == mine {
		t.Fatal("child and agent report the same mount namespace inode")
	}
	if openNS(t, pid).PID() != pid {
		t.Fatal("PID() does not return the opened pid")
	}
}
