package commands

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestBrewPackageHeadless(t *testing.T) {
	for _, tc := range []struct {
		name              string
		json, interactive bool
	}{
		{"json", true, true}, {"no-terminal", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			oldJSON, oldTerminal := jsonOutput, isInteractiveTerminalFn
			t.Cleanup(func() { jsonOutput, isInteractiveTerminalFn = oldJSON, oldTerminal })
			jsonOutput, isInteractiveTerminalFn = tc.json, func() bool { return tc.interactive }
			dir := t.TempDir()
			marker := filepath.Join(dir, "called")
			t.Setenv("BREW_TEST_MARKER", marker)
			t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
			script := "#!/bin/sh\n[ \"$HOMEBREW_NO_ASK\" = 1 ] || exit 9\nprintf '%s %s' \"$1\" \"$2\" > \"$BREW_TEST_MARKER\"\nexit \"${BREW_TEST_EXIT:-0}\"\n"
			if err := os.WriteFile(filepath.Join(dir, "brew"), []byte(script), 0755); err != nil {
				t.Fatal(err)
			}
			if err := brewPackageWithSpinner(context.Background(), "install", "qemu", "Installing"); err != nil {
				t.Fatal(err)
			}
			got, err := os.ReadFile(marker)
			if err != nil || string(got) != "install qemu" {
				t.Fatalf("brew invocation: %q, %v", got, err)
			}
			t.Setenv("BREW_TEST_EXIT", "7")
			if err := brewPackageWithSpinner(context.Background(), "install", "qemu", "Installing"); err == nil {
				t.Fatal("installer failure was swallowed")
			}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			if err := brewPackageWithSpinner(ctx, "install", "qemu", "Installing"); err == nil {
				t.Fatal("cancellation was swallowed")
			}
		})
	}
}
