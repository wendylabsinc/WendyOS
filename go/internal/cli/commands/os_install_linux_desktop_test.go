//go:build darwin || linux || windows

package commands

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/config"
)

func TestRenderLinuxDesktopInstructions_Plain(t *testing.T) {
	out := renderLinuxDesktopInstructions("", "", "", linuxDesktopMachineLabel, time.Time{})
	if !strings.Contains(out, "curl -fsSL https://install.wendy.sh/agent.sh | bash") {
		t.Fatalf("plain output missing curl command:\n%s", out)
	}
	if strings.Contains(out, "WENDY_ENROLLMENT_TOKEN") {
		t.Fatalf("plain output should not mention a token:\n%s", out)
	}
	if !strings.Contains(out, "wendy device enroll") {
		t.Fatalf("plain output should point at later enrollment:\n%s", out)
	}
	if !strings.Contains(out, "Linux machine") {
		t.Fatalf("plain output should name the Linux machine target:\n%s", out)
	}
}

// The Headless Mac path reuses the same renderer with a Mac label: the command
// is byte-for-byte identical (agent.sh auto-detects the platform) but the
// surrounding prose names the Mac.
func TestRenderLinuxDesktopInstructions_HeadlessMac(t *testing.T) {
	out := renderLinuxDesktopInstructions("", "", "", headlessMacMachineLabel, time.Time{})
	if !strings.Contains(out, "curl -fsSL https://install.wendy.sh/agent.sh | bash") {
		t.Fatalf("mac output missing curl command:\n%s", out)
	}
	if !strings.Contains(out, "your Mac") {
		t.Fatalf("mac output should name the Mac target:\n%s", out)
	}
	if strings.Contains(out, "Linux machine") {
		t.Fatalf("mac output should not mention a Linux machine:\n%s", out)
	}
}

func TestRenderLinuxDesktopInstructions_Enrolled(t *testing.T) {
	exp := time.Date(2026, 7, 7, 15, 4, 5, 0, time.UTC)
	out := renderLinuxDesktopInstructions("tok-abc", "cloud.wendy.dev:443", "Acme", linuxDesktopMachineLabel, exp)
	for _, want := range []string{
		"WENDY_ENROLLMENT_TOKEN=tok-abc",
		"WENDY_CLOUD_HOST=cloud.wendy.dev:443",
		"install.wendy.sh/agent.sh",
		"Acme",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("enrolled output missing %q:\n%s", want, out)
		}
	}
}

func TestInstallLinuxDesktop_SkipMode_PrintsPlain(t *testing.T) {
	// preEnrollSkip must never mint a token, even if a token fn is present.
	called := false
	origTok := linuxDesktopTokenFn
	linuxDesktopTokenFn = func(_ context.Context, _ *config.AuthConfig, _ string, _ int32) (string, time.Time, error) {
		called = true
		return "should-not-be-used", time.Time{}, nil
	}
	t.Cleanup(func() { linuxDesktopTokenFn = origTok })

	out := captureStdout(t, func() {
		if err := installDesktop(context.Background(), preEnrollOptions{mode: preEnrollSkip}, "", linuxDesktopMachineLabel); err != nil {
			t.Fatalf("installDesktop: %v", err)
		}
	})
	if called {
		t.Fatal("token fn must not be called in skip mode")
	}
	if !strings.Contains(out, "curl -fsSL https://install.wendy.sh/agent.sh | bash") {
		t.Fatalf("expected plain instructions:\n%s", out)
	}
}

func stubLinuxDesktopSingleSessionConfig(t *testing.T) {
	t.Helper()
	origLoad := linuxDesktopConfigLoad
	linuxDesktopConfigLoad = func() (*config.Config, error) {
		return &config.Config{
			Auth: []config.AuthConfig{
				{
					CloudGRPC: "cloud.wendy.dev:443",
					Certificates: []config.CertificateInfo{
						{OrganizationID: 7},
					},
				},
			},
		}, nil
	}
	t.Cleanup(func() { linuxDesktopConfigLoad = origLoad })

	origResolveOrg := resolveOrgFn
	resolveOrgFn = func(_ context.Context, _ *config.AuthConfig, _ bool) (OrgResolution, error) {
		return OrgResolution{ID: 7, Name: "Acme"}, nil
	}
	t.Cleanup(func() { resolveOrgFn = origResolveOrg })
}

func TestInstallLinuxDesktop_EnrolledPrintsToken(t *testing.T) {
	stubLinuxDesktopSingleSessionConfig(t)

	var gotDeviceName string
	var gotOrgID int32
	origTok := linuxDesktopTokenFn
	linuxDesktopTokenFn = func(_ context.Context, _ *config.AuthConfig, deviceName string, orgID int32) (string, time.Time, error) {
		gotDeviceName = deviceName
		gotOrgID = orgID
		return "tok-xyz", time.Date(2026, 7, 7, 15, 0, 0, 0, time.UTC), nil
	}
	t.Cleanup(func() { linuxDesktopTokenFn = origTok })

	out := captureStdout(t, func() {
		if err := installDesktop(context.Background(), preEnrollOptions{mode: preEnrollForced}, "my-box", linuxDesktopMachineLabel); err != nil {
			t.Fatalf("installDesktop: %v", err)
		}
	})

	if gotDeviceName != "my-box" {
		t.Fatalf("token fn deviceName = %q, want %q", gotDeviceName, "my-box")
	}
	if gotOrgID != 7 {
		t.Fatalf("token fn orgID = %d, want 7", gotOrgID)
	}
	for _, want := range []string{
		"WENDY_ENROLLMENT_TOKEN=tok-xyz",
		"WENDY_CLOUD_HOST=cloud.wendy.dev:443",
		"Acme",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("expected output to contain %q:\n%s", want, out)
		}
	}
}

