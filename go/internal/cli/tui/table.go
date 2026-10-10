package tui

import (
	"strings"

	bubbleTable "github.com/charmbracelet/bubbles/table"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/lipgloss/table"
	"github.com/charmbracelet/x/ansi"
)

var (
	tableHeaderForeground = ColorHeaderFg
	tableHeaderBackground = ColorHeaderBg
	tableBorderColor      = ColorBorder
	tableSelectedBg       = ColorSelectedBg
	tableSelectedFg       = ColorSelectedFg

	headerStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(tableHeaderForeground).
			Background(tableHeaderBackground).
			Padding(0, 1)

	cellStyle = lipgloss.NewStyle().
			Padding(0, 1)

	borderStyle = lipgloss.NewStyle().
			Foreground(tableBorderColor)
)

// RenderTable renders a styled table with the given headers and rows.
func RenderTable(headers []string, rows [][]string) string {
	if len(headers) == 0 {
		return ""
	}

	t := table.New().
		Border(lipgloss.RoundedBorder()).
		BorderStyle(borderStyle).
		Headers(headers...).
		StyleFunc(func(row, col int) lipgloss.Style {
			if row == table.HeaderRow {
				return headerStyle
			}
			return cellStyle
		})

	for _, row := range rows {
		t.Row(row...)
	}

	return t.Render() + "\n"
}

func BubbleTableStyles(interactive bool) bubbleTable.Styles {
	styles := bubbleTable.DefaultStyles()
	styles.Header = lipgloss.NewStyle().
		Bold(true).
		Foreground(tableHeaderForeground).
		Background(tableHeaderBackground).
		Padding(0, 1)
	styles.Cell = lipgloss.NewStyle().Padding(0, 1)
	if interactive {
		styles.Selected = lipgloss.NewStyle().
			Foreground(tableSelectedFg).
			Background(tableSelectedBg).
			Bold(true)
	} else {
		styles.Selected = lipgloss.NewStyle()
	}
	return styles
}

type BubbleTable struct {
	model         bubbleTable.Model
	viewportWidth int
	x             int
}

func NewBubbleTable(interactive bool, columns []bubbleTable.Column) BubbleTable {
	opts := []bubbleTable.Option{bubbleTable.WithFocused(interactive)}
	if len(columns) > 0 {
		opts = append(opts, bubbleTable.WithColumns(columns))
	}
	t := bubbleTable.New(opts...)
	t.SetStyles(BubbleTableStyles(interactive))
	return BubbleTable{model: t}
}

func (t BubbleTable) Update(msg tea.Msg) (BubbleTable, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		// Never answer a resize with tea.ClearScreen: bubbletea sends a
		// WindowSizeMsg at every program start, and ClearScreen erases the
		// user's entire visible terminal in place (CSI 2J) — destroyed lines
		// never reach scrollback. The renderer already repaints all lines on
		// WindowSizeMsg, so no command is needed. Guarded by
		// TestNoScreenClearingInCLISource.
		t.SetViewportWidth(msg.Width)
		return t, nil
	case tea.KeyMsg:
		switch msg.String() {
		case "left", "h":
			t.ScrollLeft()
			return t, nil
		case "right", "l":
			t.ScrollRight()
			return t, nil
		}
	}

	var cmd tea.Cmd
	t.model, cmd = t.model.Update(msg)
	return t, cmd
}

func (t BubbleTable) View() string {
	view := t.model.View()
	if t.viewportWidth <= 0 {
		return view
	}
	return CropANSIView(view, t.x, t.viewportWidth)
}

func (t BubbleTable) FullView() string {
	return t.model.View()
}

func (t *BubbleTable) SetViewportWidth(width int) {
	t.viewportWidth = max(width, 0)
	t.clampX()
}

func (t BubbleTable) ViewportWidth() int {
	return t.viewportWidth
}

func (t *BubbleTable) ScrollLeft() {
	if t.x <= 0 {
		t.x = 0
		return
	}
	t.x = max(0, t.x-BubbleTableHorizontalStep())
}

