package commands

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

// A Compose build includes its registry upload in the same builder call. This
// barrier therefore proves both that service image builds overlap and that a
// later build need not wait for an earlier service to finish uploading.
func TestBuildComposeServicesParallelOverlapsBuildAndPush(t *testing.T) {
	origBuild := buildComposeServiceImage
	origInteractive := isInteractiveTerminalFn
	t.Cleanup(func() {
		buildComposeServiceImage = origBuild
		isInteractiveTerminalFn = origInteractive
	})
	isInteractiveTerminalFn = func() bool { return false }

	const count = 4
	jobs := make(map[string]composeBuildJob, count)
	for i := range count {
		name := string(rune('a' + i))
		jobs[name] = composeBuildJob{contextDir: name, dockerfile: "Dockerfile", repo: "app-" + name}
	}

	var started sync.WaitGroup
	started.Add(count)
	var mu sync.Mutex
	cacheKeys := map[string]string{}
	buildComposeServiceImage = func(_ context.Context, _ *grpcclient.AgentConnection, _ int, _, _, _, repo, _, _ string, _ map[string]string, cacheKey string, _, _ io.Writer) error {
		mu.Lock()
		cacheKeys[repo] = cacheKey
		mu.Unlock()
		started.Done()
		started.Wait()
		return nil
	}

	done := make(chan error, 1)
	go func() {
		failed, _, err := buildComposeServicesParallel(context.Background(), nil, 5000, "linux", "docker", "linux/arm64", chunkingAuto, jobs, count, false)
		if err == nil && len(failed) != 0 {
			err = joinServiceErrors(failed)
		}
		done <- err
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("buildComposeServicesParallel: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Compose services did not enter build-and-push concurrently")
	}

	for _, job := range jobs {
		if cacheKeys[job.repo] != job.repo {
			t.Errorf("repo %q used cache key %q; concurrent exports need per-service isolation", job.repo, cacheKeys[job.repo])
		}
	}
}

func TestBuildComposeServicesParallelCancellationDoesNotStartQueuedBuilds(t *testing.T) {
	originalBuild := buildComposeServiceImage
	originalInteractive := isInteractiveTerminalFn
	t.Cleanup(func() {
		buildComposeServiceImage = originalBuild
		isInteractiveTerminalFn = originalInteractive
	})
	isInteractiveTerminalFn = func() bool { return false }

	jobs := map[string]composeBuildJob{}
	for _, name := range []string{"a", "b", "c", "d"} {
		jobs[name] = composeBuildJob{contextDir: name, dockerfile: "Dockerfile", repo: "app-" + name}
	}

	started := make(chan struct{}, len(jobs))
	var mu sync.Mutex
	startCount := 0
	buildComposeServiceImage = func(ctx context.Context, _ *grpcclient.AgentConnection, _ int, _, _, _, _, _, _ string, _ map[string]string, _ string, _, _ io.Writer) error {
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
		failed, _, err := buildComposeServicesParallel(ctx, nil, 5000, "linux", "docker", "linux/arm64", chunkingAuto, jobs, 1, false)
		done <- outcome{failed: failed, err: err}
	}()

	select {
	case <-started:
		cancel()
	case <-time.After(5 * time.Second):
		t.Fatal("first Compose service build did not start")
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
		t.Fatal("Compose builder did not return after cancellation")
	}

	mu.Lock()
	defer mu.Unlock()
	if startCount != 1 {
		t.Fatalf("started %d Compose builders after cancellation, want exactly the active one", startCount)
	}
}

func TestComposePreparationConcurrencyIsBoundedSeparately(t *testing.T) {
	ctx := withComposePrepareLimiter(context.Background())
	releases := make([]func(), 0, maxConcurrentComposePrepares)
	for range maxConcurrentComposePrepares {
		release, err := acquireComposePrepare(ctx)
		if err != nil {
			t.Fatal(err)
		}
		releases = append(releases, release)
	}

	acquired := make(chan func(), 1)
	go func() {
		release, err := acquireComposePrepare(ctx)
		if err == nil {
			acquired <- release
		}
	}()
	select {
	case release := <-acquired:
		release()
		t.Fatal("device preparation exceeded its independent concurrency bound")
	case <-time.After(50 * time.Millisecond):
	}

	releases[0]()
	select {
	case release := <-acquired:
		release()
	case <-time.After(time.Second):
		t.Fatal("queued device preparation did not start after a slot opened")
	}
	for _, release := range releases[1:] {
		release()
	}
}

func TestBuildComposeServicesParallelQuietBuildDoesNotOpenTUI(t *testing.T) {
	originalBuild := buildComposeServiceImage
	originalInteractive := isInteractiveTerminalFn
	t.Cleanup(func() {
		buildComposeServiceImage = originalBuild
		isInteractiveTerminalFn = originalInteractive
	})
	// quietBuild must short-circuit the terminal/TUI branch entirely.
	isInteractiveTerminalFn = func() bool {
		t.Fatal("quiet Compose build queried interactive terminal state")
		return true
	}

	called := false
	buildComposeServiceImage = func(_ context.Context, _ *grpcclient.AgentConnection, _ int, _, _, _, _, _, _ string, _ map[string]string, _ string, stream, logw io.Writer) error {
		called = true
		if stream == os.Stdout || stream == os.Stderr || logw == os.Stdout || logw == os.Stderr {
			t.Fatal("quiet Compose build streamed builder output to the terminal")
		}
		_, _ = io.WriteString(stream, "builder progress\n")
		_, _ = io.WriteString(logw, "builder setup\n")
		if reporter, ok := stream.(interface{ ReportImageContent([]string) }); ok {
			reporter.ReportImageContent([]string{"sha256:layer"})
		}
		return nil
	}

	jobs := map[string]composeBuildJob{
		"api": {contextDir: "api", dockerfile: "Dockerfile", repo: "app-api"},
	}
	failed, content, err := buildComposeServicesParallel(context.Background(), nil, 5000, "linux", "docker", "linux/arm64", chunkingAuto, jobs, 1, true)
	if err != nil {
		t.Fatalf("quiet Compose build: %v", err)
	}
	if len(failed) != 0 {
		t.Fatalf("quiet Compose failures = %v", failed)
	}
	if !called {
		t.Fatal("quiet Compose builder was not called")
	}
	if got := content["api"]; len(got) != 1 || got[0] != "sha256:layer" {
		t.Fatalf("prepared content = %v, want reported layer identity", got)
	}
}

func TestComposeServiceWatchHashTracksCreateRequest(t *testing.T) {
	cfg := &appconfig.AppConfig{AppID: "demo", ServiceName: "api"}
	policy := &agentpb.RestartPolicy{Mode: agentpb.RestartPolicyMode_UNLESS_STOPPED}
	base, err := composeServiceWatchHash("image-a", cfg, "server", []string{"--port", "80"}, policy, []string{"MODE=prod"})
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name   string
		image  string
		cmd    string
		args   []string
		env    []string
		policy *agentpb.RestartPolicy
	}{
		{"image", "image-b", "server", []string{"--port", "80"}, []string{"MODE=prod"}, policy},
		{"command", "image-a", "worker", []string{"--port", "80"}, []string{"MODE=prod"}, policy},
		{"args", "image-a", "server", []string{"--port", "81"}, []string{"MODE=prod"}, policy},
		{"env", "image-a", "server", []string{"--port", "80"}, []string{"MODE=dev"}, policy},
		{"restart", "image-a", "server", []string{"--port", "80"}, []string{"MODE=prod"}, &agentpb.RestartPolicy{Mode: agentpb.RestartPolicyMode_NO}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := composeServiceWatchHash(tc.image, cfg, tc.cmd, tc.args, tc.policy, tc.env)
			if err != nil {
				t.Fatal(err)
			}
			if got == base {
				t.Fatalf("%s change did not invalidate watch hash", tc.name)
			}
		})
	}
}

