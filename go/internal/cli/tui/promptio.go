package tui

import (
	"io"
	"os"
	"sync"

	tea "github.com/charmbracelet/bubbletea"
)

var (
	promptMu  sync.RWMutex
	promptOut io.Writer
)

// SetPromptOutput draws prompts on w instead of stdout until restore runs.
// Commands that never call it keep Bubble Tea's default output.
func SetPromptOutput(w io.Writer) (restore func()) {
	promptMu.Lock()
	prev := promptOut
	promptOut = w
	promptMu.Unlock()
	return func() {
		promptMu.Lock()
		promptOut = prev
		promptMu.Unlock()
	}
}

// PromptOutput returns the writer set by SetPromptOutput, if any.
func PromptOutput() (io.Writer, bool) {
	promptMu.RLock()
	defer promptMu.RUnlock()
	return promptOut, promptOut != nil
}

// PromptWriter is where a prompt's own text belongs: the prompt output when
// one is set, else stdout.
func PromptWriter() io.Writer {
	if w, ok := PromptOutput(); ok {
		return w
	}
	return os.Stdout
}

// NewProgram is tea.NewProgram drawing on the prompt output when one is set.
// Caller options come after it, so an explicit tea.WithOutput still wins.
func NewProgram(m tea.Model, opts ...tea.ProgramOption) *tea.Program {
	if w, ok := PromptOutput(); ok {
		opts = append([]tea.ProgramOption{tea.WithOutput(w)}, opts...)
	}
	return tea.NewProgram(m, opts...)
}
