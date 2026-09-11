package commands

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/cli/vm"
)

// stubQEMUMissing makes the emulator look absent, and records whether an
// install was attempted.
func stubQEMUMissing(t *testing.T, installed *bool) {
	t.Helper()
	savedLook, savedInstall := qemuLookPathFn, installQEMUFn
	qemuLookPathFn = func(string) (string, error) { return "", errors.New("not found") }
	installQEMUFn = func(context.Context) error {
		*installed = true
		return nil
	}
	t.Cleanup(func() { qemuLookPathFn, installQEMUFn = savedLook, savedInstall })
}

// stubTerminal forces the interactive check either way; the shared
// stubInteractive only covers the true case.
func stubTerminal(t *testing.T, interactive bool) {
	t.Helper()
	prev := isInteractiveTerminalFn
	isInteractiveTerminalFn = func() bool { return interactive }
	t.Cleanup(func() { isInteractiveTerminalFn = prev })
}

func TestEnsureQEMUIsANoOpWhenAlreadyInstalled(t *testing.T) {
	saved := qemuLookPathFn
	qemuLookPathFn = func(name string) (string, error) { return "/usr/bin/" + name, nil }
	t.Cleanup(func() { qemuLookPathFn = saved })

	asked := false
	stubConfirmFn(t, func(string) bool { asked = true; return true })
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
	stubConfirmFn(t, func(string) bool { asked = true; return true })

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
	stubConfirmFn(t, func(string) bool { asked = true; return true })

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
	stubConfirmFn(t, func(string) bool { asked = true; return true })

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
	stubConfirmFn(t, func(string) bool { asked = true; return false })

	err := ensureQEMUForHostOS(context.Background(), "darwin")
	if !asked {
		t.Error("never prompted")
	}
	if installed {
		t.Error("installed despite the prompt being declined")
	}
	if err == nil || !strings.Contains(err.Error(), qemuInstallHint()) {
		t.Errorf("ensureQEMUForHostOS() = %v, want it to name %q", err, qemuInstallHint())
	}
}

func TestEnsureQEMUNeverPromptsOffMac(t *testing.T) {
	// No non-brew installer is ever driven for the user, so Linux and Windows
	// get the hint and nothing else.
	for _, hostOS := range []string{"linux", "windows"} {
		var installed, asked bool
		stubQEMUMissing(t, &installed)
		stubBrewLookPath(t, func(n string) (string, error) { return "/opt/homebrew/bin/" + n, nil })
		stubTerminal(t, true)
		stubConfirmFn(t, func(string) bool { asked = true; return true })

		err := ensureQEMUForHostOS(context.Background(), hostOS)
		if asked || installed {
			t.Errorf("%s: prompted or installed", hostOS)
		}
		if err == nil {
			t.Errorf("%s: ensureQEMUForHostOS() = nil, want the not-found error", hostOS)
		}
	}
}

func TestEnsureQEMUWithYesSkipsThePromptAndInstalls(t *testing.T) {
	var installed, asked bool
	stubQEMUMissing(t, &installed)
	stubBrewLookPath(t, func(n string) (string, error) { return "/opt/homebrew/bin/" + n, nil })
	stubTerminal(t, false)
	stubConfirmFn(t, func(string) bool { asked = true; return false })

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

// stubSocketVMNetMissing records what would be run. The helper is genuinely
// absent here -- the socket paths it probes are macOS-only -- so nothing needs
// to fake that half.
func stubSocketVMNetMissing(t *testing.T, installed, started *bool) {
	t.Helper()
	savedInstall, savedStart := installSocketVMNetFn, startSocketVMNetFn
	installSocketVMNetFn = func(context.Context) error { *installed = true; return nil }
	startSocketVMNetFn = func(context.Context) error { *started = true; return nil }
	t.Cleanup(func() { installSocketVMNetFn, startSocketVMNetFn = savedInstall, savedStart })
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

	_, _ = ensureSocketVMNet(context.Background(), "darwin", "/opt/homebrew")
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