func TestImageRefContentPinned(t *testing.T) {
	digest := "sha256:" + strings.Repeat("b", 64)
	if !imageRefContentPinned("docker.io/library/alpine@" + digest) {
		t.Fatal("digest-pinned image was not eligible")
	}
	for _, ref := range []string{"alpine", "alpine:3.20", "${IMAGE}", ""} {
		if imageRefContentPinned(ref) {
			t.Fatalf("mutable or invalid image %q was eligible", ref)
		}
	}
}

func TestPlanComposeBuildSkipsVerifiedUnchangedService(t *testing.T) {
	isolateFingerprintCache(t)
	const (
		name      = "api"
		deviceKey = "device"
		desired   = "sha256:desired"
		layerDiff = "sha256:layer"
	)
	cfg := &appconfig.AppConfig{AppID: "demo", ServiceName: name, Version: "1"}
	states := map[string]agentpb.AppRunningState{
		strings.ToLower(cfg.ContainerName()): agentpb.AppRunningState_STOPPED,
	}
	saveDeployFingerprint(serviceFingerprintKey(cfg.AppID, name), deviceKey, deployFingerprint{
		InputHash: desired, LayerDiffIDs: []string{layerDiff},
	})
	fake := &multiSvcContainerClient{presentLayers: map[string]bool{layerDiff: true}}
	conn := &grpcclient.AgentConnection{ContainerService: fake}

	skip := planComposeBuildSkips(context.Background(), conn, deviceKey,
		map[string]string{name: desired}, map[string]*appconfig.AppConfig{name: cfg}, states)
	if !skip[name] {
		t.Fatal("verified unchanged Compose service was not skipped")
	}
}

func TestPlanComposeBuildSkipsFailsClosed(t *testing.T) {
	isolateFingerprintCache(t)
	const name = "api"
	cfg := &appconfig.AppConfig{AppID: "demo", ServiceName: name}
	states := map[string]agentpb.AppRunningState{
		strings.ToLower(cfg.ContainerName()): agentpb.AppRunningState_RUNNING,
	}
	saveDeployFingerprint(serviceFingerprintKey(cfg.AppID, name), "device", deployFingerprint{
		InputHash: "wanted", LayerDiffIDs: []string{"sha256:missing"},
	})
	fake := &multiSvcContainerClient{presentLayers: map[string]bool{}}
	conn := &grpcclient.AgentConnection{ContainerService: fake}

	skip := planComposeBuildSkips(context.Background(), conn, "device",
		map[string]string{name: "wanted"}, map[string]*appconfig.AppConfig{name: cfg}, states)
	if skip[name] {
		t.Fatal("Compose service skipped despite missing device image content")
	}

	delete(states, strings.ToLower(cfg.ContainerName()))
	fake.presentLayers["sha256:missing"] = true
	skip = planComposeBuildSkips(context.Background(), conn, "device",
		map[string]string{name: "wanted"}, map[string]*appconfig.AppConfig{name: cfg}, states)
	if skip[name] {
		t.Fatal("Compose service skipped despite missing device container")
	}
}

