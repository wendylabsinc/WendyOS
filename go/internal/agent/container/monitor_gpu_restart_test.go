package container

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

// gpuFakeContainerd is a fakeContainerd that also answers the
// services.GPUDeviceReporter capability, reporting the named containers as
// GPU-entitled.
type gpuFakeContainerd struct {
	fakeContainerd
	gpu    map[string]bool
	calls  int
	err    error
	lookup func(string) (bool, error)
}

func (f *gpuFakeContainerd) HasGPUEntitlement(_ context.Context, appName string) (bool, error) {
	f.calls++
	if f.lookup != nil {
		return f.lookup(appName)
	}
	return f.gpu[appName], f.err
}

// A GPU container must not be restarted the instant it is seen down, however
// long it had been running. This is the case the level-based ladder cannot
// cover: LastRestart is hours old for a long-lived app, so every level delay is
// already satisfied and the old code restarted immediately.
func TestPlanRestarts_GPUContainerHeldBackAfterCrash(t *testing.T) {
	fake := &gpuFakeContainerd{gpu: map[string]bool{backoffTestApp: true}}
	m := newMonitorWithClient(fake)
	now := time.Unix(1700000000, 0)
	m.now = func() time.Time { return now }
	m.Register(backoffTestApp, RestartUnlessStopped, 0)
	m.resolveGPUEntitlements(context.Background())

	// It had been up for hours before dying, so the ladder imposes nothing.
	m.mu.Lock()
	m.states[backoffTestApp].LastRestart = now.Add(-4 * time.Hour)
	m.mu.Unlock()

	stopped := []*agentpb.AppContainer{{
		AppName:      backoffTestApp,
		RunningState: agentpb.AppRunningState_STOPPED,
	}}

	if got := m.planRestarts(stopped); len(got) != 0 {
		t.Fatalf("planRestarts = %v on the tick that first sees a GPU app down; want none (driver still holding the dead context)", got)
	}

	// Still inside the window.
	now = now.Add(gpuMinRestartDelay - time.Second)
	if got := m.planRestarts(stopped); len(got) != 0 {
		t.Fatalf("planRestarts = %v at %v after death; want none before %v", got, gpuMinRestartDelay-time.Second, gpuMinRestartDelay)
	}

	// Past it.
	now = now.Add(2 * time.Second)
	got := m.planRestarts(stopped)
	if len(got) != 1 || got[0] != backoffTestApp {
		t.Fatalf("planRestarts = %v after %v down; want [%s]", got, gpuMinRestartDelay, backoffTestApp)
	}
}

// The floor is GPU-specific: a non-GPU app keeps its immediate first restart,
// which unattended devices depend on for prompt recovery.
func TestPlanRestarts_NonGPUContainerStillRestartsImmediately(t *testing.T) {
	fake := &gpuFakeContainerd{gpu: map[string]bool{}}
	m := newMonitorWithClient(fake)
	now := time.Unix(1700000000, 0)
	m.now = func() time.Time { return now }
	m.Register(backoffTestApp, RestartUnlessStopped, 0)
	m.resolveGPUEntitlements(context.Background())

	stopped := []*agentpb.AppContainer{{
		AppName:      backoffTestApp,
		RunningState: agentpb.AppRunningState_STOPPED,
	}}

	got := m.planRestarts(stopped)
	if len(got) != 1 || got[0] != backoffTestApp {
		t.Fatalf("planRestarts = %v for a non-GPU app on its first tick down; want [%s]", got, backoffTestApp)
	}
}

// A client without the capability must behave as it always has.
func TestPlanRestarts_ClientWithoutGPUCapability(t *testing.T) {
	m := newMonitorWithClient(&fakeContainerd{})
	now := time.Unix(1700000000, 0)
	m.now = func() time.Time { return now }
	m.Register(backoffTestApp, RestartUnlessStopped, 0)
	m.resolveGPUEntitlements(context.Background())

	stopped := []*agentpb.AppContainer{{
		AppName:      backoffTestApp,
		RunningState: agentpb.AppRunningState_STOPPED,
	}}
	if got := m.planRestarts(stopped); len(got) != 1 {
		t.Fatalf("planRestarts = %v with a client that is not a GPUDeviceReporter; want [%s]", got, backoffTestApp)
	}
}

