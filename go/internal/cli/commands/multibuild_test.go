package commands

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
	"github.com/wendylabsinc/wendy/go/internal/stagefile"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

func TestMultiServiceWatchHashTracksBuildAndRuntimeState(t *testing.T) {
	cfg := &appconfig.AppConfig{AppID: "demo", ServiceName: "api"}
	policy := &agentpb.RestartPolicy{Mode: agentpb.RestartPolicyMode_UNLESS_STOPPED}
	base, err := multiServiceWatchHash("build-a", cfg, []string{"MODE=prod"}, policy)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name   string
		build  string
		cfg    *appconfig.AppConfig
		env    []string
		policy *agentpb.RestartPolicy
	}{
		{"build", "build-b", cfg, []string{"MODE=prod"}, policy},
		{"config", "build-a", &appconfig.AppConfig{AppID: "demo", ServiceName: "worker"}, []string{"MODE=prod"}, policy},
		{"env", "build-a", cfg, []string{"MODE=dev"}, policy},
		{"restart", "build-a", cfg, []string{"MODE=prod"}, &agentpb.RestartPolicy{Mode: agentpb.RestartPolicyMode_NO}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := multiServiceWatchHash(tc.build, tc.cfg, tc.env, tc.policy)
			if err != nil {
				t.Fatal(err)
			}
			if got == base {
				t.Fatalf("%s change did not invalidate watch hash", tc.name)
			}
		})
	}
}