func writeComposeFile(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestParseComposeFile_PrefersDockerCompose(t *testing.T) {
	dir := t.TempDir()
	writeComposeFile(t, dir, "compose.yml", "services:\n  reporter:\n    image: alpine\n")
	writeComposeFile(t, dir, "docker-compose.yml", "services:\n  greeter:\n    image: alpine\n")

	cfg, name, err := parseComposeFile(dir)
	if err != nil {
		t.Fatalf("parseComposeFile: %v", err)
	}
	if name != "docker-compose.yml" {
		t.Fatalf("expected docker-compose.yml, got %q", name)
	}
	if _, ok := cfg.Services["greeter"]; !ok {
		t.Fatalf("expected service 'greeter', got %#v", cfg.Services)
	}
}

func TestParseComposeFile_NotFound(t *testing.T) {
	if _, _, err := parseComposeFile(t.TempDir()); err == nil {
		t.Fatal("expected error when no compose file is present")
	}
}

func TestComposeBuildContext(t *testing.T) {
	parse := func(t *testing.T, body string) composeService {
		t.Helper()
		var cfg composeConfig
		if err := yaml.Unmarshal([]byte(body), &cfg); err != nil {
			t.Fatal(err)
		}
		return cfg.Services["svc"]
	}

	t.Run("scalar build path", func(t *testing.T) {
		proj := t.TempDir()
		if err := os.MkdirAll(filepath.Join(proj, "api"), 0o755); err != nil {
			t.Fatal(err)
		}
		svc := parse(t, "services:\n  svc:\n    build: ./api\n")
		ctxDir, df, args, err := composeBuildContext(svc, proj)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if ctxDir != filepath.Join(proj, "api") || df != "Dockerfile" || args != nil {
			t.Fatalf("got (%q,%q,%v)", ctxDir, df, args)
		}
	})

	t.Run("mapping with custom dockerfile and args", func(t *testing.T) {
		proj := t.TempDir()
		svcDir := filepath.Join(proj, "svc")
		if err := os.MkdirAll(svcDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(svcDir, "Dockerfile.dev"), []byte("FROM alpine\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		svc := parse(t, "services:\n  svc:\n    build:\n      context: ./svc\n      dockerfile: Dockerfile.dev\n      args:\n        FOO: bar\n")
		ctxDir, df, args, err := composeBuildContext(svc, proj)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if ctxDir != svcDir || df != "Dockerfile.dev" || args["FOO"] != "bar" {
			t.Fatalf("got (%q,%q,%v)", ctxDir, df, args)
		}
	})

	t.Run("missing build returns empty", func(t *testing.T) {
		proj := t.TempDir()
		svc := parse(t, "services:\n  svc:\n    image: alpine\n")
		ctxDir, df, _, err := composeBuildContext(svc, proj)
		if err != nil || ctxDir != "" || df != "" {
			t.Fatalf("got (%q,%q,err=%v)", ctxDir, df, err)
		}
	})

	t.Run("unsupported build kind errors", func(t *testing.T) {
		proj := t.TempDir()
		svc := parse(t, "services:\n  svc:\n    build: [./a, ./b]\n")
		if _, _, _, err := composeBuildContext(svc, proj); err == nil {
			t.Fatal("expected error for sequence build kind")
		}
	})

	t.Run("dockerfile path traversal rejected", func(t *testing.T) {
		proj := t.TempDir()
		if err := os.MkdirAll(filepath.Join(proj, "svc"), 0o755); err != nil {
			t.Fatal(err)
		}
		svc := parse(t, "services:\n  svc:\n    build:\n      context: ./svc\n      dockerfile: ../../Dockerfile\n")
		if _, _, _, err := composeBuildContext(svc, proj); err == nil {
			t.Fatal("expected error for traversal dockerfile path")
		}
	})
}

func TestComposeEnv(t *testing.T) {
	parse := func(t *testing.T, body string) composeService {
		t.Helper()
		var cfg composeConfig
		if err := yaml.Unmarshal([]byte(body), &cfg); err != nil {
			t.Fatal(err)
		}
		return cfg.Services["svc"]
	}

	t.Run("mapping with mixed types", func(t *testing.T) {
		svc := parse(t, "services:\n  svc:\n    environment:\n      STR: hello\n      NUM: 42\n      BOOL: true\n")
		want := []string{"BOOL=true", "NUM=42", "STR=hello"}
		for i := 0; i < 100; i++ {
			got := composeEnv(svc)
			if len(got) != len(want) {
				t.Fatalf("iteration %d: got %v want %v", i, got, want)
			}
			for j := range want {
				if got[j] != want[j] {
					t.Fatalf("iteration %d: got %v want stable order %v", i, got, want)
				}
			}
		}
	})

	t.Run("mapping null inherits from process env", func(t *testing.T) {
		t.Setenv("WENDY_TEST_INHERIT", "from-process")
		svc := parse(t, "services:\n  svc:\n    environment:\n      WENDY_TEST_INHERIT: ~\n")
		got := composeEnv(svc)
		if len(got) != 1 || got[0] != "WENDY_TEST_INHERIT=from-process" {
			t.Fatalf("got %v", got)
		}
	})

	t.Run("list KEY entries inherit", func(t *testing.T) {
		t.Setenv("WENDY_TEST_LIST", "from-list")
		svc := parse(t, "services:\n  svc:\n    environment:\n      - WENDY_TEST_LIST\n      - EXPLICIT=value\n")
		got := composeEnv(svc)
		want := []string{"WENDY_TEST_LIST=from-list", "EXPLICIT=value"}
		if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
			t.Fatalf("got %v want %v", got, want)
		}
	})

	t.Run("list KEY without process env is dropped", func(t *testing.T) {
		os.Unsetenv("WENDY_TEST_MISSING")
		svc := parse(t, "services:\n  svc:\n    environment:\n      - WENDY_TEST_MISSING\n")
		if got := composeEnv(svc); len(got) != 0 {
			t.Fatalf("got %v, want empty", got)
		}
	})
}

func TestServiceOrder(t *testing.T) {
	parseConfig := func(t *testing.T, body string) *composeConfig {
		t.Helper()
		var cfg composeConfig
		if err := yaml.Unmarshal([]byte(body), &cfg); err != nil {
			t.Fatal(err)
		}
		return &cfg
	}

	t.Run("respects depends_on list form", func(t *testing.T) {
		cfg := parseConfig(t, "services:\n  api:\n    image: a\n    depends_on:\n      - db\n  db:\n    image: b\n")
		ordered, err := serviceOrder(cfg)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		// db must precede api.
		dbIdx, apiIdx := -1, -1
		for i, n := range ordered {
			if n == "db" {
				dbIdx = i
			}
			if n == "api" {
				apiIdx = i
			}
		}
		if dbIdx == -1 || apiIdx == -1 || dbIdx > apiIdx {
			t.Fatalf("expected db before api, got %v", ordered)
		}
	})

	t.Run("respects depends_on map form", func(t *testing.T) {
		cfg := parseConfig(t, "services:\n  api:\n    image: a\n    depends_on:\n      db:\n        condition: service_started\n  db:\n    image: b\n")
		ordered, err := serviceOrder(cfg)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if len(ordered) != 2 {
			t.Fatalf("got %v", ordered)
		}
	})

	t.Run("rejects unknown dependency", func(t *testing.T) {
		cfg := parseConfig(t, "services:\n  api:\n    image: a\n    depends_on:\n      - nope\n")
		if _, err := serviceOrder(cfg); err == nil {
			t.Fatal("expected error for unknown dependency")
		}
	})
}

func TestParseComposeVolume(t *testing.T) {
	cases := []struct {
		in       string
		src, tgt string
	}{
		{"data:/var/lib", "data", "/var/lib"},
		{"data:/var/lib:ro", "data", "/var/lib"},
		{"./host:/in/container", "./host", "/in/container"},
		{"/abs/host:/in/container", "/abs/host", "/in/container"},
		{"anonymous", "", "anonymous"},
		{"C:\\Users\\foo:/data", "C:\\Users\\foo", "/data"},
	}
	for _, c := range cases {
		src, tgt, _ := parseComposeVolume(c.in)
		if src != c.src || tgt != c.tgt {
			t.Errorf("parseComposeVolume(%q) = (%q,%q); want (%q,%q)", c.in, src, tgt, c.src, c.tgt)
		}
	}
}

func TestComposeAppConfig(t *testing.T) {
	parse := func(t *testing.T, body string) composeService {
		t.Helper()
		var cfg composeConfig
		if err := yaml.Unmarshal([]byte(body), &cfg); err != nil {
			t.Fatal(err)
		}
		return cfg.Services["svc"]
	}

	t.Run("ports synthesise network entitlement", func(t *testing.T) {
		svc := parse(t, "services:\n  svc:\n    ports:\n      - \"8080:80\"\n      - \"9000\"\n")
		cfg := composeAppConfig("proj", "svc", svc, 1)
		if cfg.AppID != "proj-svc" {
			t.Fatalf("appID: %s", cfg.AppID)
		}
		var ports []appconfig.PortMapping
		for _, e := range cfg.Entitlements {
			if e.Type == appconfig.EntitlementNetwork {
				ports = e.Ports
			}
		}
		if len(ports) != 2 || ports[0].Host != 8080 || ports[0].Container != 80 || ports[1].Host != 9000 || ports[1].Container != 9000 {
			t.Fatalf("unexpected ports: %+v", ports)
		}
	})

	t.Run("network_mode host overrides ports", func(t *testing.T) {
		svc := parse(t, "services:\n  svc:\n    network_mode: host\n    ports:\n      - \"80:80\"\n")
		cfg := composeAppConfig("proj", "svc", svc, 1)
		var found bool
		for _, e := range cfg.Entitlements {
			if e.Type == appconfig.EntitlementNetwork && e.Mode == "host" {
				found = true
			}
		}
		if !found {
			t.Fatalf("expected host-mode network entitlement, got %+v", cfg.Entitlements)
		}
	})

	t.Run("named volumes become persist entitlements; bind mounts skipped", func(t *testing.T) {
		svc := parse(t, "services:\n  svc:\n    volumes:\n      - data:/var/lib\n      - ./host:/in/container\n      - /abs/host:/in/container\n      - cache:/cache:ro\n")
		cfg := composeAppConfig("proj", "svc", svc, 1)
		var persists []appconfig.Entitlement
		for _, e := range cfg.Entitlements {
			if e.Type == appconfig.EntitlementPersist {
				persists = append(persists, e)
			}
		}
		if len(persists) != 2 {
			t.Fatalf("want 2 persist entitlements, got %d: %+v", len(persists), persists)
		}
		names := map[string]string{persists[0].Name: persists[0].Path, persists[1].Name: persists[1].Path}
		if names["data"] != "/var/lib" || names["cache"] != "/cache" {
			t.Fatalf("unexpected persist mapping: %+v", names)
		}
	})

	t.Run("multi-service groups under projectName without companion", func(t *testing.T) {
		emptySvc := parse(t, "services:\n  api:\n    image: nginx\n")
		api := composeAppConfig("myapp", "api", emptySvc, 2)
		if api.AppID != "myapp" {
			t.Fatalf("multi-service appID: want %q, got %q", "myapp", api.AppID)
		}
		if api.ServiceName != "api" {
			t.Fatalf("multi-service ServiceName: want %q, got %q", "api", api.ServiceName)
		}
		if api.ContainerName() != "myapp_api" {
			t.Fatalf("multi-service ContainerName: want %q, got %q", "myapp_api", api.ContainerName())
		}
	})

	t.Run("single-service keeps legacy appID without companion", func(t *testing.T) {
		emptySvc := parse(t, "services:\n  web:\n    image: nginx\n")
		cfg := composeAppConfig("myapp", "web", emptySvc, 1)
		if cfg.AppID != "myapp-web" {
			t.Fatalf("single-service appID: want %q, got %q", "myapp-web", cfg.AppID)
		}
		if cfg.ServiceName != "" {
			t.Fatalf("single-service ServiceName: want empty, got %q", cfg.ServiceName)
		}
		if got := composeWatchAppID("myapp", nil, map[string]*appconfig.AppConfig{"web": cfg}); got != "myapp-web" {
			t.Fatalf("single-service watch appID: want %q, got %q", "myapp-web", got)
		}
	})

	t.Run("multi-service watch uses grouped project appID", func(t *testing.T) {
		cfgs := map[string]*appconfig.AppConfig{
			"api": {AppID: "myapp", ServiceName: "api"},
			"db":  {AppID: "myapp", ServiceName: "db"},
		}
		if got := composeWatchAppID("myapp", nil, cfgs); got != "myapp" {
			t.Fatalf("multi-service watch appID: want %q, got %q", "myapp", got)
		}
	})

	t.Run("companion appID overrides watch identity", func(t *testing.T) {
		companion := &appconfig.AppConfig{AppID: "sh.wendy.example"}
		if got := composeWatchAppID("myapp", companion, nil); got != companion.AppID {
			t.Fatalf("companion watch appID: want %q, got %q", companion.AppID, got)
		}
	})
}

func TestNormalizeImageRef(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		// Bare name → docker.io/library/<name>:latest.
		{"python", "docker.io/library/python:latest"},
		// Name + tag.
		{"python:3.11-slim", "docker.io/library/python:3.11-slim"},
		// Two-segment ref → docker.io/<org>/<name>.
		{"library/nginx:1.27", "docker.io/library/nginx:1.27"},
		{"bitnami/redis:7", "docker.io/bitnami/redis:7"},
		// Custom registry passes through.
		{"gcr.io/google-containers/pause:3.9", "gcr.io/google-containers/pause:3.9"},
		{"localhost:5000/foo:bar", "localhost:5000/foo:bar"},
		{"registry.example.com:5000/team/app:1.2.3", "registry.example.com:5000/team/app:1.2.3"},
		// Digest references.
		{"python@sha256:0000000000000000000000000000000000000000000000000000000000000000", "docker.io/library/python@sha256:0000000000000000000000000000000000000000000000000000000000000000"},
		// Whitespace is trimmed.
		{"  python:3.11-slim  ", "docker.io/library/python:3.11-slim"},
		// Malformed → original input.
		{"this is not a ref", "this is not a ref"},
	}
	for _, c := range cases {
		if got := normalizeImageRef(c.in); got != c.want {
			t.Errorf("normalizeImageRef(%q) = %q; want %q", c.in, got, c.want)
		}
	}
}

func TestComposeArgv_PreservesMultiLineScript(t *testing.T) {
	body := "services:\n  svc:\n    command:\n      - python3\n      - -c\n      - |\n        import sys\n        print('hello')\n"
	var cfg composeConfig
	if err := yaml.Unmarshal([]byte(body), &cfg); err != nil {
		t.Fatal(err)
	}
	cmd, extra := composeArgv(cfg.Services["svc"])
	if cmd != "python3" {
		t.Fatalf("cmd = %q; want python3", cmd)
	}
	if len(extra) != 2 || extra[0] != "-c" {
		t.Fatalf("extra = %v", extra)
	}
	// The script body must survive intact, with its embedded newlines, so
	// the agent's strings.Fields(cmd) split can't word-split it.
	if !strings.Contains(extra[1], "import sys") || !strings.Contains(extra[1], "print('hello')") {
		t.Fatalf("script body lost; got %q", extra[1])
	}
}

func TestComposeArgv_ScalarShellSplit(t *testing.T) {
	body := "services:\n  svc:\n    command: \"python3 -m pip install -r requirements.txt\"\n"
	var cfg composeConfig
	if err := yaml.Unmarshal([]byte(body), &cfg); err != nil {
		t.Fatal(err)
	}
	cmd, extra := composeArgv(cfg.Services["svc"])
	want := []string{"-m", "pip", "install", "-r", "requirements.txt"}
	if cmd != "python3" || !slices.Equal(extra, want) {
		t.Fatalf("cmd=%q extra=%v; want python3 + %v", cmd, extra, want)
	}
}

func TestComposeArgv_Empty(t *testing.T) {
	body := "services:\n  svc:\n    image: alpine\n"
	var cfg composeConfig
	if err := yaml.Unmarshal([]byte(body), &cfg); err != nil {
		t.Fatal(err)
	}
	cmd, extra := composeArgv(cfg.Services["svc"])
	if cmd != "" || extra != nil {
		t.Fatalf("expected empty argv, got cmd=%q extra=%v", cmd, extra)
	}
}

func TestShellSplit(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"a b c", []string{"a", "b", "c"}},
		{"  spaces   between  ", []string{"spaces", "between"}},
		{`echo "hello world"`, []string{"echo", "hello world"}},
		{`echo 'single quotes work too'`, []string{"echo", "single quotes work too"}},
		{`mix "double" 'single' bare`, []string{"mix", "double", "single", "bare"}},
		{"", nil},
	}
	for _, c := range cases {
		got := shellSplit(c.in)
		if !slices.Equal(got, c.want) {
			t.Errorf("shellSplit(%q) = %v; want %v", c.in, got, c.want)
		}
	}
}

