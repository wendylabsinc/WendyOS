package commands

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

// Independent blockers are reported together. Preserve each typed cause and
// its existing recovery instructions for CLI JSON and telemetry consumers.
type runPreflightFailure struct{ causes []error }

func (e *runPreflightFailure) Error() string {
	var b strings.Builder
	b.WriteString("build prerequisites need attention:")
	for _, cause := range e.causes {
		fmt.Fprintf(&b, "\n  - %s", cause)
	}
	return b.String()
}
func (e *runPreflightFailure) Unwrap() []error { return e.causes }

func checkRunBuildPrerequisites(ctx context.Context, builder string, device *agentpb.GetAgentVersionResponse, deps doctorDeps) error {
	var failures []error
	for _, err := range []error{
		checkLocalBuildSpace(imageBuildSpaceDirs(), imageBuildSpaceSteps(builder, true)),
		checkDeviceDiskSpace(device),
	} {
		if err != nil {
			failures = append(failures, err)
		}
	}
	resolved, err := resolveOCIExportBuilder(builder)
	// Auto-selected Apple/BuildKit and remote builders retain their own checks.
	// An absent Docker CLI can still be installed by the existing interactive
	// Docker setup, so do not turn that recoverable state into a new refusal.
	if err == nil && resolved == imageBuilderDocker &&
		(imageBuilderWasExplicit(builder) || !shouldAutoAttemptAppleContainerBuilder()) {
		if docker, err := deps.lookPath("docker"); err == nil {
			probe, cancel := context.WithTimeout(ctx, doctorProbeTimeout)
			err := deps.run(probe, docker, "buildx", "version")
			cancel()
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if err != nil {
				failures = append(failures, withNextSteps(classifyCommandError(errBuilderUnavailable, errors.Join(errBuildxMissing, err)),
					"Check `docker buildx version`; install or repair the Docker Buildx plugin before building."))
			}
		}
	}
	if len(failures) == 0 {
		return nil
	}
	activationStep(ctx, "build", map[string]string{"build_prerequisites": "false"})
	return &runPreflightFailure{causes: failures}
}