// newServiceTree writes n service directories under a fresh temp root, each
// with a minimal Dockerfile, and returns the root plus the services map.
func newServiceTree(t *testing.T, n int) (string, map[string]*appconfig.ServiceConfig) {
	t.Helper()
	root := t.TempDir()
	services := map[string]*appconfig.ServiceConfig{}
	for i := range n {
		name := fmt.Sprintf("svc%02d", i)
		dir := filepath.Join(root, name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM scratch\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		services[name] = &appconfig.ServiceConfig{Context: name}
	}
	return root, services
}

// TestComputeServicePlansRunsServicesConcurrently is the reason planning was
// split out of planServicePushSkips. Per service it resolves a build file —
// which for a Stagefile project is a full compile including registry digest
// lookups — and hashes the entire build context by walking and reading every
// file. Serially that is dead wall-clock time before the first `docker build`
// even starts, and it scaled with service count: the 14-service go2 template
// paid 14 context walks back to back.
//
// The stubbed resolver blocks until every service's plan is in flight, which
// can only happen if they run at once. A serial implementation never releases
// the barrier, so the bounded wait fails the test rather than hanging the suite.
func TestComputeServicePlansRunsServicesConcurrently(t *testing.T) {
	root, services := newServiceTree(t, maxConcurrentPlans)

	var inFlight sync.WaitGroup
	inFlight.Add(len(services))
	orig := planResolveDockerfile
	defer func() { planResolveDockerfile = orig }()
	planResolveDockerfile = func(cwd, requested string, interactive bool, gpuArch string, _ ...stagefile.Option) (string, error) {
		inFlight.Done()
		inFlight.Wait() // released only once every other service's plan has started
		return "Dockerfile", nil
	}

	done := make(chan map[string]servicePlan, 1)
	go func() {
		done <- computeServicePlans(root, "linux/arm64", "", nil, services, nil)
	}()

	select {
	case plans := <-done:
		if len(plans) != len(services) {
			t.Fatalf("planned %d services, want %d", len(plans), len(services))
		}
		for name, plan := range plans {
			if plan.dockerfile != "Dockerfile" {
				t.Errorf("service %s: dockerfile = %q, want Dockerfile", name, plan.dockerfile)
			}
			if plan.inputHash == "" {
				t.Errorf("service %s: inputHash is empty", name)
			}
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("computeServicePlans did not plan all %d services concurrently", len(services))
	}
}

// TestComputeServicePlansOmitsServicesItCannotPlan preserves what the serial
// loop did by `continue`ing past a failure: a service whose build file cannot be
// resolved simply has no plan. Callers read a missing plan as "can't skip, and
// no precomputed dockerfile to reuse", so the build path resolves it again and
// surfaces the real error there rather than aborting the whole group here.
func TestComputeServicePlansOmitsServicesItCannotPlan(t *testing.T) {
	root, services := newServiceTree(t, 2)

	orig := planResolveDockerfile
	defer func() { planResolveDockerfile = orig }()
	planResolveDockerfile = func(cwd, requested string, interactive bool, gpuArch string, _ ...stagefile.Option) (string, error) {
		if filepath.Base(cwd) == "svc00" {
			return "", fmt.Errorf("no build file")
		}
		return "Dockerfile", nil
	}

	plans := computeServicePlans(root, "linux/arm64", "", nil, services, nil)

	if _, ok := plans["svc00"]; ok {
		t.Error("svc00 could not be resolved; it must not get a plan")
	}
	if _, ok := plans["svc01"]; !ok {
		t.Error("svc01 resolved cleanly; it must still get a plan when a sibling fails")
	}
}

// TestBuildServicesParallelReusesPlannedDockerfile closes the double-compile:
// planning already resolved (and, for a Stagefile, compiled) each service's
// build file, so the build goroutine must use that result instead of resolving
// the same directory a second time on every run.
func TestBuildServicesParallelReusesPlannedDockerfile(t *testing.T) {
	root, services := newServiceTree(t, 3)

	const appID = "app"
	planned := map[string]string{}
	for name := range services {
		// A name resolveDockerfile would never produce on its own, so seeing it
		// arrive at the builder proves the planned value was threaded through
		// rather than recomputed.
		planned[name] = "Dockerfile.planned-" + name
	}

	var mu sync.Mutex
	got := map[string]string{} // repo -> dockerfile the builder received
	orig := buildServiceImage
	defer func() { buildServiceImage = orig }()
	buildServiceImage = func(_ context.Context, _ *grpcclient.AgentConnection, _ int, _, _, _, repo, _, dockerfile string, _ map[string]string, _ string, _, _ io.Writer) error {
		mu.Lock()
		got[repo] = dockerfile
		mu.Unlock()
		return nil
	}

	failed, infraErr := buildServicesParallel(
		context.Background(), nil, 5000, "linux", root, appID, services, "linux/arm64", nil, "docker", nil, planned, len(services), false)
	if infraErr != nil {
		t.Fatalf("unexpected infra error: %v", infraErr)
	}
	if len(failed) != 0 {
		t.Fatalf("unexpected build failures: %v", sortedServiceErrorKeys(failed))
	}

	for name := range services {
		repo := appID + "-" + name
		if got[repo] != planned[name] {
			t.Errorf("service %s built with dockerfile %q, want the planned %q", name, got[repo], planned[name])
		}
	}
}

func TestBuildServicesParallelWithContentReportsPreparedLayers(t *testing.T) {
	root, services := newServiceTree(t, 2)
	planned := map[string]string{"svc00": "Dockerfile", "svc01": "Dockerfile"}

	originalBuild := buildServiceImage
	t.Cleanup(func() { buildServiceImage = originalBuild })
	buildServiceImage = func(_ context.Context, _ *grpcclient.AgentConnection, _ int, _, _, _, repo, _, _ string, _ map[string]string, _ string, stream, _ io.Writer) error {
		reporter, ok := stream.(interface{ ReportImageContent([]string) })
		if !ok {
			return fmt.Errorf("build output for %s cannot report image content", repo)
		}
		reporter.ReportImageContent([]string{"sha256:" + repo})
		return nil
	}

	failed, content, err := buildServicesParallelWithContent(
		context.Background(), nil, 5000, "linux", root, "app", services,
		"linux/arm64", nil, "docker", chunkingAuto, nil, planned, 2, true)
	if err != nil {
		t.Fatalf("buildServicesParallelWithContent: %v", err)
	}
	if len(failed) != 0 {
		t.Fatalf("failed = %v, want none", failed)
	}
	for name := range services {
		want := "sha256:app-" + name
		if got := content[name]; len(got) != 1 || got[0] != want {
			t.Errorf("content[%s] = %v, want [%s]", name, got, want)
		}
	}
}

func TestBuildServicesParallelWithContentDropsFailedBuildIdentity(t *testing.T) {
	root, services := newServiceTree(t, 1)
	originalBuild := buildServiceImage
	t.Cleanup(func() { buildServiceImage = originalBuild })
	buildServiceImage = func(_ context.Context, _ *grpcclient.AgentConnection, _ int, _, _, _, _ string, _ string, _ string, _ map[string]string, _ string, stream, _ io.Writer) error {
		stream.(interface{ ReportImageContent([]string) }).ReportImageContent([]string{"sha256:partial"})
		return errors.New("prepare failed")
	}

	failed, content, err := buildServicesParallelWithContent(
		context.Background(), nil, 5000, "linux", root, "app", services,
		"linux/arm64", nil, "docker", chunkingAuto, nil,
		map[string]string{"svc00": "Dockerfile"}, 1, true)
	if err != nil {
		t.Fatalf("buildServicesParallelWithContent infrastructure error: %v", err)
	}
	if failed["svc00"] == nil {
		t.Fatal("failed build missing from failure map")
	}
	if len(content) != 0 {
		t.Fatalf("content = %v, want none for failed build", content)
	}
}

func TestBuildServicesParallelCancellationStopsActiveAndQueuedBuilds(t *testing.T) {
	root, services := newServiceTree(t, 4)
	planned := make(map[string]string, len(services))
	for name := range services {
		planned[name] = "Dockerfile"
	}

	originalBuild := buildServiceImage
	originalInteractive := isInteractiveTerminalFn
	t.Cleanup(func() {
		buildServiceImage = originalBuild
		isInteractiveTerminalFn = originalInteractive
	})
	isInteractiveTerminalFn = func() bool { return false }

	started := make(chan struct{}, len(services))
	var mu sync.Mutex
	startCount := 0
	buildServiceImage = func(ctx context.Context, _ *grpcclient.AgentConnection, _ int, _, _, _, _, _, _ string, _ map[string]string, _ string, _, _ io.Writer) error {
		mu.Lock()
		startCount++
		mu.Unlock()
		started <- struct{}{}
		<-ctx.Done()
		return ctx.Err()
	}

	ctx, cancel := context.WithCancel(context.Background())
	type outcome struct {
		failed map[string]error
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		failed, err := buildServicesParallel(ctx, nil, 5000, "linux", root, "app", services,
			"linux/arm64", nil, "docker", nil, planned, 1, false)
		done <- outcome{failed: failed, err: err}
	}()

	select {
	case <-started:
		cancel()
	case <-time.After(5 * time.Second):
		t.Fatal("first service build did not start")
	}

	select {
	case got := <-done:
		if !errors.Is(got.err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", got.err)
		}
		if got.failed != nil {
			t.Fatalf("failed = %v, want nil for operation cancellation", got.failed)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("parallel builder did not return after cancellation")
	}

	mu.Lock()
	defer mu.Unlock()
	if startCount != 1 {
		t.Fatalf("started %d service builders after cancellation, want exactly the active one", startCount)
	}
}

func TestBuildServicesParallelQuietBuildDoesNotOpenTUI(t *testing.T) {
	root, services := newServiceTree(t, 1)
	originalBuild := buildServiceImage
	originalInteractive := isInteractiveTerminalFn
	t.Cleanup(func() {
		buildServiceImage = originalBuild
		isInteractiveTerminalFn = originalInteractive
	})
	isInteractiveTerminalFn = func() bool {
		t.Fatal("quiet multi-service build queried interactive terminal state")
		return true
	}

	called := false
	buildServiceImage = func(_ context.Context, _ *grpcclient.AgentConnection, _ int, _, _, _, _, _, _ string, _ map[string]string, _ string, stream, logw io.Writer) error {
		called = true
		if stream == os.Stdout || stream == os.Stderr || logw == os.Stdout || logw == os.Stderr {
			t.Fatal("quiet multi-service build streamed builder output to the terminal")
		}
		_, _ = io.WriteString(stream, "builder progress\n")
		_, _ = io.WriteString(logw, "builder setup\n")
		return nil
	}

	planned := map[string]string{}
	for name := range services {
		planned[name] = "Dockerfile"
	}
	failed, err := buildServicesParallel(context.Background(), nil, 5000, "linux", root, "app", services,
		"linux/arm64", nil, "docker", nil, planned, 1, true)
	if err != nil {
		t.Fatalf("quiet multi-service build: %v", err)
	}
	if len(failed) != 0 {
		t.Fatalf("quiet multi-service failures = %v", failed)
	}
	if !called {
		t.Fatal("quiet multi-service builder was not called")
	}
}

// TestBuildServicesParallelResolvesWhenPlanningSkippedIt keeps the fallback
// honest: planning is best-effort (it bails out on WENDY_PUSH_SKIP=0 and on any
// per-service error), so a service with no planned entry must still resolve its
// own build file rather than being built with an empty one.
func TestBuildServicesParallelResolvesWhenPlanningSkippedIt(t *testing.T) {
	root, services := newServiceTree(t, 2)

	const appID = "app"
	var mu sync.Mutex
	got := map[string]string{}
	orig := buildServiceImage
	defer func() { buildServiceImage = orig }()
	buildServiceImage = func(_ context.Context, _ *grpcclient.AgentConnection, _ int, _, _, _, repo, _, dockerfile string, _ map[string]string, _ string, _, _ io.Writer) error {
		mu.Lock()
		got[repo] = dockerfile
		mu.Unlock()
		return nil
	}

	failed, infraErr := buildServicesParallel(
		context.Background(), nil, 5000, "linux", root, appID, services, "linux/arm64", nil, "docker", nil, nil, len(services), false)
	if infraErr != nil {
		t.Fatalf("unexpected infra error: %v", infraErr)
	}
	if len(failed) != 0 {
		t.Fatalf("unexpected build failures: %v", sortedServiceErrorKeys(failed))
	}

	for name := range services {
		if repo := appID + "-" + name; got[repo] != "Dockerfile" {
			t.Errorf("service %s built with dockerfile %q, want the self-resolved Dockerfile", name, got[repo])
		}
	}
}

// ros2ExampleAppConfig mirrors Examples/ROS2/wendy.json: group-level
// frameworks.ros2 + isolation, with a per-service override on one service.
func ros2ExampleAppConfig() *appconfig.AppConfig {
	groupDomain, svcDomain := 42, 7
	return &appconfig.AppConfig{
		AppID:     "sh.wendy.examples.ros2",
		Version:   "1.0.0",
		Platform:  "linux/arm64",
		Isolation: "shared-network",
		Frameworks: &appconfig.FrameworksConfig{
			ROS2: &appconfig.ROS2Config{DomainID: &groupDomain, RMW: "rmw_cyclonedds_cpp"},
		},
		Entitlements: []appconfig.Entitlement{{Type: appconfig.EntitlementBluetooth}},
		Services: map[string]*appconfig.ServiceConfig{
			"talker": {Context: "./talker"},
			"listener": {
				Context:      "./listener",
				DependsOn:    []string{"talker"},
				Entitlements: []appconfig.Entitlement{{Type: appconfig.EntitlementGPU}},
				Frameworks: &appconfig.FrameworksConfig{
					ROS2: &appconfig.ROS2Config{DomainID: &svcDomain},
				},
			},
		},
	}
}

// The per-service AppConfig transmitted to the agent must preserve the group
// identity and runtime context — dropping frameworks/isolation here was the
// root cause of ROS_DOMAIN_ID never reaching containers (WDY-884).
func TestMultiServiceCreateConfig_PreservesGroupContext(t *testing.T) {
	appCfg := ros2ExampleAppConfig()
	got := multiServiceCreateConfig(appCfg, "talker", appCfg.Services["talker"])

	if got.AppID != "sh.wendy.examples.ros2" {
		t.Errorf("AppID = %q, want unmangled group appId", got.AppID)
	}
	if got.ServiceName != "talker" {
		t.Errorf("ServiceName = %q, want talker", got.ServiceName)
	}
	if got.ContainerName() != "sh.wendy.examples.ros2_talker" {
		t.Errorf("ContainerName() = %q, want sh.wendy.examples.ros2_talker", got.ContainerName())
	}
	if got.Isolation != "shared-network" {
		t.Errorf("Isolation = %q, want shared-network", got.Isolation)
	}
	if got.Version != "1.0.0" || got.Platform != "linux/arm64" {
		t.Errorf("Version/Platform = %q/%q, want 1.0.0/linux/arm64", got.Version, got.Platform)
	}
	ros2 := got.GetROS2Config()
	if ros2 == nil || ros2.DomainID == nil || *ros2.DomainID != 42 {
		t.Fatalf("talker must inherit group frameworks.ros2 (domainId 42), got %+v", ros2)
	}
	// Group-level entitlements are shared with every service.
	if len(got.Entitlements) != 1 || got.Entitlements[0].Type != appconfig.EntitlementBluetooth {
		t.Errorf("talker entitlements = %+v, want shared bluetooth", got.Entitlements)
	}
}

func TestMultiServiceCreateConfig_ServiceFrameworksOverride(t *testing.T) {
	appCfg := ros2ExampleAppConfig()
	got := multiServiceCreateConfig(appCfg, "listener", appCfg.Services["listener"])

	ros2 := got.GetROS2Config()
	if ros2 == nil || ros2.DomainID == nil || *ros2.DomainID != 7 {
		t.Fatalf("listener must use its own frameworks.ros2 override (domainId 7), got %+v", ros2)
	}
	// Shared + per-service entitlements are merged.
	if len(got.Entitlements) != 2 {
		t.Errorf("listener entitlements = %+v, want shared bluetooth + gpu", got.Entitlements)
	}
}

// A service's own readiness/hooks must travel with its per-service AppConfig so
// startAndStreamServices can fire them scoped to that container (WDY-1271).
func TestMultiServiceCreateConfig_PropagatesReadinessHooks(t *testing.T) {
	appCfg := ros2ExampleAppConfig()
	readiness := &appconfig.ReadinessConfig{TCPSocket: &appconfig.TCPSocketProbe{Port: 8080}, TimeoutSeconds: 5}
	hooks := &appconfig.HooksConfig{PostStart: &appconfig.HookCommand{OpenURL: "http://${WENDY_HOSTNAME}:8080", Agent: "echo hi"}}
	svc := appCfg.Services["talker"]
	svc.Readiness = readiness
	svc.Hooks = hooks

	got := multiServiceCreateConfig(appCfg, "talker", svc)

	if got.Readiness != readiness {
		t.Errorf("Readiness = %p, want the same pointer as svc.Readiness (%p)", got.Readiness, readiness)
	}
	if got.Hooks != hooks {
		t.Errorf("Hooks = %p, want the same pointer as svc.Hooks (%p)", got.Hooks, hooks)
	}
}

// The group's top-level readiness/hooks are the app-level fallback, fired once
// after all services start. A service that declares nothing must therefore get
// nil readiness/hooks rather than a copy of the group-level values.
func TestMultiServiceCreateConfig_DoesNotInheritTopLevelHooks(t *testing.T) {
	appCfg := ros2ExampleAppConfig()
	appCfg.Readiness = &appconfig.ReadinessConfig{TCPSocket: &appconfig.TCPSocketProbe{Port: 9090}}
	appCfg.Hooks = &appconfig.HooksConfig{PostStart: &appconfig.HookCommand{Agent: "echo app-level"}}

	got := multiServiceCreateConfig(appCfg, "talker", appCfg.Services["talker"])

	if got.Readiness != nil {
		t.Errorf("Readiness = %+v, want nil (top-level readiness must not leak per-service)", got.Readiness)
	}
	if got.Hooks != nil {
		t.Errorf("Hooks = %+v, want nil (top-level hooks must not leak per-service)", got.Hooks)
	}
}

func TestMultiServiceLifecycleConfig_ScopesHTTPEntitlements(t *testing.T) {
	appCfg := &appconfig.AppConfig{
		AppID: "sh.wendy.examples.wendymc",
		Entitlements: []appconfig.Entitlement{
			{Type: appconfig.EntitlementNetwork, Mode: "host"},
			{Type: appconfig.EntitlementHTTP, Port: 8080},
		},
		Readiness: &appconfig.ReadinessConfig{TimeoutSeconds: 180},
		Services: map[string]*appconfig.ServiceConfig{
			"minecraft": {Context: "./minecraft"},
			"webui":     {Context: "./webui"},
		},
	}

	for _, name := range []string{"minecraft", "webui"} {
		createCfg := multiServiceCreateConfig(appCfg, name, appCfg.Services[name])
		if port, ok := httpEntitlementPort(createCfg.Entitlements); !ok || port != 8080 {
			t.Errorf("%s create config HTTP = %d, %v; want inherited 8080", name, port, ok)
		}
		if lifecycleCfg := multiServiceLifecycleConfig(appCfg.AppID, name, appCfg.Services[name]); lifecycleCfg != nil {
			t.Errorf("%s lifecycle config = %+v, want nil (top-level HTTP must not execute per service)", name, lifecycleCfg)
		}
	}

	appLifecycle := appLevelLifecycleConfig(appCfg.AppID, appCfg)
	if appLifecycle == nil {
		t.Fatal("app-level HTTP should produce a lifecycle config")
	}
	if port, ok := httpEntitlementPort(appLifecycle.Entitlements); !ok || port != 8080 {
		t.Errorf("app-level lifecycle HTTP = %d, %v; want 8080", port, ok)
	}
	if appLifecycle.Readiness == nil || appLifecycle.Readiness.TimeoutSeconds != 180 {
		t.Errorf("app-level readiness = %+v, want timeoutSeconds 180", appLifecycle.Readiness)
	}
	if readiness := effectiveReadiness(appLifecycle); readiness == nil || readiness.TCPSocket == nil || readiness.TCPSocket.Port != 8080 || readiness.TimeoutSeconds != 180 {
		t.Errorf("effective app-level readiness = %+v, want port 8080 with timeoutSeconds 180", readiness)
	}

	appCfg.Services["webui"].Entitlements = []appconfig.Entitlement{{Type: appconfig.EntitlementHTTP, Port: 9090}}
	serviceLifecycle := multiServiceLifecycleConfig(appCfg.AppID, "webui", appCfg.Services["webui"])
	if serviceLifecycle == nil {
		t.Fatal("service-local HTTP should produce a lifecycle config")
	}
	if port, ok := httpEntitlementPort(serviceLifecycle.Entitlements); !ok || port != 9090 {
		t.Errorf("service lifecycle HTTP = %d, %v; want service-declared 9090", port, ok)
	}
}

func TestMultiServiceContainerName_MatchesAgentConvention(t *testing.T) {
	appCfg := ros2ExampleAppConfig()
	cfg := multiServiceCreateConfig(appCfg, "talker", appCfg.Services["talker"])
	// Start/stop in the multibuild path must address the same container name
	// the agent derives from (AppID, ServiceName) at creation time.
	if got := multiServiceContainerName(appCfg.AppID, "talker"); got != cfg.ContainerName() {
		t.Errorf("multiServiceContainerName = %q, ContainerName() = %q — start/stop would miss the container", got, cfg.ContainerName())
	}
}

// TestMultiServiceCreateConfig_CarriesResolvedResources covers WDY-2171/WDY-1729:
// the per-service config sent to the agent has no services map, so it must
// carry the already-resolved limits. Without them the agent resolves nothing
// and the container runs uncapped.
func TestMultiServiceCreateConfig_CarriesResolvedResources(t *testing.T) {
	pids := int64(256)
	appCfg := &appconfig.AppConfig{
		AppID:     "app",
		Resources: &appconfig.ResourceLimits{Memory: "256Mi", PIDs: &pids},
		Services: map[string]*appconfig.ServiceConfig{
			"db":  {Context: "db"},
			"api": {Context: "api", Resources: &appconfig.ResourceLimits{Memory: "128Mi"}},
		},
	}

	db := multiServiceCreateConfig(appCfg, "db", appCfg.Services["db"])
	if db.Resources == nil {
		t.Fatal("db: resources are nil; the service inherits the app-level limits")
	}
	if db.Resources.Memory != "256Mi" {
		t.Errorf("db memory = %q, want the inherited 256Mi", db.Resources.Memory)
	}

	api := multiServiceCreateConfig(appCfg, "api", appCfg.Services["api"])
	if api.Resources == nil {
		t.Fatal("api: resources are nil")
	}
	if api.Resources.Memory != "128Mi" {
		t.Errorf("api memory = %q, want its own 128Mi", api.Resources.Memory)
	}
	if api.Resources.PIDs == nil || *api.Resources.PIDs != pids {
		t.Errorf("api pids = %v, want the inherited %d (a memory override must not drop it)", api.Resources.PIDs, pids)
	}
}
