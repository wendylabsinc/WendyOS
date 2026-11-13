package commands

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
	"github.com/wendylabsinc/wendy/go/internal/stagefile"
)

// Coverage for WDY-2563: multi-service `wendy build` detection (a validated
// wendy.json services map routes to runMultiServiceBuild instead of the
// single-image detectBuildOptions path), --service/--max-concurrency
// selection, the --dockerfile/--build-type refusals, builder propagation, and
// the no-push/no-deploy guarantee (buildServiceImage, the device-registry
// seam, must never be invoked from this path).

// writeMultiServiceBuildProject writes a temp project with a validated
// wendy.json services map — appID, one entry per services key with the given
// dependsOn list and context "./<name>" — and one subdirectory per service
// holding a minimal Dockerfile. Modeled on newServiceTree (multibuild_test.go)
// but with a real manifest, mirroring Examples/IsolatedServices/wendy.json's
// shape (appId/platform/version/services) so appconfig.Validate accepts it.
// Returns the project root.
func writeMultiServiceBuildProject(t *testing.T, appID string, services map[string][]string) string {
	t.Helper()
	root := t.TempDir()

	svcCfg := make(map[string]*appconfig.ServiceConfig, len(services))
	for name, deps := range services {
		dir := filepath.Join(root, name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM scratch\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		svcCfg[name] = &appconfig.ServiceConfig{Context: "./" + name, DependsOn: deps}
	}

	cfg := &appconfig.AppConfig{
		AppID:    appID,
		Platform: "linux/arm64",
		Version:  "1.0.0",
		Services: svcCfg,
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "wendy.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

// setupMultiServiceBuildCmdTest arranges the seams every command-level
// `wendy build` multi-service test needs:
//
//   - WENDY_AGENT_SOCKET points at a socket that does not exist, so
//     resolveTarget's dialAgentSocketIfSet short-circuits immediately instead
//     of falling through to config load / mDNS discovery / an interactive
//     picker (see helpers_socket_test.go, buildkit_test.go:69's
//     TestShouldUseBuildkitOnDevice). The lazy gRPC client this produces makes
//     any subsequent RPC (e.g. resolveBuildPlatform's GetAgentVersion) fail
//     fast with Unavailable, which the production code already tolerates by
//     falling back to its linux/arm64 default — RPC failures are tolerated by
//     design here.
//   - isInteractiveTerminalFn is forced non-interactive so no TUI spinner
//     opens.
//   - buildServiceImage (the push-to-device-registry seam) fails the test if
//     it is ever invoked — the no-push/no-deploy guarantee for `wendy build`
//     against a services-map project.
//
// All three are restored via t.Cleanup.
func setupMultiServiceBuildCmdTest(t *testing.T) {
	t.Helper()

	t.Setenv("WENDY_AGENT_SOCKET", filepath.Join(t.TempDir(), "agent.sock"))

	origInteractive := isInteractiveTerminalFn
	t.Cleanup(func() { isInteractiveTerminalFn = origInteractive })
	isInteractiveTerminalFn = func() bool { return false }

	origPush := buildServiceImage
	t.Cleanup(func() { buildServiceImage = origPush })
	buildServiceImage = func(context.Context, *grpcclient.AgentConnection, int, string, string, string, string, string, string, map[string]string, string, io.Writer, io.Writer) error {
		t.Errorf("push path invoked")
		return nil
	}
}

// localBuildCall is one recorded invocation of the buildLocalServiceImage seam.
type localBuildCall struct {
	contextDir string
	imageName  string
	dockerfile string
}

// localBuildRecorder collects buildLocalServiceImage calls behind a mutex, for
// command-level `wendy build` tests that assert on which services were built
// and with what arguments, without touching Docker.
type localBuildRecorder struct {
	mu    sync.Mutex
	calls []localBuildCall
}

func (r *localBuildRecorder) record(contextDir, imageName, dockerfile string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, localBuildCall{contextDir: contextDir, imageName: imageName, dockerfile: dockerfile})
}

func (r *localBuildRecorder) snapshot() []localBuildCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]localBuildCall, len(r.calls))
	copy(out, r.calls)
	return out
}

