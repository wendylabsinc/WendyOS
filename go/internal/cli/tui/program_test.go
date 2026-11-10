package tui

import (
	"bytes"
	"context"
	"io"
	"log"
	"log/slog"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

type diagnosticModel struct {
	log     *bytes.Buffer
	escaped *bool
}

func (m diagnosticModel) Init() tea.Cmd {
	return func() tea.Msg {
		slog.Warn("TLS issuer hints paused after issuer path failure")
		*m.escaped = m.log.Len() != 0
		return tea.Quit()
	}
}
func (m diagnosticModel) Update(tea.Msg) (tea.Model, tea.Cmd) { return m, nil }
func (m diagnosticModel) View() string                        { return "picker" }

func TestRunProgramDefersTLSWarningAndRestoresLogger(t *testing.T) {
	var diagnostics bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&diagnostics)
	t.Cleanup(func() { log.SetOutput(previous) })
	escaped := false
	p := tea.NewProgram(diagnosticModel{&diagnostics, &escaped}, tea.WithInput(nil), tea.WithOutput(io.Discard))
	if _, err := RunProgram(p); err != nil {
		t.Fatal(err)
	}
	if escaped {
		t.Fatal("TLS warning was printed while the renderer owned the terminal")
	}
	if !strings.Contains(diagnostics.String(), "TLS issuer hints paused") {
		t.Fatal("deferred warning was lost")
	}
	if log.Writer() != &diagnostics {
		t.Fatal("logger not restored after quit")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	p = tea.NewProgram(quitOnceModel{}, tea.WithContext(ctx), tea.WithInput(nil), tea.WithOutput(io.Discard))
	if _, err := RunProgram(p); err == nil {
		t.Fatal("expected cancellation")
	}
	if log.Writer() != &diagnostics {
		t.Fatal("logger not restored after cancellation")
	}
}
