package commands

import (
	"context"
	"fmt"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
	"github.com/wendylabsinc/wendy/go/internal/cli/vm"
)

// defaultSimulatorVMName is the VM provisioned for someone who never named one.
const defaultSimulatorVMName = "sim"

// simulatorRefreshInterval re-reads the store while the tab is open, so a VM
// started or stopped in another terminal shows up. The read is a directory scan
// plus a small file per VM, so this is cheap.
const simulatorRefreshInterval = 2 * time.Second

// simulatorChoice is what selecting a Simulator row means.
type simulatorChoice struct {
	Name   string
	Create bool // the store is empty: provision before starting

	// Address is the forwarded agent address, empty unless the VM is running
	// with a port forward.
	Address string
}

type simulatorVMsMsg struct {
	vms []vm.Status
	err error
}

type simulatorPickerModel struct {
	picker tui.PickerModel
	err    error
	loaded bool

	// create is set by the OnCreateItem closure, and vms is the store contents
	// the new VM's name is chosen against. Pointers because Bubble Tea passes
	// models by value, so a closure cannot write to a plain field.
	create *bool
	vms    *[]vm.Status
}

func newSimulatorPickerModel(ctx context.Context) simulatorPickerModel {
	m := simulatorPickerModel{
		picker: tui.NewPickerWithTitleAndColumns("Select a simulator", simulatorPickerColumns()),
	}
	m.picker.RemoveHint = "remove"
	m.picker.OnStopItem = func(item tui.PickerItem) (string, bool) { return stopSimulatorRow(ctx, item) }
	m.picker.OnRemoveItem = removeSimulatorRow
	// Quits the picker: creating downloads an image behind its own progress
	// program, and two Bubble Tea programs cannot share a terminal. The caller
	// creates the VM and comes back.
	m.create, m.vms = new(bool), new([]vm.Status)
	create, vms := m.create, m.vms
	m.picker.OnCreateItem = func() (string, bool) {
		*create = true
		return fmt.Sprintf("Creating %s...", nextSimulatorName(*vms)), true
	}
	return m
}

// vmStatusFn reads one VM's state. A seam, like vmStatusesFn: the row actions
// below are the only place a test can reach them, and they must not touch the
// developer's real VM store.
var vmStatusFn = func(name string) (vm.Status, error) {
	store, err := vm.NewStore()
	if err != nil {
		return vm.Status{}, err
	}
	return store.Status(name)
}

// stopSimulatorRow powers 's'. Stop only: a mis-press on the wrong row can then
// never boot something, and starting already has a home (enter, or
// `wendy vm start`).
func stopSimulatorRow(ctx context.Context, item tui.PickerItem) (string, bool) {
	choice, _ := item.Value.(*simulatorChoice)
	if choice == nil || choice.Create {
		return "", false
	}
	st, err := vmStatusFn(choice.Name)
	if err != nil {
		return err.Error(), true
	}
	if !st.Running {
		return fmt.Sprintf("%s is not running", choice.Name), false
	}
	store, err := vm.NewStore()
	if err != nil {
		return err.Error(), true
	}
	if err := store.StopContext(ctx, choice.Name, false, vmStopGrace); err != nil {
		return err.Error(), true
	}
	return fmt.Sprintf("Stopped %s.", choice.Name), false
}

// removeSimulatorRow powers 'r'. Store.Remove takes the run lock, so a running
// VM is refused rather than deleted out from under its emulator -- say which
// key fixes that rather than making the user guess.
func removeSimulatorRow(item tui.PickerItem) (string, bool, *tui.PickerItem) {
	choice, _ := item.Value.(*simulatorChoice)
	if choice == nil || choice.Create {
		return "", false, &item
	}
	if st, err := vmStatusFn(choice.Name); err == nil && st.Running {
		return fmt.Sprintf("%s is running; press s to stop it first", choice.Name), true, &item
	}
	store, err := vm.NewStore()
	if err != nil {
		return err.Error(), true, &item
	}
	if err := store.Remove(choice.Name); err != nil {
		return err.Error(), true, &item
	}
	return fmt.Sprintf("Removed %s.", choice.Name), false, nil
}

// newSimulatorListModel is the read-only variant `wendy discover` shows: enter
// copies the address instead of selecting, matching what every other row in
// that view does.
func newSimulatorListModel(ctx context.Context) simulatorPickerModel {
	m := newSimulatorPickerModel(ctx)
	m.picker.OnCopyItem = func(item tui.PickerItem) string {
		choice, _ := item.Value.(*simulatorChoice)
		return copySimulatorAddress(choice)
	}
	return m
}

// nextSimulatorName picks the name 'c' creates under. Auto-named rather than
// prompted: the picker cannot host a text prompt while the table owns the
// screen, and a specific name is what `wendy vm create` is for.
func nextSimulatorName(existing []vm.Status) string {
	taken := make(map[string]bool, len(existing))
	for _, st := range existing {
		taken[st.Name] = true
	}
	if !taken[defaultSimulatorVMName] {
		return defaultSimulatorVMName
	}
	for i := 2; ; i++ {
		name := fmt.Sprintf("%s-%d", defaultSimulatorVMName, i)
		if !taken[name] {
			return name
		}
	}
}

