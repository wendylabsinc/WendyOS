package commands

import (
	"context"
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
)

func resolveHILSimulator(ctx context.Context, device string, yes bool) (string, error) {
	device = strings.TrimSpace(device)
	if device != "" {
		name, matched, err := simulatorName(device)
		if err != nil || !matched || name == "" {
			return "", fmt.Errorf("HIL requires a simulator; use --device vm:NAME or omit --device to choose one")
		}
		return name, nil
	}
	if yes || !isInteractiveTerminal() {
		return "", fmt.Errorf("HIL needs a simulator; use --device vm:NAME for non-interactive runs")
	}
	name, err := pickHILSimulatorFn(withDevicePickerPurpose(ctx, mainDevicePicker))
	if err != nil {
		return "", err
	}
	// Validate a picker result before it becomes a target selector.
	if _, matched, err := simulatorName(vmDeviceIDPrefix + name); err != nil || !matched {
		return "", fmt.Errorf("invalid simulator selection %q", name)
	}
	return name, nil
}

var pickHILSimulatorFn = pickHILSimulator

func pickHILSimulator(ctx context.Context) (string, error) {
	statuses, err := vmStatusesFn()
	if err != nil {
		return "", err
	}
	if len(statuses) == 0 {
		return "", fmt.Errorf("no simulators found; create one with 'wendy vm create <name>'")
	}
	picker := tui.NewPickerWithTitleAndColumns("Select a simulator for HIL", simulatorPickerColumns())
	updated, _ := picker.Update(tui.PickerSetMsg{Items: simulatorRows(statuses)})
	updated, _ = updated.(tui.PickerModel).Update(tui.PickerDoneMsg{})
	model := hilSimulatorPickerModel{picker: updated.(tui.PickerModel), purpose: devicePickerPurposeFromContext(ctx)}
	final, err := tea.NewProgram(model, tea.WithContext(ctx)).Run()
	if err != nil {
		return "", err
	}
	selected := final.(hilSimulatorPickerModel).picker.Selected()
	if selected == nil {
		return "", ErrUserCancelled
	}
	choice, ok := selected.Value.(*simulatorChoice)
	if !ok || choice == nil || choice.Create {
		return "", fmt.Errorf("no existing simulator selected")
	}
	return choice.Name, nil
}

// A simulator-only picker avoids offering physical devices that HIL cannot use.
// Selection does not boot the VM; the normal HIL path checks networking first.
type hilSimulatorPickerModel struct {
	picker  tui.PickerModel
	purpose devicePickerPurpose
	width   int
}

func (m hilSimulatorPickerModel) Init() tea.Cmd { return m.picker.Init() }

func (m hilSimulatorPickerModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if size, ok := msg.(tea.WindowSizeMsg); ok {
		m.width = size.Width
		size.Height = max(1, size.Height-strings.Count(m.purpose.header(size.Width), "\n"))
		msg = size
	}
	updated, cmd := m.picker.Update(msg)
	m.picker = updated.(tui.PickerModel)
	return m, cmd
}

func (m hilSimulatorPickerModel) View() string {
	if m.picker.Selected() != nil || m.picker.Cancelled() {
		return ""
	}
	return m.purpose.header(m.width) + m.picker.View()
}
