//go:build windows

package commands

import (
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
// prompt. The elevated child runs in a new console window. Returns nil when
// the child started, or an error when the user declined the UAC prompt or
// the launch otherwise failed.
func relaunchElevated() error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolving executable path: %w", err)
	}

	var quoted []string
	for _, a := range os.Args[1:] {
		quoted = append(quoted, syscall.EscapeArg(a))
	}
	params := strings.Join(quoted, " ")

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

// preAuthElevation ensures the current process has Administrator privileges,
// which raw disk writes require on Windows. When not elevated, it offers a
// UAC re-launch and, on success, exits this non-elevated process so the user
// only has one live wendy process. When the user declines or the re-launch
// fails, it returns a clear error so callers can abort before paying for any
// network or disk work.
func preAuthElevation() error {
	elevated, err := isElevated()
	if err != nil {
		// If the elevation check itself fails, don't block the caller —
		// surface the warning and let the disk write fail with its own
		// "Access denied" if we really were unprivileged.
		fmt.Fprintf(os.Stderr, "warning: could not determine elevation state: %v\n", err)
		return nil
	}
	if elevated {
		return nil
	}

	fmt.Println("Administrator privileges are required to write to a raw disk.")
	fmt.Println("Requesting elevation — Windows will show a UAC consent prompt.")
	fmt.Println("If you accept, this command will continue in a new elevated console window.")

	if err := relaunchElevated(); err != nil {
		return fmt.Errorf("administrator privileges required: %w. Right-click your terminal and choose \"Run as administrator\", then re-run this command", err)
	}

	// Hand off to the elevated child and exit so the user isn't left with
	// two wendy processes. The child runs in its own console window.
	fmt.Println("Elevated process started in a new window. Continuing there.")
	os.Exit(0)
	return nil
}

// elevationHint returns a user-facing message about privilege requirements.
func elevationHint() string {
	return "Administrator privileges are required for disk writing."
}
