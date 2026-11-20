//go:build linux

package sandbox

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"strconv"
	"strings"
	"sync"
	"testing"

	"golang.org/x/sys/unix"
)

// statusOf parses /proc/<pid>/status text.
func statusOf(text string) map[string]string {
	fields := map[string]string{}
	for _, line := range strings.Split(text, "\n") {
		if k, v, ok := strings.Cut(line, ":"); ok {
			fields[k] = strings.Join(strings.Fields(v), " ")
		}
	}
	return fields
}

// procStatus returns the child's own /proc/self/status, started by start.
func procStatus(t *testing.T, start func(*exec.Cmd) error) map[string]string {
	t.Helper()
	cmd := exec.Command("/bin/cat", "/proc/self/status")
	var out, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &stderr
	if err := start(cmd); err != nil {
		t.Errorf("start: %v", err)
		return nil
	}
	if err := cmd.Wait(); err != nil || stderr.Len() != 0 {
		t.Errorf("wait: %v; stderr %q", err, stderr.String())
	}
	return statusOf(out.String())
}

func inRange(id string) bool {
	n, err := strconv.Atoi(id)
	return err == nil && n >= firstUID && n < firstUID+uidCount
}

func TestStartDropsRootForTheChildOnly(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("dropping root needs root")
	}
	// The agent's working directory is not searchable by the sandbox user either.
	t.Chdir(t.TempDir())
	video, err := user.LookupGroup("video")
	if err != nil {
		t.Skip("no video group on this host")
	}
	id, err := Isolated("video", "no-such-group")
	if err != nil {
		t.Fatal(err)
	}

	got := procStatus(t, id.Start)
	uid, _, _ := strings.Cut(got["Uid"], " ")
	if !inRange(uid) {
		t.Fatalf("sandboxed uid %q is outside %d-%d", uid, firstUID, firstUID+uidCount-1)
	}
	for field, want := range map[string]string{
		"Uid":        strings.Join([]string{uid, uid, uid, uid}, " "),
		"Gid":        strings.Join([]string{uid, uid, uid, uid}, " "),
		"Groups":     video.Gid,
		"NoNewPrivs": "1",
		"CapPrm":     "0000000000000000",
		"CapEff":     "0000000000000000",
		"CapAmb":     "0000000000000000",
	} {
		if got[field] != want {
			t.Errorf("sandboxed %s = %q; want %q", field, got[field], want)
		}
	}

	// The flag must stay on the one thread that carries it. A leak would show
	// up as a later, unrelated child of the agent that cannot use sudo or ping,
	// so start plain children while sandboxed ones are starting too.
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 25 {
				if got := procStatus(t, id.Start); got["NoNewPrivs"] != "1" {
					t.Errorf("sandboxed NoNewPrivs = %q; want 1", got["NoNewPrivs"])
				}
			}
		})
		wg.Go(func() {
			for range 25 {
				plain := procStatus(t, (*exec.Cmd).Start)
				if plain["NoNewPrivs"] != "0" || !strings.HasPrefix(plain["Uid"], "0 ") {
					t.Errorf("an unsandboxed child inherited the sandbox: NoNewPrivs=%q Uid=%q", plain["NoNewPrivs"], plain["Uid"])
				}
			}
		})
	}
	wg.Wait()
}

