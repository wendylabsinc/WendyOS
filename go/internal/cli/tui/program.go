package tui

import (
	tea "github.com/charmbracelet/bubbletea"
	"github.com/wendylabsinc/wendy/go/internal/shared/terminaloutput"
)

// RunProgram gives the renderer exclusive terminal ownership until Run has
// restored terminal state, including on cancellation, errors and panics.
// Background diagnostics must use terminaloutput.Writer or the default logger;
// user-visible progress and errors belong in the model's View.
func RunProgram(p *tea.Program) (tea.Model, error) {
	release := terminaloutput.Suspend()
	defer release()
	return p.Run()
}
