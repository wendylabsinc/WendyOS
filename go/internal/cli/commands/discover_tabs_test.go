package commands

import (
	"context"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/wendylabsinc/wendy/go/proto/gen/cloudpb"
)

func newTestDiscoverTabsModel(authOrg int, defaultOrg int32) discoverTabsModel {
	ctx := context.Background()
	local := newDiscoverModel(ctx, defaultOpts(), true)
	if authOrg == 0 {
		return newDiscoverTabsModel(ctx, local, nil, defaultOrg, devicePickerLocalTab)
	}
	return newDiscoverTabsModel(ctx, local, pickerAuth(authOrg), defaultOrg, devicePickerLocalTab)
}

func TestDiscoverTabsShowsLocalAndCloud(t *testing.T) {
	m := newTestDiscoverTabsModel(0, 0)
	view := m.View()
	for _, want := range []string{"Local", "Cloud", "tab switch", "Scanning for WendyOS devices"} {
		if !strings.Contains(view, want) {
			t.Fatalf("local discover view does not contain %q: %q", want, view)
		}
	}
}

// discoverTabTo presses Tab until the model lands on want, so a test that cares
// about one tab does not encode how many tabs precede it.
func discoverTabTo(t *testing.T, m discoverTabsModel, want devicePickerTab) (discoverTabsModel, tea.Cmd) {
	t.Helper()
	var cmd tea.Cmd
	for range len(deviceTabOrder()) {
		if m.active == want {
			return m, cmd
		}
		var updated tea.Model
		updated, cmd = m.Update(tea.KeyMsg{Type: tea.KeyTab})
		m = updated.(discoverTabsModel)
	}
	if m.active != want {
		t.Fatalf("never reached tab %v", want)
	}
	return m, cmd
}

func TestDiscoverTabsLoggedOutCloudOffersLogin(t *testing.T) {
	m := newTestDiscoverTabsModel(0, 0)
	m, _ = discoverTabTo(t, m, devicePickerCloudTab)

	view := m.View()
	for _, want := range []string{"Discover cloud devices", "Wendy Cloud login", "Not logged in"} {
		if !strings.Contains(view, want) {
			t.Fatalf("logged-out cloud discover view does not contain %q: %q", want, view)
		}
	}

	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(discoverTabsModel)
	if cmd == nil || m.action != devicePickerLogin {
		t.Fatalf("login row result: cmd=%v action=%v", cmd != nil, m.action)
	}
}

func TestDiscoverTabsStartsCloudLazilyAndShowsDefaultOrg(t *testing.T) {
	m := newTestDiscoverTabsModel(7, 7)
	if m.cloudStarted {
		t.Fatal("cloud discovery started before visiting the Cloud tab")
	}

	m, cmd := discoverTabTo(t, m, devicePickerCloudTab)
	if cmd == nil || !m.cloudStarted {
		t.Fatalf("first Cloud visit: cmd=%v started=%v", cmd != nil, m.cloudStarted)
	}
	updated, _ := m.Update(discoverTabsOrgMsg{name: "Robotics"})
	m = updated.(discoverTabsModel)
	for _, want := range []string{"Organization: Robotics (org 7)", "default", "o switch"} {
		if !strings.Contains(m.View(), want) {
			t.Fatalf("cloud discover view does not contain %q: %q", want, m.View())
		}
	}
}

func TestDiscoverTabsOrgSwitchHotkey(t *testing.T) {
	m := newTestDiscoverTabsModel(7, 7)
	m.active = devicePickerCloudTab

	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'o'}})
	m = updated.(discoverTabsModel)
	if cmd == nil || m.action != devicePickerSwitchOrg {
		t.Fatalf("org switch result: cmd=%v action=%v", cmd != nil, m.action)
	}
}

func TestDiscoverTabsCloudRosterUsesCloudDiscoverDashboard(t *testing.T) {
	m := newTestDiscoverTabsModel(7, 7)
	m.active = devicePickerCloudTab
	asset := &cloudpb.Asset{Id: 42, Name: "cloud-pi"}

	updated, _ := m.Update(discoverTabsCloudMsg{msg: cloudScanMsg{assets: []*cloudpb.Asset{asset}}})
	m = updated.(discoverTabsModel)
	view := m.View()
	for _, want := range []string{"cloud-pi", "enter copy", "a copy all", "u update"} {
		if !strings.Contains(view, want) {
			t.Fatalf("cloud roster does not contain %q: %q", want, view)
		}
	}
}

