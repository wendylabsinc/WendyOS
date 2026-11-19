package services

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/wendylabsinc/wendy/go/internal/agent/camera"
	"github.com/wendylabsinc/wendy/go/internal/agent/ipcam"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

// csCamera is one camera the share manager's tests put in /dev.
type csCamera struct {
	base    string
	name    string
	formats []uint32
	id      camera.Identity
}

func csPort(port string) string { return "/sys/devices/platform/usb1/" + port + "/" + port + ":1.0" }

func csBrio() csCamera {
	return csCamera{"video0", "Brio 101", []uint32{v4l2PixFmtYUYV, v4l2PixFmtMJPEG},
		camera.Identity{Device: csPort("1-2.1"), Index: "0", Name: "Brio 101"}}
}

func csH264Cam() csCamera {
	return csCamera{"video2", "Logi H264", []uint32{v4l2PixFmtYUYV, v4l2PixFmtH264},
		camera.Identity{Device: csPort("1-2.2"), Index: "0", Name: "Logi H264"}}
}

func csC920() csCamera {
	return csCamera{"video4", "C920", []uint32{v4l2PixFmtYUYV, v4l2PixFmtMJPEG},
		camera.Identity{Device: csPort("1-2.3"), Index: "0", Name: "C920"}}
}

func csCam6() csCamera {
	return csCamera{"video6", "Cam Six", []uint32{v4l2PixFmtYUYV, v4l2PixFmtMJPEG},
		camera.Identity{Device: csPort("1-2.4"), Index: "0", Name: "Cam Six"}}
}

// csNode is a share node writer double: the pump's fake node, plus Close.
type csNode struct {
	*spFakeNode
	h      *csHarness
	path   string
	mode   sharedMode
	closes atomic.Int32
}

func (n *csNode) Close() error {
	n.closes.Add(1)
	n.h.event("close " + n.path)
	return nil
}

type csHarness struct {
	t    *testing.T
	s    *VideoService
	loop *fakeLoopback
	logs *observer.ObservedLogs

	mu       sync.Mutex
	cams     map[string]csCamera // by base
	opened   []*csNode
	openErr  map[string]error
	openHook func(path string)
	events   []string
	probed   []string // paths hasVideoCapture or capturePixelFormats saw
}

// newCSHarness builds a service whose /dev holds cams, whose loopback is the
// recording fake, whose share nodes open as csNodes, and whose state file is
// in a temporary directory.
func newCSHarness(t *testing.T, cams ...csCamera) *csHarness {
	t.Helper()
	s := shareTestService(t, []uint32{v4l2PixFmtYUYV, v4l2PixFmtMJPEG}, brioSizes(), []uint32{30})
	h := &csHarness{t: t, s: s, loop: s.loopback.(*fakeLoopback), cams: map[string]csCamera{}, openErr: map[string]error{}}
	for _, c := range cams {
		h.cams[c.base] = c
	}
	h.loop.onRemove = func(nr int) { h.event(fmt.Sprintf("remove %d", nr)) }

	prevPath, prevOpen := cameraSharesPath, openShareNode
	cameraSharesPath = filepath.Join(t.TempDir(), "camera-shares.json")
	openShareNode = h.open
	t.Cleanup(func() { cameraSharesPath, openShareNode = prevPath, prevOpen })

	core, logs := observer.New(zap.DebugLevel)
	h.logs = logs
	s.logger = zap.New(core)
	s.ctx, s.cancel = context.WithCancel(context.Background())
	s.hubs = make(map[string]*deviceHub)
	s.sampleSeqs = make(map[string]*atomic.Uint64)
	s.enumerateLibcamera = func(context.Context) (map[string]string, error) { return nil, nil }
	s.readStableNames = func() map[string]stableNames { return nil }
	s.classifyTransport = func(string) (camera.Transport, string) { return camera.TransportUSB, "uvcvideo" }
	s.globDevices = func() ([]string, error) {
		var paths []string
		h.mu.Lock()
		for base := range h.cams {
			paths = append(paths, "/dev/"+base)
		}
		h.mu.Unlock()
		for _, nr := range h.loop.liveAux() {
			paths = append(paths, fmt.Sprintf("/dev/video%d", nr))
		}
		sort.Strings(paths)
		return paths, nil
	}
	s.hasVideoCapture = func(path string) bool { h.probe(path); return true }
	s.capturePixelFormats = func(path string) ([]uint32, bool) {
		h.probe(path)
		h.mu.Lock()
		defer h.mu.Unlock()
		c, ok := h.cams[filepath.Base(path)]
		return c.formats, ok
	}
	s.readDeviceName = func(base string) (string, error) {
		h.mu.Lock()
		c, ok := h.cams[base]
		h.mu.Unlock()
		if ok {
			return c.name, nil
		}
		var nr int
		if _, err := fmt.Sscanf(base, "video%d", &nr); err == nil {
			if label, ok := h.loop.auxLabel(nr); ok {
				return label, nil
			}
		}
		return "", os.ErrNotExist
	}
	s.cameraIdentity = func(base string) (camera.Identity, error) {
		h.mu.Lock()
		defer h.mu.Unlock()
		c, ok := h.cams[base]
		if !ok {
			return camera.Identity{}, os.ErrNotExist
		}
		return c.id, nil
	}
	s.presentCameras = func() map[string]camera.Identity {
		h.mu.Lock()
		defer h.mu.Unlock()
		out := map[string]camera.Identity{}
		for base, c := range h.cams {
			out[base] = c.id
		}
		return out
	}
	t.Cleanup(s.Shutdown)
	return h
}

