package models_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/models"
	"github.com/wendylabsinc/wendy/go/internal/agent/models/modelstest"
)

const (
	frontDoor = "v4l2:/dev/video0"
	garage    = "v4l2:/dev/video2"
)

type harness struct {
	sup     *models.Supervisor
	clock   *modelstest.Clock
	runtime *modelstest.Runtime
	cameras *modelstest.Cameras
	files   *modelstest.Files
	root    string
}

func newHarness(t *testing.T, engine string, configure ...func(*models.Config)) *harness {
	t.Helper()
	h := &harness{
		clock:   modelstest.NewClock(),
		runtime: modelstest.NewRuntime(),
		cameras: modelstest.NewCameras(models.Camera{SourceID: frontDoor, Name: "front door"}, models.Camera{SourceID: garage, Name: "garage"}),
		files:   modelstest.NewFiles(),
		root:    t.TempDir(),
	}
	cfg := models.Config{
		Catalog: modelstest.Catalog(engine),
		Device:  models.DeviceProfile{Arch: "arm64", GPUArch: "sm_87"},
		Runtime: h.runtime, Cameras: h.cameras, Files: h.files, Root: h.root, Clock: h.clock,
	}
	for _, c := range configure {
		c(&cfg)
	}
	h.sup = models.NewSupervisor(cfg)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		h.sup.Shutdown(ctx)
	})
	return h
}

// info returns the instance's snapshot; State is zero once it is gone.
func (h *harness) info(id string) models.InstanceInfo {
	for _, i := range h.sup.List() {
		if i.ID == id {
			return i
		}
	}
	return models.InstanceInfo{}
}

// startReady starts the detector on camera and has the host report ready.
func (h *harness) startReady(t *testing.T, camera string) models.InstanceInfo {
	t.Helper()
	info, _, err := h.sup.Start(context.Background(), "coco-detector", camera)
	if err != nil {
		t.Fatal(err)
	}
	modelstest.Eventually(t, "the host to start", func() bool { return h.runtime.Running(info.ID) })
	h.sup.PublishApplicationRecord(models.AppIDPrefix+info.ID, modelstest.Status(models.HostReady))
	modelstest.Eventually(t, "the instance to be ready", func() bool { return h.info(info.ID).State == models.StateReady })
	return h.info(info.ID)
}

func TestStartRunsHostAndBecomesReady(t *testing.T) {
	h := newHarness(t, models.EngineONNXRuntime)
	info, reused, err := h.sup.Start(context.Background(), "coco-detector", frontDoor)
	if err != nil || reused {
		t.Fatalf("Start = %+v, %v, %v", info, reused, err)
	}
	if info.State != models.StatePreparing {
		t.Fatalf("state = %v, want preparing", info.State)
	}
	modelstest.Eventually(t, "the host to start", func() bool { return h.runtime.Running(info.ID) })

	spec := h.runtime.LastSpec()
	if spec.AppID != models.AppIDPrefix+info.ID || spec.CameraNode != "/dev/video255" || spec.CameraSource != frontDoor {
		t.Fatalf("spec = %+v", spec)
	}
	if spec.EngineCache != "" {
		t.Fatalf("an ONNX Runtime host got an engine cache: %q", spec.EngineCache)
	}
	if labels, err := os.ReadFile(spec.LabelsFile); err != nil || string(labels) != "person\ncar\ndog\n" {
		t.Fatalf("labels = %q, %v", labels, err)
	}
	if !slices.Contains(spec.Env(), "WENDY_MODEL_FILE="+models.HostModelFile) {
		t.Fatalf("env = %v", spec.Env())
	}

	h.sup.PublishApplicationRecord(models.AppIDPrefix+info.ID, modelstest.Status(models.HostReady))
	modelstest.Eventually(t, "ready", func() bool { return h.info(info.ID).State == models.StateReady })
	if got := h.info(info.ID).Stats.ProcessedFPS; got != 9.5 {
		t.Fatalf("processed fps = %v, want the host's 9.5", got)
	}
}

func TestStartReusesInstanceAndEnforcesCapacity(t *testing.T) {
	h := newHarness(t, models.EngineONNXRuntime, func(c *models.Config) { c.MaxRunning = 1 })
	ctx := context.Background()
	first, _, err := h.sup.Start(ctx, "coco-detector", frontDoor)
	if err != nil {
		t.Fatal(err)
	}
	again, reused, err := h.sup.Start(ctx, "coco-detector", frontDoor)
	if err != nil || !reused || again.ID != first.ID {
		t.Fatalf("second start = %+v, reused=%v, %v; want the same instance", again, reused, err)
	}
	if _, _, err := h.sup.Start(ctx, "coco-detector", garage); !errors.Is(err, models.ErrCapacity) {
		t.Fatalf("start past capacity = %v, want ErrCapacity", err)
	}
}

