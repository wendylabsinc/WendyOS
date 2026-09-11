package commands

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
)

// errBuilderWithBuildHost is returned when --builder and --build-host are both
// given. --builder selects the LOCAL image builder, which the remote path never
// runs, so honouring either one silently means ignoring the other.
var errBuilderWithBuildHost = errors.New("--builder selects a local image builder and cannot be combined with --build-host; drop one")

// loadBuildHostDefault is the seam tests replace, in the style docker.go's
// imageBuilderLookPath already establishes. Nothing here calls os.Setenv, which
// is process-global and would leak between parallel tests.
var loadBuildHostDefault = configBuildHostDefault

func configBuildHostDefault() (string, error) {
	cfg, err := config.Load()
	if err != nil {
		return "", fmt.Errorf("loading config: %w", err)
	}
	return strings.TrimSpace(cfg.DefaultBuildHost), nil
}

// resolveBuildHostName returns the device that should build, most explicit
// signal first: the --build-host flag, then the persisted per-developer
// default. An empty result means "build locally" and must leave every existing
// local path untouched.
func resolveBuildHostName(flagValue string) (string, error) {
	if v := strings.TrimSpace(flagValue); v != "" {
		return v, nil
	}
	return loadBuildHostDefault()
}

// errBuildHostOnBuildCmd is returned by `wendy build --build-host`. Only
// `wendy run` can delegate a build: the remote path's whole delivery step is a
// push into the TARGET device's registry, and `wendy build` has no target. A
// remote `wendy build` would leave the image on the build host and nowhere the
// developer asked for, so the flag is refused rather than half-honoured.
var errBuildHostOnBuildCmd = errors.New(
	"--build-host is supported by `wendy run`, not `wendy build`: a remote build delivers the image to the target device, and `wendy build` has no target; use `wendy run --build-host` instead")

// validateBuildHostFlags rejects flag combinations where both values cannot be
// honoured, so the conflict surfaces before a build rather than as a silently
// ignored flag.
func validateBuildHostFlags(buildHost, builder string) error {
	if strings.TrimSpace(buildHost) != "" && strings.TrimSpace(builder) != "" {
		return errBuilderWithBuildHost
	}
	return nil
}

// resolveAndValidateRunBuildHost resolves the persisted default before checking
// --builder. Validating only the raw flag lets `defaultBuildHost` silently win
// over an explicitly requested local builder later in the run path.
func resolveAndValidateRunBuildHost(flagValue, builder string) (string, error) {
	host, err := resolveBuildHostName(flagValue)
	if err != nil {
		return "", err
	}
	if err := validateBuildHostFlags(host, builder); err != nil {
		return "", err
	}
	return host, nil
}

func rejectUnsupportedBuildHostProject(host, project string) error {
	if strings.TrimSpace(host) == "" {
		return nil
	}
	return fmt.Errorf("build host %s cannot build %s; remote builds currently support single-service container image projects only (remove --build-host or clear defaultBuildHost to build locally)", host, project)
}

// splitFleetDevices reads a comma-separated --device value into the primary
// device and the rest.
//
// The primary is not just the first name: it is the device every existing
// decision in the run path is already made against — GPU architecture for a
// cuda: stage, agent OS, build-arg hints. Those must come from one device, and
// silently averaging them across a fleet would produce an image correct for
// none of them.
//
// Order is preserved and duplicates are refused. A fleet listed as larger than
// it is would report a device twice and hide that another was never named.
func splitFleetDevices(value string) (primary string, extras []string, err error) {
	parts := strings.Split(value, ",")
	seen := make(map[string]struct{}, len(parts))
	names := make([]string, 0, len(parts))
	for _, p := range parts {
		name := strings.TrimSpace(p)
		if name == "" {
			return "", nil, fmt.Errorf("--device %q has an empty entry; list devices as a,b,c", value)
		}
		if _, dup := seen[name]; dup {
			return "", nil, fmt.Errorf("--device lists %q more than once", name)
		}
		seen[name] = struct{}{}
		names = append(names, name)
	}
	return names[0], names[1:], nil
}

