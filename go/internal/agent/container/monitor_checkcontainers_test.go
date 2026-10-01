package container

import (
	"context"
	"io"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/wendylabsinc/wendy/go/internal/agent/services"
	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

// fakeContainerd is a minimal services.ContainerdClient for exercising
// checkContainers. ListContainers returns a fixed snapshot and StartContainer
// records the identities the monitor tries to restart.
type fakeContainerd struct {
	containers     []*agentpb.AppContainer
	bootContainers []services.BootContainer

	mu            sync.Mutex
	startCalls    []string
	started       chan string // signalled (buffered) on each StartContainer
	stoppedByUser map[string]bool
	migrateCalls  int
	rebuildCalls  int
	probeCalls    int
}

func (f *fakeContainerd) ListContainers(ctx context.Context) ([]*agentpb.AppContainer, error) {
	return f.containers, nil
}

func (f *fakeContainerd) ListBootContainers(ctx context.Context) ([]services.BootContainer, error) {
	return f.bootContainers, nil
}

func (f *fakeContainerd) SetStoppedByUser(ctx context.Context, containerID string, stopped bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.stoppedByUser == nil {
		f.stoppedByUser = map[string]bool{}
	}
	f.stoppedByUser[containerID] = stopped
	return nil
}

func (f *fakeContainerd) MigrateStoppedByUserOnce(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.migrateCalls++
	return nil
}

func (f *fakeContainerd) RebuildAppStateCaches(ctx context.Context) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rebuildCalls++
}

func (f *fakeContainerd) WarnPubliclyExposedPorts(ctx context.Context) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.probeCalls++
}

func (f *fakeContainerd) StartContainer(ctx context.Context, appName, _ string, _ *agentpb.RestartPolicy) (<-chan services.ContainerOutput, error) {
	f.mu.Lock()
	f.startCalls = append(f.startCalls, appName)
	f.mu.Unlock()
	if f.started != nil {
		f.started <- appName
	}
	ch := make(chan services.ContainerOutput)
	close(ch)
	return ch, nil
}

func (f *fakeContainerd) startCallsSnapshot() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.startCalls...)
}

// Remaining ContainerdClient methods are unused by checkContainers.
func (f *fakeContainerd) ListLayers(ctx context.Context) ([]*agentpb.LayerHeader, error) {
	return nil, nil
}
func (f *fakeContainerd) WriteLayer(ctx context.Context, digest string, reader io.Reader, size int64) error {
	return nil
}
func (f *fakeContainerd) AssembleImage(ctx context.Context, imageName string, layers []*agentpb.RunContainerLayerHeader, imageConfig []byte) error {
	return nil
}
func (f *fakeContainerd) CreateContainer(ctx context.Context, req *agentpb.CreateContainerRequest, appCfg *appconfig.AppConfig) error {
	return nil
}
func (f *fakeContainerd) CreateContainerWithProgress(ctx context.Context, req *agentpb.CreateContainerRequest, appCfg *appconfig.AppConfig, onProgress services.ProgressFunc) error {
	return nil
}
func (f *fakeContainerd) StartContainerWithStdin(ctx context.Context, appName string, stdin io.Reader, postStartAgentCommand string, restartPolicy *agentpb.RestartPolicy) (<-chan services.ContainerOutput, error) {
	return f.StartContainer(ctx, appName, postStartAgentCommand, restartPolicy)
}
func (f *fakeContainerd) StopContainer(ctx context.Context, appName string) error { return nil }
func (f *fakeContainerd) DeleteContainer(ctx context.Context, appName string, deleteImage bool) error {
	return nil
}
func (f *fakeContainerd) ContainerIDsForApp(ctx context.Context, appID string) ([]string, error) {
	return nil, nil
}
func (f *fakeContainerd) ResolveAppContainerIDs(ctx context.Context, name string) ([]string, error) {
	return nil, nil
}
func (f *fakeContainerd) GetContainerStats(ctx context.Context) ([]*agentpb.ContainerStats, error) {
	return nil, nil
}
func (f *fakeContainerd) GetResourceStats(context.Context) ([]*agentpb.ResourceContainerStats, error) {
	return nil, nil
}
func (f *fakeContainerd) GetListeningPorts(context.Context, string) ([]*agentpb.PortEntry, error) {
	return nil, nil
}
func (f *fakeContainerd) GetContainerMetrics(ctx context.Context, appName string) (services.ContainerMetrics, error) {
	return services.ContainerMetrics{}, nil
}
func (f *fakeContainerd) GetContainerMCPPort(ctx context.Context, appName string) (uint32, error) {
	return 0, nil
}
func (f *fakeContainerd) GetContainerRestartPolicyLabel(ctx context.Context, appName string) (string, error) {
	return "", nil
}
func (f *fakeContainerd) AppDeclaredVolumes(ctx context.Context) (map[string][]string, error) {
	return nil, nil
}

