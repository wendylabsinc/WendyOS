//go:build darwin || linux || windows

package commands

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
)

type installPickerTab struct {
	label  string
	picker tui.PickerModel
}

type installPickerModel struct {
	tabs      []installPickerTab
	active    int
	width     int
	selected  *tui.PickerItem
	cancelled bool
}

func newInstallPickerModel(items []tui.PickerItem) installPickerModel {
	groups := []struct {
		label string
		items []tui.PickerItem
	}{
		{label: "Linux"},
		{label: "Mac"},
		{label: "Microcontrollers"},
	}
	for _, item := range items {
		value, _ := item.Value.(string)
		group := 0
		switch {
		case value == headlessMacValue:
			group = 1
		case strings.HasPrefix(value, "wlite_"):
			group = 2
		}
		groups[group].items = append(groups[group].items, item)
	}

	var m installPickerModel
	for _, group := range groups {
		// PR builds only offer Linux devices. Catalog failures can also leave
		// a category empty, so only offer tabs with installable options.
		if len(group.items) == 0 {
			continue
		}
		picker := tui.NewPickerWithTitle("Select a device")
		updated, _ := picker.Update(tui.PickerAddMsg{Items: group.items})
		updated, _ = updated.Update(tui.PickerDoneMsg{})
		m.tabs = append(m.tabs, installPickerTab{label: group.label, picker: updated.(tui.PickerModel)})
	}
	return m
}

// All install options are loaded before opening the picker.
func (m installPickerModel) Init() tea.Cmd { return nil }

func (m installPickerModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
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

func (m installPickerModel) View() string {
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

func pickInstallDevice(items []tui.PickerItem) (string, error) {
	if len(items) == 0 {
		return "", fmt.Errorf("no devices available")
	}
	finalModel, err := tea.NewProgram(newInstallPickerModel(items)).Run()
	if err != nil {
		return "", fmt.Errorf("picker: %w", err)
	}
	m := finalModel.(installPickerModel)
	if m.cancelled {
		return "", ErrUserCancelled
	}
	if m.selected == nil {
		return "", fmt.Errorf("no selection")
	}
	return m.selected.Value.(string), nil
}
