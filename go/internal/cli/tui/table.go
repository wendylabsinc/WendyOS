package tui

import (
	bubbleTable "github.com/charmbracelet/bubbles/table"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/lipgloss/table"
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

// BubbleTableStyles returns the shared emerald styling for Bubble Tea tables.
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

// NewBubbleTable creates a Bubble Tea table using the shared emerald styling.
func NewBubbleTable(interactive bool, columns []bubbleTable.Column) bubbleTable.Model {
	opts := []bubbleTable.Option{bubbleTable.WithFocused(interactive)}
	if len(columns) > 0 {
		opts = append(opts, bubbleTable.WithColumns(columns))
	}
	t := bubbleTable.New(opts...)
	t.SetStyles(BubbleTableStyles(interactive))
	return t
}
