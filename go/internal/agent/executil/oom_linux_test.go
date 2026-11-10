package executil

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
)

func TestResetOOMScoreInKernelDoesNotChangeParent(t *testing.T) {
	before, err := os.ReadFile("/proc/self/oom_score_adj")
	if err != nil {
		t.Fatal(err)
	}
	parentScore, err := strconv.Atoi(strings.TrimSpace(string(before)))
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("/bin/cat", "/proc/self/oom_score_adj")
	ResetOOMScore(cmd)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("workload failed: %v; %s", err, out)
	}
	want := strconv.Itoa(max(parentScore, 0))
	if strings.TrimSpace(string(out)) != want {
		t.Fatalf("child OOM score = %q; want %s", out, want)
	}
	after, err := os.ReadFile("/proc/self/oom_score_adj")
	if err != nil || string(before) != string(after) {
		t.Fatalf("parent OOM score changed from %q to %q; error %v", before, after, err)
	}
}