// stubLocalBuild installs a buildLocalServiceImage stub backed by fn,
// restoring the original via t.Cleanup.
func stubLocalBuild(t *testing.T, fn func(ctx context.Context, builder, contextDir, imageName, platform, dockerfile string, buildOut, logOut io.Writer) error) {
	t.Helper()
	orig := buildLocalServiceImage
	t.Cleanup(func() { buildLocalServiceImage = orig })
	buildLocalServiceImage = fn
}

func TestBuildCmd_MultiService_BuildsFromManifestOnly(t *testing.T) {
	root := writeMultiServiceBuildProject(t, "myapp", map[string][]string{
		"api":    nil,
		"worker": nil,
	})
	chdirTo(t, root)
	setupMultiServiceBuildCmdTest(t)

	rec := &localBuildRecorder{}
	stubLocalBuild(t, func(_ context.Context, _, contextDir, imageName, _, dockerfile string, _, _ io.Writer) error {
		rec.record(contextDir, imageName, dockerfile)
		return nil
	})

	cmd := newBuildCmd()
	cmd.SetArgs([]string{})
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() = %v, want nil", err)
	}

	calls := rec.snapshot()
	if len(calls) != 2 {
		t.Fatalf("recorded %d local build calls, want exactly 2 (one per service): %+v", len(calls), calls)
	}
	seen := map[string]bool{}
	for _, c := range calls {
		name := filepath.Base(c.contextDir)
		if seen[name] {
			t.Fatalf("service %q built more than once", name)
		}
		seen[name] = true

		wantImage := strings.ToLower("myapp") + "-" + strings.ToLower(name) + ":latest"
		if c.imageName != wantImage {
			t.Errorf("service %s: imageName = %q, want %q", name, c.imageName, wantImage)
		}
		wantContext := filepath.Clean(filepath.Join(root, "./"+name))
		gotInfo, gotErr := os.Stat(c.contextDir)
		wantInfo, wantErr := os.Stat(wantContext)
		if gotErr != nil || wantErr != nil || !os.SameFile(gotInfo, wantInfo) {
			t.Errorf("service %s: contextDir = %q, want directory %q (stat errors: got=%v want=%v)", name, c.contextDir, wantContext, gotErr, wantErr)
		}
	}
	if !seen["api"] || !seen["worker"] {
		t.Fatalf("expected both api and worker built, got %+v", seen)
	}
}

func TestBuildCmd_MultiService_PropagatesBuildkitBuilder(t *testing.T) {
	root := writeMultiServiceBuildProject(t, "myapp", map[string][]string{
		"api":    nil,
		"worker": nil,
	})
	chdirTo(t, root)
	setupMultiServiceBuildCmdTest(t)

	var mu sync.Mutex
	var builders []string
	stubLocalBuild(t, func(_ context.Context, builder, _, _, _, _ string, _, _ io.Writer) error {
		mu.Lock()
		builders = append(builders, builder)
		mu.Unlock()
		return nil
	})

	cmd := newBuildCmd()
	cmd.SetArgs([]string{"--builder", "buildkit"})
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() = %v, want nil", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(builders) != 2 {
		t.Fatalf("recorded %d builders, want one per service: %v", len(builders), builders)
	}
	for _, builder := range builders {
		if builder != imageBuilderBuildkit {
			t.Fatalf("builder = %q, want %q", builder, imageBuilderBuildkit)
		}
	}
}

