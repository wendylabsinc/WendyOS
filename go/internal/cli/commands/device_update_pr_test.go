package commands

import (
	"io"
	"testing"

	"github.com/spf13/cobra"
)

func TestDeviceUpdatePRFlag(t *testing.T) {
	cmd := newDeviceUpdateCmd()
	if cmd.Flags().Lookup("pr") == nil {
		t.Fatal("expected --pr flag on device update")
	}
}

func TestDeviceUpdatePRMutualExclusion(t *testing.T) {
	const mutexErr = "--pr cannot be combined with --artifact-url"
	cmd := newDeviceUpdateCmd()
	cmd.SetArgs([]string{"--pr", "123", "--artifact-url", "https://example.com/image.wendy"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error for --pr with --artifact-url")
	}
	if got := err.Error(); got != mutexErr {
		t.Errorf("unexpected error: %q; want %q", got, mutexErr)
	}
}

func TestDeviceUpdatePRJSONExclusion(t *testing.T) {
	const jsonErr = "--pr cannot be combined with --json: the OS-update step is skipped in JSON mode"
	previous := jsonOutput
	t.Cleanup(func() { jsonOutput = previous })
	root := &cobra.Command{Use: "wendy"}
	root.PersistentFlags().BoolVar(&jsonOutput, "json", false, "")
	root.AddCommand(newDeviceUpdateCmd())
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)
	root.SetArgs([]string{"--json", "update", "--pr", "123"})
	err := root.Execute()
	if err == nil {
		t.Fatal("expected error for --pr with --json")
	}
	if got := err.Error(); got != jsonErr {
		t.Errorf("unexpected error: %q; want %q", got, jsonErr)
	}
}
