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
// prompt. The elevated child runs in a new console window. extraArgs are
// appended when they are not already present in os.Args (used to inject
// flags like --device that were resolved interactively before elevation).
// extraArgs must contain an even number of elements, treated as flag/value
// pairs. Returns nil when the child started, or an error when the user
// declined the UAC prompt or the launch otherwise failed.
func relaunchElevated(extraArgs ...string) error {
	if len(extraArgs)%2 != 0 {
		return fmt.Errorf("relaunchElevated: extraArgs must be flag/value pairs, got %d elements", len(extraArgs))
	}

	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolving executable path: %w", err)
	}

	args := os.Args[1:]
	// Inject extra args that are not already present in the original invocation.
	// We check by flag name (e.g. "--device") so we don't duplicate flags that
	// were already supplied on the command line.
	for i := 0; i < len(extraArgs); i += 2 {
		flag := extraArgs[i]
		already := false
		for _, a := range args {
			if a == flag || strings.HasPrefix(a, flag+"=") {
				already = true
				break
			}
		}
		if !already {
			args = append(args, flag, extraArgs[i+1])
		}
	}

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

	args := os.Args[1:]
	// Inject extra args that are not already present in the original invocation.
	// We check by flag name (e.g. "--device") so we don't duplicate flags that
	// were already supplied on the command line.
	for i := 0; i < len(extraArgs); i += 2 {
		flag := extraArgs[i]
		already := false
		for _, a := range args {
			if a == flag || strings.HasPrefix(a, flag+"=") {
				already = true
				break
			}
		}
		if !already && i+1 < len(extraArgs) {
			args = append(args, flag, extraArgs[i+1])
		}
	}

	var quotedArgs []string
	for _, a := range args {
		quotedArgs = append(quotedArgs, syscall.EscapeArg(a))
	}
	wendyArgs := strings.Join(quotedArgs, " ")

	// Launch via "cmd.exe /k <exe> <args>" so the elevated window stays open
	// after wendy exits and the user can read the output.
	cmdExe := os.Getenv("COMSPEC")
	if cmdExe == "" {
		cmdExe = "cmd.exe"
	}
	cmdParams := "/k " + syscall.EscapeArg(exe)
	if wendyArgs != "" {
		cmdParams += " " + wendyArgs
	}

	verbPtr, err := syscall.UTF16PtrFromString("runas")
	if err != nil {
		return fmt.Errorf("encoding verb: %w", err)
	}
	exePtr, err := syscall.UTF16PtrFromString(cmdExe)
	if err != nil {
		return fmt.Errorf("encoding cmd.exe path: %w", err)
	}
	paramsPtr, err := syscall.UTF16PtrFromString(cmdParams)
	if err != nil {
		return fmt.Errorf("encoding parameters: %w", err)
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

// preAuthElevation ensures the current process has Administrator privileges,
// which raw disk writes require on Windows. When not elevated, it offers a
// UAC re-launch and, on success, exits this non-elevated process so the user
// only has one live wendy process. When the user declines or the re-launch
// fails, it returns a clear error so callers can abort before paying for any
// network or disk work.
func preAuthElevation() error {
	return requireElevation("to write to a raw disk")
}

// elevationHint returns a user-facing message about privilege requirements.
func elevationHint() string {
	return "Administrator privileges are required for disk writing."
}

