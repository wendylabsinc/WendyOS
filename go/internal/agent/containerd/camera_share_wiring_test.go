package containerd

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/containers"
	"github.com/containerd/containerd/v2/pkg/cio"
	"github.com/containerd/errdefs"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"github.com/wendylabsinc/wendy/go/internal/agent/services"
	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
)

// shareCall is one call the client made on its camera share provider.
type shareCall struct {
	op          string // "create", "cover", "release" or "remove"
	containerID string
	appID       string // create only
	pid         uint32 // cover only
}

// fakeCameraShares records every call the client makes on its camera share
// provider, in the order the calls returned. cover, when set, runs inside
// CoverCameraShares before it returns, and its errors are returned.
type fakeCameraShares struct {
	mu     sync.Mutex
	calls  []shareCall
	syncs  [][]services.CameraShareContainer
	cover  func(containerID string, pid uint32) []error
	remove func(containerID string) // runs inside RemoveCameraShares
}

var _ CameraShareProvider = (*fakeCameraShares)(nil)

func (f *fakeCameraShares) setCover(cover func(containerID string, pid uint32) []error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cover = cover
}

func (f *fakeCameraShares) record(call shareCall) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call)
}

func (f *fakeCameraShares) CreateCameraShares(_ context.Context, containerID, appID string) {
	f.record(shareCall{op: "create", containerID: containerID, appID: appID})
}

func (f *fakeCameraShares) CoverCameraShares(_ context.Context, containerID string, pid uint32) []error {
	f.mu.Lock()
	cover := f.cover
	f.mu.Unlock()
	var errs []error
	if cover != nil {
		errs = cover(containerID, pid)
	}
	f.record(shareCall{op: "cover", containerID: containerID, pid: pid})
	return errs
}

func (f *fakeCameraShares) ReleaseCameraShares(containerID string) {
	f.record(shareCall{op: "release", containerID: containerID})
}

func (f *fakeCameraShares) RemoveCameraShares(containerID string) {
	f.mu.Lock()
	remove := f.remove
	f.mu.Unlock()
	if remove != nil {
		remove(containerID)
	}
	f.record(shareCall{op: "remove", containerID: containerID})
}

func (f *fakeCameraShares) setRemove(remove func(containerID string)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.remove = remove
}

func (f *fakeCameraShares) SyncCameraShares(_ context.Context, containers []services.CameraShareContainer) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.syncs = append(f.syncs, append([]services.CameraShareContainer(nil), containers...))
}

// ops returns, in order, the operations made for containerID, keeping only
// the kinds named (all kinds when none are).
func (f *fakeCameraShares) ops(containerID string, kinds ...string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	keep := map[string]bool{}
	for _, k := range kinds {
		keep[k] = true
	}
	var out []string
	for _, call := range f.calls {
		if call.containerID == containerID && (len(keep) == 0 || keep[call.op]) {
			out = append(out, call.op)
		}
	}
	return out
}

func (f *fakeCameraShares) callsFor(containerID string) []shareCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []shareCall
	for _, call := range f.calls {
		if call.containerID == containerID {
			out = append(out, call)
		}
	}
	return out
}

func (f *fakeCameraShares) allSyncs() [][]services.CameraShareContainer {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][]services.CameraShareContainer(nil), f.syncs...)
}

// The label is the R4 marker: set at create for every camera app, to how it
// gets its camera, and absent for an app without a camera.
func TestCameraShareLabel(t *testing.T) {
	cases := []struct {
		name         string
		entitlements []appconfig.Entitlement
		want         string
		labelled     bool
	}{
		{"camera", []appconfig.Entitlement{{Type: appconfig.EntitlementCamera}}, "shared", true},
		{"camera with another mode", []appconfig.Entitlement{{Type: appconfig.EntitlementCamera, Mode: "Direct"}}, "shared", true},
		{"camera direct", []appconfig.Entitlement{{Type: appconfig.EntitlementCamera, Mode: "direct"}}, "direct", true},
		{"video direct", []appconfig.Entitlement{{Type: appconfig.EntitlementVideo, Mode: "direct"}}, "direct", true},
		{"video", []appconfig.Entitlement{{Type: appconfig.EntitlementVideo}}, "shared", true},
		{"none", nil, "", false},
		{"no camera", []appconfig.Entitlement{{Type: appconfig.EntitlementGPU}}, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			labels := map[string]string{labelKeyAppID: "app"}
			addCameraShareLabel(labels, tc.entitlements)
			got, ok := labels[labelKeyCameraShare]
			if ok != tc.labelled || got != tc.want {
				t.Fatalf("label %q = %q (present %v), want %q (present %v)", labelKeyCameraShare, got, ok, tc.want, tc.labelled)
			}
			if sharesCamera(labels) != (tc.want == "shared") {
				t.Fatalf("sharesCamera(%v) = %v", labels, sharesCamera(labels))
			}
		})
	}
}

