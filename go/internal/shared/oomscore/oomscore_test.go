//go:build unix

package oomscore

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func writeScore(t *testing.T, score string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "oom_score_adj")
	if err := os.WriteFile(p, []byte(score+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func readScore(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(b))
}

func TestResetChildResetsANegativeScore(t *testing.T) {
	p := writeScore(t, "-999")
	cmd := exec.Command("/usr/bin/printf", "%s|%s", "one", "two")
	wrapReset(cmd, p)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("run: %v; %s", err, out)
	}
	if string(out) != "one|two" {
		t.Fatalf("output = %q; want original args", out)
	}
	if got := readScore(t, p); got != "0" {
		t.Fatalf("score = %q; want 0", got)
	}
}

func TestResetChildKeepsAPositiveScore(t *testing.T) {
	p := writeScore(t, "300")
	cmd := exec.Command("/usr/bin/true")
	wrapReset(cmd, p)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("run: %v; %s", err, out)
	}
	if got := readScore(t, p); got != "300" {
		t.Fatalf("score = %q; want 300", got)
	}
}

func TestResetChildNeverEvaluatesArguments(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	p := writeScore(t, "-999")
	args := []string{"a b", "$(touch x)", `'q"`}
	cmd := exec.Command("/usr/bin/printf", append([]string{"%s\n"}, args...)...)
	wrapReset(cmd, p)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("run: %v; %s", err, out)
	}
	if want := strings.Join(args, "\n") + "\n"; string(out) != want {
		t.Fatalf("output = %q; want %q", out, want)
	}
	if _, err := os.Stat(filepath.Join(dir, "x")); !os.IsNotExist(err) {
		t.Fatalf("argument was evaluated: %v", err)
	}
}

func TestResetChildKeepsPidAndExitCode(t *testing.T) {
	p := writeScore(t, "-999")
	cmd := exec.Command("/bin/sh", "-c", `echo $$; exit 7`)
	wrapReset(cmd, p)
	out, err := cmd.Output()
	exit, ok := err.(*exec.ExitError)
	if !ok || exit.ExitCode() != 7 {
		t.Fatalf("err = %v; want exit 7", err)
	}
	pid, perr := strconv.Atoi(strings.TrimSpace(string(out)))
	if perr != nil || pid != cmd.Process.Pid {
		t.Fatalf("target pid = %q; want %d", out, cmd.Process.Pid)
	}
}

func TestProtectWritesTheProtectedScore(t *testing.T) {
	p := writeScore(t, "0")
	if err := protectFile(p); err != nil {
		t.Fatal(err)
	}
	if got := readScore(t, p); got != "-999" {
		t.Fatalf("score = %q; want -999", got)
	}
}

func TestProtectReportsAnUnwritableFile(t *testing.T) {
	if err := protectFile(filepath.Join(t.TempDir(), "missing", "oom_score_adj")); err == nil {
		t.Fatal("want an error for a missing directory")
	}
}

func TestResetChildDoesNotStartWhenTheScoreCannotBeRead(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "started")
	cmd := exec.Command("/usr/bin/touch", marker)
	wrapReset(cmd, filepath.Join(t.TempDir(), "missing"))
	out, err := cmd.CombinedOutput()
	if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 125 {
		t.Fatalf("exit = %v; want 125, output %s", err, out)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("started the target without resetting inherited protection")
	}
}
