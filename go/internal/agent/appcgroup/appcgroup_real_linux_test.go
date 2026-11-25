package appcgroup

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

const allocateEnv = "WENDY_APPCGROUP_ALLOCATE"

func TestMain(m *testing.M) {
	if os.Getenv(allocateEnv) == "1" {
		allocate(200 << 20)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// allocate touches every page so the memory is charged, not just mapped.
func allocate(n int) {
	b := make([]byte, n)
	for i := 0; i < len(b); i += os.Getpagesize() {
		b[i] = 1
	}
	runtime.KeepAlive(b)
}

// Run in a delegated unit: systemd-run --user --wait --pipe -p Delegate=yes
// -E WENDY_REAL_CGROUP=1 <test binary> -test.run TestConfigureOnARealCgroup
func TestConfigureOnARealCgroup(t *testing.T) {
	if os.Getenv("WENDY_REAL_CGROUP") != "1" {
		t.Skip("set WENDY_REAL_CGROUP=1 and run in a delegated cgroup")
	}
	own := ownCgroup(t)
	t.Logf("own cgroup: %s", own)
	if !hasMemory([]byte(get(t, filepath.Join(own, "cgroup.controllers")))) {
		t.Fatalf("memory controller not delegated to %s (cgroup.controllers = %q)", own, get(t, filepath.Join(own, "cgroup.controllers")))
	}

	// cgroup v2 forbids processes in a cgroup that enables child controllers.
	leaf := filepath.Join(own, "test")
	mkdir(t, leaf)
	put(t, filepath.Join(leaf, "cgroup.procs"), strconv.Itoa(os.Getpid()))

	apps := filepath.Join(own, Parent)
	t.Cleanup(func() { removeCgroup(t, apps) })
	const budget = 64 << 20
	st := Configure(own, gib, gib-budget)
	t.Logf("status: %+v", st)
	if st.Err != nil || !st.Limited || st.Max != budget {
		t.Fatalf("Configure = %+v", st)
	}
	for name, want := range map[string]string{"memory.max": strconv.Itoa(budget), "memory.high": "max"} {
		if got := strings.TrimSpace(get(t, filepath.Join(apps, name))); got != want {
			t.Fatalf("%s = %q, want %q", name, got, want)
		}
	}

	// The test machine may have swap; without this the allocator could swap
	// instead of being OOM-killed. Only the probe leaf gets it.
	probe := filepath.Join(apps, "probe")
	mkdir(t, probe)
	t.Cleanup(func() { removeCgroup(t, probe) })
	put(t, filepath.Join(probe, "memory.swap.max"), "0")
	dir, err := os.OpenFile(probe, os.O_RDONLY|syscall.O_DIRECTORY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()

	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), allocateEnv+"=1")
	cmd.SysProcAttr = &syscall.SysProcAttr{UseCgroupFD: true, CgroupFD: int(dir.Fd())}
	out, err := cmd.CombinedOutput()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("allocator was not killed: err %v, output %q", err, out)
	}
	ws, ok := exitErr.Sys().(syscall.WaitStatus)
	if !ok || !ws.Signaled() || ws.Signal() != syscall.SIGKILL {
		t.Fatalf("allocator ended with %v, want SIGKILL; output %q", err, out)
	}
	t.Logf("allocator: %v", err)

	events := get(t, filepath.Join(apps, "memory.events"))
	t.Logf("%s/memory.events:\n%s", Parent, events)
	if n := eventCount(events, "oom_kill"); n < 1 {
		t.Fatalf("oom_kill = %d, want at least 1", n)
	}
	if n := eventCount(get(t, filepath.Join(leaf, "memory.events")), "oom_kill"); n != 0 {
		t.Fatalf("test leaf oom_kill = %d, want 0", n)
	}
	t.Logf("test process %d alive", os.Getpid())
}

func ownCgroup(t *testing.T) string {
	t.Helper()
	for _, line := range strings.Split(get(t, "/proc/self/cgroup"), "\n") {
		if path, ok := strings.CutPrefix(line, "0::"); ok {
			return filepath.Join(cgroupRoot, path)
		}
	}
	t.Fatal("no cgroup v2 entry in /proc/self/cgroup")
	return ""
}

func mkdir(t *testing.T, dir string) {
	t.Helper()
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
}

func removeCgroup(t *testing.T, dir string) {
	if err := os.Remove(dir); err != nil && !os.IsNotExist(err) {
		t.Logf("cleanup: %v", err)
	}
}

func eventCount(events, key string) int {
	for _, line := range strings.Split(events, "\n") {
		if f := strings.Fields(line); len(f) == 2 && f[0] == key {
			n, _ := strconv.Atoi(f[1])
			return n
		}
	}
	return -1
}