func TestConcurrentStartsShareOneInstance(t *testing.T) {
	h := newHarness(t, models.EngineONNXRuntime)
	ids := make([]string, 8)
	var wg sync.WaitGroup
	for i := range ids {
		wg.Add(1)
		go func() {
			defer wg.Done()
			info, _, err := h.sup.Start(context.Background(), "coco-detector", frontDoor)
			if err != nil {
				t.Error(err)
				return
			}
			ids[i] = info.ID
		}()
	}
	wg.Wait()
	for _, id := range ids {
		if id != ids[0] {
			t.Fatalf("concurrent starts made more than one instance: %v", ids)
		}
	}
	modelstest.Eventually(t, "the host to start", func() bool { return h.runtime.Running(ids[0]) })
	if n := len(h.sup.List()); n != 1 {
		t.Fatalf("%d instances, want 1", n)
	}
}

func TestStartRejectsWhatCannotRun(t *testing.T) {
	h := newHarness(t, models.EngineONNXRuntime)
	ctx := context.Background()
	if _, _, err := h.sup.Start(ctx, "no-such-model", frontDoor); !errors.Is(err, models.ErrUnknownModel) {
		t.Fatalf("unknown model: %v", err)
	}
	if _, _, err := h.sup.Start(ctx, "coco-detector", "v4l2:/dev/video9"); !errors.Is(err, models.ErrUnknownCamera) {
		t.Fatalf("unknown camera: %v", err)
	}
	amd := newHarness(t, models.EngineONNXRuntime, func(c *models.Config) {
		c.Catalog.Models[0].Variants[0].Requires = models.Requires{Arch: "arm64"}
		c.Device.Arch = "amd64"
	})
	if _, _, err := amd.sup.Start(ctx, "coco-detector", frontDoor); !errors.Is(err, models.ErrNoVariant) {
		t.Fatalf("no variant: %v", err)
	}
}

func TestRefusedCameraDoesNotTakeASlot(t *testing.T) {
	h := newHarness(t, models.EngineONNXRuntime, func(c *models.Config) { c.MaxRunning = 1 })
	h.cameras.Refuse(frontDoor)
	if _, _, err := h.sup.Start(context.Background(), "coco-detector", frontDoor); !errors.Is(err, models.ErrCameraNotStreamable) {
		t.Fatalf("refused camera: %v", err)
	}
	if n := len(h.sup.List()); n != 0 {
		t.Fatalf("a refused start left %d instances", n)
	}
	if owners := h.cameras.Owners(); len(owners) != 0 {
		t.Fatalf("a refused start left camera pins: %v", owners)
	}
	if _, _, err := h.sup.Start(context.Background(), "coco-detector", garage); err != nil {
		t.Fatalf("the only slot was lost to a refused start: %v", err)
	}
}

