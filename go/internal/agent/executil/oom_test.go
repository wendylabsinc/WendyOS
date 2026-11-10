//go:build unix

package executil

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestWorkloadResetsOnlyProtectedOOMScores(t *testing.T) {
	for _, score := range []string{"-1000", "-998", "0", "500"} {
		t.Run(score, func(t *testing.T) {
			dir := t.TempDir()
			scorePath := filepath.Join(dir, "oom_score_adj")
			if err := os.WriteFile(scorePath, []byte(score+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			marker := filepath.Join(dir, "must-not-exist")
			literal := "$(touch '" + marker + "'); * `echo bad`"
			cmd := exec.Command("/usr/bin/printf", "%s\n", literal)
			wrapOOMScoreReset(cmd, scorePath)
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("workload failed: %v; output %s", err, out)
			}
			wantScore := score
			if strings.HasPrefix(score, "-") {
				wantScore = "0"
			}
			if string(out) != literal+"\n" {
				t.Fatalf("output = %q; want unchanged literal argument", out)
			}
			gotScore, err := os.ReadFile(scorePath)
			if err != nil || string(gotScore) != wantScore+"\n" {
				t.Fatalf("score = %q, error %v; want %s", gotScore, err, wantScore)
			}
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Fatalf("workload argument was evaluated by wrapper: %v", err)
			}
		})
	}
}

func TestWorkloadDoesNotStartIfOOMScoreCannotBeRead(t *testing.T) {
	cmd := exec.Command("/usr/bin/printf", "workload-started")
	wrapOOMScoreReset(cmd, filepath.Join(t.TempDir(), "missing"))
	out, err := cmd.CombinedOutput()
	if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 125 {
		t.Fatalf("exit = %v; want 125, output %s", err, out)
	}
	if strings.Contains(string(out), "workload-started") {
		t.Fatal("started a workload without resetting inherited protection")
	}
}
