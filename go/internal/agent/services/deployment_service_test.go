package services

import (
	"context"
	"errors"
	"io"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type fakeDeploymentTransaction struct {
	mu                                  sync.Mutex
	calls                               []string
	previous                            string
	previousWasRunning                  bool
	activateErr, commitErr, rollbackErr error
	closed                              chan struct{}
	once                                sync.Once
}

func (f *fakeDeploymentTransaction) record(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, name)
}
func (f *fakeDeploymentTransaction) Revision() string         { return "candidate-revision" }
func (f *fakeDeploymentTransaction) PreviousRevision() string { return f.previous }
func (f *fakeDeploymentTransaction) PreviousWasRunning() bool { return f.previousWasRunning }
func (f *fakeDeploymentTransaction) Activate(context.Context) error {
	f.record("activate")
	return f.activateErr
}
func (f *fakeDeploymentTransaction) Commit(context.Context) error {
	f.record("commit")
	return f.commitErr
}
func (f *fakeDeploymentTransaction) Rollback(context.Context) (<-chan ContainerOutput, error) {
	f.record("rollback")
	return nil, f.rollbackErr
}
func (f *fakeDeploymentTransaction) Close(context.Context) error {
	f.record("close")
	f.once.Do(func() { close(f.closed) })
	return nil
}

type fakeDeploymentRuntime struct {
	*mockContainerdClient
	tx         *fakeDeploymentTransaction
	prepareErr error
	prepared   atomic.Int32
	probe      func(context.Context, *appconfig.ReadinessConfig) error
}

func (f *fakeDeploymentRuntime) PrepareDeployment(context.Context, *agentpb.CreateContainerRequest, *appconfig.AppConfig) (DeploymentTransaction, error) {
	f.prepared.Add(1)
	return f.tx, f.prepareErr
}
func (f *fakeDeploymentRuntime) ProbeReadiness(ctx context.Context, _ string, cfg *appconfig.ReadinessConfig) error {
	if f.probe != nil {
		return f.probe(ctx, cfg)
	}
	return nil
}
func newFakeDeploymentRuntime() *fakeDeploymentRuntime {
	return &fakeDeploymentRuntime{mockContainerdClient: &mockContainerdClient{startOutputCh: make(chan ContainerOutput)}, tx: &fakeDeploymentTransaction{previous: "previous-revision", previousWasRunning: true, closed: make(chan struct{})}}
}
func deploymentTestRequest(probe bool) *agentpb.RunContainerLayersRequest {
	config := `{"appId":"test-app"}`
	if probe {
		config = `{"appId":"test-app","readiness":{"tcpSocket":{"port":8080},"timeoutSeconds":1}}`
	}
	return &agentpb.RunContainerLayersRequest{AppName: "test-app", ImageName: "test:latest", AppConfig: []byte(config), Verified: true}
}

func TestDeployContainerOutcomes(t *testing.T) {
	for _, tc := range []struct {
		name  string
		probe bool
		setup func(*fakeDeploymentRuntime)
		state agentpb.DeploymentState
		calls []string
	}{
		{"ready", true, nil, agentpb.DeploymentState_READY, []string{"activate", "commit", "close"}},
		{"running-is-not-ready", false, nil, agentpb.DeploymentState_RUNNING, []string{"activate", "commit", "close"}},
		{"activation-rollback", true, func(f *fakeDeploymentRuntime) { f.tx.activateErr = errors.New("candidate create failed") }, agentpb.DeploymentState_ROLLED_BACK, []string{"activate", "rollback", "close"}},
		{"start-rollback", true, func(f *fakeDeploymentRuntime) { f.startErr = errors.New("process start failed") }, agentpb.DeploymentState_ROLLED_BACK, []string{"activate", "rollback", "close"}},
		{"commit-rollback", true, func(f *fakeDeploymentRuntime) { f.tx.commitErr = errors.New("persist failed") }, agentpb.DeploymentState_ROLLED_BACK, []string{"activate", "commit", "rollback", "close"}},
		{"rollback-failure", true, func(f *fakeDeploymentRuntime) {
			f.startErr = errors.New("start failed")
			f.tx.rollbackErr = errors.New("old snapshot missing")
		}, agentpb.DeploymentState_ROLLBACK_FAILED, []string{"activate", "rollback", "close"}},
		{"first-deploy-failure", true, func(f *fakeDeploymentRuntime) { f.startErr = errors.New("start failed"); f.tx.previous = "" }, agentpb.DeploymentState_FAILED, []string{"activate", "rollback", "close"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeDeploymentRuntime()
			if tc.setup != nil {
				tc.setup(f)
			}
			client, cleanup := startContainerServer(t, f)
			defer cleanup()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			stream, err := client.RunContainer(ctx, deploymentTestRequest(tc.probe))
			if err != nil {
				t.Fatal(err)
			}
			var result *agentpb.DeploymentResult
			for result == nil {
				event, err := stream.Recv()
				if err != nil {
					t.Fatal(err)
				}
				result = event.GetDeployment()
			}
			if result.State != tc.state {
				t.Fatalf("state=%v, want %v: %s", result.State, tc.state, result.Message)
			}
			f.tx.mu.Lock()
			calls := append([]string(nil), f.tx.calls...)
			f.tx.mu.Unlock()
			if !reflect.DeepEqual(calls, tc.calls) {
				t.Fatalf("calls=%v, want %v", calls, tc.calls)
			}
			cancel()
			close(f.startOutputCh)
		})
	}
}