// validateFleetRun rejects the combinations that cannot be honoured, before any
// device is contacted.
func validateFleetRun(extras []string, buildHost string, detach bool) error {
	if len(extras) == 0 {
		return nil
	}
	// Without a build host each device would build the image locally, in turn.
	// The point of naming several devices is that the expensive stage happens
	// once, so this combination asks for the opposite of the feature.
	if strings.TrimSpace(buildHost) == "" {
		return errors.New("deploying to several devices needs --build-host: without it each device would trigger its own local build, which is what naming them together avoids")
	}
	// Log streaming follows one container. Interleaving several devices' logs
	// into one terminal produces output no one can attribute, and the run would
	// appear to hang on whichever device is quietest.
	if !detach {
		return errors.New("deploying to several devices needs --detach: logs are streamed from a single container, so there is no sensible thing to follow across a fleet")
	}
	return nil
}

// checkFleetDeliverySupported refuses a fleet build against a build host that
// would silently drop the extra devices.
//
// proto3 discards unknown fields, so an agent predating push_targets receives a
// spec whose fleet it cannot see and whose single target is empty. It would
// build and deliver nowhere while this CLI reported a fleet deploy. Refusing is
// the only safe answer: degrading to the first device would deploy to one
// machine and claim several, which is worse than an error and invisible.
func checkFleetDeliverySupported(host string, resp *agentpbv2.GetBuildCapabilitiesResponse, deviceCount int) error {
	if deviceCount <= 1 || resp.GetMultiTargetDelivery() {
		return nil
	}
	return fmt.Errorf("build host %s cannot deliver one build to several devices; update its agent, or deploy to one device at a time", host)
}

// checkChunkDeliverySupported refuses --chunking=force against a build host
// whose agent predates chunked delivery: it would discard the mode and push
// through the registry, which is the silent fallback force exists to forbid.
// auto and off need nothing new from the host — an older one pushes through
// the registry, which is what off asks for and what auto accepts, though auto
// is told.
func checkChunkDeliverySupported(host string, resp *agentpbv2.GetBuildCapabilitiesResponse, mode string) error {
	if resp.GetChunkDelivery() {
		return nil
	}
	switch mode {
	case chunkingForce:
		return fmt.Errorf("build host %s predates chunked delivery, so --chunking=force cannot be honoured there; update its agent, or use --chunking=auto or off", host)
	case chunkingOff:
		return nil
	default:
		cliNotice("build host %s predates chunked delivery; the image will be pushed through the device's registry", host)
		return nil
	}
}

// buildChunkingMode carries --chunking to the build host, so the flag means the
// same thing whichever machine delivers the image. Empty is auto, as locally.
func buildChunkingMode(mode string) agentpbv2.ChunkingMode {
	switch mode {
	case chunkingForce:
		return agentpbv2.ChunkingMode_CHUNKING_MODE_FORCE
	case chunkingOff:
		return agentpbv2.ChunkingMode_CHUNKING_MODE_OFF
	default:
		return agentpbv2.ChunkingMode_CHUNKING_MODE_AUTO
	}
}

// checkBuildHostCapabilities refuses a build host before any context is
// transferred. Every failure names the host, and none falls back to a local
// build: a long build the developer believed was running on the Spark is worse
// than an error.
func checkBuildHostCapabilities(host string, resp *agentpbv2.GetBuildCapabilitiesResponse, platform string) error {
	if !resp.GetBuilderEnabled() {
		return fmt.Errorf("%s is not configured as a build host; enable the builder role on that device, or omit --build-host to build locally", host)
	}
	if !resp.GetBuildkitAvailable() {
		// On darwin this is a design fact, not a misconfiguration: the Mac agent
		// runs Linux containers through Apple Container, which has no BuildKit
		// underneath. Saying so stops it reading as a bug to be fixed.
		if strings.EqualFold(resp.GetOs(), "darwin") {
			return fmt.Errorf("%s has no BuildKit daemon: macOS hosts run containers through Apple Container, which has no BuildKit underneath, so a Mac cannot be a build host", host)
		}
		return fmt.Errorf("%s is enabled as a build host but has no BuildKit daemon; install buildkitd there and start it on unix:///run/buildkit/buildkitd.sock, or omit --build-host to build locally", host)
	}
	if err := checkBuildkitRootSpace(host, resp); err != nil {
		return err
	}
	if slices.Contains(resp.GetNativePlatforms(), platform) {
		return nil
	}
	if slices.Contains(resp.GetEmulatedPlatforms(), platform) {
		cliNotice("%s builds %s under emulation; expect it to be slower than a native build", host, platform)
		return nil
	}
	return fmt.Errorf("%s cannot build %s: it builds %s natively and emulates %s",
		host, platform,
		formatPlatformList(resp.GetNativePlatforms()),
		formatPlatformList(resp.GetEmulatedPlatforms()))
}

