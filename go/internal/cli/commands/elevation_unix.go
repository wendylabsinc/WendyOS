//go:build darwin || linux

package commands

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/config"
)

// preAuthElevation pre-authenticates sudo so the password prompt appears
// on the raw terminal before any TUI takes over.
func preAuthElevation() error {
	fmt.Println("You may be prompted for your password (sudo is required).")
	if err := exec.Command("sudo", "-v").Run(); err != nil {
		return fmt.Errorf("sudo authentication failed: %w", err)
	}
	return nil
}

// keepElevationAlive refreshes the sudo timestamp every minute until ctx is
// cancelled. Call after preAuthElevation() to prevent the cached credential
// from expiring during a long-running operation such as a multi-GB download.
func keepElevationAlive(ctx context.Context) {
	go func() {
		t := time.NewTicker(60 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				exec.Command("sudo", "-v").Run() //nolint:errcheck
			}
		}
	}()
}

func elevationHint() string {
	return "You may be prompted for your password (sudo is required)."
}

// thorElevationAction is how installThor should obtain the root privileges a Thor
// flash needs before it opens the recovery device (an in-process libusb handle, so
// the whole process must be root — a cached sudo timestamp is not enough).
type thorElevationAction int

const (
	thorElevationProceed   thorElevationAction = iota // already privileged (root, or Linux udev access)
	thorElevationReexec                               // re-exec the command under sudo
	thorElevationFailEarly                            // elevation needed but impossible here — instruct the user
)

// wendyJetsonUdevRulePaths are the standard udev rules directories where the
// 70-wendy-jetson.rules file (installed by the deb/rpm package, or by hand with
// jetsonUdevRuleInstallHint) grants non-root access to the Jetson
// recovery/flashing USB device.
var wendyJetsonUdevRulePaths = []string{
	"/etc/udev/rules.d/70-wendy-jetson.rules",
	"/usr/lib/udev/rules.d/70-wendy-jetson.rules",
	"/lib/udev/rules.d/70-wendy-jetson.rules",
}

// hasWendyJetsonUdevRule reports whether any of paths exists — i.e. the udev rule
// granting non-root USB access to the Jetson is installed. paths is injectable so
// the check is testable without touching /etc.
func hasWendyJetsonUdevRule(paths []string) bool {
	for _, p := range paths {
		if _, err := os.Stat(p); err == nil {
			return true
		}
	}
	return false
}

// thorElevationDecision decides how installThor obtains root before opening the
// Jetson's USB recovery device. macOS always needs root when unprivileged (the OS
// binds its own driver to the recovery device, so there is no non-root path);
// Linux can instead use the wendy udev rule, so an installed rule means proceed.
// When elevation is needed it re-execs under sudo on an interactive terminal, or
// fails early with instructions when there is no TTY to prompt on.
func thorElevationDecision(goos string, euid int, hasUdevRule, interactive bool) thorElevationAction {
	if euid == 0 {
		return thorElevationProceed
	}
	if goos == "linux" && hasUdevRule {
		return thorElevationProceed
	}
	if !interactive {
		return thorElevationFailEarly
	}
	return thorElevationReexec
}

// thorSudoPreserveEnv keeps the elevated (sudo) re-exec pointed at the same
// flashpack cache (HOME / XDG_CACHE_HOME feed os.UserCacheDir) and network config
// (proxy vars) instead of re-downloading the ~3 GB flashpack under root's env. It
// also keeps the user's wendy settings: without WENDY_ANALYTICS an opted-out user
// looks like a first run, and without WENDY_CONFIG_DIR config lands in $HOME/.wendy.
const thorSudoPreserveEnv = "--preserve-env=HOME,XDG_CACHE_HOME,WENDY_ANALYTICS,WENDY_CONFIG_DIR,HTTP_PROXY,HTTPS_PROXY,NO_PROXY,http_proxy,https_proxy,no_proxy"

// hasDeviceTypeFlag reports whether args already carries a --device-type flag in
// either "--device-type X" or "--device-type=X" form.
func hasDeviceTypeFlag(args []string) bool {
	for _, a := range args {
		if a == "--device-type" || strings.HasPrefix(a, "--device-type=") {
			return true
		}
	}
	return false
}

// buildSudoReexecArgs builds the arguments after "sudo" for re-running wendy
// elevated. self is the absolute wendy path; origArgs is os.Args[1:]. It preserves
// the cache/network env vars and, when the original invocation did not already
// specify a device type, appends --device-type jetson-agx-thor so the elevated
// process routes straight to the Thor flow instead of re-showing the device picker.
func buildSudoReexecArgs(self string, origArgs []string) []string {
	args := []string{thorSudoPreserveEnv, self}
	args = append(args, origArgs...)
	if !hasDeviceTypeFlag(origArgs) {
		args = append(args, "--device-type", thorDeviceType)
	}
	return args
}

