package containerd

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"

	tasksapi "github.com/containerd/containerd/api/services/tasks/v1"
	tasktypes "github.com/containerd/containerd/api/types/task"
	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/containers"
	"github.com/containerd/errdefs"
	"github.com/containerd/typeurl/v2"
	"github.com/opencontainers/runtime-spec/specs-go"
	"github.com/wendylabsinc/wendy/go/internal/agent/services"
	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
	"github.com/wendylabsinc/wendy/go/internal/shared/ros2inspection"
	"go.uber.org/zap"
	"google.golang.org/grpc"
)

// These stores exercise containerd's real container/task wrappers without a
// daemon. Unexpected mutation hits the embedded nil interface and fails the
// test: discovery and reuse must not pull images or delete system workloads.
type ros2TestContainerStore struct {
	containers.Store
	records []containers.Container
}

func (s ros2TestContainerStore) Get(_ context.Context, id string) (containers.Container, error) {
	for _, record := range s.records {
		if record.ID == id {
			return record, nil
		}
	}
	return containers.Container{}, errdefs.ErrNotFound
}

func (s ros2TestContainerStore) List(_ context.Context, filters ...string) ([]containers.Container, error) {
	var out []containers.Container
	for _, record := range s.records {
		match := len(filters) == 0
		for _, filter := range filters {
			for label := range record.Labels {
				match = match || filter == fmt.Sprintf("labels.%q", label)
			}
		}
		if match {
			out = append(out, record)
		}
	}
	return out, nil
}

type ros2TestTaskClient struct {
	tasksapi.TasksClient
	running map[string]uint32
}

func (s ros2TestTaskClient) Get(_ context.Context, req *tasksapi.GetRequest, _ ...grpc.CallOption) (*tasksapi.GetResponse, error) {
	pid, ok := s.running[req.ContainerID]
	if !ok {
		return nil, errdefs.ErrNotFound
	}
	return &tasksapi.GetResponse{Process: &tasktypes.Process{ID: req.ContainerID, ContainerID: req.ContainerID, Pid: pid, Status: tasktypes.Status_RUNNING}}, nil
}

func newROS2ContainerTestClient(t *testing.T, records []containers.Container, running map[string]uint32) *Client {
	t.Helper()
	spec, err := typeurl.MarshalAny(&specs.Spec{})
	if err != nil {
		t.Fatal(err)
	}
	for i := range records {
		records[i].Spec = spec
	}
	client, err := containerd.New("", containerd.WithServices(
		containerd.WithContainerStore(ros2TestContainerStore{records: records}),
		containerd.WithTaskClient(ros2TestTaskClient{running: running}),
	))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return &Client{client: client, namespace: "wendy", logger: zap.NewNop()}
}