// Entitlements are fixed for a container's lifetime, so the lookup must happen
// once per registration rather than on every tick.
func TestResolveGPUEntitlements_CachesPerContainer(t *testing.T) {
	fake := &gpuFakeContainerd{gpu: map[string]bool{backoffTestApp: true}}
	m := newMonitorWithClient(fake)
	m.Register(backoffTestApp, RestartUnlessStopped, 0)

	for range 5 {
		m.resolveGPUEntitlements(context.Background())
	}
	if fake.calls != 1 {
		t.Errorf("HasGPUEntitlement called %d times across 5 passes; want 1", fake.calls)
	}

	// A redeploy re-registers under the same name and may change entitlements,
	// so unregistering must drop the cached answer.
	m.Unregister(backoffTestApp)
	m.Register(backoffTestApp, RestartUnlessStopped, 0)
	m.resolveGPUEntitlements(context.Background())
	if fake.calls != 2 {
		t.Errorf("HasGPUEntitlement called %d times after re-registration; want 2 (cache dropped on Unregister)", fake.calls)
	}
}

// Coming back up must clear the down clock, or the next crash inherits a stale
// DownSince and is restarted immediately.
func TestPlanRestarts_DownSinceClearedWhenRunning(t *testing.T) {
	fake := &gpuFakeContainerd{gpu: map[string]bool{backoffTestApp: true}}
	m := newMonitorWithClient(fake)
	now := time.Unix(1700000000, 0)
	m.now = func() time.Time { return now }
	m.Register(backoffTestApp, RestartUnlessStopped, 0)
	m.resolveGPUEntitlements(context.Background())

	stopped := []*agentpb.AppContainer{{AppName: backoffTestApp, RunningState: agentpb.AppRunningState_STOPPED}}
	running := []*agentpb.AppContainer{{AppName: backoffTestApp, RunningState: agentpb.AppRunningState_RUNNING}}

	m.planRestarts(stopped) // marks DownSince
	now = now.Add(time.Hour)
	m.planRestarts(running) // back up: clock must reset
	now = now.Add(time.Hour)

	if got := m.planRestarts(stopped); len(got) != 0 {
		t.Fatalf("planRestarts = %v on the first tick of a fresh crash; want none (DownSince must have been cleared while running)", got)
	}
}

func TestResolveGPUEntitlements_DirectReregistration(t *testing.T) {
	fake := &gpuFakeContainerd{gpu: map[string]bool{"app": false}}
	m := newMonitorWithClient(fake)
	m.Register("app", RestartUnlessStopped, 0)
	m.resolveGPUEntitlements(context.Background())
	fake.gpu["app"] = true
	m.Register("app", RestartUnlessStopped, 0)
	m.resolveGPUEntitlements(context.Background())
	if !m.gpuEntitled["app"] {
		t.Fatal("new registration retains old non-GPU classification")
	}
}

func TestResolveGPUEntitlements_IgnoresPreviousRegistrationLookup(t *testing.T) {
	fake := &gpuFakeContainerd{gpu: map[string]bool{"app": true}}
	m := newMonitorWithClient(fake)
	m.Register("app", RestartUnlessStopped, 0)
	fake.lookup = func(string) (bool, error) {
		m.Unregister("app")
		m.Register("app", RestartUnlessStopped, 0)
		return false, nil
	}
	m.resolveGPUEntitlements(context.Background())
	if _, cached := m.gpuEntitled["app"]; cached {
		t.Fatal("old lookup applied to new registration")
	}
	fake.lookup = nil
	m.resolveGPUEntitlements(context.Background())
	if !m.gpuEntitled["app"] {
		t.Fatal("new registration was not resolved")
	}
}

func TestResolveGPUEntitlements_RetriesLookupFailure(t *testing.T) {
	fake := &gpuFakeContainerd{gpu: map[string]bool{"app": true}, err: errors.New("temporarily unavailable")}
	m := newMonitorWithClient(fake)
	m.Register("app", RestartUnlessStopped, 0)
	m.resolveGPUEntitlements(context.Background())
	if _, cached := m.gpuEntitled["app"]; cached {
		t.Fatal("error cached as non-GPU")
	}
	fake.err = nil
	m.resolveGPUEntitlements(context.Background())
	if !m.gpuEntitled["app"] || fake.calls != 2 {
		t.Fatal("lookup was not retried")
	}
}
