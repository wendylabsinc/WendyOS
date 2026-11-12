package tui

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/muesli/termenv"
)

func stepConfirm(t *testing.T, m ConfirmModel, key string) ConfirmModel {
	t.Helper()
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(key)})
	cm, ok := next.(ConfirmModel)
	if !ok {
		t.Fatalf("Update returned %T, want ConfirmModel", next)
	}
	return cm
}

func TestConfirmNoDefault_IgnoresEnter(t *testing.T) {
	m := NewConfirmNoDefault("Install?")

	// Enter must not resolve the prompt when there is no default.
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	cm := next.(ConfirmModel)
	if cm.answered {
		t.Error("Enter resolved a no-default prompt; want it ignored")
	}

	// Arrows/tab must not establish a default either.
	cm = stepConfirm(t, cm, "left")
	if cm.choice {
		t.Error("left arrow set a choice on a no-default prompt")
	}
}

func TestConfirmNoDefault_ResolvesOnKey(t *testing.T) {
	yes := stepConfirm(t, NewConfirmNoDefault("Install?"), "y")
	if !yes.answered || !yes.Confirmed() {
		t.Errorf("after 'y': answered=%v confirmed=%v; want both true", yes.answered, yes.Confirmed())
	}

	no := stepConfirm(t, NewConfirmNoDefault("Install?"), "n")
	if !no.answered || no.Confirmed() {
		t.Errorf("after 'n': answered=%v confirmed=%v; want answered=true confirmed=false", no.answered, no.Confirmed())
	}
	if no.Cancelled() {
		t.Error("'n' should not count as cancelled")
	}
}

func TestConfirmDefaultYes_EnterConfirms(t *testing.T) {
	// Enter with no explicit key resolves to the default (Yes). This is the
	// semantics commands.confirmFn relies on (empty input counts as yes).
	next, _ := NewConfirmDefaultYes("Update?").Update(tea.KeyMsg{Type: tea.KeyEnter})
	cm := next.(ConfirmModel)
	if !cm.answered || !cm.Confirmed() {
		t.Errorf("Enter on default-yes: answered=%v confirmed=%v; want both true", cm.answered, cm.Confirmed())
	}
}

func TestConfirmDefaultNo_EnterDeclines(t *testing.T) {
	// Enter resolves to the default (No). This is the semantics
	// commands.confirmDefaultNoFn relies on (empty input counts as no).
	next, _ := NewConfirm("Continue?").Update(tea.KeyMsg{Type: tea.KeyEnter})
	cm := next.(ConfirmModel)
	if !cm.answered {
		t.Error("Enter on default-no should resolve the prompt")
	}
	if cm.Confirmed() {
		t.Error("Enter on default-no should count as no")
	}
}

func TestConfirmNoDefault_CtrlCCancels(t *testing.T) {
	next, _ := NewConfirmNoDefault("Install?").Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	cm := next.(ConfirmModel)
	if !cm.Cancelled() {
		t.Error("Ctrl+C should cancel the prompt")
	}
	if cm.answered {
		t.Error("Ctrl+C should not mark the prompt answered")
	}
}

func TestConfirmCompactDefaultNo(t *testing.T) {
	for _, tt := range []struct {
		name string
		key  tea.KeyMsg
		yes  bool
	}{
		{name: "enter", key: tea.KeyMsg{Type: tea.KeyEnter}},
		{name: "y", key: tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")}, yes: true},
		{name: "Y", key: tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("Y")}, yes: true},
		{name: "n", key: tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("n")}},
		{name: "N", key: tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("N")}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			next, cmd := NewConfirmDefaultNo("Trust?").Update(tt.key)
			m, ok := next.(ConfirmModel)
			if !ok {
				t.Fatalf("Update returned %T, want ConfirmModel", next)
			}
			if !m.answered || m.Confirmed() != tt.yes || m.Cancelled() {
				t.Fatalf("answered=%v confirmed=%v cancelled=%v; want true, %v, false", m.answered, m.Confirmed(), m.Cancelled(), tt.yes)
			}
			if cmd == nil {
				t.Fatal("answer did not exit the prompt")
			}
			if m.View() != "" {
				t.Fatal("answered prompt should be hidden")
			}
		})
	}
}

func TestConfirmCompactDefaultNoIgnoresNavigation(t *testing.T) {
	for _, key := range []tea.KeyMsg{
		{Type: tea.KeyLeft},
		{Type: tea.KeyRight},
		{Type: tea.KeyTab},
		{Type: tea.KeyRunes, Runes: []rune("h")},
		{Type: tea.KeyRunes, Runes: []rune("l")},
	} {
		t.Run(key.String(), func(t *testing.T) {
			next, _ := NewConfirmDefaultNo("Trust?").Update(key)
			m, ok := next.(ConfirmModel)
			if !ok {
				t.Fatalf("Update returned %T, want ConfirmModel", next)
			}
			next, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
			m, ok = next.(ConfirmModel)
			if !ok {
				t.Fatalf("Update returned %T, want ConfirmModel", next)
			}
			if !m.answered || m.Confirmed() {
				t.Fatal("navigation changed the default No answer")
			}
		})
	}
}

func TestConfirmCompactDefaultNoView(t *testing.T) {
	const question = "Do you want to trust this device anyways?"
	if got, want := NewConfirmDefaultNo(question).View(), question+" [y/N]\n"; got != want {
		t.Fatalf("View() = %q, want %q", got, want)
	}
}

func TestConfirmCompactDefaultNoCancellation(t *testing.T) {
	for _, input := range []string{"\x03", "q"} {
		t.Run(input, func(t *testing.T) {
			confirmed, err := ConfirmDefaultNo("Trust?", tea.WithInput(strings.NewReader(input)), tea.WithOutput(io.Discard), tea.WithoutRenderer())
			if confirmed || !errors.Is(err, ErrCancelled) {
				t.Fatalf("ConfirmDefaultNo() = %v, %v; want false, ErrCancelled", confirmed, err)
			}
		})
	}
}

func TestConfirmShowsTheChoiceWithoutColour(t *testing.T) {
	withColor(t, termenv.Ascii)
	if got, want := NewConfirm("Flash?").View(), "Flash?   Yes  [No]  (y/n)\n"; got != want {
		t.Errorf("default No: %q, want %q", got, want)
	}
	if got, want := NewConfirmDefaultYes("Flash?").View(), "Flash?  [Yes]  No   (y/n)\n"; got != want {
		t.Errorf("default Yes: %q, want %q", got, want)
	}
}

func TestConfirmColouredViewIsUnchanged(t *testing.T) {
	withColor(t, termenv.TrueColor)
	want := fmt.Sprintf("%s  %s %s  %s\n", confirmQuestion.Render("Flash?"),
		confirmActive.Render("Yes"), confirmInactive.Render("No"), confirmHint.Render("(y/n)"))
	if got := NewConfirmDefaultYes("Flash?").View(); got != want || strings.Contains(got, "[Yes]") {
		t.Fatalf("coloured view %q; want %q", got, want)
	}
}
