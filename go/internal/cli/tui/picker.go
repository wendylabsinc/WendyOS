package tui

import (
	"strings"

	bubbleTable "github.com/charmbracelet/bubbles/table"
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

	// DedupKey is used for deduplication. If empty, Name is used.
	// Items with the same DedupKey (case-insensitive) are merged via MergeItem.
	DedupKey string

	// Value is the opaque payload returned when this item is selected.
	Value interface{}
}

// PickerAddMsg adds new items to the picker. Duplicates (by DedupKey, or Name
// if DedupKey is empty) are merged via MergeItem or silently dropped.
type PickerAddMsg struct {
	Items []PickerItem
}

// PickerDoneMsg signals that discovery has finished. The picker remains
// interactive so the user can still select from the collected items.
type PickerDoneMsg struct{}

// PickerModel is a Bubble Tea model that presents a live-updating list of
// items and lets the user select one with arrow keys + Enter.
type PickerModel struct {
	Title string // header line, e.g. "Select a device"

	// MergeItem is called when a new item shares a DedupKey with an existing
	// item. The caller can update existing in place (type, address, value, ...).
	// If nil, duplicate items are silently dropped.
	MergeItem func(existing *PickerItem, incoming PickerItem)

	// OnSetDefault is called when the user presses 'd' on the highlighted item.
	// If nil, 'd' is ignored.
	OnSetDefault func(item PickerItem)

	// OnUnsetDefault is called when the user presses 'x'.
	// If nil, 'x' is ignored.
	OnUnsetDefault func()

	// DefaultKey is the DedupKey (or Name if DedupKey is empty) of the item
	// that is currently the default. Shown with a ★ indicator.
	DefaultKey string

	items    []PickerItem
	seenIdx  map[string]int // dedup key -> index in items
	table    bubbleTable.Model
	selected *PickerItem
	scanning bool
	quitting bool
	height   int
}

// NewPicker creates a new picker model with the default "Select a device" title.
func NewPicker() PickerModel {
	m := PickerModel{
		Title:    "Select a device",
		seenIdx:  make(map[string]int),
		table:    newPickerTable(),
		scanning: true,
	}
	m.refreshTable()
	return m
}

// NewPickerWithTitle creates a new picker model with a custom title.
func NewPickerWithTitle(title string) PickerModel {
	m := PickerModel{
		Title:    title,
		seenIdx:  make(map[string]int),
		table:    newPickerTable(),
		scanning: true,
	}
	m.refreshTable()
	return m
}

func (m PickerModel) Init() tea.Cmd { return nil }

func (m PickerModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.height = msg.Height
		m.refreshTable()
		return m, nil
	case tea.KeyMsg:
		switch msg.String() {
		case "enter":
			cursor := m.table.Cursor()
			if len(m.items) > 0 && cursor >= 0 && cursor < len(m.items) {
				item := m.items[cursor]
				m.selected = &item
				return m, tea.Quit
			}
		case "d":
			if m.OnSetDefault != nil {
				cursor := m.table.Cursor()
				if len(m.items) > 0 && cursor >= 0 && cursor < len(m.items) {
					item := m.items[cursor]
					key := strings.ToLower(item.DedupKey)
					if key == "" {
						key = strings.ToLower(item.Name)
					}
					m.DefaultKey = key
					m.OnSetDefault(item)
					m.refreshTable()
				}
			}
			return m, nil
		case "x":
			if m.OnUnsetDefault != nil {
				m.DefaultKey = ""
				m.OnUnsetDefault()
				m.refreshTable()
			}
			return m, nil
		case "q", "ctrl+c":
			m.quitting = true
			return m, tea.Quit
		default:
			var cmd tea.Cmd
			m.table, cmd = m.table.Update(msg)
			return m, cmd
		}

	case PickerAddMsg:
		changed := false
		for _, item := range msg.Items {
			key := strings.ToLower(item.DedupKey)
			if key == "" {
				key = strings.ToLower(item.Name)
			}
			if idx, ok := m.seenIdx[key]; ok {
				if m.MergeItem != nil {
					m.MergeItem(&m.items[idx], item)
					changed = true
				}
				continue
			}
			m.seenIdx[key] = len(m.items)
			m.items = append(m.items, item)
			changed = true
		}
		if changed {
			m.refreshTable()
		}

	case PickerDoneMsg:
		m.scanning = false
	}

	return m, nil
}

var (
	pickerTitle    = lipgloss.NewStyle().Bold(true).Foreground(ColorPrimary)
	pickerHint     = lipgloss.NewStyle().Foreground(ColorDim)
	pickerScanning = lipgloss.NewStyle().Foreground(ColorPrimary)
)

