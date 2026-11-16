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

func assertTourCommandsSurviveLiveRenderer(t *testing.T, wizard tourWizardModel, wantPhase tourPhase) {
	t.Helper()
	width := wizard.width
	frame := wizard.View()
	command := "wendy auth login"
	if wizard.phase == phaseAptInstall {
		command = linuxDesktopCommand("", "", linuxDesktopMachineLabel)
	}
	output := &tourProgramOutput{command: command, fullscreen: make(chan struct{}, 1)}
	ready := make(chan struct{})
	model := tourObservedCommandModel{tourCommandScreenModel: tourCommandScreenModel{tourWizardModel: wizard}, ready: ready, once: &sync.Once{}}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	program := tea.NewProgram(model, tea.WithInput(strings.NewReader("")), tea.WithOutput(output), tea.WithAltScreen(), tea.WithContext(ctx), tea.WithoutSignalHandler())
	driverDone := make(chan struct{})
	go func() {
		defer close(driverDone)
		program.Send(tea.WindowSizeMsg{Width: width, Height: 80})
		if wizard.phase != phaseWelcome {
			program.Send(key("enter"))
		} // rapid continuation while output may be pending
		program.Send(tea.WindowSizeMsg{Width: 100, Height: 80})
		program.Send(tea.WindowSizeMsg{Width: width, Height: 80})
		select {
		case <-ready:
		case <-ctx.Done():
			return
		}
		if wizard.phase == phaseWelcome {
			program.Send(tea.KeyMsg{Type: tea.KeyCtrlC})
		} else {
			program.Send(key("enter"))
			if wizard.phase == phaseAptInstall {
				select {
				case <-output.fullscreen:
				case <-ctx.Done():
					return
				}
				program.Send(tea.KeyMsg{Type: tea.KeyCtrlC})
			}
		}
	}()
	final, err := program.Run()
	cancel()
	<-driverDone
	if err != nil {
		t.Fatalf("width %d: live program: %v; output=%q", width, err, output.String())
	}
	if final.(tourObservedCommandModel).phase != wantPhase {
		t.Fatal("continuation changed its destination")
	}
	if !strings.Contains(output.String(), frame) {
		t.Fatalf("width %d: actual renderer did not emit the complete frame", width)
	}
	for _, line := range strings.Split(wizard.viewCommands(), "\n") {
		if strings.HasPrefix(line, "curl ") || strings.HasPrefix(line, "echo ") || strings.HasPrefix(line, "sudo ") || strings.HasPrefix(line, "wendy ") {
			if strings.Count(output.String(), line) != 1 {
				t.Fatalf("command cropped or duplicated after rapid Enter/resize: %q", line)
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

func TestTourLinuxCommandsSurviveLiveRenderer(t *testing.T) {
	for _, width := range []int{40, 100} {
		wizard := newTourWizardModel()
		wizard.phase, wizard.width = phaseAptInstall, width
		assertTourCommandsSurviveLiveRenderer(t, wizard, phaseEnterHostname)
	}
}

func TestTourCompletionCommandsSurviveLiveRenderer(t *testing.T) {
	for _, width := range []int{40, 100} {
		for _, project := range []string{"", "/tmp/long sample project 'with quotes' and spaces"} {
			wizard := newTourWizardModel()
			wizard.phase, wizard.width, wizard.projectPath, wizard.foundAddr = phaseCloud, width, project, "edge.local"
			assertTourCommandsSurviveLiveRenderer(t, wizard, phaseCloud)
		}
	}
}

func TestTourDiagnosticsSurviveLiveRenderer(t *testing.T) {
	for _, width := range []int{40, 100} {
		wizard := newTourWizardModel()
		wizard.phase, wizard.width, wizard.targetName = phaseError, width, "edge"
		wizard.hostname = "192.0.2.10"
		wizard.discoveryStarted = time.Now().Add(-3 * time.Minute)
		discovery := wizard.cmdDiscoveryCheck()().(tourDiscoveryErrorMsg)
		wizard.err = discovery.err
		assertTourCommandsSurviveLiveRenderer(t, wizard, phaseError)
		ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
		wizard.ctx, wizard.foundAddr, wizard.projectPath = ctx, "edge.local", "/tmp/long sample path 'with quotes'"
		readiness := wizard.cmdSampleReadiness()().(tourSampleReadyMsg)
		cancel()
		wizard.err = readiness.err
		assertTourCommandsSurviveLiveRenderer(t, wizard, phaseError)
		wizard.err = errors.New("transport refused with a diagnostic longer than the narrow screen\nprintf '%s\\n' 'complete literal diagnostic command with long arguments'")
		assertTourCommandsSurviveLiveRenderer(t, wizard, phaseError)
	}
}

func TestTourWelcomeSurvivesLiveRenderer(t *testing.T) {
	for _, width := range []int{40, 100} {
		wizard := newTourWizardModel()
		wizard.width = width
		assertTourCommandsSurviveLiveRenderer(t, wizard, phaseWelcome)
	}
}

func TestTourCommandOutputPreservesCancellationAndCause(t *testing.T) {
	for _, key := range []tea.KeyMsg{key("q"), {Type: tea.KeyCtrlC}} {
		m := tourCommandScreenModel{tourWizardModel: newTourWizardModel(), pending: true}
		updated, cmd := m.Update(key)
		if cmd == nil || !errors.Is(tourModelResult(updated.(tourCommandScreenModel).tourWizardModel), ErrUserCancelled) {
			t.Fatal("pending output swallowed cancellation")
		}
	}
	original, writeFailure := errors.New("original boot failure"), errors.New("terminal write failed")
	wizard := newTourWizardModel()
	wizard.phase, wizard.err = phaseError, original
	m := tourCommandScreenModel{tourWizardModel: wizard, pending: true}
	updated, cmd := m.Update(tourCommandOutputDoneMsg{phase: phaseError, err: writeFailure})
	result := tourModelResult(updated.(tourCommandScreenModel).tourWizardModel)
	if cmd == nil || !errors.Is(result, original) || !errors.Is(result, writeFailure) {
		t.Fatal("output failure lost original diagnostic")
	}
	updated, cmd = m.Update(tourCommandOutputDoneMsg{phase: phaseCloud})
	if cmd != nil || updated.(tourCommandScreenModel).displayed {
		t.Fatal("stale callback changed current screen")
	}
}
