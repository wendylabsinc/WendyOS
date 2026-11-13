//go:build darwin || linux || windows

package commands

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
)

func stubTourRetry(t *testing.T, interactive bool, confirm func(context.Context, io.Writer) (bool, error)) {
	t.Helper()
	oldInteractive, oldConfirm := isInteractiveTerminalFn, confirmTourRetry
	t.Cleanup(func() { isInteractiveTerminalFn, confirmTourRetry = oldInteractive, oldConfirm })
	isInteractiveTerminalFn = func() bool { return interactive }
	confirmTourRetry = confirm
}

func TestTourStepRetriesUntilReady(t *testing.T) {
	prompts, attempts := 0, 0
	stubTourRetry(t, true, func(context.Context, io.Writer) (bool, error) {
		prompts++
		return true, nil
	})
	var out bytes.Buffer
	err := runTourStep(context.Background(), &out, "Starting the simulator", func() error {
		attempts++
		if attempts < 3 {
			// A child timeout must not cancel the whole tour.
			return context.DeadlineExceeded
		}
		return nil
	})
	if err != nil || prompts != 2 || attempts != 3 {
		t.Fatalf("error=%v prompts=%d attempts=%d", err, prompts, attempts)
	}
	if !strings.Contains(out.String(), "Starting the simulator needs attention: context deadline exceeded") {
		t.Fatalf("missing explanation: %s", out.String())
	}
}

func TestTourStepDoesNotRetryCancellationOrNoninteractiveFailure(t *testing.T) {
	cause := errors.New("Docker is unavailable")
	for _, tt := range []struct {
		name        string
		interactive bool
		err, want   error
	}{
		{"noninteractive", false, cause, cause},
		{"user cancellation", true, ErrUserCancelled, ErrUserCancelled},
		{"context cancellation", true, context.Canceled, ErrUserCancelled},
		{"prompt cancellation", true, tui.ErrCancelled, ErrUserCancelled},
	} {
		t.Run(tt.name, func(t *testing.T) {
			stubTourRetry(t, tt.interactive, func(context.Context, io.Writer) (bool, error) {
				t.Fatal("unexpected retry prompt")
				return false, nil
			})
			attempts := 0
			err := runTourStep(context.Background(), io.Discard, "Setup", func() error {
				attempts++
				return tt.err
			})
			if !errors.Is(err, tt.want) || attempts != 1 {
				t.Fatalf("error=%v attempts=%d", err, attempts)
			}
		})
	}
}

func TestTourStepRetryPromptExit(t *testing.T) {
	promptFailure := errors.New("terminal unavailable")
	for _, tt := range []struct {
		name      string
		promptErr error
		want      error
	}{
		{"decline", nil, ErrUserCancelled},
		{"cancel", tui.ErrCancelled, ErrUserCancelled},
		{"prompt failure", promptFailure, promptFailure},
	} {
		t.Run(tt.name, func(t *testing.T) {
			stubTourRetry(t, true, func(context.Context, io.Writer) (bool, error) {
				return false, tt.promptErr
			})
			attempts := 0
			err := runTourStep(context.Background(), io.Discard, "Setup", func() error {
				attempts++
				return errors.New("setup failed")
			})
			if !errors.Is(err, tt.want) || attempts != 1 {
				t.Fatalf("error=%v attempts=%d", err, attempts)
			}
		})
	}
}

func TestTourStepChecksContextBeforeWorkAndAfterPrompt(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	stubTourRetry(t, true, func(context.Context, io.Writer) (bool, error) {
		cancel()
		return true, nil
	})
	attempts := 0
	run := func() error { attempts++; return errors.New("not ready") }
	if err := runTourStep(ctx, io.Discard, "Setup", run); !errors.Is(err, context.Canceled) || attempts != 1 {
		t.Fatalf("error=%v attempts=%d", err, attempts)
	}
	if err := runTourStep(ctx, io.Discard, "Setup", run); !errors.Is(err, context.Canceled) || attempts != 1 {
		t.Fatalf("cancelled context started work: error=%v attempts=%d", err, attempts)
	}
}

func TestTourStepReloadsCorrectedProject(t *testing.T) {
	dir := t.TempDir()
	want, path, err := loadTourRobotProject(dir)
	if err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{invalid JSON"), 0600); err != nil {
		t.Fatal(err)
	}
	prompts := 0
	stubTourRetry(t, true, func(context.Context, io.Writer) (bool, error) {
		prompts++
		if err := os.WriteFile(path, original, 0600); err != nil {
			t.Fatal(err)
		}
		return true, nil
	})
	var got tourRobotProject
	var out bytes.Buffer
	err = runTourStep(context.Background(), &out, "Preparing your starter", func() error {
		var err error
		got, _, err = loadTourRobotProject(dir)
		return err
	})
	if err != nil || prompts != 1 || got != want {
		t.Fatalf("error=%v prompts=%d project=%+v, want %+v", err, prompts, got, want)
	}
	if !strings.Contains(out.String(), path) {
		t.Fatalf("missing file to correct: %s", out.String())
	}
}