func (m PickerModel) View() string {
	if m.quitting || m.selected != nil {
		return ""
	}

	var sb strings.Builder

	hint := " (↑/↓ navigate, enter select, q quit)"
	if m.OnSetDefault != nil {
		hint = " (↑/↓ navigate, enter select, d set default, x unset default, q quit)"
	}
	sb.WriteString(pickerTitle.Render(m.Title) + pickerHint.Render(hint) + "\n\n")

	if len(m.items) == 0 {
		if m.scanning {
			sb.WriteString(pickerScanning.Render("  Scanning...") + "\n")
		} else {
			sb.WriteString(pickerHint.Render("  No options found.") + "\n")
		}
		return sb.String()
	}

	sb.WriteString(m.table.View() + "\n")

	if m.scanning {
		sb.WriteString("\n" + pickerScanning.Render("  Scanning for more results...") + "\n")
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

type pickerColumnDef struct {
	title    string
	minWidth int
	maxWidth int
	value    func(PickerItem) string
	required bool
}

var pickerColumnDefs = []pickerColumnDef{
	{
		title:    "Name",
		minWidth: 18,
		maxWidth: 48,
		value: func(item PickerItem) string {
			return item.Name
		},
		required: true,
	},
	{
		title:    "Type",
		minWidth: 12,
		maxWidth: 18,
		value: func(item PickerItem) string {
			return item.Type
		},
	},
	{
		title:    "Address",
		minWidth: 14,
		maxWidth: 28,
		value: func(item PickerItem) string {
			return item.Address
		},
	},
	{
		title:    "Description",
		minWidth: 20,
		maxWidth: 48,
		value: func(item PickerItem) string {
			return item.Description
		},
	},
}

func newPickerTable() bubbleTable.Model {
	return NewBubbleTable(true, nil)
}

func (m *PickerModel) refreshTable() {
	hasDefaultCol := m.DefaultKey != "" || m.OnSetDefault != nil
	activeCols := pickerActiveColumns(m.items)
	rows := pickerRows(m.items, activeCols, m.DefaultKey)

	var cols []bubbleTable.Column
	if hasDefaultCol {
		// Leading ★ column, then the data columns (offset by 1 in rows).
		cols = append(cols, bubbleTable.Column{Title: "", Width: 3})
		for i, def := range activeCols {
			colIdx := i + 1 // rows have the star column at index 0
			width := lipgloss.Width(def.title)
			for _, row := range rows {
				if colIdx < len(row) {
					width = max(width, lipgloss.Width(row[colIdx]))
				}
			}
			width += 2
			width = max(width, def.minWidth)
			width = min(width, def.maxWidth)
			cols = append(cols, bubbleTable.Column{Title: def.title, Width: width})
		}
	} else {
		cols = pickerColumns(rows, activeCols)
	}
	m.table.SetColumns(cols)
	m.table.SetRows(rows)
	if len(rows) > 0 && m.table.Cursor() < 0 {
		m.table.SetCursor(0)
	}
	m.table.SetWidth(pickerTableWidth(m.table.Columns()))
	m.table.SetHeight(pickerTableHeight(len(rows), m.height))
}

func pickerActiveColumns(items []PickerItem) []pickerColumnDef {
	var active []pickerColumnDef
	for _, def := range pickerColumnDefs {
		if def.required {
			active = append(active, def)
			continue
		}
		for _, item := range items {
			if def.value(item) != "" {
				active = append(active, def)
				break
			}
		}
	}
	return active
}

func pickerRows(items []PickerItem, cols []pickerColumnDef, defaultKey string) []bubbleTable.Row {
	rows := make([]bubbleTable.Row, 0, len(items))
	for _, item := range items {
		var row bubbleTable.Row
		// Add leading ★ column when default tracking is active.
		if defaultKey != "" {
			key := strings.ToLower(item.DedupKey)
			if key == "" {
				key = strings.ToLower(item.Name)
			}
			if key == defaultKey {
				row = append(row, "★")
			} else {
				row = append(row, "")
			}
		}
		for _, col := range cols {
			row = append(row, col.value(item))
		}
		rows = append(rows, row)
	}
	return rows
}

func pickerColumns(rows []bubbleTable.Row, defs []pickerColumnDef) []bubbleTable.Column {
	cols := make([]bubbleTable.Column, len(defs))
	for i, def := range defs {
		width := lipgloss.Width(def.title)
		for _, row := range rows {
			if i >= len(row) {
				continue
			}
			width = max(width, lipgloss.Width(row[i]))
		}
		width += 2
		width = max(width, def.minWidth)
		width = min(width, def.maxWidth)
		cols[i] = bubbleTable.Column{Title: def.title, Width: width}
	}
	return cols
}

func pickerTableWidth(cols []bubbleTable.Column) int {
	total := 0
	for _, col := range cols {
		total += col.Width + 2
	}
	return total
}

func pickerTableHeight(rowCount, windowHeight int) int {
	height := max(rowCount+1, 4)
	if windowHeight > 0 {
		return min(height, max(windowHeight-5, 4))
	}
	return min(height, 12)
}
