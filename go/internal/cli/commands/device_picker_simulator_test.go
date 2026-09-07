package commands

import (
	"context"
	"runtime"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
	"github.com/wendylabsinc/wendy/go/internal/cli/vm"
)

func stoppedVM(name, version string) vm.Status {
	return vm.Status{
		Name:   name,
		Exists: true,
		Meta:   vm.Meta{Name: name, ImageVersion: version},
	}
}

func simulatorViewFor(t *testing.T, statuses ...vm.Status) (simulatorPickerModel, string) {
	t.Helper()
	m := newSimulatorPickerModel(context.Background())
	m, _ = m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	m, _ = m.Update(simulatorVMsMsg{vms: statuses})
	return m, m.View()
}

func TestSimulatorTabListsEveryVMWithItsState(t *testing.T) {
	_, view := simulatorViewFor(t,
		runningVM("dev", vm.NetUser, 50053),
		stoppedVM("scratch", "0.19.0"),
	)
	for _, want := range []string{"dev", "running", "127.0.0.1:50053", "scratch", "stopped"} {
		if !strings.Contains(view, want) {
			t.Errorf("simulator view = %q, want it to contain %q", view, want)
		}
	}
}

func TestSimulatorTabShowsAVersionOnAStoppedVM(t *testing.T) {
	// The reason meta and state are separate records: provenance outlives a run.
	_, view := simulatorViewFor(t, stoppedVM("scratch", "0.19.0"))
	if !strings.Contains(view, "0.19.0") {
		t.Errorf("simulator view = %q, want the version of a stopped VM", view)
	}
}

func TestSimulatorTabOffersToCreateOneWhenTheStoreIsEmpty(t *testing.T) {
	// An empty tab explains nothing; a fresh machine still gets one action.
	_, view := simulatorViewFor(t)
	for _, want := range []string{"Simulator", "not created"} {
		if !strings.Contains(view, want) {
			t.Errorf("empty simulator view = %q, want it to contain %q", view, want)
		}
	}
}

func TestSimulatorRowsMarkTheEmptyStoreRowForCreation(t *testing.T) {
	items := simulatorRows(nil)
	if len(items) != 1 {
		t.Fatalf("simulatorRows(nil) returned %d rows, want 1", len(items))
	}
	choice, ok := items[0].Value.(*simulatorChoice)
	if !ok {
		t.Fatalf("row value is %T, want *simulatorChoice", items[0].Value)
	}
	if !choice.Create || choice.Name != defaultSimulatorVMName {
		t.Errorf("choice = %+v, want Create with the default name", choice)
	}
}

func TestSimulatorRowsCarryRunningStateAndAddress(t *testing.T) {
	items := simulatorRows([]vm.Status{runningVM("dev", vm.NetUser, 50053)})
	choice := items[0].Value.(*simulatorChoice)
	if choice.Address != "127.0.0.1:50053" || choice.Create {
		t.Errorf("choice = %+v, want a running VM at 127.0.0.1:50053", choice)
	}
}

func TestSimulatorRefreshRemovesAVMThatDisappeared(t *testing.T) {
	// Set, not add: a VM removed in another terminal must stop being offered.
	m := newSimulatorPickerModel(context.Background())
	m, _ = m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	m, _ = m.Update(simulatorVMsMsg{vms: []vm.Status{stoppedVM("gone", ""), stoppedVM("stays", "")}})
	if !strings.Contains(m.View(), "gone") {
		t.Fatal("first refresh did not list the VM")
	}

	m, _ = m.Update(simulatorVMsMsg{vms: []vm.Status{stoppedVM("stays", "")}})
	if strings.Contains(m.View(), "gone") {
		t.Errorf("removed VM is still listed: %q", m.View())
	}
	if !strings.Contains(m.View(), "stays") {
		t.Errorf("surviving VM disappeared: %q", m.View())
	}
}

func TestSimulatorTabSurvivesAnUnreadableStore(t *testing.T) {
	m := newSimulatorPickerModel(context.Background())
	m, _ = m.Update(simulatorVMsMsg{err: context.DeadlineExceeded})
	if view := m.View(); !strings.Contains(view, "Could not read") {
		t.Errorf("view = %q, want it to explain the store could not be read", view)
	}
}

