package containerd

import (
	"context"
	"sync/atomic"
	"testing"

	tasksapi "github.com/containerd/containerd/api/services/tasks/v1"
	tasktypes "github.com/containerd/containerd/api/types/task"
	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/containers"
	"github.com/containerd/typeurl/v2"
	specs "github.com/opencontainers/runtime-spec/specs-go"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/emptypb"
)

type optionalAvahiTaskClient struct {
	tasksapi.TasksClient
	deleted atomic.Bool
	stopped atomic.Bool
}

func (s *optionalAvahiTaskClient) Get(_ context.Context, r *tasksapi.GetRequest, _ ...grpc.CallOption) (*tasksapi.GetResponse, error) {
	state := tasktypes.Status_CREATED
	if s.stopped.Load() {
		state = tasktypes.Status_STOPPED
	}
	return &tasksapi.GetResponse{Process: &tasktypes.Process{ID: r.ContainerID, ContainerID: r.ContainerID, Pid: 1234, Status: state}}, nil
}
func (s *optionalAvahiTaskClient) Wait(context.Context, *tasksapi.WaitRequest, ...grpc.CallOption) (*tasksapi.WaitResponse, error) {
	return &tasksapi.WaitResponse{}, nil
}
func (s *optionalAvahiTaskClient) Kill(context.Context, *tasksapi.KillRequest, ...grpc.CallOption) (*emptypb.Empty, error) {
	s.stopped.Store(true)
	return &emptypb.Empty{}, nil
}
func (s *optionalAvahiTaskClient) Delete(context.Context, *tasksapi.DeleteTaskRequest, ...grpc.CallOption) (*tasksapi.DeleteResponse, error) {
	s.deleted.Store(true)
	return &tasksapi.DeleteResponse{}, nil
}

func TestStopCreatedTaskWithoutAvahiManager(t *testing.T) {
	spec, err := typeurl.MarshalAny(&specs.Spec{})
	if err != nil {
		t.Fatal(err)
	}
	tasks := &optionalAvahiTaskClient{}
	remote, err := containerd.New("", containerd.WithServices(
		containerd.WithContainerStore(ros2TestContainerStore{records: []containers.Container{{ID: "plain-app", Labels: map[string]string{labelKeyAppID: "plain-app"}, Spec: spec}}}),
		containerd.WithTaskClient(tasks)))
	if err != nil {
		t.Fatal(err)
	}
	defer remote.Close()
	c := &Client{client: remote, namespace: "recovery-test", logger: zap.NewNop()}
	if err := c.stopOne(c.withNamespace(context.Background()), "plain-app"); err != nil {
		t.Fatal(err)
	}
	if !tasks.deleted.Load() {
		t.Fatal("created task was not reaped")
	}
}