// Two helpers alive at once never share a uid, so one cannot read the other.
func TestIsolatedHelpersCannotReadEachOther(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("dropping root needs root")
	}
	id, err := Isolated()
	if err != nil {
		t.Fatal(err)
	}

	// Each victim is a cat that waits on its stdin with a secret in its
	// environment. They start at the same time, as cameras do.
	const victims = 6
	pids := make([]int, victims)
	var wg sync.WaitGroup
	for i := range victims {
		wg.Go(func() {
			cmd := exec.Command("/bin/cat")
			cmd.Env = []string{"CAMERA_PASSWORD=hunter2"}
			stdin, err := cmd.StdinPipe()
			if err != nil {
				t.Error(err)
				return
			}
			if err := id.Start(cmd); err != nil {
				t.Error(err)
				return
			}
			pids[i] = cmd.Process.Pid
			t.Cleanup(func() {
				stdin.Close()
				cmd.Wait() //nolint:errcheck
			})
		})
	}
	wg.Wait()
	if t.Failed() {
		t.FailNow()
	}
	seen := map[string]int{}
	for _, pid := range pids {
		status, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
		if err != nil {
			t.Fatal(err)
		}
		uid, _, _ := strings.Cut(statusOf(string(status))["Uid"], " ")
		if other, dup := seen[uid]; dup || !inRange(uid) {
			t.Fatalf("pid %d runs as uid %q, in range %v, shared with pid %d", pid, uid, inRange(uid), other)
		}
		seen[uid] = pid
	}
	t.Logf("%d helpers alive at once, uid -> pid: %v", victims, seen)

	// run is one more helper, trying to read path.
	run := func(path string) (string, error) {
		cmd := exec.Command("/bin/cat", path)
		var out, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &stderr
		if err := id.Start(cmd); err != nil {
			t.Fatal(err)
		}
		err := cmd.Wait()
		return out.String() + stderr.String(), err
	}
	// The control: a helper can read its own environment, so a refusal below
	// is about whose process it is, not about /proc.
	if out, err := run("/proc/self/environ"); err != nil {
		t.Fatalf("a helper cannot read its own environment: %v: %s", err, out)
	}
	for _, file := range []string{"environ", "mem"} {
		out, err := run(fmt.Sprintf("/proc/%d/%s", pids[0], file))
		if err == nil || !strings.Contains(out, "Permission denied") || strings.Contains(out, "hunter2") {
			t.Errorf("another helper read %s of pid %d: err=%v output=%q", file, pids[0], err, out)
		}
		t.Logf("another helper reading it: %s", strings.TrimSpace(out))
	}
}

func TestLookupRefusesWhatIsNotASandbox(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("lookups are skipped without root")
	}
	if id, err := Lookup("no-such-user-wendy-sandbox"); err == nil {
		t.Fatalf("unknown user resolved to %v", id)
	}
	if id, err := Lookup("root"); err == nil {
		t.Fatalf("root accepted as a sandbox identity: %v", id)
	}
}

func TestPickUIDSkipsUidsInUse(t *testing.T) {
	live, err := liveUIDs()
	if err != nil || !live[uint32(os.Getuid())] {
		t.Fatalf("liveUIDs does not include this process's own uid: %v, %v", live, err)
	}

	const free = firstUID + 123
	taken := map[uint32]bool{}
	for uid := uint32(firstUID); uid < firstUID+uidCount; uid++ {
		taken[uid] = uid != free
	}
	for range 3 {
		if uid, err := pickUID(taken); err != nil || uid != free {
			t.Fatalf("pickUID = %d, %v; want the one free uid %d", uid, err, free)
		}
	}
	taken[free] = true
	if uid, err := pickUID(taken); err == nil {
		t.Fatalf("pickUID returned %d with every uid in use", uid)
	}
	// With nothing in use, consecutive picks differ.
	a, _ := pickUID(nil)
	b, _ := pickUID(nil)
	if a == b || a == 0 || b == 0 {
		t.Fatalf("consecutive picks = %d, %d; want two different uids", a, b)
	}
}

func TestHideMemoryMakesTheProcessUndumpable(t *testing.T) {
	t.Cleanup(func() { unix.Prctl(unix.PR_SET_DUMPABLE, 1, 0, 0, 0) }) //nolint:errcheck
	if err := HideMemory(); err != nil {
		t.Fatal(err)
	}
	if got, err := unix.PrctlRetInt(unix.PR_GET_DUMPABLE, 0, 0, 0, 0); err != nil || got != 0 {
		t.Fatalf("dumpable = %d, %v; want 0", got, err)
	}
}
