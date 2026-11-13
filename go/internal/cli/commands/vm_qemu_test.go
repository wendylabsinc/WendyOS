package commands

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/cli/vm"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
)

// stubQEMUMissing makes the emulator look absent, and records whether an
// install was attempted.
func stubQEMUMissing(t *testing.T, installed *bool) {
	t.Helper()
	savedLook, savedInstall := findQEMUFn, installQEMUFn
	findQEMUFn = func(string) (string, error) { return "", errors.New("not found") }
	installQEMUFn = func(context.Context) error {
		*installed = true
		return nil
	}
	t.Cleanup(func() { findQEMUFn, installQEMUFn = savedLook, savedInstall })
}

// stubTerminal forces the interactive check either way; the shared
// stubInteractive only covers the true case.
func stubTerminal(t *testing.T, interactive bool) {
	t.Helper()
	prev := isInteractiveTerminalFn
	isInteractiveTerminalFn = func() bool { return interactive }
	t.Cleanup(func() { isInteractiveTerminalFn = prev })
}

func stubQEMUConfirmAnswer(t *testing.T, confirm func(string) bool) {
	t.Helper()
	saved := qemuConfirmFn
	qemuConfirmFn = func(_ context.Context, question string) (bool, error) { return confirm(question), nil }
	t.Cleanup(func() { qemuConfirmFn = saved })
}

func TestEnsureQEMUIsANoOpWhenAlreadyInstalled(t *testing.T) {
	saved := findQEMUFn
	findQEMUFn = func(name string) (string, error) { return "/usr/bin/" + name, nil }
	t.Cleanup(func() { findQEMUFn = saved })

	asked := false
	stubQEMUConfirmAnswer(t, func(string) bool { asked = true; return true })
	if err := ensureQEMUForHostOS(context.Background(), "darwin"); err != nil {
		t.Fatalf("ensureQEMUForHostOS() = %v, want nil", err)
	}
	if asked {
		t.Error("prompted even though QEMU is already installed")
	}
}

func TestEnsureQEMUOffersToInstallOnInteractiveMac(t *testing.T) {
	var installed, asked bool
	stubQEMUMissing(t, &installed)
	stubBrewLookPath(t, func(n string) (string, error) { return "/opt/homebrew/bin/" + n, nil })
	stubTerminal(t, true)
	stubQEMUConfirmAnswer(t, func(question string) bool {
		asked = true
		if question != "Do you want to install QEMU?" {
			t.Errorf("question = %q", question)
		}
		return true
	})

	// The stub keeps reporting the binary as missing, so the post-install PATH
	// re-check is what this lands on -- which is the point: an install that
	// does not put QEMU on PATH must say so rather than claim success.
	err := ensureQEMUForHostOS(context.Background(), "darwin")
	if !asked {
		t.Error("did not prompt before installing")
	}
	if !installed {
		t.Error("accepted the prompt but never ran the install")
	}
	if err == nil || !strings.Contains(err.Error(), "not yet on PATH") {
		t.Errorf("ensureQEMUForHostOS() = %v, want the PATH re-check error", err)
	}
}

func TestEnsureQEMUDoesNotPromptWithoutHomebrew(t *testing.T) {
	var installed, asked bool
	stubQEMUMissing(t, &installed)
	stubBrewLookPath(t, func(string) (string, error) { return "", errors.New("not found") })
	stubTerminal(t, true)
	stubQEMUConfirmAnswer(t, func(string) bool { asked = true; return true })

	err := ensureQEMUForHostOS(context.Background(), "darwin")
	if asked || installed {
		t.Error("prompted or installed on a host with no Homebrew")
	}
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("ensureQEMUForHostOS() = %v, want the plain not-found error", err)
	}
}

func TestEnsureQEMUDoesNotPromptWhenNotInteractive(t *testing.T) {
	var installed, asked bool
	stubQEMUMissing(t, &installed)
	stubBrewLookPath(t, func(n string) (string, error) { return "/opt/homebrew/bin/" + n, nil })
	stubTerminal(t, false)
	stubQEMUConfirmAnswer(t, func(string) bool { asked = true; return true })

	if err := ensureQEMUForHostOS(context.Background(), "darwin"); err == nil {
		t.Fatal("ensureQEMUForHostOS() = nil, want the not-found error")
	}
	if asked || installed {
		t.Error("prompted or installed without a terminal to prompt on")
	}
}

