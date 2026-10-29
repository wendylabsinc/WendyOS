package containerd

import (
	"context"
	"errors"
	tasksapi "github.com/containerd/containerd/api/services/tasks/v1"
	tasktypes "github.com/containerd/containerd/api/types/task"
	"github.com/containerd/containerd/v2/core/containers"
	"github.com/containerd/errdefs"
	"github.com/containerd/typeurl/v2"
	"github.com/opencontainers/runtime-spec/specs-go"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/emptypb"
	"sync"
	"testing"

	containerdclient "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/pkg/cio"
)

type interruptedOwnerContainer struct {
	containerdclient.Container
	name      string
	labels    map[string]string
	inspected bool
}

func (c *interruptedOwnerContainer) ID() string { return c.name }
func (c *interruptedOwnerContainer) Labels(context.Context) (map[string]string, error) {
	return c.labels, nil
}
func (c *interruptedOwnerContainer) Task(context.Context, cio.Attach) (containerdclient.Task, error) {
	c.inspected = true
	return nil, errors.New("stop before real task cleanup")
}

func TestInterruptedRecoveryValidatesExactLabeledOwner(t *testing.T) {
	for _, tc := range []struct {
		name, app, service string
		valid              bool
	}{
		{"app", "app", "", true},
		{"my_app", "my_app", "", true},
		{"my_app_worker", "my_app", "worker", true},
		{"my_app_worker", "other", "worker", false},
		{"my_app_worker", "my_app", "other", false},
		{"my_app_worker", "my_app", "bad_service", false},
		{"my_app_worker", "../app", "worker", false},
		{"my_app_worker", "", "", false},
	} {
		t.Run(tc.name+"/"+tc.app+"/"+tc.service, func(t *testing.T) {
			ctr := &interruptedOwnerContainer{name: tc.name, labels: map[string]string{labelKeyAppID: tc.app, labelKeyServiceName: tc.service}}
			c := &Client{}
			_ = c.recoverInterruptedTaskStart(context.Background(), ctr)
			if ctr.inspected != tc.valid {
				t.Fatalf("task inspected=%v, want %v", ctr.inspected, tc.valid)
			}
			app, service, err := containerOwnerFromLabels(tc.name, ctr.labels)
			if tc.valid && (err != nil || app != tc.app || service != tc.service) {
				t.Fatalf("cleanup owner = %q/%q, %v", app, service, err)
			}
		})
	}
}

type interruptedRecoveryTasks struct {
	tasksapi.TasksClient
	mu      sync.Mutex
	status  tasktypes.Status
	exited  chan struct{}
	deleted bool
}

func (s *interruptedRecoveryTasks) Get(_ context.Context, req *tasksapi.GetRequest, _ ...grpc.CallOption) (*tasksapi.GetResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.deleted {
		return nil, errdefs.ErrNotFound
	}
	return &tasksapi.GetResponse{Process: &tasktypes.Process{ID: req.ContainerID, ContainerID: req.ContainerID, Pid: 1234, Status: s.status}}, nil
}
func (s *interruptedRecoveryTasks) Wait(ctx context.Context, _ *tasksapi.WaitRequest, _ ...grpc.CallOption) (*tasksapi.WaitResponse, error) {
	select {
	case <-s.exited:
		return &tasksapi.WaitResponse{}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
func (s *interruptedRecoveryTasks) Kill(context.Context, *tasksapi.KillRequest, ...grpc.CallOption) (*emptypb.Empty, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.status != tasktypes.Status_STOPPED {
		s.status = tasktypes.Status_STOPPED
		close(s.exited)
	}
	return &emptypb.Empty{}, nil
}
func (s *interruptedRecoveryTasks) Delete(context.Context, *tasksapi.DeleteTaskRequest, ...grpc.CallOption) (*tasksapi.DeleteResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deleted = true
	return &tasksapi.DeleteResponse{}, nil
}

func TestInterruptedRecoveryReapsNoRestartServiceAndPreservesRunningTask(t *testing.T) {
	for _, status := range []tasktypes.Status{tasktypes.Status_CREATED, tasktypes.Status_RUNNING} {
		t.Run(status.String(), func(t *testing.T) {
			labels := wendyLabels("my_app", "worker", "v1", nil, nil, "shared-network", nil)
			labels[labelKeyRestartPolicy] = "no"
			spec, err := typeurl.MarshalAny(&specs.Spec{})
			if err != nil {
				t.Fatal(err)
			}
			store := ros2TestContainerStore{records: []containers.Container{{ID: "my_app_worker", Labels: labels, Spec: spec}}}
			tasks := &interruptedRecoveryTasks{status: status, exited: make(chan struct{})}
			remote, err := containerdclient.New("", containerdclient.WithDefaultRuntime("io.containerd.runc.v2"), containerdclient.WithServices(containerdclient.WithContainerStore(store), containerdclient.WithTaskClient(tasks)))
			if err != nil {
				t.Fatal(err)
			}
			defer remote.Close()
			c := &Client{client: remote, namespace: "wendy-test", logger: zap.NewNop()}
			if err := c.RecoverInterruptedTaskStarts(context.Background()); err != nil {
				t.Fatal(err)
			}
			tasks.mu.Lock()
			deleted := tasks.deleted
			tasks.mu.Unlock()
			if deleted != (status == tasktypes.Status_CREATED) {
				t.Fatalf("deleted=%v for %s", deleted, status)
			}
		})
	}
}
