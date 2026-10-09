package containerd

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"testing"

	tasksapi "github.com/containerd/containerd/api/services/tasks/v1"
	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/containers"
	"github.com/containerd/errdefs"
	localoci "github.com/wendylabsinc/wendy/go/internal/agent/oci"
	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestStaleNANSocketContainers(t *testing.T) {
	dir := t.TempDir()
	oldPath, hostPath := filepath.Join(dir, "old.sock"), filepath.Join(dir, "nan0")
	oldListener, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: oldPath, Net: "unixgram"})
	if err != nil {
		t.Fatal(err)
	}
	defer oldListener.Close()
	hostListener, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: hostPath, Net: "unixgram"})
	if err != nil {
		t.Fatal(err)
	}
	defer hostListener.Close()
	oldInfo, err := os.Stat(oldPath)
	if err != nil {
		t.Fatal(err)
	}
	hostInfo, err := os.Stat(hostPath)
	if err != nil {
		t.Fatal(err)
	}
	oldHostPath, oldStat := localoci.NANControlSocketHostPath, nanSocketStat
	localoci.NANControlSocketHostPath = hostPath
	mountedInfo := oldInfo
	nanSocketStat = func(path string) (os.FileInfo, error) {
		if path == "/proc/1234/root"+localoci.NANControlSocketContainerPath {
			return mountedInfo, nil
		}
		return os.Stat(path)
	}
	t.Cleanup(func() { localoci.NANControlSocketHostPath, nanSocketStat = oldHostPath, oldStat })

	for _, tc := range []struct {
		name      string
		labels    map[string]string
		running   bool
		wantStale bool
	}{
		{"nan", map[string]string{appconfig.EntitlementAnnotationKeyPrefix + appconfig.EntitlementNAN: "{}"}, true, true},
		{"ordinary app", map[string]string{}, true, false},
		{"explicitly stopped", map[string]string{appconfig.EntitlementAnnotationKeyPrefix + appconfig.EntitlementNAN: "{}", labelKeyStoppedByUser: "true"}, true, false},
		{"stopped task", map[string]string{appconfig.EntitlementAnnotationKeyPrefix + appconfig.EntitlementNAN: "{}"}, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			labels := map[string]string{labelKeyAppID: "camera", labelKeyAppVersion: "1"}
			for key, value := range tc.labels {
				labels[key] = value
			}
			record := containers.Container{ID: "camera", Labels: labels}
			var taskClient tasksapi.TasksClient = stoppedNANTaskClient{}
			if tc.running {
				taskClient = ros2TestTaskClient{running: map[string]uint32{"camera": 1234}}
			}
			remote, err := containerd.New("", containerd.WithServices(
				containerd.WithContainerStore(nanLifecycleStore{records: []containers.Container{record}}),
				containerd.WithTaskClient(taskClient),
			))
			if err != nil {
				t.Fatal(err)
			}
			defer remote.Close()
			client := &Client{client: remote, namespace: "wendy", logger: zap.NewNop()}
			stale, err := client.StaleNANSocketContainers(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if got := len(stale) == 1 && stale[0] == "camera"; got != tc.wantStale {
				t.Fatalf("stale containers = %v, want stale=%t", stale, tc.wantStale)
			}
			mountedInfo = hostInfo
			fresh, err := client.StaleNANSocketContainers(context.Background())
			if err != nil || len(fresh) != 0 {
				t.Fatalf("fresh socket: %v, %v", fresh, err)
			}
			mountedInfo = oldInfo
		})
	}
	localoci.NANControlSocketHostPath = filepath.Join(dir, "missing")
	client := nanLifecycleClient(t, "camera", true)
	stale, err := client.StaleNANSocketContainers(context.Background())
	if err != nil || len(stale) != 0 {
		t.Fatalf("missing host socket restarted app: %v, %v", stale, err)
	}
}

type nanLifecycleStore struct {
	containers.Store
	records []containers.Container
}

type stoppedNANTaskClient struct{ tasksapi.TasksClient }

func (stoppedNANTaskClient) Get(context.Context, *tasksapi.GetRequest, ...grpc.CallOption) (*tasksapi.GetResponse, error) {
	return nil, status.Error(codes.NotFound, "no running task")
}

func (s nanLifecycleStore) List(context.Context, ...string) ([]containers.Container, error) {
	return s.records, nil
}

func (s nanLifecycleStore) Get(_ context.Context, id string) (containers.Container, error) {
	for _, record := range s.records {
		if record.ID == id {
			return record, nil
		}
	}
	return containers.Container{}, errdefs.ErrNotFound
}

func nanLifecycleClient(t *testing.T, appID string, running bool) *Client {
	t.Helper()
	record := containers.Container{ID: appID, Labels: map[string]string{
		labelKeyAppID: appID,
		appconfig.EntitlementAnnotationKeyPrefix + appconfig.EntitlementNAN: "{}",
	}}
	var taskClient tasksapi.TasksClient = stoppedNANTaskClient{}
	if running {
		taskClient = ros2TestTaskClient{running: map[string]uint32{appID: 1234}}
	}
	remote, err := containerd.New("", containerd.WithServices(
		containerd.WithContainerStore(nanLifecycleStore{records: []containers.Container{record}}),
		containerd.WithTaskClient(taskClient),
	))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = remote.Close() })
	return &Client{client: remote, namespace: "wendy", logger: zap.NewNop()}
}