func TestEnsureQEMUDeclinedKeepsTheManualCommand(t *testing.T) {
	var installed, asked bool
	stubQEMUMissing(t, &installed)
	stubBrewLookPath(t, func(n string) (string, error) { return "/opt/homebrew/bin/" + n, nil })
	stubTerminal(t, true)
	stubQEMUConfirmAnswer(t, func(string) bool { asked = true; return false })

	err := ensureQEMUForHostOS(context.Background(), "darwin")
	if !asked {
		t.Error("never prompted")
	}
	if installed {
		t.Error("installed despite the prompt being declined")
	}
	if err == nil || !strings.Contains(err.Error(), qemuInstallHintForHostOS("darwin")) {
		t.Errorf("ensureQEMUForHostOS() = %v, want the Homebrew install command", err)
	}
}

// stubLinuxHost describes a Linux host with only onPath installed, running as
// root or not, and passes each package-manager command to install instead of
// running it.
func stubLinuxHost(t *testing.T, root bool, onPath []string, install func([]string) error) {
	t.Helper()
	savedLook, savedEUID, savedInstall := linuxLookPathFn, linuxGeteuidFn, installLinuxPackagesFn
	t.Cleanup(func() { linuxLookPathFn, linuxGeteuidFn, installLinuxPackagesFn = savedLook, savedEUID, savedInstall })
	linuxLookPathFn = func(name string) (string, error) {
		if slices.Contains(onPath, name) {
			return "/usr/bin/" + name, nil
		}
		return "", exec.ErrNotFound
	}
	linuxGeteuidFn = func() int {
		if root {
			return 0
		}
		return 1000
	}
	installLinuxPackagesFn = func(_ context.Context, command []string) error { return install(command) }
}

func TestEnsureQEMUOnLinux(t *testing.T) {
	aptGet := []string{"sudo", "apt-get", "install", "-y", "qemu-system-arm", "qemu-efi-aarch64"}
	installFailed := errors.New("exit status 100")
	for _, tc := range []struct {
		name                   string
		onPath                 []string
		root, interactive, yes bool
		decline, stillMissing  bool
		confirmErr, installErr error
		wantAsked              bool
		wantCommand            []string
		wantErr                error
		wantMessage            string
	}{
		{name: "apt-get", onPath: []string{"sudo", "apt-get"}, interactive: true, wantAsked: true, wantCommand: aptGet},
		{name: "dnf", onPath: []string{"sudo", "dnf"}, interactive: true, wantAsked: true,
			wantCommand: []string{"sudo", "dnf", "install", "-y", "qemu-system-aarch64", "edk2-aarch64"}},
		{name: "apt-get before dnf", onPath: []string{"sudo", "dnf", "apt-get"}, interactive: true, wantAsked: true, wantCommand: aptGet},
		{name: "root needs no sudo", onPath: []string{"apt-get"}, root: true, interactive: true, wantAsked: true,
			wantCommand: []string{"apt-get", "install", "-y", "qemu-system-arm", "qemu-efi-aarch64"}},
		{name: "no sudo", onPath: []string{"apt-get"}, interactive: true, wantMessage: qemuInstallHintForHostOS("linux")},
		{name: "no package manager", onPath: []string{"sudo"}, interactive: true, wantMessage: qemuInstallHintForHostOS("linux")},
		{name: "declined", onPath: []string{"sudo", "apt-get"}, interactive: true, decline: true, wantAsked: true, wantMessage: qemuInstallHintForHostOS("linux")},
		{name: "cancelled prompt", onPath: []string{"sudo", "apt-get"}, interactive: true, confirmErr: ErrUserCancelled, wantAsked: true, wantErr: ErrUserCancelled},
		{name: "no terminal", onPath: []string{"sudo", "apt-get"}, wantMessage: "qemu-system-aarch64 not found"},
		// sudo needs a person to ask: --yes alone never starts it.
		{name: "yes without a terminal", onPath: []string{"sudo", "apt-get"}, yes: true, wantMessage: "qemu-system-aarch64 not found"},
		{name: "yes still asks", onPath: []string{"sudo", "apt-get"}, interactive: true, yes: true, wantAsked: true, wantCommand: aptGet},
		{name: "install failed", onPath: []string{"sudo", "apt-get"}, interactive: true, installErr: installFailed, wantAsked: true,
			wantCommand: aptGet, wantErr: installFailed, wantMessage: "installing QEMU via apt-get"},
		{name: "still missing", onPath: []string{"sudo", "apt-get"}, interactive: true, stillMissing: true, wantAsked: true,
			wantCommand: aptGet, wantMessage: "still not on PATH"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var asked, installed bool
			var command []string
			savedFind, savedYes := findQEMUFn, vmAssumeYes
			t.Cleanup(func() { findQEMUFn, vmAssumeYes = savedFind, savedYes })
			findQEMUFn = func(hostOS string) (string, error) {
				if hostOS != "linux" {
					t.Fatalf("looked up QEMU for %q", hostOS)
				}
				if installed && !tc.stillMissing {
					return "/usr/bin/qemu-system-aarch64", nil
				}
				return "", errors.New("not found")
			}
			stubLinuxHost(t, tc.root, tc.onPath, func(c []string) error {
				installed, command = true, c
				return tc.installErr
			})
			saved := qemuConfirmFn
			qemuConfirmFn = func(_ context.Context, question string) (bool, error) {
				asked = true
				if question != "Do you want to install QEMU?" {
					t.Errorf("question = %q", question)
				}
				return !tc.decline, tc.confirmErr
			}
			t.Cleanup(func() { qemuConfirmFn = saved })
			stubTerminal(t, tc.interactive)
			vmAssumeYes = tc.yes

			err := ensureQEMUForHostOS(context.Background(), "linux")
			if asked != tc.wantAsked || !slices.Equal(command, tc.wantCommand) {
				t.Fatalf("asked=%t command=%q, want asked=%t command=%q", asked, command, tc.wantAsked, tc.wantCommand)
			}
			if tc.wantErr == nil && tc.wantMessage == "" {
				if err != nil {
					t.Fatalf("ensureQEMUForHostOS() = %v, want nil", err)
				}
				return
			}
			if err == nil || (tc.wantErr != nil && !errors.Is(err, tc.wantErr)) || !strings.Contains(err.Error(), tc.wantMessage) {
				t.Fatalf("ensureQEMUForHostOS() = %v, want %v containing %q", err, tc.wantErr, tc.wantMessage)
			}
		})
	}
}

