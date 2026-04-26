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
	Locked      bool   // when true, item cannot be toggled or selected
}

// ChecklistModel is a Bubble Tea model that presents a list of items with
// yes/no toggles. Navigate with up/down, toggle with left/right or space,
// and confirm with enter. Row 0 is a "Select all" toggle.
type ChecklistModel struct {
	Title          string
	SelectAllLabel string // defaults to "Select all"
	items          []ChecklistItem
	cursor         int // 0 = select-all row, 1..len(items) = item rows
	done           bool
	quitting       bool
}

// NewChecklist creates a new checklist model. The cursor starts on the
// "Select all" row at position 0.
func NewChecklist(title string, items []ChecklistItem) ChecklistModel {
	cp := make([]ChecklistItem, len(items))
	copy(cp, items)
	return ChecklistModel{
		Title:          title,
		SelectAllLabel: "Select all",
		items:          cp,
	}
}

func (m ChecklistModel) allSelected() bool {
	for _, item := range m.items {
		if !item.Locked && !item.Selected {
			return false
		}
	}
	return true
}

func (m *ChecklistModel) setAll(v bool) {
	for i := range m.items {
		if !m.items[i].Locked {
			m.items[i].Selected = v
		}
	}
}

// totalRows returns the number of visible rows (select-all + items).
func (m ChecklistModel) totalRows() int {
	return 1 + len(m.items)
}

func (m *ChecklistModel) toggle() {
	if len(m.items) == 0 {
		return
	}
	if m.cursor == 0 {
		// Select-all row: toggle all non-locked items to the opposite of current state.
		m.setAll(!m.allSelected())
	} else if !m.items[m.cursor-1].Locked {
		m.items[m.cursor-1].Selected = !m.items[m.cursor-1].Selected
	}
}

func (m *ChecklistModel) set(v bool) {
	if len(m.items) == 0 {
		return
	}
	if m.cursor == 0 {
		m.setAll(v)
	} else if !m.items[m.cursor-1].Locked {
		m.items[m.cursor-1].Selected = v
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
			if m.cursor < m.totalRows()-1 {
				m.cursor++
			}
		case "left", "right", " ":
			m.toggle()
		case "y", "Y":
			m.set(true)
		case "n", "N":
			m.set(false)
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
	clSeparator   = lipgloss.NewStyle().Foreground(ColorDim)
	clLocked      = lipgloss.NewStyle().Foreground(ColorDim).Padding(0, 1)
	clLockedLabel = lipgloss.NewStyle().Foreground(ColorDim)
)

func (m ChecklistModel) View() string {
	if m.done || m.quitting {
		return ""
	}

	var sb strings.Builder

	sb.WriteString(clTitle.Render(m.Title))
	sb.WriteString(clHint.Render("  (↑/↓ navigate, y/n or ←/→/space toggle, enter confirm)"))
	sb.WriteString("\n\n")

	// Select-all row.
	{
		pointer := "  "
		label := clLabel.Render(m.SelectAllLabel)
		if m.cursor == 0 {
			pointer = clCursor.Render("▸ ")
			label = clLabelActive.Render(m.SelectAllLabel)
		}
		yes, no := clOff.Render("Yes"), clOn.Render("No")
		if m.allSelected() {
			yes, no = clOn.Render("Yes"), clOff.Render("No")
		}
		sb.WriteString(fmt.Sprintf("%s%s %s  %s\n", pointer, yes, no, label))
	}

	sb.WriteString(clSeparator.Render("  ───") + "\n")

	// Item rows.
	for i, item := range m.items {
		row := i + 1 // account for select-all row
		pointer := "  "

		if item.Locked {
			if row == m.cursor {
				pointer = clCursor.Render("▸ ")
			}
			label := clLockedLabel.Render(item.Label)
			protected := clLocked.Render("[protected]")
			sb.WriteString(fmt.Sprintf("%s%s  %s", pointer, protected, label))
			if item.Description != "" {
				sb.WriteString("  " + clDesc.Render(item.Description))
			}
			sb.WriteString("\n")
			continue
		}

		label := clLabel.Render(item.Label)
		if row == m.cursor {
			pointer = clCursor.Render("▸ ")
			label = clLabelActive.Render(item.Label)
		}

		yes, no := clOff.Render("Yes"), clOn.Render("No")
		if item.Selected {
			yes, no = clOn.Render("Yes"), clOff.Render("No")
		}

		sb.WriteString(fmt.Sprintf("%s%s %s  %s", pointer, yes, no, label))
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
	return RunChecklistModel(NewChecklist(title, items), programOpts...)
}

// RunChecklistModel runs an interactive checklist from an already-configured
// ChecklistModel and returns the selected items.
func RunChecklistModel(m ChecklistModel, programOpts ...tea.ProgramOption) ([]ChecklistItem, error) {
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
		return nil, ErrCancelled
	}
	return model.SelectedItems(), nil
}