func TestComposeCompanionWarnings(t *testing.T) {
	cfg := &composeConfig{
		Services: map[string]composeService{
			"camera":   {},
			"detector": {},
		},
	}

	t.Run("nil companion produces no warnings", func(t *testing.T) {
		if w := composeCompanionWarnings(nil, cfg); len(w) != 0 {
			t.Fatalf("want no warnings, got %v", w)
		}
	})

	t.Run("all services matched", func(t *testing.T) {
		companion := &appconfig.AppConfig{
			AppID: "com.example.robot",
			Services: map[string]*appconfig.ServiceConfig{
				"camera":   {Entitlements: []appconfig.Entitlement{{Type: "camera"}}},
				"detector": {Entitlements: []appconfig.Entitlement{{Type: "gpu"}}},
			},
		}
		if w := composeCompanionWarnings(companion, cfg); len(w) != 0 {
			t.Fatalf("want no warnings for matched services, got %v", w)
		}
	})

	t.Run("unmatched service name warns", func(t *testing.T) {
		companion := &appconfig.AppConfig{
			AppID: "com.example.robot",
			Services: map[string]*appconfig.ServiceConfig{
				"camera":  {Entitlements: []appconfig.Entitlement{{Type: "camera"}}},
				"missing": {Entitlements: []appconfig.Entitlement{{Type: "gpu"}}},
			},
		}
		w := composeCompanionWarnings(companion, cfg)
		if len(w) != 1 {
			t.Fatalf("want 1 warning, got %v", w)
		}
		if !strings.Contains(w[0], "missing") {
			t.Errorf("warning should mention the unknown service name, got %q", w[0])
		}
	})

	t.Run("empty services map produces no warnings", func(t *testing.T) {
		companion := &appconfig.AppConfig{AppID: "com.example.robot"}
		if w := composeCompanionWarnings(companion, cfg); len(w) != 0 {
			t.Fatalf("want no warnings for empty services, got %v", w)
		}
	})
}