func TestQEMUInstallHintNamesBothLinuxPackageManagers(t *testing.T) {
	hint := qemuInstallHintForHostOS("linux")
	for _, want := range []string{"apt install qemu-system-arm qemu-efi-aarch64", "dnf install qemu-system-aarch64 edk2-aarch64"} {
		if !strings.Contains(hint, want) {
			t.Errorf("Linux hint = %q, want it to name %q", hint, want)
		}
	}
}

func TestEnsureQEMUOnWindows(t *testing.T) {
	for _, tc := range []struct {
		name                                                      string
		installed, interactive, yes, winget, accept, stillMissing bool
		cancelBefore, cancelPrompt, cancelInstall                 bool
		installErr                                                error
		wantAsked, wantInstall, wantError                         bool
	}{
		{name: "already installed", installed: true},
		{name: "accepted", interactive: true, winget: true, accept: true, wantAsked: true, wantInstall: true},
		{name: "declined", interactive: true, winget: true, wantAsked: true, wantError: true},
		{name: "noninteractive", winget: true, wantError: true},
		{name: "explicit yes", yes: true, winget: true, wantInstall: true},
		{name: "no winget", interactive: true, wantError: true},
		{name: "installer failed", interactive: true, winget: true, accept: true, installErr: errors.New("install failed"), wantAsked: true, wantInstall: true, wantError: true},
		{name: "still missing", interactive: true, winget: true, accept: true, stillMissing: true, wantAsked: true, wantInstall: true, wantError: true},
		{name: "cancelled before lookup", cancelBefore: true, wantError: true},
		{name: "cancelled prompt", interactive: true, winget: true, accept: true, cancelPrompt: true, wantAsked: true, wantError: true},
		{name: "cancelled install", yes: true, winget: true, cancelInstall: true, wantInstall: true, wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			savedFind, savedWinget := findQEMUFn, wingetLookPathFn
			savedInstall, savedConfirm, savedYes := installWindowsQEMUFn, qemuConfirmFn, vmAssumeYes
			t.Cleanup(func() {
				findQEMUFn, wingetLookPathFn = savedFind, savedWinget
				installWindowsQEMUFn, qemuConfirmFn, vmAssumeYes = savedInstall, savedConfirm, savedYes
			})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var asked, installed bool
			findQEMUFn = func(hostOS string) (string, error) {
				if hostOS != "windows" {
					t.Fatalf("looked up QEMU for %q", hostOS)
				}
				if tc.installed || installed && !tc.stillMissing {
					return `C:\Program Files\qemu\qemu-system-aarch64.exe`, nil
				}
				return "", errors.New("not found")
			}
			wingetLookPathFn = func(string) (string, error) {
				if tc.winget {
					return "winget.exe", nil
				}
				return "", errors.New("not found")
			}
			qemuConfirmFn = func(context.Context, string) (bool, error) {
				asked = true
				if tc.cancelPrompt {
					cancel()
				}
				return tc.accept, nil
			}
			installWindowsQEMUFn = func(context.Context) error {
				installed = true
				if tc.cancelInstall {
					cancel()
				}
				return tc.installErr
			}
			vmAssumeYes = tc.yes
			stubTerminal(t, tc.interactive)
			if tc.cancelBefore {
				cancel()
			}
			err := ensureQEMUForHostOS(ctx, "windows")
			if (err != nil) != tc.wantError || asked != tc.wantAsked || installed != tc.wantInstall {
				t.Fatalf("err=%v asked=%t installed=%t", err, asked, installed)
			}
			if tc.cancelBefore || tc.cancelPrompt || tc.cancelInstall {
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("err=%v, want cancellation", err)
				}
			}
			if tc.installErr != nil && !errors.Is(err, tc.installErr) {
				t.Fatalf("lost installer error: %v", err)
			}
			if err != nil && !tc.wantInstall && !tc.cancelBefore && !tc.cancelPrompt {
				message := err.Error()
				if !strings.Contains(message, "winget install --id SoftwareFreedomConservancy.QEMU") || !strings.Contains(message, "x86_64") {
					t.Fatalf("missing Windows install or guest architecture guidance: %s", message)
				}
			}
		})
	}
}

