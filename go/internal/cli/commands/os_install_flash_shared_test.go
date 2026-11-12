//go:build darwin || linux || windows

package commands

import (
	"io"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
)

func stubStdoutTerminal(t *testing.T, tty bool) {
	t.Helper()
	orig := stdoutIsTerminalFn
	t.Cleanup(func() { stdoutIsTerminalFn = orig })
	stdoutIsTerminalFn = func() bool { return tty }
}

func TestFlashStepsUIOnlyWhenStdoutIsATerminal(t *testing.T) {
	for _, tc := range []struct {
		interactive, stdoutTTY, want bool
	}{
		{true, true, true},
		// `wendy install | tee log` typed at a terminal: the session asks
		// questions, but the log is kept for the step output.
		{true, false, false},
		{false, true, false},
		{false, false, false},
	} {
		setInteractive(t, tc.interactive)
		stubStdoutTerminal(t, tc.stdoutTTY)
		if got := flashStepsUI(); got != tc.want {
			t.Errorf("interactive=%v stdout terminal=%v: steps UI %v; want %v", tc.interactive, tc.stdoutTTY, got, tc.want)
		}
	}
}

func TestRunFlashStepsPipedStdoutKeepsTheStepStream(t *testing.T) {
	setInteractive(t, true)
	stubStdoutTerminal(t, false)
	var terminal lockedBuffer
	t.Cleanup(tui.SetPromptOutput(&terminal))

	steps := []flashStep{{id: stepStage1, label: "Stage 1", run: func(out io.Writer, _ func(string)) (bool, error) {
		_, err := io.WriteString(out, "stage one output\n")
		return false, err
	}}}
	out, err := captureCommandStdout(t, func() error {
		_, err := runFlashSteps("Flashing", steps, func() {}, io.Discard)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Flashing", "==> Stage 1", "stage one output"} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout %q lacks %q", out, want)
		}
	}
	if terminal.String() != "" {
		t.Errorf("drew %q on the terminal instead of the plain step stream", terminal.String())
	}
}

func TestRunFlashStepPlainAbortWarningReachesThePromptTerminal(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("os.Interrupt cannot be sent to a process on Windows")
	}
	var terminal lockedBuffer
	t.Cleanup(tui.SetPromptOutput(&terminal))
	const warning = "Recovery data was handed over. Press ctrl+c again to abort anyway."

	cancelled := false
	step := flashStep{id: stepStage2, label: "Stage 2", abortWarning: warning, run: func(io.Writer, func(string)) (bool, error) {
		self, err := os.FindProcess(os.Getpid())
		if err != nil {
			return false, err
		}
		// The runner is listening for the interrupt while this step runs.
		if err := self.Signal(os.Interrupt); err != nil {
			return false, err
		}
		for deadline := time.Now().Add(5 * time.Second); !strings.Contains(terminal.String(), warning); {
			if time.Now().After(deadline) {
				t.Error("the abort warning never reached the prompt terminal")
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		return false, nil
	}}
	if _, err := runFlashStepPlain(step, io.Discard, func() { cancelled = true }); err != nil || cancelled {
		t.Fatalf("one ctrl+c: err %v, cancelled %v; want the step to finish", err, cancelled)
	}
}