func (h *csHarness) open(_ *zap.Logger, path string, mode sharedMode) (shareNodeIO, error) {
	h.mu.Lock()
	hook, err := h.openHook, h.openErr[path]
	h.mu.Unlock()
	if hook != nil {
		hook(path)
	}
	h.event("open " + path)
	if err != nil {
		return nil, err
	}
	n := &csNode{spFakeNode: newSpFakeNode(), h: h, path: path, mode: mode}
	h.mu.Lock()
	h.opened = append(h.opened, n)
	h.mu.Unlock()
	return n, nil
}

func (h *csHarness) event(e string) {
	h.mu.Lock()
	h.events = append(h.events, e)
	h.mu.Unlock()
}

func (h *csHarness) probe(path string) {
	h.mu.Lock()
	h.probed = append(h.probed, path)
	h.mu.Unlock()
}

func (h *csHarness) openedNodes() []*csNode {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]*csNode(nil), h.opened...)
}

func (h *csHarness) eventLog() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.events...)
}

func (h *csHarness) logged(substr string) int {
	n := 0
	for _, e := range h.logs.All() {
		if strings.Contains(e.Message, substr) {
			n++
		}
	}
	return n
}

func (h *csHarness) records(containerID string) []*shareRecord {
	h.s.shares.mu.Lock()
	defer h.s.shares.mu.Unlock()
	c := h.s.shares.containers[containerID]
	if c == nil {
		return nil
	}
	return append([]*shareRecord(nil), c.shares...)
}

