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
	if !strings.Contains(out, "curl -fsSL https://install.wendy.dev/agent.sh | bash -s -- --network-access network") {
		t.Fatalf("plain output missing curl command with network access:\n%s", out)
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

// The Headless Mac path reuses the same renderer with a Mac label: agent.sh
// auto-detects the platform but rejects --network-access on macOS, so the Mac
// command omits it and the prose says to opt in when the app first opens.
func TestRenderLinuxDesktopInstructions_HeadlessMac(t *testing.T) {
	out := renderLinuxDesktopInstructions("", "", "", headlessMacMachineLabel, time.Time{})
	if !strings.Contains(out, "curl -fsSL https://install.wendy.dev/agent.sh | bash") {
		t.Fatalf("mac output missing curl command:\n%s", out)
	}
	if strings.Contains(out, "--network-access") {
		t.Fatalf("mac output must not pass the Linux-only access flag:\n%s", out)
	}
	if !strings.Contains(out, "Allow Other Devices") {
		t.Fatalf("mac output should say how to allow network access:\n%s", out)
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
		"install.wendy.dev/agent.sh",
		"--network-access network",
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
	if !strings.Contains(out, "curl -fsSL https://install.wendy.dev/agent.sh | bash") {
		t.Fatalf("expected plain instructions:\n%s", out)
	}
}

func stubLinuxDesktopSingleSessionConfig(t *testing.T) {
	t.Helper()
	// Valid for a day: the install checks the certificate before enrolling.
	certPEM := testCertPEM(t, time.Now().Add(24*time.Hour))
	origLoad := linuxDesktopConfigLoad
	linuxDesktopConfigLoad = func() (*config.Config, error) {
		return &config.Config{
			Auth: []config.AuthConfig{
				{
					CloudGRPC: "cloud.wendy.dev:443",
					Certificates: []config.CertificateInfo{
						{OrganizationID: 7, PemCertificate: certPEM},
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
	if got := linuxDesktopCommand("", "", linuxDesktopMachineLabel); got != "curl -fsSL https://install.wendy.dev/agent.sh | bash -s -- --network-access network" {
		t.Fatalf("plain command = %q", got)
	}
	if got := linuxDesktopCommand("", "", headlessMacMachineLabel); got != "curl -fsSL https://install.wendy.dev/agent.sh | bash" {
		t.Fatalf("plain mac command = %q", got)
	}
	if got, want := linuxDesktopCommand("tok-abc", "cloud.wendy.dev:443", headlessMacMachineLabel),
		"curl -fsSL https://install.wendy.dev/agent.sh | WENDY_ENROLLMENT_TOKEN=tok-abc WENDY_CLOUD_HOST=cloud.wendy.dev:443 bash"; got != want {
		t.Fatalf("enrolled mac command\n got: %q\nwant: %q", got, want)
	}
	got := linuxDesktopCommand("tok-abc", "cloud.wendy.dev:443", linuxDesktopMachineLabel)
	want := "curl -fsSL https://install.wendy.dev/agent.sh | WENDY_ENROLLMENT_TOKEN=tok-abc WENDY_CLOUD_HOST=cloud.wendy.dev:443 bash -s -- --network-access network"
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
		if !copyLinuxDesktopCommand("tok-xyz", "cloud.wendy.dev:443", linuxDesktopMachineLabel, true) {
			t.Fatal("expected copyLinuxDesktopCommand to report success")
		}
	})
	if copied != linuxDesktopCommand("tok-xyz", "cloud.wendy.dev:443", linuxDesktopMachineLabel) {
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

	if copyLinuxDesktopCommand("tok", "host", linuxDesktopMachineLabel, false) {
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
		if copyLinuxDesktopCommand("tok", "host", linuxDesktopMachineLabel, true) {
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

// stubDesktopEnrollment makes an install on a terminal mint the token
// "tok-xyz" without Wendy Cloud or the clipboard, and returns the sessions the
// tokens were minted with.
func stubDesktopEnrollment(t *testing.T) *[]*config.AuthConfig {
	t.Helper()
	stubEnrollPrompts(t)
	t.Setenv("WENDY_SECRET_STORE", "file")
	origTok, origClip := linuxDesktopTokenFn, clipboardWriter
	t.Cleanup(func() { linuxDesktopTokenFn, clipboardWriter = origTok, origClip })
	var minted []*config.AuthConfig
	linuxDesktopTokenFn = func(_ context.Context, auth *config.AuthConfig, _ string, _ int32) (string, time.Time, error) {
		minted = append(minted, auth)
		return "tok-xyz", time.Date(2026, 7, 7, 15, 0, 0, 0, time.UTC), nil
	}
	clipboardWriter = func(string) error { return nil }
	return &minted
}

// A Linux desktop or Mac install mints its enrollment token from the CLI's
// login, so it asks what the other installs ask. With no login it used to
// print the unenrolled command without a word; it now offers the login and
// mints the token with the session that login saved.
func TestInstallDesktopOffersLoginWhenLoggedOut(t *testing.T) {
	for _, tc := range []struct {
		name  string
		login bool // the answer to "Log in now and enroll this device?"
	}{
		{name: "logs in and enrolls", login: true},
		{name: "login declined"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			minted := stubDesktopEnrollment(t)
			stubInteractive(t)
			setTempConfig(t, &config.Config{})
			asked := 0
			confirmInstallLogin = func() (bool, error) {
				asked++
				return tc.login, nil
			}
			logins := 0
			stubEnrollmentLogin(t, func(context.Context, oidcLoginOptions) error {
				logins++
				saved := *oidcEnrollmentAuth(t)
				saved.CloudGRPC = "cloud.wendy.dev:443"
				return config.Save(&config.Config{Auth: []config.AuthConfig{saved}})
			})

			var err error
			out := captureStdout(t, func() {
				err = installDesktop(context.Background(), preEnrollOptions{mode: preEnrollAuto}, "my-box", headlessMacMachineLabel)
			})

			if err != nil {
				t.Fatalf("installDesktop: %v", err)
			}
			if asked != 1 {
				t.Fatalf("login questions = %d, want 1\n%s", asked, out)
			}
			if tc.login {
				if logins != 1 || len(*minted) != 1 || (*minted)[0].CloudGRPC != "cloud.wendy.dev:443" {
					t.Fatalf("logins = %d, tokens = %d; want one token from the session the login saved", logins, len(*minted))
				}
				if !strings.Contains(out, "WENDY_ENROLLMENT_TOKEN=tok-xyz") {
					t.Fatalf("the command lacks the enrollment token:\n%s", out)
				}
				return
			}
			if logins != 0 || len(*minted) != 0 || strings.Contains(out, "WENDY_ENROLLMENT_TOKEN") {
				t.Fatalf("logins = %d, tokens = %d; want the unenrolled command:\n%s", logins, len(*minted), out)
			}
			if !strings.Contains(out, "curl -fsSL https://install.wendy.dev/agent.sh | bash") {
				t.Fatalf("expected the unenrolled command:\n%s", out)
			}
		})
	}
}

// With an expired login the token request used to go to Wendy Cloud anyway and
// come back refused. The install now offers a new sign-in first, as the other
// installs do, and mints the token with the new certificate.
func TestInstallDesktopOffersSignInWhenLoginExpired(t *testing.T) {
	for _, tc := range []struct {
		name   string
		signIn bool // the answer to "Sign in again now and continue enrollment?"
	}{
		{name: "signed in again", signIn: true},
		{name: "sign-in declined"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			minted := stubDesktopEnrollment(t)
			withStubbedReloginDeps(t, true, false, tc.signIn, nil)
			auth := expiredEnrollmentAuth(t)
			auth.CloudGRPC = "cloud.wendy.dev:443"
			setTempConfig(t, &config.Config{Auth: []config.AuthConfig{*auth}})
			confirmPreEnroll = func() (bool, error) { return true, nil }
			freshPEM := testCertPEM(t, time.Now().Add(time.Hour))
			logins := 0
			stubEnrollmentLogin(t, func(context.Context, oidcLoginOptions) error {
				logins++
				fresh := *auth
				fresh.Certificates = append([]config.CertificateInfo(nil), auth.Certificates...)
				fresh.Certificates[0].PemCertificate = freshPEM
				return config.Save(&config.Config{Auth: []config.AuthConfig{fresh}})
			})

			var err error
			out := captureStdout(t, func() {
				err = installDesktop(context.Background(), preEnrollOptions{mode: preEnrollAuto}, "my-box", linuxDesktopMachineLabel)
			})

			if err != nil {
				t.Fatalf("installDesktop: %v", err)
			}
			if tc.signIn {
				if logins != 1 || len(*minted) != 1 || (*minted)[0].Certificates[0].PemCertificate != freshPEM {
					t.Fatalf("sign-ins = %d, tokens = %d; want one token minted with the new certificate", logins, len(*minted))
				}
				if !strings.Contains(out, "WENDY_ENROLLMENT_TOKEN=tok-xyz") {
					t.Fatalf("the command lacks the enrollment token:\n%s", out)
				}
				return
			}
			if logins != 0 || len(*minted) != 0 || strings.Contains(out, "WENDY_ENROLLMENT_TOKEN") {
				t.Fatalf("sign-ins = %d, tokens = %d; want the unenrolled command:\n%s", logins, len(*minted), out)
			}
		})
	}
}