func (t *BubbleTable) ScrollRight() {
	t.x = min(t.maxX(), t.x+BubbleTableHorizontalStep())
}

func (t *BubbleTable) ClampScroll() {
	t.clampX()
}

func (t *BubbleTable) clampX() {
	t.x = min(max(t.x, 0), t.maxX())
}

func (t BubbleTable) ScrollOffset() int {
	return t.x
}

func (t BubbleTable) CanScroll() bool {
	return t.maxX() > 0
}

func (t BubbleTable) maxX() int {
	if t.viewportWidth <= 0 {
		return 0
	}
	return max(0, t.model.Width()-t.viewportWidth)
}

func BubbleTableHorizontalStep() int {
	return 8
}

func (t BubbleTable) Columns() []bubbleTable.Column {
	return t.model.Columns()
}

func (t *BubbleTable) SetColumns(cols []bubbleTable.Column) {
	t.model.SetColumns(cols)
	t.clampX()
}

func (t BubbleTable) Rows() []bubbleTable.Row {
	return t.model.Rows()
}

func (t *BubbleTable) SetRows(rows []bubbleTable.Row) {
	t.model.SetRows(rows)
	// bubbles' table.SetRows drives the cursor to -1 whenever it is called with
	// an empty slice, and never restores it once rows reappear. A negative
	// cursor makes UpdateViewport render zero rows, so a table that gets its
	// data after an initial empty SetRows (a window-resize or empty poll landing
	// before the first payload) looks empty until the user presses an arrow key.
	// Clamp the cursor back into range so the first row is visible immediately.
	if len(rows) > 0 && t.model.Cursor() < 0 {
		t.model.SetCursor(0)
	}
}

func (t BubbleTable) Cursor() int {
	return t.model.Cursor()
}

func (t *BubbleTable) SetCursor(n int) {
	t.model.SetCursor(n)
}

func (t BubbleTable) Width() int {
	return t.model.Width()
}

func (t *BubbleTable) SetWidth(width int) {
	t.model.SetWidth(width)
	t.clampX()
}

func (t BubbleTable) Height() int {
	return t.model.Height()
}

func (t *BubbleTable) SetHeight(height int) {
	t.model.SetHeight(height)
	// bubbles recalculates the rendered rows on resize, but leaves the old
	// scroll offset in place. Reconcile it without moving the selected row.
	if t.model.Cursor() >= 0 {
		t.model.MoveDown(0)
	}
}

func (t BubbleTable) SelectedRow() bubbleTable.Row {
	return t.model.SelectedRow()
}

// CropANSIView cuts every rendered line to width columns from offset. This
// prevents terminal wrapping while preserving ANSI escape sequences.
func CropANSIView(view string, offset, width int) string {
	if width <= 0 {
		return view
	}

	lines := strings.Split(view, "\n")
	for i, line := range lines {
		lines[i] = ansi.Cut(line, offset, offset+width)
	}
	return strings.Join(lines, "\n")
}

// WrapHint preserves recovery instructions that would otherwise be cropped at
// the terminal edge. A zero width means the terminal size is not known yet.
func WrapHint(text string, width int) string {
	if width <= 0 {
		return text
	}
	// Break only at whitespace: splitting at hyphens makes commands such as
	// sync-time and hostname arguments ambiguous when copied from the view.
	var lines []string
	for _, paragraph := range strings.Split(text, "\n") {
		line, rest := "", paragraph
		for _, word := range strings.Fields(paragraph) {
			index := strings.Index(rest, word)
			separator := rest[:index]
			rest = rest[index+len(word):]
			if line != "" && ansi.StringWidth(line+separator+word) > width {
				lines = append(lines, line)
				line = word
			} else {
				line += separator + word
			}
		}
		lines = append(lines, line)
	}
	// Unusually long tokens still have to fit inside the terminal viewport.
	return ansi.Hardwrap(strings.Join(lines, "\n"), width, true)
}