func formatPlatformList(platforms []string) string {
	if len(platforms) == 0 {
		return "nothing"
	}
	return strings.Join(platforms, ", ")
}

// assertNoLocalBuilderNeeded documents and enforces the neo → spark → robot
// requirement: with --build-host, nothing on this path may look for a local
// container builder. It exists so that guarantee is covered by a test rather
// than left as a convention someone later breaks by adding a daemon bootstrap.
func assertNoLocalBuilderNeeded(host string) error {
	if strings.TrimSpace(host) == "" {
		return errors.New("assertNoLocalBuilderNeeded called without a build host")
	}
	return nil
}

// classifyRemoteBuildError separates "the build failed" from "the build
// succeeded but could not be delivered". The remedies diverge — a build-file
// fix versus mesh reachability or registry auth — so collapsing them would send
// the developer to debug the wrong layer.
func classifyRemoteBuildError(host string, err error) error {
	if err == nil {
		return nil
	}
	const deliveryFailurePrefix = "pushing the built image to the target device failed:"
	if status.Code(err) == codes.Unavailable && strings.HasPrefix(status.Convert(err).Message(), deliveryFailurePrefix) {
		return fmt.Errorf("image built on %s but could not be delivered to the device: %w", host, err)
	}
	// Generic Unavailable and DeadlineExceeded errors can happen before or
	// during the build. Claiming the image was built sends the developer to
	// debug delivery when the build host may simply have disconnected.
	// Marked as an image-build failure so the caller surfaces it directly rather
	// than masking it behind a local fallback.
	return &imageBuildFailedError{err: fmt.Errorf("build on %s failed or did not complete: %w", host, err)}
}