func TestApplyComposeCompanion(t *testing.T) {
	baseAppCfg := func() *appconfig.AppConfig {
		return &appconfig.AppConfig{
			AppID: "proj-camera",
			Entitlements: []appconfig.Entitlement{
				{Type: appconfig.EntitlementNetwork},
			},
		}
	}

	t.Run("nil companion is a no-op", func(t *testing.T) {
		got := baseAppCfg()
		applyComposeCompanion(got, nil, "camera")
		if len(got.Entitlements) != 1 || got.Isolation != "" || got.Frameworks != nil {
			t.Errorf("nil companion should not change AppConfig: %+v", got)
		}
	})

	t.Run("sets appId, serviceName, isolation and group frameworks", func(t *testing.T) {
		domainZero := 0
		companion := &appconfig.AppConfig{
			AppID:      "com.example.robot",
			Isolation:  "shared-ipc",
			Frameworks: &appconfig.FrameworksConfig{ROS2: &appconfig.ROS2Config{DomainID: &domainZero}},
		}
		got := baseAppCfg()
		applyComposeCompanion(got, companion, "camera")
		if got.AppID != "com.example.robot" {
			t.Errorf("AppID = %q, want %q", got.AppID, "com.example.robot")
		}
		if got.ServiceName != "camera" {
			t.Errorf("ServiceName = %q, want %q", got.ServiceName, "camera")
		}
		if got.Isolation != "shared-ipc" {
			t.Errorf("Isolation = %q, want %q", got.Isolation, "shared-ipc")
		}
		if got.Frameworks == nil || got.Frameworks.ROS2 == nil {
			t.Error("Frameworks.ROS2 should be set")
		}
	})

	t.Run("appends shared then per-service entitlements", func(t *testing.T) {
		companion := &appconfig.AppConfig{
			AppID:        "com.example.robot",
			Entitlements: []appconfig.Entitlement{{Type: appconfig.EntitlementBluetooth}},
			Services: map[string]*appconfig.ServiceConfig{
				"camera": {
					Entitlements: []appconfig.Entitlement{
						{Type: appconfig.EntitlementCamera},
						{Type: appconfig.EntitlementGPU},
					},
				},
			},
		}
		got := baseAppCfg()
		applyComposeCompanion(got, companion, "camera")
		// 1 compose + 1 shared + 2 per-service = 4
		if len(got.Entitlements) != 4 {
			t.Fatalf("want 4 entitlements (1 compose + 1 shared + 2 per-service), got %d: %+v", len(got.Entitlements), got.Entitlements)
		}
	})

	t.Run("per-service frameworks override group frameworks", func(t *testing.T) {
		groupID, svcID := 0, 42
		groupROS2 := &appconfig.ROS2Config{DomainID: &groupID}
		svcROS2 := &appconfig.ROS2Config{DomainID: &svcID}
		companion := &appconfig.AppConfig{
			AppID:      "com.example.robot",
			Frameworks: &appconfig.FrameworksConfig{ROS2: groupROS2},
			Services: map[string]*appconfig.ServiceConfig{
				"camera": {
					Frameworks: &appconfig.FrameworksConfig{ROS2: svcROS2},
				},
			},
		}
		got := baseAppCfg()
		applyComposeCompanion(got, companion, "camera")
		if got.Frameworks == nil || got.Frameworks.ROS2 == nil {
			t.Fatal("Frameworks.ROS2 should be set")
		}
		if got.Frameworks.ROS2.DomainID == nil || *got.Frameworks.ROS2.DomainID != 42 {
			t.Errorf("DomainID = %v, want 42 (per-service override)", got.Frameworks.ROS2.DomainID)
		}
	})

	t.Run("group frameworks apply when service has no frameworks", func(t *testing.T) {
		domainFive := 5
		groupROS2 := &appconfig.ROS2Config{DomainID: &domainFive}
		companion := &appconfig.AppConfig{
			AppID:      "com.example.robot",
			Frameworks: &appconfig.FrameworksConfig{ROS2: groupROS2},
			Services: map[string]*appconfig.ServiceConfig{
				"camera": {Entitlements: []appconfig.Entitlement{{Type: appconfig.EntitlementCamera}}},
			},
		}
		got := baseAppCfg()
		applyComposeCompanion(got, companion, "camera")
		if got.Frameworks == nil || got.Frameworks.ROS2 == nil || got.Frameworks.ROS2.DomainID == nil || *got.Frameworks.ROS2.DomainID != 5 {
			t.Errorf("expected group-level ROS2 DomainID=5, got %+v", got.Frameworks)
		}
	})

	t.Run("unknown service uses only group config", func(t *testing.T) {
		companion := &appconfig.AppConfig{
			AppID:     "com.example.robot",
			Isolation: "shared-ipc",
			Services: map[string]*appconfig.ServiceConfig{
				"camera": {Entitlements: []appconfig.Entitlement{{Type: appconfig.EntitlementCamera}}},
			},
		}
		got := baseAppCfg()
		applyComposeCompanion(got, companion, "detector") // not in services map
		if got.Isolation != "shared-ipc" {
			t.Errorf("Isolation = %q, want %q", got.Isolation, "shared-ipc")
		}
		// No per-service entitlements should be added.
		if len(got.Entitlements) != 1 {
			t.Errorf("want 1 entitlement (no per-service addition), got %d", len(got.Entitlements))
		}
	})

	t.Run("keeps the shared and the per-service camera entries with their settings", func(t *testing.T) {
		shared := appconfig.Entitlement{Type: appconfig.EntitlementCamera, Allowlist: []string{"/dev/video0"},
			Format: appconfig.CameraFormatNative, Stamp: appconfig.CameraStampFrame}
		perService := appconfig.Entitlement{Type: appconfig.EntitlementCamera, Allowlist: []string{"Logi H264"},
			Format: appconfig.CameraFormatH264, Stamp: appconfig.CameraStampFrame}
		companion := &appconfig.AppConfig{
			AppID:        "com.example.robot",
			Entitlements: []appconfig.Entitlement{shared},
			Services: map[string]*appconfig.ServiceConfig{
				"camera": {Entitlements: []appconfig.Entitlement{perService}},
			},
		}
		got := baseAppCfg()
		applyComposeCompanion(got, companion, "camera")
		if want := []appconfig.Entitlement{{Type: appconfig.EntitlementNetwork}, shared, perService}; !reflect.DeepEqual(got.Entitlements, want) {
			t.Fatalf("entitlements = %+v, want the compose one, then the shared and the per-service camera entries, %+v", got.Entitlements, want)
		}
		wantEntitlementLabels(t, got, map[string]string{
			"sh.wendy/entitlement.network":  "enabled=true",
			"sh.wendy/entitlement.camera.0": "format=native,stamp=frame,allowlist=/dev/video0",
			"sh.wendy/entitlement.camera.1": "format=h264,stamp=frame,allowlist=Logi H264",
		})
	})
}

func TestDeduplicateEntitlements(t *testing.T) {
	gpu := appconfig.Entitlement{Type: appconfig.EntitlementGPU}
	cam := appconfig.Entitlement{Type: appconfig.EntitlementCamera}
	net := appconfig.Entitlement{Type: appconfig.EntitlementNetwork, Mode: "host"}
	persist1 := appconfig.Entitlement{Type: appconfig.EntitlementPersist, Name: "data"}
	persist2 := appconfig.Entitlement{Type: appconfig.EntitlementPersist, Name: "logs"}
	serial0 := appconfig.Entitlement{Type: appconfig.EntitlementSerial, Device: "ttyUSB0"}
	serial1 := appconfig.Entitlement{Type: appconfig.EntitlementSerial, Device: "ttyUSB1"}

	t.Run("removes exact duplicates", func(t *testing.T) {
		got := deduplicateEntitlements([]appconfig.Entitlement{gpu, cam, gpu})
		if len(got) != 2 {
			t.Fatalf("want 2, got %d: %+v", len(got), got)
		}
	})

	t.Run("preserves order, first occurrence wins", func(t *testing.T) {
		got := deduplicateEntitlements([]appconfig.Entitlement{gpu, cam, gpu, net})
		if len(got) != 3 || got[0].Type != appconfig.EntitlementGPU || got[1].Type != appconfig.EntitlementCamera {
			t.Fatalf("unexpected order: %+v", got)
		}
	})

	t.Run("distinct persist names are kept", func(t *testing.T) {
		got := deduplicateEntitlements([]appconfig.Entitlement{persist1, persist2, persist1})
		if len(got) != 2 {
			t.Fatalf("want 2, got %d: %+v", len(got), got)
		}
	})

	t.Run("distinct serial devices are kept", func(t *testing.T) {
		got := deduplicateEntitlements([]appconfig.Entitlement{serial0, serial1, serial0})
		if len(got) != 2 || got[0].Device != "ttyUSB0" || got[1].Device != "ttyUSB1" {
			t.Fatalf("want ttyUSB0 and ttyUSB1 once each, got %+v", got)
		}
	})

	t.Run("all parameter fields participate in identity", func(t *testing.T) {
		ents := []appconfig.Entitlement{
			{Type: appconfig.EntitlementGPIO, Pins: []int{17}},
			{Type: appconfig.EntitlementGPIO, Pins: []int{18}},
			{Type: appconfig.EntitlementCamera, Allowlist: []string{"/dev/video0"}},
			{Type: appconfig.EntitlementCamera, Allowlist: []string{"/dev/video1"}},
			{Type: appconfig.EntitlementNetwork, Ports: []appconfig.PortMapping{{Host: 8080, Container: 80}}},
			{Type: appconfig.EntitlementNetwork, Ports: []appconfig.PortMapping{{Host: 9090, Container: 90}}},
		}
		if got := deduplicateEntitlements(ents); len(got) != len(ents) {
			t.Fatalf("parameterized entitlements collapsed: got %+v", got)
		}
	})

	t.Run("camera entries that differ only in format or stamp are kept", func(t *testing.T) {
		entry := func(format, stamp string) appconfig.Entitlement {
			return appconfig.Entitlement{Type: appconfig.EntitlementCamera, Allowlist: []string{"/dev/video0"}, Format: format, Stamp: stamp}
		}
		ents := []appconfig.Entitlement{
			entry("", ""),
			entry(appconfig.CameraFormatNative, ""),
			entry(appconfig.CameraFormatH264, ""),
			entry("", appconfig.CameraStampFrame),
			entry(appconfig.CameraFormatNative, appconfig.CameraStampFrame),
		}
		got := deduplicateEntitlements(append(ents, entry(appconfig.CameraFormatNative, appconfig.CameraStampFrame)))
		if len(got) != len(ents) {
			t.Fatalf("want the %d distinct entries, got %d: %+v", len(ents), len(got), got)
		}
		for i, e := range got {
			if e.Format != ents[i].Format || e.Stamp != ents[i].Stamp {
				t.Errorf("entry %d = %+v, want %+v", i, e, ents[i])
			}
		}
	})

	t.Run("empty input", func(t *testing.T) {
		if got := deduplicateEntitlements(nil); len(got) != 0 {
			t.Fatalf("want empty, got %+v", got)
		}
	})
}

