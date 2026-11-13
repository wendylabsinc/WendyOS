//go:build darwin || linux || windows

package commands

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// Observe supported screen-mode output without replacing Bubble Tea's renderer.
type tourProgramOutput struct {
	mu         sync.Mutex
	text       strings.Builder
	command    string
	fullscreen chan struct{}
}

func (w *tourProgramOutput) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	n, err := w.text.Write(p)
	// This assertion observes EnterAltScreen's emitted sequence, not a custom
	// escape injected into the application or its displayed command lines.
	if strings.Contains(w.text.String(), w.command) && strings.Contains(string(p), "\x1b[?1049h") {
		select {
		case w.fullscreen <- struct{}{}:
		default:
		}
	}
	return n, err
}

func (w *tourProgramOutput) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.text.String()
}

type tourObservedCommandModel struct {
	tourCommandScreenModel
	ready chan struct{}
	once  *sync.Once
}

func (m tourObservedCommandModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	updated, cmd := m.tourCommandScreenModel.Update(msg)
	m.tourCommandScreenModel = updated.(tourCommandScreenModel)
	if m.displayed {
		m.once.Do(func() { close(m.ready) })
	}
	return m, cmd
}

func TestTourLinuxCommandsSurviveLiveRenderer(t *testing.T) {
	for _, width := range []int{40, 100} {
		wizard := newTourWizardModel()
		wizard.phase, wizard.width = phaseAptInstall, width
		frame := wizard.View()
		command := linuxDesktopCommand("", "", linuxDesktopMachineLabel)
		output := &tourProgramOutput{command: command, fullscreen: make(chan struct{}, 1)}
		ready := make(chan struct{})
		model := tourObservedCommandModel{tourCommandScreenModel: tourCommandScreenModel{tourWizardModel: wizard}, ready: ready, once: &sync.Once{}}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		program := tea.NewProgram(model, tea.WithInput(strings.NewReader("")), tea.WithOutput(output), tea.WithAltScreen(), tea.WithContext(ctx), tea.WithoutSignalHandler())
		driverDone := make(chan struct{})
		go func() {
			defer close(driverDone)
			program.Send(tea.WindowSizeMsg{Width: width, Height: 80})
			program.Send(key("enter")) // rapid continuation while output may be pending
			program.Send(tea.WindowSizeMsg{Width: 100, Height: 80})
			program.Send(tea.WindowSizeMsg{Width: width, Height: 80})
			select {
			case <-ready:
			case <-ctx.Done():
				return
			}
			program.Send(key("enter"))
			select {
			case <-output.fullscreen:
			case <-ctx.Done():
				return
			}
			program.Send(tea.KeyMsg{Type: tea.KeyCtrlC})
		}()
		final, err := program.Run()
		cancel()
		<-driverDone
		if err != nil {
			t.Fatalf("width %d: live program: %v; output=%q", width, err, output.String())
		}
		if final.(tourObservedCommandModel).phase != phaseEnterHostname {
			t.Fatal("continuation no longer enters the hostname screen")
		}
		if !strings.Contains(output.String(), frame) {
			t.Fatalf("width %d: actual renderer did not emit the complete frame", width)
		}
		for _, line := range strings.Split(wizard.viewCommands(), "\n") {
			if strings.HasPrefix(line, "curl ") || strings.HasPrefix(line, "echo ") || strings.HasPrefix(line, "sudo ") {
				if strings.Count(output.String(), line) != 1 {
					t.Fatalf("command cropped or duplicated after rapid Enter/resize: %q", line)
				}
			}
		}
	}
}

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