// runRemoteBuild builds the image on another WendyOS device, has that device
// deliver it to the target over the mesh — by chunks into the target's content
// store, registered under the same localhost:<port>/<repo> name a registry push
// would have given it — and then creates the container by that name through
// the unchanged registry-push deploy path.
//
// Nothing here touches a local container builder: no ensureDockerDaemon, no
// ensureBuildxBuilder, no ensureAppleContainerSystemForBuilder. That is the
// point of the feature — see assertNoLocalBuilderNeeded.
func runRemoteBuild(
	ctx context.Context,
	target *grpcclient.AgentConnection,
	host, cwd string,
	appCfg *appconfig.AppConfig,
	platform, dockerfile string,
	buildArgs map[string]string,
	deployEnv []string,
	opts runOptions,
) error {
	if err := assertNoLocalBuilderNeeded(host); err != nil {
		return err
	}

	builder, err := connectBuildHost(ctx, host)
	if err != nil {
		return fmt.Errorf("connecting to build host %s: %w", host, err)
	}
	defer builder.Close()

	caps, err := builder.BuildService.GetBuildCapabilities(ctx, &agentpbv2.GetBuildCapabilitiesRequest{})
	if err != nil {
		return fmt.Errorf("querying build host %s: %w", host, err)
	}
	if err := checkBuildHostCapabilities(host, caps, platform); err != nil {
		return err
	}
	if err := checkChunkDeliverySupported(host, caps, opts.chunking); err != nil {
		return err
	}

	// Resolve the build file on THIS machine: a Stagefile compile pins digests
	// and writes its lockfile into the project, which must happen where the repo
	// is, not in a scratch dir on the builder.
	//
	// The GPU architecture comes from the TARGET, not the build host: a cuda:
	// stage is compiled for the hardware that will RUN the image. Asking the
	// Spark would pin the image to the Spark's GPU and quietly mis-target the
	// robot.
	resolved, err := prepareDockerBuildFile(cwd, dockerfile, resolveGPUArch(ctx, cwd, opts.gpuArch, target))
	if err != nil {
		return err
	}
	resolvedPath := filepath.Join(cwd, resolved)

	tarBytes, err := packBuildContext(cwd, resolvedPath)
	if err != nil {
		return err
	}

	pushTarget, err := targetPushTarget(ctx, target, appCfg)
	if err != nil {
		return err
	}
	// Kept as a slice so the delivery path, the capability guard and the
	// reporting are the same code for one device as for ten -- a fleet path that
	// only runs when someone passes several devices is a fleet path nobody has
	// tested.
	pushTargets := []*agentpbv2.PushTarget{pushTarget}
	// Names for the report. The primary is whatever --device resolved to.
	nameOf := map[int32]string{pushTarget.GetAssetId(): deviceFlag}

	// Resolve every extra device BEFORE building. A fleet member that cannot be
	// reached is worth an error now, not after the build host has spent minutes
	// on an image most of the fleet will never see.
	fleetConns := make([]*grpcclient.AgentConnection, 0, len(opts.fleetDevices))
	defer func() {
		for _, c := range fleetConns {
			c.Close()
		}
	}()
	for _, name := range opts.fleetDevices {
		conn, err := connectFleetDevice(ctx, name)
		if err != nil {
			return fmt.Errorf("connecting to %s: %w", name, err)
		}
		fleetConns = append(fleetConns, conn)

		// One build produces one image for one platform. A device of a different
		// architecture cannot run it, and finding that out after delivery would
		// leave a camera holding an image it can never start.
		if err := assertSamePlatform(ctx, name, conn, platform); err != nil {
			return err
		}
		t, err := targetPushTarget(ctx, conn, appCfg)
		if err != nil {
			return fmt.Errorf("resolving %s as a delivery target: %w", name, err)
		}
		pushTargets = append(pushTargets, t)
		nameOf[t.GetAssetId()] = name
	}

	if err := checkFleetDeliverySupported(host, caps, len(pushTargets)); err != nil {
		return err
	}

	buildTitle := fmt.Sprintf("Building on %s for %s...", tui.Value(host), tui.Value(platform))
	var buildResult *agentpbv2.BuildImageResult
	if err := runBuildWithProgress(ctx, buildTitle, dumpRawAlways, func(buildCtx context.Context, stream, logw io.Writer) error {
		manifest, err := pushBuildContext(buildCtx, builder.ContainerService, tarBytes)
		if err != nil {
			return err
		}
		// A single device keeps using the original field. The fleet field is
		// invisible to an agent that predates it -- proto3 drops unknown fields
		// -- so sending it for a one-device build would break exactly the case
		// that works today, against exactly the agents most likely to be in the
		// field. checkFleetDeliverySupported above is what allows the other
		// branch.
		spec := &agentpbv2.BuildSpec{
			AppId:    appCfg.AppID,
			Platform: platform,
			Context:  manifest,
			Chunking: buildChunkingMode(opts.chunking),
			Definition: &agentpbv2.BuildSpec_DockerfileBuild{
				DockerfileBuild: &agentpbv2.DockerfileBuild{
					Dockerfile: resolved,
					BuildArgs:  buildArgs,
				},
			},
		}
		if len(pushTargets) == 1 {
			spec.PushTarget = pushTargets[0]
		} else {
			spec.PushTargets = pushTargets
		}
		buildResult, err = streamRemoteBuild(buildCtx, builder, spec, stream)
		return err
	}); err != nil {
		return classifyRemoteBuildError(host, err)
	}

	appConfigData, err := json.Marshal(appCfg)
	if err != nil {
		return fmt.Errorf("marshaling app config: %w", err)
	}
	createReq := &agentpb.CreateContainerRequest{
		ImageName:     localRegistryReference(ctx, target, appCfg),
		AppName:       appCfg.AppID,
		AppConfig:     appConfigData,
		RestartPolicy: resolveRestartPolicy(opts),
		UserArgs:      opts.userArgs,
		Env:           deployEnv,
	}
	if len(fleetConns) == 0 {
		if err := reportedDeliveryError(buildResult, pushTargets[0].GetAssetId()); err != nil {
			return fmt.Errorf("image built on %s but could not be delivered to %s: %w", host, deviceFlag, err)
		}
		return startAndStreamContainer(ctx, target, appCfg, createReq, opts)
	}

	// Fleet run. Every device gets the image; each still needs its own container
	// created, and one device failing that must not strand the others -- the
	// image is already on them.
	report := make([]*agentpbv2.DeliveryResult, 0, len(pushTargets))
	primaryErr := startAfterReportedDelivery(buildResult, pushTargets[0].GetAssetId(), func() error {
		return startAndStreamContainer(ctx, target, appCfg, createReq, opts)
	})
	report = append(report, deliveryOutcome(pushTargets[0].GetAssetId(), primaryErr))

	for i, conn := range fleetConns {
		req := &agentpb.CreateContainerRequest{
			// Resolved per device: the reference names that device's own registry.
			ImageName:     localRegistryReference(ctx, conn, appCfg),
			AppName:       appCfg.AppID,
			AppConfig:     appConfigData,
			RestartPolicy: resolveRestartPolicy(opts),
			UserArgs:      opts.userArgs,
			Env:           deployEnv,
		}
		assetID := pushTargets[i+1].GetAssetId()
		targetErr := startAfterReportedDelivery(buildResult, assetID, func() error {
			return startAndStreamContainer(ctx, conn, appCfg, req, opts)
		})
		report = append(report, deliveryOutcome(assetID, targetErr))
	}

	lines, failed := fleetDeliveryReport(report, nameOf)
	cliLogln("Deployed from one build on %s:", tui.Value(host))
	for _, l := range lines {
		cliLogln("%s", l)
	}
	if failed > 0 {
		return &errPartialFleetDeploy{failed: failed, total: len(report)}
	}
	return nil
}

