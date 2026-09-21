package commands

import (
	"errors"
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
)

func stubVMCreatePrompts(t *testing.T) *[]string {
	t.Helper()
	originalInteractive, originalName, originalProfile := isInteractiveTerminalFn, promptVMCreateNameFn, pickSimulatorProfileFn
	originalJSON, originalYes := jsonOutput, vmAssumeYes
	t.Cleanup(func() {
		isInteractiveTerminalFn, promptVMCreateNameFn, pickSimulatorProfileFn = originalInteractive, originalName, originalProfile
		jsonOutput, vmAssumeYes = originalJSON, originalYes
	})
	jsonOutput, vmAssumeYes = false, false
	isInteractiveTerminalFn = func() bool { return true }
	var calls []string
	promptVMCreateNameFn = func() (string, error) {
		calls = append(calls, "name")
		return "prompted-vm", nil
	}
	pickSimulatorProfileFn = func() (string, error) {
		calls = append(calls, "profile")
		return "go2", nil
	}
	return &calls
}

func TestResolveVMCreateInputsPromptsOnlyForOmittedValues(t *testing.T) {
	for _, tt := range []struct {
		name        string
		args        []string
		profile     string
		profileSet  bool
		wantName    string
		wantProfile string
		wantCalls   string
	}{
		{name: "both omitted", profile: "generic", wantName: "prompted-vm", wantProfile: "go2", wantCalls: "name,profile"},
		{name: "explicit name", args: []string{"named-vm"}, profile: "generic", wantName: "named-vm", wantProfile: "go2", wantCalls: "profile"},
		{name: "explicit generic", profile: "generic", profileSet: true, wantName: "prompted-vm", wantProfile: "generic", wantCalls: "name"},
		{name: "explicit go2", profile: "go2", profileSet: true, wantName: "prompted-vm", wantProfile: "go2", wantCalls: "name"},
		{name: "explicit g1", profile: "g1", profileSet: true, wantName: "prompted-vm", wantProfile: "g1", wantCalls: "name"},
		{name: "both explicit", args: []string{"named-vm"}, profile: "g1", profileSet: true, wantName: "named-vm", wantProfile: "g1"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			calls := stubVMCreatePrompts(t)
			name, profile, err := resolveVMCreateInputs(tt.args, tt.profile, tt.profileSet)
			if err != nil || name != tt.wantName || profile != tt.wantProfile {
				t.Fatalf("got (%q, %q, %v), want (%q, %q, nil)", name, profile, err, tt.wantName, tt.wantProfile)
			}
			if got := strings.Join(*calls, ","); got != tt.wantCalls {
				t.Fatalf("prompt order = %q, want %q", got, tt.wantCalls)
			}
		})
	}
}

func TestResolveVMCreateInputsKeepsNoninteractiveUsageScriptable(t *testing.T) {
	for _, mode := range []string{"no terminal", "json", "yes"} {
		t.Run(mode, func(t *testing.T) {
			calls := stubVMCreatePrompts(t)
			switch mode {
			case "no terminal":
				isInteractiveTerminalFn = func() bool { return false }
			case "json":
				jsonOutput = true
			case "yes":
				vmAssumeYes = true
			}
			name, profile, err := resolveVMCreateInputs([]string{"named-vm"}, "generic", false)
			if err != nil || name != "named-vm" || profile != "generic" {
				t.Fatalf("default profile: got (%q, %q, %v)", name, profile, err)
			}
			name, profile, err = resolveVMCreateInputs([]string{"named-vm"}, "g1", true)
			if err != nil || name != "named-vm" || profile != "g1" {
				t.Fatalf("explicit profile: got (%q, %q, %v)", name, profile, err)
			}
			_, _, err = resolveVMCreateInputs(nil, "generic", false)
			if err == nil || !strings.Contains(err.Error(), "wendy vm create <name>") {
				t.Fatalf("missing-name error = %v, want argument guidance", err)
			}
			if len(*calls) != 0 {
				t.Fatalf("noninteractive input unexpectedly prompted: %v", *calls)
			}
		})
	}
}

func TestResolveVMCreateInputsRejectsInvalidValuesBeforeFurtherPrompts(t *testing.T) {
	for _, tt := range []struct {
		name      string
		args      []string
		profile   string
		prompted  string
		wantCalls string
	}{
		{name: "invalid explicit profile", profile: "unknown", prompted: "prompted-vm"},
		{name: "empty explicit profile", prompted: "prompted-vm"},
		{name: "invalid explicit name", args: []string{"../outside"}, profile: "generic", prompted: "prompted-vm"},
		{name: "invalid prompted name", profile: "generic", prompted: "Bad Name", wantCalls: "name"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			calls := stubVMCreatePrompts(t)
			promptVMCreateNameFn = func() (string, error) {
				*calls = append(*calls, "name")
				return tt.prompted, nil
			}
			_, _, err := resolveVMCreateInputs(tt.args, tt.profile, tt.profile != "generic")
			if err == nil {
				t.Fatal("invalid input was accepted")
			}
			if got := strings.Join(*calls, ","); got != tt.wantCalls {
				t.Fatalf("prompt order = %q, want %q", got, tt.wantCalls)
			}
		})
	}
}

func TestVMCreateCommandStopsOnPromptCancellation(t *testing.T) {
	for _, tt := range []struct {
		name      string
		args      []string
		nameErr   error
		wantCalls string
	}{
		{name: "name cancelled", nameErr: tui.ErrCancelled, wantCalls: "name"},
		{name: "profile cancelled", wantCalls: "name,profile"},
		{name: "explicit name only", args: []string{"named-vm"}, wantCalls: "profile"},
		{name: "explicit generic only", args: []string{"--profile", "generic"}, nameErr: tui.ErrCancelled, wantCalls: "name"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			calls := stubVMCreatePrompts(t)
			promptVMCreateNameFn = func() (string, error) {
				*calls = append(*calls, "name")
				return "prompted-vm", tt.nameErr
			}
			pickSimulatorProfileFn = func() (string, error) {
				*calls = append(*calls, "profile")
				return "", ErrUserCancelled
			}
			cmd := newVMCreateCmd()
			// A command that falls through after cancellation must fail to open
			// this image, rather than download or create anything on the host.
			cmd.SetArgs(append(append([]string{}, tt.args...), "--image", t.TempDir()+"/missing.wic"))
			cmd.SetOut(&strings.Builder{})
			cmd.SetErr(&strings.Builder{})
			if err := cmd.Execute(); !errors.Is(err, ErrUserCancelled) {
				t.Fatalf("cancelled create returned %v, want ErrUserCancelled", err)
			}
			if got := strings.Join(*calls, ","); got != tt.wantCalls {
				t.Fatalf("prompt order = %q, want %q", got, tt.wantCalls)
			}
		})
	}
}

func TestVMCreateCommandHonorsExplicitGenericProfile(t *testing.T) {
	calls := stubVMCreatePrompts(t)
	cmd := newVMCreateCmd()
	// Conflicting image flags stop creation before accessing the VM store.
	cmd.SetArgs([]string{"--profile", "generic", "--image", "unused.wic", "--version", "1.2.3"})
	cmd.SetOut(&strings.Builder{})
	cmd.SetErr(&strings.Builder{})
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "--image cannot be combined") {
		t.Fatalf("create returned %v, want image flag conflict", err)
	}
	if got := strings.Join(*calls, ","); got != "name" {
		t.Fatalf("prompt order = %q, want only name for explicit --profile generic", got)
	}
}
