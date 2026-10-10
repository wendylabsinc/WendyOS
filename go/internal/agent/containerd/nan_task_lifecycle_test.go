package containerd

import (
	"context"
	"fmt"
	tasksapi "github.com/containerd/containerd/api/services/tasks/v1"
	tasktypes "github.com/containerd/containerd/api/types/task"
	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/containers"
	localoci "github.com/wendylabsinc/wendy/go/internal/agent/oci"
	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
	"google.golang.org/grpc"
	"net"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func captureNANRemoval(t *testing.T, client *Client) *[]string {
	t.Helper()
	oldHelper, oldRoot := runNANHelper, nanClientRootPath
	nanClientRootPath = t.TempDir()
	var removed []string
	runNANHelper = func(_ context.Context, args ...string) error {
		if !client.mu.TryLock() {
			t.Error("supplicant cleanup held global container map mutex")
		} else {
			client.mu.Unlock()
		}
		if len(args) != 2 || args[0] != "ndi-remove" {
			t.Errorf("unexpected NAN command: %v", args)
		} else {
			removed = append(removed, args[1])
		}
		return nil
	}
	t.Cleanup(func() { runNANHelper, nanClientRootPath = oldHelper, oldRoot })
	return &removed
}
func TestNANConfirmedExitRemovesOnlyItsAppNDI(t *testing.T) {
	c := nanLifecycleClient(t, "camera", false)
	removed := captureNANRemoval(t, c)
	c.releaseNANAfterTaskExit(context.Background(), "camera", "camera", 1)
	if len(*removed) != 1 || (*removed)[0] != nanAppNDI("camera") {
		t.Fatalf("removals=%v", *removed)
	}
	c.releaseNANAfterTaskExit(context.Background(), "other", "", 1)
	c.releaseNANAfterTaskExit(context.Background(), "other", "other", 0)
	if len(*removed) != 1 {
		t.Fatal("unowned task cleanup removed NDI")
	}
}
func TestNANLateExitCannotRemoveReplacementPreparedInterface(t *testing.T) {
	c := nanLifecycleClient(t, "camera", false)
	removed := captureNANRemoval(t, c)
	// This intentionally models preparation before the replacement becomes
	// Running, so checking running status alone would incorrectly remove it.
	c.meshIngressRuns = map[string]uint64{"camera": 2}
	c.releaseNANAfterTaskExit(context.Background(), "camera", "camera", 1)
	if len(*removed) != 0 {
		t.Fatalf("late exit removed replacement: %v", *removed)
	}
}
func TestNANExitCleanupWaitsForReplacementAndDoesNotBlockOtherApp(t *testing.T) {
	c := nanLifecycleClient(t, "camera", false)
	removed := captureNANRemoval(t, c)
	unlock := c.lockNANOperation("camera")
	entered, done := make(chan struct{}), make(chan struct{})
	go func() {
		close(entered)
		c.releaseNANAfterTaskExit(context.Background(), "camera", "camera", 1)
		close(done)
	}()
	<-entered
	select {
	case <-done:
		t.Fatal("cleanup raced preparation")
	case <-time.After(20 * time.Millisecond):
	}
	other := make(chan struct{})
	go func() { release := c.lockNANOperation("other"); release(); close(other) }()
	select {
	case <-other:
	case <-time.After(time.Second):
		t.Fatal("unrelated app blocked")
	}
	c.meshIngressMu.Lock()
	c.meshIngressRuns = map[string]uint64{"camera": 2}
	c.meshIngressMu.Unlock()
	unlock()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("cleanup did not finish")
	}
	if len(*removed) != 0 {
		t.Fatalf("replacement removed: %v", *removed)
	}
}
func TestNANFailedRestartReleasesPreparedIdleInterface(t *testing.T) {
	c := nanLifecycleClient(t, "camera", false)
	removed := captureNANRemoval(t, c)
	unlock := c.lockNANOperation("camera")
	lease := nanCreateLease{client: c, ctx: context.Background(), appID: "camera", attempted: true}
	lease.finishStart()
	unlock()
	if len(*removed) != 1 {
		t.Fatalf("failed restart stranded NDI: %v", *removed)
	}
	lease.committed = true
	lease.finishStart()
	if len(*removed) != 1 {
		t.Fatal("successful restart NDI removed")
	}
}
func TestNANExplicitStopWaitsForAppPreparation(t *testing.T) {
	c := nanLifecycleClient(t, "camera", false)
	removed := captureNANRemoval(t, c)
	unlock := c.lockNANOperation("camera")
	done := make(chan error, 1)
	go func() { done <- c.releaseNANIfIdle(context.Background(), "camera") }()
	select {
	case <-done:
		t.Fatal("stop raced app preparation")
	case <-time.After(20 * time.Millisecond):
	}
	unlock()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if len(*removed) != 1 {
		t.Fatal("explicit stop failed cleanup")
	}
}

type statusNANTasks struct {
	tasksapi.TasksClient
	status tasktypes.Status
	mu     sync.Mutex
}