func startAfterReportedDelivery(result *agentpbv2.BuildImageResult, assetID int32, start func() error) error {
	if err := reportedDeliveryError(result, assetID); err != nil {
		return err
	}
	return start()
}

// reportedDeliveryError returns the build host's failure for one target. An
// empty delivery list is the legacy single-target contract: older build hosts
// reported delivery failure through the stream status and success by EOF, so
// there is no per-target result to inspect.
//
// Once a host sends any per-target results, absence is a failure. Failing
// closed prevents the CLI from creating a container from an older image that
// happens to remain registered under the same :latest name.
func reportedDeliveryError(result *agentpbv2.BuildImageResult, assetID int32) error {
	if result == nil || len(result.GetDeliveries()) == 0 {
		return nil
	}
	for _, delivery := range result.GetDeliveries() {
		if delivery.GetAssetId() != assetID {
			continue
		}
		if delivery.GetDelivered() {
			return nil
		}
		reason := delivery.GetError()
		if reason == "" {
			reason = "delivery failed (no reason reported)"
		}
		return errors.New(reason)
	}
	return fmt.Errorf("build host returned no delivery result for asset %d", assetID)
}

// deliveryOutcome records what happened to one device without collapsing a
// failure into the run's overall status. See fleetDeliveryReport for why a
// partial fleet deploy must not read as success.
func deliveryOutcome(assetID int32, err error) *agentpbv2.DeliveryResult {
	if err != nil {
		return &agentpbv2.DeliveryResult{AssetId: assetID, Error: err.Error()}
	}
	return &agentpbv2.DeliveryResult{AssetId: assetID, Delivered: true}
}

// fleetDeliveryReport turns the agent's per-device outcomes into what the
// developer reads, and into the exit status.
//
// It exists as a pure function because the rule it encodes is the one worth
// testing: a run where some devices missed the image is NOT a success. Every
// other failure mode in this feature has been a wrong outcome reported as a
// right one, and a fleet deploy is the easiest place yet to hide one — the
// build succeeds, most devices update, and nobody looks at the tail of the
// output.
func fleetDeliveryReport(deliveries []*agentpbv2.DeliveryResult, nameOf map[int32]string) (lines []string, failed int) {
	for _, d := range deliveries {
		name := nameOf[d.GetAssetId()]
		if name == "" {
			name = fmt.Sprintf("asset %d", d.GetAssetId())
		}
		if d.GetDelivered() {
			lines = append(lines, fmt.Sprintf("  %-24s delivered", name))
			continue
		}
		failed++
		reason := d.GetError()
		if reason == "" {
			// An agent that reports a failure without saying why still must not
			// be summarised as "fine".
			reason = "delivery failed (no reason reported)"
		}
		lines = append(lines, fmt.Sprintf("  %-24s FAILED: %s", name, reason))
	}
	return lines, failed
}