func TestRollbackKeepsPreviouslyStoppedRevisionOutOfRestartMonitor(t *testing.T) {
	f := newFakeDeploymentRuntime()
	f.tx.previousWasRunning = false
	f.tx.activateErr = errors.New("candidate creation failed")
	mon := &mockMonitorRegistrar{}
	client, cleanup := startContainerServerWithMonitor(t, f, mon)
	defer cleanup()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stream, err := client.RunContainer(ctx, deploymentTestRequest(true))
	if err != nil {
		t.Fatal(err)
	}
	var result *agentpb.DeploymentResult
	for result == nil {
		event, err := stream.Recv()
		if err != nil {
			t.Fatal(err)
		}
		result = event.GetDeployment()
	}
	if result.State != agentpb.DeploymentState_ROLLED_BACK {
		t.Fatalf("result=%v", result)
	}
	if len(mon.registerCalls) != 0 {
		t.Fatalf("stopped previous revision registered for restart: %v", mon.registerCalls)
	}
	if !reflect.DeepEqual(mon.unregisterCalls, []string{"test-app"}) {
		t.Fatalf("unregister calls=%v", mon.unregisterCalls)
	}
	if len(f.stoppedByUserCalls) != 0 {
		t.Fatalf("saved stop intent was changed: %v", f.stoppedByUserCalls)
	}
	close(f.startOutputCh)
}

func TestDeployContainerRejectsBeforePreparing(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*agentpb.RunContainerLayersRequest)
		code   codes.Code
	}{
		{"identity", func(r *agentpb.RunContainerLayersRequest) { r.AppName = "another-app" }, codes.InvalidArgument},
		{"invalid-probe", func(r *agentpb.RunContainerLayersRequest) {
			r.AppConfig = []byte(`{"appId":"test-app","readiness":{"tcpSocket":{"port":0}}}`)
		}, codes.InvalidArgument},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeDeploymentRuntime()
			client, cleanup := startContainerServer(t, f)
			defer cleanup()
			r := deploymentTestRequest(false)
			tc.change(r)
			stream, err := client.RunContainer(context.Background(), r)
			if err == nil {
				_, err = stream.Recv()
			}
			if status.Code(err) != tc.code {
				t.Fatalf("error=%v", err)
			}
			if f.prepared.Load() != 0 {
				t.Fatal("invalid request prepared a candidate")
			}
		})
	}
}

func TestDeployContainerReadinessFailureRestoresPrevious(t *testing.T) {
	f := newFakeDeploymentRuntime()
	f.probe = func(context.Context, *appconfig.ReadinessConfig) error { return errors.New("HTTP 503") }
	client, cleanup := startContainerServer(t, f)
	defer cleanup()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stream, err := client.RunContainer(ctx, deploymentTestRequest(true))
	if err != nil {
		t.Fatal(err)
	}
	for {
		event, err := stream.Recv()
		if err != nil {
			t.Fatal(err)
		}
		if r := event.GetDeployment(); r != nil {
			if r.State != agentpb.DeploymentState_ROLLED_BACK || !strings.Contains(r.Message, "HTTP 503") {
				t.Fatalf("result=%v", r)
			}
			break
		}
	}
	if _, err := stream.Recv(); err != io.EOF {
		t.Fatalf("failed deploy did not terminate: %v", err)
	}
	close(f.startOutputCh)
}

func TestDeployContainerDisconnectStillCommitsVerifiedCandidate(t *testing.T) {
	f := newFakeDeploymentRuntime()
	entered := make(chan struct{})
	release := make(chan struct{})
	f.probe = func(ctx context.Context, _ *appconfig.ReadinessConfig) error {
		close(entered)
		select {
		case <-release:
			return ctx.Err()
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	client, cleanup := startContainerServer(t, f)
	defer cleanup()
	ctx, cancel := context.WithCancel(context.Background())
	stream, err := client.RunContainer(ctx, deploymentTestRequest(true))
	if err != nil {
		t.Fatal(err)
	}
	if event, err := stream.Recv(); err != nil || event.GetStarted() == nil {
		t.Fatalf("start: %v %v", event, err)
	}
	<-entered
	cancel()
	close(release)
	select {
	case <-f.tx.closed:
	case <-time.After(3 * time.Second):
		t.Fatal("disconnect abandoned transaction")
	}
	f.tx.mu.Lock()
	calls := append([]string(nil), f.tx.calls...)
	f.tx.mu.Unlock()
	if !reflect.DeepEqual(calls, []string{"activate", "commit", "close"}) {
		t.Fatalf("calls=%v", calls)
	}
	close(f.startOutputCh)
}

func TestReadinessHonorsParentDeadline(t *testing.T) {
	f := newFakeDeploymentRuntime()
	f.probe = func(ctx context.Context, _ *appconfig.ReadinessConfig) error { <-ctx.Done(); return ctx.Err() }
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	started := time.Now()
	err := waitForAgentReadiness(ctx, f, "test-app", &appconfig.ReadinessConfig{TCPSocket: &appconfig.TCPSocketProbe{Port: 8080}, TimeoutSeconds: 30})
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(started) > time.Second {
		t.Fatalf("unbounded readiness: %v", err)
	}
}

func TestLifecycleLockSerializesServiceWithGroup(t *testing.T) {
	var locks appMutex
	unlock := locks.lockApp("app_with_underscores_service")
	acquired := make(chan struct{})
	go func() { release := locks.lockApp("app_with_underscores"); close(acquired); release() }()
	select {
	case <-acquired:
		t.Fatal("group bypassed service lock")
	case <-time.After(20 * time.Millisecond):
	}
	unlock()
	unlock()
	select {
	case <-acquired:
	case <-time.After(time.Second):
		t.Fatal("group lock not released")
	}
}
