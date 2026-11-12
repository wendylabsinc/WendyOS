package commands

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Keep the transport cause inspectable without using its wrapper as the
// diagnosis. A failed stream does not establish that the application exited.
type runStreamFailure struct {
	phase   string // completes "run failed ...": when the stream failed
	message string
	cause   error
}

func (e *runStreamFailure) Error() string { return e.message }
func (e *runStreamFailure) Unwrap() error { return e.cause }

func diagnoseRunStreamFailure(ctx context.Context, conn *grpcclient.AgentConnection, cfg *appconfig.AppConfig, started bool, cause error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	phase := "while receiving app output"
	message := "The app output connection ended. App state could not be verified."
	steps := []string{"Check the device connection, then inspect the app before deploying again."}
	detail, lost := runStreamCause(cause)
	var category error
	if !started {
		phase = "during app start"
		if !lost {
			// The device answered with an error. That is the failure, not a
			// lost connection, so report it rather than a network check.
			return withNextSteps(&runStreamFailure{phase: phase, cause: cause,
				message: "The run failed before the device confirmed app start. " + detail},
				"Resolve the reported error, then run again.")
		}
		message = "The connection ended before the device confirmed app start. App state is unknown."
	} else if conn != nil && conn.ContainerService != nil {
		// The lookup and the confirmation below are bounded separately: a
		// shared deadline lets a slow lookup expire the confirmation window
		// and turn a recorded crash into "could not be confirmed".
		lookup, cancel := context.WithTimeout(ctx, appExitLookupTimeout)
		c, err := lookupAppContainer(lookup, conn, cfg.AppID)
		cancel()
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err == nil && c != nil {
			switch {
			case startedAppBaseline(cfg).versionChanged(c):
				message = "The app output connection ended. Another app version is now deployed."
			case c.GetRunningState() == agentpb.AppRunningState_RUNNING:
				message = "The app output connection ended, but the device reports the app is still running."
				steps = []string{"Reconnect to its output with `wendy device logs --app " + shellQuoteArg(cfg.AppID) + "`. The app was not restarted."}
			case appExitFailure(cfg.AppID, c) != nil:
				// A replacement can briefly expose the previous app's exit record.
				// A lost stream does not prove that the task ended, so confirm the
				// observation using the same conservative rules as a log follower.
				verdict, _, last, confirmErr := appReplaced(ctx, conn, cfg, startedAppBaseline(cfg), c, nil)
				if confirmErr == nil && followedExitStands(verdict != appOwn, last) {
					message = appCrashedError(cfg.AppID, last).Error()
					category = errAppCrashed
					steps = []string{"Inspect the recorded exit and recent app output before running again."}
				} else {
					message = "The app output connection ended. The app may have been replaced or restarted; a crash could not be confirmed."
				}
			case cleanStop(c):
				message = "The app output connection ended. The device reports a clean app exit."
			default:
				message = "The app output connection ended. The app is stopped, but the device did not record an exit reason."
			}
		}
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	err := error(&runStreamFailure{phase: phase, message: message + "\n" + detail, cause: cause})
	if category != nil {
		err = classifyCommandError(category, err)
	}
	return withNextSteps(err, steps...)
}

// runStreamCause describes the error that ended a run stream and reports
// whether it is a lost connection. Any other gRPC status is an answer: the
// stream ended because of the error it names.
func runStreamCause(cause error) (detail string, lost bool) {
	st, ok := status.FromError(cause)
	if !ok {
		return "Connection detail: " + cause.Error(), true
	}
	switch st.Code() {
	case codes.Unavailable, codes.Canceled, codes.DeadlineExceeded:
		return "Connection detail: " + st.Message(), true
	}
	return fmt.Sprintf("Reported error (%s): %s", st.Code(), st.Message()), false
}

// A bounded tail prevents an attached process from growing CLI memory or
// filling the host disk. Only failures create a private local diagnostic file.
type recentRunOutput struct{ stdout, stderr []byte }

const recentRunOutputLimit = 32 << 10

func appendRunTail(dst, data []byte) []byte {
	if len(data) >= recentRunOutputLimit {
		return append(dst[:0], data[len(data)-recentRunOutputLimit:]...)
	}
	if excess := len(dst) + len(data) - recentRunOutputLimit; excess > 0 {
		dst = append(dst[:0], dst[excess:]...)
	}
	return append(dst, data...)
}

func (r *recentRunOutput) failure(err error) error {
	if err == nil || errors.Is(err, context.Canceled) || ExecutionErrorClass(err) == "user_cancelled" || ExecutionErrorClass(err) == "terminated" {
		return err
	}
	if len(r.stdout)+len(r.stderr) == 0 {
		return err
	}
	raw := "Recent stderr (up to 32 KiB):\n" + string(r.stderr) + "\nRecent stdout (up to 32 KiB):\n" + string(r.stdout)
	path, saveErr := writeBuildFailureLog("runtime", raw)
	if saveErr != nil {
		return err
	} // The original output was already streamed.
	steps := []string{"Recent runtime output saved locally: " + path}
	if len(r.stderr) > 0 && !jsonOutput {
		lines := lastBuildOutputLines(strings.Split(buildFailureANSIRe.ReplaceAllString(string(r.stderr), ""), "\n"), 8)
		cliLogln("Recent app stderr:")
		for _, line := range lines {
			cliLogln("  %s", line)
		}
	}
	return withNextSteps(err, steps...)
}

// runPhaseError names the phase a failure belongs to when the failing code
// knows it. The activation step is an analytics marker that some paths never
// advance (Compose stays on "connect", the no-build fast path on "build"), so
// it is only the fallback.
type runPhaseError struct {
	phase string
	err   error
}

func (e *runPhaseError) Error() string { return e.err.Error() }
func (e *runPhaseError) Unwrap() error { return e.err }

// inRunPhase marks err as having failed in phase, unless a more specific
// phase is already recorded.
func inRunPhase(phase string, err error) error {
	var marked *runPhaseError
	if err == nil || errors.As(err, &marked) {
		return err
	}
	return &runPhaseError{phase: phase, err: err}
}

// runStepPhases names the activation steps of `wendy run`. A step that is not
// listed is internal and is left out of the message.
var runStepPhases = map[string]string{
	"project": "project setup", "connect": "device connection", "auth": "registry sign-in",
	"build": "build", "deploy": "deploy", "readiness": "readiness check",
}

func runPhaseFailure(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	switch ExecutionErrorClass(err) {
	case "user_cancelled", "context_canceled", "terminated":
		return err
	}
	var streamFailure *runStreamFailure
	var marked *runPhaseError
	switch {
	// A confirmed crash also carries the stream failure that revealed it.
	case errors.Is(err, errAppCrashed):
		return fmt.Errorf("run failed during app execution: %w", err)
	case errors.As(err, &streamFailure):
		return fmt.Errorf("run failed %s: %w", streamFailure.phase, err)
	case errors.As(err, &marked):
		return fmt.Errorf("run failed during %s: %w", marked.phase, err)
	}
	progress, ok := ctx.Value(activationKey{}).(*activationProgress)
	if !ok {
		return err
	}
	progress.Lock()
	step := progress.properties["step"]
	progress.Unlock()
	phase, ok := runStepPhases[step]
	if !ok {
		return err
	}
	return fmt.Errorf("run failed during %s: %w", phase, err)
}
