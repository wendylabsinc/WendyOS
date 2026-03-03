package tui

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// PickerItem represents a selectable row in the device picker.
type PickerItem struct {
	// Display columns rendered in the table.
	Name        string
	Description string // optional secondary text rendered dimmed
	Type        string // "LAN", "Bluetooth", "External", etc.
	Address     string

	// Value is the opaque payload returned when this item is selected.
	Value interface{}
}

// PickerAddMsg adds new items to the picker. Duplicates (by Address) are ignored.
type PickerAddMsg struct {
	Items []PickerItem
}

// PickerDoneMsg signals that discovery has finished. The picker remains
// interactive so the user can still select from the collected items.
type PickerDoneMsg struct{}

// PickerModel is a Bubble Tea model that presents a live-updating list of
// items and lets the user select one with arrow keys + Enter.
type PickerModel struct {
	Title    string // header line, e.g. "Select a device"
	items    []PickerItem
	seen     map[string]bool
	cursor   int
	selected *PickerItem
	scanning bool
	quitting bool
}

// NewPicker creates a new picker model with the default "Select a device" title.
func NewPicker() PickerModel {
	return PickerModel{
		Title:    "Select a device",
		seen:     make(map[string]bool),
		scanning: true,
	}
}

// NewPickerWithTitle creates a new picker model with a custom title.
func NewPickerWithTitle(title string) PickerModel {
	return PickerModel{
		Title:    title,
		seen:     make(map[string]bool),
		scanning: true,
	}
}

func (m PickerModel) Init() tea.Cmd { return nil }

func (m PickerModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
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
		case "enter":
			if len(m.items) > 0 && m.cursor < len(m.items) {
				item := m.items[m.cursor]
				m.selected = &item
				return m, tea.Quit
			}
		case "q", "ctrl+c":
			m.quitting = true
			return m, tea.Quit
		}

	case PickerAddMsg:
		for _, item := range msg.Items {
			key := item.Name + ":" + item.Type + ":" + item.Address
			if m.seen[key] {
				continue
			}
			m.seen[key] = true
			m.items = append(m.items, item)
		}

	case PickerDoneMsg:
		m.scanning = false
	}

	return m, nil
}

var (
	pickerTitle    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("205"))
	pickerHint     = lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
	pickerCursor   = lipgloss.NewStyle().Foreground(lipgloss.Color("229")).Bold(true)
	pickerNormal   = lipgloss.NewStyle()
	pickerScanning = lipgloss.NewStyle().Foreground(lipgloss.Color("205"))
)

func (m PickerModel) View() string {
	if m.quitting || m.selected != nil {
		return ""
	}

	var sb strings.Builder

	sb.WriteString(pickerTitle.Render(m.Title) + pickerHint.Render(" (↑/↓ navigate, enter select, q quit)") + "\n\n")

	if len(m.items) == 0 {
		if m.scanning {
			sb.WriteString(pickerScanning.Render("  Scanning for devices...") + "\n")
		} else {
			sb.WriteString(pickerHint.Render("  No devices found.") + "\n")
		}
		return sb.String()
	}

	// Render as a simple list with a cursor indicator.
	for i, item := range m.items {
		cursor := "  "
		style := pickerNormal
		if i == m.cursor {
			cursor = "> "
			style = pickerCursor
		}

		var line string
		if item.Type == "" && item.Address == "" {
			line = fmt.Sprintf("%s%s", cursor, item.Name)
		} else {
			line = fmt.Sprintf("%s%-24s %-12s %s", cursor, item.Name, item.Type, item.Address)
		}
		sb.WriteString(style.Render(line))
		if item.Description != "" {
			sb.WriteString(" " + pickerHint.Render(item.Description))
		}
		sb.WriteString("\n")
	}

	if m.scanning {
		sb.WriteString("\n" + pickerScanning.Render("  Scanning...") + "\n")
	}

	return sb.String()
}

// Cancelled returns true if the user quit the picker without selecting (e.g. Ctrl+C).
func (m PickerModel) Cancelled() bool {
	return m.quitting
}

// Selected returns the item the user chose, or nil if they quit without selecting.
func (m PickerModel) Selected() *PickerItem {
	return m.selected
}
