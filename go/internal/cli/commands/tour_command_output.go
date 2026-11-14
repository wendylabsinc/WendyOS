//go:build darwin || linux || windows

package commands

import (
	"errors"
	"fmt"
	"io"

	tea "github.com/charmbracelet/bubbletea"
)

// Bubble Tea truncates View lines to the window width, even without a border.
// These screens use its terminal handoff to print the complete instructions.
func tourHasCommandOutput(phase tourPhase) bool {
	return phase == phaseWelcome || phase == phaseAptInstall || phase == phaseCloud || phase == phaseError
}

type tourCommandOutputDoneMsg struct {
	phase tourPhase
	err   error
}

type tourCommandOutput struct {
	text   string
	stdout io.Writer
}

func (o *tourCommandOutput) SetStdin(io.Reader)    {}
func (o *tourCommandOutput) SetStderr(io.Writer)   {}
func (o *tourCommandOutput) SetStdout(w io.Writer) { o.stdout = w }
func (o *tourCommandOutput) Run() error {
	text := "\n" + o.text
	n, err := io.WriteString(o.stdout, text)
	if err == nil && n != len(text) {
		err = io.ErrShortWrite
	}
	return err
}

// Keep terminal-output bookkeeping outside the wizard's navigation logic.
// Exec prints instructions; it does not execute the displayed shell commands.
type tourCommandScreenModel struct {
	tourWizardModel
	pending   bool
	displayed bool
}

func (m tourCommandScreenModel) View() string {
	if tourHasCommandOutput(m.phase) {
		if m.phase == phaseCloud || m.phase == phaseError {
			return wizHintStyle.Render("Press any key to exit")
		}
		if m.phase == phaseAptInstall {
			return wizHintStyle.Render("Enter to continue")
		}
		return wizHintStyle.Render("Press Enter to begin")
	}
	return m.tourWizardModel.View()
}

func (m tourCommandScreenModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if done, ok := msg.(tourCommandOutputDoneMsg); ok {
		if done.phase != m.phase || !m.pending {
			return m, nil
		}
		m.pending = false
		if done.err != nil {
			m.err = errors.Join(m.err, fmt.Errorf("printing tour instructions: %w", done.err))
			return m, tea.Quit
		}
		m.displayed = true
		return m, nil
	}
	if key, ok := msg.(tea.KeyMsg); ok && tourHasCommandOutput(m.phase) && !m.displayed {
		// Cancellation remains available while output is pending. Ignore an
		// early continuation rather than hiding instructions before they print.
		if key.Type != tea.KeyCtrlC && key.String() != "q" {
			return m, nil
		}
	}
	previous := m.phase
	updated, cmd := m.tourWizardModel.Update(msg)
	wizard, ok := updated.(tourWizardModel)
	if !ok {
		return updated, cmd
	}
	m.tourWizardModel = wizard
	if m.phase != previous {
		m.pending, m.displayed = false, false
	}
	if tourHasCommandOutput(m.phase) {
		if !m.pending && !m.displayed && m.width > 0 {
			m.pending = true
			phase := m.phase
			return m, tea.Sequence(tea.ExitAltScreen,
				tea.Exec(&tourCommandOutput{text: m.tourWizardModel.View()}, func(err error) tea.Msg {
					return tourCommandOutputDoneMsg{phase: phase, err: err}
				}), cmd)
		}
	} else if tourHasCommandOutput(previous) && m.phase != phaseDone {
		return m, tea.Sequence(tea.EnterAltScreen, cmd)
	}
	return m, cmd
}
