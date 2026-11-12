package containerd

import (
	"context"
	"fmt"
	"os"
	"testing"

	tasksapi "github.com/containerd/containerd/api/services/tasks/v1"
	tasktypes "github.com/containerd/containerd/api/types/task"
	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/containers"
	"github.com/containerd/errdefs"
	"go.uber.org/zap"
	"google.golang.org/grpc"

	"github.com/wendylabsinc/wendy/go/internal/agent/hoststats"
)

// appLabelStore answers the exact app-label filter AppCgroups uses.
type appLabelStore struct {
	containers.Store
	records []containers.Container
}

func (s appLabelStore) List(_ context.Context, filters ...string) ([]containers.Container, error) {
	var out []containers.Container
	for _, record := range s.records {
		for _, filter := range filters {
			if filter == fmt.Sprintf("labels.%q==%q", labelKeyAppID, record.Labels[labelKeyAppID]) {
				out = append(out, record)
			}
		}
	}
	return out, nil
}

func (s appLabelStore) Get(_ context.Context, id string) (containers.Container, error) {
	for _, record := range s.records {
		if record.ID == id {
			return record, nil
		}
	}
	return containers.Container{}, errdefs.ErrNotFound
}

type processTasks struct {
	tasksapi.TasksClient
	processes map[string]*tasktypes.Process
}

func (p processTasks) Get(_ context.Context, req *tasksapi.GetRequest, _ ...grpc.CallOption) (*tasksapi.GetResponse, error) {
	if process, ok := p.processes[req.ContainerID]; ok {
		return &tasksapi.GetResponse{Process: process}, nil
	}
	return nil, errdefs.ErrNotFound
}

func appCgroupsClient(t *testing.T, processes map[string]*tasktypes.Process) *Client {
	t.Helper()
	app := map[string]string{labelKeyAppID: "sh.wendy.spare"}
	remote, err := containerd.New("", containerd.WithServices(
		containerd.WithContainerStore(appLabelStore{records: []containers.Container{
			{ID: "sh.wendy.spare_model", Labels: app},
			{ID: "sh.wendy.spare_worker", Labels: app},
			{ID: "sh.wendy.spare_reviewer", Labels: app},
			{ID: "other", Labels: map[string]string{labelKeyAppID: "other"}},
		}}),
		containerd.WithTaskClient(processTasks{processes: processes})))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = remote.Close() })
	return &Client{client: remote, namespace: "gpu-admission-test", logger: zap.NewNop()}
}

func TestAppCgroupsResolvesOnlyLiveTasks(t *testing.T) {
	self := uint32(os.Getpid())
	own, err := hoststats.ProcessCgroup(self)
	if err != nil {
		t.Skipf("test process has no isolated cgroup v2 path: %v", err)
	}
	process := func(id string, pid uint32, status tasktypes.Status) *tasktypes.Process {
		return &tasktypes.Process{ID: id, ContainerID: id, Pid: pid, Status: status}
	}
	c := appCgroupsClient(t, map[string]*tasktypes.Process{
		"sh.wendy.spare_model": process("sh.wendy.spare_model", self, tasktypes.Status_RUNNING),
		// Restart backoff: the exited PID is gone, or already reused (PID 1).
		"sh.wendy.spare_worker":   process("sh.wendy.spare_worker", 1<<30, tasktypes.Status_STOPPED),
		"sh.wendy.spare_reviewer": process("sh.wendy.spare_reviewer", 1, tasktypes.Status_STOPPED),
		"other":                   process("other", 1, tasktypes.Status_RUNNING),
	})
	groups, err := c.AppCgroups(context.Background(), "sh.wendy.spare")
	if err != nil {
		t.Fatalf("an exited sibling made ownership unavailable: %v", err)
	}
	if len(groups) != 1 || groups[0] != own {
		t.Fatalf("groups = %q, want only the live task's %q", groups, own)
	}
}

func TestAppCgroupsWithoutLiveTasksFails(t *testing.T) {
	c := appCgroupsClient(t, map[string]*tasktypes.Process{
		"sh.wendy.spare_model": {ID: "sh.wendy.spare_model", ContainerID: "sh.wendy.spare_model", Pid: 1, Status: tasktypes.Status_STOPPED},
	})
	if groups, err := c.AppCgroups(context.Background(), "sh.wendy.spare"); err == nil {
		t.Fatalf("stopped app reported cgroups %q", groups)
	}
}