func (f *fakeContainerd) MissingChunks(_ context.Context, hashes [][32]byte) ([][32]byte, error) {
	return hashes, nil
}

func (f *fakeContainerd) PresentLayers(_ context.Context, _ []string) (map[string]int64, error) {
	return nil, nil
}

func (f *fakeContainerd) StageChunk(_ context.Context, _ [32]byte, _ []byte) error {
	return nil
}

func (f *fakeContainerd) AssembleLayerFromChunks(_ context.Context, _ string, _ [][32]byte) error {
	return nil
}

func newMonitorWithClient(c services.ContainerdClient) *ContainerMonitor {
	return NewContainerMonitor(zap.NewNop(), c, nil, time.Second)
}

// A healthy single-service services-map app is monitored under its
// "{appID}_{serviceName}" container name. ListContainers reports the bare appID
// as AppName but exposes the service via Services, so the monitor must NOT treat
// the running service as stopped and restart it (WDY-1552).
func TestCheckContainers_SingleServiceMapApp_NotRestarted(t *testing.T) {
	fake := &fakeContainerd{
		containers: []*agentpb.AppContainer{{
			AppName:      "myapp",
			RunningState: agentpb.AppRunningState_RUNNING,
			Services: []*agentpb.ServiceEntry{
				{Name: "web", RunningState: agentpb.AppRunningState_RUNNING},
			},
		}},
	}
	m := newMonitorWithClient(fake)
	m.Register("myapp_web", RestartUnlessStopped, 0)

	m.checkContainers(context.Background())

	if calls := fake.startCallsSnapshot(); len(calls) != 0 {
		t.Fatalf("StartContainer called for healthy service: %v", calls)
	}
	m.mu.Lock()
	fc := m.states["myapp_web"].FailureCount
	m.mu.Unlock()
	if fc != 0 {
		t.Fatalf("FailureCount = %d, want 0 (no spurious restart)", fc)
	}
}

// Single-service apps deploy as a BARE-named container (AppConfig.ContainerName
// returns the appID when ServiceName is empty), so their monitor state is
// registered under the bare appID — while ListContainers still reports the
// service via Services. The monitor must recognize the bare registration as
// running too, or it force-restarts the healthy app every tick.
func TestCheckContainers_SingleServiceMapApp_BareRegistration_NotRestarted(t *testing.T) {
	fake := &fakeContainerd{
		containers: []*agentpb.AppContainer{{
			AppName:      "myapp",
			RunningState: agentpb.AppRunningState_RUNNING,
			Services: []*agentpb.ServiceEntry{
				{Name: "web", RunningState: agentpb.AppRunningState_RUNNING},
			},
		}},
	}
	m := newMonitorWithClient(fake)
	m.Register("myapp", RestartUnlessStopped, 0)

	m.checkContainers(context.Background())

	if calls := fake.startCallsSnapshot(); len(calls) != 0 {
		t.Fatalf("StartContainer called for healthy service: %v", calls)
	}
	m.mu.Lock()
	fc := m.states["myapp"].FailureCount
	m.mu.Unlock()
	if fc != 0 {
		t.Fatalf("FailureCount = %d, want 0 (no spurious restart)", fc)
	}
}

