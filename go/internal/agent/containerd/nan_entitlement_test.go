package containerd

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"testing"
	"time"

	tasksapi "github.com/containerd/containerd/api/services/tasks/v1"
	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/containers"
	"github.com/containerd/errdefs"
	"github.com/wendylabsinc/wendy/go/internal/agent/nancontrol"
	localoci "github.com/wendylabsinc/wendy/go/internal/agent/oci"
	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func nanTestSocketPath(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "nan-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, "nan0")
}

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

func TestNANPreparationWaitsForProviderManagementTransaction(t *testing.T) {
	unlock, err := nancontrol.Lock(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := prepareNANEntitlement(ctx, "camera"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("app prepared during provider reset: %v", err)
	}
}

func TestNANSocketPermissionRestoreWaitsForFinalReservation(t *testing.T) {
	oldRoot, oldHelper, oldRestore := nanClientRootPath, runNANHelper, restoreNANSocketAccess
	nanClientRootPath = t.TempDir()
	runNANHelper = func(context.Context, ...string) error { return nil }
	restored := 0
	restoreNANSocketAccess = func(string) error { restored++; return nil }
	t.Cleanup(func() { nanClientRootPath, runNANHelper, restoreNANSocketAccess = oldRoot, oldHelper, oldRestore })
	for _, app := range []string{"camera", "sensor"} {
		if err := os.Mkdir(nanAppClientDir(app), 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := releaseNANEntitlement(context.Background(), "camera"); err != nil {
		t.Fatal(err)
	}
	if restored != 0 {
		t.Fatal("a second app's prepared reservation lost socket access")
	}
	if err := releaseNANEntitlement(context.Background(), "sensor"); err != nil {
		t.Fatal(err)
	}
	if restored != 1 {
		t.Fatalf("final reservation restored permissions %d times", restored)
	}
}

func TestNANMonitorRetriesPermissionRestoreAfterFinalDelete(t *testing.T) {
	oldRoot, oldRestore, oldPath := nanClientRootPath, restoreNANSocketAccess, localoci.NANControlSocketHostPath
	nanClientRootPath = t.TempDir()
	localoci.NANControlSocketHostPath = nanTestSocketPath(t)
	conn, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: localoci.NANControlSocketHostPath, Net: "unixgram"})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	calls := 0
	restoreNANSocketAccess = func(string) error {
		calls++
		if calls == 1 {
			return errors.New("temporary restore failure")
		}
		return nil
	}
	t.Cleanup(func() {
		nanClientRootPath, restoreNANSocketAccess, localoci.NANControlSocketHostPath = oldRoot, oldRestore, oldPath
	})
	c := newROS2ContainerTestClient(t, nil, nil)
	for range 2 {
		if _, err := c.StaleNANSocketContainers(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 2 {
		t.Fatalf("permission restore retried %d times", calls)
	}
}

func TestNANValidationRetiresOnlyUnusedPreparation(t *testing.T) {
	for _, tc := range []struct {
		name         string
		running      bool
		cleanupFails bool
	}{
		{name: "unused preparation"},
		{name: "running sibling", running: true},
		{name: "cleanup failure", cleanupFails: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const appID = "com.example.validate"
			oldRoot, oldHelper := nanClientRootPath, runNANHelper
			nanClientRootPath = t.TempDir()
			var calls [][]string
			cleanupErr := errors.New("NDI removal failed")
			runNANHelper = func(_ context.Context, args ...string) error {
				calls = append(calls, append([]string(nil), args...))
				if tc.cleanupFails {
					return cleanupErr
				}
				return nil
			}
			t.Cleanup(func() { nanClientRootPath, runNANHelper = oldRoot, oldHelper })
			dir := nanAppClientDir(appID)
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			client := nanLifecycleClient(t, appID, tc.running)
			lease := nanCreateLease{client: client, ctx: client.withNamespace(context.Background()), appID: appID, attempted: true, validating: true}
			var validationErr error
			lease.finish(&validationErr)
			if errors.Is(validationErr, cleanupErr) != tc.cleanupFails {
				t.Fatalf("validation result = %v", validationErr)
			}
			if tc.running {
				if len(calls) != 0 {
					t.Fatalf("validation removed a running sibling's NDI: %v", calls)
				}
			} else if len(calls) != 1 || len(calls[0]) != 2 || calls[0][0] != "ndi-remove" || calls[0][1] != nanAppNDI(appID) {
				t.Fatalf("validation NDI retirement = %v", calls)
			}
			_, err := os.Stat(dir)
			if (tc.running || tc.cleanupFails) && err != nil {
				t.Fatalf("live or failed-cleanup directory was removed: %v", err)
			}
			if !tc.running && !tc.cleanupFails && !os.IsNotExist(err) {
				t.Fatalf("unused validation directory remains: %v", err)
			}
		})
	}
}
