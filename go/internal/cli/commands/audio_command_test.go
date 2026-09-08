package commands

import (
	"io"
	"strings"
	"testing"
)

func TestShouldOpenAudioSetDefaultTUI(t *testing.T) {
	tests := []struct {
		name        string
		idSet       bool
		interactive bool
		json        bool
		want        bool
	}{
		{name: "interactive without id", interactive: true, want: true},
		{name: "interactive with id", idSet: true, interactive: true},
		{name: "non-interactive without id"},
		{name: "json on interactive terminal", interactive: true, json: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := shouldOpenAudioSetDefaultTUI(tt.idSet, tt.interactive, tt.json); got != tt.want {
				t.Fatalf("shouldOpenAudioSetDefaultTUI(%v, %v, %v) = %v, want %v", tt.idSet, tt.interactive, tt.json, got, tt.want)
			}
		})
	}
}

func TestAudioSetDefaultStillRequiresIDNonInteractively(t *testing.T) {
	originalInteractive := isInteractiveTerminalFn
	isInteractiveTerminalFn = func() bool { return false }
	t.Cleanup(func() { isInteractiveTerminalFn = originalInteractive })

	cmd := newAudioSetDefaultCmd()
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), `required flag(s) "id" not set`) {
		t.Fatalf("error = %v, want missing id error", err)
	}
}