func TestComposeRestartPolicy(t *testing.T) {
	cases := []struct {
		in   string
		want agentpb.RestartPolicyMode
	}{
		{"always", agentpb.RestartPolicyMode_UNLESS_STOPPED},
		{"unless-stopped", agentpb.RestartPolicyMode_UNLESS_STOPPED},
		{"on-failure", agentpb.RestartPolicyMode_ON_FAILURE},
		{"no", agentpb.RestartPolicyMode_NO},
		{"", agentpb.RestartPolicyMode_NO},
		{"weird", agentpb.RestartPolicyMode_DEFAULT},
	}
	for _, c := range cases {
		got := composeRestartPolicy(c.in).GetMode()
		if got != c.want {
			t.Errorf("composeRestartPolicy(%q) = %v; want %v", c.in, got, c.want)
		}
	}
}

func TestComposeWarnUnsupportedFields(t *testing.T) {
	raw := `
services:
  api:
    image: python:3.11
    devices:
      - /dev/video0
    privileged: true
    ipc: host
`
	var cfg composeConfig
	if err := yaml.Unmarshal([]byte(raw), &cfg); err != nil {
		t.Fatal(err)
	}

	warnings := unsupportedComposeWarnings(cfg.Services["api"])

	contains := func(field string) bool {
		for _, w := range warnings {
			if w == field {
				return true
			}
		}
		return false
	}
	if !contains("devices") {
		t.Errorf("expected 'devices' in warnings, got %v", warnings)
	}
	if !contains("privileged") {
		t.Errorf("expected 'privileged' in warnings, got %v", warnings)
	}
	if !contains("ipc") {
		t.Errorf("expected 'ipc' in warnings, got %v", warnings)
	}
}

func TestParseXWendy(t *testing.T) {
	parseSvc := func(t *testing.T, body string) composeService {
		t.Helper()
		var cfg composeConfig
		if err := yaml.Unmarshal([]byte(body), &cfg); err != nil {
			t.Fatal(err)
		}
		return cfg.Services["web"]
	}

	t.Run("full config populates readiness and hooks", func(t *testing.T) {
		svc := parseSvc(t, `
services:
  web:
    image: alpine
    x-wendy:
      readiness:
        tcpSocket:
          port: 3000
        timeoutSeconds: 30
      hooks:
        postStart:
          openURL: "http://${WENDY_HOSTNAME}:3000"
          agent: "touch /tmp/x"
`)
		r, h, warnings, err := parseXWendy(svc.XWendy, "web")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(warnings) != 0 {
			t.Fatalf("unexpected warnings: %v", warnings)
		}
		if r == nil || r.TCPSocket == nil || r.TCPSocket.Port != 3000 || r.TimeoutSeconds != 30 {
			t.Fatalf("readiness not populated as expected: %+v", r)
		}
		if h == nil || h.PostStart == nil {
			t.Fatalf("hooks not populated: %+v", h)
		}
		if h.PostStart.OpenURL != "http://${WENDY_HOSTNAME}:3000" || h.PostStart.Agent != "touch /tmp/x" || h.PostStart.CLI != "" {
			t.Fatalf("hooks.postStart mismatch: %+v", h.PostStart)
		}
	})

	t.Run("zero node returns all nil", func(t *testing.T) {
		svc := parseSvc(t, "services:\n  web:\n    image: alpine\n")
		r, h, warnings, err := parseXWendy(svc.XWendy, "web")
		if err != nil || r != nil || h != nil || warnings != nil {
			t.Fatalf("expected all nil/no error, got r=%v h=%v warnings=%v err=%v", r, h, warnings, err)
		}
	})

	t.Run("unknown keys warn in sorted order, config still parses", func(t *testing.T) {
		// Declared zeta-before-alpha so the assertion proves the warnings are
		// emitted sorted (parseXWendy sorts unknown keys), not map-iteration order.
		svc := parseSvc(t, "services:\n  web:\n    x-wendy:\n      zeta: 1\n      alpha: 2\n")
		r, h, warnings, err := parseXWendy(svc.XWendy, "web")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if r != nil || h != nil {
			t.Fatalf("expected nil readiness/hooks, got r=%v h=%v", r, h)
		}
		if len(warnings) != 2 {
			t.Fatalf("expected two unknown-key warnings, got %v", warnings)
		}
		if !strings.Contains(warnings[0], `unknown x-wendy key "alpha"`) {
			t.Fatalf("expected first (sorted) warning to name \"alpha\", got %v", warnings)
		}
		if !strings.Contains(warnings[1], `unknown x-wendy key "zeta"`) {
			t.Fatalf("expected second (sorted) warning to name \"zeta\", got %v", warnings)
		}
	})

	t.Run("invalid port errors with service-scoped prefix", func(t *testing.T) {
		svc := parseSvc(t, "services:\n  web:\n    x-wendy:\n      readiness:\n        tcpSocket:\n          port: 0\n")
		_, _, _, err := parseXWendy(svc.XWendy, "web")
		if err == nil || !strings.Contains(err.Error(), "services.web.x-wendy.readiness") {
			t.Fatalf("expected readiness validation error, got %v", err)
		}
	})

	t.Run("non-portable cli opener produces one lint warning", func(t *testing.T) {
		svc := parseSvc(t, "services:\n  web:\n    x-wendy:\n      hooks:\n        postStart:\n          cli: \"open http://x\"\n")
		r, h, warnings, err := parseXWendy(svc.XWendy, "web")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if r != nil {
			t.Fatalf("expected nil readiness, got %v", r)
		}
		if h == nil || h.PostStart == nil || h.PostStart.CLI != "open http://x" {
			t.Fatalf("hooks not populated as expected: %+v", h)
		}
		if len(warnings) != 1 {
			t.Fatalf("expected exactly one lint warning, got %v", warnings)
		}
	})

	t.Run("scalar readiness value errors", func(t *testing.T) {
		svc := parseSvc(t, "services:\n  web:\n    x-wendy:\n      readiness: \"yes\"\n")
		_, _, _, err := parseXWendy(svc.XWendy, "web")
		if err == nil {
			t.Fatal("expected error for scalar readiness value")
		}
	})
}

// TestComposeAppLevelAgentHookDropped guards the condition behind the warning
// runComposeWithAgent prints when a compose companion declares a top-level
// hooks.postStart.agent: compose apps have no app-level container to run it in,
// so it is dropped — and Stage-A ValidateJSON stays silent because a compose
// companion has no services map. Only a non-empty .agent triggers it;
// openURL/cli-only top-level hooks (which DO run as an app-level fallback) do not.
func TestComposeAppLevelAgentHookDropped(t *testing.T) {
	cases := []struct {
		name string
		cfg  *appconfig.AppConfig
		want bool
	}{
		{"nil cfg", nil, false},
		{"nil hooks", &appconfig.AppConfig{AppID: "app"}, false},
		{"nil postStart", &appconfig.AppConfig{Hooks: &appconfig.HooksConfig{}}, false},
		{"openURL only", &appconfig.AppConfig{Hooks: &appconfig.HooksConfig{PostStart: &appconfig.HookCommand{OpenURL: "http://x"}}}, false},
		{"cli only", &appconfig.AppConfig{Hooks: &appconfig.HooksConfig{PostStart: &appconfig.HookCommand{CLI: "touch x"}}}, false},
		{"agent set", &appconfig.AppConfig{Hooks: &appconfig.HooksConfig{PostStart: &appconfig.HookCommand{Agent: "echo hi"}}}, true},
		{"agent set alongside openURL", &appconfig.AppConfig{Hooks: &appconfig.HooksConfig{PostStart: &appconfig.HookCommand{OpenURL: "http://x", Agent: "echo hi"}}}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := composeAppLevelAgentHookDropped(tc.cfg); got != tc.want {
				t.Errorf("composeAppLevelAgentHookDropped(%+v) = %v, want %v", tc.cfg, got, tc.want)
			}
		})
	}
}

