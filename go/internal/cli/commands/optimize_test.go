package commands

import (
	"io"
	"os"
	"path/filepath"
	"testing"
)

func writeOptFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

func TestRunOptimizeFindsAndFixes(t *testing.T) {
	dir := t.TempDir()
	writeOptFile(t, dir, "Dockerfile", "FROM rust:1\nRUN cargo build\n")

	rep, _, err := runOptimize(optimizeOptions{Dir: dir})
	if err != nil {
		t.Fatalf("runOptimize: %v", err)
	}
	_, warn, _, fixable := rep.Counts()
	if warn == 0 || fixable == 0 {
		t.Fatalf("expected warnings and fixable findings, got counts: info=?, warn=%d, err=?, fixable=%d", warn, fixable)
	}

	// With --fix, the cache-mount + .dockerignore fixes apply, and a re-run drops fixable count.
	repFixed, applied, err := runOptimize(optimizeOptions{Dir: dir, Fix: true})
	if err != nil {
		t.Fatalf("runOptimize fix: %v", err)
	}
	var anyApplied bool
	for _, a := range applied {
		if a.Applied {
			anyApplied = true
		}
	}
	if !anyApplied {
		t.Fatalf("expected at least one applied fix")
	}
	// Re-running clean should report fewer fixable findings than the first pass.
	_, _, _, fixableAfter := repFixed.Counts()
	if fixableAfter >= fixable {
		t.Fatalf("fixable did not decrease after --fix: before=%d after=%d", fixable, fixableAfter)
	}
}

func TestRunOptimizeNoProject(t *testing.T) {
	dir := t.TempDir()
	rep, _, err := runOptimize(optimizeOptions{Dir: dir})
	if err != nil {
		t.Fatalf("runOptimize: %v", err)
	}
	if len(rep.Findings) != 0 {
		t.Fatalf("expected no findings for empty dir, got %+v", rep.Findings)
	}
}

// optimize used to call os.Exit(2) on bad flags and analysis failures, which
// skipped the CLI's error reporting and collided with the usage-error status
// for plain failures. Bad flags are usage errors; everything else is returned.
func TestOptimizeCommandReturnsErrorsInsteadOfExiting(t *testing.T) {
	t.Chdir(t.TempDir())
	for _, args := range [][]string{
		{"--agentic", "--fix"},
		{"--severity", "bogus"},
	} {
		cmd := newOptimizeCmd()
		cmd.SetArgs(args)
		cmd.SetOut(io.Discard)
		cmd.SetErr(io.Discard)
		err := cmd.Execute()
		if !IsUsageError(err) {
			t.Errorf("optimize %q: err = %v, want a usage error", args, err)
		}
	}
}