// pinCacheDirEnv returns environ with XDG_CACHE_HOME set to cacheBase so the
// sudo-elevated re-exec resolves the same wendy cache directory — and reuses the
// already-downloaded flashpack — even when a distro's sudoers forces HOME=/root
// (always_set_home), which would otherwise send os.UserCacheDir() to /root/.cache
// on Linux. cacheBase is the invoking user's os.UserCacheDir(). XDG_CACHE_HOME is
// already in thorSudoPreserveEnv, so sudo passes it through. Harmless on macOS,
// where os.UserCacheDir() ignores XDG_CACHE_HOME.
func pinCacheDirEnv(environ []string, cacheBase string) []string {
	const key = "XDG_CACHE_HOME="
	out := make([]string, 0, len(environ)+1)
	replaced := false
	for _, e := range environ {
		if strings.HasPrefix(e, key) {
			out = append(out, key+cacheBase)
			replaced = true
			continue
		}
		out = append(out, e)
	}
	if !replaced {
		out = append(out, key+cacheBase)
	}
	return out
}

// jetsonUdevRuleInstallHint returns the shell commands that install the wendy
// Jetson udev rule by hand — the same rule the deb/rpm packages ship — one per
// line, each prefixed with indent.
func jetsonUdevRuleInstallHint(indent string) string {
	return indent + "echo '" + usbUdevRule + "' | sudo tee " + usbUdevRulePath + "\n" +
		indent + "sudo udevadm control --reload-rules && sudo udevadm trigger"
}

// thorElevationReason is the explanation printed just before the sudo re-exec,
// tailored per platform.
func thorElevationReason(goos string) string {
	if goos == "darwin" {
		return "Flashing a Jetson AGX Thor needs administrator access — it talks to the board's USB recovery device directly, which requires root on macOS."
	}
	return "Flashing a Jetson AGX Thor needs USB access to the board's recovery device.\n" +
		"  Tip: install the udev rule once (the wendy deb/rpm packages include it) to skip sudo next time:\n" +
		jetsonUdevRuleInstallHint("    ")
}

// createElevatedRunDirs creates, as the user, the cache and log directories the
// elevated run writes into. The elevated run gives what it creates back to the
// user when it exits (HandBackSudoFiles), but not if it's killed; created by
// root, these directories, and on a fresh host ~/.cache itself, would then stay
// root-owned. Best-effort: the elevated run creates any that are missing.
func createElevatedRunDirs() {
	_, _ = osCacheDir()
	_, _ = config.LogDir()
}

// errThorNeedsRoot is returned when a Thor flash needs elevation but cannot obtain
// it here (no interactive terminal to prompt on, or no sudo on PATH). It carries
// the exact command to re-run, plus the Linux udev alternative.
func errThorNeedsRoot() error {
	msg := "flashing a Jetson AGX Thor requires administrator access — it opens the board's USB recovery device directly, which needs root.\n" +
		"  Re-run:  sudo wendy install --device-type " + thorDeviceType
	if runtime.GOOS == "linux" {
		msg += "\n  Or install the udev rule once (the wendy deb/rpm packages include it), then no sudo is needed:\n" +
			jetsonUdevRuleInstallHint("    ")
	}
	return errors.New(msg)
}

// ensureThorRootAccess guarantees the Thor flash has the root privileges it needs
// to open the USB recovery device. When elevation is required and possible it
// prints a reason and re-execs the command under sudo, replacing this process
// (single-process, so the stage-2 abort-guard and signal handling are unchanged) —
// it does not return in that case. It returns nil when the caller may proceed
// (already root, or Linux udev access), or errThorNeedsRoot when elevation is
// needed but impossible here.
func ensureThorRootAccess() error {
	switch thorElevationDecision(
		runtime.GOOS,
		os.Geteuid(),
		hasWendyJetsonUdevRule(wendyJetsonUdevRulePaths),
		isInteractiveTerminal(),
	) {
	case thorElevationProceed:
		return nil
	case thorElevationFailEarly:
		return errThorNeedsRoot()
	}

	// thorElevationReexec: re-run the whole command under sudo.
	sudo, err := exec.LookPath("sudo")
	if err != nil {
		return errThorNeedsRoot() // no sudo to elevate with — instruct instead of failing to exec
	}
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolving wendy executable path for sudo re-exec: %w", err)
	}

	fmt.Println(thorElevationReason(runtime.GOOS))
	fmt.Println("Re-running under sudo (you may be prompted for your password)…")

	createElevatedRunDirs()
	argv := append([]string{"sudo"}, buildSudoReexecArgs(self, os.Args[1:])...)
	// Pin XDG_CACHE_HOME to this (unprivileged) user's cache base so the elevated
	// run reuses the already-downloaded flashpack even if sudo rewrites HOME.
	env := os.Environ()
	if base, cerr := os.UserCacheDir(); cerr == nil {
		env = pinCacheDirEnv(env, base)
	}
	// syscall.Exec replaces this process image; on success it never returns.
	return fmt.Errorf("re-executing under sudo: %w", syscall.Exec(sudo, argv, env))
}
