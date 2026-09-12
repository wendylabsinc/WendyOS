package chat

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
)

func TestSetupChoicesFilterAndScrollToSelection(t *testing.T) {
	choices := make([]setupChoice, 30)
	for i := range choices {
		choices[i] = setupChoice{ID: fmt.Sprintf("model-%02d", i), Label: fmt.Sprintf("Model %02d", i), Detail: "Supports tool calling"}
	}
	m := newSetupChoiceModel("Choose a model", "Select a model already available on your server.", choices)
	m.Update(tea.WindowSizeMsg{Width: 50, Height: 15})
	m.Update(tea.KeyMsg{Type: tea.KeyCtrlEnd})
	if m.selected != 29 || !strings.Contains(ansi.Strip(m.View()), "Model 29") || m.viewport.YOffset == 0 {
		t.Fatal("last selection must scroll into view")
	}
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("MODEL-17")})
	if len(m.filtered) != 1 || m.filtered[0] != 17 || m.selected != 0 {
		t.Fatalf("case-insensitive model search failed: %#v, %d", m.filtered, m.selected)
	}
	m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if !m.done || !strings.Contains(ansi.Strip(m.View()), "Model 17") {
		t.Fatal("Enter should select the matching model and retain a short confirmation")
	}
}

func TestSetupChoicesEmptySearchCanBeEdited(t *testing.T) {
	choices := make([]setupChoice, 9)
	for i := range choices {
		choices[i] = setupChoice{ID: fmt.Sprint(i), Label: fmt.Sprint(i)}
	}
	m := newSetupChoiceModel("Choose", "", choices)
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("absent")})
	m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if m.done || !strings.Contains(ansi.Strip(m.View()), "No matching models") {
		t.Fatal("empty search should remain editable")
	}
	m.Update(tea.KeyMsg{Type: tea.KeyCtrlU})
	if len(m.filtered) != len(choices) {
		t.Fatal("clearing the search should restore all choices")
	}
}

func TestSetupSecretNeverUsesDefaultOrRendersValue(t *testing.T) {
	m := newSetupTextModel("API key", "Paste your key", "existing-secret", true)
	if m.input.Value() != "" || strings.Contains(m.View(), "existing-secret") {
		t.Fatal("secret fields must start empty")
	}
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("new-secret")})
	if strings.Contains(m.View(), "new-secret") || !strings.Contains(ansi.Strip(m.View()), "•") {
		t.Fatal("secret must be masked while editing")
	}
	m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if m.input.Value() != "new-secret" || strings.Contains(m.View(), "new-secret") || strings.Contains(m.View(), "•") {
		t.Fatal("secret confirmation must omit the value and its length")
	}
}

func TestSetupTextAllowsEmptyOptionalCredentialAndCancellationClearsIt(t *testing.T) {
	m := newSetupTextModel("Optional API key", "Enter to skip", "", true)
	m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if !m.done || m.input.Value() != "" {
		t.Fatal("an optional empty credential must be accepted")
	}
	for _, key := range []tea.KeyType{tea.KeyEsc, tea.KeyCtrlC} {
		m := newSetupTextModel("API key", "", "", true)
		m.input.SetValue("secret-to-discard")
		m.Update(tea.KeyMsg{Type: key})
		if !m.cancelled || m.View() != "" || m.input.Value() != "" {
			t.Fatal("canceling must discard the secret and prompt")
		}
	}
}

func TestSetupFramesWrapAndSanitizeUntrustedText(t *testing.T) {
	label := "Device\x1b[2J\u202e"
	description := strings.Repeat("Hardware models available on this server. ", 5)
	choice := newSetupChoiceModel(label, description, []setupChoice{{ID: "1", Label: label, Detail: description}})
	text := newSetupTextModel(label, description, "http://localhost:1234/v1", false)
	work := newSetupWorkModel(label, func() {}, nil, nil)
	work.Update(setupProgressMsg(description + "\x1b]52;c;c2VjcmV0\x07"))
	for _, m := range []tea.Model{choice, text, work} {
		for _, size := range [][2]int{{100, 30}, {40, 20}, {15, 8}, {1, 1}, {0, 0}} {
			m.Update(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
			view := m.View()
			if len(strings.Split(view, "\n")) > max(1, size[1]) {
				t.Errorf("%T overflows terminal rows at %v", m, size)
			}
			for _, line := range strings.Split(view, "\n") {
				if ansi.StringWidth(line) > max(1, size[0]) {
					t.Errorf("%T overflows terminal columns at %v", m, size)
				}
			}
			if strings.Contains(view, "\x1b[2J") || strings.Contains(view, "\u202e") || strings.Contains(view, "\x1b]52") {
				t.Errorf("%T rendered untrusted terminal controls", m)
			}
		}
	}
}

func TestSetupTerminalCancellationReturnsSentinel(t *testing.T) {
	for _, input := range []string{"\x03", "\x1b"} {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		ui := terminalSetupUI{input: strings.NewReader(input), output: &bytes.Buffer{}}
		_, err := ui.Text(ctx, "API key", "", "", true)
		cancel()
		if !errors.Is(err, tui.ErrCancelled) {
			t.Fatalf("input %q: cancellation = %v", input, err)
		}
	}
}

func TestSetupWorkCancellationJoinsWorker(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var finished atomic.Bool
	ui := terminalSetupUI{input: strings.NewReader("\x03"), output: &bytes.Buffer{}}
	err := ui.Work(ctx, "Finding models", func(ctx context.Context, progress func(string)) error {
		for i := 0; i < 1000; i++ {
			progress("Still finding models")
		}
		<-ctx.Done()
		finished.Store(true)
		return ctx.Err()
	})
	if !errors.Is(err, tui.ErrCancelled) || !finished.Load() {
		t.Fatalf("Work returned before canceled worker finished: %v, finished=%v", err, finished.Load())
	}
}

func TestSetupWorkReturnsErrorWithoutRenderingIt(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	output := &bytes.Buffer{}
	ui := terminalSetupUI{input: strings.NewReader(""), output: output}
	want := errors.New("sensitive provider response")
	err := ui.Work(ctx, "Finding models", func(context.Context, func(string)) error { return want })
	if !errors.Is(err, want) || strings.Contains(output.String(), want.Error()) {
		t.Fatalf("provider error should be returned only for guided recovery: %v, output=%q", err, output.String())
	}
}

func TestSetupWorkParentCancellationJoinsWorker(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var finished atomic.Bool
	ui := terminalSetupUI{input: strings.NewReader(""), output: &bytes.Buffer{}}
	err := ui.Work(ctx, "Finding models", func(ctx context.Context, _ func(string)) error {
		cancel()
		<-ctx.Done()
		finished.Store(true)
		return ctx.Err()
	})
	if !errors.Is(err, context.Canceled) || !finished.Load() {
		t.Fatalf("parent cancellation returned before worker finished: %v", err)
	}
}

func TestSetupNoteSanitizesExternalMessages(t *testing.T) {
	output := &bytes.Buffer{}
	terminalSetupUI{output: output}.Note("before\x1b[2Jafter\u202e\nnext")
	if output.String() != "beforeafter\nnext\n" {
		t.Fatalf("unsafe note output: %q", output.String())
	}
}
