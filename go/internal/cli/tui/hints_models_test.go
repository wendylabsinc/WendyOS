package tui

import (
	"slices"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

const hintMarker = "💡"

func TestSpinnerShowsHintWhileRunning(t *testing.T) {
	m := NewSpinner("Building...")
	if !strings.Contains(m.View(), hintMarker) {
		t.Fatal("expected running spinner View to include a hint")
	}
}

func TestSpinnerHidesHintWhenDone(t *testing.T) {
	m := NewSpinner("Building...")
	updated, _ := m.Update(SpinnerDoneMsg{})
	if strings.Contains(updated.View(), hintMarker) {
		t.Fatal("expected completed spinner View to omit the hint")
	}
}

func TestProgressShowsHintWhileRunning(t *testing.T) {
	m := NewProgress("Downloading...")
	if !strings.Contains(m.View(), hintMarker) {
		t.Fatal("expected running progress View to include a hint")
	}
}

func TestProgressHidesHintWhenDone(t *testing.T) {
	m := NewProgress("Downloading...")
	updated, _ := m.Update(ProgressDoneMsg{})
	if strings.Contains(updated.View(), hintMarker) {
		t.Fatal("expected completed progress View to omit the hint")
	}
}

func TestMultiSpinnerShowsHintWhileRunning(t *testing.T) {
	m := NewMultiSpinner("Building 2 services...", []string{"api", "web"})
	if !strings.Contains(m.View(), hintMarker) {
		t.Fatal("expected running multispinner View to include a hint")
	}
}

func TestMultiSpinnerHidesHintWhenDone(t *testing.T) {
	m := NewMultiSpinner("Building 2 services...", []string{"api", "web"})
	updated, _ := m.Update(MultiSpinnerAllDoneMsg{})
	if strings.Contains(updated.View(), hintMarker) {
		t.Fatal("expected completed multispinner View to omit the hint")
	}
}

func TestWithoutHintsShowsNoTipAndStopsRotating(t *testing.T) {
	// Simulator setup showed "Tip: Scaffold a new project ... wendy init",
	// which has nothing to do with booting a VM.
	models := map[string]interface {
		View() string
		Update(tea.Msg) (tea.Model, tea.Cmd)
	}{
		"spinner":      NewSpinner("Booting...").WithoutHints(),
		"progress":     NewProgress("Sending...").WithoutHints(),
		"build steps":  NewBuildStepsModel("Building...").WithoutHints(),
		"multispinner": NewMultiSpinner("Building 2...", []string{"api", "web"}).WithoutHints(),
	}
	for name, m := range models {
		if strings.Contains(m.View(), hintMarker) {
			t.Errorf("%s: WithoutHints still renders a tip: %q", name, m.View())
		}
		updated, cmd := m.Update(hintTickMsg{})
		if cmd != nil {
			t.Errorf("%s: a hint tick rescheduled rotation with nothing to rotate", name)
		}
		if strings.Contains(updated.View(), hintMarker) {
			t.Errorf("%s: a tick brought a tip back", name)
		}
	}
}

func TestHintsNeverNameTheRunningCommand(t *testing.T) {
	t.Cleanup(func() { SetRunningCommand("") })
	const deviceInfoTip = "'wendy device info'"
	SetRunningCommand("wendy device info")
	for range 50 {
		for _, hint := range newHintRotator().hints {
			if strings.Contains(hint, deviceInfoTip) {
				t.Fatalf("tip %q recommends the running command", hint)
			}
		}
	}
	// Only the whole command: a variant is still worth knowing.
	kept := hintsBesides(ProgressHints, "wendy run")
	if !slices.ContainsFunc(kept, func(h string) bool { return strings.Contains(h, "'wendy run --watch'") }) {
		t.Error("the 'wendy run --watch' tip was dropped during a plain 'wendy run'")
	}
	if got := hintsBesides(ProgressHints, ""); len(got) != len(ProgressHints) {
		t.Errorf("no running command dropped %d tips", len(ProgressHints)-len(got))
	}
}

func TestHintTickRotatesAndReschedules(t *testing.T) {
	m := NewSpinner("Building...")
	updated, cmd := m.Update(hintTickMsg{})
	// A non-nil command is returned to reschedule the next rotation tick.
	// (We don't invoke it: tea.Tick blocks for the full interval.)
	if cmd == nil {
		t.Fatal("expected hintTickMsg handling to reschedule another tick")
	}
	if !strings.Contains(updated.View(), hintMarker) {
		t.Fatal("expected spinner to still show a hint after a tick")
	}
}