func TestEnsureQEMUWithYesSkipsThePromptAndInstalls(t *testing.T) {
	var installed, asked bool
	stubQEMUMissing(t, &installed)
	stubBrewLookPath(t, func(n string) (string, error) { return "/opt/homebrew/bin/" + n, nil })
	stubTerminal(t, false)
	stubQEMUConfirmAnswer(t, func(string) bool { asked = true; return false })

	saved := vmAssumeYes
	vmAssumeYes = true
	t.Cleanup(func() { vmAssumeYes = saved })

	_ = ensureQEMUForHostOS(context.Background(), "darwin")
	if asked {
		t.Error("prompted despite --yes")
	}
	if !installed {
		t.Error("--yes did not install QEMU")
	}
}

func TestVMCommandHasAYesFlag(t *testing.T) {
	if newVMCmd().PersistentFlags().Lookup("yes") == nil {
		t.Error("wendy vm has no --yes flag")
	}
}

func TestQEMUInstallHintIsTheOnlySourceInHelp(t *testing.T) {
	// The help text used to carry its own copy of the install command, which
	// then drifted from the error a user actually hits.
	if long := newVMCmd().Long; !strings.Contains(long, qemuInstallHint()) {
		t.Errorf("vm help = %q, want it to name %q", long, qemuInstallHint())
	}
}

func TestEnsureQEMUCancelledDoesNotInstall(t *testing.T) {
	var installed bool
	stubQEMUMissing(t, &installed)
	stubBrewLookPath(t, func(n string) (string, error) { return "/opt/homebrew/bin/" + n, nil })
	stubTerminal(t, true)
	saved := qemuConfirmFn
	qemuConfirmFn = func(context.Context, string) (bool, error) { return false, ErrUserCancelled }
	t.Cleanup(func() { qemuConfirmFn = saved })

	if err := ensureQEMUForHostOS(context.Background(), "darwin"); !errors.Is(err, ErrUserCancelled) {
		t.Fatalf("err = %v, want cancellation", err)
	}
	if installed {
		t.Fatal("installed QEMU after cancellation")
	}
}

