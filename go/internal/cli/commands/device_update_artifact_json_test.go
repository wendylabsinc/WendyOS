package commands

import (
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
	if err := prepareExplicitDeviceOSOutput(update, "https://example.com/image.wendy"); err != nil {
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
	if err := prepareExplicitDeviceOSOutput(newDeviceUpdateCmd(), ""); err != nil {
		t.Fatal(err)
	}
	if !jsonOutput {
		t.Fatal("automatic agent-only JSON behavior unexpectedly changed")
	}
}
