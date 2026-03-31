package tui

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// ChecklistItem represents a single toggleable row in a checklist.
type ChecklistItem struct {
	Label       string
	Description string // shown dimmed after the label
	Value       string // opaque payload returned for selected items
	Selected    bool   // default selection state
}

// ChecklistModel is a Bubble Tea model that presents a list of items with
// yes/no toggles. Navigate with up/down, toggle with left/right or space,
// and confirm with enter.
type ChecklistModel struct {
	Title    string
	items    []ChecklistItem
	cursor   int
	done     bool
	quitting bool
}

// NewChecklist creates a new checklist model.
func NewChecklist(title string, items []ChecklistItem) ChecklistModel {
	cp := make([]ChecklistItem, len(items))
	copy(cp, items)
	return ChecklistModel{
		Title: title,
		items: cp,
	}
}

func (m ChecklistModel) Init() tea.Cmd { return nil }

func (m ChecklistModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "up", "k":
			if m.cursor > 0 {
				m.cursor--
			}
		case "down", "j":
			if m.cursor < len(m.items)-1 {
				m.cursor++
			}
		case "left", "right", " ":
			m.items[m.cursor].Selected = !m.items[m.cursor].Selected
		case "y", "Y":
			m.items[m.cursor].Selected = true
		case "n", "N":
			m.items[m.cursor].Selected = false
		case "enter":
			m.done = true
			return m, tea.Quit
		case "ctrl+c":
			m.quitting = true
			return m, tea.Quit
		}
	}
	return m, nil
}

var (
	clTitle       = lipgloss.NewStyle().Bold(true).Foreground(ColorPrimary)
	clHint        = lipgloss.NewStyle().Foreground(ColorDim)
	clCursor      = lipgloss.NewStyle().Bold(true).Foreground(ColorPrimary)
	clOn          = lipgloss.NewStyle().Bold(true).Foreground(ColorSelectedFg).Background(ColorSelectedBg).Padding(0, 1)
	clOff         = lipgloss.NewStyle().Foreground(ColorDim).Padding(0, 1)
	clLabel       = lipgloss.NewStyle()
	clLabelActive = lipgloss.NewStyle().Bold(true).Foreground(ColorPrimary)
	clDesc        = lipgloss.NewStyle().Foreground(ColorDim)
)

func (m ChecklistModel) View() string {
	if m.done || m.quitting {
		return ""
	}

	var sb strings.Builder

	sb.WriteString(clTitle.Render(m.Title))
	sb.WriteString(clHint.Render("  (↑/↓ navigate, y/n or ←/→/space toggle, enter confirm)"))
	sb.WriteString("\n\n")

	for i, item := range m.items {
		pointer := "  "
		label := clLabel.Render(item.Label)
		if i == m.cursor {
			pointer = clCursor.Render("▸ ")
			label = clLabelActive.Render(item.Label)
		}

		toggle := clOff.Render("No")
		if item.Selected {
			toggle = clOn.Render("Yes")
		}

		sb.WriteString(fmt.Sprintf("%s%s  %s", pointer, toggle, label))
		if item.Description != "" {
			sb.WriteString("  " + clDesc.Render(item.Description))
		}
		sb.WriteString("\n")
	}

	return sb.String()
}

// Cancelled returns true if the user quit without confirming.
func (m ChecklistModel) Cancelled() bool {
	return m.quitting
}

// SelectedItems returns the items the user toggled on.
func (m ChecklistModel) SelectedItems() []ChecklistItem {
	var selected []ChecklistItem
	for _, item := range m.items {
		if item.Selected {
			selected = append(selected, item)
		}
	}
	return selected
}

// RunChecklist runs an interactive checklist and returns the selected items.
func RunChecklist(title string, items []ChecklistItem, programOpts ...tea.ProgramOption) ([]ChecklistItem, error) {
	m := NewChecklist(title, items)
	p := tea.NewProgram(m, programOpts...)
	result, err := p.Run()
	if err != nil {
		return nil, fmt.Errorf("checklist: %w", err)
	}
	model, ok := result.(ChecklistModel)
	if !ok {
		return nil, fmt.Errorf("checklist: unexpected model type %T", result)
	}
	if model.Cancelled() {
		return nil, fmt.Errorf("checklist: cancelled")
	}
	return model.SelectedItems(), nil
}
