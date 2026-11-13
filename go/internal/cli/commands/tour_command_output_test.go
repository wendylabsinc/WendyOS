//go:build darwin || linux || windows

package commands

import (
	"errors"
	"io"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func TestTourCommandOutputGuard(t *testing.T) {
	m := tourCommandScreenModel{tourWizardModel: newTourWizardModel()}
	updated, cmd := m.Update(tea.WindowSizeMsg{Width: 40, Height: 80})
	m = updated.(tourCommandScreenModel)
	if cmd == nil || !m.pending || m.displayed {
		t.Fatal("first size message must schedule output")
	}
	updated, cmd = m.Update(key("enter"))
	m = updated.(tourCommandScreenModel)
	if m.phase != phaseWelcome || cmd != nil {
		t.Fatal("early Enter advanced before output completed")
	}
	updated, cmd = m.Update(tea.WindowSizeMsg{Width: 100, Height: 80})
	m = updated.(tourCommandScreenModel)
	if cmd != nil {
		t.Fatal("resize duplicated pending output")
	}
	updated, _ = m.Update(tourCommandOutputDoneMsg{phase: phaseWelcome})
	m = updated.(tourCommandScreenModel)
	updated, cmd = m.Update(tea.WindowSizeMsg{Width: 40, Height: 80})
	m = updated.(tourCommandScreenModel)
	if cmd != nil || !m.displayed {
		t.Fatal("resize duplicated completed output")
	}
	updated, cmd = m.Update(key("enter"))
	m = updated.(tourCommandScreenModel)
	if m.phase != phaseLoadDevices || cmd == nil || m.pending || m.displayed {
		t.Fatal("normal continuation changed after output")
	}
}

type tourOutputFailWriter struct{ err error }

func (w tourOutputFailWriter) Write([]byte) (int, error) { return 0, w.err }

func TestTourCommandOutputFailure(t *testing.T) {
	cause := errors.New("terminal write failed")
	output := &tourCommandOutput{text: "instructions\n"}
	output.SetStdout(tourOutputFailWriter{err: cause})
	if !errors.Is(output.Run(), cause) {
		t.Fatal("write failure lost")
	}
	output.SetStdout(tourOutputFailWriter{})
	if !errors.Is(output.Run(), io.ErrShortWrite) {
		t.Fatal("short write was reported as success")
	}
	m := tourCommandScreenModel{tourWizardModel: newTourWizardModel(), pending: true}
	updated, cmd := m.Update(tourCommandOutputDoneMsg{phase: phaseWelcome, err: cause})
	failed := updated.(tourCommandScreenModel)
	if failed.displayed || cmd == nil || !errors.Is(tourModelResult(failed.tourWizardModel), cause) {
		t.Fatal("failed output allowed continuation or lost its cause")
	}
	var text strings.Builder
	output.SetStdout(&text)
	if err := output.Run(); err != nil || text.String() != "\ninstructions\n" {
		t.Fatal("output did not use supplied stdout")
	}
}