func (h *csHarness) containerIDs() []string {
	h.s.shares.mu.Lock()
	defer h.s.shares.mu.Unlock()
	var ids []string
	for id := range h.s.shares.containers {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// stateFile reads the state file back as plain JSON.
func (h *csHarness) stateFile() map[string]any {
	h.t.Helper()
	b, err := os.ReadFile(cameraSharesPath)
	if err != nil {
		h.t.Fatalf("reading the state file: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		h.t.Fatalf("state file %s does not parse: %v", b, err)
	}
	return out
}

func (h *csHarness) writeState(content string) {
	h.t.Helper()
	if err := os.WriteFile(cameraSharesPath, []byte(content), 0o644); err != nil {
		h.t.Fatal(err)
	}
}

// csStateRecord is one share in the state file's JSON, for writing fixtures.
func csStateRecord(c csCamera, path string, mode sharedMode, node int) string {
	return fmt.Sprintf(`{"camera":{"device":%q,"index":%q,"name":%q},"path":%q,`+
		`"mode":{"width":%d,"height":%d,"fourcc":%q,"bytesPerLine":%d,"fps":%d},"node":%d}`,
		c.id.Device, c.id.Index, c.id.Name, path, mode.Width, mode.Height, mode.Fourcc, mode.BytesPerLine, mode.FPS, node)
}

func TestCreateCameraShares_OneNodePerSharedCameraLabelledWithItsName(t *testing.T) {
	h := newCSHarness(t, csBrio(), csH264Cam())
	h.s.CreateCameraShares(context.Background(), "cv-a", "sh.wendy.cv-a")

	if got, want := h.loop.shareCalls(), []auxCall{{255, "Brio 101"}}; !reflect.DeepEqual(got, want) {
		t.Errorf("EnsureShareNode calls = %+v, want %+v", got, want)
	}
	if got := h.loop.auxCreatedCount(); got != 0 {
		t.Errorf("EnsureAuxNode called %d times; share nodes come from EnsureShareNode", got)
	}
	nodes := h.openedNodes()
	if len(nodes) != 1 || nodes[0].path != "/dev/video255" || nodes[0].mode != (sharedMode{1280, 720, "YUYV", 2560, 30}) {
		t.Fatalf("opened = %+v, want one /dev/video255 at 1280x720 YUYV 2560 30", nodes)
	}
	if n := h.logged("camera share: cv-a shares Brio 101 (/dev/video0) through /dev/video255 at 1280x720 YUYV, 30 fps"); n != 1 {
		t.Errorf("shares line logged %d times, want 1; logs: %v", n, h.logs.All())
	}
	if n := h.logged("camera share: cv-a gets the real camera Logi H264 (/dev/video2): the camera produces H.264 itself; sharing it comes in a later release"); n != 1 {
		t.Errorf("H.264 line logged %d times, want 1; logs: %v", n, h.logs.All())
	}
	if _, apps, ok := h.s.sharedModeOf("/dev/video0"); !ok || !reflect.DeepEqual(apps, []string{"cv-a"}) {
		t.Errorf("sharedModeOf(/dev/video0) = %v, %v; want [cv-a]", apps, ok)
	}
	if _, _, ok := h.s.sharedModeOf("/dev/video2"); ok {
		t.Error("the H.264 camera is shared")
	}

	state := h.stateFile()
	want := map[string]any{
		"version": float64(1),
		"containers": map[string]any{"cv-a": map[string]any{
			"app": "sh.wendy.cv-a",
			"shares": []any{map[string]any{
				"camera": map[string]any{"device": csPort("1-2.1"), "index": "0", "name": "Brio 101"},
				"path":   "/dev/video0",
				"mode": map[string]any{"width": float64(1280), "height": float64(720), "fourcc": "YUYV",
					"bytesPerLine": float64(2560), "fps": float64(30)},
				"node": float64(255),
			}},
		}},
	}
	if !reflect.DeepEqual(state, want) {
		t.Errorf("state file = %+v\nwant %+v", state, want)
	}
	if info, err := os.Stat(cameraSharesPath); err != nil || info.Mode().Perm() != 0o644 {
		t.Errorf("state file mode = %v, %v; want 0644", info.Mode().Perm(), err)
	}
}

func TestCreateCameraShares_NoFreeNodeGivesTheRealCamera(t *testing.T) {
	h := newCSHarness(t, csBrio())
	h.loop.auxErr = ipcam.ErrBandExhausted
	h.s.CreateCameraShares(context.Background(), "cv-a", "sh.wendy.cv-a")

	if n := h.logged("camera share: cv-a gets the real camera Brio 101 (/dev/video0): no free v4l2loopback node number (200-255 are in use)"); n != 1 {
		t.Errorf("no-free-node line logged %d times, want 1; logs: %v", n, h.logs.All())
	}
	if len(h.loop.shareCalls()) != 0 || len(h.openedNodes()) != 0 || len(h.records("cv-a")) != 0 {
		t.Errorf("something was made: nodes %v, opened %v, records %v", h.loop.shareCalls(), h.openedNodes(), h.records("cv-a"))
	}
	if _, _, ok := h.s.sharedModeOf("/dev/video0"); ok {
		t.Error("the camera is shared without a node")
	}
}

func TestCreateCameraShares_NoModuleSaysSoOnce(t *testing.T) {
	h := newCSHarness(t, csBrio(), csC920())
	h.loop.availableErr = errors.New("no module")
	h.s.CreateCameraShares(context.Background(), "cv-a", "sh.wendy.cv-a")

	if n := h.logged("camera share: cv-a gets the real cameras: no compatible v4l2loopback module on this device"); n != 1 {
		t.Errorf("no-module line logged %d times, want 1; logs: %v", n, h.logs.All())
	}
	if n := len(h.logs.All()); n != 1 {
		t.Errorf("%d lines logged, want only the no-module line: %v", n, h.logs.All())
	}
}

func TestCreateCameraShares_NodeThatWillNotOpenIsRemoved(t *testing.T) {
	h := newCSHarness(t, csBrio())
	h.openErr["/dev/video255"] = errors.New("VIDIOC_S_FMT on /dev/video255: device or resource busy")
	h.s.CreateCameraShares(context.Background(), "cv-a", "sh.wendy.cv-a")

	if got, want := h.eventLog(), []string{"open /dev/video255", "remove 255"}; !reflect.DeepEqual(got, want) {
		t.Errorf("events = %v, want %v", got, want)
	}
	if n := h.logged("camera share: cv-a gets the real camera Brio 101 (/dev/video0): VIDIOC_S_FMT on /dev/video255: device or resource busy"); n != 1 {
		t.Errorf("open-failure line logged %d times, want 1; logs: %v", n, h.logs.All())
	}
	if len(h.records("cv-a")) != 0 || len(h.loop.liveAux()) != 0 {
		t.Errorf("records %v, live nodes %v; want none", h.records("cv-a"), h.loop.liveAux())
	}
}

func TestCreateCameraShares_StaysWithinItsBudget(t *testing.T) {
	outOfTime := fmt.Sprintf("camera sharing setup ran out of time (%s)", cameraShareCreateBudget)

	t.Run("a decision that blocks", func(t *testing.T) {
		h := newCSHarness(t, csBrio(), csC920())
		release, returned := make(chan struct{}), make(chan struct{})
		var releaseOnce sync.Once
		releaseAll := func() {
			releaseOnce.Do(func() { close(release) })
			select {
			case <-returned:
			case <-time.After(6 * time.Second):
			}
		}
		t.Cleanup(releaseAll)
		prev := frameRatesFor
		frameRatesFor = func(path string, pixfmt, w, ht uint32) []uint32 {
			if path == "/dev/video4" {
				defer close(returned)
				select {
				case <-release:
				case <-time.After(5 * time.Second):
				}
			}
			return prev(path, pixfmt, w, ht)
		}

		start := time.Now()
		h.s.CreateCameraShares(context.Background(), "cv-a", "sh.wendy.cv-a")
		if elapsed := time.Since(start); elapsed > 2200*time.Millisecond {
			t.Errorf("CreateCameraShares took %s, want at most 2.2s", elapsed)
		}
		if n := h.logged("camera share: cv-a shares Brio 101 (/dev/video0) through /dev/video255"); n != 1 {
			t.Errorf("the share made in time is not there; logs: %v", h.logs.All())
		}
		if n := h.logged("camera share: cv-a gets the real camera C920 (/dev/video4): " + outOfTime); n != 1 {
			t.Errorf("out-of-time line logged %d times, want 1; logs: %v", n, h.logs.All())
		}

		releaseAll()
		time.Sleep(20 * time.Millisecond)
		if got := h.loop.liveAux(); !reflect.DeepEqual(got, []int{255}) {
			t.Errorf("live nodes = %v, want only 255", got)
		}
		if n := len(h.openedNodes()); n != 1 || len(h.records("cv-a")) != 1 {
			t.Errorf("opened %d nodes, %d records; want the one share made in time", n, len(h.records("cv-a")))
		}
	})

	t.Run("a node made after the budget", func(t *testing.T) {
		h := newCSHarness(t, csBrio(), csC920(), csCam6())
		start := time.Now()
		h.openHook = func(path string) {
			if path == "/dev/video254" {
				time.Sleep(time.Until(start.Add(cameraShareCreateBudget + 50*time.Millisecond)))
			}
		}
		h.s.CreateCameraShares(context.Background(), "cv-a", "sh.wendy.cv-a")
		if elapsed := time.Since(start); elapsed > 2200*time.Millisecond {
			t.Errorf("CreateCameraShares took %s, want at most 2.2s", elapsed)
		}
		if got := h.loop.liveAux(); !reflect.DeepEqual(got, []int{255}) {
			t.Errorf("live nodes = %v, want only 255: the half-made node is removed", got)
		}
		for _, n := range h.openedNodes() {
			if n.path == "/dev/video254" && n.closes.Load() != 1 {
				t.Errorf("the half-made node's writer closed %d times, want 1", n.closes.Load())
			}
		}
		for _, line := range []string{
			"camera share: cv-a gets the real camera C920 (/dev/video4): " + outOfTime,
			"camera share: cv-a gets the real camera Cam Six (/dev/video6): " + outOfTime,
		} {
			if n := h.logged(line); n != 1 {
				t.Errorf("%q logged %d times, want 1; logs: %v", line, n, h.logs.All())
			}
		}
		if n := len(h.records("cv-a")); n != 1 {
			t.Errorf("%d records, want the one share made in time", n)
		}
	})
}

func TestRemoveCameraShares_StopsPumpsRemovesNodesAndForgets(t *testing.T) {
	h := newCSHarness(t, csBrio(), csC920())
	h.s.CreateCameraShares(context.Background(), "cv-a", "sh.wendy.cv-a")
	recs := h.records("cv-a")
	if len(recs) != 2 {
		t.Fatalf("%d records, want 2", len(recs))
	}
	h.mu.Lock()
	h.events = nil
	h.mu.Unlock()

	h.s.RemoveCameraShares("cv-a")

	for _, r := range recs {
		select {
		case <-r.done:
		default:
			t.Errorf("the pump of node %d is still running", r.node)
		}
	}
	if got, want := h.eventLog(), []string{"close /dev/video255", "remove 255", "close /dev/video254", "remove 254"}; !reflect.DeepEqual(got, want) {
		t.Errorf("events = %v, want %v", got, want)
	}
	for _, n := range h.openedNodes() {
		if n.closes.Load() != 1 {
			t.Errorf("%s closed %d times, want 1", n.path, n.closes.Load())
		}
	}
	if ids := h.containerIDs(); len(ids) != 0 {
		t.Errorf("containers left: %v", ids)
	}
	if _, _, ok := h.s.sharedModeOf("/dev/video0"); ok {
		t.Error("/dev/video0 is still shared")
	}
	if containers := h.stateFile()["containers"].(map[string]any); len(containers) != 0 {
		t.Errorf("state file still holds %v", containers)
	}

	h.s.RemoveCameraShares("cv-a")
	h.s.RemoveCameraShares("never-made")
	if got := h.eventLog(); len(got) != 4 {
		t.Errorf("removing unknown containers did something: %v", got)
	}
}

func TestCameraShares_RedeployCyclesLeakNothing(t *testing.T) {
	h := newCSHarness(t, csBrio())
	for i := 0; i < 60; i++ {
		h.s.CreateCameraShares(context.Background(), "cv-a", "sh.wendy.cv-a")
		if n := len(h.records("cv-a")); n != 1 {
			t.Fatalf("cycle %d: %d records, want 1", i, n)
		}
		h.s.RemoveCameraShares("cv-a")
	}
	if got := h.loop.liveAux(); len(got) != 0 {
		t.Errorf("nodes left after 60 redeploys: %v", got)
	}
	for _, c := range h.loop.shareCalls() {
		if c.nr != 255 {
			t.Fatalf("a redeploy used node %d; every one should reuse 255", c.nr)
		}
	}
	if n := len(h.loop.shareCalls()); n != 60 {
		t.Errorf("%d nodes made, want 60", n)
	}
	if ids := h.containerIDs(); len(ids) != 0 {
		t.Errorf("records left: %v", ids)
	}
	if containers := h.stateFile()["containers"].(map[string]any); len(containers) != 0 {
		t.Errorf("state file still holds %v", containers)
	}
	for _, n := range h.openedNodes() {
		if n.closes.Load() != 1 {
			t.Fatalf("%s closed %d times, want 1", n.path, n.closes.Load())
		}
	}
}

func TestSyncCameraShares_AdoptsNodesAfterAgentRestart(t *testing.T) {
	h := newCSHarness(t, csBrio())
	// A mode the policy would not pick today: adoption must reopen the node at
	// the mode its writer locked in, never a recomputed one.
	stored := sharedMode{Width: 640, Height: 480, Fourcc: "YUYV", BytesPerLine: 1280, FPS: 15}
	h.writeState(`{"version":1,"containers":{"cv-a":{"app":"sh.wendy.cv-a","shares":[` +
		csStateRecord(csBrio(), "/dev/video0", stored, 254) + `]}}}`)
	h.loop.addLiveAux(254, "Brio 101")
	h.s.loadCameraShares()

	if !h.s.IsShareNode("/dev/video254") {
		t.Error("a node whose record is loaded but not adopted is not a share node")
	}

	h.s.SyncCameraShares(context.Background(), []CameraShareContainer{{ID: "cv-a", Running: true, PID: 4242}})

	nodes := h.openedNodes()
	if len(nodes) != 1 || nodes[0].path != "/dev/video254" || nodes[0].mode != stored {
		t.Fatalf("opened %+v, want /dev/video254 at the stored mode %v", nodes, stored)
	}
	if len(h.loop.shareCalls()) != 0 || h.loop.auxCreatedCount() != 0 || len(h.loop.removedAux()) != 0 || len(h.loop.allocations()) != 0 {
		t.Errorf("adoption made or removed nodes: share %v, aux %d, removed %v, allocated %v",
			h.loop.shareCalls(), h.loop.auxCreatedCount(), h.loop.removedAux(), h.loop.allocations())
	}
	// An adopted pump blanks an idle node once.
	nodes[0].send(t, 0)
	if e := nodes[0].next(t); e.kind != "blank" {
		t.Errorf("first write on the adopted idle node = %q, want blank", e.kind)
	}

	h.s.SyncCameraShares(context.Background(), []CameraShareContainer{{ID: "cv-a", Running: true, PID: 4242}})
	if n := len(h.openedNodes()); n != 1 {
		t.Errorf("a second sync opened the node again (%d opens)", n)
	}
	if recs := h.records("cv-a"); len(recs) != 1 || recs[0].node != 254 || recs[0].pump == nil {
		t.Errorf("records = %+v, want node 254 with a pump", recs)
	}
}

func TestSyncCameraShares_LeavesANodeLostInARebootForTheStart(t *testing.T) {
	h := newCSHarness(t, csBrio())
	h.writeState(`{"version":1,"containers":{"cv-a":{"app":"sh.wendy.cv-a","shares":[` +
		csStateRecord(csBrio(), "/dev/video0", testSharedMode, 254) + `]}}}`)
	h.s.loadCameraShares()

	h.s.SyncCameraShares(context.Background(), []CameraShareContainer{{ID: "cv-a"}})

	if len(h.openedNodes()) != 0 || len(h.loop.shareCalls()) != 0 || len(h.loop.removedAux()) != 0 {
		t.Errorf("sync acted on a node that is gone: opened %v, made %v, removed %v",
			h.openedNodes(), h.loop.shareCalls(), h.loop.removedAux())
	}
	if recs := h.records("cv-a"); len(recs) != 1 || recs[0].pump != nil {
		t.Errorf("records = %+v, want the record kept without a pump", recs)
	}
}

func TestSyncCameraShares_DropsContainersDeletedWhileAgentWasDown(t *testing.T) {
	h := newCSHarness(t, csBrio(), csC920())
	h.writeState(`{"version":1,"containers":{` +
		`"cv-gone":{"app":"sh.wendy.cv-gone","shares":[` + csStateRecord(csBrio(), "/dev/video0", testSharedMode, 254) + `]},` +
		`"cv-gone-rebooted":{"app":"sh.wendy.cv-gone-rebooted","shares":[` + csStateRecord(csBrio(), "/dev/video0", testSharedMode, 250) + `]},` +
		`"cv-a":{"app":"sh.wendy.cv-a","shares":[` + csStateRecord(csC920(), "/dev/video4", testSharedMode, 253) + `]}}}`)
	h.loop.addLiveAux(254, "Brio 101")
	h.loop.addLiveAux(253, "C920")
	h.s.loadCameraShares()

	wide := &agentpb.StreamVideoRequest{Width: 1920, Height: 1080}
	if _, err := h.s.applySharedMode("/dev/video0", wide); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("before the sync, 1920x1080 on /dev/video0 = %v, want the shared-mode refusal", err)
	}

	h.s.SyncCameraShares(context.Background(), []CameraShareContainer{{ID: "cv-a", Running: true, PID: 7}})

	if got := h.loop.removedAux(); !reflect.DeepEqual(got, []int{254}) {
		t.Errorf("removed nodes = %v, want [254] (250 no longer exists)", got)
	}
	if ids := h.containerIDs(); !reflect.DeepEqual(ids, []string{"cv-a"}) {
		t.Errorf("containers = %v, want [cv-a]", ids)
	}
	containers := h.stateFile()["containers"].(map[string]any)
	if _, ok := containers["cv-gone"]; ok || len(containers) != 1 {
		t.Errorf("state file containers = %v, want cv-a only", containers)
	}
	got, err := h.s.applySharedMode("/dev/video0", wide)
	if err != nil || got != wide {
		t.Errorf("after the sync, 1920x1080 on /dev/video0 = %v, %v; want it admitted untouched", got, err)
	}
	if h.s.IsShareNode("/dev/video254") {
		t.Error("/dev/video254 is still a share node")
	}
}

func TestSyncCameraShares_KeepsSharesCreatedAfterItsListing(t *testing.T) {
	h := newCSHarness(t, csBrio())
	h.s.CreateCameraShares(context.Background(), "cv-new", "sh.wendy.cv-new")
	// A sync whose container listing was taken just before cv-new existed.
	h.s.SyncCameraShares(context.Background(), nil)
	if n := len(h.records("cv-new")); n != 1 || len(h.loop.removedAux()) != 0 {
		t.Errorf("sync dropped a container created after its listing: %d records, removed %v", n, h.loop.removedAux())
	}
}

func TestShutdown_StopsSharePumpsAndKeepsNodesForTheNextAgent(t *testing.T) {
	h := newCSHarness(t, csBrio())
	h.s.CreateCameraShares(context.Background(), "cv-a", "sh.wendy.cv-a")
	recs := h.records("cv-a")
	if len(recs) != 1 {
		t.Fatalf("%d records, want 1", len(recs))
	}
	done := recs[0].done

	h.s.Shutdown()

	select {
	case <-done:
	default:
		t.Error("the pump is still running after Shutdown")
	}
	if n := h.openedNodes()[0].closes.Load(); n != 1 {
		t.Errorf("writer closed %d times, want 1", n)
	}
	if got := h.loop.removedAux(); len(got) != 0 {
		t.Errorf("Shutdown removed nodes %v; the next agent adopts them", got)
	}
	if got := h.loop.liveAux(); !reflect.DeepEqual(got, []int{255}) {
		t.Errorf("live nodes = %v, want [255]", got)
	}
	if recs := h.records("cv-a"); len(recs) != 1 || recs[0].pump != nil {
		t.Errorf("records after Shutdown = %+v, want one without a pump", recs)
	}
	if _, ok := h.stateFile()["containers"].(map[string]any)["cv-a"]; !ok {
		t.Error("the state file lost cv-a")
	}
	h.s.Shutdown()
	if n := h.openedNodes()[0].closes.Load(); n != 1 {
		t.Errorf("a second Shutdown closed the writer again (%d closes)", n)
	}
}

func TestCameraShareState_SurvivesReload(t *testing.T) {
	h := newCSHarness(t, csBrio(), csC920())
	h.s.CreateCameraShares(context.Background(), "cv-a", "sh.wendy.cv-a")
	h.s.CreateCameraShares(context.Background(), "cv-b", "sh.wendy.cv-b")

	type rec struct {
		App, Name, Path string
		Camera          camera.Identity
		Mode            sharedMode
		Node            int
	}
	snapshot := func(s *VideoService) map[string][]rec {
		s.shares.mu.Lock()
		defer s.shares.mu.Unlock()
		out := map[string][]rec{}
		for id, c := range s.shares.containers {
			for _, r := range c.shares {
				out[id] = append(out[id], rec{c.app, r.cameraName, r.cameraPath, r.camera, r.mode, r.node})
			}
		}
		return out
	}
	before := snapshot(h.s)
	if len(before["cv-a"]) != 2 || len(before["cv-b"]) != 2 {
		t.Fatalf("records before reload = %+v", before)
	}

	reloaded := &VideoService{logger: zap.NewNop()}
	reloaded.loadCameraShares()
	if after := snapshot(reloaded); !reflect.DeepEqual(after, before) {
		t.Errorf("after reload = %+v\nwant %+v", after, before)
	}
	reloaded.shares.mu.Lock()
	for _, c := range reloaded.shares.containers {
		for _, r := range c.shares {
			if r.pump != nil || r.io != nil {
				t.Errorf("a reloaded record has a pump or writer: %+v", r)
			}
		}
	}
	reloaded.shares.mu.Unlock()

	t.Run("a file that does not parse is set aside", func(t *testing.T) {
		h.writeState(`{"version":1,"containers":`)
		core, logs := observer.New(zap.DebugLevel)
		s := &VideoService{logger: zap.New(core)}
		s.loadCameraShares()
		if len(s.shares.containers) != 0 {
			t.Errorf("records from a corrupt file: %+v", s.shares.containers)
		}
		if _, err := os.Stat(cameraSharesPath + ".corrupt"); err != nil {
			t.Errorf("no .corrupt file: %v", err)
		}
		if _, err := os.Stat(cameraSharesPath); !os.IsNotExist(err) {
			t.Errorf("the corrupt file is still in place: %v", err)
		}
		if logs.FilterMessageSnippet("camera-shares.json.corrupt").Len() != 1 {
			t.Errorf("the set-aside is not logged: %v", logs.All())
		}
	})
}

func TestListCameras_NeverProbesShareNodes(t *testing.T) {
	h := newCSHarness(t, csBrio())
	h.writeState(`{"version":1,"containers":{"cv-a":{"app":"sh.wendy.cv-a","shares":[` +
		csStateRecord(csBrio(), "/dev/video0", testSharedMode, 254) + `]}}}`)
	h.loop.addLiveAux(254, "Brio 101")
	h.loop.addLiveAux(253, "Brio 101")            // a share node whose record never got written
	h.loop.addLiveAux(252, "Wendy IP camera 252") // not a share node
	h.s.loadCameraShares()

	devices, err := h.s.listCameras(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(devices) != 1 || devices[0].GetPath() != "/dev/video0" {
		t.Errorf("listCameras = %v, want /dev/video0 only", devices)
	}
	h.s.CreateCameraShares(context.Background(), "cv-b", "sh.wendy.cv-b")
	h.s.ensureTwoPlaneForLocalCameras(context.Background())

	h.mu.Lock()
	probed := append([]string(nil), h.probed...)
	h.mu.Unlock()
	for _, p := range probed {
		if p != "/dev/video0" {
			t.Errorf("%s was probed; only the real camera may be", p)
		}
	}
	for path, want := range map[string]bool{
		"/dev/video254": true,  // loaded record, not adopted
		"/dev/video255": true,  // cv-b's node
		"/dev/video253": true,  // in the band, named like a shared camera
		"/dev/video252": false, // in the band, another name
		"/dev/video0":   false,
		"/dev/video60":  false,
		"/dev/null":     false,
	} {
		if got := h.s.IsShareNode(path); got != want {
			t.Errorf("IsShareNode(%s) = %v, want %v", path, got, want)
		}
	}
}

func TestResolveShareCamera_NeverPicksAShareNodeOrAnAmbiguousCamera(t *testing.T) {
	brio := csBrio()
	moved := csCamera{"video2", "Brio 101", brio.formats, camera.Identity{Device: csPort("1-3"), Index: "0", Name: "Brio 101"}}
	other := camera.Identity{Device: csPort("1-4"), Index: "0", Name: "Brio 101"}

	t.Run("a moved camera is followed and its path recorded", func(t *testing.T) {
		h := newCSHarness(t, brio)
		h.s.CreateCameraShares(context.Background(), "cv-a", "sh.wendy.cv-a")
		h.mu.Lock()
		delete(h.cams, "video0")
		h.cams["video2"] = moved
		h.mu.Unlock()
		r := h.records("cv-a")[0]
		if path, ok := h.s.currentSharePath(r); !ok || path != "/dev/video2" {
			t.Fatalf("currentSharePath = %q, %v; want /dev/video2", path, ok)
		}
		if _, _, ok := h.s.sharedModeOf("/dev/video2"); !ok {
			t.Error("the shared-mode rules did not follow the camera to /dev/video2")
		}
		shares := h.stateFile()["containers"].(map[string]any)["cv-a"].(map[string]any)["shares"].([]any)
		if p := shares[0].(map[string]any)["path"]; p != "/dev/video2" {
			t.Errorf("state file path = %v, want /dev/video2", p)
		}
	})

	t.Run("two same-model cameras both gone: a third claims neither", func(t *testing.T) {
		h := newCSHarness(t, moved)
		h.writeState(`{"version":1,"containers":{"cv-a":{"app":"a","shares":[` +
			csStateRecord(brio, "/dev/video0", testSharedMode, 254) + `,` +
			csStateRecord(csCamera{id: other}, "/dev/video4", testSharedMode, 253) + `]}}}`)
		h.s.loadCameraShares()
		for _, r := range h.records("cv-a") {
			if path, ok := h.s.currentSharePath(r); ok {
				t.Errorf("record for %s resolved to %s; the camera could be either", r.camera.Device, path)
			}
		}
	})

	t.Run("a share node is never the camera", func(t *testing.T) {
		h := newCSHarness(t, brio)
		h.s.CreateCameraShares(context.Background(), "cv-a", "sh.wendy.cv-a")
		h.mu.Lock()
		delete(h.cams, "video0")
		// As if the loopback node had a device link: same name and index.
		h.cams["video255"] = csCamera{"video255", "Brio 101", brio.formats,
			camera.Identity{Device: "/sys/devices/virtual/video4linux/video255", Index: "0", Name: "Brio 101"}}
		h.mu.Unlock()
		if path, ok := h.s.currentSharePath(h.records("cv-a")[0]); ok {
			t.Errorf("resolved to %s, the share node itself", path)
		}
	})
}
