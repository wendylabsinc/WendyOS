package containerd

import (
	"context"
	"github.com/containerd/typeurl/v2"
	"github.com/opencontainers/runtime-spec/specs-go"
	"io"
	"strings"
	"time"

	containerdclient "github.com/containerd/containerd/v2/client"
	"github.com/wendylabsinc/wendy/go/internal/agent/services"
	"go.uber.org/zap"
	"sync"
	"testing"

	tasksapi "github.com/containerd/containerd/api/services/tasks/v1"
	tasktypes "github.com/containerd/containerd/api/types/task"
	"github.com/containerd/containerd/v2/core/containers"
	"github.com/containerd/containerd/v2/pkg/cio"
	"google.golang.org/grpc"
)

func TestAttachmentClosePreservesSurvivingTaskFIFOs(t *testing.T) {
	removed := false
	original := cio.NewFIFOSet(cio.Config{Stdout: "/task/stdout", Stderr: "/task/stderr"}, func() error {
		removed = true
		return nil
	})
	attached := preserveTaskFIFOs(original)
	if attached.Config != original.Config {
		t.Fatal("attachment changed the task's FIFO paths")
	}
	if err := attached.Close(); err != nil || removed {
		t.Fatalf("detaching removed surviving task FIFOs: err=%v, removed=%v", err, removed)
	}
	_ = original.Close()
	if !removed {
		t.Fatal("original owner could not retire exited task FIFOs")
	}
}

func TestLateOutputExitDoesNotForgetReplacement(t *testing.T) {
	c := &Client{}
	old := c.trackTaskOutput("app", 100)
	replacement := c.trackTaskOutput("app", 100) // PID reuse must remain safe.
	c.forgetTaskOutput("app", old)
	if c.outputOwners["app"] != replacement {
		t.Fatal("old output reader released the replacement task's ownership")
	}
	c.forgetTaskOutput("app", replacement)
	if _, tracked := c.outputOwners["app"]; tracked {
		t.Fatal("exited task's output reader remained tracked")
	}
}

func TestOutputExitRequiresCurrentReader(t *testing.T) {
	for _, state := range []string{"current", "replacement", "closed", "same-pid"} {
		t.Run(state, func(t *testing.T) {
			c := &Client{}
			owner := c.trackTaskOutput("app", 100)
			switch state {
			case "replacement":
				c.trackTaskOutput("app", 200)
			case "closed":
				c.closeRecoveredTaskOutput()
			case "same-pid":
				c.trackTaskOutput("app", 100)
			}
			called := false
			c.withTaskOutputOwner("app", owner, func() { called = true })
			if called != (state == "current") {
				t.Fatalf("exit side effect for %s = %v", state, called)
			}
		})
	}
}

func TestRecoveredOutputCloseIsConcurrentAndIdempotent(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	r := &recoveredTaskOutput{cancel: cancel}
	c := &Client{recoveredOutput: map[string]*recoveredTaskOutput{"app": r}}
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(c.closeRecoveredTaskOutput)
	}
	wg.Wait()
	if ctx.Err() != context.Canceled || !c.outputClosed {
		t.Fatal("shutdown did not detach the recovered reader")
	}
}

func TestRecoveredOutputWriterCopiesReusableBuffer(t *testing.T) {
	var retained []byte
	writer := recoveredOutputWriter(func(p []byte) { retained = p })
	buffer := []byte("app output")
	if n, err := writer.Write(buffer); err != nil || n != len(buffer) {
		t.Fatalf("write = %d, %v", n, err)
	}
	buffer[0] = 'X'
	if string(retained) != "app output" {
		t.Fatal("publisher retained cio's mutable copy buffer")
	}
}

type delayedOutputIO struct {
	cio.IO
	entered chan struct{}
	release chan struct{}
}

func (f *delayedOutputIO) Wait() { close(f.entered); <-f.release }

type outputTaskForTest struct {
	containerdclient.Task
	output cio.IO
}

func (f outputTaskForTest) IO() cio.IO { return f.output }

func TestLiveOutputDelayedDrainCannotEndReplacement(t *testing.T) {
	// Hold the real streamOutput at its IO drain, start a replacement with the
	// same PID, then let the old exit callback and queued terminal run. A nil
	// containerd client also makes a stale exit-label write fail immediately.
	c := &Client{logger: zap.NewNop()}
	oldOwner := c.trackTaskOutput("app", 100)
	blocked := &delayedOutputIO{entered: make(chan struct{}), release: make(chan struct{})}
	task := outputTaskForTest{output: blocked}
	exit := make(chan containerdclient.ExitStatus, 1)
	exit <- *containerdclient.NewExitStatus(1, time.Now(), nil)
	output := make(chan services.ContainerOutput, 64)
	stdoutR, stdoutW := io.Pipe()
	stderrR, stderrW := io.Pipe()
	go c.streamOutput(context.Background(), task, exit, output, "app", "", 0, oldOwner, stdoutR, stderrR, stdoutW, stderrW)
	select {
	case <-blocked.entered:
	case <-time.After(time.Second):
		t.Fatal("old run did not reach its IO drain")
	}
	// Production starts hold this lock while installing output ownership.
	unlock := c.lockNetworkOperation("app")
	replacement := c.trackTaskOutput("app", 100)
	unlock()
	close(blocked.release)
	var terminal services.ContainerOutput
	select {
	case terminal = <-output:
	case <-time.After(time.Second):
		t.Fatal("replacement blocked behind the old output drain")
	}
	if !terminal.Done || terminal.PublishIfCurrent == nil {
		t.Fatal("stream did not preserve terminal ownership")
	}
	lm := services.NewContainerLogManager(zap.NewNop(), services.NewTelemetryBroadcaster())
	_, logs := lm.Subscribe("app")
	lm.Publish("app", terminal)
	select {
	case got := <-logs:
		t.Fatalf("old drain ended replacement logs: %+v", got)
	default:
	}
	lm.Publish("app", c.taskOutputDone("app", replacement))
	select {
	case got := <-logs:
		if !got.Done {
			t.Fatal("replacement terminal was lost")
		}
	case <-time.After(time.Second):
		t.Fatal("current task could not end its stream")
	}
}