func TestNANAppNDIIsStableAndAppScoped(t *testing.T) {
	a := nanAppNDI("com.example.camera")
	if a != nanAppNDI("com.example.camera") {
		t.Fatal("app NDI name changed between calls")
	}
	if a == nanAppNDI("com.example.other") {
		t.Fatal("different apps share an NDI name")
	}
	if !regexp.MustCompile(`^wa[0-9a-f]{12}$`).MatchString(a) || len(a) > 15 {
		t.Fatalf("app NDI %q is not a valid reserved Linux interface name", a)
	}
	if a == "wnanndi0" {
		t.Fatal("app NDI overlaps mesh provider NDI")
	}
}

func TestFailedNANContainerCreateReleasesOrphanNDI(t *testing.T) {
	const appID = "com.example.camera"
	oldRoot, oldHelper := nanClientRootPath, runNANHelper
	nanClientRootPath = t.TempDir()
	var calls [][]string
	runNANHelper = func(_ context.Context, args ...string) error {
		calls = append(calls, append([]string(nil), args...))
		return nil
	}
	t.Cleanup(func() {
		nanClientRootPath, runNANHelper = oldRoot, oldHelper
	})
	clientDir := nanAppClientDir(appID)
	if err := os.MkdirAll(clientDir, 0o700); err != nil {
		t.Fatal(err)
	}
	client := newROS2ContainerTestClient(t, nil, nil)
	lease := nanCreateLease{client: client, ctx: client.withNamespace(context.Background()), appID: appID, attempted: true}
	createErr := errors.New("forced OCI spec validation failure")
	lease.finish(&createErr)
	if createErr == nil || createErr.Error() != "forced OCI spec validation failure" {
		t.Fatalf("rollback lost the original create failure: %v", createErr)
	}
	if len(calls) != 1 || len(calls[0]) != 2 || calls[0][0] != "ndi-remove" || calls[0][1] != nanAppNDI(appID) {
		t.Fatalf("failed create did not remove its app NDI: %v", calls)
	}
	if _, err := os.Stat(clientDir); !os.IsNotExist(err) {
		t.Fatalf("failed create left client socket directory: %v", err)
	}
}

func TestCommittedNANContainerCreateKeepsNDI(t *testing.T) {
	oldHelper := runNANHelper
	runNANHelper = func(_ context.Context, args ...string) error {
		t.Fatalf("committed container released NDI: %v", args)
		return nil
	}
	t.Cleanup(func() { runNANHelper = oldHelper })
	lease := nanCreateLease{attempted: true, committed: true}
	var createErr error
	lease.finish(&createErr)
}

func TestFailedNANRedeployKeepsRunningSiblingNDI(t *testing.T) {
	const appID = "com.example.camera"
	oldHelper := runNANHelper
	runNANHelper = func(_ context.Context, args ...string) error {
		t.Fatalf("running NAN app NDI removed during failed redeploy: %v", args)
		return nil
	}
	t.Cleanup(func() { runNANHelper = oldHelper })
	client := nanLifecycleClient(t, appID, true)
	lease := nanCreateLease{client: client, ctx: client.withNamespace(context.Background()), appID: appID, attempted: true}
	createErr := errors.New("forced OCI spec validation failure")
	lease.finish(&createErr)
	if createErr.Error() != "forced OCI spec validation failure" {
		t.Fatalf("cleanup changed original failure: %v", createErr)
	}
}

func TestStoppedNANAppReleasesItsNDI(t *testing.T) {
	const appID = "com.example.camera"
	oldRoot, oldHelper := nanClientRootPath, runNANHelper
	nanClientRootPath = t.TempDir()
	removed := ""
	runNANHelper = func(_ context.Context, args ...string) error {
		if len(args) != 2 || args[0] != "ndi-remove" {
			t.Fatalf("unexpected NAN helper command: %v", args)
		}
		removed = args[1]
		return nil
	}
	t.Cleanup(func() { nanClientRootPath, runNANHelper = oldRoot, oldHelper })
	clientDir := nanAppClientDir(appID)
	if err := os.MkdirAll(clientDir, 0o700); err != nil {
		t.Fatal(err)
	}
	client := nanLifecycleClient(t, appID, false)
	if err := client.releaseNANIfIdle(client.withNamespace(context.Background()), appID); err != nil {
		t.Fatalf("releaseNANIfIdle: %T %v", err, err)
	}
	if removed != nanAppNDI(appID) {
		t.Fatalf("stopped app NDI removal = %q", removed)
	}
	if _, err := os.Stat(clientDir); !os.IsNotExist(err) {
		t.Fatalf("stopped app left client socket directory: %v", err)
	}
}

func TestNANEntitlementDetection(t *testing.T) {
	if hasNANEntitlement([]appconfig.Entitlement{{Type: appconfig.EntitlementNetwork}}) {
		t.Fatal("network alone must not grant NAN control")
	}
	if !hasNANEntitlement([]appconfig.Entitlement{{Type: appconfig.EntitlementNetwork}, {Type: appconfig.EntitlementNAN}}) {
		t.Fatal("NAN entitlement was not detected")
	}
}