func TestBuildCmd_MultiService_ServiceFlagSelectsDependencyClosure(t *testing.T) {
	root := writeMultiServiceBuildProject(t, "myapp", map[string][]string{
		"api":    nil,
		"worker": {"api"},
		"extra":  nil,
	})
	chdirTo(t, root)
	setupMultiServiceBuildCmdTest(t)

	rec := &localBuildRecorder{}
	stubLocalBuild(t, func(_ context.Context, _, contextDir, imageName, _, dockerfile string, _, _ io.Writer) error {
		rec.record(contextDir, imageName, dockerfile)
		return nil
	})

	cmd := newBuildCmd()
	cmd.SetArgs([]string{"--service", "worker"})
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() = %v, want nil", err)
	}

	var got []string
	for _, c := range rec.snapshot() {
		got = append(got, filepath.Base(c.contextDir))
	}
	slices.Sort(got)
	want := []string{"api", "worker"}
	if !slices.Equal(got, want) {
		t.Fatalf("built services = %v, want %v (worker's dependency closure, not extra)", got, want)
	}

	// Separately: an unknown --service value is refused, naming the bad value.
	cmd2 := newBuildCmd()
	cmd2.SetArgs([]string{"--service", "nope"})
	cmd2.SetOut(io.Discard)
	cmd2.SetErr(io.Discard)
	err := cmd2.Execute()
	if err == nil {
		t.Fatal("Execute() with --service nope = nil, want an error")
	}
	if !strings.Contains(err.Error(), "nope") {
		t.Fatalf("error = %q, want it to name the unknown service %q", err.Error(), "nope")
	}
}

func TestBuildCmd_MultiService_ReportsEveryFailedService(t *testing.T) {
	root := writeMultiServiceBuildProject(t, "myapp", map[string][]string{
		"a": nil,
		"b": nil,
		"c": nil,
	})
	chdirTo(t, root)
	setupMultiServiceBuildCmdTest(t)

	stubLocalBuild(t, func(_ context.Context, _, contextDir, _, _, _ string, _, _ io.Writer) error {
		switch filepath.Base(contextDir) {
		case "a":
			return errors.New("boom in a")
		case "c":
			return errors.New("boom in c")
		default:
			return nil
		}
	})

	cmd := newBuildCmd()
	cmd.SetArgs([]string{})
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	err := cmd.Execute()
	if err == nil {
		t.Fatal("Execute() = nil, want an error reporting the failed services")
	}
	msg := err.Error()
	if !strings.Contains(msg, "service a:") {
		t.Errorf("error %q missing \"service a:\"", msg)
	}
	if !strings.Contains(msg, "service c:") {
		t.Errorf("error %q missing \"service c:\"", msg)
	}
	if strings.Contains(msg, "service b:") {
		t.Errorf("error %q must not report service b (it did not fail)", msg)
	}
}

func TestBuildCmd_MultiService_RejectsSingleImageFlags(t *testing.T) {
	root := writeMultiServiceBuildProject(t, "myapp", map[string][]string{
		"api":    nil,
		"worker": nil,
	})
	// A root-level Dockerfile so --dockerfile's earlier confinedDockerfilePath
	// existence check passes and the multi-service rejection (not "does not
	// exist") is what's actually exercised.
	if err := os.WriteFile(filepath.Join(root, "Dockerfile"), []byte("FROM scratch\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	chdirTo(t, root)
	setupMultiServiceBuildCmdTest(t)

	stubLocalBuild(t, func(context.Context, string, string, string, string, string, io.Writer, io.Writer) error {
		t.Errorf("local build path invoked; every case in this table must be rejected before any build starts")
		return nil
	})

	cases := []struct {
		name       string
		args       []string
		wantSubstr string
	}{
		{
			name:       "dockerfile",
			args:       []string{"--dockerfile", "Dockerfile"},
			wantSubstr: "not supported for multi-service projects",
		},
		{
			name:       "build-type",
			args:       []string{"--build-type", "docker"},
			wantSubstr: "not supported for multi-service projects",
		},
		{
			name:       "max-concurrency negative",
			args:       []string{"--max-concurrency", "-1"},
			wantSubstr: "--max-concurrency must be >= 0",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmd := newBuildCmd()
			cmd.SetArgs(tc.args)
			cmd.SetOut(io.Discard)
			cmd.SetErr(io.Discard)
			err := cmd.Execute()
			if err == nil {
				t.Fatalf("Execute(%v) = nil, want an error containing %q", tc.args, tc.wantSubstr)
			}
			if !strings.Contains(err.Error(), tc.wantSubstr) {
				t.Fatalf("Execute(%v) error = %q, want it to contain %q", tc.args, err.Error(), tc.wantSubstr)
			}
		})
	}
}

func TestBuildCmd_MultiService_StagefileContextsResolveThroughPlanner(t *testing.T) {
	root := writeMultiServiceBuildProject(t, "myapp", map[string][]string{
		"api": nil,
	})
	chdirTo(t, root)
	setupMultiServiceBuildCmdTest(t)

	origPlan := planResolveDockerfile
	t.Cleanup(func() { planResolveDockerfile = origPlan })
	planResolveDockerfile = func(string, string, bool, string, ...stagefile.Option) (string, error) {
		return "Dockerfile.generated", nil
	}

	rec := &localBuildRecorder{}
	stubLocalBuild(t, func(_ context.Context, _, contextDir, imageName, _, dockerfile string, _, _ io.Writer) error {
		rec.record(contextDir, imageName, dockerfile)
		return nil
	})

	cmd := newBuildCmd()
	cmd.SetArgs([]string{})
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() = %v, want nil", err)
	}

	calls := rec.snapshot()
	if len(calls) != 1 {
		t.Fatalf("recorded %d local build calls, want 1: %+v", len(calls), calls)
	}
	if calls[0].dockerfile != "Dockerfile.generated" {
		t.Fatalf("dockerfile = %q, want the planner-resolved %q (service contexts must flow through the same seam as `wendy run`)", calls[0].dockerfile, "Dockerfile.generated")
	}
}

