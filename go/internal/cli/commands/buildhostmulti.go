package commands

import (
	"context"
	"fmt"
	"io"
	"path/filepath"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
	"github.com/wendylabsinc/wendy/go/internal/stagefile"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
)

// buildServicesRemote is buildServicesParallelWithContent with the build host
// doing the building: each selected service is built on `host` and delivered
// by that host straight into the target device, under exactly the
// localhost:<port>/<app>-<service>:latest reference runMultiServiceWithAgent
// then creates the container from.
//
// It exists because the projects with the most to gain from a remote build are
// the ones that were excluded from it (WDY-3120): a multi-service CUDA app for
// a Jetson is minutes of local build per service, which is precisely the cost
// --build-host removes. Nothing about a service is special to a builder — a
// service is a build context plus a Dockerfile, the same thing the
// single-service remote path already sends — so the only work here is to send
// N of them and name each one's destination repository.
//
// The service scheduling, progress UI, per-service failure reporting and
// --keep-going semantics are buildServicesParallelCore's, unchanged: this
// supplies a different serviceBuildFn and nothing else. --service X therefore
// narrows a remote build exactly as it narrows a local one, because the subset
// is resolved before either is reached.
//
// It returns no prepared content. A remote build reports an image digest, not
// the uncompressed layer identities QueryLayers verifies, so there is nothing
// that could authorize a later push-skip. The caller therefore ignores
// persistent skips on this path and invalidates the preceding fingerprint after
// each remote build attempt. Watch-mode preservation remains separate: a
// service already confirmed unchanged and running during this watch session
// does not invoke this function's build callback at all.
func buildServicesRemote(
	ctx context.Context,
	conn *grpcclient.AgentConnection,
	host, cwd string,
	appCfg *appconfig.AppConfig,
	services map[string]*appconfig.ServiceConfig,
	platform string,
	buildArgs map[string]string,
	chunking string,
	skip map[string]bool,
	dockerfiles map[string]string,
	maxConcurrency int,
	quietBuild bool,
	sfOpts ...stagefile.Option,
) (map[string]error, error) {
	if err := assertNoLocalBuilderNeeded(host); err != nil {
		return nil, err
	}

	builder, err := connectBuildHost(ctx, host)
	if err != nil {
		return nil, fmt.Errorf("connecting to build host %s: %w", host, err)
	}
	defer builder.Close()

	caps, err := builder.BuildService.GetBuildCapabilities(ctx, &agentpbv2.GetBuildCapabilitiesRequest{})
	if err != nil {
		return nil, fmt.Errorf("querying build host %s: %w", host, err)
	}
	// Checked once for the group rather than once per service: the answer is a
	// property of the host, and repeating it would print the emulation and
	// chunk-delivery notices once per service.
	if err := checkBuildHostCapabilities(host, caps, platform); err != nil {
		return nil, err
	}
	if err := checkChunkDeliverySupported(host, caps, chunking); err != nil {
		return nil, err
	}

	// One target, resolved once. Its mesh identity, registry port and agent port
	// are the same for every service; only the repository differs.
	base, err := targetPushTargetBase(ctx, conn)
	if err != nil {
		return nil, err
	}

	// The GPU architecture comes from the TARGET, not the build host: a cuda:
	// stage is compiled for the hardware that will RUN the image.
	gpuArch := serviceGPUArch(ctx, cwd, services, conn)

	if !quietBuild {
		cliLogln("Building %d service(s) on %s for %s...", len(services), tui.Value(host), tui.Value(platform))
	}

	build := func(ctx context.Context, contextDir, repo, dockerfile string, buildOut, logOut io.Writer) error {
		// Resolved and packed on THIS machine. A Stagefile compile pins digests
		// and writes its lockfile into the project, which must happen where the
		// repo is; buildServicesParallelCore has already done that compile by
		// the time it calls us, and the ignore file is keyed on the resolved
		// name it passes.
		tarBytes, err := packBuildContext(contextDir, filepath.Join(contextDir, dockerfile))
		if err != nil {
			return err
		}
		manifest, err := pushBuildContext(ctx, builder.ContainerService, tarBytes)
		if err != nil {
			return classifyRemoteBuildError(host, err)
		}

		spec := serviceBuildSpec(base, repo, platform, dockerfile, buildArgs, chunking, manifest)

		result, err := streamRemoteBuild(ctx, builder, spec, buildOut)
		if err != nil {
			return classifyRemoteBuildError(host, err)
		}
		// Delivery is reported per target and not folded into the stream
		// status, so a service whose image never reached the device must not
		// read as built — the deploy below would otherwise create a container
		// from whatever older image still holds this :latest name.
		if err := reportedDeliveryError(result, spec.GetPushTarget().GetAssetId()); err != nil {
			fmt.Fprintf(logOut, "delivery to the device failed: %v\n", err)
			return fmt.Errorf("image built on %s but could not be delivered to the device: %w", host, err)
		}
		return nil
	}

	return buildServicesParallelCore(ctx, build, cwd, appCfg.AppID, services, gpuArch, skip, dockerfiles, maxConcurrency, quietBuild, sfOpts...)
}

// serviceBuildSpec is one service's build, addressed to the build host.
//
// Split out as a pure function because the two names in it are the ones that
// go wrong silently. The repository must be the reference the deploy path then
// creates the container from, or the build succeeds, the image lands under a
// name nobody reads, and the container starts from whatever stale image still
// holds the expected one.
//
// AppId carries the per-service repo rather than the app id. On the build host
// AppId selects nothing but the stable context directory, which BuildImage
// clears and re-extracts under a per-directory lock: one id for the whole group
// would serialise every service behind the others and hand each build a
// directory the previous service had just overwritten, so BuildKit's
// local-source cache would be invalidated on every single build. A name per
// service gives each its own directory and its own cache, and it is the same
// <app>-<service> identity the images already carry.
func serviceBuildSpec(
	base *agentpbv2.PushTarget,
	repo, platform, dockerfile string,
	buildArgs map[string]string,
	chunking string,
	manifest *agentpbv2.ChunkManifest,
) *agentpbv2.BuildSpec {
	return &agentpbv2.BuildSpec{
		AppId:    repo,
		Platform: platform,
		Context:  manifest,
		Chunking: buildChunkingMode(chunking),
		PushTarget: &agentpbv2.PushTarget{
			AssetId:      base.GetAssetId(),
			RegistryPort: base.GetRegistryPort(),
			Repository:   repo + ":latest",
			AgentPort:    base.GetAgentPort(),
		},
		Definition: &agentpbv2.BuildSpec_DockerfileBuild{
			DockerfileBuild: &agentpbv2.DockerfileBuild{
				Dockerfile: dockerfile,
				BuildArgs:  buildArgs,
			},
		},
	}
}