func TestQEMURecoveryStopsOnContextCancellation(t *testing.T) {
	for _, hostOS := range []string{"darwin", "linux"} {
		for _, firmware := range []bool{false, true} {
			for _, beforePrompt := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/firmware=%t/before_prompt=%t", hostOS, firmware, beforePrompt), func(t *testing.T) {
					var installed, asked bool
					stubQEMUMissing(t, &installed)
					stubTerminal(t, true)
					stubBrewLookPath(t, func(string) (string, error) { return "/test/brew", nil })
					stubLinuxHost(t, false, []string{"sudo", "apt-get"}, func([]string) error { installed = true; return nil })
					savedConfirm, savedYes := qemuConfirmFn, vmAssumeYes
					savedFind, savedInstall := findQEMUFirmwareFn, installQEMUFirmwareFn
					t.Cleanup(func() {
						qemuConfirmFn, vmAssumeYes = savedConfirm, savedYes
						findQEMUFirmwareFn, installQEMUFirmwareFn = savedFind, savedInstall
					})
					vmAssumeYes = false
					findQEMUFirmwareFn = func(string, string) (string, error) { return "", vm.ErrFirmwareNotFound }
					installQEMUFirmwareFn = func(context.Context) error { installed = true; return nil }
					ctx, cancel := context.WithCancel(context.Background())
					defer cancel()
					qemuConfirmFn = func(promptCtx context.Context, _ string) (bool, error) {
						asked = true
						if promptCtx != ctx {
							t.Fatal("confirmation did not receive the operation context")
						}
						cancel()
						return true, nil
					}
					if beforePrompt {
						cancel()
					}

					var err error
					if firmware {
						_, err = ensureQEMUFirmwareForHostOS(ctx, hostOS, "/test/homebrew")
					} else {
						err = ensureQEMUForHostOS(ctx, hostOS)
					}
					if !errors.Is(err, context.Canceled) || installed || asked == beforePrompt {
						t.Fatalf("err=%v installed=%t asked=%t, want cancellation with no install and asked=%t", err, installed, asked, !beforePrompt)
					}
				})
			}
		}
	}
}

func TestEnsureQEMUFirmwareRecovery(t *testing.T) {
	for _, tc := range []struct {
		name, hostOS     string
		interactive, yes bool
		brew, accept     bool
		alreadyInstalled bool
		stillMissing     bool
		confirmErr       error
		installErr       error
		wantAsked        bool
		wantInstalled    bool
		wantErr          error
	}{
		{name: "already available", hostOS: "darwin", alreadyInstalled: true},
		{name: "accepted repair", hostOS: "darwin", interactive: true, brew: true, accept: true, wantAsked: true, wantInstalled: true},
		{name: "declined repair", hostOS: "darwin", interactive: true, brew: true, wantAsked: true, wantErr: vm.ErrFirmwareNotFound},
		{name: "cancelled prompt", hostOS: "darwin", interactive: true, brew: true, confirmErr: ErrUserCancelled, wantAsked: true, wantErr: ErrUserCancelled},
		{name: "missing after repair", hostOS: "darwin", interactive: true, brew: true, accept: true, stillMissing: true, wantAsked: true, wantInstalled: true, wantErr: vm.ErrFirmwareNotFound},
		{name: "cancelled install", hostOS: "darwin", interactive: true, brew: true, accept: true, installErr: ErrUserCancelled, wantAsked: true, wantInstalled: true, wantErr: ErrUserCancelled},
		{name: "no terminal", hostOS: "darwin", brew: true, wantErr: vm.ErrFirmwareNotFound},
		{name: "explicit yes", hostOS: "darwin", brew: true, yes: true, wantInstalled: true},
		{name: "no Homebrew", hostOS: "darwin", interactive: true, accept: true, wantErr: vm.ErrFirmwareNotFound},
		{name: "Linux without a package manager", hostOS: "linux", interactive: true, brew: true, accept: true, wantErr: vm.ErrFirmwareNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			savedFind, savedInstall := findQEMUFirmwareFn, installQEMUFirmwareFn
			savedConfirm, savedYes := qemuConfirmFn, vmAssumeYes
			t.Cleanup(func() {
				findQEMUFirmwareFn, installQEMUFirmwareFn = savedFind, savedInstall
				qemuConfirmFn, vmAssumeYes = savedConfirm, savedYes
			})
			var asked, installed bool
			findCalls := 0
			findQEMUFirmwareFn = func(hostOS, prefix string) (string, error) {
				findCalls++
				if hostOS != tc.hostOS || prefix != "/test/homebrew" {
					t.Fatalf("firmware discovery used host %q and prefix %q", hostOS, prefix)
				}
				if tc.alreadyInstalled || installed && !tc.stillMissing {
					return "/test/arm64.fd", nil
				}
				return "", vm.ErrFirmwareNotFound
			}
			installQEMUFirmwareFn = func(context.Context) error {
				installed = true
				return tc.installErr
			}
			qemuConfirmFn = func(context.Context, string) (bool, error) {
				asked = true
				return tc.accept, tc.confirmErr
			}
			vmAssumeYes = tc.yes
			stubTerminal(t, tc.interactive)
			stubBrewLookPath(t, func(string) (string, error) {
				if tc.brew {
					return "/test/brew", nil
				}
				return "", errors.New("brew not found")
			})
			stubLinuxHost(t, false, nil, func(command []string) error {
				t.Fatalf("ran %q on a host without a package manager", command)
				return nil
			})

			firmware, err := ensureQEMUFirmwareForHostOS(context.Background(), tc.hostOS, "/test/homebrew")
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
			if asked != tc.wantAsked || installed != tc.wantInstalled {
				t.Errorf("asked=%t installed=%t, want asked=%t installed=%t", asked, installed, tc.wantAsked, tc.wantInstalled)
			}
			if err == nil && firmware != "/test/arm64.fd" {
				t.Errorf("firmware = %q", firmware)
			}
			if installed && tc.installErr == nil && findCalls != 2 {
				t.Errorf("discovery calls = %d, want another check after repair", findCalls)
			}
		})
	}
}

