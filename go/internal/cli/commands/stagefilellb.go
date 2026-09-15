package commands

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"github.com/wendylabsinc/wendy/go/internal/stagefile"
	stagefilesolve "github.com/wendylabsinc/wendy/go/internal/stagefile/solve"
)

type stagefileLLBPlan struct {
	source  string
	options []stagefile.Option
}

var stagefileLLBPlans sync.Map

func stagefileLLBPlanKey(dir, generated string) string {
	abs, err := filepath.Abs(dir)
	if err != nil {
		abs = filepath.Clean(dir)
	}
	return abs + "\x00" + generated
}

// rememberStagefileLLBPlan preserves the exact compiler inputs that produced a
// generated Dockerfile. The lower build layers historically receive only that
// filename; retaining the plan lets the direct backend honor variants, GPU
// targets, --debug, ROS 2 framework options, and download progress unchanged.
func rememberStagefileLLBPlan(dir, generated, source string, opts []stagefile.Option) {
	stagefileLLBPlans.Store(stagefileLLBPlanKey(dir, generated), stagefileLLBPlan{
		source: source, options: append([]stagefile.Option(nil), opts...),
	})
}

func directStagefileLLBPlan(ctx context.Context, dir, dockerfile, builder string) (stagefileLLBPlan, bool, error) {
	source, stagefileGenerated := stagefileSourceForGenerated(dockerfile)
	if !stagefileGenerated {
		return stagefileLLBPlan{}, false, nil
	}
	normalized, err := normalizeImageBuilder(builder)
	if err != nil {
		return stagefileLLBPlan{}, false, err
	}
	useLLB, err := stagefileBackendLLB(stagefileBackendFromContext(ctx), normalized)
	if err != nil || !useLLB {
		return stagefileLLBPlan{}, false, err
	}
	if value, ok := stagefileLLBPlans.Load(stagefileLLBPlanKey(dir, dockerfile)); ok {
		return value.(stagefileLLBPlan), true, nil
	}
	// No plan was recorded for this generated file. A remembered plan is the
	// only reliable signal that a Stagefile was compiled in this process:
	// stagefileSourceForGenerated maps any Dockerfile.generated name to a
	// Stagefile name, but a hand-written Dockerfile that received a safe optimize
	// fix is also written to Dockerfile.generated and has no such source. If the
	// mapped source is absent, this is not a Stagefile build — fall through to
	// the Dockerfile path rather than compiling a file that does not exist (F11).
	if _, statErr := os.Stat(filepath.Join(dir, source)); statErr != nil {
		return stagefileLLBPlan{}, false, nil
	}
	// The source exists but no plan was recorded, so the invocation's variant,
	// GPU target, --debug and ROS 2 options are unrecoverable. Refuse rather
	// than silently rebuild from the source with its declared defaults (F14).
	return stagefileLLBPlan{}, false, fmt.Errorf(
		"cannot build %s with the direct LLB backend: no compiler plan was recorded for %s, so options such as --gpu and --debug would be silently dropped; build without pointing --dockerfile at the generated file, or use --stagefile-backend=dockerfile",
		dockerfile, source)
}

