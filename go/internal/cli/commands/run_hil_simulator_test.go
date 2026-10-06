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

// WENDY_DEVICE usually names the real device — the --hil peer — so HIL takes
// its simulator from the variable only when the variable names one. An
// explicit --device keeps its meaning: HIL still refuses a non-simulator.
func TestHILDeviceSelectorIgnoresANonSimulatorWENDY_DEVICE(t *testing.T) {
	restoreDeviceGlobals(t)
	for _, tc := range []struct {
		name, flag, env, want string
	}{
		{"env names the real device", "", "wendyos-thor.local", ""},
		{"env names a simulator", "", "vm:dev", "vm:dev"},
		{"env names the simulator alias", "", "sim", "sim"},
		{"env names a malformed simulator", "", "vm:bad/name", "vm:bad/name"},
		{"explicit real device", "wendyos-thor.local", "", "wendyos-thor.local"},
		{"explicit device outranks env", "wendyos-thor.local", "vm:dev", "wendyos-thor.local"},
		{"explicit simulator", "vm:dev", "wendyos-thor.local", "vm:dev"},
		{"neither", "", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			deviceFlag = tc.flag
			t.Setenv(deviceEnvVar, tc.env)
			applyDeviceEnv()
			if got := hilDeviceSelector(); got != tc.want {
				t.Fatalf("hilDeviceSelector() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestHILWithWENDY_DEVICEOpensTheSimulatorPicker(t *testing.T) {
	restoreDeviceGlobals(t)
	oldInteractive, oldPicker := isInteractiveTerminalFn, pickHILSimulatorFn
	t.Cleanup(func() { isInteractiveTerminalFn, pickHILSimulatorFn = oldInteractive, oldPicker })
	isInteractiveTerminalFn = func() bool { return true }
	selected := false
	pickHILSimulatorFn = func(context.Context) (string, error) { selected = true; return "g1-sim", nil }

	run := func(t *testing.T, args ...string) error {
		t.Helper()
		cmd := newRunCmd()
		cmd.SetArgs(append(args, "--prefix", t.TempDir()))
		cmd.SetOut(io.Discard)
		cmd.SetErr(io.Discard)
		return cmd.Execute()
	}

	t.Run("real device in WENDY_DEVICE", func(t *testing.T) {
		selected, deviceFlag = false, ""
		t.Setenv(deviceEnvVar, "wendyos-thor.local")
		applyDeviceEnv()
		// An empty project stops execution after selection, before device access.
		if err := run(t, "--hil=wendyos-thor.local"); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("expected missing project after simulator selection, got %v", err)
		}
		if !selected || deviceFlag != "wendyos-thor.local" {
			t.Fatalf("selected=%v target=%q", selected, deviceFlag)
		}
	})
	t.Run("simulator in WENDY_DEVICE", func(t *testing.T) {
		selected, deviceFlag = false, ""
		t.Setenv(deviceEnvVar, "vm:dev")
		applyDeviceEnv()
		if err := run(t, "--hil=wendyos-thor.local"); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("expected missing project after simulator selection, got %v", err)
		}
		if selected {
			t.Fatal("opened the picker although WENDY_DEVICE names a simulator")
		}
	})
	t.Run("explicit real device", func(t *testing.T) {
		selected, deviceFlag = false, "wendyos-thor.local"
		t.Setenv(deviceEnvVar, "")
		applyDeviceEnv()
		err := run(t, "--hil=wendyos-thor.local")
		if err == nil || !strings.Contains(err.Error(), "HIL requires a simulator") {
			t.Fatalf("explicit --device wendyos-thor.local: got %v", err)
		}
		if selected {
			t.Fatal("opened the picker for an explicit --device")
		}
	})
}
