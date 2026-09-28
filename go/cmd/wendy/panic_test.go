package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// An unrecovered panic used to exit 2, the same status as a usage error,
// with Go's trace instead of an envelope.
func TestExecuteRecoveringTurnsAPanicIntoAnInternalError(t *testing.T) {
	executed, err := executeRecovering(func() (*cobra.Command, error) {
		panic("boom")
	})
	if executed != nil {
		t.Errorf("executed = %v, want nil after a panic", executed)
	}
	var internal *internalError
	if !errors.As(err, &internal) {
		t.Fatalf("err = %v, want an internal error", err)
	}
	if got := errorClass(err); got != "internal_error" {
		t.Errorf("errorClass = %q, want internal_error", got)
	}
	if !strings.Contains(string(internal.stack), "panic_test.go") {
		t.Errorf("stack does not show where the panic happened:\n%s", internal.stack)
	}

	// No panic: the result passes through untouched.
	root := &cobra.Command{Use: "wendy"}
	want := errors.New("plain failure")
	if executed, err := executeRecovering(func() (*cobra.Command, error) { return root, want }); executed != root || err != want {
		t.Errorf("executeRecovering = %v, %v; want the command and error unchanged", executed, err)
	}
}

func TestReportFailure_InternalError(t *testing.T) {
	_, err := executeRecovering(func() (*cobra.Command, error) { panic(errors.New("nil map write")) })

	var out strings.Builder
	if code := reportFailure(&out, err, nil, []string{"--json"}); code != 70 {
		t.Errorf("exit = %d, want 70", code)
	}
	// JSON mode is what a non-terminal run gets, so the stack must not be
	// lost there: it comes first, and the envelope stays the last line.
	stack, envelope, ok := cutLastLine(out.String())
	if !ok || !strings.Contains(stack, "goroutine ") || !strings.Contains(stack, "panic_test.go") {
		t.Errorf("JSON mode must print the stack before the envelope:\n%s", out.String())
	}
	got := decodeEnvelope(t, envelope)
	if got.Code != "internal_error" || got.Exit != 70 || got.Retryable ||
		got.Message != "internal error: nil map write" || len(got.NextSteps) != 1 {
		t.Errorf("envelope = %+v", got)
	}

	out.Reset()
	if code := reportFailure(&out, err, nil, []string{"--json=false"}); code != 70 {
		t.Errorf("text-mode exit = %d, want 70", code)
	}
	text := out.String()
	if !strings.Contains(text, "internal error: nil map write") || !strings.Contains(text, "goroutine ") {
		t.Errorf("text mode must show the error and the stack for a bug report:\n%s", text)
	}
}

// cutLastLine splits out into everything before its last line and that last
// line (with its newline); ok is false when out has fewer than two lines.
func cutLastLine(out string) (before, last string, ok bool) {
	trimmed := strings.TrimSuffix(out, "\n")
	i := strings.LastIndex(trimmed, "\n")
	if i < 0 {
		return "", out, false
	}
	return out[:i+1], out[i+1:], true
}
