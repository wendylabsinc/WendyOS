package commands

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"
	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
)

// addReadinessFlags defines --wait-ready and --readiness-timeout. The names,
// types, defaults, and --readiness-timeout's help text match the agent-verified
// deployment work (PR #1882) exactly, as do validateReadinessOptions' rules and
// messages, so that work can later replace this CLI-side implementation
// without changing the interface. --wait-ready's help differs only because
// this implementation is not agent-verified.
func addReadinessFlags(cmd *cobra.Command, opts *runOptions) {
	cmd.Flags().BoolVar(&opts.waitReady, "wait-ready", false, "Wait until the app passes its readiness probe (or, without one, stays running) before succeeding; fail if it crashes or times out")
	cmd.Flags().DurationVar(&opts.readinessTimeout, "readiness-timeout", 0, "Override the readiness deadline (whole seconds, 1s to 1h)")
}

// validateReadinessOptions is PR #1882's validation, verbatim.
func validateReadinessOptions(opts runOptions) error {
	if opts.readinessTimeout != 0 && (opts.readinessTimeout < time.Second || opts.readinessTimeout > time.Hour || opts.readinessTimeout%time.Second != 0) {
		return fmt.Errorf("--readiness-timeout must be a whole number of seconds between 1s and 1h")
	}
	if opts.deploy && (opts.waitReady || opts.readinessTimeout != 0) {
		return fmt.Errorf("--deploy only creates containers; omit --deploy to use readiness verification")
	}
	return nil
}

// validateWaitReadyFlags rejects flag combinations this implementation cannot
// honor, before any build starts. watch and hil report --watch and --hil,
// which live outside runOptions.
func validateWaitReadyFlags(opts runOptions, watch, hil bool) error {
	if err := validateReadinessOptions(opts); err != nil {
		return classifyCommandError(errConfigInvalid, err)
	}
	if !opts.waitReady && opts.readinessTimeout == 0 {
		return nil
	}
	switch {
	case hil:
		return commandErrorf(errConfigInvalid, "--wait-ready and --readiness-timeout cannot be combined with --hil")
	case opts.waitReady && watch:
		return commandErrorf(errConfigInvalid, "--wait-ready cannot be combined with --watch yet")
	case opts.detach && !opts.waitReady:
		return commandErrorf(errConfigInvalid, "--readiness-timeout needs --wait-ready when used with --detach: a detached run does not otherwise wait for readiness")
	}
	return nil
}

// errWaitReadyUnsupported refuses --wait-ready for a run path it does not
// cover yet, naming what it does cover.
func errWaitReadyUnsupported(what string) error {
	return commandErrorf(errConfigInvalid, "--wait-ready is not supported for %s yet; it covers single-container image projects (Dockerfile, Containerfile, Stagefile, or Python) on WendyOS devices", what)
}

// rejectUnsupportedWaitReady refuses --wait-ready before runWithAgent does any
// work when the project would take a path the wait is not wired into:
// multi-service and Compose groups, native (darwin) commands, Xcode, and Swift
// packages built on the host without a container build file. It mirrors
// runWithAgent's own routing.
//
// appCfg may be nil: runCommand calls this once for the project-type-only
// checks (Compose/Xcode/Swift, all knowable from the filesystem alone) before
// wendy.json exists yet — a missing wendy.json otherwise sends a first run
// through a device-resolving preflight before appCfg is ever loaded — and
// again once appCfg is loaded for the appCfg-dependent checks (multi-service,
// native run.command). A nil appCfg simply skips those two checks.
func rejectUnsupportedWaitReady(cwd string, appCfg *appconfig.AppConfig, opts runOptions) error {
	if !opts.waitReady {
		return nil
	}
	if appCfg != nil {
		switch {
		case len(appCfg.Services) > 0:
			return errWaitReadyUnsupported("multi-service projects")
		case appCfg.Run != nil && appCfg.Run.Command != "":
			return errWaitReadyUnsupported("native commands (run.command)")
		}
	}
	projectType, err := resolveRunProjectType(cwd, opts.buildType)
	if err != nil {
		return nil // runWithAgent reports the detection error itself
	}
	switch projectType {
	case "compose":
		return errWaitReadyUnsupported("Compose projects")
	case "xcode":
		return errWaitReadyUnsupported("Xcode projects")
	case "swift":
		if normalizeBuildType(opts.buildType) == "swift" || !hasContainerBuildFile(cwd) {
			return errWaitReadyUnsupported("Swift packages built without a Dockerfile or Containerfile")
		}
	}
	return nil
}

