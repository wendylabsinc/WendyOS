package tui

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
)

var (
	headerStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("229")).
			Background(lipgloss.Color("57")).
			Padding(0, 1)

	cellStyle = lipgloss.NewStyle().
			Padding(0, 1)

	separatorStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("240"))
)

// RenderTable renders a styled table with the given headers and rows.
func RenderTable(headers []string, rows [][]string) string {
	if len(headers) == 0 {
		return ""
	}

	// Calculate column widths.
	colWidths := make([]int, len(headers))
	for i, h := range headers {
		colWidths[i] = lipgloss.Width(h)
	}
	for _, row := range rows {
		for i, cell := range row {
			if i < len(colWidths) {
				w := lipgloss.Width(cell)
				if w > colWidths[i] {
					colWidths[i] = w
				}
			}
		}
	}

	var sb strings.Builder

	// Header row.
	var headerCells []string
	for i, h := range headers {
		headerCells = append(headerCells, headerStyle.Width(colWidths[i]+2).Render(h))
	}
	sb.WriteString(lipgloss.JoinHorizontal(lipgloss.Top, headerCells...))
	sb.WriteString("\n")

	// Separator.
	var sepParts []string
	for _, w := range colWidths {
		sepParts = append(sepParts, strings.Repeat("-", w+2))
	}
	sb.WriteString(separatorStyle.Render(strings.Join(sepParts, "+")))
	sb.WriteString("\n")

	// Data rows.
	for _, row := range rows {
		var cells []string
		for i := range headers {
			val := ""
			if i < len(row) {
				val = row[i]
			}
			cells = append(cells, cellStyle.Width(colWidths[i]+2).Render(val))
		}
		sb.WriteString(lipgloss.JoinHorizontal(lipgloss.Top, cells...))
		sb.WriteString("\n")
	}

	return sb.String()
}
