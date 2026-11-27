package commands

import (
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func TestExplicitArtifactCannotUseAgentOnlyJSONExit(t *testing.T) {
	previous := jsonOutput
	t.Cleanup(func() { jsonOutput = previous })

	root := &cobra.Command{Use: "wendy"}
	root.PersistentFlags().BoolVar(&jsonOutput, "json", false, "")
	update := newDeviceUpdateCmd()
	root.AddCommand(update)

	// The root command auto-enables JSON for a non-TTY. An explicit OS
	// artifact must clear that mode before either agent-status JSON return.
	jsonOutput = true
	if err := prepareExplicitDeviceOSOutput(update, "https://example.com/image.wendy", 0, false); err != nil {
		t.Fatalf("auto-JSON explicit artifact: %v", err)
	}
	if jsonOutput {
		t.Fatal("auto-JSON still enabled: device update would return before applying the OS artifact")
	}

	// A user-requested --json=true is different: fail before updating the
	// agent, with a clear error instead of producing a misleading success.
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)
	root.SetArgs([]string{"update", "--artifact-url", "https://example.com/image.wendy", "--json", "--yes"})
	err := root.Execute()
	if err == nil || !strings.Contains(err.Error(), "cannot be combined with --json") {
		t.Fatalf("device update explicit --json error = %v, want conflict before connection", err)
	}
	if !jsonOutput {
		t.Fatal("explicit --json setting was silently overridden")
	}
}

func TestAutomaticDeviceUpdateKeepsNonTTYJSON(t *testing.T) {
	previous := jsonOutput
	t.Cleanup(func() { jsonOutput = previous })
	jsonOutput = true
	if err := prepareExplicitDeviceOSOutput(newDeviceUpdateCmd(), "", 0, false); err != nil {
		t.Fatal(err)
	}
	if !jsonOutput {
		t.Fatal("automatic agent-only JSON behavior unexpectedly changed")
	}
}

func TestAutomaticJSONYieldsToYesAndPR(t *testing.T) {
	previous := jsonOutput
	t.Cleanup(func() { jsonOutput = previous })
	for _, c := range []struct {
		name string
		pr   int
		yes  bool
	}{{"--yes", 0, true}, {"--pr", 123, false}, {"--pr --yes", 123, true}} {
		jsonOutput = true
		if err := prepareExplicitDeviceOSOutput(newDeviceUpdateCmd(), "", c.pr, c.yes); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if jsonOutput {
			t.Fatalf("%s: automatic JSON kept, so the OS step would be skipped", c.name)
		}
	}
}

func TestExplicitJSONStaysAgentOnly(t *testing.T) {
	previous := jsonOutput
	t.Cleanup(func() { jsonOutput = previous })
	root := &cobra.Command{Use: "wendy"}
	root.PersistentFlags().BoolVar(&jsonOutput, "json", false, "")
	update := newDeviceUpdateCmd()
	root.AddCommand(update)
	if err := root.PersistentFlags().Set("json", "true"); err != nil {
		t.Fatal(err)
	}

	if err := prepareExplicitDeviceOSOutput(update, "", 0, true); err != nil || !jsonOutput {
		t.Fatalf("--json --yes: err %v, JSON %v; want agent-only JSON", err, jsonOutput)
	}
	const prErr = "--pr cannot be combined with --json: the OS-update step is skipped in JSON mode"
	if err := prepareExplicitDeviceOSOutput(update, "", 123, true); err == nil || err.Error() != prErr {
		t.Fatalf("--json --pr: err %v, want %q", err, prErr)
	}
}

// Both agent paths return early in JSON mode, so cover each of them.
func TestDeviceUpdateYesRunsTheOSStepWithoutATerminal(t *testing.T) {
	for _, c := range []struct {
		name  string
		agent *refusingAgent
	}{{"agent updated", &refusingAgent{}}, {"agent current", &refusingAgent{approved: true}}} {
		t.Run(c.name, func(t *testing.T) {
			run := runDeviceUpdate(t, c.agent, true, "--yes")
			if run.err != nil || !run.osStep || !run.assumeYes || strings.Contains(run.out, `"status"`) {
				t.Fatalf("want text output and an OS step that applies, got err %v, OS step %v, --yes %v\n%s", run.err, run.osStep, run.assumeYes, run.out)
			}
		})
	}
}

func TestDeviceUpdateExplicitJSONYesUpdatesOnlyTheAgent(t *testing.T) {
	for _, c := range []struct {
		name   string
		agent  *refusingAgent
		status string
	}{{"agent updated", &refusingAgent{}, "success"}, {"agent current", &refusingAgent{approved: true}, "up-to-date"}} {
		t.Run(c.name, func(t *testing.T) {
			run := runDeviceUpdate(t, c.agent, true, "--json", "--yes")
			if run.err != nil || run.osStep || !strings.Contains(run.out, `"status":"`+c.status+`"`) {
				t.Fatalf("want agent-only JSON, got err %v, OS step %v\n%s", run.err, run.osStep, run.out)
			}
		})
	}
}

// Leaving automatic JSON must not hand a run with no terminal to the device
// picker: a missing --device stays the documented no_device error.
func TestDeviceUpdateWithoutATerminalOrDeviceIsNoDevice(t *testing.T) {
	restoreDeviceGlobals(t)
	prevTTY := isInteractiveTerminalFn
	t.Cleanup(func() { isInteractiveTerminalFn = prevTTY })
	isInteractiveTerminalFn = func() bool { return false }
	t.Setenv("WENDY_DEVICE", "")
	t.Setenv("WENDY_AGENT_SOCKET", "")
	t.Setenv("WENDY_CONFIG_DIR", t.TempDir())
	for _, args := range [][]string{
		{"--yes"},
		{"--nightly", "--yes"},
		{"--pr", "5", "--yes"},
		{"--artifact-url", "https://example.com/image.wendy", "--yes"},
	} {
		deviceFlag, jsonOutput = "", true
		root := &cobra.Command{Use: "wendy", SilenceUsage: true, SilenceErrors: true}
		root.PersistentFlags().BoolVar(&jsonOutput, "json", true, "")
		root.AddCommand(newDeviceUpdateCmd())
		root.SetArgs(append([]string{"update"}, args...))
		if err := root.Execute(); !errors.Is(err, errNoDevice) {
			t.Fatalf("%v: err = %v, want errNoDevice", args, err)
		}
	}
}

func TestDeviceUpdatePRWithoutATerminalNeedsYes(t *testing.T) {
	prevTTY := isInteractiveTerminalFn
	t.Cleanup(func() { isInteractiveTerminalFn = prevTTY })
	isInteractiveTerminalFn = func() bool { return false }
	for _, json := range []bool{true, false} {
		agent := &refusingAgent{}
		run := runDeviceUpdate(t, agent, json, "--pr", "123")
		var usage *usageError
		if !errors.As(run.err, &usage) || run.err.Error() != "--pr requires --yes when not run from a terminal" || run.osStep || agent.approved {
			t.Fatalf("json=%v: want a usage refusal before the agent upload, got err %v, OS step %v, agent updated %v", json, run.err, run.osStep, agent.approved)
		}
	}
	run := runDeviceUpdate(t, &refusingAgent{}, true, "--pr", "123", "--yes")
	if run.err != nil || !run.osStep || !run.assumeYes || run.prNumber != 123 {
		t.Fatalf("--pr --yes: want the PR OS step with --yes, got err %v, OS step %v, --yes %v, PR %d\n%s", run.err, run.osStep, run.assumeYes, run.prNumber, run.out)
	}
}
