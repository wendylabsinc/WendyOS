//go:build !windows

package commands

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestRunWithTimeout_Success(t *testing.T) {
	out, err := runWithTimeout(5*time.Second, "sh", "-c", "echo ok")
	if err != nil {
		t.Fatalf("runWithTimeout: %v", err)
	}
	if strings.TrimSpace(string(out)) != "ok" {
		t.Fatalf("output = %q, want ok", out)
	}
}

func TestRunWithTimeout_FailureIsNotTimeout(t *testing.T) {
	_, err := runWithTimeout(5*time.Second, "sh", "-c", "exit 3")
	if err == nil || errors.Is(err, errCommandTimedOut) {
		t.Fatalf("err = %v, want a plain exit failure", err)
	}
}

func TestRunWithTimeout_HungCommand(t *testing.T) {
	start := time.Now()
	_, err := runWithTimeout(100*time.Millisecond, "sh", "-c", "sleep 30")
	if !errors.Is(err, errCommandTimedOut) {
		t.Fatalf("err = %v, want errCommandTimedOut", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("timeout took %s", elapsed)
	}
}

// Mirrors `sudo diskutil`: the signalled parent exits but a stuck child keeps
// the output pipe open. The call must still return.
func TestRunWithTimeout_OrphanHoldsPipe(t *testing.T) {
	start := time.Now()
	_, err := runWithTimeout(100*time.Millisecond, "sh", "-c", "sleep 30 & sleep 30")
	if !errors.Is(err, errCommandTimedOut) {
		t.Fatalf("err = %v, want errCommandTimedOut", err)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("orphaned child kept the call alive for %s", elapsed)
	}
}