// Only containers labelled "shared" reach the share manager's sync. A running
// one carries its task's pid; one not running carries none. Containers made
// before sharing existed have no label and are left out, whatever their
// entitlements.
func TestCameraShareContainers(t *testing.T) {
	shared := map[string]string{labelKeyAppID: "a", labelKeyCameraShare: "shared"}
	infos := []containerCameraInfo{
		{name: "shared-running", labels: shared, running: true, pid: 4242},
		{name: "shared-stopped", labels: shared, running: false},
		{name: "shared-created", labels: shared, running: false, pid: 99},
		{name: "direct", labels: map[string]string{labelKeyAppID: "d", labelKeyCameraShare: "direct"}, running: true, pid: 7},
		{name: "pre-existing", labels: map[string]string{labelKeyAppID: "p", "sh.wendy/entitlement.camera": `{"type":"camera"}`}, running: true, pid: 9},
		{name: "odd-value", labels: map[string]string{labelKeyAppID: "o", labelKeyCameraShare: "Shared"}, running: true, pid: 10},
		{name: "plain", labels: map[string]string{labelKeyAppID: "x"}, running: true, pid: 11},
	}
	got := cameraShareContainers(infos)
	want := []services.CameraShareContainer{
		{ID: "shared-running", Running: true, PID: 4242},
		{ID: "shared-stopped"},
		{ID: "shared-created"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("cameraShareContainers = %+v, want %+v", got, want)
	}
}

// A listing that missed a container must not reach the sync: the share
// manager would take the missing container for deleted and remove its nodes.
func TestCameraShareSyncSkipsAnIncompleteListing(t *testing.T) {
	fake := &fakeCameraShares{}
	c := &Client{logger: zap.NewNop()}
	c.SetCameraShareProvider(fake)
	infos := []containerCameraInfo{
		{name: "shared", labels: map[string]string{labelKeyCameraShare: "shared"}, running: true, pid: 5},
		{name: "plain", labels: map[string]string{}},
	}

	c.syncCameraShares(context.Background(), infos, false)
	if syncs := fake.allSyncs(); len(syncs) != 0 {
		t.Fatalf("an incomplete listing was synced: %+v", syncs)
	}

	c.syncCameraShares(context.Background(), infos, true)
	want := [][]services.CameraShareContainer{{{ID: "shared", Running: true, PID: 5}}}
	if syncs := fake.allSyncs(); !reflect.DeepEqual(syncs, want) {
		t.Fatalf("syncs = %+v, want %+v", syncs, want)
	}
}

// cameraInfoContainerForTest is a listed container whose Info returns labels
// or infoErr, and which has no task.
type cameraInfoContainerForTest struct {
	containerd.Container
	id      string
	labels  map[string]string
	infoErr error
}

func (f cameraInfoContainerForTest) ID() string { return f.id }

func (f cameraInfoContainerForTest) Info(context.Context, ...containerd.InfoOpts) (containers.Container, error) {
	if f.infoErr != nil {
		return containers.Container{}, f.infoErr
	}
	return containers.Container{ID: f.id, Labels: f.labels}, nil
}

func (f cameraInfoContainerForTest) Task(context.Context, cio.Attach) (containerd.Task, error) {
	return nil, errdefs.ErrNotFound
}

// A container deleted between the listing and its read is gone either way, so
// the sync still runs without it; one that exists but cannot be read stops the
// share sync, since the share manager would take it for deleted.
func TestCameraShareSyncSkipsOnlyUnreadableContainers(t *testing.T) {
	shared := map[string]string{labelKeyAppID: "a", labelKeyCameraShare: "shared"}
	deleted := cameraInfoContainerForTest{id: "deleted", infoErr: fmt.Errorf("container %q: %w", "deleted", errdefs.ErrNotFound)}
	kept := cameraInfoContainerForTest{id: "kept", labels: shared}
	unreadable := cameraInfoContainerForTest{id: "unreadable", infoErr: errors.New("connection reset")}

	fake := &fakeCameraShares{}
	c := &Client{logger: zap.NewNop()}
	c.SetCameraShareProvider(fake)
	infos, complete := c.cameraInfos(context.Background(), []containerd.Container{deleted, kept})
	if !complete {
		t.Fatal("a container deleted since the listing marked the listing incomplete")
	}
	c.syncCameraShares(context.Background(), infos, complete)
	want := [][]services.CameraShareContainer{{{ID: "kept"}}}
	if syncs := fake.allSyncs(); !reflect.DeepEqual(syncs, want) {
		t.Fatalf("syncs = %+v, want %+v", syncs, want)
	}

	if _, complete := c.cameraInfos(context.Background(), []containerd.Container{kept, unreadable, deleted}); complete {
		t.Fatal("a container that exists but could not be read left the listing complete")
	}
}

// Every cover error is logged with the app's name, and none is returned: the
// start goes ahead with the real camera.
func TestCameraShareCoverLogsEachErrorWithTheAppName(t *testing.T) {
	core, logs := observer.New(zapcore.DebugLevel)
	fake := &fakeCameraShares{cover: func(string, uint32) []error {
		return []error{errors.New("setns failed"), errors.New("timed out")}
	}}
	c := &Client{logger: zap.New(core)}
	c.SetCameraShareProvider(fake)

	if !c.coverCameraShares(context.Background(), "app_svc", "app_svc", 77) {
		t.Fatal("coverCameraShares reported no cover with a provider set")
	}
	if got := fake.callsFor("app_svc"); !reflect.DeepEqual(got, []shareCall{{op: "cover", containerID: "app_svc", pid: 77}}) {
		t.Fatalf("calls = %+v", got)
	}
	entries := logs.FilterField(zap.String("app_name", "app_svc")).All()
	if len(entries) != 2 {
		t.Fatalf("logged %d entries with app_name, want one per error: %+v", len(entries), logs.All())
	}
	for i, want := range []string{"setns failed", "timed out"} {
		if got := entries[i].ContextMap()["error"]; got != want {
			t.Fatalf("entry %d error = %v, want %q", i, got, want)
		}
	}
}

// Without a provider (no video service, other platforms) every share call is
// a no-op.
func TestCameraShareCallsWithoutAProviderDoNothing(t *testing.T) {
	c := &Client{logger: zap.NewNop()}
	ctx := context.Background()
	c.createCameraShares(ctx, "app", "app")
	if c.coverCameraShares(ctx, "app", "app", 1) {
		t.Fatal("coverCameraShares reported a cover without a provider")
	}
	c.releaseCameraShares("app")
	c.removeCameraShares("app")
	c.syncCameraShares(ctx, []containerCameraInfo{{name: "app", labels: map[string]string{labelKeyCameraShare: "shared"}}}, true)
	// Neither provider set: the sync must return before touching containerd.
	c.SyncCameraLoopbacks(ctx)
}

// A nil *VideoService (main.go without a video service) is stored as no
// provider, not as an interface holding a nil pointer that would pass every
// nil check and then panic (WDY-3835's class of bug).
func TestCameraShareProviderNilVideoService(t *testing.T) {
	c := &Client{logger: zap.NewNop()}
	c.SetCameraShareProvider((*services.VideoService)(nil))
	if c.cameraShareProvider != nil {
		t.Fatalf("cameraShareProvider = %#v, want nil", c.cameraShareProvider)
	}
	c.removeCameraShares("app")
}
