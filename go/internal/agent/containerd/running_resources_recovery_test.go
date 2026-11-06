package containerd

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	specs "github.com/opencontainers/runtime-spec/specs-go"
	"github.com/wendylabsinc/wendy/go/internal/agent/dbusproxy"
	"go.uber.org/zap"
)

type recoveryResourceTask struct {
	containerd.Task
	pid   uint32
	state containerd.ProcessStatus
}

func (t recoveryResourceTask) Pid() uint32 { return t.pid }
func (t recoveryResourceTask) Status(context.Context) (containerd.Status, error) {
	return containerd.Status{Status: t.state}, nil
}

type recoveryResourceContainer struct {
	staleContainerForTest
	labels map[string]string
	spec   func(context.Context) (*specs.Spec, error)
}

func (recoveryResourceContainer) ID() string { return "resource-app_ui" }
func (c recoveryResourceContainer) Labels(context.Context) (map[string]string, error) {
	return c.labels, nil
}
func (c recoveryResourceContainer) Spec(ctx context.Context) (*specs.Spec, error) { return c.spec(ctx) }

func recoveryBluetoothSpec() *specs.Spec {
	return &specs.Spec{Mounts: []specs.Mount{{Destination: "/var/run/dbus", Source: dbusproxy.SocketDir("resource-app_ui"), Type: "bind"}}}
}

func TestRunningResourcesWaitForLifecycleAndRecheckCreatedOrStoppedTask(t *testing.T) {
	for _, state := range []containerd.ProcessStatus{containerd.Created, containerd.Stopped} {
		t.Run(string(state), func(t *testing.T) {
			var mu sync.Mutex
			current := containerd.Running
			proxy := &recordingProxyManager{}
			c := &Client{logger: zap.NewNop(), namespace: "recovery-test", proxyManager: proxy}
			ctr := recoveryResourceContainer{labels: map[string]string{labelKeyAppID: "resource-app"}, spec: func(context.Context) (*specs.Spec, error) {
				t.Error("stale running snapshot reached resource preparation")
				return recoveryBluetoothSpec(), nil
			}}
			ctr.taskFn = func(ctx context.Context) (containerd.Task, error) {
				if ns, _ := namespaces.Namespace(ctx); ns != "recovery-test" {
					t.Errorf("task lookup namespace: %q", ns)
				}
				mu.Lock()
				defer mu.Unlock()
				return recoveryResourceTask{pid: 1234, state: current}, nil
			}
			unlock := c.lockNetworkOperation(ctr.ID())
			started, done := make(chan struct{}), make(chan error, 1)
			go func() { close(started); done <- c.restoreRunningTaskResources(context.Background(), ctr) }()
			<-started
			select {
			case err := <-done:
				unlock()
				t.Fatalf("recovery raced the canonical lifecycle lock: %v", err)
			case <-time.After(20 * time.Millisecond):
			}
			// Stop won, or a replacement is still held at CREATED before CNI.
			mu.Lock()
			current = state
			mu.Unlock()
			unlock()
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(time.Second):
				t.Fatal("recovery did not finish")
			}
			if len(proxy.started) != 0 {
				t.Fatal("stale snapshot recreated a proxy")
			}
		})
	}
}

func TestRunningResourcesRejectTaskExitOrReplacementDuringSpecRead(t *testing.T) {
	for _, replacement := range []bool{false, true} {
		t.Run(map[bool]string{false: "exited", true: "replaced"}[replacement], func(t *testing.T) {
			proxy := &recordingProxyManager{}
			c := &Client{logger: zap.NewNop(), namespace: "recovery-test", proxyManager: proxy}
			current := recoveryResourceTask{pid: 1234, state: containerd.Running}
			ctr := recoveryResourceContainer{labels: map[string]string{labelKeyAppID: "resource-app"}, spec: func(context.Context) (*specs.Spec, error) {
				if replacement {
					current.pid = 5678
				} else {
					current.state = containerd.Stopped
				}
				return recoveryBluetoothSpec(), nil
			}}
			ctr.taskFn = func(context.Context) (containerd.Task, error) { return current, nil }
			if err := c.restoreRunningTaskResources(context.Background(), ctr); err != nil {
				t.Fatal(err)
			}
			if len(proxy.started) != 0 {
				t.Fatal("exited/replaced task recovered stale resources")
			}
		})
	}
}

type blockingRecoveryProxy struct {
	recordingProxyManager
	entered, release chan struct{}
}

func (p *blockingRecoveryProxy) Start(ctx context.Context, id string) (string, error) {
	close(p.entered)
	<-p.release
	return p.recordingProxyManager.Start(ctx, id)
}

func TestRunningResourcesHoldCanonicalLockThroughExternalPreparation(t *testing.T) {
	p := &blockingRecoveryProxy{entered: make(chan struct{}), release: make(chan struct{})}
	c := &Client{logger: zap.NewNop(), namespace: "recovery-test", proxyManager: p}
	ctr := recoveryResourceContainer{labels: map[string]string{labelKeyAppID: "resource-app"}, spec: func(context.Context) (*specs.Spec, error) {
		if !c.mu.TryLock() {
			t.Error("global map mutex held across resource RPC")
		} else {
			c.mu.Unlock()
		}
		return recoveryBluetoothSpec(), nil
	}}
	ctr.taskFn = func(context.Context) (containerd.Task, error) {
		return recoveryResourceTask{pid: 1234, state: containerd.Running}, nil
	}
	done := make(chan error, 1)
	go func() { done <- c.restoreRunningTaskResources(context.Background(), ctr) }()
	select {
	case <-p.entered:
	case <-time.After(time.Second):
		t.Fatal("recovery did not reach proxy preparation")
	}
	stopDone := make(chan struct{})
	go func() { unlock := c.lockNetworkOperation(ctr.ID()); unlock(); close(stopDone) }()
	otherDone := make(chan struct{})
	go func() { unlock := c.lockNetworkOperation("other-app"); unlock(); close(otherDone) }()
	select {
	case <-otherDone:
	case <-time.After(time.Second):
		t.Fatal("unrelated app blocked")
	}
	select {
	case <-stopDone:
		close(p.release)
		t.Fatal("stop entered while resources were being prepared")
	case <-time.After(20 * time.Millisecond):
	}
	close(p.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	select {
	case <-stopDone:
	case <-time.After(time.Second):
		t.Fatal("stop did not proceed after recovery")
	}
}

func TestRunningResourcesHonorFreshStoppedOwner(t *testing.T) {
	for _, stopping := range []bool{false, true} {
		labels := map[string]string{labelKeyAppID: "resource-app"}
		c := &Client{logger: zap.NewNop(), namespace: "recovery-test"}
		if stopping {
			c.appStopping = map[string]bool{"resource-app": true}
		} else {
			labels[labelKeyStoppedByUser] = "true"
		}
		ctr := recoveryResourceContainer{labels: labels}
		ctr.taskFn = func(context.Context) (containerd.Task, error) {
			t.Error("stopped owner reached task recovery")
			return nil, errors.New("stopped")
		}
		if err := c.restoreRunningTaskResources(context.Background(), ctr); err != nil {
			t.Fatal(err)
		}
	}
}