func TestEnsureQEMUFirmwareOnLinux(t *testing.T) {
	aptGet := []string{"sudo", "apt-get", "install", "-y", "qemu-efi-aarch64"}
	installFailed := errors.New("exit status 100")
	for _, tc := range []struct {
		name                   string
		onPath                 []string
		interactive, yes       bool
		decline, stillMissing  bool
		confirmErr, installErr error
		wantAsked              bool
		wantCommand            []string
		wantErr                error
		wantMessage            string
	}{
		// QEMU is already installed, so only the firmware package is added.
		{name: "apt-get", onPath: []string{"sudo", "apt-get"}, interactive: true, wantAsked: true, wantCommand: aptGet},
		{name: "dnf", onPath: []string{"sudo", "dnf"}, interactive: true, wantAsked: true,
			wantCommand: []string{"sudo", "dnf", "install", "-y", "edk2-aarch64"}},
		{name: "declined", onPath: []string{"sudo", "apt-get"}, interactive: true, decline: true, wantAsked: true, wantErr: vm.ErrFirmwareNotFound},
		{name: "cancelled prompt", onPath: []string{"sudo", "apt-get"}, interactive: true, confirmErr: ErrUserCancelled, wantAsked: true, wantErr: ErrUserCancelled},
		{name: "yes without a terminal", onPath: []string{"sudo", "apt-get"}, yes: true, wantErr: vm.ErrFirmwareNotFound},
		{name: "no package manager", onPath: []string{"sudo"}, interactive: true, wantErr: vm.ErrFirmwareNotFound},
		{name: "install failed", onPath: []string{"sudo", "apt-get"}, interactive: true, installErr: installFailed, wantAsked: true,
			wantCommand: aptGet, wantErr: installFailed, wantMessage: "installing QEMU's ARM64 firmware via apt-get"},
		{name: "still missing", onPath: []string{"sudo", "apt-get"}, interactive: true, stillMissing: true, wantAsked: true,
			wantCommand: aptGet, wantErr: vm.ErrFirmwareNotFound, wantMessage: "still unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var asked, installed bool
			var command []string
			savedFind, savedConfirm, savedYes := findQEMUFirmwareFn, qemuConfirmFn, vmAssumeYes
			t.Cleanup(func() { findQEMUFirmwareFn, qemuConfirmFn, vmAssumeYes = savedFind, savedConfirm, savedYes })
			findQEMUFirmwareFn = func(hostOS, _ string) (string, error) {
				if hostOS != "linux" {
					t.Fatalf("firmware discovery used host %q", hostOS)
				}
				if installed && !tc.stillMissing {
					return "/usr/share/AAVMF/AAVMF_CODE.fd", nil
				}
				return "", vm.ErrFirmwareNotFound
			}
			stubLinuxHost(t, false, tc.onPath, func(c []string) error {
				installed, command = true, c
				return tc.installErr
			})
			qemuConfirmFn = func(_ context.Context, question string) (bool, error) {
				asked = true
				if question != "Do you want to install QEMU's ARM64 firmware?" {
					t.Errorf("question = %q", question)
				}
				return !tc.decline, tc.confirmErr
			}
			stubTerminal(t, tc.interactive)
			vmAssumeYes = tc.yes

			firmware, err := ensureQEMUFirmwareForHostOS(context.Background(), "linux", "")
			if asked != tc.wantAsked || !slices.Equal(command, tc.wantCommand) {
				t.Fatalf("asked=%t command=%q, want asked=%t command=%q", asked, command, tc.wantAsked, tc.wantCommand)
			}
			if tc.wantErr == nil {
				if err != nil || firmware != "/usr/share/AAVMF/AAVMF_CODE.fd" {
					t.Fatalf("ensureQEMUFirmwareForHostOS() = %q, %v, want the installed firmware", firmware, err)
				}
				return
			}
			if !errors.Is(err, tc.wantErr) || !strings.Contains(err.Error(), tc.wantMessage) {
				t.Fatalf("ensureQEMUFirmwareForHostOS() = %v, want %v containing %q", err, tc.wantErr, tc.wantMessage)
			}
		})
	}
}