func TestStopRemovesHostCameraAndRunDir(t *testing.T) {
	h := newHarness(t, models.EngineONNXRuntime)
	info := h.startReady(t, frontDoor)
	final, err := h.sup.Stop(context.Background(), info.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if final.State != models.StateStopped || final.StateDetail != "stopped" {
		t.Fatalf("final = %v %q", final.State, final.StateDetail)
	}
	if !slices.Contains(h.runtime.Removed(), info.ID) {
		t.Fatal("the host container was not removed")
	}
	if len(h.cameras.Owners()) != 0 {
		t.Fatal("the camera pin was not released")
	}
	if _, err := os.Stat(filepath.Join(h.root, "run", info.ID)); !os.IsNotExist(err) {
		t.Fatalf("the run directory remains: %v", err)
	}
	if len(h.sup.List()) != 0 {
		t.Fatal("a stopped instance is still listed")
	}
	if _, err := h.sup.Stop(context.Background(), info.ID, ""); !errors.Is(err, models.ErrUnknownInstance) {
		t.Fatalf("second stop: %v", err)
	}
}

func TestStopDuringImagePullCancelsIt(t *testing.T) {
	h := newHarness(t, models.EngineONNXRuntime)
	h.runtime.BlockEnsure = make(chan struct{}) // the pull never finishes on its own
	info, _, err := h.sup.Start(context.Background(), "coco-detector", frontDoor)
	if err != nil {
		t.Fatal(err)
	}
	modelstest.Eventually(t, "the pull to begin", func() bool { return h.info(info.ID).StateDetail == "pulling host image" })
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	final, err := h.sup.Stop(ctx, info.ID, "")
	if err != nil {
		t.Fatalf("stop did not interrupt the pull: %v", err)
	}
	if final.State != models.StateStopped {
		t.Fatalf("final state = %v", final.State)
	}
	if h.runtime.Starts(info.ID) != 0 {
		t.Fatal("a host started after the instance was stopped")
	}
}

func TestHostReportedFailureRemovesInstance(t *testing.T) {
	h := newHarness(t, models.EngineONNXRuntime)
	info := h.startReady(t, frontDoor)
	h.sup.PublishApplicationRecord(models.AppIDPrefix+info.ID, modelstest.Failed("no frames from camera"))
	modelstest.Eventually(t, "the failed instance to go", func() bool { return h.info(info.ID).State == 0 })
	if !slices.Contains(h.runtime.Removed(), info.ID) {
		t.Fatal("the failed host was not removed")
	}
}

func TestCatalogReportsFitAndFirstStartCost(t *testing.T) {
	h := newHarness(t, models.EngineONNXRuntime)
	view := h.sup.Catalog(context.Background())
	if len(view.Models) != 1 || view.Models[0].Variant == nil {
		t.Fatalf("view = %+v", view)
	}
	entry := view.Models[0]
	if entry.ImageCached || entry.DownloadBytes != 10 || entry.NeedsEngineBuild {
		t.Fatalf("first-start cost = %+v", entry)
	}
	if len(view.Cameras) != 2 || view.MaxRunning != 2 || view.Running != 0 {
		t.Fatalf("view = %+v", view)
	}
	h.startReady(t, frontDoor)
	entry = h.sup.Catalog(context.Background()).Models[0]
	if !entry.ImageCached || entry.DownloadBytes != 0 {
		t.Fatalf("after a start = %+v", entry)
	}
}

// TestStartsHeldForARefusedCameraAllFail holds several concurrent Starts of
// the same model and camera behind one in-flight, ultimately refused,
// Acquire. None of them may be handed the doomed instance as "reused".
func TestStartsHeldForARefusedCameraAllFail(t *testing.T) {
	h := newHarness(t, models.EngineONNXRuntime)
	h.cameras.Refuse(frontDoor)
	h.cameras.BlockAcquire = make(chan struct{})

	const n = 5
	reused := make([]bool, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, r, err := h.sup.Start(context.Background(), "coco-detector", frontDoor)
			reused[i], errs[i] = r, err
		}()
	}
	modelstest.Eventually(t, "the first start to be held", func() bool { return len(h.sup.List()) == 1 })
	time.Sleep(20 * time.Millisecond) // give the rest a moment to queue behind it
	close(h.cameras.BlockAcquire)
	wg.Wait()

	for i := range n {
		if reused[i] {
			t.Errorf("start %d: reused=true for a camera that was refused", i)
		}
		if !errors.Is(errs[i], models.ErrCameraNotStreamable) {
			t.Errorf("start %d: err = %v, want ErrCameraNotStreamable", i, errs[i])
		}
	}
	if got := len(h.sup.List()); got != 0 {
		t.Fatalf("%d instances left after every start was refused", got)
	}
	if owners := h.cameras.Owners(); len(owners) != 0 {
		t.Fatalf("camera pins left: %v", owners)
	}
}

// TestStartDuringTeardownStartsAfresh starts a fresh instance while a failed
// one for the same model and camera is still tearing down: the caller must
// wait, not be handed the dying instance.
func TestStartDuringTeardownStartsAfresh(t *testing.T) {
	h := newHarness(t, models.EngineONNXRuntime)
	info := h.startReady(t, frontDoor)
	h.runtime.BlockRemove = make(chan struct{})
	h.sup.PublishApplicationRecord(models.AppIDPrefix+info.ID, modelstest.Failed("no frames from camera"))
	modelstest.Eventually(t, "teardown to begin", func() bool { return h.runtime.Removing(info.ID) })

	done := make(chan struct{})
	var fresh models.InstanceInfo
	var reused bool
	var err error
	go func() {
		fresh, reused, err = h.sup.Start(context.Background(), "coco-detector", frontDoor)
		close(done)
	}()
	select {
	case <-done:
		t.Fatal("start returned before the failing instance finished tearing down")
	case <-time.After(50 * time.Millisecond):
	}

	close(h.runtime.BlockRemove)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("start did not return after teardown finished")
	}
	if err != nil {
		t.Fatal(err)
	}
	if reused {
		t.Fatal("start reused the failing instance")
	}
	if fresh.ID == info.ID {
		t.Fatal("start returned the same instance id as the failing one")
	}
}
