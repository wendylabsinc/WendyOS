package tui

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

func TestMultiSpinnerDoneRowShowsCacheCounts(t *testing.T) {
	m := NewMultiSpinner("Building 1 service(s)...", []string{"api"})
	next, _ := m.Update(MultiSpinnerStartMsg{Name: "api"})
	m = next.(MultiSpinnerModel)
	next, _ = m.Update(MultiSpinnerDoneMsg{Name: "api", Dur: 21300 * time.Millisecond, Cached: 4, Rebuilt: 2})
	m = next.(MultiSpinnerModel)
	v := m.View()
	if !strings.Contains(v, "4 cached") || !strings.Contains(v, "2 rebuilt") {
		t.Fatalf("done row missing cache counts:\n%s", v)
	}
}

func TestMultiSpinnerRowsFitTerminal(t *testing.T) {
	names := []string{"adapter", "benchmark", "fsm", "inference", strings.Repeat("long-service-", 12)}
	m := NewMultiSpinner("Building 5 service(s)...", names)
	m.rows[0].status = MultiSpinnerRunning
	m.rows[0].detail = "pull dustynv/pytorch:2.7-r36.4.0-cu128-24.04 · " + strings.Repeat("downloading 界 ", 20)
	m.rows[1].status = MultiSpinnerRunning
	m.rows[1].detail = "compiling\r\nnext line\twith progress"
	m.rows[2].status = MultiSpinnerDone
	m.rows[2].dur = time.Minute
	m.rows[3].status = MultiSpinnerFailed

	for _, width := range []int{200, 80, 40, 10, 1, 100} {
		next, _ := m.Update(tea.WindowSizeMsg{Width: width, Height: 24})
		m = next.(MultiSpinnerModel)
		lines := strings.Split(strings.TrimSuffix(m.View(), "\n"), "\n")
		if len(lines) != len(names)+2 {
			t.Fatalf("width %d: got %d lines, want %d", width, len(lines), len(names)+2)
		}
		for _, line := range lines {
			if got := ansi.StringWidth(line); got > width {
				t.Errorf("width %d: line occupies %d columns: %q", width, got, line)
			}
			if strings.ContainsAny(line, "\r\t") {
				t.Errorf("control characters in row: %q", line)
			}
		}
	}
}

func TestMultiSpinnerLongServiceNamesStayOnOneLine(t *testing.T) {
	names := []string{"health-high-low", "transcription", "website-frontend"}
	m := NewMultiSpinner("Building 3 service(s)...", names)

	v := m.View()
	lines := strings.Split(strings.TrimSuffix(v, "\n"), "\n")
	// The title, one line per service, and one hint. A fixed-width lipgloss
	// style used to wrap every name longer than 12 cells into an extra line.
	if got, want := len(lines), len(names)+2; got != want {
		t.Fatalf("view has %d lines, want %d:\n%s", got, want, v)
	}
	for _, name := range names {
		if got := strings.Count(v, name); got != 1 {
			t.Errorf("service name %q appears %d times in view:\n%s", name, got, v)
		}
	}
}
