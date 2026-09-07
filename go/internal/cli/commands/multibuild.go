package commands

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
	"github.com/wendylabsinc/wendy/go/internal/stagefile"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

const maxConcurrentBuilds = 4

// serviceBuildFn builds the image for one service. repo is the lowercased
// "<appID>-<service>" name that is both the image repo and the cache key.
type serviceBuildFn func(ctx context.Context, contextDir, repo, dockerfile string, buildOut, logOut io.Writer) error

// multiBuildConcurrency returns the default number of service images to
// build+push at once for a group of numServices.
func multiBuildConcurrency(numServices int) int {
	n := maxConcurrentBuilds
	if n > numServices {
		n = numServices
	}
	if n < 1 {
		n = 1
	}
	return n
}

// resolveBuildConcurrency returns the effective build+push concurrency for
// buildCount services. A positive override (--max-concurrency, WDY-1693) takes
// precedence over the default; either way the result is clamped to
// [1, buildCount].
func resolveBuildConcurrency(buildCount, override int) int {
	if buildCount < 1 {
		return 1
	}
	n := multiBuildConcurrency(buildCount)
	if override > 0 {
		n = override
	}
	if n > buildCount {
		n = buildCount
	}
	if n < 1 {
		n = 1
	}
	return n
}

func resolveServiceSubset(services map[string]*appconfig.ServiceConfig, only string) (map[string]*appconfig.ServiceConfig, error) {
	if only == "" {
		return services, nil
	}

	svc, ok := services[only]
	if !ok {
		return nil, fmt.Errorf("--service %q not found in services map", only)
	}

	subset := map[string]*appconfig.ServiceConfig{only: svc}
	var walk func(name string) error
	walk = func(name string) error {
		svc, ok := services[name]
		if !ok || svc == nil {
			return nil
		}
		for _, dep := range svc.DependsOn {
			if _, seen := subset[dep]; !seen {
				depSvc, ok := services[dep]
				if !ok {
					return fmt.Errorf("service %q depends on unknown service %q", name, dep)
				}
				subset[dep] = depSvc
				if err := walk(dep); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := walk(only); err != nil {
		return nil, err
	}
	return subset, nil
}

// serviceTopoOrder delegates to the shared appconfig package.
func serviceTopoOrder(services map[string]*appconfig.ServiceConfig) ([]string, error) {
	return appconfig.ServiceTopoOrder(services)
}

// buildServiceImage is the default per-service build+prepare step. It shares
// Compose's persistent OCI-layout/chunk path, including its registry fallback,
// so successful builds can report device-verifiable layer diff IDs. It is a
// package var so stress/concurrency tests can substitute a fake builder.
var buildServiceImage = buildAndPrepareComposeImageForAgent

// serviceGPUArch is the GPU architecture every service in a multi-service
// project builds against: one device, so one answer, resolved once for the
// whole group rather than per service.
func serviceGPUArch(ctx context.Context, cwd string, services map[string]*appconfig.ServiceConfig, conn *grpcclient.AgentConnection) string {
	dirs := make([]string, 0, len(services))
	for _, svc := range services {
		dirs = append(dirs, filepath.Join(cwd, svc.Context))
	}
	return resolveGPUArchForDirs(ctx, dirs, "", conn)
}

// planResolveDockerfile is the build-file resolution step used while planning.
// Like buildServiceImage it is a package var so concurrency tests can substitute
// a stub and exercise the parallel scheduling without a real project on disk.
var planResolveDockerfile = resolveDockerfile

// maxConcurrentPlans bounds how many services are planned at once. Planning is
// local work — a build-file resolve (a Stagefile compile, for a Stagefile
// project) plus a full walk-and-hash of the build context. Its higher limit
// keeps planning fast without letting a very large group open every context at
// once.
const maxConcurrentPlans = 8

// servicePlan is the per-service work that has to happen before we can decide
// whether a service's build+push can be skipped: which build file it builds
// from, and the hash of everything that could change its image.
type servicePlan struct {
	dockerfile    string
	inputHash     string
	contentPinned bool
}

// computeServicePlans resolves each service's build file and build-input hash.
//
// Both halves are independent per-service work with no shared state, and both
// are expensive: resolving a build file compiles a Stagefile (parse, registry
// digest resolution, codegen, two file writes) and hashing the build context
// walks and reads every file in it. Running them one service at a time put that
// cost on the critical path before the first build even started, and it scaled
// with the size of the group.
//
// A service whose resolve or hash fails is simply absent from the result — the
// same outcome the serial loop produced by `continue`ing. Callers read a missing
// plan as "don't skip this service, and don't reuse anything for it", so the
// real error surfaces from the build path instead of aborting the whole group
// during planning.
func computeServicePlans(cwd, platform, gpuArch string, appCfg *appconfig.AppConfig, services map[string]*appconfig.ServiceConfig, buildArgs map[string]string, sfOpts ...stagefile.Option) map[string]servicePlan {
	var mu sync.Mutex
	plans := make(map[string]servicePlan, len(services))

	sem := make(chan struct{}, maxConcurrentPlans)
	var wg sync.WaitGroup
	for name, svc := range services {
		wg.Go(func() {
			sem <- struct{}{}
			defer func() { <-sem }()

			contextDir := filepath.Join(cwd, svc.Context)
			dockerfile, err := planResolveDockerfile(contextDir, "", false, gpuArch, sfOpts...)
			if err != nil {
				return
			}
			hash, err := computeBuildInputHash(contextDir, dockerfile, platform, buildArgs, expandServiceEnv(appCfg, svc))
			if err != nil {
				return
			}
			contentPinned, err := dockerfileBasesContentPinned(contextDir, dockerfile)
			if err != nil {
				return
			}

			mu.Lock()
			plans[name] = servicePlan{dockerfile: dockerfile, inputHash: hash, contentPinned: contentPinned}
			mu.Unlock()
		})
	}
	wg.Wait()
	return plans
}

// serviceFingerprintKey namespaces a deploy fingerprint per service within an
// app group, so each service's build inputs are tracked independently.
func serviceFingerprintKey(appID, service string) string {
	return appID + "/svc/" + service
}

func multiServiceWatchHash(buildHash string, cfg *appconfig.AppConfig, env []string, restartPolicy *agentpb.RestartPolicy) (string, error) {
	return watchDesiredHash(struct {
		BuildHash     string
		Config        *appconfig.AppConfig
		Env           []string
		RestartPolicy *agentpb.RestartPolicy
	}{buildHash, cfg, env, restartPolicy})
}

// deviceContainerNames returns the lowercased set of container identities the
// device currently knows about (any running state). ListContainers reports one
// entry per app group whose AppName is the bare app id; per-service identities
// live in its Services list. We record both the bare app id (single-container
// apps) and each "<appId>_<service>" name (multi-service apps), matching
// AppConfig.ContainerName / multiServiceContainerName so callers can look a
// service up directly. Best-effort: on any RPC error it returns an empty set, so
// callers simply don't skip anything.
func deviceContainerStates(ctx context.Context, conn *grpcclient.AgentConnection) map[string]agentpb.AppRunningState {
	states := map[string]agentpb.AppRunningState{}
	stream, err := conn.ContainerService.ListContainers(ctx, &agentpb.ListContainersRequest{})
	if err != nil {
		return states
	}
	for {
		resp, recvErr := stream.Recv()
		if recvErr == io.EOF {
			break
		}
		if recvErr != nil {
			return states
		}
		c := resp.GetContainer()
		if c == nil {
			continue
		}
		app := c.GetAppName()
		states[strings.ToLower(app)] = c.GetRunningState()
		for _, s := range c.GetServices() {
			if s.GetName() != "" {
				states[strings.ToLower(app+"_"+s.GetName())] = s.GetRunningState()
			}
		}
	}
	return states
}

func deviceContainerNames(ctx context.Context, conn *grpcclient.AgentConnection) map[string]bool {
	present := map[string]bool{}
	for name := range deviceContainerStates(ctx, conn) {
		present[name] = true
	}
	return present
}

// planServicePushSkips decides, per service, whether its build+push can be
// skipped. A skip is only permitted when ALL of the following hold: the build
// inputs are unchanged since the last successful push to this device, the
// device still has that service's container, AND we can confirm the device
// still holds the exact image content we pushed (contentPresent). It returns
// the skip set and the freshly computed per-service input hashes, so the caller
// can persist fingerprints for the services it actually builds.
//
// The content check is the fix for WDY-1824: an unchanged input hash and a
// present container do not prove the device still has the pushed image content
// (blobs can be GC'd, a push can half-complete, or a rebuilt local base image
// never changes the input hash), so skipping on those alone could leave the
// device running a stale/partial image while the CLI reports success.
//
// Best-effort throughout: any error for a service (or WENDY_PUSH_SKIP=0) just
// means "don't skip it".
//
// The expensive half — resolving each service's build file and hashing its build
// context — runs concurrently in computeServicePlans; only the cheap decisions
// that consult the device happen here. The resolved build files are returned
// alongside so the build path can reuse them instead of resolving (and, for a
// Stagefile, recompiling) every service a second time in the same run.
func planServicePushSkips(ctx context.Context, conn *grpcclient.AgentConnection, cwd, appID, deviceKey, platform string, appCfg *appconfig.AppConfig, services map[string]*appconfig.ServiceConfig, buildArgs map[string]string, sfOpts ...stagefile.Option) (skip map[string]bool, hashes, dockerfiles map[string]string) {
	skip = map[string]bool{}
	hashes = map[string]string{}
	dockerfiles = map[string]string{}
	if os.Getenv("WENDY_PUSH_SKIP") == "0" {
		return skip, hashes, dockerfiles
	}

	plans := computeServicePlans(cwd, platform, serviceGPUArch(ctx, cwd, services, conn), appCfg, services, buildArgs, sfOpts...)
	present := deviceContainerNames(ctx, conn)
	type candidate struct {
		name string
		fp   *deployFingerprint
	}
	var candidates []candidate
	allDiffIDs := map[string]struct{}{}
	for name := range services {
		plan, planned := plans[name]
		if !planned {
			continue
		}
		hashes[name] = plan.inputHash
		dockerfiles[name] = plan.dockerfile
		if !plan.contentPinned {
			// A mutable FROM tag must be resolved by a normal build. Presence of
			// the last deployment's layers is not proof that tag is unchanged.
			delete(hashes, name)
			continue
		}

		fp, ok := loadDeployFingerprint(serviceFingerprintKey(appID, name), deviceKey)
		if !ok || fp.InputHash != plan.inputHash {
			continue
		}
		if !present[strings.ToLower(multiServiceContainerName(appID, name))] {
			continue
		}
		if len(fp.LayerDiffIDs) == 0 {
			continue
		}
		candidates = append(candidates, candidate{name: name, fp: fp})
		for _, id := range fp.LayerDiffIDs {
			allDiffIDs[id] = struct{}{}
		}
	}

	// Container presence is not enough: confirm the device still holds the
	// exact content we prepared. Query every candidate service in one RPC to
	// avoid making push-skip latency scale with the service count.
	ids := make([]string, 0, len(allDiffIDs))
	for id := range allDiffIDs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	confirmed, ok := devicePresentLayers(ctx, conn, ids)
	if !ok {
		return skip, hashes, dockerfiles
	}
	for _, candidate := range candidates {
		allPresent := true
		for _, id := range candidate.fp.LayerDiffIDs {
			if !confirmed[id] {
				allPresent = false
				break
			}
		}
		if allPresent {
			skip[candidate.name] = true
		}
	}
	return skip, hashes, dockerfiles
}

// contentPresentForService reports whether the device is confirmed to still hold
// the image content recorded for a service, gating a push-skip
// (WDY-1824).
//
// It is fail-closed. The default chunk-preparation path records diff IDs that
// QueryLayers can verify. Registry-only builds do not expose an equivalent
// device-verifiable identity yet, so their fingerprints omit diff IDs and this
// returns false.
func contentPresentForService(ctx context.Context, conn *grpcclient.AgentConnection, fp *deployFingerprint) bool {
	if fp == nil {
		return false
	}
	return deviceHasAllLayers(ctx, conn, fp.LayerDiffIDs)
}

// runMultiServiceWithAgent orchestrates the full build → push → create →
// stream pipeline for a multi-service wendy.json on a single agent.
func runMultiServiceWithAgent(ctx context.Context, conn *grpcclient.AgentConnection, cwd string, appCfg *appconfig.AppConfig, opts runOptions) error {
	services, err := resolveServiceSubset(appCfg.Services, opts.service)
	if err != nil {
		return err
	}
	portConfigs := []*appconfig.AppConfig{appCfg}
	for name, svc := range services {
		portConfigs = append(portConfigs, multiServiceCreateConfig(appCfg, name, svc))
	}
	if err := prepareVMAppPorts(ctx, conn, portConfigs...); err != nil {
		return err
	}

	versionResp, err := agentVersionForRun(ctx, conn)
	if err != nil {
		return fmt.Errorf("querying device version: %w", err)
	}
	printRunDiskUsageWarning(versionResp)
	agentOS := versionResp.GetOs()
	architecture := versionResp.GetCpuArchitecture()
	if architecture == "" {
		cliLogln("Warning: agent did not report CPU architecture; assuming arm64.")
		architecture = "arm64"
	}
	platform := resolveAgentPlatform(appCfg.Platform, agentOS, architecture)
	if strings.EqualFold(agentOS, appconfig.PlatformDarwin) {
		if err := rejectUnsupportedMacRunProject("multi-service", platform); err != nil {
			return err
		}
	}

	if err := requireRegistryAuth(ctx, conn); err != nil {
		return err
	}

	regPort := registryPort(agentOS)

	buildArgs := map[string]string{
		"WENDY_PLATFORM": wendyPlatform(versionResp.GetDeviceType()),
	}
	if opts.debug {
		cliLogln("Warning: building with WENDY_DEBUG=true — do not deploy to production.")
		buildArgs["WENDY_DEBUG"] = "true"
	}
	applyDeviceBuildArgHints(buildArgs, versionResp)

	// Ensure the Apple Container system is up once, before the parallel builds,
	// so an explicit --builder apple-container prompts/starts a single time
	// rather than racing across service goroutines.
	if err := ensureAppleContainerSystemForBuilder(ctx, opts.builder, opts.yes); err != nil {
		return err
	}

	// Decide which services can skip build+push entirely: those whose build
	// inputs are unchanged since the last successful push to this device, whose
	// container is still present, AND whose image content the device is confirmed
	// to still hold. This is the WDY-1692 optimization (avoid re-pushing unchanged
	// images — notably the multi-GB GPU base — and the HEAD-check storm re-push
	// triggers), tightened for WDY-1824 so it never skips onto stale/partial
	// content. The default builder now uses the same chunk-preparation path as
	// Compose and records its layer diff IDs; registry-only builds continue to
	// fail closed because they do not provide a verifiable content identity.
	deviceKey := deviceFingerprintKey(versionResp)
	sfOpts := debugStagefileOptions(opts.debug)
	skip, hashes, dockerfiles := planServicePushSkips(ctx, conn, cwd, appCfg.AppID, deviceKey, platform, appCfg, services, buildArgs, sfOpts...)

	// Build the full per-service create configs before selecting watch work: a
	// service is unchanged only when both its image inputs and its effective
	// runtime configuration match the last successful cycle.
	svcCfgs := make(map[string]*appconfig.AppConfig, len(services))
	svcLifecycleCfgs := make(map[string]*appconfig.AppConfig, len(services))
	for name, svc := range services {
		svcCfgs[name] = multiServiceCreateConfig(appCfg, name, svc)
		svcLifecycleCfgs[name] = multiServiceLifecycleConfig(appCfg.AppID, name, svc)
	}

	preserve := map[string]bool{}
	desiredHashes := map[string]string{}
	if opts.watchState != nil {
		states := deviceContainerStates(ctx, conn)
		candidates := map[string]watchServiceCandidate{}
		for name, svc := range services {
			buildHash, planned := hashes[name]
			if !planned {
				continue
			}
			desiredHash, err := multiServiceWatchHash(buildHash, svcCfgs[name], expandServiceEnv(appCfg, svc), resolveRestartPolicy(opts))
			if err != nil {
				continue
			}
			desiredHashes[name] = desiredHash
			candidates[name] = watchServiceCandidate{
				appID:         appCfg.AppID,
				containerName: multiServiceContainerName(appCfg.AppID, name),
				desiredHash:   desiredHash,
			}
		}
		preserve = selectPreservedWatchServices(opts.watchState, deviceKey, candidates, states)
		for name := range preserve {
			skip[name] = true
		}
		if n := len(preserve); n > 0 {
			cliLogln("%d of %d services unchanged and running; leaving them untouched.", n, len(services))
		}
	}
	if n := len(skip); n > 0 {
		if opts.watchState == nil {
			cliLogln("%d of %d services unchanged and already on device; skipping their build/push.", n, len(services))
		}
	}

	// Build all service images in parallel, then create and start containers.
	failed, preparedContent, buildErr := buildServicesParallelWithContent(ctx, conn, regPort, agentOS, cwd, appCfg.AppID, services, platform, buildArgs, opts.builder, opts.chunking, skip, dockerfiles, opts.maxConcurrency, opts.quietBuild, sfOpts...)
	if buildErr != nil {
		return buildErr
	}

	recordServiceDeployFingerprints(appCfg.AppID, appCfg.Version, deviceKey, services, skip, failed, hashes, preparedContent)

	// Default (all-or-nothing): any build/push failure aborts the whole group so
	// no half-deployed group is left behind. --keep-going deploys what built and
	// reports the rest (WDY-1691).
	if len(failed) > 0 && !opts.keepGoing {
		return joinServiceErrors(failed)
	}

	// Determine which services can actually be deployed: those that built and
	// whose dependencies all built too. partialErr is surfaced at the end so the
	// command still exits non-zero after deploying the healthy subset.
	deployServices := services
	var partialErr error
	if len(failed) > 0 {
		deployable, dropped := resolveDeployableServices(services, failed)
		deployServices = deployable
		cliNotice("Partial deploy: %d deploying, %d failed (%s)%s.",
			len(deployable), len(failed), strings.Join(sortedServiceErrorKeys(failed), ", "),
			droppedSummary(dropped))
		partialErr = joinServiceErrors(failed)
		if len(deployable) == 0 {
			cliNotice("No services left to deploy.")
			return partialErr
		}
	}

	// Create (and start) containers in dependency order.
	ordered, err := serviceTopoOrder(deployServices)
	if err != nil {
		return err
	}
	adjustedPreserve := adjustSharedNamespacePreserve(ordered, preserve, appCfg.Isolation)
	if len(adjustedPreserve) < len(preserve) {
		cliLogln("Shared-namespace primary changed; restarting the affected service group.")
	}
	preserve = adjustedPreserve
	preservedLifecycle := preservedServicesInOrder(ordered, preserve)
	if len(preserve) > 0 {
		ordered = filterPreservedServices(ordered, preserve)
		if len(ordered) == 0 {
			cliLogln("All services are unchanged and running; nothing to redeploy.")
			if opts.detach || opts.deploy {
				return partialErr
			}
		}
	}

	// The app-level fallback fires the group's top-level readiness/hooks once,
	// after the selected services have started. A partial watch deploy may still
	// run it: every omitted service was independently confirmed unchanged and
	// running, and the session lease suppresses it after the first success.
	// An explicit --service run cannot make that whole-app guarantee.
	appLevelCfg := appLevelLifecycleConfig(appCfg.AppID, appCfg)
	if opts.service != "" {
		appLevelCfg = nil
	}

	createService := func(name string) error {
		svc := services[name]
		deviceImage := fmt.Sprintf("localhost:%d/%s-%s:latest", regPort,
			strings.ToLower(appCfg.AppID), strings.ToLower(name))

		serviceCfg := svcCfgs[name]
		appConfigData, err := json.Marshal(serviceCfg)
		if err != nil {
			return fmt.Errorf("marshaling config for service %s: %w", name, err)
		}

		restartPolicy := resolveRestartPolicy(opts)
		createReq := &agentpb.CreateContainerRequest{
			ImageName:     deviceImage,
			AppName:       serviceCfg.ContainerName(),
			AppConfig:     appConfigData,
			RestartPolicy: restartPolicy,
			Env:           expandServiceEnv(appCfg, svc),
		}

		cliLogln("Creating container for service %s...", name)
		if err := createContainerWithProgress(ctx, conn.ContainerService, createReq); err != nil {
			return fmt.Errorf("creating container for service %s: %w", name, err)
		}
		cliLogln("Service %s container created.", name)
		return nil
	}

	if opts.deploy {
		// Create-only: no service ever starts, so shared-namespace groups
		// cannot join here — the join happens at create time against the
		// primary's running task. Such groups should be deployed without
		// --deploy (or started service-by-service in dependency order).
		// postStart hooks and readiness gating are start-time concerns, so they
		// are deliberately never fired on this path.
		for _, name := range ordered {
			if err := createService(name); err != nil {
				return err
			}
		}
		cliLogln("App group %s created (not started, --deploy).", appCfg.AppID)
		return partialErr
	}

	// Create and start each service in dependency order, multiplexing log
	// output with per-service prefixes. Interleaving create and start is
	// load-bearing for shared-ipc/shared-network groups: a secondary's
	// namespace join is resolved at container create time against the
	// primary's running task, so the primary must be started before the
	// next service is created.
	if err := startAndStreamServices(ctx, conn, appCfg.AppID, ordered, preservedLifecycle, opts, createService, svcCfgs, svcLifecycleCfgs, appLevelCfg); err != nil {
		return err
	}
	if ctx.Err() == nil {
		for _, name := range ordered {
			if h := desiredHashes[name]; h != "" {
				opts.watchState.record(watchServiceKey(deviceKey, appCfg.AppID, name), h)
			}
		}
	}
	// In --keep-going mode, exit non-zero after deploying the healthy subset so
	// callers/CI still see that some services failed.
	return partialErr
}

// recordServiceDeployFingerprints persists only successful build/preparations.
// Skipped services retain their already-matching fingerprint; failed services
// must never record content that may only have been partially transferred.
func recordServiceDeployFingerprints(appID, appVersion, deviceKey string, services map[string]*appconfig.ServiceConfig, skip map[string]bool, failed map[string]error, hashes map[string]string, preparedContent map[string][]string) {
	for name := range services {
		if skip[name] || failed[name] != nil {
			continue
		}
		// Registry-only/fallback builds do not surface device-verifiable diff
		// IDs. Leave any existing verifiable fingerprint intact instead of
		// replacing it with one that can never authorize a content check.
		if h, ok := hashes[name]; ok && len(preparedContent[name]) > 0 {
			saveDeployFingerprint(serviceFingerprintKey(appID, name), deviceKey, deployFingerprint{
				InputHash:    h,
				AppVersion:   appVersion,
				LayerDiffIDs: preparedContent[name],
			})
		}
	}
}

// droppedSummary formats the services skipped because a dependency failed, for
// the partial-deploy notice. Returns "" when nothing was dropped.
func droppedSummary(dropped map[string]string) string {
	if len(dropped) == 0 {
		return ""
	}
	names := make([]string, 0, len(dropped))
	for n := range dropped {
		names = append(names, n)
	}
	sort.Strings(names)
	return fmt.Sprintf(", %d skipped (failed dependency: %s)", len(names), strings.Join(names, ", "))
}

// buildServicesParallelCore builds all service images concurrently (up to
// maxConcurrentBuilds at a time), invoking build for each one. Services in skip
// are already on the device with unchanged inputs, so their build+push is
// skipped (WDY-1692). Progress is shown via a Bubbletea multi-spinner in
// interactive terminals and via plain log lines otherwise.
//
// dockerfiles carries the build file planning already resolved per service, so a
// Stagefile project is not compiled twice in the same run. It is best-effort:
// planning bails out entirely under WENDY_PUSH_SKIP=0 and drops any service it
// could not plan, so a service missing from the map resolves its own build file
// here — which is also where that resolution's error belongs, since this is the
// path that reports per-service failures.
func buildServicesParallelCore(
	ctx context.Context,
	build serviceBuildFn,
	cwd, appID string,
	services map[string]*appconfig.ServiceConfig,
	gpuArch string,
	skip map[string]bool,
	dockerfiles map[string]string,
	maxConcurrency int,
	quietBuild bool,
	sfOpts ...stagefile.Option,
) (map[string]error, error) {
	buildCtx, cancelBuild := context.WithCancel(ctx)
	defer cancelBuild()

	names := make([]string, 0, len(services))
	for n := range services {
		names = append(names, n)
	}
	sort.Strings(names)

	type result struct {
		name string
		err  error
		dur  time.Duration
		log  string
	}

	results := make(chan result, len(names))

	// Concurrency (and the tunnel pressure it controls) is driven by the services
	// actually built — skipped ones push nothing.
	buildCount := 0
	for _, n := range names {
		if !skip[n] {
			buildCount++
		}
	}
	concurrency := resolveBuildConcurrency(buildCount, maxConcurrency)
	if !quietBuild && maxConcurrency > 0 && concurrency < buildCount {
		cliLogln("Building up to %d service(s) at a time (--max-concurrency).", concurrency)
	}
	sem := make(chan struct{}, concurrency)

	var prog *tea.Program
	if !quietBuild && isInteractiveTerminal() {
		title := fmt.Sprintf("Building %d service(s)...", len(names))
		m := tui.NewMultiSpinner(title, names)
		prog = tui.NewProgressProgram(m)
	}

	var wg sync.WaitGroup
	for _, name := range names {
		wg.Add(1)
		go func(name string, svc *appconfig.ServiceConfig) {
			defer wg.Done()

			// Unchanged service already on the device: skip build+push entirely.
			if skip[name] {
				if prog != nil {
					prog.Send(tui.MultiSpinnerStartMsg{Name: name})
					prog.Send(tui.MultiSpinnerDoneMsg{Name: name, Err: nil, Dur: 0})
				} else if !quietBuild {
					cliLogln("Service %s unchanged; skipping build/push (already on device).", name)
				}
				results <- result{name: name}
				return
			}

			select {
			case sem <- struct{}{}:
			case <-buildCtx.Done():
				results <- result{name: name, err: buildCtx.Err()}
				return
			}
			defer func() { <-sem }()
			if err := buildCtx.Err(); err != nil {
				results <- result{name: name, err: err}
				return
			}

			if prog != nil {
				prog.Send(tui.MultiSpinnerStartMsg{Name: name})
			} else if !quietBuild {
				cliLogln("Building service %s...", name)
			}

			start := time.Now()
			contextDir := filepath.Join(cwd, svc.Context)
			repo := fmt.Sprintf("%s-%s", strings.ToLower(appID), strings.ToLower(name))
			dockerfile, planned := dockerfiles[name]
			var dockerfileErr error
			if !planned {
				dockerfile, dockerfileErr = planResolveDockerfile(contextDir, "", false, gpuArch, sfOpts...)
			}

			var buildOut io.Writer
			logBuf := boundedBuffer{max: maxRawBuildCapture}
			var tally func() tui.BuildTally = func() tui.BuildTally { return tui.BuildTally{} }
			if prog != nil {
				// Parse this service's stream into per-row detail updates and
				// cache/rebuild tallies. Raw output is still buffered for the
				// on-failure dump.
				emit, getTally := newServiceProgressEmitter(prog, name)
				tally = getTally
				parser := tui.NewBuildParser(emit)
				buildOut = io.MultiWriter(parser, &logBuf)
			} else if quietBuild {
				buildOut = &logBuf
			} else {
				buildOut = os.Stdout
			}
			var logOutW io.Writer = &logBuf
			if prog == nil && !quietBuild {
				logOutW = os.Stderr
			}
			err := dockerfileErr
			if err == nil {
				// Pass the per-service repo as the build's cache key so each concurrent
				// build gets its own isolated local buildx cache dir (WDY-1689); sharing
				// one dir corrupts BuildKit's cache-export ingest store under concurrency.
				err = build(buildCtx, contextDir, repo, dockerfile, buildOut, logOutW)
			}
			dur := time.Since(start)

			if prog != nil {
				t := tally()
				prog.Send(tui.MultiSpinnerDoneMsg{Name: name, Err: err, Dur: dur, Cached: t.Cached, Rebuilt: t.Rebuilt})
			} else if err != nil {
				if buildCtx.Err() == nil {
					if !quietBuild {
						cliLogln("Service %s build failed: %v", name, err)
					}
				}
			} else if !quietBuild {
				cliLogln("Service %s built (%s).", name, dur.Round(time.Millisecond))
			}

			results <- result{name: name, err: err, dur: dur, log: logBuf.String()}
		}(name, services[name])
	}

	// Wait for all goroutines, close the results channel, then signal TUI done.
	go func() {
		wg.Wait()
		close(results)
		if prog != nil {
			prog.Send(tui.MultiSpinnerAllDoneMsg{})
		}
	}()

	var progressErr error
	if prog != nil {
		final, runErr := prog.Run()
		if runErr != nil {
			cancelBuild()
			progressErr = fmt.Errorf("build progress TUI: %w", runErr)
		} else if fm, ok := final.(tui.MultiSpinnerModel); !ok {
			cancelBuild()
			progressErr = fmt.Errorf("build progress TUI: unexpected final model %T", final)
		} else if errors.Is(fm.Err(), tui.ErrCancelled) {
			// One Ctrl-C owns cancellation for the entire service group. Queued
			// services never start, and all active builder commands share this
			// context so they unwind before we return.
			cancelBuild()
			progressErr = ErrUserCancelled
		}
	}

	// Collect per-service failures. For failed services, summarize their buffered
	// output now that the spinner has exited and the terminal is clean, retaining
	// the full raw log in a temporary file for deeper inspection. The caller
	// decides whether any failure aborts the group (default) or only its own
	// service is dropped (--keep-going, WDY-1691).
	failed := map[string]error{}
	for r := range results {
		if r.err != nil {
			failed[r.name] = r.err
			// Skip rendering after UI/context cancellation and for the friendly
			// "no registry on the Mac agent" error, where retried-push spam would
			// bury the actionable message.
			if progressErr == nil && buildCtx.Err() == nil && r.log != "" && !isRegistryUnavailable(r.err) {
				renderBuildFailure(os.Stderr, r.name, r.log, r.err)
			}
		}
	}
	if progressErr != nil {
		return nil, progressErr
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return failed, nil
}

// buildServicesParallel is retained as the test/build scheduling seam. Normal
// multi-service runs use buildServicesParallelWithContent so successful chunk
// preparations can persist their exact image identities.
func buildServicesParallel(
	ctx context.Context,
	conn *grpcclient.AgentConnection,
	regPort int,
	agentOS string,
	cwd, appID string,
	services map[string]*appconfig.ServiceConfig,
	platform string,
	buildArgs map[string]string,
	builder string,
	skip map[string]bool,
	dockerfiles map[string]string,
	maxConcurrency int,
	quietBuild bool,
	sfOpts ...stagefile.Option,
) (map[string]error, error) {
	failed, _, err := buildServicesParallelWithContent(ctx, conn, regPort, agentOS, cwd, appID, services, platform, buildArgs, builder, chunkingAuto, skip, dockerfiles, maxConcurrency, quietBuild, sfOpts...)
	return failed, err
}

// buildServicesParallelWithContent builds each service using the same
// OCI-layout/chunk preparation path as Compose and returns the uncompressed
// layer identities reported by successful preparations. --chunking=off keeps
// the registry-only path; because it cannot currently report a device-
// verifiable identity, its content result is deliberately empty and subsequent
// skip checks fail closed.
func buildServicesParallelWithContent(
	ctx context.Context,
	conn *grpcclient.AgentConnection,
	regPort int,
	agentOS string,
	cwd, appID string,
	services map[string]*appconfig.ServiceConfig,
	platform string,
	buildArgs map[string]string,
	builder, chunking string,
	skip map[string]bool,
	dockerfiles map[string]string,
	maxConcurrency int,
	quietBuild bool,
	sfOpts ...stagefile.Option,
) (map[string]error, map[string][]string, error) {
	// Resolved once for the group: every service deploys to this one device,
	// so they share its GPU architecture.
	gpuArch := serviceGPUArch(ctx, cwd, services, conn)

	content := map[string][]string{}
	var contentMu sync.Mutex
	build := func(ctx context.Context, contextDir, repo, dockerfile string, buildOut, logOut io.Writer) error {
		buildImage := buildServiceImage
		if chunking == chunkingOff {
			buildImage = buildAndPushImageForAgent
		} else if chunking == chunkingForce {
			buildImage = buildAndPrepareComposeImageForAgentForce
		}
		reportingOut := &imageBuildProgressWriter{
			Writer: buildOut,
			reportContent: func(ids []string) {
				contentMu.Lock()
				content[repo] = ids
				contentMu.Unlock()
			},
		}
		return buildImage(ctx, conn, regPort, agentOS, builder, contextDir, repo, platform, dockerfile, buildArgs, repo, reportingOut, logOut)
	}

	failed, err := buildServicesParallelCore(withComposePrepareLimiter(ctx), build, cwd, appID, services, gpuArch, skip, dockerfiles, maxConcurrency, quietBuild, sfOpts...)
	if err != nil {
		return failed, nil, err
	}
	byService := make(map[string][]string, len(content))
	for name := range services {
		if failed[name] != nil || skip[name] {
			continue
		}
		repo := fmt.Sprintf("%s-%s", strings.ToLower(appID), strings.ToLower(name))
		if ids := content[repo]; len(ids) > 0 {
			byService[name] = append([]string(nil), ids...)
		}
	}
	return failed, byService, nil
}

// buildServicesLocal builds every service image into the local image store
// (no device, no registry push) — the `wendy build` flavor over
// buildServicesParallelCore.
func buildServicesLocal(ctx context.Context, cwd, appID string, services map[string]*appconfig.ServiceConfig,
	platform, builder, gpuArch string, maxConcurrency int, quietBuild bool,
	sfOpts ...stagefile.Option) (map[string]error, error) {
	build := func(ctx context.Context, contextDir, repo, dockerfile string, buildOut, logOut io.Writer) error {
		return buildLocalServiceImage(ctx, builder, contextDir, repo+":latest", platform, dockerfile, buildOut, logOut)
	}
	// skip=nil: `wendy build` builds every selected service every invocation —
	// push-skip fingerprinting is a device-deploy optimization and does not
	// apply here. dockerfiles=nil: the core's per-service planResolveDockerfile
	// fallback resolves each context, the same non-interactive
	// Stagefile>Dockerfile rules `wendy run` uses.
	return buildServicesParallelCore(ctx, build, cwd, appID, services, gpuArch, nil, nil, maxConcurrency, quietBuild, sfOpts...)
}

// sortedServiceErrorKeys returns the service names in failed, sorted, for stable
// error/report output.
func sortedServiceErrorKeys(failed map[string]error) []string {
	names := make([]string, 0, len(failed))
	for n := range failed {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// joinServiceErrors builds a single error from a per-service failure map, in
// stable order.
func joinServiceErrors(failed map[string]error) error {
	var errs []error
	for _, n := range sortedServiceErrorKeys(failed) {
		errs = append(errs, fmt.Errorf("service %s: %w", n, failed[n]))
	}
	return errors.Join(errs...)
}

// resolveDeployableServices returns the subset of services that can be deployed:
// those that built successfully and whose (transitive) dependencies all built
// successfully too. dropped maps each service that is skipped *because of a failed
// dependency* (not its own failure) to a human-readable reason. Failed services
// are reported separately via the failed map. A dependency cycle is treated as
// not deployable (serviceTopoOrder reports the cycle itself).
func resolveDeployableServices(services map[string]*appconfig.ServiceConfig, failed map[string]error) (deployable map[string]*appconfig.ServiceConfig, dropped map[string]string) {
	deployable = map[string]*appconfig.ServiceConfig{}
	dropped = map[string]string{}

	const (
		unknown  = 0
		yes      = 1
		no       = 2
		visiting = 3
	)
	state := map[string]int{}

	var canDeploy func(name string) bool
	canDeploy = func(name string) bool {
		switch state[name] {
		case yes:
			return true
		case no, visiting:
			return false
		}
		state[name] = visiting
		svc, ok := services[name]
		if !ok || svc == nil || failed[name] != nil {
			state[name] = no
			return false
		}
		for _, dep := range svc.DependsOn {
			if !canDeploy(dep) {
				state[name] = no
				dropped[name] = fmt.Sprintf("dependency %q was not deployed", dep)
				return false
			}
		}
		state[name] = yes
		return true
	}

	for name := range services {
		if canDeploy(name) {
			deployable[name] = services[name]
		}
	}
	return deployable, dropped
}

var serviceLogStyle = lipgloss.NewStyle().Foreground(tui.ColorInfo)

// newServiceProgressEmitter returns an emit callback for tui.NewBuildParser that
// forwards the active step as a MultiSpinner detail line and accumulates the
// cached/rebuilt tally for the service's done row.
func newServiceProgressEmitter(prog *tea.Program, name string) (func(tui.BuildStepEvent), func() tui.BuildTally) {
	var t tui.BuildTally
	emit := func(e tui.BuildStepEvent) {
		switch e.Status {
		case tui.BuildStepRunning:
			// Prefer the step's live progress ("[525/1027] 51% Compiling …",
			// "61% 128MB/210MB 9.4MB/s") over its bare label: with one row per
			// service that sub-line is the only place a user sees movement.
			detail := e.Display
			if p := tui.ProgressDetail(e); p != "" {
				detail = e.Display + " · " + p
			}
			prog.Send(tui.MultiSpinnerDetailMsg{Name: name, Detail: detail})
		case tui.BuildStepCached:
			if e.Kind == tui.BuildVertexStep {
				t.Cached++
			}
		case tui.BuildStepDone:
			if e.Kind == tui.BuildVertexStep {
				t.Rebuilt++
			}
		}
	}
	return emit, func() tui.BuildTally { return t }
}

// multiServiceCreateConfig builds the per-service AppConfig transmitted to
// the agent for a standalone multi-service app. Each service carries the group
// identity and runtime context used for namespace sharing, ROS 2 environment
// injection, and container naming.
//
// Only the service's OWN readiness/hooks are copied. The group's top-level
// appCfg.Readiness/.Hooks are deliberately not copied here: those are the
// app-level fallback that fires once after every service has started (see
// appLevelLifecycleConfig / startAndStreamServices), so copying them onto each
// per-service config would trigger hooks.postStart.agent for every container.
func multiServiceCreateConfig(appCfg *appconfig.AppConfig, name string, svc *appconfig.ServiceConfig) *appconfig.AppConfig {
	cfg := &appconfig.AppConfig{
		AppID:       appCfg.AppID,
		ServiceName: name,
		Version:     appCfg.Version,
		Platform:    appCfg.Platform,
		Isolation:   appCfg.Isolation,
		Frameworks:  appCfg.Frameworks,
		Readiness:   svc.Readiness,
		Hooks:       svc.Hooks,
	}
	cfg.Entitlements = append(append([]appconfig.Entitlement{}, appCfg.Entitlements...), svc.Entitlements...)
	cfg.Entitlements = deduplicateEntitlements(cfg.Entitlements)
	if svc.Frameworks != nil {
		cfg.Frameworks = svc.Frameworks
	}
	// The per-service config carries no services map, so the agent's
	// ResolveResourcesForService has nothing to merge — resolve the app-level
	// default against this service's override here instead. Without this the
	// container is created with no limits at all (WDY-1729).
	cfg.Resources = appCfg.ResolveResourcesForService(name)
	return cfg
}

// multiServiceLifecycleConfig builds the CLI-private lifecycle view for one
// services-map entry. Unlike multiServiceCreateConfig, it intentionally sees
// only HTTP entitlements declared by the service itself; inherited top-level
// HTTP belongs to the app-level lifecycle config and must run only once.
func multiServiceLifecycleConfig(appID, name string, svc *appconfig.ServiceConfig) *appconfig.AppConfig {
	if svc == nil {
		return nil
	}
	return lifecycleConfig(appID, name, svc.Readiness, svc.Hooks, svc.Entitlements)
}

// multiServiceContainerName returns the container name the agent derives for
// a service: "{appId}_{serviceName}" (WDY-878). Start/stop calls must address
// the same name the create path produced.
func multiServiceContainerName(appID, serviceName string) string {
	return appID + "_" + serviceName
}

// startAndStreamServices starts all service containers. Attached runs stream
// their combined output with a "[serviceName] " prefix. Watch cycles return
// after Started because the session log follower owns their output.
// createService is invoked for each service, in dependency order, immediately
// before that service is started — after every earlier service is already
// running. This ordering is required for shared-ipc/shared-network groups:
// the agent resolves a secondary's namespace join at container create time
// against the primary's running task.
//
// preservedLifecycle names unchanged running services whose host lifecycle may
// still be pending from a canceled or failed earlier watch cycle. They are not
// restarted; the watch-session lease makes completed lifecycle work a no-op.
// svcCfgs supplies the full create/agent-hook config for each service, while
// svcLifecycleCfgs supplies the private CLI lifecycle view whose entitlements
// contain only service-declared HTTP. appLevelCfg, when non-nil, is the
// group-level fallback fired once after every service has started. Hook work
// does not block the sequential create→start→Started loop, which allows a
// dependent service to join an already-running service's namespaces.
func startAndStreamServices(ctx context.Context, conn *grpcclient.AgentConnection, appID string, ordered, preservedLifecycle []string, opts runOptions, createService func(name string) error, svcCfgs, svcLifecycleCfgs map[string]*appconfig.AppConfig, appLevelCfg *appconfig.AppConfig) error {
	runCtx, runCancel := context.WithCancel(ctx)
	defer runCancel()

	runner := &serviceHookRunner{conn: conn, opts: opts}

	// Ctrl+C stops all services. The watch loop owns the signal for the whole
	// session and leaves the group running when it stops, so a watch cycle must
	// not install its own handler.
	sigCh := make(chan os.Signal, 1)
	if !opts.isWatch() {
		signal.Notify(sigCh, os.Interrupt)
		defer signal.Stop(sigCh)
	}
	go func() {
		select {
		case <-sigCh:
		case <-runCtx.Done():
			return
		}
		cliLogln("\nStopping services...")
		runCancel()
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer stopCancel()
		var stopWg sync.WaitGroup
		for _, name := range ordered {
			stopWg.Add(1)
			go func(name string) {
				defer stopWg.Done()
				_, _ = conn.ContainerService.StopContainer(stopCtx, &agentpb.StopContainerRequest{
					AppName: multiServiceContainerName(appID, name),
				})
			}(name)
		}
		stopWg.Wait()
	}()

	if opts.detach {
		for _, name := range ordered {
			if err := createService(name); err != nil {
				return err
			}
			containerName := multiServiceContainerName(appID, name)
			stream, err := conn.ContainerService.StartContainer(contextWithPostStartAgentHook(runCtx, svcCfgs[name]), &agentpb.StartContainerRequest{
				AppName: containerName,
			})
			if err != nil {
				return fmt.Errorf("starting service %s: %w", name, err)
			}
			if _, err := stream.Recv(); err != nil && err != io.EOF {
				return fmt.Errorf("waiting for service %s to start: %w", name, err)
			}
		}
		cliLogln("App group %s running in detached mode.", appID)
		// No host-side lifecycle work: detached runs do not wait for readiness,
		// announce the app URL, or fire host postStart hooks — see
		// runPostStartIfReady's doc comment (WDY-2041). The agent-side hooks
		// attached to the start RPCs above still run on the device.
		return nil
	}

	if opts.isWatch() {
		// The session log follower owns output. Each cycle creates and starts only
		// changed services, waits for Started, runs eligible host lifecycle work,
		// and returns.
		for _, name := range ordered {
			if err := createService(name); err != nil {
				return err
			}
			startCtx, startCancel := context.WithCancel(runCtx)
			stream, err := conn.ContainerService.StartContainer(contextWithPostStartAgentHook(startCtx, svcCfgs[name]), &agentpb.StartContainerRequest{
				AppName: multiServiceContainerName(appID, name),
			})
			if err != nil {
				startCancel()
				return fmt.Errorf("starting service %s: %w", name, err)
			}
			if err := awaitStarted(stream); err != nil {
				startCancel()
				return fmt.Errorf("waiting for service %s to start: %w", name, err)
			}
			startCancel()
			runner.startAsync(runCtx, svcLifecycleCfgs[name])
		}
		for _, name := range preservedLifecycle {
			runner.startAsync(runCtx, svcLifecycleCfgs[name])
		}
		runner.startAsync(runCtx, appLevelCfg)
		if len(ordered) > 0 {
			cliLogln("App group %s started (%d services).", appID, len(ordered))
		}
		runner.reap()
		return ctx.Err()
	}

	type logLine struct {
		service string
		stdout  bool
		data    []byte
	}
	lines := make(chan logLine, 256)

	// Create and start sequentially in dependency order; the first Recv
	// blocks until the agent's Started ack, guaranteeing each service's task
	// is running before the next service's container is created.
	// Every early-error return mirrors the happy-path teardown: cancel first
	// (aborts in-flight hook waits, kills tracked cli-hook children), then wait
	// out the log goroutines and reap hooks already startAsync'd for earlier
	// services, so nothing outlives this call.
	var wg sync.WaitGroup
	for _, name := range ordered {
		if err := createService(name); err != nil {
			runCancel()
			wg.Wait()
			runner.reap()
			return err
		}
		containerName := multiServiceContainerName(appID, name)
		stream, err := conn.ContainerService.StartContainer(contextWithPostStartAgentHook(runCtx, svcCfgs[name]), &agentpb.StartContainerRequest{
			AppName: containerName,
		})
		if err != nil {
			runCancel()
			wg.Wait()
			runner.reap()
			return fmt.Errorf("starting service %s: %w", name, err)
		}
		if _, err := stream.Recv(); err != nil && err != io.EOF {
			runCancel()
			wg.Wait()
			runner.reap()
			return fmt.Errorf("waiting for service %s to start: %w", name, err)
		}
		// This service's task is running (Started ack received). Fire its
		// readiness→announce→postStart sequence on a goroutine so a slow or
		// failing probe never delays creating/starting the next service — the
		// sequential Started-ack ordering above is load-bearing for
		// shared-ipc/shared-network joins and must not be disturbed (WDY-1271).
		runner.startAsync(runCtx, svcLifecycleCfgs[name])
		wg.Add(1)
		go func(name string, stream agentpb.WendyContainerService_StartContainerClient) {
			defer wg.Done()
			for {
				resp, recvErr := stream.Recv()
				if recvErr == io.EOF {
					return
				}
				if recvErr != nil {
					if runCtx.Err() == nil {
						cliLogln("Warning: service %s stream: %v", name, recvErr)
					}
					return
				}
				if out := resp.GetStdoutOutput(); out != nil {
					select {
					case lines <- logLine{service: name, stdout: true, data: out.GetData()}:
					case <-runCtx.Done():
						return
					}
				}
				if out := resp.GetStderrOutput(); out != nil {
					select {
					case lines <- logLine{service: name, stdout: false, data: out.GetData()}:
					case <-runCtx.Done():
						return
					}
				}
			}
		}(name, stream)
	}

	// Every service has started: fire the app-level fallback (nil on subset
	// runs). Async, for the same non-blocking reason as the per-service hooks.
	runner.startAsync(runCtx, appLevelCfg)

	go func() {
		wg.Wait()
		close(lines)
	}()

	cliLogln("App group %s started (%d services).", appID, len(ordered))

	for line := range lines {
		prefix := serviceLogStyle.Render(fmt.Sprintf("[%s] ", line.service))
		if line.stdout {
			fmt.Fprintf(os.Stdout, "%s%s", prefix, line.data)
		} else {
			fmt.Fprintf(os.Stderr, "%s%s", prefix, line.data)
		}
	}

	// The run has ended (streams closed, or Ctrl+C already canceled runCtx).
	// Cancel to abort any in-flight readiness wait and kill tracked cli hooks,
	// then reap so no hook goroutine or child outlives this call — mirroring
	// run.go's `runCancel(); postStartCmd.Wait()`.
	runCancel()
	runner.reap()

	cliLogln("\nApp group %s stopped.", appID)
	return nil
}