// errPartialFleetDeploy is returned when the image reached some devices and not
// others. Named so the exit path cannot accidentally treat it as success.
type errPartialFleetDeploy struct {
	failed, total int
}

func (e *errPartialFleetDeploy) Error() string {
	return fmt.Sprintf("deployed to %d of %d devices; %d failed — see the report above",
		e.total-e.failed, e.total, e.failed)
}

// connectBuildHost resolves and connects to the build host by name, reusing the
// same connect machinery as --device so LKG cache, mDNS and cloud fallback all
// apply unchanged.
//
// The cloud fallback is what makes this feature usable by the developer it was
// written for. resolveTarget alone is direct/LAN only, so a remote developer --
// precisely the person who does not want to build locally -- could never reach
// a build host, while the deploy target resolved over the tunnel in the same
// command. `wendy cloud run --build-host` was the only working spelling, and it
// prints a deprecation notice pointing back at this one.
//
// host is passed explicitly: the fallback must not read --device, which names
// the TARGET. See resolveWithCloudFallback.
func connectBuildHost(ctx context.Context, host string) (*grpcclient.AgentConnection, error) {
	sel, err := resolveWithCloudFallback(ctx, host, SelectDevice(host), NonInteractive(), SuppressUpdateCheck())
	if err != nil {
		return nil, err
	}
	if sel.Agent == nil {
		sel.Close()
		return nil, fmt.Errorf("%s is not a WendyOS device with an agent; a build host must be one", host)
	}
	return sel.Agent, nil
}

// targetPushTarget describes where the BUILD HOST should push, as a mesh peer
// rather than a hostname.
//
// The asset id is deliberate: a name like device-<id>.cloud.wendy.dev only
// resolves where the mesh DNS server runs, which excludes adopted Linux hosts
// whose resolver already owns 127.0.0.53 — so a reachable peer looked
// unreachable. The build host's peer dialer needs only the id (WDY-2356).
func targetPushTarget(ctx context.Context, target *grpcclient.AgentConnection, appCfg *appconfig.AppConfig) (*agentpbv2.PushTarget, error) {
	resp, err := target.ProvisioningService.IsProvisioned(ctx, &agentpb.IsProvisionedRequest{})
	if err != nil {
		return nil, fmt.Errorf("determining the target device's mesh identity: %w", err)
	}
	prov, ok := resp.GetResponse().(*agentpb.IsProvisionedResponse_Provisioned)
	if !ok {
		return nil, fmt.Errorf("the target device is not provisioned, so a build host cannot reach its registry; provision it or omit --build-host")
	}
	agentOS, err := targetAgentOS(ctx, target)
	if err != nil {
		return nil, err
	}
	agentPort, err := connectedTargetAgentPort(target)
	if err != nil {
		return nil, fmt.Errorf("determining the target device's agent port: %w", err)
	}
	return &agentpbv2.PushTarget{
		AssetId:      prov.Provisioned.GetAssetId(),
		RegistryPort: uint32(registryPort(agentOS)),
		Repository:   strings.ToLower(appCfg.AppID) + ":latest",
		AgentPort:    agentPort,
	}, nil
}

// connectedTargetAgentPort reports the mTLS endpoint the CLI actually used.
// Direct and session-proxy connections retain that address in Addr. A cloud
// connection is a pre-built gRPC channel with no network Addr; cloud tunnelling
// currently reaches the standard provisioned-agent port, so use that default.
func connectedTargetAgentPort(target *grpcclient.AgentConnection) (uint32, error) {
	if target.Addr == "" {
		return uint32(defaultAgentPort + agentMTLSPortOffset), nil
	}
	_, portText, err := net.SplitHostPort(target.Addr)
	if err != nil {
		return 0, fmt.Errorf("parsing connected address %q: %w", target.Addr, err)
	}
	port, err := strconv.ParseUint(portText, 10, 16)
	if err != nil || port == 0 {
		return 0, fmt.Errorf("invalid port in connected address %q", target.Addr)
	}
	return uint32(port), nil
}

