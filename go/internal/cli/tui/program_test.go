package tui

import (
	"bytes"
	"context"
	"io"
	"log"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/wendylabsinc/wendy/go/internal/shared/terminaloutput"
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

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

type idleModel struct{}

func (idleModel) Init() tea.Cmd                       { return nil }
func (idleModel) Update(tea.Msg) (tea.Model, tea.Cmd) { return idleModel{}, nil }
func (idleModel) View() string                        { return "waiting for a question" }

func TestRunInlineProgramPrintsDiagnosticsWhileRunning(t *testing.T) {
	var diagnostics bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&diagnostics)
	t.Cleanup(func() { log.SetOutput(previous) })
	var screen lockedBuffer
	p := tea.NewProgram(idleModel{}, tea.WithInput(nil), tea.WithOutput(&screen))
	done := make(chan error, 1)
	go func() {
		_, err := RunInlineProgram(p)
		done <- err
	}()
	stopped := false
	stop := func() error {
		if stopped {
			return nil
		}
		stopped = true
		p.Quit()
		return <-done
	}
	t.Cleanup(func() { _ = stop() })
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(screen.String(), "waiting for a question") {
		if time.Now().After(deadline) {
			t.Fatal("program never rendered")
		}
		time.Sleep(10 * time.Millisecond)
	}
	// The default logger points at diagnostics, not os.Stderr, so log.Print
	// stays deferred; terminaloutput.Stderr is the inline path.
	log.Print("deferred logger line")
	for !strings.Contains(screen.String(), "TLS issuer hints paused") {
		_, _ = terminaloutput.Stderr.Write([]byte("TLS issuer hints paused\n"))
		if time.Now().After(deadline) {
			t.Fatalf("diagnostic never printed above the running program: %q", screen.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
	if diagnostics.Len() != 0 {
		t.Fatal("non-stderr logger output escaped while the program ran")
	}
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(diagnostics.String(), "deferred logger line") || log.Writer() != &diagnostics {
		t.Fatal("logger output lost or logger not restored")
	}
}