// TestInstallLinuxDesktop_EnrolledValidatesDeviceName pins down Finding 1
// concretely: the enroll path must route deviceName through
// resolveDeviceName (so an interactive user gets validated/prompted for a
// name) rather than handing the raw flag straight to the token minter. A
// syntactically invalid name is rejected by resolveDeviceName's validation
// before linuxDesktopTokenFn is ever reached; prior to the fix the raw value
// sailed through unchecked and the token fn stub below would have been
// called.
func TestInstallLinuxDesktop_EnrolledValidatesDeviceName(t *testing.T) {
	stubLinuxDesktopSingleSessionConfig(t)

	called := false
	origTok := linuxDesktopTokenFn
	linuxDesktopTokenFn = func(_ context.Context, _ *config.AuthConfig, _ string, _ int32) (string, time.Time, error) {
		called = true
		return "should-not-be-used", time.Time{}, nil
	}
	t.Cleanup(func() { linuxDesktopTokenFn = origTok })

	err := installDesktop(context.Background(), preEnrollOptions{mode: preEnrollForced}, "BadName", linuxDesktopMachineLabel)
	if err == nil {
		t.Fatal("expected an error for an invalid --device-name, got nil")
	}
	if !strings.Contains(err.Error(), "device name") {
		t.Fatalf("expected error to mention device name validation, got: %v", err)
	}
	if called {
		t.Fatal("token fn must not be called when the device name fails validation")
	}
}

func TestLinuxDesktopCommand(t *testing.T) {
	if got := linuxDesktopCommand("", ""); got != "curl -fsSL https://install.wendy.sh/agent.sh | bash" {
		t.Fatalf("plain command = %q", got)
	}
	got := linuxDesktopCommand("tok-abc", "cloud.wendy.dev:443")
	want := "curl -fsSL https://install.wendy.sh/agent.sh | WENDY_ENROLLMENT_TOKEN=tok-abc WENDY_CLOUD_HOST=cloud.wendy.dev:443 bash"
	if got != want {
		t.Fatalf("enrolled command\n got: %q\nwant: %q", got, want)
	}
	// The copyable command must be a single line so it pastes and runs directly.
	if strings.ContainsAny(got, "\n\\") {
		t.Fatalf("copyable command must be a single line without continuations: %q", got)
	}
}

func TestCopyLinuxDesktopCommand_Interactive(t *testing.T) {
	var copied string
	orig := clipboardWriter
	clipboardWriter = func(s string) error { copied = s; return nil }
	t.Cleanup(func() { clipboardWriter = orig })

	out := captureStdout(t, func() {
		if !copyLinuxDesktopCommand("tok-xyz", "cloud.wendy.dev:443", true) {
			t.Fatal("expected copyLinuxDesktopCommand to report success")
		}
	})
	if copied != linuxDesktopCommand("tok-xyz", "cloud.wendy.dev:443") {
		t.Fatalf("clipboard got %q", copied)
	}
	if !strings.Contains(out, "clipboard") {
		t.Fatalf("expected a copied-to-clipboard hint, got:\n%s", out)
	}
}

func TestCopyLinuxDesktopCommand_NonInteractive(t *testing.T) {
	called := false
	orig := clipboardWriter
	clipboardWriter = func(string) error { called = true; return nil }
	t.Cleanup(func() { clipboardWriter = orig })

	if copyLinuxDesktopCommand("tok", "host", false) {
		t.Fatal("must not report success when non-interactive")
	}
	if called {
		t.Fatal("must not touch the clipboard when non-interactive")
	}
}

func TestCopyLinuxDesktopCommand_ClipboardFailure(t *testing.T) {
	orig := clipboardWriter
	clipboardWriter = func(string) error { return errors.New("no clipboard tool") }
	t.Cleanup(func() { clipboardWriter = orig })

	out := captureStdout(t, func() {
		if copyLinuxDesktopCommand("tok", "host", true) {
			t.Fatal("must not report success when the clipboard write fails")
		}
	})
	if strings.Contains(out, "clipboard") {
		t.Fatalf("must not claim it copied when the write failed:\n%s", out)
	}
}

func TestInstallLinuxDesktop_ForcedNonInteractive_TokenError(t *testing.T) {
	stubLinuxDesktopSingleSessionConfig(t)

	origTok := linuxDesktopTokenFn
	linuxDesktopTokenFn = func(_ context.Context, _ *config.AuthConfig, _ string, _ int32) (string, time.Time, error) {
		return "", time.Time{}, errors.New("boom")
	}
	t.Cleanup(func() { linuxDesktopTokenFn = origTok })

	err := installDesktop(context.Background(), preEnrollOptions{mode: preEnrollForced}, "my-box", linuxDesktopMachineLabel)
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Fatalf("expected error to contain %q, got: %v", "boom", err)
	}
}
