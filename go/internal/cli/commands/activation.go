package commands

import (
	"context"
	"errors"
	"os/exec"
	"strconv"
	"sync"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/analytics"
)

type activationKey struct{}
type activationProgress struct {
	sync.Mutex
	stage, command string
	properties     map[string]string
}

func activationStep(ctx context.Context, step string, checks map[string]string) {
	progress, ok := ctx.Value(activationKey{}).(*activationProgress)
	if !ok {
		return
	}
	progress.Lock()
	progress.properties["step"] = step
	for k, v := range checks {
		progress.properties[k] = v
	}
	props := make(map[string]string, len(progress.properties))
	for k, v := range progress.properties {
		props[k] = v
	}
	progress.Unlock()
	analytics.TrackStage(ctx, progress.stage+"_progress", progress.command, "started", "", props)
}

// The returned closure records only an observed terminal outcome. A process
// killed before it runs has an unknown outcome, never an inferred cancellation.
func startActivationOperation(ctx context.Context, stage, command string) (context.Context, func(error)) {
	ctx = analytics.WithAttempt(ctx)
	began := time.Now()
	progress := &activationProgress{stage: stage, command: command, properties: map[string]string{"step": "prepare", "stage": stage, "is_interactive": strconv.FormatBool(isInteractiveTerminal()), "install_method": "unknown"}}
	if stage == "run" {
		for _, key := range []string{"project_recognized", "target_selected", "device_reachable", "auth_valid", "build_prerequisites"} {
			progress.properties[key] = "unknown"
		}
	}
	ctx = context.WithValue(ctx, activationKey{}, progress)
	analytics.TrackStage(ctx, stage+"_started", command, "started", "", progress.properties)
	return ctx, func(err error) {
		class, outcome := ExecutionErrorClass(err), "completed"
		if err != nil {
			outcome = "failed"
		}
		if class == "user_cancelled" || class == "context_canceled" {
			outcome = "cancelled"
		}
		progress.Lock()
		props := make(map[string]string, len(progress.properties)+2)
		for k, v := range progress.properties {
			props[k] = v
		}
		progress.Unlock()
		if err != nil {
			props["error_code"], props["error_detail"] = analytics.Diagnostic(class)
		}
		props["duration_ms"] = strconv.FormatInt(time.Since(began).Milliseconds(), 10)
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			props["underlying_exit_code"] = strconv.Itoa(exit.ExitCode())
		}
		analytics.TrackStage(ctx, stage+"_finished", command, outcome, class, props)
	}
}
