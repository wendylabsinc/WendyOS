//go:build linux

package containerd

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	tasksapi "github.com/containerd/containerd/api/services/tasks/v1"
	tasktypes "github.com/containerd/containerd/api/types/task"
	containerdclient "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/containers"
	"github.com/wendylabsinc/wendy/go/internal/agent/services"
	"go.uber.org/zap"
	"google.golang.org/grpc"
)

type fifoRecoveryTasks struct {
	*survivingRecoveryTasks
	stdout string
}

func (s fifoRecoveryTasks) Get(ctx context.Context, req *tasksapi.GetRequest, opts ...grpc.CallOption) (*tasksapi.GetResponse, error) {
	response, err := s.interruptedRecoveryTasks.Get(ctx, req, opts...)
	if err == nil {
		response.Process.Stdout = s.stdout
	}
	return response, err
}

func TestRecoveredOutputReadsSurvivingFIFOAndPreservesItOnDetach(t *testing.T) {
	fifo := filepath.Join(t.TempDir(), "stdout")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	tasks := fifoRecoveryTasks{survivingRecoveryTasks: &survivingRecoveryTasks{interruptedRecoveryTasks: interruptedRecoveryTasks{status: tasktypes.Status_RUNNING}, waiting: make(chan struct{}), canceled: make(chan struct{})}, stdout: fifo}
	labels := wendyLabels("app", "", "v1", nil, nil, "host", nil)
	labels[labelKeyRestartPolicy] = "no"
	remote, err := containerdclient.New("", containerdclient.WithServices(containerdclient.WithContainerStore(ros2TestContainerStore{records: []containers.Container{{ID: "app", Labels: labels}}}), containerdclient.WithTaskClient(tasks)))
	if err != nil {
		t.Fatal(err)
	}
	defer remote.Close()
	c := &Client{client: remote, namespace: "wendy-test", logger: zap.NewNop()}
	defer c.closeRecoveredTaskOutput()
	output := make(chan services.ContainerOutput, 4)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := c.RecoverRunningTaskOutput(ctx, func(_ string, p services.ContainerOutput) { output <- p }); err != nil {
		t.Fatal(err)
	}
	cancel() // Logging must outlive the boot RPC.
	var writer *os.File
	deadline := time.Now().Add(time.Second)
	for {
		writer, err = os.OpenFile(fifo, os.O_WRONLY|syscall.O_NONBLOCK, 0)
		if err == nil {
			break
		}
		if !time.Now().Before(deadline) {
			t.Fatalf("recovered reader never opened FIFO: %v", err)
		}
		time.Sleep(time.Millisecond)
	}
	defer writer.Close()
	if _, err := writer.WriteString("surviving output\n"); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-output:
		if string(got.Stdout) != "surviving output\n" {
			t.Fatalf("recovered stdout=%q", got.Stdout)
		}
	case <-time.After(time.Second):
		t.Fatal("surviving stdout did not reach publisher")
	}
	c.closeRecoveredTaskOutput()
	if _, err := os.Stat(fifo); err != nil {
		t.Fatalf("detach removed surviving task FIFO: %v", err)
	}
}