func TestBuildCmd_ServiceFlagWithoutServicesMapErrors(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "Dockerfile"), []byte("FROM scratch\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A wendy.json with no services map: single-app config, not multi-service.
	const wendyJSON = `{"appId":"myapp","platform":"linux/arm64","version":"1.0.0"}`
	if err := os.WriteFile(filepath.Join(root, "wendy.json"), []byte(wendyJSON), 0o644); err != nil {
		t.Fatal(err)
	}
	chdirTo(t, root)
	setupMultiServiceBuildCmdTest(t)

	stubLocalBuild(t, func(context.Context, string, string, string, string, string, io.Writer, io.Writer) error {
		t.Errorf("local build path invoked; --service without a services map must be refused first")
		return nil
	})

	cmd := newBuildCmd()
	cmd.SetArgs([]string{"--service", "api"})
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	err := cmd.Execute()
	if err == nil {
		t.Fatal("Execute() = nil, want an error")
	}
	if !strings.Contains(err.Error(), "--service requires a wendy.json services map") {
		t.Fatalf("error = %q, want it to contain %q", err.Error(), "--service requires a wendy.json services map")
	}
}

// TestBuildServicesLocal_BoundedConcurrencyAndSingleBuildPerService covers
// WDY-2563's --max-concurrency for the local (no-push) build flavor: with 6
// services and maxConcurrency=2, buildServicesLocal must never run more than 2
// builds at once, and each service must be built exactly once.
func TestBuildServicesLocal_BoundedConcurrencyAndSingleBuildPerService(t *testing.T) {
	root, services := newServiceTree(t, 6)

	var mu sync.Mutex
	current, peak := 0, 0
	built := map[string]int{}
	stubLocalBuild(t, func(_ context.Context, _, _, imageName, _, _ string, _, _ io.Writer) error {
		mu.Lock()
		current++
		if current > peak {
			peak = current
		}
		built[imageName]++
		mu.Unlock()

		// Hold the slot briefly so overlapping calls are actually observable;
		// the semaphore in buildServicesParallelCore is what bounds peak, not
		// this sleep.
		time.Sleep(20 * time.Millisecond)

		mu.Lock()
		current--
		mu.Unlock()
		return nil
	})

	failed, err := buildServicesLocal(context.Background(), root, "app", services, "linux/arm64", "docker", "", 2, true)
	if err != nil {
		t.Fatalf("buildServicesLocal: %v", err)
	}
	if len(failed) != 0 {
		t.Fatalf("unexpected build failures: %v", sortedServiceErrorKeys(failed))
	}

	mu.Lock()
	defer mu.Unlock()
	if peak > 2 {
		t.Fatalf("peak concurrent builds = %d, want <= 2 (maxConcurrency)", peak)
	}
	if len(built) != len(services) {
		t.Fatalf("built %d distinct images, want %d: %v", len(built), len(services), built)
	}
	for name, count := range built {
		if count != 1 {
			t.Errorf("image %s built %d times, want exactly 1", name, count)
		}
	}
}

