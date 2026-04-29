//go:build darwin

package commands

import (
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestFindDescendantInPSOutput(t *testing.T) {
	// Synthetic process tree:
	//   1234 (sudo)
	//     └─ 5678 (sudo monitor, in new session)
	//          └─ 9012 (dd)
	//   1234's siblings have unrelated names that should not match.
	psOutput := `
   1   0 launchd
1234   1 sudo
5678 1234 sudo
9012 5678 dd
4242   1 dd
8888 9012 child-of-dd
`
	t.Run("finds dd two levels deep", func(t *testing.T) {
		pid, ok := findDescendantInPSOutput(psOutput, 1234, "dd")
		if !ok {
			t.Fatal("expected to find dd descendant")
		}
		if pid != 9012 {
			t.Errorf("got pid=%d, want 9012", pid)
		}
	})
	t.Run("does not return root itself", func(t *testing.T) {
		_, ok := findDescendantInPSOutput(psOutput, 9012, "dd")
		if ok {
			t.Error("expected not to find root as its own descendant")
		}
	})
	t.Run("ignores unrelated dd at top level", func(t *testing.T) {
		// Walking from 1234 should never reach 4242 (parented to launchd).
		pid, ok := findDescendantInPSOutput(psOutput, 1234, "dd")
		if !ok || pid == 4242 {
			t.Errorf("got pid=%d ok=%v, want unrelated dd to be skipped", pid, ok)
		}
	})
	t.Run("returns false when no match", func(t *testing.T) {
		_, ok := findDescendantInPSOutput(psOutput, 1234, "nonexistent")
		if ok {
			t.Error("expected no match")
		}
	})
	t.Run("handles full path in comm field", func(t *testing.T) {
		// macOS `ps -o comm=` may print the full executable path.
		ps := "1234 1 sudo\n5678 1234 /usr/bin/dd\n"
		pid, ok := findDescendantInPSOutput(ps, 1234, "dd")
		if !ok {
			t.Fatal("expected match on basename of /usr/bin/dd")
		}
		if pid != 5678 {
			t.Errorf("got pid=%d, want 5678", pid)
		}
	})
}

func TestScanDDProgressBSD(t *testing.T) {
	// Sample BSD dd SIGINFO output (each block is one signal).
	input := strings.Join([]string{
		"123+0 records in",
		"123+0 records out",
		"515899392 bytes transferred in 5.123 secs (100644789 bytes/sec)",
		"246+0 records in",
		"246+0 records out",
		"1031798784 bytes transferred in 10.246 secs (100704321 bytes/sec)",
		"",
	}, "\n")

	var got []int64
	scanDDProgressBSD(strings.NewReader(input), func(written int64) {
		got = append(got, written)
	})

	want := []int64{515899392, 1031798784}
	if len(got) != len(want) {
		t.Fatalf("got %d updates (%v), want %d (%v)", len(got), got, len(want), want)
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("update %d: got %d, want %d", i, got[i], w)
		}
	}
}

func TestScanDDProgressBSD_IgnoresNonProgressLines(t *testing.T) {
	// Lines that don't contain "bytes transferred" should be ignored.
	input := "starting up...\n123+0 records in\n123+0 records out\n"

	var got []int64
	scanDDProgressBSD(strings.NewReader(input), func(written int64) {
		got = append(got, written)
	})

	if len(got) != 0 {
		t.Errorf("expected no progress updates, got %v", got)
	}
}

func TestScanDDProgressBSD_NilCallback(t *testing.T) {
	// Should drain the reader without panicking when progressFn is nil.
	scanDDProgressBSD(strings.NewReader("anything"), nil)
}

// TestFindDescendantNamed_RealProcessTree exercises the live ps invocation
// against a real subprocess tree (sh → sleep), which is structurally similar
// to sudo's monitor → command pattern. This catches parsing regressions in
// real `ps` output, beyond what synthetic input covers.
func TestFindDescendantNamed_RealProcessTree(t *testing.T) {
	cmd := exec.Command("sh", "-c", "exec sleep 5")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer cmd.Process.Kill() //nolint:errcheck
	defer cmd.Wait()         //nolint:errcheck

	// Give the OS a moment to settle the process tree.
	time.Sleep(200 * time.Millisecond)

	// `sh -c "exec sleep 5"` exec's into sleep, so cmd.Process.Pid IS sleep.
	// findDescendantNamed should not find sleep as a descendant of itself,
	// and should find it as a descendant of its actual parent.
	if _, ok := findDescendantNamed(cmd.Process.Pid, "sleep"); ok {
		t.Error("did not expect to find sleep as descendant of itself")
	}
	// sleep's parent is this test process; verify we can find sleep from here.
	pid, ok := findDescendantNamed(os.Getpid(), "sleep")
	if !ok {
		t.Fatal("expected to find sleep as descendant of the test process")
	}
	if pid != cmd.Process.Pid {
		t.Errorf("got pid=%d, want %d", pid, cmd.Process.Pid)
	}
}