func (s *statusNANTasks) Get(_ context.Context, req *tasksapi.GetRequest, _ ...grpc.CallOption) (*tasksapi.GetResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return &tasksapi.GetResponse{Process: &tasktypes.Process{ID: req.ContainerID, ContainerID: req.ContainerID, Pid: 1234, Status: s.status}}, nil
}
func TestNANLiveOrPausedSiblingKeepsSharedAppNDI(t *testing.T) {
	for _, state := range []tasktypes.Status{tasktypes.Status_RUNNING, tasktypes.Status_PAUSED, tasktypes.Status_CREATED, tasktypes.Status_UNKNOWN} {
		t.Run(state.String(), func(t *testing.T) {
			c := nanLifecycleClient(t, "camera", false)
			removed := captureNANRemoval(t, c)
			record := containers.Container{ID: "camera_worker", Labels: map[string]string{labelKeyAppID: "camera", appconfig.EntitlementAnnotationKeyPrefix + appconfig.EntitlementNAN: "{}"}}
			remote, err := containerd.New("", containerd.WithServices(containerd.WithContainerStore(nanLifecycleStore{records: []containers.Container{record}}), containerd.WithTaskClient(&statusNANTasks{status: state})))
			if err != nil {
				t.Fatal(err)
			}
			defer remote.Close()
			c.client = remote
			c.releaseNANAfterTaskExit(context.Background(), "camera_main", "camera", 1)
			if len(*removed) != 0 {
				t.Fatalf("live sibling lost shared NDI: %v", *removed)
			}
		})
	}
}

func TestNANMonitorRechecksInheritedStoppedTaskBeforeCleanup(t *testing.T) {
	for _, restarted := range []bool{false, true} {
		t.Run(fmt.Sprint(restarted), func(t *testing.T) {
			c := nanLifecycleClient(t, "camera", false)
			removed := captureNANRemoval(t, c)
			host := filepath.Join(t.TempDir(), "nan0")
			socket, err := net.Listen("unix", host)
			if err != nil {
				t.Fatal(err)
			}
			defer socket.Close()
			old := localoci.NANControlSocketHostPath
			localoci.NANControlSocketHostPath = host
			t.Cleanup(func() { localoci.NANControlSocketHostPath = old })
			record := containers.Container{ID: "camera", Labels: map[string]string{labelKeyAppID: "camera", labelKeyStoppedByUser: "true", appconfig.EntitlementAnnotationKeyPrefix + appconfig.EntitlementNAN: "{}"}}
			tasks := &changingNANTasks{restarted: restarted}
			remote, err := containerd.New("", containerd.WithServices(containerd.WithContainerStore(nanLifecycleStore{records: []containers.Container{record}}), containerd.WithTaskClient(tasks)))
			if err != nil {
				t.Fatal(err)
			}
			defer remote.Close()
			c.client = remote
			stale, err := c.StaleNANSocketContainers(context.Background())
			if err != nil || len(stale) != 0 {
				t.Fatalf("explicit stop became restart candidate: %v %v", stale, err)
			}
			want := 1
			if restarted {
				want = 0
			}
			if len(*removed) != want {
				t.Fatalf("removals=%v restarted=%v", *removed, restarted)
			}
		})
	}
}

type changingNANTasks struct {
	tasksapi.TasksClient
	calls     int
	restarted bool
}

func (s *changingNANTasks) Get(_ context.Context, req *tasksapi.GetRequest, _ ...grpc.CallOption) (*tasksapi.GetResponse, error) {
	s.calls++
	state := tasktypes.Status_STOPPED
	if s.restarted && s.calls > 2 {
		state = tasktypes.Status_RUNNING
	}
	return &tasksapi.GetResponse{Process: &tasktypes.Process{ID: req.ContainerID, ContainerID: req.ContainerID, Pid: 1234, Status: state}}, nil
}

func TestNANMonitorMissingTaskAndNonEntitledIsolation(t *testing.T) {
	for _, entitled := range []bool{true, false} {
		t.Run(fmt.Sprint(entitled), func(t *testing.T) {
			c := nanLifecycleClient(t, "camera", false)
			removed := captureNANRemoval(t, c)
			host := filepath.Join(t.TempDir(), "nan0")
			socket, err := net.Listen("unix", host)
			if err != nil {
				t.Fatal(err)
			}
			defer socket.Close()
			old := localoci.NANControlSocketHostPath
			localoci.NANControlSocketHostPath = host
			t.Cleanup(func() { localoci.NANControlSocketHostPath = old })
			if !entitled {
				record := containers.Container{ID: "other", Labels: map[string]string{labelKeyAppID: "other"}}
				// No task client: any attempt to inspect or mutate this unrelated app
				// fails the test instead of quietly returning a convenient stopped state.
				remote, err := containerd.New("", containerd.WithServices(containerd.WithContainerStore(nanLifecycleStore{records: []containers.Container{record}}), containerd.WithTaskClient(&struct{ tasksapi.TasksClient }{})))
				if err != nil {
					t.Fatal(err)
				}
				defer remote.Close()
				c.client = remote
			}
			stale, err := c.StaleNANSocketContainers(context.Background())
			if err != nil || len(stale) != 0 {
				t.Fatalf("monitor=%v %v", stale, err)
			}
			want := 0
			if entitled {
				want = 1
			}
			if len(*removed) != want {
				t.Fatalf("entitled=%v removed=%v", entitled, *removed)
			}
		})
	}
}
