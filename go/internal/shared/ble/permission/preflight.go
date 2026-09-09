// Package permission checks whether this process has OS permission to use
// Bluetooth Low Energy, before any scan or connection touches the platform's
// BLE stack for the first time.
package permission

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"sync"
)

// CheckArg is the hidden CLI subcommand a caller re-execs itself as to run
// the actual platform probe (scan.RunBLECheck) in a disposable child
// process. cmd/wendy registers the other half of this contract: a hidden
// command named CheckArg that runs the probe and exits with its result.
const CheckArg = "__ble-check"

// mu guards resolved/resolvedErr: the CheckArg subprocess canary result,
// cached for the life of the process once it is genuinely known — terminal
// Bluetooth permission cannot change while wendy is running, so a real
// success or a real subprocess failure is worth never re-probing.
//
// A result is deliberately NOT cached, and the next call re-probes from
// scratch, when the probe subprocess died only because the CALLER's ctx was
// canceled or timed out while it was starting or still running: that proves
// nothing about Bluetooth permission, only that this particular caller
// stopped waiting. Preflight always execs with the caller's ctx (rather than
// one scoped to itself) so a caller can still kill an in-flight probe
// promptly on shutdown — trading that responsiveness away to get
// unconditional caching is not this package's call to make.
var (
	mu          sync.Mutex
	resolved    bool
	resolvedErr error
)

// runCheck execs exe as CheckArg and reports the probe's outcome. A package
// var, following this codebase's seam convention (see blePreflightFn et al.
// in shared/discovery/ble_lite_discovery.go), so a test can replace it
// without a real subprocess, cgo, or CoreBluetooth. Production never
// reassigns it.
var runCheck = func(ctx context.Context, exe string) error {
	cmd := exec.CommandContext(ctx, exe, CheckArg)
	cmd.Stdout = nil
	cmd.Stderr = nil
	return cmd.Run()
}

// Preflight verifies BLE access is available before any scanner or GATT
// connection touches the platform Bluetooth stack. Touching CoreBluetooth
// for the first time in a process without Bluetooth TCC permission can
// SIGABRT the whole process instead of returning an error; running that
// first touch in a disposable child re-exec'd with CheckArg means only the
// child dies, and this process gets back a clean error instead.
//
// A genuinely determined result is cached for the process's lifetime (see
// the resolved/-Err doc comment). A probe interrupted only by the caller's
// own ctx ending is inconclusive: this call returns an error, but nothing is
// cached, so the next call — presumably with a live context — gets a real
// answer instead of being stuck with a false "unavailable" forever.
func Preflight(ctx context.Context) error {
	mu.Lock()
	defer mu.Unlock()
	if resolved {
		return resolvedErr
	}

	exe, exeErr := os.Executable()
	if exeErr != nil {
		resolved = true // can't locate self, assume BLE is available
		return nil
	}

	if runErr := runCheck(ctx, exe); runErr != nil {
		// A subprocess killed by exec.CommandContext's Cancel because ctx
		// ended does NOT return context.Canceled/DeadlineExceeded from
		// Run(): os/exec's Wait() prefers the child's own exit error (e.g.
		// "signal: killed") over the context error whenever the process
		// actually exited non-zero, which is exactly what a killed process
		// does. So errors.Is(runErr, context.Canceled) would miss this, the
		// common, case — checking ctx.Err() after the fact is the only
		// reliable signal. (A genuine failure that happens to coincide with
		// an unrelated ctx cancellation is misclassified as inconclusive
		// too, but that only costs one harmless extra retry later — never a
		// stuck false negative, which is the failure mode being fixed here.)
		if ctx.Err() != nil {
			return fmt.Errorf("Bluetooth preflight interrupted: %w", ctx.Err())
		}
		resolvedErr = fmt.Errorf("Bluetooth unavailable - your terminal may not have Bluetooth permission")
		resolved = true
		return resolvedErr
	}

	resolved = true
	return nil
}
