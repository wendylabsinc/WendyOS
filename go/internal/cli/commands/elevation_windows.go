//go:build windows

package commands

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// isElevated reports whether the current process holds an elevated access
// token. Uses the documented OpenProcessToken + GetTokenInformation API
// instead of probing `net session`, which suffers false negatives when the
// Server service is stopped or in some domain configurations.
func isElevated() (bool, error) {
	var token windows.Token
	if err := windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_QUERY, &token); err != nil {
		return false, fmt.Errorf("OpenProcessToken: %w", err)
	}
	defer token.Close()

	var elevation struct {
		TokenIsElevated uint32
	}
	var returned uint32
	err := windows.GetTokenInformation(
		token,
		windows.TokenElevation,
		(*byte)(unsafe.Pointer(&elevation)),
		uint32(unsafe.Sizeof(elevation)),
		&returned,
	)
	if err != nil {
		return false, fmt.Errorf("GetTokenInformation: %w", err)
	}
	return elevation.TokenIsElevated != 0, nil
}

// relaunchElevated re-launches the current executable with the original
// arguments through the shell's "runas" verb, which triggers a UAC consent
// prompt. The elevated child runs in a new console window. extraArgs are
// complete "--flag=value" tokens appended when their flag is not already
// present in os.Args (used to inject answers like --device-type that were
// resolved interactively before elevation). The attached =value form is
// required: cobra parses a detached value after a boolean flag (e.g.
// "--rootfs-only false") as a positional argument, not the flag's value.
// Returns nil when the child started, or an error when the user declined
// the UAC prompt or the launch otherwise failed.
func relaunchElevated(extraArgs ...string) error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolving executable path: %w", err)
	}

	args := injectElevationArgs(os.Args[1:], extraArgs)

	var quotedArgs []string
	for _, a := range args {
		quotedArgs = append(quotedArgs, syscall.EscapeArg(a))
	}
	params := strings.Join(quotedArgs, " ")

	// Launch the executable directly via ShellExecute. We deliberately do NOT
	// wrap with `cmd.exe /k` here: syscall.EscapeArg only handles
	// CreateProcess-style quoting, not cmd.exe metacharacters (%, &, |, <, >,
	// ^), so an arg containing those would be interpreted by cmd.exe and
	// could expand env vars or chain commands in the elevated session.
	verbPtr, err := syscall.UTF16PtrFromString("runas")
	if err != nil {
		return fmt.Errorf("encoding verb: %w", err)
	}
	exePtr, err := syscall.UTF16PtrFromString(exe)
	if err != nil {
		return fmt.Errorf("encoding exe path: %w", err)
	}
	var paramsPtr *uint16
	if params != "" {
		paramsPtr, err = syscall.UTF16PtrFromString(params)
		if err != nil {
			return fmt.Errorf("encoding parameters: %w", err)
		}
	}

	const swNormal int32 = 1
	if err := windows.ShellExecute(0, verbPtr, exePtr, paramsPtr, nil, swNormal); err != nil {
		// User clicking "No" on the UAC consent prompt surfaces here.
		if errors.Is(err, windows.ERROR_CANCELLED) {
			return fmt.Errorf("user declined the elevation prompt")
		}
		return fmt.Errorf("ShellExecute runas: %w", err)
	}
	return nil
}

// injectElevationArgs appends each "--flag=value" token from extraArgs to args
// unless the flag (the part before "=") already appears there, either as a
// bare token or in attached =value form, so flags the user typed themselves
// are never duplicated on the elevated command line.
func injectElevationArgs(args, extraArgs []string) []string {
	merged := append([]string(nil), args...)
	for _, extra := range extraArgs {
		name, _, _ := strings.Cut(extra, "=")
		already := false
		for _, a := range merged {
			if a == name || strings.HasPrefix(a, name+"=") {
				already = true
				break
			}
		}
		if !already {
			merged = append(merged, extra)
		}
	}
	return merged
}

// requireElevation is the shared implementation for all elevation gates. It
// checks whether the process is already elevated and, if not, prints purpose,
// triggers a UAC re-launch via relaunchElevated, and exits so only the
// elevated child continues. Returns an error when the user declines UAC or
// the re-launch fails so the caller can abort cleanly.
func requireElevation(purpose string, extraArgs ...string) error {
	elevated, err := isElevated()
	if err != nil {
		// If the elevation check itself fails, don't block the caller —
		// surface the warning and let the operation fail with its own error
		// (e.g. "Access denied") if we really were unprivileged.
		fmt.Fprintf(os.Stderr, "warning: could not determine elevation state: %v\n", err)
		return nil
	}
	if elevated {
		return nil
	}

	fmt.Printf("Administrator privileges are required %s.\n", purpose)
	fmt.Println("Requesting elevation — Windows will show a UAC consent prompt.")
	fmt.Println("If you accept, this command will continue in a new elevated console window.")

	if err := relaunchElevated(extraArgs...); err != nil {
		return fmt.Errorf("administrator privileges required: %w. Right-click your terminal and choose \"Run as administrator\", then re-run this command", err)
	}

	// Hand off to the elevated child and exit so the user isn't left with
	// two wendy processes. The child runs in its own console window.
	fmt.Println("Elevated process started in a new window. Continuing there.")
	os.Exit(0)
	return nil
}

// elevateForT234Flash elevates as soon as an interactive `wendy os install`
// commits to a T234 flash mode — full recovery needs it for the driver
// install and raw disk writes, rootfs-only for the raw disk write. Doing it
// before the remaining wizard questions matters because the UAC relaunch
// starts a fresh process in a new console: everything answered before it
// would have to be answered again there. The answers made so far ride along
// as --device-type and --rootfs-only so the elevated instance resumes at the
// next question instead of re-asking. No-op when the process is already
// elevated.
func elevateForT234Flash(deviceType string, rootfsOnly bool) error {
	purpose := "to flash the Jetson over USB recovery"
	if rootfsOnly {
		purpose = "to write the OS image to a raw disk"
	}
	return requireElevation(purpose,
		"--device-type="+deviceType,
		fmt.Sprintf("--rootfs-only=%t", rootfsOnly))
}

// processElevated reports whether this process already holds administrator
// rights (false when the check fails). Used to skip elevation guidance that
// no longer applies once the UAC handoff has happened.
func processElevated() bool {
	elevated, err := isElevated()
	return err == nil && elevated
}

// preAuthElevation ensures the current process has Administrator privileges,
// which raw disk writes require on Windows. When not elevated, it offers a
// UAC re-launch and, on success, exits this non-elevated process so the user
// only has one live wendy process. When the user declines or the re-launch
// fails, it returns a clear error so callers can abort before paying for any
// network or disk work.
func preAuthElevation() error {
	return requireElevation("to write to a raw disk")
}

func elevationHint() string {
	return "Administrator privileges are required for disk writing."
}

// keepElevationAlive is a no-op on Windows: UAC elevation is process-wide and
// does not expire, so there is no credential cache to refresh.
func keepElevationAlive(_ context.Context) {}

// ensureThorRootAccess is a no-op on Windows. The Thor flash's up-front root
// requirement (WDY-1843) is a macOS/Linux concern — Windows elevates a helper
// when either USB stage needs a driver install, so there is nothing to do
// here. Provided so the cross-platform installThor flow compiles on Windows.
func ensureThorRootAccess() error { return nil }