// Starting a VM, from `wendy vm start` or `wendy run`, makes the same firmware
// offer as the tour instead of failing on a host that has QEMU without it.
func TestResolveVMSpecOffersMissingFirmware(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("the firmware is offered on macOS and Linux")
	}
	setTempConfig(t, &config.Config{})
	store, err := vm.NewStore()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(store.Dir("fw-test"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(store.DiskPath("fw-test"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := store.WriteMeta(vm.Meta{Name: "fw-test", Profile: "generic"}); err != nil {
		t.Fatal(err)
	}
	port, err := vm.PickHostPort(vm.DefaultAgentPort, simulatorPortSearchTries)
	if err != nil {
		t.Fatal(err)
	}

	var asked, installed bool
	savedQEMU, savedFind, savedInstall := ensureQEMUFn, findQEMUFirmwareFn, installQEMUFirmwareFn
	savedConfirm, savedYes := qemuConfirmFn, vmAssumeYes
	t.Cleanup(func() {
		ensureQEMUFn, findQEMUFirmwareFn, installQEMUFirmwareFn = savedQEMU, savedFind, savedInstall
		qemuConfirmFn, vmAssumeYes = savedConfirm, savedYes
	})
	ensureQEMUFn = func(context.Context) error { return nil }
	findQEMUFirmwareFn = func(string, string) (string, error) {
		if installed {
			return "/test/arm64.fd", nil
		}
		return "", vm.ErrFirmwareNotFound
	}
	installQEMUFirmwareFn = func(context.Context) error { installed = true; return nil }
	stubLinuxHost(t, false, []string{"sudo", "apt-get"}, func([]string) error { installed = true; return nil })
	stubBrewLookPath(t, func(string) (string, error) { return "/test/brew", nil })
	qemuConfirmFn = func(context.Context, string) (bool, error) { asked = true; return true, nil }
	vmAssumeYes = false
	stubTerminal(t, true)

	spec, _, err := resolveVMSpec("fw-test", vmStartOptions{netMode: string(vm.NetUser), hostPort: port, memoryMiB: 1024, cpus: 1})
	if err != nil {
		t.Fatalf("resolveVMSpec() = %v", err)
	}
	if !asked || !installed || spec.FirmwareCode != "/test/arm64.fd" {
		t.Fatalf("asked=%t installed=%t firmware=%q, want the firmware offered and used", asked, installed, spec.FirmwareCode)
	}
}

// stubSocketVMNetMissing simulates a missing helper that becomes available
// after starting, regardless of the host's actual installation.
func stubSocketVMNetMissing(t *testing.T, installed, started *bool) {
	t.Helper()
	savedFind := findSocketVMNetFn
	savedInstall, savedStart := installSocketVMNetFn, startSocketVMNetFn
	findSocketVMNetFn = func(hostOS, brewPrefix string) (string, error) {
		if hostOS != "darwin" {
			return savedFind(hostOS, brewPrefix)
		}
		if *started {
			return "/test/socket_vmnet", nil
		}
		return "", fmt.Errorf("%w: brew install socket_vmnet", vm.ErrSocketVMNetNotFound)
	}
	installSocketVMNetFn = func(context.Context) error { *installed = true; return nil }
	startSocketVMNetFn = func(context.Context) error { *started = true; return nil }
	t.Cleanup(func() {
		findSocketVMNetFn = savedFind
		installSocketVMNetFn, startSocketVMNetFn = savedInstall, savedStart
	})
}

func TestEnsureSocketVMNetNeverPromptsOffMac(t *testing.T) {
	// vmnet is an Apple framework: there is nothing to offer to install, and
	// the error must not recommend Homebrew on a host without it.
	for _, hostOS := range []string{"linux", "windows"} {
		var installed, started, asked bool
		stubSocketVMNetMissing(t, &installed, &started)
		stubTerminal(t, true)
		stubBrewLookPath(t, func(n string) (string, error) { return "/opt/homebrew/bin/" + n, nil })
		stubConfirmFn(t, func(string) bool { asked = true; return true })

		_, err := ensureSocketVMNet(context.Background(), hostOS, "")
		if asked || installed || started {
			t.Errorf("%s: prompted or installed for a platform that cannot run vmnet", hostOS)
		}
		if !errors.Is(err, vm.ErrSharedNetUnsupported) {
			t.Errorf("%s: err = %v, want ErrSharedNetUnsupported", hostOS, err)
		}
	}
}

func TestEnsureSocketVMNetInstallsAndStartsOnMac(t *testing.T) {
	// Install alone leaves the socket missing, so both halves must run.
	var installed, started, asked bool
	stubSocketVMNetMissing(t, &installed, &started)
	stubTerminal(t, true)
	stubBrewLookPath(t, func(n string) (string, error) { return "/opt/homebrew/bin/" + n, nil })
	stubConfirmFn(t, func(string) bool { asked = true; return true })

	socket, err := ensureSocketVMNet(context.Background(), "darwin", "/opt/homebrew")
	if err != nil {
		t.Fatalf("ensureSocketVMNet() = %v, want nil", err)
	}
	if socket != "/test/socket_vmnet" {
		t.Errorf("socket = %q, want /test/socket_vmnet", socket)
	}
	if !asked {
		t.Error("did not prompt before installing")
	}
	if !installed || !started {
		t.Errorf("installed=%v started=%v, want both", installed, started)
	}
}

func TestEnsureSocketVMNetDeclinedKeepsTheManualCommands(t *testing.T) {
	var installed, started bool
	stubSocketVMNetMissing(t, &installed, &started)
	stubTerminal(t, true)
	stubBrewLookPath(t, func(n string) (string, error) { return "/opt/homebrew/bin/" + n, nil })
	stubConfirmFn(t, func(string) bool { return false })

	_, err := ensureSocketVMNet(context.Background(), "darwin", "/opt/homebrew")
	if installed || started {
		t.Error("installed despite the prompt being declined")
	}
	if err == nil || !strings.Contains(err.Error(), "brew install socket_vmnet") {
		t.Errorf("err = %v, want it to name the manual command", err)
	}
}

func TestEnsureSocketVMNetDoesNotPromptWithoutHomebrew(t *testing.T) {
	var installed, started, asked bool
	stubSocketVMNetMissing(t, &installed, &started)
	stubTerminal(t, true)
	stubBrewLookPath(t, func(string) (string, error) { return "", errors.New("not found") })
	stubConfirmFn(t, func(string) bool { asked = true; return true })

	if _, err := ensureSocketVMNet(context.Background(), "darwin", ""); err == nil {
		t.Fatal("ensureSocketVMNet() = nil, want the not-found error")
	}
	if asked || installed || started {
		t.Error("prompted or installed on a host with no Homebrew")
	}
}
