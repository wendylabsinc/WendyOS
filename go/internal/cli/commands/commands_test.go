package commands

import (
	"testing"

	"github.com/wendylabsinc/wendy/proto/gen/agentpb"
)

func TestResolveRestartPolicy_Default(t *testing.T) {
	opts := runOptions{}
	rp := resolveRestartPolicy(opts)
	if rp == nil {
		t.Fatal("resolveRestartPolicy returned nil")
	}
	if rp.Mode != agentpb.RestartPolicyMode_DEFAULT {
		t.Errorf("Mode = %v; want DEFAULT", rp.Mode)
	}
}

func TestResolveRestartPolicy_UnlessStopped(t *testing.T) {
	opts := runOptions{restartUnlessStopped: true}
	rp := resolveRestartPolicy(opts)
	if rp.Mode != agentpb.RestartPolicyMode_UNLESS_STOPPED {
		t.Errorf("Mode = %v; want UNLESS_STOPPED", rp.Mode)
	}
}

func TestResolveRestartPolicy_OnFailure(t *testing.T) {
	opts := runOptions{restartOnFailure: true}
	rp := resolveRestartPolicy(opts)
	if rp.Mode != agentpb.RestartPolicyMode_ON_FAILURE {
		t.Errorf("Mode = %v; want ON_FAILURE", rp.Mode)
	}
}

func TestResolveRestartPolicy_NoRestart(t *testing.T) {
	opts := runOptions{noRestart: true}
	rp := resolveRestartPolicy(opts)
	if rp.Mode != agentpb.RestartPolicyMode_NO {
		t.Errorf("Mode = %v; want NO", rp.Mode)
	}
}

func TestResolveRestartPolicy_UnlessStoppedTakesPrecedence(t *testing.T) {
	// When multiple flags are set, restartUnlessStopped should win (checked first).
	opts := runOptions{
		restartUnlessStopped: true,
		restartOnFailure:     true,
		noRestart:            true,
	}
	rp := resolveRestartPolicy(opts)
	if rp.Mode != agentpb.RestartPolicyMode_UNLESS_STOPPED {
		t.Errorf("Mode = %v; want UNLESS_STOPPED (should take precedence)", rp.Mode)
	}
}

func TestNewRunCmd(t *testing.T) {
	cmd := newRunCmd()
	if cmd.Use != "run" {
		t.Errorf("Use = %q; want %q", cmd.Use, "run")
	}
	if cmd.Short == "" {
		t.Error("Short should not be empty")
	}

	// Verify expected flags exist.
	expectedFlags := []string{"debug", "deploy", "detach", "restart-unless-stopped", "restart-on-failure", "no-restart", "user-args"}
	for _, name := range expectedFlags {
		if cmd.Flags().Lookup(name) == nil {
			t.Errorf("missing flag %q", name)
		}
	}
}

func TestNewBuildCmd(t *testing.T) {
	cmd := newBuildCmd()
	if cmd.Use != "build" {
		t.Errorf("Use = %q; want %q", cmd.Use, "build")
	}
	if cmd.Short == "" {
		t.Error("Short should not be empty")
	}
}

func TestNewDeviceCmd(t *testing.T) {
	cmd := newDeviceCmd()
	if cmd.Use != "device" {
		t.Errorf("Use = %q; want %q", cmd.Use, "device")
	}
	if cmd.Short == "" {
		t.Error("Short should not be empty")
	}

	// Verify subcommands exist.
	subCmds := cmd.Commands()
	subNames := make(map[string]bool)
	for _, c := range subCmds {
		subNames[c.Name()] = true
	}

	expectedSubs := []string{"version", "set-default", "unset-default", "setup", "update"}
	for _, name := range expectedSubs {
		if !subNames[name] {
			t.Errorf("device command missing subcommand %q", name)
		}
	}
}

func TestNewAuthCmd(t *testing.T) {
	cmd := newAuthCmd()
	if cmd.Use != "auth" {
		t.Errorf("Use = %q; want %q", cmd.Use, "auth")
	}
	if cmd.Short == "" {
		t.Error("Short should not be empty")
	}

	subCmds := cmd.Commands()
	subNames := make(map[string]bool)
	for _, c := range subCmds {
		subNames[c.Name()] = true
	}

	expectedSubs := []string{"login", "logout", "refresh-certs"}
	for _, name := range expectedSubs {
		if !subNames[name] {
			t.Errorf("auth command missing subcommand %q", name)
		}
	}
}
