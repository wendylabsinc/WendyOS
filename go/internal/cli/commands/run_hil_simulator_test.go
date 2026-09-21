package commands

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
	"github.com/wendylabsinc/wendy/go/internal/cli/vm"
)

func TestHILSimulatorSelection(t *testing.T) {
	oldInteractive, oldPicker := isInteractiveTerminalFn, pickHILSimulatorFn
	t.Cleanup(func() { isInteractiveTerminalFn, pickHILSimulatorFn = oldInteractive, oldPicker })
	calls := 0
	pickHILSimulatorFn = func(ctx context.Context) (string, error) {
		calls++
		if devicePickerPurposeFromContext(ctx) != mainDevicePicker {
			t.Fatal("missing main device purpose")
		}
		return "g1-sim", nil
	}
	isInteractiveTerminalFn = func() bool { return true }
	if name, err := resolveHILSimulator(context.Background(), "", false); name != "g1-sim" || err != nil {
		t.Fatalf("got %q, %v", name, err)
	}
	if name, err := resolveHILSimulator(context.Background(), "vm:explicit", true); name != "explicit" || err != nil {
		t.Fatalf("got %q, %v", name, err)
	}
	for _, device := range []string{"spark.local", "vm:", "vm:bad/name"} {
		if _, err := resolveHILSimulator(context.Background(), device, false); err == nil {
			t.Fatalf("accepted %q", device)
		}
	}
	for _, interactive := range []bool{true, false} {
		isInteractiveTerminalFn = func() bool { return interactive }
		if _, err := resolveHILSimulator(context.Background(), "", interactive); err == nil || !strings.Contains(err.Error(), "--device vm:NAME") {
			t.Fatalf("got %v", err)
		}
	}
	if calls != 1 {
		t.Fatalf("picker calls=%d", calls)
	}
	isInteractiveTerminalFn = func() bool { return true }
	pickHILSimulatorFn = func(context.Context) (string, error) { return "", ErrUserCancelled }
	if _, err := resolveHILSimulator(context.Background(), "", false); !errors.Is(err, ErrUserCancelled) {
		t.Fatalf("got %v", err)
	}
}

func TestHILBareFlagsSelectSimulatorWithoutPersistingTarget(t *testing.T) {
	oldInteractive, oldPicker, oldBuildPicker, oldDevice := isInteractiveTerminalFn, pickHILSimulatorFn, pickRunBuildHostDevice, deviceFlag
	t.Cleanup(func() {
		isInteractiveTerminalFn, pickHILSimulatorFn, pickRunBuildHostDevice, deviceFlag = oldInteractive, oldPicker, oldBuildPicker, oldDevice
	})
	isInteractiveTerminalFn = func() bool { return true }
	deviceFlag = ""
	selected := false
	pickHILSimulatorFn = func(context.Context) (string, error) { selected = true; return "g1-sim", nil }
	pickRunBuildHostDevice = func(context.Context) (*SelectedDevice, error) {
		return &SelectedDevice{Agent: &grpcclient.AgentConnection{Addr: "spark.local:50052"}}, nil
	}
	cmd := newRunCmd()
	// An empty project stops execution after selection, before device access.
	cmd.SetArgs([]string{"--hil", "--build-host", "--prefix", t.TempDir()})
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	if err := cmd.Execute(); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected missing project after selection, got %v", err)
	}
	if !selected || deviceFlag != "" {
		t.Fatalf("selected=%v target=%q", selected, deviceFlag)
	}
}

func TestHILSimulatorPickerSelectsStoppedVMAndCancels(t *testing.T) {
	picker := tui.NewPickerWithTitleAndColumns("Select a simulator for HIL", simulatorPickerColumns())
	updated, _ := picker.Update(tui.PickerSetMsg{Items: simulatorRows([]vm.Status{{Name: "g1-sim", Exists: true}})})
	model := hilSimulatorPickerModel{picker: updated.(tui.PickerModel), purpose: mainDevicePicker, width: 40}
	if view := model.View(); !strings.Contains(view, "MAIN DEVICE") || !strings.Contains(view, "g1-sim") {
		t.Fatalf("missing simulator context: %s", view)
	}
	chosen, cmd := model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	selected := chosen.(hilSimulatorPickerModel).picker.Selected()
	if cmd == nil || selected == nil || selected.Value.(*simulatorChoice).Name != "g1-sim" {
		t.Fatal("stopped VM was not selectable")
	}
	cancelled, cmd := model.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if cmd == nil || !cancelled.(hilSimulatorPickerModel).picker.Cancelled() || cancelled.View() != "" {
		t.Fatal("picker did not cancel")
	}
}
