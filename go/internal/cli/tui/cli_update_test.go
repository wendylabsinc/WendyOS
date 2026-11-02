package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

func TestCLIUpdateDialogRequiresDecision(t *testing.T) {
	initial := cliUpdateModel{current: "2026.09.29-120000", latest: "2026.10.05-134249", width: 80, height: 24}
	for _, text := range []string{"UPDATE YOUR WENDY CLI", initial.current, initial.latest, "> Download now", "Ask again later...", "24 hours"} {
		if !strings.Contains(initial.View(), text) {
			t.Fatalf("dialog missing %q: %s", text, initial.View())
		}
	}
	for _, tc := range []struct {
		name             string
		keys             []tea.KeyMsg
		later, cancelled bool
	}{
		{name: "download", keys: []tea.KeyMsg{{Type: tea.KeyEnter}}},
		{name: "later", keys: []tea.KeyMsg{{Type: tea.KeyTab}, {Type: tea.KeyEnter}}, later: true},
		{name: "arrow navigation", keys: []tea.KeyMsg{{Type: tea.KeyDown}, {Type: tea.KeyUp}, {Type: tea.KeyEnter}}},
		{name: "escape", keys: []tea.KeyMsg{{Type: tea.KeyEsc}}, cancelled: true},
		{name: "interrupt", keys: []tea.KeyMsg{{Type: tea.KeyCtrlC}}, cancelled: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := initial
			for i, key := range tc.keys {
				next, cmd := m.Update(key)
				m = next.(cliUpdateModel)
				if i < len(tc.keys)-1 && (m.answered || cmd != nil) {
					t.Fatal("navigation dismissed the dialog")
				}
			}
			if m.later != tc.later || m.cancelled != tc.cancelled || m.answered == tc.cancelled || m.View() != "" {
				t.Fatalf("unexpected decision: %+v", m)
			}
		})
	}
}

// Bubble Tea keeps only the bottom lines of a view taller than the terminal.
func TestCLIUpdateDialogKeepsVersionsInShortTerminals(t *testing.T) {
	for _, size := range [][2]int{{80, 24}, {80, 12}, {32, 15}} {
		m := cliUpdateModel{current: "2026.09.29-120000", latest: "2026.10.05-134249"}
		next, _ := m.Update(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
		view := next.View()
		if got := lipgloss.Height(view); got > size[1] {
			t.Fatalf("%dx%d: dialog height %d", size[0], size[1], got)
		}
		for _, text := range []string{"UPDATE YOUR WENDY CLI", "Installed:", "Available:", "Download now", "Ask again later..."} {
			if !strings.Contains(view, text) {
				t.Fatalf("%dx%d: dialog missing %q: %s", size[0], size[1], text, view)
			}
		}
	}
}

func TestCLIUpdateDialogFitsTerminalWidth(t *testing.T) {
	for _, width := range []int{32, 60, 80, 120} {
		m := cliUpdateModel{current: "2026.09.29-120000", latest: "2026.10.05-134249"}
		next, _ := m.Update(tea.WindowSizeMsg{Width: width, Height: 40})
		if got := lipgloss.Width(next.View()); got > width {
			t.Fatalf("dialog width %d exceeds terminal width %d", got, width)
		}
	}
}
