package container

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/services"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

type staleNANMonitorClient struct {
	*fakeContainerd
	stale             []string
	mu                sync.Mutex
	stops             []string
	scanCount         int
	clearOnSecondScan bool
	onStop            func()
}

func (f *staleNANMonitorClient) StaleNANSocketContainers(context.Context) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.scanCount++
	if f.clearOnSecondScan && f.scanCount >= 2 {
		return nil, nil
	}
	return f.stale, nil
}

func (f *staleNANMonitorClient) StopContainer(_ context.Context, name string) error {
	f.mu.Lock()
	f.stops = append(f.stops, name)
	f.mu.Unlock()
	if f.onStop != nil {
		f.onStop()
	}
	return nil
}

func TestMonitorRefreshesOnlyStaleRunningNANApp(t *testing.T) {
	f := &staleNANMonitorClient{
		fakeContainerd: &fakeContainerd{
			containers: []*agentpb.AppContainer{{AppName: "camera", RunningState: agentpb.AppRunningState_RUNNING}},
			started:    make(chan string, 1),
		},
		stale: []string{"camera"},
	}
	m := newMonitorWithClient(f)
	m.Register("camera", RestartUnlessStopped, 0)
	m.checkContainers(context.Background())
	select {
	case name := <-f.started:
		if name != "camera" {
			t.Fatalf("started %q", name)
		}
	case <-time.After(time.Second):
		t.Fatal("stale NAN app was not restarted")
	}
	f.mu.Lock()
	stops := append([]string(nil), f.stops...)
	f.mu.Unlock()
	if len(stops) != 1 || stops[0] != "camera" {
		t.Fatalf("graceful stop calls = %v", stops)
	}
	m.mu.Lock()
	failures := m.states["camera"].FailureCount
	m.mu.Unlock()
	if failures != 0 {
		t.Fatalf("socket refresh counted as app crash: %d", failures)
	}
}

func TestMonitorDoesNotRefreshExplicitlyStoppedNANApp(t *testing.T) {
	f := &staleNANMonitorClient{
		fakeContainerd: &fakeContainerd{containers: []*agentpb.AppContainer{{AppName: "camera", RunningState: agentpb.AppRunningState_RUNNING}}},
		stale:          []string{"camera"},
	}
	m := newMonitorWithClient(f)
	m.Register("camera", RestartUnlessStopped, 0)
	m.MarkExplicitStop("camera")
	m.restartStaleNAN(context.Background(), "camera")
	f.mu.Lock()
	stops := len(f.stops)
	f.mu.Unlock()
	if stops != 0 || len(f.startCallsSnapshot()) != 0 {
		t.Fatal("explicitly stopped NAN app was restarted")
	}
}

func TestMonitorSkipsUnmonitoredAndAlreadyRefreshedNANApps(t *testing.T) {
	f := &staleNANMonitorClient{
		fakeContainerd: &fakeContainerd{containers: []*agentpb.AppContainer{{AppName: "camera", RunningState: agentpb.AppRunningState_RUNNING}}},
		stale:          []string{"camera"},
	}
	m := newMonitorWithClient(f)
	m.checkContainers(context.Background()) // restart policy "no" has no registration
	if f.scanCount != 1 {
		t.Fatalf("stale scan count = %d", f.scanCount)
	}
	m.Register("camera", RestartUnlessStopped, 0)
	f.clearOnSecondScan = true
	m.restartStaleNAN(context.Background(), "camera")
	// The task became healthy between scheduling and execution.
	f.mu.Lock()
	stops := len(f.stops)
	f.mu.Unlock()
	if stops != 0 || len(f.startCallsSnapshot()) != 0 {
		t.Fatal("healthy or unmonitored NAN app was restarted")
	}
}

func TestMonitorDoesNotReviveNANAppStoppedDuringRefresh(t *testing.T) {
	f := &staleNANMonitorClient{fakeContainerd: &fakeContainerd{}, stale: []string{"camera"}}
	m := newMonitorWithClient(f)
	m.Register("camera", RestartUnlessStopped, 0)
	f.onStop = func() { m.MarkExplicitStop("camera") }
	m.restartStaleNAN(context.Background(), "camera")
	if len(f.startCallsSnapshot()) != 0 {
		t.Fatal("user-stopped NAN app was revived")
	}
}

type staleNANGroupClient struct {
	*fakeContainerdClient
	stale     []string
	restarted chan string
}

func (f *staleNANGroupClient) StaleNANSocketContainers(context.Context) ([]string, error) {
	return f.stale, nil
}

func (f *staleNANGroupClient) RestartGroup(_ context.Context, appID string) (map[string]<-chan services.ContainerOutput, error) {
	f.restarted <- appID
	return nil, nil
}

func TestMonitorUsesGroupRestartForStaleNANMember(t *testing.T) {
	base := &fakeContainerd{containers: []*agentpb.AppContainer{{
		AppName: "camera", RunningState: agentpb.AppRunningState_RUNNING,
		Services: []*agentpb.ServiceEntry{{Name: "radio", RunningState: agentpb.AppRunningState_RUNNING}, {Name: "ui", RunningState: agentpb.AppRunningState_RUNNING}},
	}}}
	f := &staleNANGroupClient{
		fakeContainerdClient: &fakeContainerdClient{ContainerdClient: base, groupOf: map[string]string{"camera_radio": "camera", "camera_ui": "camera"}},
		stale:                []string{"camera_radio"}, restarted: make(chan string, 1),
	}
	m := newMonitorWithClient(f)
	m.Register("camera_radio", RestartUnlessStopped, 0)
	m.Register("camera_ui", RestartUnlessStopped, 0)
	m.checkContainers(context.Background())
	select {
	case appID := <-f.restarted:
		if appID != "camera" {
			t.Fatalf("restarted group %q", appID)
		}
	case <-time.After(time.Second):
		t.Fatal("stale member did not restart whole group")
	}
	if calls := base.startCallsSnapshot(); len(calls) != 0 {
		t.Fatalf("group member restarted separately: %v", calls)
	}
}
