package commands

import (
	"io"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func calibrateTestCommand(t *testing.T, interactive bool) (*cobra.Command, *[]string, *int, *int) {
	t.Helper()
	oldLocal, oldRemote := armCalibrationRunLocal, armCalibrationRunRemote
	oldInteractive, oldJSON, oldDevice := isInteractiveTerminalFn, jsonOutput, deviceFlag
	t.Cleanup(func() {
		armCalibrationRunLocal, armCalibrationRunRemote = oldLocal, oldRemote
		isInteractiveTerminalFn, jsonOutput, deviceFlag = oldInteractive, oldJSON, oldDevice
	})
	isInteractiveTerminalFn = func() bool { return interactive }
	jsonOutput, deviceFlag = false, ""
	var argv []string
	local, remote := 0, 0
	armCalibrationRunLocal = func(cmd *cobra.Command, args []string) error { argv = args; local++; return nil }
	armCalibrationRunRemote = func(cmd *cobra.Command, args []string) error { argv = args; remote++; return nil }
	root := &cobra.Command{Use: "wendy"}
	root.AddGroup(&cobra.Group{ID: "manage", Title: "Manage:"})
	root.PersistentFlags().BoolVar(&jsonOutput, "json", false, "JSON")
	root.PersistentFlags().StringVar(&deviceFlag, "device", "", "device")
	root.AddCommand(newCalibrateCmd())
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)
	return root, &argv, &local, &remote
}

func TestCalibrateCommandIsInRoot(t *testing.T) {
	cmd, _, err := NewRootCmd().Find([]string{"calibrate", "arms"})
	if err != nil || cmd.CommandPath() != "wendy calibrate arms" {
		t.Fatalf("missing command: %v %v", cmd, err)
	}
}

func TestCalibrateArmsForwardsSelectedDeviceArguments(t *testing.T) {
	root, argv, local, remote := calibrateTestCommand(t, true)
	root.SetArgs([]string{"calibrate", "arms", "6", "--device", "thor", "--model", "big_yam", "--channel", "can3", "--id", "right-arm", "--gripper", "none", "--output", "/path with spaces"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	if *local != 0 || *remote != 1 || deviceFlag != "thor" {
		t.Fatal("wrong launch target")
	}
	joined := strings.Join((*argv)[5:], "|")
	if joined != "arms|6|--model|big_yam|--id|right-arm|--channel|can3|--gripper|none|--output|/path with spaces" {
		t.Fatalf("wrong runtime arguments: %s", joined)
	}
}

func TestCalibrateDemoAndProfilesRemainOffline(t *testing.T) {
	for _, args := range [][]string{{"calibrate", "arms", "6", "--demo", "--auto", "--json"}, {"calibrate", "profiles", "--json"}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			root, _, local, remote := calibrateTestCommand(t, false)
			root.SetArgs(args)
			if err := root.Execute(); err != nil {
				t.Fatal(err)
			}
			if *local != 1 || *remote != 0 {
				t.Fatal("offline command contacted hardware")
			}
		})
	}
}

func TestCalibrateInvalidRequestNeverLaunches(t *testing.T) {
	for _, flags := range [][]string{
		{"7"}, {"0"}, {"six"}, {"6", "--auto"}, {"6", "--model", "so101"},
		{"6", "--channel", "can0; reboot"}, {"6", "--id", "../wrong"}, {"6", "--gripper", "crank_4310"},
		{"6", "--demo", "--device", "thor"}, {"6", "--local", "--device", "thor"},
	} {
		t.Run(strings.Join(flags, " "), func(t *testing.T) {
			root, _, local, remote := calibrateTestCommand(t, true)
			root.SetArgs(append([]string{"calibrate", "arms"}, flags...))
			if err := root.Execute(); err == nil {
				t.Fatal("invalid request accepted")
			}
			if *local != 0 || *remote != 0 {
				t.Fatal("invalid request launched a runtime")
			}
		})
	}
}

func TestCalibrateHardwareRequiresTerminal(t *testing.T) {
	root, _, local, remote := calibrateTestCommand(t, false)
	root.SetArgs([]string{"calibrate", "arms", "6", "--device", "thor"})
	if err := root.Execute(); err == nil || !strings.Contains(err.Error(), "terminal") {
		t.Fatal("missing terminal gate")
	}
	if *local != 0 || *remote != 0 {
		t.Fatal("noninteractive hardware request launched")
	}
}
