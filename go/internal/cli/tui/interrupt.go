package tui

import (
	"io"
	"os"

	tea "github.com/charmbracelet/bubbletea"
)

// InterruptFilter routes Bubble Tea's InterruptMsg (raised by a SIGINT signal or
// an explicitly sent interrupt) through the model's normal Ctrl+C handling.
//
// Bubble Tea's default interrupt handling is a "kill": it returns from the event
// loop with ErrInterrupted *without* rendering the model's final View, and its
// renderer only erases the bottom line on the way out. For a multi-line progress
// frame — a spinner/bar plus the rotating hint line — that leaves everything
// above the bottom line, including the hint, on screen. Translating the
// interrupt into a Ctrl+C key press lets the model run its existing graceful
// quit path (set its done/quitting state, return tea.Quit), so Bubble Tea draws
// the cleared final frame and the hint does not linger.
func InterruptFilter(_ tea.Model, msg tea.Msg) tea.Msg {
	if _, ok := msg.(tea.InterruptMsg); ok {
		return tea.KeyMsg{Type: tea.KeyCtrlC}
	}
	return msg
}

// NewProgressProgram builds a tea.Program for one of the progress models
// (SpinnerModel, ProgressModel, MultiSpinnerModel, BuildStepsModel) with
// InterruptFilter installed so the hint is cleared on interrupt instead of
// lingering. It also defaults the program's output to stderr, since these
// models render transient progress UI rather than a command's actual result,
// which stays on stdout for piping/redirection. Use it instead of
// tea.NewProgram when running those models.
func NewProgressProgram(model tea.Model, opts ...tea.ProgramOption) *tea.Program {
	// A set prompt output is the terminal the person is watching; stderr may be piped.
	var out io.Writer = os.Stderr
	if w, ok := PromptOutput(); ok {
		out = w
	}
	// Defaults first so explicit caller opts can override them.
	return tea.NewProgram(model,
		append([]tea.ProgramOption{tea.WithOutput(out), tea.WithFilter(InterruptFilter)}, opts...)...)
}
