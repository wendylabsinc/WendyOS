package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestOptimizeFindingsPreserveProcessOutputAndExit(t *testing.T) {
	for _, jsonMode := range []string{"true", "false"} {
		t.Run("json="+jsonMode, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM rust:1\nRUN cargo build\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(os.Args[0], "--json="+jsonMode, "project", "optimize", "--severity", "warning")
			cmd.Dir = dir
			cmd.Env = append(childEnv(t.TempDir(), t.TempDir()), "WENDY_NO_UPDATE_CHECK=1")
			var stdout, stderr bytes.Buffer
			cmd.Stdout, cmd.Stderr = &stdout, &stderr
			err := cmd.Run()
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
				t.Fatalf("optimize exit = %v; stdout=%q, stderr=%q", err, stdout.String(), stderr.String())
			}
			if stderr.Len() != 0 {
				t.Fatalf("report failure added stderr: %q", stderr.String())
			}
			if jsonMode == "true" {
				var report struct {
					Findings []json.RawMessage `json:"findings"`
				}
				if err := json.Unmarshal(stdout.Bytes(), &report); err != nil || len(report.Findings) == 0 {
					t.Fatalf("missing JSON report: %v; stdout=%q", err, stdout.String())
				}
			} else if !strings.Contains(stdout.String(), "cargo build") || !strings.Contains(stdout.String(), "findings") {
				t.Fatalf("missing text report: %q", stdout.String())
			}
		})
	}
}