func TestBuildComposeServiceConfigs_XWendy(t *testing.T) {
	t.Run("multi-service: only x-wendy service gets readiness/hooks", func(t *testing.T) {
		body := `
services:
  frontend:
    image: alpine
    x-wendy:
      readiness:
        tcpSocket:
          port: 3000
      hooks:
        postStart:
          agent: "touch /tmp/ready"
  backend:
    image: alpine
`
		var cfg composeConfig
		if err := yaml.Unmarshal([]byte(body), &cfg); err != nil {
			t.Fatal(err)
		}
		svcCfgs, warnings, err := buildComposeServiceConfigs("proj", &cfg, nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(warnings) != 0 {
			t.Fatalf("unexpected warnings: %v", warnings)
		}
		frontend := svcCfgs["frontend"]
		if frontend == nil {
			t.Fatal("expected frontend AppConfig")
		}
		if frontend.AppID != "proj" || frontend.ServiceName != "frontend" {
			t.Fatalf("frontend grouped naming wrong: AppID=%q ServiceName=%q", frontend.AppID, frontend.ServiceName)
		}
		if frontend.Readiness == nil || frontend.Readiness.TCPSocket == nil || frontend.Readiness.TCPSocket.Port != 3000 {
			t.Fatalf("frontend readiness not applied: %+v", frontend.Readiness)
		}
		if frontend.Hooks == nil || frontend.Hooks.PostStart == nil || frontend.Hooks.PostStart.Agent != "touch /tmp/ready" {
			t.Fatalf("frontend hooks not applied: %+v", frontend.Hooks)
		}

		backend := svcCfgs["backend"]
		if backend == nil {
			t.Fatal("expected backend AppConfig")
		}
		if backend.AppID != "proj" || backend.ServiceName != "backend" {
			t.Fatalf("backend grouped naming wrong: AppID=%q ServiceName=%q", backend.AppID, backend.ServiceName)
		}
		if backend.Readiness != nil || backend.Hooks != nil {
			t.Fatalf("backend should have nil readiness/hooks, got %+v / %+v", backend.Readiness, backend.Hooks)
		}
	})

	t.Run("single-service: legacy naming with x-wendy applied", func(t *testing.T) {
		body := `
services:
  web:
    image: alpine
    x-wendy:
      readiness:
        tcpSocket:
          port: 8080
`
		var cfg composeConfig
		if err := yaml.Unmarshal([]byte(body), &cfg); err != nil {
			t.Fatal(err)
		}
		svcCfgs, _, err := buildComposeServiceConfigs("myapp", &cfg, nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		web := svcCfgs["web"]
		if web == nil {
			t.Fatal("expected web AppConfig")
		}
		if web.AppID != "myapp-web" {
			t.Fatalf("legacy appID: want %q, got %q", "myapp-web", web.AppID)
		}
		if web.ServiceName != "" {
			t.Fatalf("legacy ServiceName: want empty, got %q", web.ServiceName)
		}
		if web.Readiness == nil || web.Readiness.TCPSocket == nil || web.Readiness.TCPSocket.Port != 8080 {
			t.Fatalf("readiness not applied: %+v", web.Readiness)
		}
	})
}

func TestComposeServiceLifecycleConfigs_ScopesHTTP(t *testing.T) {
	composeCfg := &composeConfig{Services: map[string]composeService{
		"minecraft": {Image: "alpine"},
		"webui":     {Image: "alpine"},
	}}
	companion := &appconfig.AppConfig{
		AppID:        "app",
		Entitlements: []appconfig.Entitlement{{Type: appconfig.EntitlementHTTP, Port: 8080}},
		Readiness:    &appconfig.ReadinessConfig{TimeoutSeconds: 180},
		Services: map[string]*appconfig.ServiceConfig{
			"webui": {Entitlements: []appconfig.Entitlement{{Type: appconfig.EntitlementHTTP, Port: 9090}}},
		},
	}

	createCfgs, warnings, err := buildComposeServiceConfigs("proj", composeCfg, companion)
	if err != nil {
		t.Fatalf("buildComposeServiceConfigs: %v", err)
	}
	if len(warnings) != 0 {
		t.Fatalf("unexpected warnings: %v", warnings)
	}
	lifecycleCfgs := composeServiceLifecycleConfigs(createCfgs, companion)
	for _, name := range []string{"minecraft", "webui"} {
		if port, ok := httpEntitlementPort(createCfgs[name].Entitlements); !ok || port != 8080 {
			t.Errorf("%s create config HTTP = %d, %v; want inherited top-level 8080", name, port, ok)
		}
	}
	if lifecycleCfgs["minecraft"] != nil {
		t.Errorf("minecraft lifecycle config = %+v, want nil", lifecycleCfgs["minecraft"])
	}
	if port, ok := httpEntitlementPort(lifecycleCfgs["webui"].Entitlements); !ok || port != 9090 {
		t.Errorf("webui lifecycle HTTP = %d, %v; want service-declared 9090", port, ok)
	}
	appLifecycle := appLevelLifecycleConfig(companion.AppID, companion)
	if readiness := effectiveReadiness(appLifecycle); readiness == nil || readiness.TCPSocket == nil || readiness.TCPSocket.Port != 8080 || readiness.TimeoutSeconds != 180 {
		t.Errorf("effective app lifecycle readiness = %+v, want port 8080 and timeoutSeconds 180", readiness)
	}
}

func TestApplyComposeCompanion_LifecycleOverride(t *testing.T) {
	xWendyAppCfg := func() *appconfig.AppConfig {
		return &appconfig.AppConfig{
			AppID:     "proj-web",
			Readiness: &appconfig.ReadinessConfig{TCPSocket: &appconfig.TCPSocketProbe{Port: 3000}},
			Hooks:     &appconfig.HooksConfig{PostStart: &appconfig.HookCommand{Agent: "touch /tmp/x-wendy"}},
		}
	}

	t.Run("companion service-level readiness and hooks override x-wendy values", func(t *testing.T) {
		companion := &appconfig.AppConfig{
			AppID: "com.example.robot",
			Services: map[string]*appconfig.ServiceConfig{
				"web": {
					Readiness: &appconfig.ReadinessConfig{TCPSocket: &appconfig.TCPSocketProbe{Port: 9000}},
					Hooks:     &appconfig.HooksConfig{PostStart: &appconfig.HookCommand{Agent: "touch /tmp/companion"}},
				},
			},
		}
		got := xWendyAppCfg()
		applyComposeCompanion(got, companion, "web")
		if got.Readiness == nil || got.Readiness.TCPSocket == nil || got.Readiness.TCPSocket.Port != 9000 {
			t.Fatalf("readiness not overridden: %+v", got.Readiness)
		}
		if got.Hooks == nil || got.Hooks.PostStart == nil || got.Hooks.PostStart.Agent != "touch /tmp/companion" {
			t.Fatalf("hooks not overridden: %+v", got.Hooks)
		}
	})

	t.Run("companion sets only readiness: hooks stay x-wendy, readiness replaced", func(t *testing.T) {
		companion := &appconfig.AppConfig{
			AppID: "com.example.robot",
			Services: map[string]*appconfig.ServiceConfig{
				"web": {
					Readiness: &appconfig.ReadinessConfig{TCPSocket: &appconfig.TCPSocketProbe{Port: 9000}},
				},
			},
		}
		got := xWendyAppCfg()
		applyComposeCompanion(got, companion, "web")
		if got.Readiness == nil || got.Readiness.TCPSocket == nil || got.Readiness.TCPSocket.Port != 9000 {
			t.Fatalf("readiness not overridden: %+v", got.Readiness)
		}
		if got.Hooks == nil || got.Hooks.PostStart == nil || got.Hooks.PostStart.Agent != "touch /tmp/x-wendy" {
			t.Fatalf("hooks should survive from x-wendy, got %+v", got.Hooks)
		}
	})

	t.Run("companion top-level readiness/hooks never land per-service", func(t *testing.T) {
		companion := &appconfig.AppConfig{
			AppID:     "com.example.robot",
			Readiness: &appconfig.ReadinessConfig{TCPSocket: &appconfig.TCPSocketProbe{Port: 1234}},
			Hooks:     &appconfig.HooksConfig{PostStart: &appconfig.HookCommand{Agent: "touch /tmp/top-level"}},
		}

		t.Run("service with x-wendy values keeps them", func(t *testing.T) {
			got := xWendyAppCfg()
			applyComposeCompanion(got, companion, "web")
			if got.Readiness == nil || got.Readiness.TCPSocket.Port != 3000 {
				t.Fatalf("readiness should stay x-wendy value, got %+v", got.Readiness)
			}
			if got.Hooks == nil || got.Hooks.PostStart.Agent != "touch /tmp/x-wendy" {
				t.Fatalf("hooks should stay x-wendy value, got %+v", got.Hooks)
			}
		})

		t.Run("service with no x-wendy values stays nil", func(t *testing.T) {
			got := &appconfig.AppConfig{AppID: "proj-other"}
			applyComposeCompanion(got, companion, "other")
			if got.Readiness != nil || got.Hooks != nil {
				t.Fatalf("top-level readiness/hooks must not land per-service, got readiness=%+v hooks=%+v", got.Readiness, got.Hooks)
			}
		})
	})
}

func TestComposeWarnUnsupportedFields_NoneWhenClean(t *testing.T) {
	raw := `
services:
  api:
    image: python:3.11
    environment:
      - FOO=bar
    restart: unless-stopped
`
	var cfg composeConfig
	if err := yaml.Unmarshal([]byte(raw), &cfg); err != nil {
		t.Fatal(err)
	}

	warnings := unsupportedComposeWarnings(cfg.Services["api"])
	if len(warnings) != 0 {
		t.Errorf("expected no warnings for clean service, got %v", warnings)
	}
}

func TestComposeBuildContext_PrefersStagefile(t *testing.T) {
	parse := func(t *testing.T, body string) composeService {
		t.Helper()
		var cfg composeConfig
		if err := yaml.Unmarshal([]byte(body), &cfg); err != nil {
			t.Fatal(err)
		}
		return cfg.Services["svc"]
	}

	t.Run("scalar build path with stagefile", func(t *testing.T) {
		proj := t.TempDir()
		svcDir := filepath.Join(proj, "api")
		if err := os.MkdirAll(svcDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(svcDir, stagefileSourceName), []byte("version: 1\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		svc := parse(t, "services:\n  svc:\n    build: ./api\n")
		_, df, _, err := composeBuildContext(svc, proj)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if df != stagefileSourceName {
			t.Fatalf("got %q, want %q", df, stagefileSourceName)
		}
	})

	t.Run("mapping without dockerfile with stagefile", func(t *testing.T) {
		proj := t.TempDir()
		svcDir := filepath.Join(proj, "svc")
		if err := os.MkdirAll(svcDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(svcDir, stagefileSourceName), []byte("version: 1\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		svc := parse(t, "services:\n  svc:\n    build:\n      context: ./svc\n")
		_, df, _, err := composeBuildContext(svc, proj)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if df != stagefileSourceName {
			t.Fatalf("got %q, want %q", df, stagefileSourceName)
		}
	})

	t.Run("explicit dockerfile wins over stagefile", func(t *testing.T) {
		proj := t.TempDir()
		svcDir := filepath.Join(proj, "svc")
		if err := os.MkdirAll(svcDir, 0o755); err != nil {
			t.Fatal(err)
		}
		for name, body := range map[string]string{
			stagefileSourceName: "version: 1\n",
			"Dockerfile.dev":    "FROM alpine\n",
		} {
			if err := os.WriteFile(filepath.Join(svcDir, name), []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		svc := parse(t, "services:\n  svc:\n    build:\n      context: ./svc\n      dockerfile: Dockerfile.dev\n")
		_, df, _, err := composeBuildContext(svc, proj)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if df != "Dockerfile.dev" {
			t.Fatalf("got %q, want %q", df, "Dockerfile.dev")
		}
	})
}

func TestComposeStagefileOverride(t *testing.T) {
	proj := t.TempDir()
	svcDir := filepath.Join(proj, "server")
	if err := os.MkdirAll(svcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	source := "version: 1\nstages:\n  - name: app\n    from: debian:12\n"
	if err := os.WriteFile(filepath.Join(svcDir, stagefileSourceName), []byte(source), 0o644); err != nil {
		t.Fatal(err)
	}
	// Pre-seeded pin so the compile never touches a live registry.
	lockContent := "version: 1\nsourceHash: sha256:irrelevant\nimages:\n  debian:12: sha256:fakepindigest\n"
	if err := os.WriteFile(filepath.Join(svcDir, "build.stagefile.lock.yaml"), []byte(lockContent), 0o644); err != nil {
		t.Fatal(err)
	}
	compose := "services:\n  server:\n    build: ./server\n  cached:\n    image: alpine\n"
	if err := os.WriteFile(filepath.Join(proj, "docker-compose.yml"), []byte(compose), 0o644); err != nil {
		t.Fatal(err)
	}
	companion := `{
  "appId": "sh.wendy.compose-test",
  "services": {
    "server": {
      "frameworks": {
        "ros2": {"distro": "humble", "rmw": "cyclonedds"}
      }
    }
  }
}`
	if err := os.WriteFile(filepath.Join(proj, "wendy.json"), []byte(companion), 0o644); err != nil {
		t.Fatal(err)
	}

	path, cleanup, err := composeStagefileOverride(proj, "")
	if err != nil {
		t.Fatalf("composeStagefileOverride: %v", err)
	}
	if path == "" {
		t.Fatal("expected an override file, got none")
	}
	defer cleanup()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Services map[string]struct {
			Build struct {
				Context    string `yaml:"context"`
				Dockerfile string `yaml:"dockerfile"`
			} `yaml:"build"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatalf("parsing override: %v\n%s", err, data)
	}
	ov, ok := doc.Services["server"]
	if !ok || ov.Build.Context != "server" || ov.Build.Dockerfile != "Dockerfile.generated" {
		t.Fatalf("override = %+v\n%s", doc, data)
	}
	if _, hasCached := doc.Services["cached"]; hasCached {
		t.Fatalf("image-only service must not be overridden:\n%s", data)
	}
	generated, err := os.ReadFile(filepath.Join(svcDir, "Dockerfile.generated"))
	if err != nil {
		t.Fatalf("expected compiled Dockerfile.generated: %v", err)
	}
	if !strings.Contains(string(generated), "sha256:fakepindigest") {
		t.Fatalf("generated Dockerfile missing pinned digest:\n%s", generated)
	}
	if !strings.Contains(string(generated), "'ros-humble-rmw-cyclonedds-cpp'") {
		t.Fatalf("generated Dockerfile missing service framework middleware:\n%s", generated)
	}
}

func TestComposeStagefileOverride_NoStagefiles(t *testing.T) {
	proj := t.TempDir()
	svcDir := filepath.Join(proj, "server")
	if err := os.MkdirAll(svcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(svcDir, "Dockerfile"), []byte("FROM alpine\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	compose := "services:\n  server:\n    build: ./server\n"
	if err := os.WriteFile(filepath.Join(proj, "docker-compose.yml"), []byte(compose), 0o644); err != nil {
		t.Fatal(err)
	}
	path, cleanup, err := composeStagefileOverride(proj, "")
	if err != nil {
		t.Fatalf("composeStagefileOverride: %v", err)
	}
	if path != "" || cleanup != nil {
		t.Fatalf("expected no override for a Dockerfile-only project, got %q", path)
	}
}