func TestROS2SystemFallbackEligibleWithoutRunningApp(t *testing.T) {
	for _, stoppedApp := range []bool{false, true} {
		t.Run(fmt.Sprintf("stopped_app_%t", stoppedApp), func(t *testing.T) {
			records := []containers.Container{{ID: ros2SystemSidecarName, Labels: map[string]string{labelKeyROS2SystemSidecar: "humble"}}}
			if stoppedApp {
				records = append(records, containers.Container{ID: "stopped-app", Labels: map[string]string{appconfig.ROS2AnnotationKey: "distro=humble,domain_id=42"}})
			}
			c := newROS2ContainerTestClient(t, records, map[string]uint32{ros2SystemSidecarName: 10})
			_, err := c.EnsureROS2Sidecars(context.Background())
			if !errors.Is(err, services.ErrNoRunningROS2Containers) {
				t.Fatalf("no running app must permit system fallback: %v", err)
			}
			// App reconciliation and the boot reaper must leave the standalone
			// system CLI intact, despite its intentionally absent anchor labels.
			if err := c.ReapOrphanedROS2Sidecars(context.Background()); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestROS2RunningAppKeepsItsSidecarWithSystemCLIAvailable(t *testing.T) {
	name := ros2SidecarName("")
	c := newROS2ContainerTestClient(t, []containers.Container{
		{ID: ros2SystemSidecarName, Labels: map[string]string{labelKeyROS2SystemSidecar: "humble"}},
		{ID: "app", Labels: map[string]string{appconfig.ROS2AnnotationKey: "distro=humble,domain_id=42"}},
		{ID: name, Labels: map[string]string{labelKeyROS2Sidecar: "humble", labelKeyROS2AnchorID: "app", labelKeyROS2AnchorPID: "20"}},
	}, map[string]uint32{ros2SystemSidecarName: 10, "app": 20, name: 30})
	sidecars, err := c.EnsureROS2Sidecars(context.Background())
	if err != nil || len(sidecars) != 1 || sidecars[0].Name != name || sidecars[0].DomainID != 42 {
		t.Fatalf("running app routing changed: %+v, %v", sidecars, err)
	}
}

func TestROS2SystemCLIReusesRunningContainerWithoutApp(t *testing.T) {
	c := newROS2ContainerTestClient(t, []containers.Container{{ID: ros2SystemSidecarName, Labels: map[string]string{labelKeyROS2SystemSidecar: "humble"}}}, map[string]uint32{ros2SystemSidecarName: 10})
	sidecar, err := c.ensureStandaloneROS2Sidecar(context.Background(), ros2SystemSidecarName, labelKeyROS2SystemSidecar, 0, ros2SystemSidecarSpec("/profile.xml"))
	if err != nil || sidecar.Name != ros2SystemSidecarName || sidecar.DomainID != 0 || sidecar.RMW != ros2inspection.FastRTPSRMW {
		t.Fatalf("system reuse: %+v, %v", sidecar, err)
	}
}

func TestROS2SystemCLIRejectsNameCollision(t *testing.T) {
	c := newROS2ContainerTestClient(t, []containers.Container{{ID: ros2SystemSidecarName}}, map[string]uint32{ros2SystemSidecarName: 10})
	if _, err := c.ensureStandaloneROS2Sidecar(context.Background(), ros2SystemSidecarName, labelKeyROS2SystemSidecar, 0, ros2SystemSidecarSpec("/profile.xml")); err == nil {
		t.Fatal("reused unrelated workload with reserved name")
	}
}

func TestROS2RecordingVerifiesOnlySelectedSidecar(t *testing.T) {
	c := newROS2ContainerTestClient(t, []containers.Container{
		{ID: ros2SystemSidecarName, Labels: map[string]string{labelKeyROS2SystemSidecar: "humble"}},
		{ID: "stale-app-sidecar", Labels: map[string]string{labelKeyROS2Sidecar: "humble", labelKeyROS2AnchorID: "gone", labelKeyROS2AnchorPID: "20"}},
	}, map[string]uint32{ros2SystemSidecarName: 10, "stale-app-sidecar": 30})
	if err := c.VerifyROS2SidecarNamed(context.Background(), ros2SystemSidecarName); err != nil {
		t.Fatalf("unrelated stale app should not invalidate system recording: %v", err)
	}
	for _, name := range []string{"stale-app-sidecar", "missing-app-sidecar"} {
		if err := c.VerifyROS2SidecarNamed(context.Background(), name); err == nil {
			t.Fatalf("live system CLI should not hide unavailable app sidecar %q", name)
		}
	}
}

func TestROS2RecordingDetectsUnavailableSystemCLI(t *testing.T) {
	for _, tc := range []struct {
		name   string
		labels map[string]string
	}{
		{"stopped", map[string]string{labelKeyROS2SystemSidecar: "humble"}},
		{"wrong identity", map[string]string{labelKeyROS2HostSidecar: "humble"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newROS2ContainerTestClient(t, []containers.Container{{ID: ros2SystemSidecarName, Labels: tc.labels}}, nil)
			if err := c.VerifyROS2SidecarNamed(context.Background(), ros2SystemSidecarName); err == nil {
				t.Fatal("unavailable system CLI passed verification")
			}
		})
	}
}

func TestROS2SystemCLISpecCanReachRobotAndRecordBags(t *testing.T) {
	spec := ros2SystemSidecarSpec("/profile.xml")
	if !spec.Root.Readonly || !spec.Process.NoNewPrivileges || spec.Process.User.UID != 0 {
		t.Fatal("system CLI requires access to root-owned recordings with read-only root and no new privileges")
	}
	if len(spec.Process.Capabilities.Effective)+len(spec.Process.Capabilities.Permitted)+len(spec.Process.Capabilities.Bounding) != 0 {
		t.Fatal("system CLI does not need capabilities")
	}
	privateIPC, bagMount := false, false
	for _, ns := range spec.Linux.Namespaces {
		if ns.Type == "network" || ns.Path != "" {
			t.Fatalf("system CLI must use host network and private remaining namespaces: %+v", ns)
		}
		privateIPC = privateIPC || ns.Type == "ipc"
	}
	for _, mount := range spec.Mounts {
		if mount.Destination == ROS2BagDir {
			bagMount = mount.Source == ROS2BagDir && slices.Contains(mount.Options, "rw")
		}
		if mount.Destination == "/dev/shm" && mount.Type != "tmpfs" {
			t.Fatal("system CLI must not share host IPC buffers")
		}
	}
	if !privateIPC || !bagMount {
		t.Fatal("missing private IPC namespace or writable recording directory")
	}
	for _, env := range []string{"RMW_IMPLEMENTATION=" + ros2inspection.FastRTPSRMW, "ROS_LOCALHOST_ONLY=0", "ROS_AUTOMATIC_DISCOVERY_RANGE=SUBNET", "FASTRTPS_DEFAULT_PROFILES_FILE=" + ros2HostProfileMount} {
		if !slices.Contains(spec.Process.Env, env) {
			t.Errorf("missing system DDS setting %s", env)
		}
	}
}