func TestReconcileBootContainers_StartsStoppedApp(t *testing.T) {
	fake := &fakeContainerd{
		started:        make(chan string, 1),
		bootContainers: []services.BootContainer{{Name: "boot-app", RestartPolicy: "unless-stopped"}},
		containers: []*agentpb.AppContainer{{
			AppName:      "boot-app",
			RunningState: agentpb.AppRunningState_STOPPED,
		}},
	}
	m := newMonitorWithClient(fake)

	m.ReconcileBootContainers(context.Background())

	select {
	case got := <-fake.started:
		if got != "boot-app" {
			t.Fatalf("started %q, want boot-app", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("expected ReconcileBootContainers to start boot-app, got none")
	}
}

// TestReconcileBootContainers_RunsMigration verifies the one-time stopped-by-user
// back-fill is invoked as part of boot reconcile (before listing eligible apps),
// so an upgrade can't resurrect apps the user had already stopped.
func TestReconcileBootContainers_RunsMigration(t *testing.T) {
	fake := &fakeContainerd{started: make(chan string, 1)}
	m := newMonitorWithClient(fake)

	m.ReconcileBootContainers(context.Background())

	fake.mu.Lock()
	calls := fake.migrateCalls
	fake.mu.Unlock()
	if calls != 1 {
		t.Fatalf("MigrateStoppedByUserOnce called %d times, want 1", calls)
	}
}

func TestReconcileBootContainers_RebuildsCaches(t *testing.T) {
	f := &fakeContainerd{}
	m := newMonitorWithClient(f)
	m.ReconcileBootContainers(context.Background())

	f.mu.Lock()
	defer f.mu.Unlock()
	if f.rebuildCalls != 1 {
		t.Fatalf("RebuildAppStateCaches called %d times, want 1", f.rebuildCalls)
	}
}

func TestReconcileBootContainers_NothingToDo(t *testing.T) {
	fake := &fakeContainerd{
		started: make(chan string, 1),
		containers: []*agentpb.AppContainer{{
			AppName:      "plain-app",
			RunningState: agentpb.AppRunningState_STOPPED,
		}},
	}
	m := newMonitorWithClient(fake)

	m.ReconcileBootContainers(context.Background()) // no bootContainers

	select {
	case got := <-fake.started:
		t.Fatalf("started %q; nothing should start when no apps are eligible", got)
	case <-time.After(200 * time.Millisecond):
		// expected: nothing started
	}
}

// A genuinely stopped service in a services-map app is still restarted under its
// per-service container name.
func TestCheckContainers_SingleServiceMapApp_RestartsWhenDown(t *testing.T) {
	fake := &fakeContainerd{
		started: make(chan string, 1),
		containers: []*agentpb.AppContainer{{
			AppName:      "myapp",
			RunningState: agentpb.AppRunningState_STOPPED,
			Services: []*agentpb.ServiceEntry{
				{Name: "web", RunningState: agentpb.AppRunningState_STOPPED},
			},
		}},
	}
	m := newMonitorWithClient(fake)
	m.Register("myapp_web", RestartUnlessStopped, 0) // LastRestart zero => backoff already elapsed

	m.checkContainers(context.Background())

	select {
	case got := <-fake.started:
		if got != "myapp_web" {
			t.Fatalf("restarted %q, want %q", got, "myapp_web")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("expected StartContainer(myapp_web), got none")
	}
}

// Legacy single-container apps (no Services) continue to match on the bare appID
// and are not restarted while running.
func TestCheckContainers_LegacySingleContainer_NotRestarted(t *testing.T) {
	fake := &fakeContainerd{
		containers: []*agentpb.AppContainer{{
			AppName:      "legacyapp",
			RunningState: agentpb.AppRunningState_RUNNING,
		}},
	}
	m := newMonitorWithClient(fake)
	m.Register("legacyapp", RestartUnlessStopped, 0)

	m.checkContainers(context.Background())

	if calls := fake.startCallsSnapshot(); len(calls) != 0 {
		t.Fatalf("StartContainer called for healthy legacy app: %v", calls)
	}
}

// TestCheckContainers_SuppressedContainer_NotRestarted is the full
// checkContainers-round-trip regression guard for F2 (see
// TestMonitorSuppressSkipsRestart in monitor_test.go for the narrower
// planRestarts-only version). While a Suppress handle is held, a tick must
// not call StartContainer for that name even though it is stopped and its
// policy would otherwise restart it; once resumed, the very same tick
// conditions restart it normally.
func TestCheckContainers_SuppressedContainer_NotRestarted(t *testing.T) {
	fake := &fakeContainerd{
		started: make(chan string, 1),
		containers: []*agentpb.AppContainer{{
			AppName:      "crashloop-app",
			RunningState: agentpb.AppRunningState_STOPPED,
		}},
	}
	m := newMonitorWithClient(fake)
	m.Register("crashloop-app", RestartUnlessStopped, 0)

	resume := m.Suppress("crashloop-app")

	m.checkContainers(context.Background())

	select {
	case got := <-fake.started:
		t.Fatalf("StartContainer called for suppressed container: %q", got)
	case <-time.After(200 * time.Millisecond):
		// expected: nothing started while suppressed
	}

	resume()
	m.checkContainers(context.Background())

	select {
	case got := <-fake.started:
		if got != "crashloop-app" {
			t.Fatalf("restarted %q, want %q", got, "crashloop-app")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("expected StartContainer(crashloop-app) after resume, got none")
	}
}

// TestRestartSingle_SkipsWhenSuppressedBeforeStart is the regression guard for
// the in-flight-resurrection gap (F2 round 1 follow-up, finding 2): a
// restartSingle goroutine can be dispatched by a tick that observed the
// container down and unsuppressed, and then have a Suppress land before it
// actually reaches the StartContainer call — there is no blocking operation
// between goroutine dispatch and that call to naturally serialize the two, so
// the only way to close the window deterministically is a re-check
// immediately before the call, which is what this asserts. The test invokes
// restartSingle directly rather than via `go` + a synchronization gate:
// restartSingle's guard runs synchronously as its very first statement with
// nothing blocking beforehand, so there is no I/O point available to
// interleave two goroutines on deterministically — calling it after Suppress
// exercises the exact same code path (the guard check) that a genuinely
// raced goroutine would hit, without relying on scheduler timing.
func TestRestartSingle_SkipsWhenSuppressedBeforeStart(t *testing.T) {
	fake := &fakeContainerd{started: make(chan string, 1)}
	m := newMonitorWithClient(fake)
	m.Register("crashloop-app", RestartUnlessStopped, 0)

	resume := m.Suppress("crashloop-app")
	defer resume()

	m.restartSingle(context.Background(), "crashloop-app")

	if calls := fake.startCallsSnapshot(); len(calls) != 0 {
		t.Fatalf("StartContainer called for suppressed container: %v", calls)
	}
}

// TestRestartSingle_SkipsWhenExplicitlyStoppedBeforeStart mirrors the above
// for MarkExplicitStop, the other piece of state a stop operation sets that
// an already-scheduled restartSingle must honor.
func TestRestartSingle_SkipsWhenExplicitlyStoppedBeforeStart(t *testing.T) {
	fake := &fakeContainerd{started: make(chan string, 1)}
	m := newMonitorWithClient(fake)
	m.Register("crashloop-app", RestartUnlessStopped, 0)
	m.MarkExplicitStop("crashloop-app")

	m.restartSingle(context.Background(), "crashloop-app")

	if calls := fake.startCallsSnapshot(); len(calls) != 0 {
		t.Fatalf("StartContainer called for an explicitly-stopped container: %v", calls)
	}
}

// TestRestartSingle_StartsWhenNotBlocked is the regression control for the two
// tests above: the new re-check must not over-fire and block a legitimate,
// unsuppressed restart.
func TestRestartSingle_StartsWhenNotBlocked(t *testing.T) {
	fake := &fakeContainerd{started: make(chan string, 1)}
	m := newMonitorWithClient(fake)
	m.Register("crashloop-app", RestartUnlessStopped, 0)

	m.restartSingle(context.Background(), "crashloop-app")

	select {
	case got := <-fake.started:
		if got != "crashloop-app" {
			t.Fatalf("started %q, want crashloop-app", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("expected StartContainer(crashloop-app), got none")
	}
}

// TestCheckContainers_ExplicitlyStoppedAlwaysPolicyStaysStopped is the full
// round-trip guard that a manual stop wins over RestartAlways.
func TestCheckContainers_ExplicitlyStoppedAlwaysPolicyStaysStopped(t *testing.T) {
	fake := &fakeContainerd{
		started: make(chan string, 1),
		containers: []*agentpb.AppContainer{{
			AppName:      "always-app",
			RunningState: agentpb.AppRunningState_STOPPED,
		}},
	}
	m := newMonitorWithClient(fake)
	m.Register("always-app", RestartAlways, 0)
	m.MarkExplicitStop("always-app")

	m.checkContainers(context.Background())

	select {
	case got := <-fake.started:
		t.Fatalf("unexpectedly started %q after explicit stop", got)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestProbeExposedPortsInvokesProber(t *testing.T) {
	f := &fakeContainerd{}
	m := newMonitorWithClient(f)
	m.probeExposedPorts(context.Background())

	f.mu.Lock()
	defer f.mu.Unlock()
	if f.probeCalls != 1 {
		t.Fatalf("WarnPubliclyExposedPorts called %d times, want 1", f.probeCalls)
	}
}

func TestRestartSingle_DoesNotResurrectRetiredAlias(t *testing.T) {
	fake := &fakeContainerd{started: make(chan string, 1)}
	m := newMonitorWithClient(fake)
	m.Register("demo", RestartUnlessStopped, 0)
	m.Unregister("demo")
	m.Register("demo_relay", RestartUnlessStopped, 0)
	m.MarkExplicitStop("demo_relay")
	// Model old alias work already scheduled before canonical registration.
	m.restartSingle(context.Background(), "demo")
	m.restartSingle(context.Background(), "demo_relay")
	if calls := fake.startCallsSnapshot(); len(calls) != 0 {
		t.Fatalf("stopped app resurrected: %v", calls)
	}
}
