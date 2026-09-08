package commands

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
	"github.com/wendylabsinc/wendy/go/internal/cli/vm"
)

// installQEMUFn runs `brew install qemu`; indirected so tests can stub it
// without invoking Homebrew.
var installQEMUFn = installQEMUViaBrew

// vmAssumeYes accepts the VM prompts -- installing QEMU, and downloading the
// simulator image. Bound to `wendy vm --yes` only: `wendy run --yes` resolves
// non-interactively and never reaches the picker that raises them, so mirroring
// the flag here would be an unsynchronised write for no gain.
var vmAssumeYes bool

// ensureQEMUForHostOS resolves the emulator, and on an interactive macOS
// session where it is missing offers to install QEMU via Homebrew first.
// Non-interactive runs, other platforms and hosts without Homebrew fall
// straight through to the plain "not found" error.
//
// Homebrew's qemu also ships the aarch64 UEFI firmware, so accepting here
// resolves FindFirmware's separate failure at the same time.
func ensureQEMUForHostOS(ctx context.Context, hostOS string) error {
	binary := vm.Spec{}.Binary()
	if _, err := qemuLookPathFn(binary); err == nil {
		return nil
	}
	notFound := fmt.Errorf("%s not found: install QEMU (%s)", binary, qemuInstallHint())

	if hostOS != "darwin" {
		return notFound
	}
	if !isInteractiveTerminalFn() && !vmAssumeYes {
		return notFound
	}
	if _, err := brewLookPathFn("brew"); err != nil {
		return notFound
	}
	if !vmAssumeYes && !confirmFn("QEMU is required to run a WendyOS VM and was not found. Install it now with `brew install qemu`?") {
		return notFound
	}
	if err := installQEMUFn(ctx); err != nil {
		if errors.Is(err, ErrUserCancelled) {
			return err
		}
		return fmt.Errorf("installing QEMU via Homebrew: %w", err)
	}
	if _, err := qemuLookPathFn(binary); err != nil {
		return errors.New("QEMU was installed via Homebrew but is not yet on PATH; " +
			"open a new terminal or run: eval \"$(brew shellenv)\"")
	}
	return nil
}

func installQEMUViaBrew(ctx context.Context) error {
	return brewInstallWithSpinner(ctx, "qemu", "Installing QEMU via Homebrew...")
}

// brewInstallWithSpinner runs `brew install <formula>` behind a spinner instead
// of streaming Homebrew's own log; the captured output is printed only on
// failure.
func brewInstallWithSpinner(ctx context.Context, formula, label string) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	cmd := exec.CommandContext(ctx, "brew", "install", formula)
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

	finalModel, err := prog.Run()
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
	switch runtime.GOOS {
	case "darwin":
		return "brew install qemu"
	case "windows":
		return "the QEMU for Windows installer"
	default:
		return "apt install qemu-system-arm qemu-efi-aarch64"
	}
}

// socketVMNetStartTimeout bounds the wait for the daemon to bind its socket.
const socketVMNetStartTimeout = 10 * time.Second

// installSocketVMNetFn and startSocketVMNetFn run the two Homebrew commands
// socket_vmnet needs; indirected so tests never invoke Homebrew.
var (
	installSocketVMNetFn = installSocketVMNetViaBrew
	startSocketVMNetFn   = startSocketVMNetService
)

// ensureSocketVMNet resolves the socket_vmnet endpoint that shared networking
// needs, offering to install and start the helper on an interactive Mac.
//
// Two steps, because the socket only exists once the service runs: `brew
// install` alone leaves the path missing and would look like a failed install.
func ensureSocketVMNet(ctx context.Context, hostOS, brewPrefix string) (string, error) {
	socket, err := vm.FindSocketVMNet(hostOS, brewPrefix)
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
		socket, err = vm.FindSocketVMNet(hostOS, brewPrefix)
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
