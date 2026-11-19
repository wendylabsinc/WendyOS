package tui

import (
	"reflect"
	"strings"
	"testing"

	bubbleTable "github.com/charmbracelet/bubbles/table"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
)

// Regression test for the "wendy discover eats my terminal" bug: bubbletea
// delivers a WindowSizeMsg at every program start, and BubbleTable.Update used
// to answer it with tea.ClearScreen. That emits CSI 2J on the primary screen,
// destroying the user's visible terminal content in place (erased lines never
// reach scrollback). The renderer already repaints on WindowSizeMsg, so no
// command is needed here at all.
func TestBubbleTableWindowSizeMsgMustNotClearScreen(t *testing.T) {
	cols := []bubbleTable.Column{{Title: "Name", Width: 20}}
	tbl := NewBubbleTable(true, cols)

	_, cmd := tbl.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	if cmd == nil {
		return
	}
	if msg := cmd(); reflect.TypeOf(msg) == reflect.TypeOf(tea.ClearScreen()) {
		t.Fatal("BubbleTable.Update answered WindowSizeMsg with tea.ClearScreen; " +
			"this wipes the user's visible terminal content at TUI startup and on every resize")
	}
}

// The underlying bubbles table.SetRows() drives its cursor to -1 whenever it is
// called with an empty slice, and never restores it once rows reappear. A
// negative cursor makes the table render zero rows, so an interactive table
// that receives its data after an initial empty SetRows (e.g. a window-resize
// or empty poll landing before the first payload) looks empty until the user
// presses an arrow key. BubbleTable.SetRows must guard against this.
func TestBubbleTableSetRowsRecoversCursorAfterEmpty(t *testing.T) {
	cols := []bubbleTable.Column{{Title: "Name", Width: 20}}
	tbl := NewBubbleTable(true, cols)
	tbl.SetHeight(5)

	// Empty first (the trigger).
	tbl.SetRows(nil)

	// Then real data arrives.
	tbl.SetRows([]bubbleTable.Row{{"my-app"}})

	if cur := tbl.Cursor(); cur < 0 {
		t.Fatalf("cursor = %d after non-empty SetRows, want >= 0", cur)
	}
	if view := tbl.FullView(); !strings.Contains(view, "my-app") {
		t.Fatalf("expected row visible in view without arrow-key input, got:\n%s", view)
	}
}

// A non-empty SetRows should not disturb an already-valid cursor position.
func TestBubbleTableSetRowsPreservesValidCursor(t *testing.T) {
	cols := []bubbleTable.Column{{Title: "Name", Width: 20}}
	tbl := NewBubbleTable(true, cols)
	tbl.SetHeight(10)
	tbl.SetRows([]bubbleTable.Row{{"a"}, {"b"}, {"c"}})
	tbl.SetCursor(2)

	tbl.SetRows([]bubbleTable.Row{{"a"}, {"b"}, {"c"}, {"d"}})
	if cur := tbl.Cursor(); cur != 2 {
		t.Fatalf("cursor = %d after SetRows, want it preserved at 2", cur)
	}
}

// rowLine returns the rendered line that shows text.
func rowLine(t *testing.T, view, text string) string {
	t.Helper()
	for _, line := range strings.Split(view, "\n") {
		if strings.Contains(line, text) {
			return line
		}
	}
	t.Fatalf("no line shows %q in:\n%s", text, view)
	return ""
}

func TestBubbleTableSelectedRowWithAndWithoutColour(t *testing.T) {
	render := func(p termenv.Profile) string {
		withColor(t, p)
		tbl := NewBubbleTable(true, []bubbleTable.Column{{Title: "Drive", Width: 10}})
		tbl.SetHeight(5)
		tbl.SetRows([]bubbleTable.Row{{"disk4"}, {"disk5"}})
		tbl.SetCursor(1)
		return tbl.FullView()
	}

	plain := render(termenv.Ascii)
	selected, other := rowLine(t, plain, "disk5"), rowLine(t, plain, "disk4")
	if !strings.HasPrefix(selected, ">disk5") || strings.HasPrefix(other, ">") || len(selected) != len(other) {
		t.Fatalf("plain rows %q and %q; want only the cursor row marked, at the same width", selected, other)
	}

	coloured := render(termenv.TrueColor)
	want := lipgloss.NewStyle().Foreground(tableSelectedFg).Background(tableSelectedBg).Bold(true).Render(" disk5      ")
	if got := rowLine(t, coloured, "disk5"); got != want || strings.Contains(coloured, ">") {
		t.Fatalf("coloured cursor row %q; want the highlight unchanged, %q", got, want)
	}
}