// refreshCmd reads the store. Scheduled from Init rather than on the tab
// switch: a lazy start would return a non-nil cmd from the tab handler, which
// is how the Cloud tab signals its one-time discovery start.
func (m simulatorPickerModel) refreshCmd() tea.Cmd {
	return func() tea.Msg {
		statuses, err := vmStatusesFn()
		return simulatorVMsMsg{vms: statuses, err: err}
	}
}

func (m simulatorPickerModel) Init() tea.Cmd {
	// The picker's own Init drives its spinner; without it the table renders
	// under a "Scanning..." banner that never clears.
	return tea.Batch(m.picker.Init(), m.refreshCmd())
}

// simulatorPickerColumns describes a VM rather than a network device. The
// device column set is wrong here: VM rows have no USB, no mTLS and no probe
// state, and their key attribute -- running or stopped -- has no home in it.
func simulatorPickerColumns() []tui.PickerColumn {
	return []tui.PickerColumn{
		{Title: "Name", MinWidth: 12, Required: true, Value: func(i tui.PickerItem) string { return i.Name }},
		{Title: "State", MinWidth: 8, Required: true, Value: func(i tui.PickerItem) string { return i.Type }},
		{Title: "Address", MinWidth: 16, Value: func(i tui.PickerItem) string { return i.Address }},
		{Title: "Version", MinWidth: 10, Value: func(i tui.PickerItem) string { return i.OSVersion }},
	}
}

// simulatorRows turns the store's contents into picker rows. An empty store
// still offers one row, so a fresh machine has something to select rather than
// an empty tab that explains nothing.
func simulatorRows(statuses []vm.Status) []tui.PickerItem {
	if len(statuses) == 0 {
		return []tui.PickerItem{{
			Name: "Simulator",
			Type: "not created",
			// Says c, not enter: enter copies in the discover view, while c
			// creates in both.
			Hint: "press c to create it — this downloads the WendyOS image once",
			Value: &simulatorChoice{
				Name:   defaultSimulatorVMName,
				Create: true,
			},
		}}
	}
	items := make([]tui.PickerItem, 0, len(statuses))
	for _, st := range statuses {
		choice := &simulatorChoice{Name: st.Name, Address: vmAddress(st)}
		items = append(items, tui.PickerItem{
			Name:      st.Name,
			Type:      vmStateLabel(st),
			Address:   choice.Address,
			OSVersion: st.Meta.ImageVersion,
			Value:     choice,
		})
	}
	return items
}

func (m simulatorPickerModel) Update(msg tea.Msg) (simulatorPickerModel, tea.Cmd) {
	switch msg := msg.(type) {
	case simulatorVMsMsg:
		m.loaded, m.err = true, msg.err
		if msg.err == nil {
			// Kept so 'c' can name the new VM against what already exists.
			if m.vms != nil {
				*m.vms = msg.vms
			}
			// Set, not add: a VM removed elsewhere has to disappear rather
			// than linger as a row that cannot be selected.
			updated, cmd := m.picker.Update(tui.PickerSetMsg{Items: simulatorRows(msg.vms)})
			m.picker = updated.(tui.PickerModel)
			// The store read is the scan, and it just finished: without this the
			// banner claims it is still looking.
			done, doneCmd := m.picker.Update(tui.PickerDoneMsg{})
			m.picker = done.(tui.PickerModel)
			return m, tea.Batch(cmd, doneCmd, delayThen(simulatorRefreshInterval, m.refreshCmd()))
		}
		return m, delayThen(simulatorRefreshInterval, m.refreshCmd())
	default:
		updated, cmd := m.picker.Update(msg)
		m.picker = updated.(tui.PickerModel)
		return m, cmd
	}
}

func (m simulatorPickerModel) View() string {
	if !m.loaded {
		return "Looking for local VMs..."
	}
	if m.err != nil {
		return "Could not read the VM store: " + m.err.Error()
	}
	return m.picker.View()
}

// createRequested reports that 'c' was pressed, which quits the picker so the
// download can own the terminal.
func (m simulatorPickerModel) createRequested() bool {
	return m.create != nil && *m.create
}

func (m simulatorPickerModel) selected() *simulatorChoice {
	// A create reads as selecting a VM that does not exist yet, so every
	// caller's existing Create handling applies without a second code path.
	if m.createRequested() {
		return &simulatorChoice{Name: nextSimulatorName(*m.vms), Create: true}
	}
	item := m.picker.Selected()
	if item == nil {
		return nil
	}
	choice, _ := item.Value.(*simulatorChoice)
	return choice
}

func (m simulatorPickerModel) cancelled() bool { return m.picker.Cancelled() }
