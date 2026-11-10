package jobops

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// SubmitObserved optionally waits for terminal evidence without model polling.
// A deadline never cancels the detached job. An observation failure preserves
// the confirmed receipt so a caller can inspect/retry the same named job.
func SubmitObserved(ctx context.Context, conn *grpcclient.AgentConnection, spec Spec, waitSeconds int) (map[string]any, error) {
	if waitSeconds < 0 || waitSeconds > 3600 {
		return nil, fmt.Errorf("wait_seconds must be 0..3600")
	}
	receipt, err := Submit(ctx, conn, spec)
	if err != nil || waitSeconds == 0 {
		return receipt, err
	}
	jobPath, ok := receipt["job_path"].(string)
	if !ok || jobPath == "" {
		receipt["observation_error"] = "confirmed response did not contain a job path; inspect the named job before retrying"
		receipt["completion"] = "unknown"
		return receipt, nil
	}
	observation, err := WaitTerminal(ctx, conn, jobPath, waitSeconds)
	if err != nil {
		receipt["observation_error"] = err.Error()
		receipt["completion"] = "unknown"
		return receipt, nil
	}
	receipt["observation"] = observation
	// The current observation supersedes the initial submission snapshot.
	// Leaving "starting" at the top level after completed steps causes agents
	// to inspect again despite already having terminal evidence.
	if state, ok := observation["reported_state"].(string); ok && state != "" {
		receipt["reported_state"] = state
	}
	if stage, ok := observation["stage"].(string); ok && stage != "" {
		receipt["stage"] = stage
	}
	observation["expected_steps"] = len(spec.Steps)
	observation["step_check"] = checkStepOutcomes(observation, spec)
	return receipt, nil
}

// WaitTerminal keeps routine polling outside the model. Every underlying host
// RPC remains bounded to 30 seconds; cancelling observation never signals a job.
func WaitTerminal(ctx context.Context, conn *grpcclient.AgentConnection, jobPath string, waitSeconds int) (map[string]any, error) {
	if waitSeconds < 0 || waitSeconds > 3600 {
		return nil, fmt.Errorf("wait_seconds must be 0..3600")
	}
	chunk := min(waitSeconds, 30)
	if err := ValidateInspect(jobPath, InspectOptions{WaitSeconds: chunk, UntilTerminal: true}); err != nil {
		return nil, err
	}
	deadline := time.Now().Add(time.Duration(waitSeconds) * time.Second)
	ctx, cancel := context.WithDeadline(ctx, deadline.Add(10*time.Second))
	defer cancel()
	retries := 0
	for {
		remaining := max(0, int(math.Ceil(time.Until(deadline).Seconds())))
		observation, err := Inspect(ctx, conn, jobPath, InspectOptions{WaitSeconds: min(remaining, 30), UntilTerminal: true})
		if err != nil {
			// A lost observation stream does not mean the detached job failed.
			// Retry only reads of this exact path on this same authenticated
			// connection. Never resubmit, choose another target, or retry an
			// authorization, malformed-response, or missing-evidence error.
			delay := time.Duration(1<<retries) * 250 * time.Millisecond
			if status.Code(err) == codes.Unavailable && retries < 3 && ctx.Err() == nil && time.Now().Add(delay).Before(deadline) {
				timer := time.NewTimer(delay)
				select {
				case <-ctx.Done():
					timer.Stop()
					return nil, ctx.Err()
				case <-timer.C:
				}
				retries++
				continue
			}
			if retries > 0 {
				return nil, fmt.Errorf("observation failed after %d read-only transport retries: %w", retries, err)
			}
			return nil, err
		}
		if retries > 0 {
			observation["transport_retries"] = retries
		}
		state, _ := observation["reported_state"].(string)
		if state == "completed" || state == "failed" || state == "stopped" || state == "cancelled" || !time.Now().Before(deadline) {
			return observation, nil
		}
	}
}

func checkStepOutcomes(observation map[string]any, spec Spec) string {
	state, _ := observation["reported_state"].(string)
	if state == "failed" {
		return "failed"
	}
	if state != "completed" {
		return "pending"
	}
	outcomes, ok := observation["step_outcomes"].([]any)
	if !ok || len(outcomes) != len(spec.Steps) || observation["step_outcomes_omitted"] != float64(0) {
		return "unknown"
	}
	for i, raw := range outcomes {
		step, ok := raw.(map[string]any)
		if !ok || step["label"] != spec.Steps[i].Name {
			return "unknown"
		}
		code, codeKnown := step["returncode"].(float64)
		timedOut, timeoutKnown := step["timed_out"].(bool)
		if !codeKnown || !timeoutKnown {
			return "unknown"
		}
		if code != 0 || timedOut {
			return "failed"
		}
	}
	return "passed"
}
