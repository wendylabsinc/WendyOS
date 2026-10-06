package commands

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
)

type pickerGroup struct {
	label string
	items []tui.PickerItem
}

type tabbedPickerTab struct {
	label  string
	picker tui.PickerModel
}

type tabbedPickerModel struct {
	tabs      []tabbedPickerTab
	active    int
	width     int
	selected  *tui.PickerItem
	cancelled bool
}

func newTabbedPickerModel(title string, groups []pickerGroup) tabbedPickerModel {
	var m tabbedPickerModel
	for _, group := range groups {
		if len(group.items) == 0 {
			continue
		}
		picker := tui.NewPickerWithTitle(title)
		updated, _ := picker.Update(tui.PickerAddMsg{Items: group.items})
		updated, _ = updated.Update(tui.PickerDoneMsg{})
		m.tabs = append(m.tabs, tabbedPickerTab{label: group.label, picker: updated.(tui.PickerModel)})
	}
	return m
}

// All options are loaded before opening the picker.
func (m tabbedPickerModel) Init() tea.Cmd { return nil }

func (m tabbedPickerModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if len(m.tabs) == 0 || m.selected != nil || m.cancelled {
		return m, nil
	}
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		msg.Height = max(1, msg.Height-2) // tab strip and blank line
		var cmds []tea.Cmd
		for i := range m.tabs {
			updated, cmd := m.tabs[i].picker.Update(msg)
			m.tabs[i].picker = updated.(tui.PickerModel)
			cmds = append(cmds, cmd)
		}
		return m, tea.Batch(cmds...)
	case tea.KeyMsg:
		if key := msg.String(); key == "tab" || key == "shift+tab" {
			m.active = (m.active + tabCycleDelta(key) + len(m.tabs)) % len(m.tabs)
			return m, nil
		}
	}
	updated, cmd := m.tabs[m.active].picker.Update(msg)
	picker := updated.(tui.PickerModel)
	m.tabs[m.active].picker = picker
	m.selected = picker.Selected()
	m.cancelled = picker.Cancelled()
	return m, cmd
}

func (m tabbedPickerModel) View() string {
	if len(m.tabs) == 0 || m.selected != nil || m.cancelled {
		return ""
	}
	var labels []string
	for i, tab := range m.tabs {
		style := devicePickerTabInactive
		if i == m.active {
			style = devicePickerTabActive
		}
		labels = append(labels, style.Render(tab.label))
	}
	header := strings.Join(labels, devicePickerTabInactive.Render(" | "))
	if len(m.tabs) > 1 {
		header += devicePickerTabInactive.Render("  (tab/shift+tab switch)")
	}
	if m.width > 0 {
		header = tui.CropANSIView(header, 0, m.width)
	}
	return header + "\n\n" + m.tabs[m.active].picker.View()
}

func runTabbedPicker(model tabbedPickerModel) (string, error) {
	if len(model.tabs) == 0 {
		return "", fmt.Errorf("no options available")
	}
	finalModel, err := tea.NewProgram(model).Run()
	if err != nil {
		return "", fmt.Errorf("picker: %w", err)
	}
	m := finalModel.(tabbedPickerModel)
	if m.cancelled {
		return "", ErrUserCancelled
	}
	if m.selected == nil {
		return "", fmt.Errorf("no selection")
	}
	return m.selected.Value.(string), nil
}