// rejectWaitReadyBuildHost refuses --wait-ready for remote builds (and so for
// fleets, which require one): their start path is not wired yet, and a fleet
// would print one outcome per device.
func rejectWaitReadyBuildHost(opts runOptions) error {
	if opts.waitReady && strings.TrimSpace(opts.buildHost) != "" {
		return errWaitReadyUnsupported("remote builds (--build-host)")
	}
	return nil
}

// waitReadyJSONStdoutGuard keeps stdout for the one JSON object a
// `--detach --wait-ready` run prints in JSON mode: non-interactive build and
// chunk-push progress (buildProgressOut, stdout by default) moves to stderr
// for the run. It returns the restore func; it is a no-op otherwise.
func waitReadyJSONStdoutGuard(opts runOptions) func() {
	if !jsonOutput || !opts.detach || !opts.waitReady {
		return func() {}
	}
	return setBuildProgressOut(os.Stderr)
}

// waitReadyJSONRun follows a `--json --detach --wait-ready` run so it can keep
// that run's promise of exactly one JSON object on stdout when it fails before
// the wait prints an outcome: runCommand records the app and device as it
// learns them, and reportWaitReadyOutcome records that it printed the object.
// It travels in the run context, like runInterruptNotes.
type waitReadyJSONRun struct {
	mu      sync.Mutex
	app     string
	device  string
	printed bool
}

type waitReadyJSONRunKey struct{}

// waitReadyJSONRunFrom returns ctx's waitReadyJSONRun, or nil outside such a
// run; every method is a no-op on nil.
func waitReadyJSONRunFrom(ctx context.Context) *waitReadyJSONRun {
	run, _ := ctx.Value(waitReadyJSONRunKey{}).(*waitReadyJSONRun)
	return run
}

// noteWaitReadyApp records the app a --wait-ready JSON run deploys.
func noteWaitReadyApp(ctx context.Context, app string) {
	if run := waitReadyJSONRunFrom(ctx); run != nil {
		run.mu.Lock()
		defer run.mu.Unlock()
		run.app = app
	}
}

// noteWaitReadyDevice records the device a --wait-ready JSON run deploys to,
// named as its outcome would name it (see waitReadyDevice).
func noteWaitReadyDevice(ctx context.Context, conn *grpcclient.AgentConnection) {
	if run := waitReadyJSONRunFrom(ctx); run != nil && conn != nil {
		run.mu.Lock()
		defer run.mu.Unlock()
		run.device = waitReadyDevice(conn)
	}
}

// markWaitReadyOutcomePrinted records that the run's JSON object is out.
func markWaitReadyOutcomePrinted(ctx context.Context) {
	if run := waitReadyJSONRunFrom(ctx); run != nil {
		run.mu.Lock()
		defer run.mu.Unlock()
		run.printed = true
	}
}

// runReportingWaitReadyFailure runs run and, for a `--json --detach
// --wait-ready` run that failed without printing its outcome (config, device,
// build, push, or start), prints a "failed" outcome as the run's one JSON
// object on stdout. Ctrl-C (ErrUserCancelled, or ctx cancelled by main's
// signal context) and SIGTERM (errTerminated) print nothing, like an
// interrupted wait.
func runReportingWaitReadyFailure(ctx context.Context, opts runOptions, run func(context.Context) error) error {
	if !jsonOutput || !opts.detach || !opts.waitReady {
		return run(ctx)
	}
	state := &waitReadyJSONRun{}
	err := run(context.WithValue(ctx, waitReadyJSONRunKey{}, state))
	if err == nil || ctx.Err() != nil || errors.Is(err, ErrUserCancelled) || errors.Is(err, errTerminated) {
		return err
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if !state.printed {
		_ = json.NewEncoder(os.Stdout).Encode(waitReadyOutcome{
			Status:    waitReadyStatusFailed,
			App:       state.app,
			Device:    state.device,
			Readiness: readinessNotChecked,
			Message:   err.Error(),
		})
	}
	return err
}
