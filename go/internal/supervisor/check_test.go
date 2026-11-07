package supervisor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// thisBinary is a real ELF for the test's own architecture.
func thisBinary(t *testing.T) (path, sum string) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(exe)
	if err != nil {
		t.Fatal(err)
	}
	h := sha256.Sum256(data)
	return exe, hex.EncodeToString(h[:])
}

func printsVersion(v string) VersionRunner {
	return func(context.Context, string) (string, error) { return v + "\n", nil }
}

func TestCheckAcceptsAGoodAgent(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the test binary is an ELF only on Linux")
	}
	path, sum := thisBinary(t)
	v, err := Check(context.Background(), path, Want{SHA256: sum, Arch: runtime.GOARCH}, printsVersion("2026.11.02-101010"))
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if v != "2026.11.02-101010" {
		t.Fatalf("version = %q", v)
	}
}

func TestCheckRejects(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the test binary is an ELF only on Linux")
	}
	path, sum := thisBinary(t)
	text := filepath.Join(t.TempDir(), "agent")
	if err := os.WriteFile(text, []byte("#!/bin/sh\necho hi\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	textSum := sha256.Sum256([]byte("#!/bin/sh\necho hi\n"))
	otherArch := "arm64"
	if runtime.GOARCH == "arm64" {
		otherArch = "amd64"
	}

	cases := []struct {
		name    string
		path    string
		want    Want
		run     VersionRunner
		errPart string
	}{
		{"missing sha256", path, Want{Arch: runtime.GOARCH}, printsVersion("dev"), "sha256"},
		{"sha256 mismatch", path, Want{SHA256: strings.Repeat("0", 64), Arch: runtime.GOARCH}, printsVersion("dev"), "sha256"},
		{"not an ELF", text, Want{SHA256: hex.EncodeToString(textSum[:]), Arch: runtime.GOARCH}, printsVersion("dev"), "ELF"},
		{"wrong architecture", path, Want{SHA256: sum, Arch: otherArch}, printsVersion("dev"), "architecture"},
		{"does not start", path, Want{SHA256: sum, Arch: runtime.GOARCH},
			func(context.Context, string) (string, error) { return "", errors.New("exit status 127") }, "--version"},
		{"prints no version", path, Want{SHA256: sum, Arch: runtime.GOARCH}, printsVersion("  "), "--version"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Check(context.Background(), c.path, c.want, c.run)
			if err == nil || !strings.Contains(err.Error(), c.errPart) {
				t.Fatalf("want an error mentioning %q, got %v", c.errPart, err)
			}
		})
	}
}

func TestCheckRefusesAProgramThatIsNotASupervisedAgent(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("needs a system ELF")
	}
	path, err := exec.LookPath("true")
	if err != nil {
		t.Skip("no true(1) on this host")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	h := sha256.Sum256(data)
	ran := false
	run := func(context.Context, string) (string, error) { ran = true; return "1.0\n", nil }
	_, err = Check(context.Background(), path, Want{SHA256: hex.EncodeToString(h[:]), Arch: runtime.GOARCH}, run)
	if err == nil || !strings.Contains(err.Error(), "supervisor") {
		t.Fatalf("want a refusal naming the supervisor, got %v", err)
	}
	if ran {
		t.Fatal("a binary without the contract must never be run, not even for --version")
	}
}