func TestTagDiscoverTabsCmdPreservesBatchChildren(t *testing.T) {
	cmd := tagDiscoverTabsCmd(tea.Batch(
		func() tea.Msg { return "first" },
		func() tea.Msg { return "second" },
	), devicePickerLocalTab)
	batch, ok := cmd().(tea.BatchMsg)
	if !ok {
		t.Fatalf("tagged batch returned unexpected message type")
	}
	if len(batch) != 2 {
		t.Fatalf("tagged batch children = %d, want 2", len(batch))
	}
	for i, child := range batch {
		msg, ok := child().(discoverTabsLocalMsg)
		if !ok {
			t.Fatalf("child %d returned unexpected message type", i)
		}
		if msg.msg == nil {
			t.Fatalf("child %d lost its payload", i)
		}
	}
}

func TestDiscoverTabsShowsTheSimulatorTab(t *testing.T) {
	// The Simulator tab is the only surface that lists a STOPPED VM: a Local
	// tab row needs an answering agent, so a VM that is off or still booting
	// never appears there.
	m := newTestDiscoverTabsModel(0, 0)
	if view := m.View(); !strings.Contains(view, "Simulator") {
		t.Errorf("discover header = %q, want a Simulator tab", view)
	}
	m, _ = discoverTabTo(t, m, devicePickerSimulatorTab)
	if view := m.View(); !strings.Contains(view, "enter copy address") {
		t.Errorf("simulator tab view = %q, want the copy hint", view)
	}
}

func TestCopySimulatorAddressExplainsWhatItCannotCopy(t *testing.T) {
	// Points at the key, not the command: c creates one right here now.
	if got := copySimulatorAddress(&simulatorChoice{Name: "sim", Create: true}); !strings.Contains(got, "press c") {
		t.Errorf("create row = %q, want it to point at the c key", got)
	}
	if got := copySimulatorAddress(&simulatorChoice{Name: "sim"}); !strings.Contains(got, "vm start sim") {
		t.Errorf("stopped VM = %q, want it to name 'wendy vm start sim'", got)
	}
}

func TestDiscoverOpensOnTheRequestedTab(t *testing.T) {
	// Creating a VM leaves and re-enters this view. Coming back on Local drops
	// the user somewhere they did not ask to be, with no sign the create ran.
	ctx := context.Background()
	m := newDiscoverTabsModel(ctx, newDiscoverModel(ctx, defaultOpts(), true), nil, 0, devicePickerSimulatorTab)
	if m.active != devicePickerSimulatorTab {
		t.Errorf("active = %v, want the Simulator tab", m.active)
	}
	// The list polls only from the first time its tab is shown, so opening
	// straight onto it has to start that up front or the table stays empty.
	if !m.simStarted {
		t.Error("opening on the Simulator tab did not start its polling")
	}
	stubVMStatuses(t, stoppedVM("created", "test"))
	// Execute the actual initial simulator commands, not just the flag which
	// previously claimed polling had started without scheduling anything.
	batch, ok := m.Init()().(tea.BatchMsg)
	if !ok || len(batch) != 2 {
		t.Fatal("Init must schedule local and simulator initialization")
	}
	var found bool
	var walk func(tea.Cmd)
	walk = func(cmd tea.Cmd) {
		if cmd == nil {
			return
		}
		switch msg := cmd().(type) {
		case tea.BatchMsg:
			for _, child := range msg {
				walk(child)
			}
		case discoverTabsSimulatorMsg:
			if rows, ok := msg.msg.(simulatorVMsMsg); ok {
				found = true
				if len(rows.vms) != 1 || rows.vms[0].Name != "created" {
					t.Fatalf("wrong initial rows: %+v", rows)
				}
				updated, next := m.Update(msg)
				if next == nil || !strings.Contains(updated.View(), "created") {
					t.Fatal("initial rows did not render/schedule refresh")
				}
			}
		}
	}
	walk(batch[1])
	if !found {
		t.Fatal("Init did not poll simulator store")
	}
}

func TestDiscoverStillDefaultsToLocal(t *testing.T) {
	m := newTestDiscoverTabsModel(0, 0)
	if m.active != devicePickerLocalTab {
		t.Errorf("active = %v, want Local", m.active)
	}
	if m.simStarted {
		t.Error("the simulator poll started before its tab was ever shown")
	}
}