type survivingRecoveryTasks struct {
	interruptedRecoveryTasks
	waiting  chan struct{}
	canceled chan struct{}
}

func (s *survivingRecoveryTasks) Wait(ctx context.Context, _ *tasksapi.WaitRequest, _ ...grpc.CallOption) (*tasksapi.WaitResponse, error) {
	close(s.waiting)
	<-ctx.Done()
	close(s.canceled)
	return nil, ctx.Err()
}

func TestRecoveredOutputDeduplicatesAndSurvivesBootRPCCancellation(t *testing.T) {
	labels := wendyLabels("app", "", "v1", nil, nil, "host", nil)
	labels[labelKeyRestartPolicy] = "no"
	tasks := &survivingRecoveryTasks{interruptedRecoveryTasks: interruptedRecoveryTasks{status: tasktypes.Status_RUNNING}, waiting: make(chan struct{}), canceled: make(chan struct{})}
	remote, err := containerdclient.New("", containerdclient.WithServices(containerdclient.WithContainerStore(ros2TestContainerStore{records: []containers.Container{{ID: "app", Labels: labels}}}), containerdclient.WithTaskClient(tasks)))
	if err != nil {
		t.Fatal(err)
	}
	defer remote.Close()
	c := &Client{client: remote, namespace: "wendy-test", logger: zap.NewNop()}
	defer c.closeRecoveredTaskOutput()
	ctx, cancel := context.WithCancel(context.Background())
	if err := c.RecoverRunningTaskOutput(ctx, nil); err != nil {
		t.Fatal(err)
	}
	select {
	case <-tasks.waiting:
	case <-time.After(time.Second):
		t.Fatal("output recovery did not install its waiter")
	}
	c.outputMu.Lock()
	owner := c.outputOwners["app"]
	c.outputMu.Unlock()
	if owner == nil {
		t.Fatal("--no-restart task was not attached")
	}
	if err := c.RecoverRunningTaskOutput(ctx, nil); err != nil {
		t.Fatal(err)
	}
	c.outputMu.Lock()
	unchanged := c.outputOwners["app"] == owner
	c.outputMu.Unlock()
	if !unchanged {
		t.Fatal("second reconciliation replaced the live output reader")
	}
	cancel()
	select {
	case <-tasks.canceled:
		t.Fatal("boot RPC cancellation detached the live reader")
	default:
	}
	c.closeRecoveredTaskOutput()
	select {
	case <-tasks.canceled:
	case <-time.After(time.Second):
		t.Fatal("shutdown did not cancel recovered task monitoring")
	}
}

type aliasOutputContainerStore struct{ ros2TestContainerStore }

func (s aliasOutputContainerStore) List(context.Context, ...string) ([]containers.Container, error) {
	return s.records, nil
}

func TestFailedAliasReplacementSupersedesOldOutputOwner(t *testing.T) {
	labels := wendyLabels("my_app", "worker", "v1", nil, nil, "shared-network", nil)
	labels[labelKeySerialIdentities] = "invalid-json" // Fail before creating a task.
	spec, err := typeurl.MarshalAny(&specs.Spec{})
	if err != nil {
		t.Fatal(err)
	}
	store := aliasOutputContainerStore{ros2TestContainerStore{records: []containers.Container{{ID: "my_app_worker", Labels: labels, Spec: spec}}}}
	remote, err := containerdclient.New("", containerdclient.WithServices(containerdclient.WithContainerStore(store)))
	if err != nil {
		t.Fatal(err)
	}
	defer remote.Close()
	c := &Client{client: remote, namespace: "wendy-test", logger: zap.NewNop()}
	old := c.trackTaskOutput("my_app_worker", 100)
	_, err = c.StartContainer(context.Background(), "my_app", "", nil)
	if err == nil || !strings.Contains(err.Error(), "serial device identities") {
		t.Fatalf("start error=%v", err)
	}
	called := false
	c.withTaskOutputOwner("my_app_worker", old, func() { called = true })
	if called {
		t.Fatal("failed replacement left old output able to overwrite its diagnostics")
	}
}