func TestDevicePickerShowsAllThreeTabs(t *testing.T) {
	m := newDevicePickerModel(context.Background(), tui.NewPicker(), nil, 0)
	view := m.View()
	for _, want := range []string{"Local", "Simulator", "Cloud", "tab switch"} {
		if !strings.Contains(view, want) {
			t.Errorf("picker view = %q, want it to contain %q", view, want)
		}
	}
}

func TestDevicePickerCyclesThroughSimulator(t *testing.T) {
	m := newDevicePickerModel(context.Background(), tui.NewPicker(), nil, 0)
	for _, want := range []devicePickerTab{devicePickerSimulatorTab, devicePickerCloudTab, devicePickerLocalTab} {
		updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyTab})
		m = updated.(devicePickerModel)
		if m.active != want {
			t.Fatalf("after Tab, active = %v, want %v", m.active, want)
		}
	}
}

func TestDevicePickerChoiceReportsTheConfirmingTab(t *testing.T) {
	// The regression test for the bug the tagged struct exists to prevent: a
	// child keeps a selection from an earlier visit, and a fixed-order payload
	// check would let that stale pick beat the tab the user confirmed on.
	m := newDevicePickerModel(context.Background(), tui.NewPicker(), nil, 0)
	m, _ = m.updateSimulator(tea.WindowSizeMsg{Width: 120, Height: 30})
	m, _ = m.updateSimulator(simulatorVMsMsg{vms: []vm.Status{stoppedVM("dev", "")}})
	m.active = devicePickerSimulatorTab
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(devicePickerModel)

	choice, ok := m.choice()
	if !ok {
		t.Fatal("choice() reported no selection after Enter on the Simulator tab")
	}
	if choice.Tab != devicePickerSimulatorTab {
		t.Errorf("choice.Tab = %v, want the Simulator tab", choice.Tab)
	}
	if choice.Simulator == nil || choice.Simulator.Name != "dev" {
		t.Errorf("choice.Simulator = %+v, want the selected VM", choice.Simulator)
	}
}

func TestDevicePickerChoiceIsEmptyWhenCancelled(t *testing.T) {
	m := newDevicePickerModel(context.Background(), tui.NewPicker(), nil, 0)
	m.cancelled = true
	if _, ok := m.choice(); ok {
		t.Error("choice() reported a selection after the picker was cancelled")
	}
}

func TestDevicePickerChoiceIsEmptyWhenQuittingToLogIn(t *testing.T) {
	// The login and org-switch retry flows must keep working unchanged.
	m := newDevicePickerModel(context.Background(), tui.NewPicker(), nil, 0)
	m.hasChosen, m.action = true, devicePickerLogin
	if _, ok := m.choice(); ok {
		t.Error("choice() reported a selection while quitting to log in")
	}
}

func TestSimulatorBootTimeoutScalesWithAcceleration(t *testing.T) {
	// An emulated boot takes minutes, so the budget must not be a
	// hardware-sized one; an accelerated host should not wait that long.
	got := simulatorBootTimeout()
	if vm.AccelFor(runtime.GOOS, runtime.GOARCH) == vm.AccelTCG {
		if got < 2*time.Minute {
			t.Errorf("simulatorBootTimeout() = %v, too short for an emulated boot", got)
		}
		return
	}
	if got > time.Minute+30*time.Second {
		t.Errorf("simulatorBootTimeout() = %v, longer than an accelerated boot needs", got)
	}
}

func TestSimulatorListCopiesTheAddressOnEnter(t *testing.T) {
	// The tab advertises "enter copy address". It was dead: the tabs model
	// consumed enter before the picker ever saw it, so nothing was copied.
	var copied string
	saved := clipboardWriter
	clipboardWriter = func(s string) error { copied = s; return nil }
	t.Cleanup(func() { clipboardWriter = saved })

	m := newSimulatorListModel(context.Background())
	m, _ = m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	m, _ = m.Update(simulatorVMsMsg{vms: []vm.Status{runningVM("dev", vm.NetUser, 50053)}})
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})

	if copied != "127.0.0.1:50053" {
		t.Errorf("clipboard = %q, want the VM address", copied)
	}
}