func directStagefileLLBAddress(ctx context.Context, builder string, progress io.Writer) (string, error) {
	normalized, err := resolveOCIExportBuilder(builder)
	if err != nil {
		return "", err
	}
	// An explicit BUILDKIT_HOST points at a daemon the caller manages; use it
	// verbatim for either builder.
	if strings.TrimSpace(os.Getenv("BUILDKIT_HOST")) != "" {
		return stagefilesolve.Address(ctx)
	}
	if normalized != imageBuilderDocker && normalized != imageBuilderBuildkit {
		return "", fmt.Errorf("direct Stagefile LLB builds require docker or buildkit, got %q", normalized)
	}
	// Off-device, buildx's BuildKit daemon lives in a docker-managed container
	// the CLI must create before dialing it. This is true for --builder=buildkit
	// too: without the bootstrap its first use dialed a builder nobody created
	// and failed with the wrapped "may not have been created yet" error (F16).
	// On-device there is no docker to reach the container through, so Address
	// resolves the buildkitd socket directly with nothing to bootstrap.
	if _, dockerErr := exec.LookPath("docker"); dockerErr != nil {
		return stagefilesolve.Address(ctx)
	}
	// Match the Dockerfile OCI path's cross-process setup serialization. The
	// lock is released before solving, so independent builds still overlap once
	// the shared daemon is known to be running.
	releaseLock, err := buildLock.acquire(ctx, progress)
	if err != nil {
		return "", err
	}
	builderName, err := ensureOCIExportBuilder(ctx, progress)
	releaseLock()
	if err != nil {
		return "", err
	}
	return stagefilesolve.AddressForBuildxBuilder(ctx, builderName)
}

func solveStagefileLLB(ctx context.Context, dir, platform, builder string, plan stagefileLLBPlan, output stagefilesolve.Output, progress io.Writer) error {
	compiled, err := stagefile.CompileToLLB(dir, platform, plan.options...)
	if err != nil {
		return fmt.Errorf("compiling %s to LLB: %w", plan.source, err)
	}
	addr, err := directStagefileLLBAddress(ctx, builder, progress)
	if err != nil {
		return err
	}
	if err := stagefilesolve.Run(ctx, addr, stagefilesolve.Request{
		Def: compiled.Definition, Config: compiled.Config, BaseConfig: compiled.BaseConfig,
		Platform: platform, ContextDir: dir, Output: output, Progress: progress,
	}); err != nil {
		return err
	}
	return nil
}

func maybeBuildStagefileLLBToOCI(ctx context.Context, dir, dockerfile, platform, builder, tarPath, layoutDir string, progress io.Writer) (bool, error) {
	plan, ok, err := directStagefileLLBPlan(ctx, dir, dockerfile, builder)
	if err != nil || !ok {
		return ok, err
	}
	output := stagefilesolve.Output{OCILayoutPath: tarPath, OCILayoutDir: layoutDir}
	if err := solveStagefileLLB(ctx, dir, platform, builder, plan, output, progress); err != nil {
		return true, &imageBuildFailedError{err}
	}
	return true, nil
}

func maybeBuildStagefileLLBToDocker(ctx context.Context, dir, dockerfile, imageName, platform, builder string) (bool, error) {
	plan, ok, err := directStagefileLLBPlan(ctx, dir, dockerfile, builder)
	if err != nil || !ok {
		return ok, err
	}
	if builder == imageBuilderBuildkit {
		return true, fmt.Errorf("`wendy build --stagefile-backend=llb` needs Docker to load the completed image; use --builder=docker, or use `wendy run` for an on-device BuildKit build")
	}
	tmp, err := os.CreateTemp("", "wendy-stagefile-*.docker.tar")
	if err != nil {
		return true, err
	}
	tarPath := tmp.Name()
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tarPath)
		return true, err
	}
	_ = os.Remove(tarPath)
	defer os.Remove(tarPath)

	err = runBuildWithProgress(ctx, "Building Stagefile with direct LLB...", dumpRawAlways, func(buildCtx context.Context, stream, _ io.Writer) error {
		return solveStagefileLLB(buildCtx, dir, platform, builder, plan, stagefilesolve.Output{
			DockerTarPath: tarPath, ImageRef: imageName,
		}, stream)
	})
	if err != nil {
		return true, err
	}
	load := exec.CommandContext(ctx, "docker", "load", "--input", tarPath)
	load.Stdout = os.Stdout
	load.Stderr = os.Stderr
	if err := load.Run(); err != nil {
		return true, fmt.Errorf("loading direct LLB image into Docker: %w", err)
	}
	cliSuccess("Build completed successfully.")
	return true, nil
}