// localRegistryReference is the same image as the target itself sees it. The
// target's registry names images from its OWN listen address, so whatever
// address the build host pushed to, the image lands here.
func localRegistryReference(ctx context.Context, target *grpcclient.AgentConnection, appCfg *appconfig.AppConfig) string {
	agentOS, _ := targetAgentOS(ctx, target)
	return fmt.Sprintf("localhost:%d/%s:latest", registryPort(agentOS), strings.ToLower(appCfg.AppID))
}

func targetAgentOS(ctx context.Context, target *grpcclient.AgentConnection) (string, error) {
	resp, err := target.AgentService.GetAgentVersion(ctx, &agentpb.GetAgentVersionRequest{})
	if err != nil {
		return "", fmt.Errorf("querying the target device: %w", err)
	}
	if os := resp.GetOs(); os != "" {
		return os, nil
	}
	return "linux", nil
}

// streamRemoteBuild drives one BuildImage stream, forwarding the build host's
// plain-mode output into the CLI's existing progress renderer.
func streamRemoteBuild(ctx context.Context, builder *grpcclient.AgentConnection, spec *agentpbv2.BuildSpec, out io.Writer) (*agentpbv2.BuildImageResult, error) {
	stream, err := builder.BuildService.BuildImage(ctx)
	if err != nil {
		return nil, err
	}
	if err := stream.Send(&agentpbv2.BuildImageRequest{Spec: spec}); err != nil {
		return nil, err
	}
	if err := stream.CloseSend(); err != nil {
		return nil, err
	}
	return consumeBuildProgress(stream.Recv, out)
}

