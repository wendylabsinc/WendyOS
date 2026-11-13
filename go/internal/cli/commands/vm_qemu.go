package commands

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
	"github.com/wendylabsinc/wendy/go/internal/cli/vm"
)

// installQEMUFn runs `brew install qemu`; indirected so tests can stub it
// without invoking Homebrew.
var installQEMUFn = installQEMUViaBrew

var (
	wingetLookPathFn     = exec.LookPath
	installWindowsQEMUFn = installQEMUViaWinget
)

var (
	qemuConfirmFn = func(ctx context.Context, question string) (bool, error) {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		accepted, err := tui.ConfirmDefaultYes(question, tea.WithContext(ctx), tea.WithOutput(os.Stderr))
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		if errors.Is(err, tui.ErrCancelled) {
			return false, ErrUserCancelled
		}
		return accepted, err
	}
	findQEMUFirmwareFn    = vm.FindFirmware
	installQEMUFirmwareFn = installQEMUFirmwareViaBrew
	ensureQEMUFirmwareFn  = func(ctx context.Context) (string, error) {
		return ensureQEMUFirmwareForHostOS(ctx, runtime.GOOS, vm.BrewPrefix())
	}
)

// vmAssumeYes accepts the VM prompts -- installing QEMU, and downloading the
// simulator image. Bound to `wendy vm --yes` only: `wendy run --yes` resolves
// non-interactively and never reaches the picker that raises them, so mirroring
// the flag here would be an unsynchronised write for no gain. It never answers
// the Linux install prompts, which run sudo.
var vmAssumeYes bool

// linuxQEMUPackageManager names the packages a Linux package manager installs
// QEMU's ARM64 emulator and UEFI firmware from.
type linuxQEMUPackageManager struct {
	name     string   // the executable
	qemu     []string // the emulator and its firmware
	firmware []string // the firmware alone, for a host that already has QEMU
}

// linuxQEMUPackageManagers are tried in order; the first on PATH is used.
var linuxQEMUPackageManagers = []linuxQEMUPackageManager{
	{name: "apt-get", qemu: []string{"qemu-system-arm", "qemu-efi-aarch64"}, firmware: []string{"qemu-efi-aarch64"}},
	{name: "dnf", qemu: []string{"qemu-system-aarch64", "edk2-aarch64"}, firmware: []string{"edk2-aarch64"}},
}

// Seams so tests can describe a Linux host and never run its package manager.
var (
	linuxLookPathFn        = exec.LookPath
	linuxGeteuidFn         = os.Geteuid
	installLinuxPackagesFn = installLinuxPackages
)