func TestSimulatorTabStopsSayingItIsScanning(t *testing.T) {
	// The store read IS the scan; without reporting it done the populated
	// table sat under a "Scanning..." banner forever.
	_, view := simulatorViewFor(t, stoppedVM("dev", "0.19.0"))
	if strings.Contains(view, "Scanning") {
		t.Errorf("simulator view = %q, want no scanning banner once the store is read", view)
	}
}

func TestNextSimulatorNameSkipsWhatExists(t *testing.T) {
	// 'c' cannot prompt for a name -- the table owns the screen -- so it picks
	// the first free one. A specific name is what `wendy vm create` is for.
	if got := nextSimulatorName(nil); got != defaultSimulatorVMName {
		t.Errorf("nextSimulatorName(empty) = %q, want %q", got, defaultSimulatorVMName)
	}
	one := []vm.Status{{Name: "sim"}}
	if got := nextSimulatorName(one); got != "sim-2" {
		t.Errorf("nextSimulatorName(sim) = %q, want sim-2", got)
	}
	two := []vm.Status{{Name: "sim"}, {Name: "sim-2"}}
	if got := nextSimulatorName(two); got != "sim-3" {
		t.Errorf("nextSimulatorName(sim, sim-2) = %q, want sim-3", got)
	}
	// A gap is reused rather than skipped past.
	gap := []vm.Status{{Name: "sim"}, {Name: "sim-3"}}
	if got := nextSimulatorName(gap); got != "sim-2" {
		t.Errorf("nextSimulatorName(sim, sim-3) = %q, want sim-2", got)
	}
}

func TestPressingCreateQuitsWithACreateChoice(t *testing.T) {
	// Create has to leave the picker: the image download runs its own progress
	// program, and two Bubble Tea programs cannot share a terminal. It surfaces
	// as a selection so every caller's existing Create handling applies.
	m := newSimulatorPickerModel(context.Background())
	if m.createRequested() {
		t.Fatal("createRequested() is true before any key was pressed")
	}
	updated, _ := m.Update(simulatorVMsMsg{vms: []vm.Status{{Name: "sim", Exists: true}}})
	m = updated
	m2, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'c'}})
	m = m2

	if !m.createRequested() {
		t.Fatal("pressing c did not request a create")
	}
	choice := m.selected()
	if choice == nil || !choice.Create {
		t.Fatalf("selected() = %+v, want a Create choice", choice)
	}
	if choice.Name != "sim-2" {
		t.Errorf("selected().Name = %q, want sim-2 alongside the existing sim", choice.Name)
	}
}

func TestRemoveRefusesARunningVMAndSaysWhichKeyFixesIt(t *testing.T) {
	// Store.Remove takes the run lock, so a running VM is refused anyway. The
	// point here is that the refusal names the key that unblocks it instead of
	// leaving the user stuck in the picker.
	saved := vmStatusFn
	vmStatusFn = func(string) (vm.Status, error) {
		return vm.Status{Exists: true, Running: true, State: vm.State{PID: 4321}}, nil
	}
	t.Cleanup(func() { vmStatusFn = saved })

	item := tui.PickerItem{Name: "dev", Value: &simulatorChoice{Name: "dev"}}
	flash, isErr, replacement := removeSimulatorRow(item)
	if !isErr {
		t.Error("removing a running VM was reported as success")
	}
	if replacement == nil {
		t.Error("the row was dropped even though the VM survived")
	}
	if !strings.Contains(flash, "press s") {
		t.Errorf("flash = %q, want it to name the key that fixes this", flash)
	}
}

func TestStopOnAStoppedVMIsANoOpNotAnError(t *testing.T) {
	// Stop-only: pressing s on the wrong row must never boot anything, and a
	// stopped VM is not an error worth colouring red.
	saved := vmStatusFn
	vmStatusFn = func(string) (vm.Status, error) { return vm.Status{Exists: true}, nil }
	t.Cleanup(func() { vmStatusFn = saved })

	flash, isErr := stopSimulatorRow(context.Background(), tui.PickerItem{Name: "dev", Value: &simulatorChoice{Name: "dev"}})
	if isErr {
		t.Errorf("stopping an already-stopped VM reported an error: %q", flash)
	}
	if !strings.Contains(flash, "not running") {
		t.Errorf("flash = %q, want it to say the VM is not running", flash)
	}
}