// consumeBuildProgress forwards build log lines until the stream ends.
//
// Split from streamRemoteBuild so the termination rule can be tested without a
// gRPC connection: it is the part with a way to go wrong.
func consumeBuildProgress(recv func() (*agentpbv2.BuildImageProgress, error), out io.Writer) (*agentpbv2.BuildImageResult, error) {
	for {
		msg, err := recv()
		if err == io.EOF {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		if line := msg.GetLogLine(); line != "" {
			fmt.Fprintln(out, line)
		}
		// The result event is terminal by definition. Today the agent returns
		// right after sending it, so EOF follows immediately — but a client that
		// stops only on EOF is one trailing event away from waiting forever on a
		// build that has already finished.
		if result := msg.GetResult(); result != nil {
			return result, nil
		}
	}
}

// connectFleetDevice connects to one extra delivery target by name.
//
// The name is passed EXPLICITLY, and that is the whole subtlety. The cloud
// fallback otherwise reads --device, which after the fleet split names the
// PRIMARY -- so every member of the fleet would tunnel to the same machine, and
// a deploy meant for three cameras would build one image and hand it to one
// device three times while reporting three deliveries.
//
// Before this, a fleet member not on the developer's network failed with "name
// resolver error: produced zero addresses". Measured on main: deploying
// ccr2,ccr1 from a laptop on neither network connected the primary and the
// build host over the tunnel, then stopped here -- three devices, three
// resolvers, and only this one direct-only.
func connectFleetDevice(ctx context.Context, name string) (*grpcclient.AgentConnection, error) {
	sel, err := resolveWithCloudFallback(ctx, name, SelectDevice(name), NonInteractive(), SuppressUpdateCheck())
	if err != nil {
		return nil, err
	}
	if sel.Agent == nil {
		sel.Close()
		return nil, fmt.Errorf("%s is not a WendyOS device with an agent", name)
	}
	return sel.Agent, nil
}

// assertSamePlatform refuses a fleet whose members do not all run the image
// that is about to be built.
//
// One build produces one image for one platform. Delivering it to a device of
// another architecture leaves that camera holding an image it can never start
// -- a failure that surfaces long after the deploy reported success, on the
// device least likely to be watched.
func assertSamePlatform(ctx context.Context, name string, conn *grpcclient.AgentConnection, platform string) error {
	// Resolved the SAME way the primary device's platform was, via
	// resolveAgentPlatform. Comparing the raw agent OS instead looks equivalent
	// and is not: a WendyOS device reports "wendyos", never "linux", so a check
	// against the platform string's OS half rejects every device it is asked
	// about. Measured -- it refused ccr1 for a linux/arm64 build that ccr1 runs
	// perfectly well.
	versionResp, err := conn.AgentService.GetAgentVersion(ctx, &agentpb.GetAgentVersionRequest{})
	if err != nil {
		return fmt.Errorf("determining what %s runs: %w", name, err)
	}
	arch := versionResp.GetCpuArchitecture()
	if arch == "" {
		arch = "arm64"
	}
	got := resolveAgentPlatform("", versionResp.GetOs(), arch)
	if !strings.EqualFold(got, platform) {
		return fmt.Errorf("%s is %s but this build targets %s; one build makes one image for one platform, so deploy %s separately",
			name, got, platform, name)
	}
	return nil
}

// Thresholds for the build host's BuildKit cache directory.
//
// A cache is not incidental: an edge image that compiles a TensorRT engine
// writes gigabytes, and the cache is designed to grow rather than to be
// reclaimed. These numbers are about the FILESYSTEM the cache sits on, not the
// size of one build.
const (
	// Below this, refuse. A build that fills the filesystem it is writing to is
	// not a failed build -- on an image-based OS that filesystem is the one the
	// device boots from, so the outcome is a damaged device. Measured case: a
	// Jetson AGX Thor whose default cache location had 4.6 GB free on the A/B
	// rootfs, beside a data partition with 862 GB.
	buildkitRootMinFreeBytes = 8 << 30 // 8 GiB
	// Between the two, build but say so. Plenty of real images fit; the point is
	// that the operator finds out before the disk does.
	buildkitRootWarnFreeBytes = 25 << 30 // 25 GiB
)

// checkBuildkitRootSpace refuses, or warns about, a build host whose BuildKit
// cache is on a filesystem too small to hold one.
//
// An older agent reports no support bit and preserves the pre-existing behavior.
// Once an agent advertises support, however, missing data is an inspection
// failure and cannot be treated as evidence that the cache filesystem is safe.
func checkBuildkitRootSpace(host string, resp *agentpbv2.GetBuildCapabilitiesResponse) error {
	warning, err := assessBuildkitRootSpace(host, resp)
	if err != nil {
		return err
	}
	if warning != "" {
		cliNotice("%s", warning)
	}
	return nil
}

// assessBuildkitRootSpace contains the policy separately from terminal output,
// making the refusal and warning boundaries directly testable.
func assessBuildkitRootSpace(host string, resp *agentpbv2.GetBuildCapabilitiesResponse) (string, error) {
	if !resp.GetBuildkitRootInspectionSupported() {
		// An older agent predates these fields. Preserve the behavior that worked
		// before this safety check rather than turning a CLI update into an outage.
		return "", nil
	}
	root, free := resp.GetBuildkitRoot(), resp.GetBuildkitRootFreeBytes()
	if root == "" || resp.GetBuildkitRootTotalBytes() == 0 {
		return "", fmt.Errorf("%s could not verify where its BuildKit cache is stored or how much space it has; inspect the running buildkitd configuration, restart it, and retry, or build elsewhere", host)
	}
	if free < buildkitRootMinFreeBytes {
		return "", fmt.Errorf(
			"%s keeps its BuildKit cache in %s, which has only %s free; a build cache there would fill that filesystem. Point buildkitd at a larger partition with --root (or add `root = \"/data/buildkit/root\"` to its active TOML configuration) and restart it, or build elsewhere",
			host, root, humanBytes(free))
	}
	if free < buildkitRootWarnFreeBytes {
		return fmt.Sprintf("%s keeps its BuildKit cache in %s, with %s free; a large image build may exhaust it",
			host, root, humanBytes(free)), nil
	}
	return "", nil
}

// humanBytes renders a byte count at GiB/MiB granularity, which is the scale
// these messages are about.
func humanBytes(n uint64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GiB", float64(n)/float64(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.0f MiB", float64(n)/float64(1<<20))
	default:
		return fmt.Sprintf("%d B", n)
	}
}