// ensureQEMUForHostOS offers the host's package manager when QEMU is missing.
// Non-interactive runs need --yes to authorize installation, except on Linux,
// which always asks first.
//
// Homebrew's qemu also ships the aarch64 UEFI firmware, as do the Linux
// packages, so accepting here resolves FindFirmware's separate failure at the
// same time.
func ensureQEMUForHostOS(ctx context.Context, hostOS string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := findQEMUFn(hostOS); err == nil {
		return nil
	}
	notFound := fmt.Errorf("qemu-system-aarch64 not found: install QEMU (%s). The ARM64 emulator runs the WendyOS guest on both ARM64 and x86_64 PCs", qemuInstallHintForHostOS(hostOS))
	if hostOS == "linux" {
		return ensureQEMUOnLinux(ctx, notFound)
	}

	if !isInteractiveTerminalFn() && !vmAssumeYes {
		return notFound
	}
	installer, packageManager := installQEMUFn, "Homebrew"
	switch hostOS {
	case "darwin":
		if _, err := brewLookPathFn("brew"); err != nil {
			return notFound
		}
	case "windows":
		if _, err := wingetLookPathFn("winget"); err != nil {
			return notFound
		}
		installer, packageManager = installWindowsQEMUFn, "WinGet"
	default:
		return notFound
	}
	if !vmAssumeYes {
		cliLogln("QEMU runs the simulator and is not installed. Wendy can install it with `%s`.", qemuInstallHintForHostOS(hostOS))
		accepted, err := qemuConfirmFn(ctx, "Do you want to install QEMU?")
		if err != nil {
			return err
		}
		if !accepted {
			return notFound
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := installer(ctx); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if errors.Is(err, ErrUserCancelled) {
			return err
		}
		return fmt.Errorf("installing QEMU via %s: %w", packageManager, err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := findQEMUFn(hostOS); err != nil {
		if hostOS == "windows" {
			return fmt.Errorf("QEMU installation finished but qemu-system-aarch64.exe was not found on PATH or in %%ProgramFiles%%\\qemu; check the installation and retry")
		}
		return errors.New("QEMU was installed via Homebrew but is not yet on PATH; " +
			"open a new terminal or run: eval \"$(brew shellenv)\"")
	}
	return nil
}

// ensureQEMUOnLinux installs the emulator together with its firmware.
func ensureQEMUOnLinux(ctx context.Context, notFound error) error {
	installed, err := offerLinuxQEMUPackages(ctx, "QEMU", "QEMU runs the simulator and is not installed.",
		func(m linuxQEMUPackageManager) []string { return m.qemu })
	if err != nil {
		return err
	}
	if !installed {
		return notFound
	}
	if _, err := findQEMUFn("linux"); err != nil {
		return errors.New("QEMU was installed but qemu-system-aarch64 is still not on PATH; check the installation and retry")
	}
	return nil
}

// offerLinuxQEMUPackages asks, then installs what (QEMU or its firmware) with
// the host's package manager. The prompt has fully exited before the command
// starts, and the callers run outside any Bubble Tea program, so sudo can ask
// for a password. Without a terminal, or without a known package manager, it
// installs nothing and leaves the caller's manual hint to explain.
func offerLinuxQEMUPackages(ctx context.Context, what, reason string, packages func(linuxQEMUPackageManager) []string) (bool, error) {
	if !isInteractiveTerminalFn() {
		return false, nil
	}
	manager, command, ok := linuxQEMUInstallCommand(packages)
	if !ok {
		return false, nil
	}
	cliLogln("%s Wendy can install it with `%s`.", reason, strings.Join(command, " "))
	accepted, err := qemuConfirmFn(ctx, "Do you want to install "+what+"?")
	if err != nil || !accepted {
		return false, err
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	note := ""
	if command[0] == "sudo" {
		note = " sudo may ask for your password."
	}
	cliLogln("Installing %s via %s.%s", what, manager, note)
	if err := installLinuxPackagesFn(ctx, command); err != nil {
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		return false, fmt.Errorf("installing %s via %s: %w", what, manager, err)
	}
	return true, ctx.Err()
}

// linuxQEMUInstallCommand returns the first package manager on PATH and the
// command that installs packages with it as root. Without root, that needs
// sudo; ok is false when sudo or every package manager is missing.
func linuxQEMUInstallCommand(packages func(linuxQEMUPackageManager) []string) (manager string, command []string, ok bool) {
	if linuxGeteuidFn() != 0 {
		if _, err := linuxLookPathFn("sudo"); err != nil {
			return "", nil, false
		}
		command = []string{"sudo"}
	}
	for _, m := range linuxQEMUPackageManagers {
		if _, err := linuxLookPathFn(m.name); err == nil {
			command = append(command, m.name, "install", "-y")
			return m.name, append(command, packages(m)...), true
		}
	}
	return "", nil, false
}

// installLinuxPackages streams the package manager's output rather than
// hiding it behind a spinner: sudo may need to prompt for a password.
func installLinuxPackages(ctx context.Context, command []string) error {
	cmd := exec.CommandContext(ctx, command[0], command[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stderr, os.Stderr
	return cmd.Run()
}

func installQEMUViaWinget(ctx context.Context) error {
	cliLogln("Installing QEMU via WinGet. Windows may ask for administrator permission.")
	cmd := exec.CommandContext(ctx, "winget", "install", "--id", "SoftwareFreedomConservancy.QEMU", "--exact", "--source", "winget", "--accept-source-agreements", "--accept-package-agreements")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stderr, os.Stderr
	return cmd.Run()
}

func installQEMUViaBrew(ctx context.Context) error {
	return brewInstallWithSpinner(ctx, "qemu", "Installing QEMU via Homebrew...")
}

// ensureQEMUFirmwareForHostOS also covers hosts where QEMU is already on PATH
// but the ARM64 firmware was not installed or is unusable.
func ensureQEMUFirmwareForHostOS(ctx context.Context, hostOS, brewPrefix string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	firmware, err := findQEMUFirmwareFn(hostOS, brewPrefix)
	if err == nil {
		return firmware, nil
	}
	if hostOS == "linux" && errors.Is(err, vm.ErrFirmwareNotFound) {
		return ensureQEMUFirmwareOnLinux(ctx, brewPrefix, err)
	}
	if hostOS != "darwin" || !errors.Is(err, vm.ErrFirmwareNotFound) {
		return "", err
	}
	if !isInteractiveTerminalFn() && !vmAssumeYes {
		return "", err
	}
	if _, lookErr := brewLookPathFn("brew"); lookErr != nil {
		return "", err
	}
	if !vmAssumeYes {
		cliLogln("The simulator needs ARM64 firmware. Homebrew supplies it with QEMU; Wendy can install or reinstall that package to restore it.")
		accepted, confirmErr := qemuConfirmFn(ctx, "Do you want to install QEMU's ARM64 firmware?")
		if confirmErr != nil {
			return "", confirmErr
		}
		if !accepted {
			return "", err
		}
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if installErr := installQEMUFirmwareFn(ctx); installErr != nil {
		if errors.Is(installErr, ErrUserCancelled) {
			return "", installErr
		}
		return "", fmt.Errorf("installing QEMU's ARM64 firmware via Homebrew: %w", installErr)
	}
	firmware, err = findQEMUFirmwareFn(hostOS, brewPrefix)
	if err != nil {
		return "", fmt.Errorf("QEMU was installed via Homebrew but its ARM64 firmware is still unavailable: %w", err)
	}
	return firmware, nil
}

// ensureQEMUFirmwareOnLinux installs only the firmware package: QEMU itself has
// its own check, which runs first.
func ensureQEMUFirmwareOnLinux(ctx context.Context, brewPrefix string, notFound error) (string, error) {
	installed, err := offerLinuxQEMUPackages(ctx, "QEMU's ARM64 firmware", "The simulator needs QEMU's ARM64 firmware, which is not installed.",
		func(m linuxQEMUPackageManager) []string { return m.firmware })
	if err != nil {
		return "", err
	}
	if !installed {
		return "", notFound
	}
	firmware, err := findQEMUFirmwareFn("linux", brewPrefix)
	if err != nil {
		return "", fmt.Errorf("QEMU's ARM64 firmware was installed but is still unavailable: %w", err)
	}
	return firmware, nil
}

func installQEMUFirmwareViaBrew(ctx context.Context) error {
	// Installing an existing formula does not restore missing package files.
	// Reinstall only if Homebrew owns an installed QEMU package.
	installed, err := exec.CommandContext(ctx, "brew", "list", "--versions", "qemu").Output()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err == nil && len(bytes.TrimSpace(installed)) > 0 {
		return brewPackageWithSpinner(ctx, "reinstall", "qemu", "Restoring QEMU's ARM64 firmware via Homebrew...")
	}
	return installQEMUViaBrew(ctx)
}

// brewInstallWithSpinner runs `brew install <formula>` behind a spinner instead
// of streaming Homebrew's own log; the captured output is printed only on
// failure.
func brewInstallWithSpinner(ctx context.Context, formula, label string) error {
	return brewPackageWithSpinner(ctx, "install", formula, label)
}

func brewPackageWithSpinner(ctx context.Context, action, formula, label string) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	cmd := exec.CommandContext(ctx, "brew", action, formula)
	// Wendy already obtained consent. A second Homebrew prompt would have no
	// input and its captured output would be hidden behind the spinner.
	cmd.Env = append(os.Environ(), "HOMEBREW_NO_ASK=1")
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	cmd.Stdin = nil

	prog := tui.NewProgressProgram(tui.NewSpinner(label))

	var (
		runErr error
		doneCh = make(chan struct{})
	)
	go func() {
		defer close(doneCh)
		runErr = cmd.Run()
		prog.Send(tui.SpinnerDoneMsg{})
	}()

	finalModel, err := tui.RunProgram(prog)
	if err != nil {
		cancel()
		<-doneCh
		return fmt.Errorf("spinner TUI: %w", err)
	}
	sm, ok := finalModel.(tui.SpinnerModel)
	if !ok {
		cancel()
		<-doneCh
		return fmt.Errorf("spinner TUI: unexpected model type %T", finalModel)
	}
	if !sm.Done() {
		cancel()
		<-doneCh
		return ErrUserCancelled
	}

	<-doneCh
	if runErr != nil {
		_, _ = os.Stderr.Write(out.Bytes())
		return runErr
	}
	return nil
}

// qemuInstallHint names the command that installs QEMU and, on macOS, its UEFI
// firmware. The single source for that advice: the VM command's help, the
// missing-emulator error and the missing-firmware error all read it.
func qemuInstallHint() string {
	return qemuInstallHintForHostOS(runtime.GOOS)
}

func qemuInstallHintForHostOS(hostOS string) string {
	switch hostOS {
	case "darwin":
		return "brew install qemu"
	case "windows":
		return "winget install --id SoftwareFreedomConservancy.QEMU --exact --source winget"
	default:
		return "apt install qemu-system-arm qemu-efi-aarch64, or dnf install qemu-system-aarch64 edk2-aarch64"
	}
}

// socketVMNetStartTimeout bounds the wait for the daemon to bind its socket.
const socketVMNetStartTimeout = 10 * time.Second

// Indirect discovery and the two Homebrew commands so tests do not depend on
// the host's socket_vmnet installation or invoke Homebrew.
var (
	findSocketVMNetFn    = vm.FindSocketVMNet
	installSocketVMNetFn = installSocketVMNetViaBrew
	startSocketVMNetFn   = startSocketVMNetService
)

// ensureSocketVMNet resolves the socket_vmnet endpoint that shared networking
// needs, offering to install and start the helper on an interactive Mac.
//
// Two steps, because the socket only exists once the service runs: `brew
// install` alone leaves the path missing and would look like a failed install.
func ensureSocketVMNet(ctx context.Context, hostOS, brewPrefix string) (string, error) {
	socket, err := findSocketVMNetFn(hostOS, brewPrefix)
	if err == nil {
		return socket, nil
	}
	// Not a Mac: there is nothing to install, so the error already says so.
	if errors.Is(err, vm.ErrSharedNetUnsupported) {
		return "", err
	}
	if !isInteractiveTerminalFn() && !vmAssumeYes {
		return "", err
	}
	if _, lookErr := brewLookPathFn("brew"); lookErr != nil {
		return "", err
	}
	if !vmAssumeYes && !confirmFn("Shared networking needs the socket_vmnet helper, which was not found. "+
		"Install and start it now with Homebrew? It needs sudo to start.") {
		return "", err
	}

	if instErr := installSocketVMNetFn(ctx); instErr != nil {
		if errors.Is(instErr, ErrUserCancelled) {
			return "", instErr
		}
		return "", fmt.Errorf("installing socket_vmnet via Homebrew: %w", instErr)
	}
	if startErr := startSocketVMNetFn(ctx); startErr != nil {
		return "", fmt.Errorf("starting socket_vmnet: %w", startErr)
	}
	// brew services returns once launchctl has loaded the job, before the
	// daemon has bound its socket. Without this the freshly installed helper
	// reports "not found" and tells the user to install what they just did.
	deadline := time.Now().Add(socketVMNetStartTimeout)
	for {
		socket, err = findSocketVMNetFn(hostOS, brewPrefix)
		if err == nil || time.Now().After(deadline) {
			return socket, err
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}

func installSocketVMNetViaBrew(ctx context.Context) error {
	return brewInstallWithSpinner(ctx, "socket_vmnet", "Installing socket_vmnet via Homebrew...")
}

// startSocketVMNetService runs the privileged half. Its output is streamed
// rather than captured: sudo may need to prompt for a password, which a spinner
// would hide.
func startSocketVMNetService(ctx context.Context) error {
	cliLogln("Starting socket_vmnet (sudo brew services start socket_vmnet)...")
	cmd := exec.CommandContext(ctx, "sudo", "brew", "services", "start", "socket_vmnet")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stderr, os.Stderr
	return cmd.Run()
}