// TestBuildServicesLocal_CancellationReturnsNilMapAndStopsQueued mirrors
// TestBuildServicesParallelCancellationStopsActiveAndQueuedBuilds
// (multibuild_test.go) through buildServicesLocal: one Ctrl-C must stop the
// active build and never start any queued one.
func TestBuildServicesLocal_CancellationReturnsNilMapAndStopsQueued(t *testing.T) {
	root, services := newServiceTree(t, 4)

	started := make(chan struct{}, len(services))
	var mu sync.Mutex
	startCount := 0
	stubLocalBuild(t, func(ctx context.Context, _, _, _, _, _ string, _, _ io.Writer) error {
		mu.Lock()
		startCount++
		mu.Unlock()
		started <- struct{}{}
		<-ctx.Done()
		return ctx.Err()
	})

	ctx, cancel := context.WithCancel(context.Background())
	type outcome struct {
		failed map[string]error
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		failed, err := buildServicesLocal(ctx, root, "app", services, "linux/arm64", "docker", "", 1, true)
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
		t.Fatal("buildServicesLocal did not return after cancellation")
	}

	mu.Lock()
	defer mu.Unlock()
	if startCount != 1 {
		t.Fatalf("started %d service builders after cancellation, want exactly the active one", startCount)
	}
}

func TestBuildServiceImageLocally_MissingBuildFileNamesContext(t *testing.T) {
	contextDir := t.TempDir()
	err := buildServiceImageLocally(context.Background(), "", contextDir, "app-api:latest", "linux/arm64", "", io.Discard, io.Discard)
	if err == nil {
		t.Fatal("buildServiceImageLocally with dockerfile=\"\" = nil, want an error")
	}
	if !strings.Contains(err.Error(), contextDir) {
		t.Errorf("error %q does not name the context dir %q", err.Error(), contextDir)
	}
	if !strings.Contains(err.Error(), "no container build file found") {
		t.Errorf("error %q missing \"no container build file found\"", err.Error())
	}
}

func TestBuildServiceImageLocally_DockerBuildxLoadArgs(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("stub builder uses the POSIX `true` command")
	}

	origCmd := imageBuilderCommandContext
	origGOOS := imageBuilderHostGOOS
	t.Cleanup(func() {
		imageBuilderCommandContext = origCmd
		imageBuilderHostGOOS = origGOOS
	})
	// linux (not darwin/arm64) disables the Apple Container auto-attempt so
	// the plain docker buildx path below is what actually runs.
	imageBuilderHostGOOS = func() string { return "linux" }

	var gotProgram string
	var gotArgs []string
	var gotCmd *exec.Cmd
	imageBuilderCommandContext = func(ctx context.Context, name string, arg ...string) *exec.Cmd {
		gotProgram = name
		gotArgs = append([]string(nil), arg...)
		gotCmd = exec.Command("true")
		return gotCmd
	}

	contextDir := t.TempDir()
	err := buildServiceImageLocally(context.Background(), "", contextDir, "foo-api:latest", "linux/arm64", "Dockerfile", io.Discard, io.Discard)
	if err != nil {
		t.Fatalf("buildServiceImageLocally: %v", err)
	}

	if gotProgram != "docker" {
		t.Fatalf("program = %q, want %q", gotProgram, "docker")
	}
	want := []string{"buildx", "build", "--platform", "linux/arm64", "--progress", "plain", "-f", "Dockerfile", "-t", "foo-api:latest", "--load", "."}
	if !slices.Equal(gotArgs, want) {
		t.Fatalf("args = %v, want %v", gotArgs, want)
	}
	if gotCmd == nil {
		t.Fatal("imageBuilderCommandContext was never called")
	}
	if gotCmd.Dir != contextDir {
		t.Fatalf("cmd.Dir = %q, want %q", gotCmd.Dir, contextDir)
	}
}
