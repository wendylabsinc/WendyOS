package commands

import (
	"context"
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

// buildxProbeTimeout bounds the `docker buildx version` probe. A var so tests
// can shrink it.
var buildxProbeTimeout = doctorProbeTimeout

// checkRunBuildPrerequisites reports what blocks a local image build: a full
// disk here or on the device, and a Docker install without Buildx.
// buildHostOffered says whether --build-host is a way out for this project.
func checkRunBuildPrerequisites(ctx context.Context, builder string, buildHostOffered bool, device *agentpb.GetAgentVersionResponse, deps doctorDeps) error {
	failures := buildDiskFailures(imageBuildSpaceDirs(), imageBuildSpaceSteps(builder, buildHostOffered), device)
	resolved, err := resolveOCIExportBuilder(builder)
	// Auto-selected Apple/BuildKit and remote builders retain their own checks.
	// An absent Docker CLI can still be installed by the existing interactive
	// Docker setup, so do not turn that recoverable state into a new refusal.
	if err == nil && resolved == imageBuilderDocker &&
		(imageBuilderWasExplicit(builder) || !shouldAutoAttemptAppleContainerBuilder()) {
		if docker, err := deps.lookPath("docker"); err == nil {
			probe, cancel := context.WithTimeout(ctx, buildxProbeTimeout)
			err := deps.run(probe, docker, "buildx", "version")
			timedOut := probe.Err() != nil
			cancel()
			if ctx.Err() != nil {
				return ctx.Err()
			}
			// A probe that ran out of time proves nothing about Buildx. Let
			// the build go ahead; if Buildx really is missing, the build's own
			// failure explains it (buildxMissingError).
			if err != nil && !timedOut {
				failures = append(failures, buildxNotInstalledError())
			}
		}
	}
	return runPreflightResult(ctx, failures)
}

// checkToolchainBuildPrerequisites is the check for builds that use a
// toolchain instead of an image builder (Swift, Xcode): the two disks only.
func checkToolchainBuildPrerequisites(ctx context.Context, cwd string, device *agentpb.GetAgentVersionResponse) error {
	return runPreflightResult(ctx, buildDiskFailures(toolchainBuildSpaceDirs(cwd), []string{wendyCacheClearStep}, device))
}

func buildDiskFailures(localDirs, localSteps []string, device *agentpb.GetAgentVersionResponse) []error {
	var failures []error
	for _, err := range []error{checkLocalBuildSpace(localDirs, localSteps), checkDeviceDiskSpace(device)} {
		if err != nil {
			failures = append(failures, err)
		}
	}
	return failures
}

func runPreflightResult(ctx context.Context, failures []error) error {
	if len(failures) == 0 {
		return nil
	}
	activationStep(ctx, "build", map[string]string{"build_prerequisites": "false"})
	return &runPreflightFailure{causes: failures}
}
