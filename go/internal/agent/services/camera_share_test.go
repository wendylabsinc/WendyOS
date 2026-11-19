package services

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
	"golang.org/x/sys/unix"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/wendylabsinc/wendy/go/internal/agent/camera"
	"github.com/wendylabsinc/wendy/go/internal/agent/devcover"
	"github.com/wendylabsinc/wendy/go/internal/agent/devwatch"
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

// csClock is a fake removal clock: After moves it forward by d at once, so a
// removal's settle wait runs in no real time and its fake time is exact. With
// frozen set, After never fires.
type csClock struct {
	mu     sync.Mutex
	now    time.Time
	frozen bool
}

func (c *csClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *csClock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.frozen {
		return nil
	}
	c.now = c.now.Add(d)
	ch := make(chan time.Time, 1)
	ch <- c.now
	return ch
}

func (c *csClock) setFrozen(frozen bool) {
	c.mu.Lock()
	c.frozen = frozen
	c.mu.Unlock()
}

type csHarness struct {
	t     *testing.T
	s     *VideoService
	loop  *fakeLoopback
	logs  *observer.ObservedLogs
	clock *csClock

	mu        sync.Mutex
	cams      map[string]csCamera // by base
	opened    []*csNode
	openErr   map[string]error
	openHook  func(path string)
	events    []string
	probed    []string // paths hasVideoCapture or capturePixelFormats saw
	udev      func(nr int) bool
	held      map[string][]deviceHolder // by path
	holderErr error                     // the holder scan fails with it
	removedAt map[int]time.Time         // fake time of each node's RemoveAuxNode

	// Covering (Task 9). Each fake namespace stands for one pid, with the
	// inode 1000+pid; the agent's own is csAgentNS, init's csInitNS.
	namespaces map[*devcover.Namespace]uint32 // the pid each was opened for
	nsOpenErr  error
	hostNSErr  error    // reading the host's namespaces fails with it
	nsClosed   []uint32 // the pids of the namespaces closed, in order
	closeHook  func(pid uint32)
	covers     []csCover
	coverErr   map[string]error // by the covered path
	coverHook  func(src, dst string)
	covered    bool // what Covered reports
	watcher    *csWatcher

	// A node's /dev entry: giving it udev's access, unlinking it before a
	// removal and making it again fail with these; entryOps records each in
	// order (apart from events, whose exact sequences other tests check).
	accessErr, unlinkErr, restoreErr error
	entryOps                         []string
}

// csRdev is the fake device number of node nr: major 81, minor nr.
func csRdev(nr int) uint64 { return unix.Mkdev(81, uint32(nr)) }

func (h *csHarness) entryOp(op string) {
	h.mu.Lock()
	h.entryOps = append(h.entryOps, op)
	h.mu.Unlock()
}

func (h *csHarness) entryLog() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.entryOps...)
}

func (h *csHarness) setEntryErrs(access, unlink, restore error) {
	h.mu.Lock()
	h.accessErr, h.unlinkErr, h.restoreErr = access, unlink, restore
	h.mu.Unlock()
}

// csAgentNS and csInitNS are the inodes of the agent's and init's mount
// namespaces in the harness: the host's.
const (
	csAgentNS = 1
	csInitNS  = 2
)

func csNSInode(pid uint32) uint64 { return 1000 + uint64(pid) }

// csCover is one cover the fake coverPath applied.
type csCover struct {
	pid      uint32
	src, dst string
	timeout  time.Duration
}

// csWatcher is the injected /dev watcher: the test feeds it events. Its first
// fails calls fail at once, as a watch that could not be set up.
type csWatcher struct {
	mu      sync.Mutex
	onEvent func(devwatch.Event)
	dir     string
	fails   int
	calls   int
	ready   chan struct{}
	once    sync.Once
}

func (h *csHarness) setHolderErr(err error) {
	h.mu.Lock()
	h.holderErr = err
	h.mu.Unlock()
}

// vanish takes a node away as something outside the agent would.
func (h *csHarness) vanish(nr int) {
	h.loop.mu.Lock()
	delete(h.loop.aux, nr)
	h.loop.mu.Unlock()
}

// newCSHarness builds a service whose /dev holds cams, whose loopback is the
// recording fake, whose share nodes open as csNodes, and whose state file is
// in a temporary directory.
func newCSHarness(t *testing.T, cams ...csCamera) *csHarness {
	t.Helper()
	s := shareTestService(t, []uint32{v4l2PixFmtYUYV, v4l2PixFmtMJPEG}, brioSizes(), []uint32{30})
	h := &csHarness{t: t, s: s, loop: s.loopback.(*fakeLoopback), cams: map[string]csCamera{}, openErr: map[string]error{},
		clock: &csClock{now: time.Unix(1_000_000, 0)}, held: map[string][]deviceHolder{}, removedAt: map[int]time.Time{},
		namespaces: map[*devcover.Namespace]uint32{}, coverErr: map[string]error{}, covered: true,
		watcher: &csWatcher{ready: make(chan struct{})}}
	for _, c := range cams {
		h.cams[c.base] = c
	}
	h.loop.onRemove = func(nr int) {
		h.event(fmt.Sprintf("remove %d", nr))
		h.entryOp(fmt.Sprintf("remove %d", nr))
		h.mu.Lock()
		h.removedAt[nr] = h.clock.Now()
		h.mu.Unlock()
	}

	prevPath, prevOpen := cameraSharesPath, openShareNode
	prevClock, prevUdev, prevHolders := shareRemovalClock, shareNodeUdevDone, shareNodeHolders
	cameraSharesPath = filepath.Join(t.TempDir(), "camera-shares.json")
	openShareNode = h.open
	shareRemovalClock = h.clock
	shareNodeUdevDone = func(nr int) bool {
		h.mu.Lock()
		udev := h.udev
		h.mu.Unlock()
		return udev == nil || udev(nr)
	}
	shareNodeHolders = func(nr int) ([]deviceHolder, error) {
		h.mu.Lock()
		defer h.mu.Unlock()
		if h.holderErr != nil {
			return nil, h.holderErr
		}
		return append([]deviceHolder(nil), h.held[shareNodePath(nr)]...), nil
	}
	prevCover, prevOpenNS, prevCovered, prevInode, prevCloseNS := coverPath, openNamespace, coveredPath, namespaceInode, closeNamespace
	prevDevHolders, prevWatch, prevHostNS := deviceHolders, watchDevices, hostMountNSInodes
	prevRetry, prevSettle := cameraShareWatchRetry, cameraShareWatchSettle
	coverPath, openNamespace, coveredPath = h.cover, h.openNS, h.isCovered
	namespaceInode, closeNamespace = h.nsInode, h.closeNS
	deviceHolders = func(path string) []deviceHolder {
		h.mu.Lock()
		defer h.mu.Unlock()
		return append([]deviceHolder(nil), h.held[path]...)
	}
	watchDevices = h.watcher.watch
	hostMountNSInodes = func() ([]uint64, error) {
		h.mu.Lock()
		defer h.mu.Unlock()
		if h.hostNSErr != nil {
			return nil, h.hostNSErr
		}
		return []uint64{csAgentNS, csInitNS}, nil
	}
	cameraShareWatchRetry, cameraShareWatchSettle = 20*time.Millisecond, 10*time.Millisecond
	// A node's /dev entry: the fake loopback's nodes are the kernel's (sysfs),
	// each with device number 81:<nr>, and its noEntry the nodes whose /dev
	// entry is gone. Unlinking and making an entry again change that and are
	// recorded, and either can be made to fail.
	prevAccess, prevRdev, prevUnlink, prevRestore := setShareNodeAccess, shareNodeRdev, unlinkShareNodeEntry, restoreShareNodeEntry
	setShareNodeAccess = func(path string) error {
		h.mu.Lock()
		err := h.accessErr
		h.mu.Unlock()
		h.entryOp("access " + path)
		return err
	}
	shareNodeRdev = func(nr int) (uint64, error) {
		if _, ok := h.loop.auxLabel(nr); !ok {
			return 0, os.ErrNotExist
		}
		return csRdev(nr), nil
	}
	unlinkShareNodeEntry = func(nr int, rdev uint64) error {
		h.mu.Lock()
		err := h.unlinkErr
		h.mu.Unlock()
		if err != nil {
			return err
		}
		if !h.loop.hasEntry(nr) {
			return os.ErrNotExist
		}
		h.loop.setEntry(nr, false)
		h.entryOp(fmt.Sprintf("unlink %d %d:%d", nr, unix.Major(rdev), unix.Minor(rdev)))
		return nil
	}
	restoreShareNodeEntry = func(nr int, rdev uint64) error {
		h.mu.Lock()
		err := h.restoreErr
		h.mu.Unlock()
		if err != nil {
			return err
		}
		h.loop.setEntry(nr, true)
		h.entryOp(fmt.Sprintf("mknod %d %d:%d", nr, unix.Major(rdev), unix.Minor(rdev)))
		return nil
	}
	t.Cleanup(func() {
		cameraSharesPath, openShareNode = prevPath, prevOpen
		shareRemovalClock, shareNodeUdevDone, shareNodeHolders = prevClock, prevUdev, prevHolders
		coverPath, openNamespace, coveredPath, namespaceInode, closeNamespace = prevCover, prevOpenNS, prevCovered, prevInode, prevCloseNS
		deviceHolders, watchDevices, hostMountNSInodes = prevDevHolders, prevWatch, prevHostNS
		cameraShareWatchRetry, cameraShareWatchSettle = prevRetry, prevSettle
		setShareNodeAccess, shareNodeRdev, unlinkShareNodeEntry, restoreShareNodeEntry = prevAccess, prevRdev, prevUnlink, prevRestore
	})

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
			if h.loop.hasEntry(nr) {
				paths = append(paths, fmt.Sprintf("/dev/video%d", nr))
			}
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

// drainRemovals waits for every removal attempt in flight to end: done, or
// left in place because a holder has the node open.
func (h *csHarness) drainRemovals() {
	h.t.Helper()
	drainShareRemovals(h.t, h.s)
}

func drainShareRemovals(t *testing.T, s *VideoService) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		s.shares.removalWG.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("share node removals did not finish")
	}
}

func (h *csHarness) setHeld(path string, holders ...deviceHolder) {
	h.mu.Lock()
	h.held[path] = holders
	h.mu.Unlock()
}

func (h *csHarness) setUdev(udev func(nr int) bool) {
	h.mu.Lock()
	h.udev = udev
	h.mu.Unlock()
}

func (h *csHarness) removalTime(nr int) (time.Time, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	at, ok := h.removedAt[nr]
	return at, ok
}

func (h *csHarness) pendingRemovals() []int {
	h.s.shares.mu.Lock()
	defer h.s.shares.mu.Unlock()
	var out []int
	for nr := range h.s.shares.removing {
		out = append(out, nr)
	}
	sort.Ints(out)
	return out
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
	h.drainRemovals()

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
		h.drainRemovals()
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
	// The writers close before RemoveCameraShares returns; the nodes go in
	// the background, each once.
	h.drainRemovals()
	events := h.eventLog()
	if len(events) != 4 || !reflect.DeepEqual(events[:2], []string{"close /dev/video255", "close /dev/video254"}) {
		t.Fatalf("events = %v, want both closes, then both removals", events)
	}
	if removes := append([]string(nil), events[2:]...); !reflect.DeepEqual(sortedStrings(removes), []string{"remove 254", "remove 255"}) {
		t.Errorf("removals = %v, want 254 and 255 once each", removes)
	}
	if got := h.loop.liveAux(); len(got) != 0 || len(h.pendingRemovals()) != 0 {
		t.Errorf("live nodes %v, pending removals %v; want none", got, h.pendingRemovals())
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
	if state := h.stateFile(); len(state["containers"].(map[string]any)) != 0 || state["removing"] != nil {
		t.Errorf("state file still holds %v", state)
	}

	h.s.RemoveCameraShares("cv-a")
	h.s.RemoveCameraShares("never-made")
	h.drainRemovals()
	if got := h.eventLog(); len(got) != 4 {
		t.Errorf("removing unknown containers did something: %v", got)
	}
}

func sortedStrings(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

// Removals run in the background, so a redeploy's Create can come before the
// old node is gone; then the new share takes another number until it is.
// Drained between redeploys, every one reuses 255; back to back, they still
// leave nothing once the removals drain.
func TestCameraShares_RedeployCyclesLeakNothing(t *testing.T) {
	check := func(t *testing.T, h *csHarness) {
		t.Helper()
		if got := h.loop.liveAux(); len(got) != 0 {
			t.Errorf("nodes left after 60 redeploys: %v", got)
		}
		if got := h.pendingRemovals(); len(got) != 0 {
			t.Errorf("removals left after 60 redeploys: %v", got)
		}
		if n := len(h.loop.shareCalls()); n != 60 {
			t.Errorf("%d nodes made, want 60", n)
		}
		if n := len(h.loop.removedAux()); n != 60 {
			t.Errorf("%d nodes removed, want 60", n)
		}
		if ids := h.containerIDs(); len(ids) != 0 {
			t.Errorf("records left: %v", ids)
		}
		if state := h.stateFile(); len(state["containers"].(map[string]any)) != 0 || state["removing"] != nil {
			t.Errorf("state file still holds %v", state)
		}
		for _, n := range h.openedNodes() {
			if n.closes.Load() != 1 {
				t.Fatalf("%s closed %d times, want 1", n.path, n.closes.Load())
			}
		}
	}

	t.Run("drained between redeploys", func(t *testing.T) {
		h := newCSHarness(t, csBrio())
		for i := 0; i < 60; i++ {
			h.s.CreateCameraShares(context.Background(), "cv-a", "sh.wendy.cv-a")
			if n := len(h.records("cv-a")); n != 1 {
				t.Fatalf("cycle %d: %d records, want 1", i, n)
			}
			h.s.RemoveCameraShares("cv-a")
			h.drainRemovals()
		}
		for _, c := range h.loop.shareCalls() {
			if c.nr != 255 {
				t.Fatalf("a redeploy used node %d; every one should reuse 255", c.nr)
			}
		}
		check(t, h)
	})

	t.Run("back to back", func(t *testing.T) {
		h := newCSHarness(t, csBrio())
		for i := 0; i < 60; i++ {
			h.s.CreateCameraShares(context.Background(), "cv-a", "sh.wendy.cv-a")
			if n := len(h.records("cv-a")); n != 1 {
				t.Fatalf("cycle %d: %d records, want 1", i, n)
			}
			h.s.RemoveCameraShares("cv-a")
		}
		h.drainRemovals()
		check(t, h)
	})
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
	h.drainRemovals()

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
		Siblings        []string
	}
	snapshot := func(s *VideoService) map[string][]rec {
		s.shares.mu.Lock()
		defer s.shares.mu.Unlock()
		out := map[string][]rec{}
		for id, c := range s.shares.containers {
			for _, r := range c.shares {
				out[id] = append(out[id], rec{c.app, r.cameraName, r.cameraPath, r.camera, r.mode, r.node, r.siblings})
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

func TestRemoveShareNode_WaitsForUdevAndOneSecond(t *testing.T) {
	for _, tc := range []struct {
		name   string
		udevAt time.Duration // after the node was made
		want   time.Duration // when it is removed
	}{
		{"udev first: the node's age decides", 300 * time.Millisecond, shareNodeSettleAge},
		{"udev last: udev decides", 1500 * time.Millisecond, 1500 * time.Millisecond},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newCSHarness(t, csBrio())
			t0 := h.clock.Now()
			h.setUdev(func(int) bool { return !h.clock.Now().Before(t0.Add(tc.udevAt)) })
			h.s.CreateCameraShares(context.Background(), "cv-a", "sh.wendy.cv-a")
			h.s.RemoveCameraShares("cv-a")
			h.drainRemovals()
			at, ok := h.removalTime(255)
			if !ok {
				t.Fatal("the node was never removed")
			}
			if got := at.Sub(t0); got < tc.want || got > tc.want+shareNodeSettlePoll {
				t.Errorf("removed %s after it was made, want %s", got, tc.want)
			}
		})
	}
}

func TestRemoveShareNode_ProceedsAfterTheBound(t *testing.T) {
	t.Run("udev never finishes", func(t *testing.T) {
		h := newCSHarness(t, csBrio())
		t0 := h.clock.Now()
		h.setUdev(func(int) bool { return false })
		h.s.CreateCameraShares(context.Background(), "cv-a", "sh.wendy.cv-a")
		h.s.RemoveCameraShares("cv-a")
		h.drainRemovals()
		at, ok := h.removalTime(255)
		if !ok {
			t.Fatal("the node was never removed")
		}
		if got := at.Sub(t0); got < shareNodeSettleBound || got > shareNodeSettleBound+shareNodeSettlePoll {
			t.Errorf("removed %s after the wait began, want the %s bound", got, shareNodeSettleBound)
		}
		if n := h.logged("camera share: /dev/video255 is not settled after 2s"); n != 1 {
			t.Errorf("the give-up line logged %d times, want 1; logs: %v", n, h.logs.All())
		}
	})

	t.Run("an earlier agent's node counts as old", func(t *testing.T) {
		h := newCSHarness(t, csBrio())
		h.writeState(`{"version":1,"containers":{"cv-gone":{"app":"a","shares":[` +
			csStateRecord(csBrio(), "/dev/video0", testSharedMode, 254) + `]}}}`)
		h.loop.addLiveAux(254, "Brio 101")
		h.s.loadCameraShares()
		t0 := h.clock.Now()
		h.s.SyncCameraShares(context.Background(), nil)
		h.drainRemovals()
		if at, ok := h.removalTime(254); !ok || !at.Equal(t0) {
			t.Errorf("removed at %v (%v), want at once, with no wait for its age", at.Sub(t0), ok)
		}
	})
}

func TestRemoveShareNode_HeldNodeIsLeftAndRetriedOnTheNextSync(t *testing.T) {
	h := newCSHarness(t, csBrio())
	h.s.CreateCameraShares(context.Background(), "cv-a", "sh.wendy.cv-a")
	h.setHeld("/dev/video255", deviceHolder{PID: 4321, Comm: "wireplumber"})
	h.s.RemoveCameraShares("cv-a")
	h.drainRemovals()

	line := "camera share: left /dev/video255 in place: it is open in pid 4321 (wireplumber); it is removed later"
	if n := h.logged(line); n != 1 {
		t.Errorf("held line logged %d times, want 1; logs: %v", n, h.logs.All())
	}
	if got := h.loop.removedAux(); len(got) != 0 {
		t.Fatalf("a held node was removed: %v", got)
	}
	if !h.s.IsShareNode("/dev/video255") || !reflect.DeepEqual(h.pendingRemovals(), []int{255}) {
		t.Errorf("the held node is not a share node waiting to be removed (pending %v)", h.pendingRemovals())
	}
	state := h.stateFile()
	if len(state["containers"].(map[string]any)) != 0 {
		t.Errorf("the held node is still a container's share in the state file: %v", state)
	}
	if want := []any{map[string]any{"node": float64(255), "name": "Brio 101"}}; !reflect.DeepEqual(state["removing"], want) {
		t.Errorf("state file removing = %v, want %v", state["removing"], want)
	}

	// Its number is not reused while it waits.
	h.s.CreateCameraShares(context.Background(), "cv-b", "sh.wendy.cv-b")
	if recs := h.records("cv-b"); len(recs) != 1 || recs[0].node != 254 {
		t.Errorf("cv-b's records = %+v, want node 254", recs)
	}

	// Still held at the next sync: still there, and said once.
	h.s.SyncCameraShares(context.Background(), []CameraShareContainer{{ID: "cv-b", Running: true}})
	h.drainRemovals()
	if got := h.loop.removedAux(); len(got) != 0 || h.logged(line) != 1 {
		t.Errorf("after a sync while held: removed %v, held line %d times", got, h.logged(line))
	}

	// The holder lets go; the next sync removes it.
	h.setHeld("/dev/video255")
	h.s.SyncCameraShares(context.Background(), []CameraShareContainer{{ID: "cv-b", Running: true}})
	h.drainRemovals()
	if got := h.loop.removedAux(); !reflect.DeepEqual(got, []int{255}) {
		t.Errorf("removed %v, want [255]", got)
	}
	if h.s.IsShareNode("/dev/video255") || len(h.pendingRemovals()) != 0 {
		t.Errorf("after removal: IsShareNode %v, pending %v", h.s.IsShareNode("/dev/video255"), h.pendingRemovals())
	}
	if state := h.stateFile(); state["removing"] != nil {
		t.Errorf("state file still lists removals: %v", state["removing"])
	}
	if recs := h.records("cv-b"); len(recs) != 1 {
		t.Errorf("cv-b lost its share: %+v", recs)
	}
}

func TestRemoveShareNode_ARemovalTheDriverRefusesIsRetried(t *testing.T) {
	h := newCSHarness(t, csBrio())
	h.s.CreateCameraShares(context.Background(), "cv-a", "sh.wendy.cv-a")
	h.loop.mu.Lock()
	h.loop.refuseRemove = true // opened after the holder check
	h.loop.mu.Unlock()
	h.s.RemoveCameraShares("cv-a")
	h.drainRemovals()
	h.s.SyncCameraShares(context.Background(), nil) // refused again: said once
	h.drainRemovals()
	if n := h.logged("camera share: left /dev/video255 in place: removing it did not take; it is removed later"); n != 1 {
		t.Errorf("refusal line logged %d times, want 1; logs: %v", n, h.logs.All())
	}
	if !reflect.DeepEqual(h.pendingRemovals(), []int{255}) || !h.s.IsShareNode("/dev/video255") {
		t.Errorf("pending %v; want 255 still waiting and a share node", h.pendingRemovals())
	}

	h.loop.mu.Lock()
	h.loop.refuseRemove = false
	h.loop.mu.Unlock()
	h.s.SyncCameraShares(context.Background(), nil)
	h.drainRemovals()
	if got := h.loop.liveAux(); len(got) != 0 || len(h.pendingRemovals()) != 0 {
		t.Errorf("live %v, pending %v; want the node gone", got, h.pendingRemovals())
	}
}

func TestRemoveShareNode_NeverRemovedTwice(t *testing.T) {
	h := newCSHarness(t, csBrio())
	// An earlier agent's file holds node 254 both as cv-gone's share and as a
	// removal: both lead to the one node.
	h.writeState(`{"version":1,"containers":{"cv-gone":{"app":"a","shares":[` +
		csStateRecord(csBrio(), "/dev/video0", testSharedMode, 254) + `]}},"removing":[{"node":254,"name":"Brio 101"}]}`)
	h.loop.addLiveAux(254, "Brio 101")
	h.s.loadCameraShares()
	h.setHeld("/dev/video254", deviceHolder{PID: 99, Comm: "v4l_id"})

	h.s.SyncCameraShares(context.Background(), nil)
	h.drainRemovals()
	h.setHeld("/dev/video254")
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			h.s.SyncCameraShares(context.Background(), nil)
			h.s.RemoveCameraShares("cv-gone")
		}()
	}
	wg.Wait()
	h.drainRemovals()
	if got := h.loop.removedAux(); !reflect.DeepEqual(got, []int{254}) {
		t.Errorf("RemoveAuxNode calls = %v, want 254 once", got)
	}
}

func TestShutdown_DoesNotHangOnARemovalWaitingToSettle(t *testing.T) {
	h := newCSHarness(t, csBrio())
	h.s.CreateCameraShares(context.Background(), "cv-a", "sh.wendy.cv-a")
	h.setUdev(func(int) bool { return false })
	h.clock.setFrozen(true) // the settle wait never ends on its own

	start := time.Now()
	h.s.RemoveCameraShares("cv-a")
	if took := time.Since(start); took > 500*time.Millisecond {
		t.Errorf("RemoveCameraShares took %s: it waited for the node to settle", took)
	}
	if got := h.loop.liveAux(); !reflect.DeepEqual(got, []int{255}) || !h.s.IsShareNode("/dev/video255") {
		t.Errorf("live nodes %v, IsShareNode %v; want 255 still there and a share node", got, h.s.IsShareNode("/dev/video255"))
	}

	done := make(chan struct{})
	go func() {
		h.s.Shutdown()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Shutdown hung on a removal waiting to settle")
	}
	if got := h.loop.removedAux(); len(got) != 0 {
		t.Errorf("Shutdown removed %v", got)
	}
	if want := []any{map[string]any{"node": float64(255), "name": "Brio 101"}}; !reflect.DeepEqual(h.stateFile()["removing"], want) {
		t.Errorf("state file removing = %v, want %v", h.stateFile()["removing"], want)
	}

	// The next agent knows the node and removes it at its first sync.
	h.clock.setFrozen(false)
	h.setUdev(nil)
	next := &VideoService{logger: zap.NewNop(), loopback: h.loop, readDeviceName: h.s.readDeviceName}
	next.loadCameraShares()
	if !next.IsShareNode("/dev/video255") {
		t.Error("the next agent does not know /dev/video255 is a share node")
	}
	next.SyncCameraShares(context.Background(), nil)
	drainShareRemovals(t, next)
	if got := h.loop.removedAux(); !reflect.DeepEqual(got, []int{255}) {
		t.Errorf("the next agent removed %v, want [255]", got)
	}
}

func TestProcDeviceHolders_FindsOtherProcessesHoldingTheDevice(t *testing.T) {
	root := t.TempDir()
	prev := procRoot
	procRoot = root
	t.Cleanup(func() { procRoot = prev })
	regular := filepath.Join(root, "regular")
	if err := os.WriteFile(regular, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	proc := func(pid, comm string, fds map[string]string) {
		t.Helper()
		dir := filepath.Join(root, pid)
		for _, sub := range []string{"fd", "ns"} {
			if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.WriteFile(filepath.Join(dir, "comm"), []byte(comm+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "ns", "mnt"), nil, 0o644); err != nil {
			t.Fatal(err)
		}
		for fd, target := range fds {
			if err := os.Symlink(target, filepath.Join(dir, "fd", fd)); err != nil {
				t.Fatal(err)
			}
		}
	}
	proc("100", "wireplumber", map[string]string{"3": regular, "4": "/dev/null"})
	proc("200", "bash", map[string]string{"0": regular, "1": "/dev/zero"})
	proc(strconv.Itoa(os.Getpid()), "wendy-agent", map[string]string{"5": "/dev/null"}) // the agent itself
	proc("self", "wendy-agent", map[string]string{"5": "/dev/null"})

	fi, err := os.Stat(filepath.Join(root, "100", "ns", "mnt"))
	if err != nil {
		t.Fatal(err)
	}
	want := []deviceHolder{{PID: 100, Comm: "wireplumber", MountNS: uint64(fi.Sys().(*syscall.Stat_t).Ino)}}
	if got := procDeviceHolders("/dev/null"); !reflect.DeepEqual(got, want) {
		t.Errorf("procDeviceHolders(/dev/null) = %+v, want %+v", got, want)
	}
	if got := procDeviceHolders(regular); got != nil {
		t.Errorf("a regular file has holders: %+v", got)
	}
	if got := procDeviceHolders(filepath.Join(root, "missing")); got != nil {
		t.Errorf("a missing device has holders: %+v", got)
	}
}

// opensOf counts how many writers were opened on path.
func (h *csHarness) opensOf(path string) int {
	n := 0
	for _, node := range h.openedNodes() {
		if node.path == path {
			n++
		}
	}
	return n
}

// Fix round 1, I1: X's record keeps 255 across a reboot that removed the node;
// W, sharing the same camera, must get another number, and nothing done for X
// may touch W's node.
func TestCreateCameraShares_NeverTakesANumberAnotherShareHolds(t *testing.T) {
	h := newCSHarness(t, csBrio())
	h.writeState(`{"version":1,"containers":{"cv-x":{"app":"x","shares":[` +
		csStateRecord(csBrio(), "/dev/video0", testSharedMode, 255) + `]}}}`)
	h.s.loadCameraShares()

	h.s.CreateCameraShares(context.Background(), "cv-w", "sh.wendy.cv-w")
	recs := h.records("cv-w")
	if len(recs) != 1 || recs[0].node == 255 {
		t.Fatalf("cv-w's records = %+v; it must not get 255, which cv-x holds", recs)
	}
	w := recs[0].node

	h.s.SyncCameraShares(context.Background(), []CameraShareContainer{{ID: "cv-x"}, {ID: "cv-w", Running: true}})
	if n := h.opensOf("/dev/video255"); n > 1 {
		t.Errorf("/dev/video255 opened %d times", n)
	}
	if n := h.opensOf(shareNodePath(w)); n != 1 {
		t.Errorf("cv-w's node opened %d times, want 1 (at create)", n)
	}

	h.s.RemoveCameraShares("cv-x")
	h.drainRemovals()
	if live := h.loop.liveAux(); !slices.Contains(live, w) || slices.Contains(h.loop.removedAux(), w) {
		t.Errorf("cv-x's removal touched cv-w's node %d: live %v, removed %v", w, live, h.loop.removedAux())
	}
	if recs := h.records("cv-w"); len(recs) != 1 || recs[0].io == nil {
		t.Errorf("cv-w's share is gone: %+v", recs)
	}
}

// Fix round 1, I1: two records of one camera on one number (as a file from
// before the fix can hold): the node gets one writer, and the record without
// it never removes the other's node.
func TestSyncCameraShares_TwoRecordsOnOneNumberNeverShareANode(t *testing.T) {
	h := newCSHarness(t, csBrio())
	h.writeState(`{"version":1,"containers":{` +
		`"cv-w":{"app":"w","shares":[` + csStateRecord(csBrio(), "/dev/video0", testSharedMode, 255) + `]},` +
		`"cv-x":{"app":"x","shares":[` + csStateRecord(csBrio(), "/dev/video0", testSharedMode, 255) + `]}}}`)
	h.loop.addLiveAux(255, "Brio 101")
	h.s.loadCameraShares()

	listed := []CameraShareContainer{{ID: "cv-w", Running: true}, {ID: "cv-x", Running: true}}
	h.s.SyncCameraShares(context.Background(), listed)
	h.s.SyncCameraShares(context.Background(), listed)
	if n := h.opensOf("/dev/video255"); n != 1 {
		t.Errorf("/dev/video255 opened %d times, want once", n)
	}

	h.s.RemoveCameraShares("cv-x")
	h.drainRemovals()
	if got := h.loop.removedAux(); len(got) != 0 {
		t.Errorf("removing cv-x removed %v, cv-w's node", got)
	}
	h.s.RemoveCameraShares("cv-w")
	h.drainRemovals()
	if got := h.loop.removedAux(); !reflect.DeepEqual(got, []int{255}) {
		t.Errorf("removing cv-w removed %v, want [255]", got)
	}
}

// Fix round 1, I2: a camera of the same model seen beside the shared one is
// another camera, so the share never follows it.
func TestResolveShareCamera_ASameModelCameraSeenBesideItIsNeverFollowed(t *testing.T) {
	brio := csBrio()
	other := csCamera{"video2", "Brio 101", brio.formats, camera.Identity{Device: csPort("1-3"), Index: "0", Name: "Brio 101"}}

	t.Run("plugged in after create, then the shared one unplugged", func(t *testing.T) {
		h := newCSHarness(t, brio)
		h.s.CreateCameraShares(context.Background(), "cv-a", "sh.wendy.cv-a")
		r := h.records("cv-a")[0]
		h.mu.Lock()
		h.cams["video2"] = other
		h.mu.Unlock()
		if path, ok := h.s.currentSharePath(r); !ok || path != "/dev/video0" {
			t.Fatalf("with both present: %q, %v; want /dev/video0", path, ok)
		}
		shares := h.stateFile()["containers"].(map[string]any)["cv-a"].(map[string]any)["shares"].([]any)
		if got := shares[0].(map[string]any)["sameModelPorts"]; !reflect.DeepEqual(got, []any{other.id.Device}) {
			t.Errorf("state file sameModelPorts = %v, want [%s]", got, other.id.Device)
		}

		h.mu.Lock()
		delete(h.cams, "video0")
		h.mu.Unlock()
		if path, ok := h.s.currentSharePath(r); ok {
			t.Errorf("after the shared camera left: resolved to %s, the other camera", path)
		}

		reloaded := &VideoService{logger: zap.NewNop()}
		reloaded.loadCameraShares()
		if got := reloaded.shares.containers["cv-a"].shares[0].siblings; !reflect.DeepEqual(got, []string{other.id.Device}) {
			t.Errorf("reloaded siblings = %v, want [%s]", got, other.id.Device)
		}
	})

	t.Run("present at create", func(t *testing.T) {
		h := newCSHarness(t, brio, other)
		h.s.CreateCameraShares(context.Background(), "cv-a", "sh.wendy.cv-a")
		for _, r := range h.records("cv-a") {
			want := other.id.Device
			if r.camera == other.id {
				want = brio.id.Device
			}
			if !reflect.DeepEqual(r.siblings, []string{want}) {
				t.Errorf("record of %s: siblings %v, want [%s]", r.camera.Device, r.siblings, want)
			}
		}
	})

	t.Run("a camera only ever seen alone is followed to a new port", func(t *testing.T) {
		h := newCSHarness(t, brio)
		h.s.CreateCameraShares(context.Background(), "cv-a", "sh.wendy.cv-a")
		r := h.records("cv-a")[0]
		if _, ok := h.s.currentSharePath(r); !ok {
			t.Fatal("the camera does not resolve while present")
		}
		h.mu.Lock()
		delete(h.cams, "video0")
		h.cams["video2"] = other // the same camera, on another port
		h.mu.Unlock()
		if path, ok := h.s.currentSharePath(r); !ok || path != "/dev/video2" {
			t.Errorf("moved camera = %q, %v; want /dev/video2", path, ok)
		}
	})
}

// Fix round 1, M1: once Shutdown begins, nothing is stored or adopted.
func TestShutdown_NothingIsStoredOrAdoptedAfterIt(t *testing.T) {
	h := newCSHarness(t, csBrio())
	h.s.CreateCameraShares(context.Background(), "cv-a", "sh.wendy.cv-a")
	h.s.Shutdown()

	h.s.SyncCameraShares(context.Background(), []CameraShareContainer{{ID: "cv-a", Running: true}})
	if n := h.opensOf("/dev/video255"); n != 1 {
		t.Errorf("a sync after Shutdown opened the node again (%d opens)", n)
	}
	h.s.CreateCameraShares(context.Background(), "cv-b", "sh.wendy.cv-b")
	if n := h.logged("camera share: cv-b gets the real cameras: the video service is shutting down"); n != 1 {
		t.Errorf("Create after Shutdown: line logged %d times; logs: %v", n, h.logs.All())
	}
	if len(h.records("cv-b")) != 0 || len(h.loop.shareCalls()) != 1 {
		t.Errorf("Create after Shutdown made %v", h.loop.shareCalls())
	}
}

// Fix round 1, M1: a Remove of a container whose Create is still running ends
// with the container sharing nothing.
func TestRemoveCameraShares_DuringItsCreateLeavesNoShare(t *testing.T) {
	h := newCSHarness(t, csBrio(), csC920())
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	h.openHook = func(string) {
		once.Do(func() {
			close(entered)
			<-release
		})
	}
	created := make(chan struct{})
	go func() {
		defer close(created)
		h.s.CreateCameraShares(context.Background(), "cv-a", "sh.wendy.cv-a")
	}()
	<-entered
	h.s.RemoveCameraShares("cv-a") // nothing stored yet: it returns at once
	close(release)
	<-created
	h.drainRemovals()

	if recs := h.records("cv-a"); len(recs) != 0 {
		t.Errorf("cv-a still shares %d cameras", len(recs))
	}
	if got := h.loop.liveAux(); len(got) != 0 {
		t.Errorf("nodes left: %v", got)
	}
	for _, line := range []string{
		"camera share: cv-a gets the real camera Brio 101 (/dev/video0): " + shareRefusedRemoved,
		"camera share: cv-a gets the real camera C920 (/dev/video4): " + shareRefusedRemoved,
	} {
		if n := h.logged(line); n != 1 {
			t.Errorf("%q logged %d times; logs: %v", line, n, h.logs.All())
		}
	}
	for _, n := range h.openedNodes() {
		if n.closes.Load() != 1 {
			t.Errorf("%s's writer closed %d times, want 1", n.path, n.closes.Load())
		}
	}
}

// Fix round 1, M2: a decision that arrives when the setup is over makes no node.
func TestCreateCameraShares_ADecisionAtTheDeadlineMakesNothing(t *testing.T) {
	h := newCSHarness(t, csBrio())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	prev := frameRatesFor
	frameRatesFor = func(path string, pixfmt, w, ht uint32) []uint32 {
		cancel() // the setup ends as the decision is made
		return prev(path, pixfmt, w, ht)
	}
	h.s.CreateCameraShares(ctx, "cv-a", "sh.wendy.cv-a")
	if n := len(h.loop.shareCalls()); n != 0 {
		t.Errorf("%d nodes made after the setup ended", n)
	}
	if n := h.logged("camera share: cv-a gets the real camera Brio 101 (/dev/video0): camera sharing setup was cancelled"); n != 1 {
		t.Errorf("cancelled line logged %d times; logs: %v", n, h.logs.All())
	}
}

// Fix round 1, M3: a camera the policy refuses is logged with the policy's
// reason, even when its identity cannot be read.
func TestCreateCameraShares_PolicyReasonComesBeforeTheIdentity(t *testing.T) {
	h := newCSHarness(t, csBrio(), csH264Cam())
	h.s.cameraIdentity = func(string) (camera.Identity, error) { return camera.Identity{}, errors.New("no device link") }
	h.s.CreateCameraShares(context.Background(), "cv-a", "sh.wendy.cv-a")
	if n := h.logged("camera share: cv-a gets the real camera Logi H264 (/dev/video2): the camera produces H.264 itself; sharing it comes in a later release"); n != 1 {
		t.Errorf("policy line logged %d times; logs: %v", n, h.logs.All())
	}
	if n := h.logged("Logi H264 (/dev/video2): its sysfs identity could not be read"); n != 0 {
		t.Errorf("the refused camera got an identity warning; logs: %v", h.logs.All())
	}
	if n := h.logged("camera share: cv-a gets the real camera Brio 101 (/dev/video0): its sysfs identity could not be read: no device link"); n != 1 {
		t.Errorf("identity line for the shared camera logged %d times; logs: %v", n, h.logs.All())
	}
}

// Fix round 1, 8b review: a queued removal reserves its number even when its
// node is gone (a reboot), so a new share never lands on it.
func TestCreateCameraShares_NeverTakesANumberARemovalHolds(t *testing.T) {
	h := newCSHarness(t, csBrio())
	h.writeState(`{"version":1,"containers":{},"removing":[{"node":255,"name":"Brio 101"}]}`)
	h.s.loadCameraShares()
	h.s.CreateCameraShares(context.Background(), "cv-a", "sh.wendy.cv-a")
	if recs := h.records("cv-a"); len(recs) != 1 || recs[0].node != 254 {
		t.Errorf("cv-a's records = %+v, want node 254: 255 is queued for removal", recs)
	}
}

// Fix round 1, 8b review: an entry that outlived its node never removes a node
// made at its number afterwards.
func TestRemoveShareNode_AStaleEntryNeverRemovesANewNode(t *testing.T) {
	setup := func(t *testing.T) *csHarness {
		h := newCSHarness(t, csBrio())
		h.s.CreateCameraShares(context.Background(), "cv-a", "sh.wendy.cv-a")
		h.setHeld("/dev/video255", deviceHolder{PID: 7, Comm: "app"})
		h.s.RemoveCameraShares("cv-a")
		h.drainRemovals()
		if !reflect.DeepEqual(h.pendingRemovals(), []int{255}) {
			t.Fatalf("pending = %v, want [255]", h.pendingRemovals())
		}
		h.vanish(255) // removed outside the agent; the entry stays
		h.setHeld("/dev/video255")
		return h
	}

	t.Run("a share made meanwhile takes another number", func(t *testing.T) {
		h := setup(t)
		h.s.CreateCameraShares(context.Background(), "cv-b", "sh.wendy.cv-b")
		if recs := h.records("cv-b"); len(recs) != 1 || recs[0].node != 254 {
			t.Fatalf("cv-b's records = %+v, want node 254", recs)
		}
		h.s.SyncCameraShares(context.Background(), []CameraShareContainer{{ID: "cv-b", Running: true}})
		h.drainRemovals()
		if got := h.loop.removedAux(); len(got) != 0 {
			t.Errorf("RemoveAuxNode called on %v", got)
		}
		if live := h.loop.liveAux(); !reflect.DeepEqual(live, []int{254}) || len(h.pendingRemovals()) != 0 {
			t.Errorf("live %v, pending %v; want cv-b's 254 and no entry", live, h.pendingRemovals())
		}
	})

	t.Run("another kind of node took the number", func(t *testing.T) {
		h := setup(t)
		h.loop.addLiveAux(255, "Wendy sensor 0") // as the two-plane path could
		h.s.SyncCameraShares(context.Background(), nil)
		h.drainRemovals()
		if got := h.loop.removedAux(); len(got) != 0 {
			t.Errorf("RemoveAuxNode called on %v", got)
		}
		if n := h.logged("camera share: /dev/video255 is no longer the share node of Brio 101; it is left alone"); n != 1 {
			t.Errorf("left-alone line logged %d times; logs: %v", n, h.logs.All())
		}
		if len(h.pendingRemovals()) != 0 {
			t.Errorf("pending = %v, want none", h.pendingRemovals())
		}
	})
}

// Fix round 1, 8b review: a state file holding a node both as a share and as
// a removal keeps the share.
func TestSyncCameraShares_ARecordWinsOverARemovalOfItsNumber(t *testing.T) {
	h := newCSHarness(t, csBrio())
	h.writeState(`{"version":1,"containers":{"cv-a":{"app":"a","shares":[` +
		csStateRecord(csBrio(), "/dev/video0", testSharedMode, 255) + `]}},"removing":[{"node":255,"name":"Brio 101"}]}`)
	h.loop.addLiveAux(255, "Brio 101")
	h.s.loadCameraShares()

	h.s.SyncCameraShares(context.Background(), []CameraShareContainer{{ID: "cv-a", Running: true}})
	h.drainRemovals()
	if got := h.loop.removedAux(); len(got) != 0 {
		t.Errorf("the record's node was removed: %v", got)
	}
	if n := h.logged("camera share: dropped the queued removal of /dev/video255: a share's record holds it"); n != 1 {
		t.Errorf("dropped line logged %d times; logs: %v", n, h.logs.All())
	}
	if n := h.opensOf("/dev/video255"); n != 1 {
		t.Errorf("the record was not adopted (%d opens)", n)
	}
	if state := h.stateFile(); state["removing"] != nil {
		t.Errorf("state file still lists the removal: %v", state["removing"])
	}
}

// Fix round 1, 8b review: when who holds a node cannot be told, it is kept.
func TestRemoveShareNode_AHolderScanThatFailsKeepsTheNode(t *testing.T) {
	h := newCSHarness(t, csBrio())
	h.s.CreateCameraShares(context.Background(), "cv-a", "sh.wendy.cv-a")
	h.setHolderErr(errors.New("open /proc: permission denied"))
	h.s.RemoveCameraShares("cv-a")
	h.drainRemovals()
	h.s.SyncCameraShares(context.Background(), nil)
	h.drainRemovals()
	line := "camera share: left /dev/video255 in place: what has it open could not be read (open /proc: permission denied); it is removed later"
	if n := h.logged(line); n != 1 {
		t.Errorf("scan-failure line logged %d times, want 1; logs: %v", n, h.logs.All())
	}
	if got := h.loop.removedAux(); len(got) != 0 || !reflect.DeepEqual(h.pendingRemovals(), []int{255}) {
		t.Errorf("removed %v, pending %v; want nothing removed and 255 kept", got, h.pendingRemovals())
	}
	h.setHolderErr(nil)
	h.s.SyncCameraShares(context.Background(), nil)
	h.drainRemovals()
	if got := h.loop.removedAux(); !reflect.DeepEqual(got, []int{255}) {
		t.Errorf("removed %v once the scan works, want [255]", got)
	}
}

// Fix round 1, 8b review: each thing about a removal is said once, however
// often it is retried.
func TestRemoveShareNode_RepeatedNotesAreLoggedOnce(t *testing.T) {
	h := newCSHarness(t, csBrio())
	h.s.CreateCameraShares(context.Background(), "cv-a", "sh.wendy.cv-a")
	h.setUdev(func(int) bool { return false })
	h.setHeld("/dev/video255", deviceHolder{PID: 7, Comm: "app"})
	h.s.RemoveCameraShares("cv-a")
	for i := 0; i < 3; i++ {
		h.drainRemovals()
		h.s.SyncCameraShares(context.Background(), nil)
	}
	h.drainRemovals()
	for _, line := range []string{
		"camera share: /dev/video255 is not settled after 2s",
		"camera share: left /dev/video255 in place: it is open in pid 7 (app); it is removed later",
	} {
		if n := h.logged(line); n != 1 {
			t.Errorf("%q logged %d times, want 1", line, n)
		}
	}
}

// Fix round 1, 8b review: a removal stuck in the system cannot hold Shutdown.
func TestShutdown_IsBoundedWhenARemovalIsStuck(t *testing.T) {
	h := newCSHarness(t, csBrio())
	prevWait := shareRemovalShutdownWait
	shareRemovalShutdownWait = 100 * time.Millisecond
	t.Cleanup(func() { shareRemovalShutdownWait = prevWait })
	h.s.CreateCameraShares(context.Background(), "cv-a", "sh.wendy.cv-a")

	scanning, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	stuck := shareNodeHolders
	shareNodeHolders = func(nr int) ([]deviceHolder, error) {
		once.Do(func() { close(scanning) })
		<-release // a stat on a hung mount
		return stuck(nr)
	}
	t.Cleanup(func() { drainShareRemovals(t, h.s) })
	t.Cleanup(func() { close(release) })

	h.s.RemoveCameraShares("cv-a")
	<-scanning
	start := time.Now()
	h.s.Shutdown()
	if took := time.Since(start); took > time.Second {
		t.Errorf("Shutdown took %s with a removal stuck", took)
	}
	if n := h.logged("camera share: a share node removal is still running after 100ms; shutting down without it"); n != 1 {
		t.Errorf("bounded-wait line logged %d times; logs: %v", n, h.logs.All())
	}
}

// Fix round 1, 8b review: the share removal's holder scan takes the node's
// device number from sysfs, and says when it cannot scan.
func TestSysfsShareNodeHolders_ReadsTheDeviceNumberFromSysfs(t *testing.T) {
	sysfs, proc := t.TempDir(), t.TempDir()
	prevSysfs, prevProc := videoSysfsRoot, procRoot
	videoSysfsRoot, procRoot = sysfs, proc
	t.Cleanup(func() { videoSysfsRoot, procRoot = prevSysfs, prevProc })

	fi, err := os.Stat("/dev/null")
	if err != nil {
		t.Fatal(err)
	}
	rdev := uint64(fi.Sys().(*syscall.Stat_t).Rdev)
	if err := os.MkdirAll(filepath.Join(sysfs, "video255"), 0o755); err != nil {
		t.Fatal(err)
	}
	dev := fmt.Sprintf("%d:%d\n", unix.Major(rdev), unix.Minor(rdev))
	if err := os.WriteFile(filepath.Join(sysfs, "video255", "dev"), []byte(dev), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(proc, "100", "fd"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(proc, "100", "comm"), []byte("v4l_id\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/dev/null", filepath.Join(proc, "100", "fd", "3")); err != nil {
		t.Fatal(err)
	}

	got, err := sysfsShareNodeHolders(255)
	if err != nil || len(got) != 1 || got[0].PID != 100 || got[0].Comm != "v4l_id" {
		t.Errorf("sysfsShareNodeHolders(255) = %+v, %v; want pid 100 (v4l_id)", got, err)
	}
	if _, err := sysfsShareNodeHolders(254); err == nil {
		t.Error("a node with no sysfs entry gave no error")
	}
	procRoot = filepath.Join(proc, "missing")
	if _, err := sysfsShareNodeHolders(255); err == nil {
		t.Error("an unreadable /proc gave no error")
	}
}

// ---- Covers at start and after a replug (Task 9) ----

// csAgentPID and csInitPID are pids the harness puts in the agent's and in
// init's mount namespace.
const (
	csAgentPID = 7001
	csInitPID  = 1
)

func (h *csHarness) openNS(pid uint32) (*devcover.Namespace, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.nsOpenErr != nil {
		return nil, h.nsOpenErr
	}
	// Only seams ever touch it, so its zero descriptor is never used.
	n := new(devcover.Namespace)
	h.namespaces[n] = pid
	return n, nil
}

func (h *csHarness) nsPID(n *devcover.Namespace) uint32 {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.namespaces[n]
}

func (h *csHarness) nsInode(n *devcover.Namespace) (uint64, error) {
	switch pid := h.nsPID(n); pid {
	case csAgentPID:
		return csAgentNS, nil
	case csInitPID:
		return csInitNS, nil
	default:
		return csNSInode(pid), nil
	}
}

func (h *csHarness) closeNS(n *devcover.Namespace) error {
	pid := h.nsPID(n)
	h.mu.Lock()
	hook := h.closeHook
	h.mu.Unlock()
	if hook != nil {
		hook(pid)
	}
	h.mu.Lock()
	h.nsClosed = append(h.nsClosed, pid)
	h.mu.Unlock()
	return nil
}

func (h *csHarness) cover(n *devcover.Namespace, src, dst string, timeout time.Duration) error {
	h.mu.Lock()
	hook, err, pid := h.coverHook, h.coverErr[dst], h.namespaces[n]
	h.mu.Unlock()
	if hook != nil {
		hook(src, dst)
	}
	if err != nil {
		return err
	}
	h.mu.Lock()
	h.covers = append(h.covers, csCover{pid, src, dst, timeout})
	h.mu.Unlock()
	h.event(fmt.Sprintf("cover %s %s", src, dst))
	return nil
}

func (h *csHarness) isCovered(_ *devcover.Namespace, _, _ string) (bool, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.covered, nil
}

func (h *csHarness) setCovered(covered bool) {
	h.mu.Lock()
	h.covered = covered
	h.mu.Unlock()
}

func (h *csHarness) setCoverHook(hook func(src, dst string)) {
	h.mu.Lock()
	h.coverHook = hook
	h.mu.Unlock()
}

func (h *csHarness) setCoverErr(dst string, err error) {
	h.mu.Lock()
	h.coverErr[dst] = err
	h.mu.Unlock()
}

func (h *csHarness) coverList() []csCover {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]csCover(nil), h.covers...)
}

// nsOpens counts the namespaces opened for pid.
func (h *csHarness) nsOpens(pid uint32) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, p := range h.namespaces {
		if p == pid {
			n++
		}
	}
	return n
}

func (h *csHarness) nsCloses() []uint32 {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]uint32(nil), h.nsClosed...)
}

// waitFor polls cond every millisecond for at most within.
func csWaitFor(within time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(within)
	for {
		if cond() {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(time.Millisecond)
	}
}

// drainCovers waits for every start's cover goroutine, which saves the state
// file after its call returned.
func (h *csHarness) drainCovers() {
	h.t.Helper()
	done := make(chan struct{})
	go func() {
		h.s.shares.coverWG.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		h.t.Fatal("start covers did not finish")
	}
}

// waitCovers waits for n covers in all, and returns them.
func (h *csHarness) waitCovers(n int, within time.Duration) []csCover {
	h.t.Helper()
	if !csWaitFor(within, func() bool { return len(h.coverList()) >= n }) {
		h.t.Fatalf("%d covers after %s, want %d: %+v", len(h.coverList()), within, n, h.coverList())
	}
	return h.coverList()
}

// waitLogged waits until substr has been logged n times.
func (h *csHarness) waitLogged(substr string, n int) {
	h.t.Helper()
	if !csWaitFor(2*time.Second, func() bool { return h.logged(substr) >= n }) {
		h.t.Fatalf("%q logged %d times, want %d; logs: %v", substr, h.logged(substr), n, h.logs.All())
	}
}

func (w *csWatcher) watch(ctx context.Context, dir string, onEvent func(devwatch.Event)) error {
	w.mu.Lock()
	w.calls++
	if w.calls <= w.fails {
		w.mu.Unlock()
		return errors.New("inotify_add_watch(/dev): no space left on device")
	}
	w.onEvent, w.dir = onEvent, dir
	w.mu.Unlock()
	w.once.Do(func() { close(w.ready) })
	<-ctx.Done()
	return nil
}

// watch starts the service's discovery, which runs the replug watcher, and
// waits for the watcher to be on /dev.
func (h *csHarness) watch() {
	h.t.Helper()
	h.s.StartDiscovery()
	select {
	case <-h.watcher.ready:
	case <-time.After(2 * time.Second):
		h.t.Fatal("the replug watcher never started")
	}
	h.watcher.mu.Lock()
	dir := h.watcher.dir
	h.watcher.mu.Unlock()
	if dir != "/dev" {
		h.t.Fatalf("the watcher watches %q, want /dev", dir)
	}
}

func (h *csHarness) feed(base string, created bool) {
	h.watcher.mu.Lock()
	onEvent := h.watcher.onEvent
	h.watcher.mu.Unlock()
	onEvent(devwatch.Event{Name: base, Created: created})
}

// plug puts c in /dev and tells the watcher; unplug takes it out.
func (h *csHarness) plug(c csCamera) {
	h.mu.Lock()
	h.cams[c.base] = c
	h.mu.Unlock()
	h.feed(c.base, true)
}

func (h *csHarness) unplug(base string) {
	h.mu.Lock()
	delete(h.cams, base)
	h.mu.Unlock()
	h.feed(base, false)
}

// takeOut removes a camera without telling the watcher.
func (h *csHarness) takeOut(base string) {
	h.mu.Lock()
	delete(h.cams, base)
	h.mu.Unlock()
}

// csRecordView is a record's fields read under the share lock.
type csRecordView struct {
	node     int
	made     time.Time
	path     string
	camera   camera.Identity
	siblings []string
	tookReal string
	pump     bool
	opening  bool
}

func (h *csHarness) view(containerID string) []csRecordView {
	h.s.shares.mu.Lock()
	defer h.s.shares.mu.Unlock()
	c := h.s.shares.containers[containerID]
	if c == nil {
		return nil
	}
	var out []csRecordView
	for _, r := range c.shares {
		out = append(out, csRecordView{r.node, r.nodeMade, r.cameraPath, r.camera,
			append([]string(nil), r.siblings...), r.tookRealCamera, r.pump != nil, r.opening})
	}
	return out
}

// ageNamespaces makes every held namespace d older.
func (h *csHarness) ageNamespaces(d time.Duration) {
	h.s.shares.mu.Lock()
	defer h.s.shares.mu.Unlock()
	for _, c := range h.s.shares.containers {
		c.nsAt = c.nsAt.Add(-d)
	}
}

func (h *csHarness) stateShares(containerID string) []map[string]any {
	h.t.Helper()
	c, ok := h.stateFile()["containers"].(map[string]any)[containerID].(map[string]any)
	if !ok {
		h.t.Fatalf("the state file has no %s", containerID)
	}
	var out []map[string]any
	for _, sh := range c["shares"].([]any) {
		out = append(out, sh.(map[string]any))
	}
	return out
}

// coversWithout drops the timeouts, for comparing where covers went.
func coversWithout(covers []csCover) []csCover {
	out := make([]csCover, len(covers))
	for i, c := range covers {
		out[i] = csCover{pid: c.pid, src: c.src, dst: c.dst}
	}
	return out
}

func csC922() csCamera {
	return csCamera{"video2", "C922", []uint32{v4l2PixFmtYUYV, v4l2PixFmtMJPEG},
		camera.Identity{Device: csPort("1-2.2"), Index: "0", Name: "C922"}}
}

func TestCoverCameraShares_CoversEachSharedCameraAtItsCurrentPath(t *testing.T) {
	h := newCSHarness(t, csBrio(), csC922())
	h.s.CreateCameraShares(context.Background(), "cv-a", "sh.wendy.cv-a")

	if errs := h.s.CoverCameraShares(context.Background(), "cv-a", 4242); len(errs) != 0 {
		t.Fatalf("CoverCameraShares errors: %v", errs)
	}
	got := h.coverList()
	want := []csCover{{pid: 4242, src: "/dev/video255", dst: "/dev/video0"}, {pid: 4242, src: "/dev/video254", dst: "/dev/video2"}}
	if !reflect.DeepEqual(coversWithout(got), want) {
		t.Fatalf("covers = %+v, want %+v", got, want)
	}
	for _, c := range got {
		if c.timeout <= 0 || c.timeout > cameraShareCoverBudget {
			t.Errorf("cover of %s had %s, want at most the start's %s", c.dst, c.timeout, cameraShareCoverBudget)
		}
	}
	if n := h.nsOpens(4242); n != 1 {
		t.Errorf("namespace of 4242 opened %d times, want 1", n)
	}
	for _, line := range []string{
		"camera share: cv-a starts with its node /dev/video255 at /dev/video0 (Brio 101)",
		"camera share: cv-a starts with its node /dev/video254 at /dev/video2 (C922)",
	} {
		if n := h.logged(line); n != 1 {
			t.Errorf("%q logged %d times, want 1; logs: %v", line, n, h.logs.All())
		}
	}
	if errs := h.s.CoverCameraShares(context.Background(), "cv-none", 77); len(errs) != 0 || h.nsOpens(77) != 0 {
		t.Errorf("a container that shares nothing: errors %v, %d namespaces opened", errs, h.nsOpens(77))
	}
}

func TestCoverCameraShares_StaysWithinTheStartBudget(t *testing.T) {
	t.Run("a cover that blocks", func(t *testing.T) {
		h := newCSHarness(t, csBrio())
		h.s.CreateCameraShares(context.Background(), "cv-a", "sh.wendy.cv-a")
		release := make(chan struct{})
		t.Cleanup(func() { close(release) })
		h.setCoverHook(func(string, string) {
			select {
			case <-release:
			case <-time.After(2 * time.Second):
			}
		})

		start := time.Now()
		errs := h.s.CoverCameraShares(context.Background(), "cv-a", 4242)
		if took := time.Since(start); took > 300*time.Millisecond {
			t.Errorf("CoverCameraShares took %s, want at most 300ms", took)
		}
		if len(errs) != 1 || !errors.Is(errs[0], devcover.ErrTimeout) || !strings.Contains(errs[0].Error(), "in time") {
			t.Fatalf("errors = %v, want one that names the timeout", errs)
		}
		if n := h.logged("camera share: cv-a starts with the real camera Brio 101 (/dev/video0): cover did not finish in time"); n != 1 {
			t.Errorf("timeout line logged %d times, want 1; logs: %v", n, h.logs.All())
		}
		if got := h.coverList(); len(got) != 0 {
			t.Errorf("covers applied: %+v", got)
		}
	})

	t.Run("making a node again", func(t *testing.T) {
		h := newCSHarness(t, csBrio())
		h.writeState(`{"version":1,"containers":{"cv-a":{"app":"sh.wendy.cv-a","shares":[` +
			csStateRecord(csBrio(), "/dev/video0", testSharedMode, 254) + `]}}}`)
		h.s.loadCameraShares()
		h.mu.Lock()
		h.openHook = func(path string) {
			if path == "/dev/video254" {
				time.Sleep(400 * time.Millisecond)
			}
		}
		h.mu.Unlock()

		start := time.Now()
		errs := h.s.CoverCameraShares(context.Background(), "cv-a", 4242)
		if took := time.Since(start); took > 300*time.Millisecond {
			t.Errorf("CoverCameraShares took %s, want at most 300ms", took)
		}
		if len(errs) != 1 || !errors.Is(errs[0], devcover.ErrTimeout) {
			t.Fatalf("errors = %v, want the timeout", errs)
		}
		// The node made late is kept, with its writer, for the next start; no
		// cover lands after the start went ahead.
		if !csWaitFor(2*time.Second, func() bool { v := h.view("cv-a"); return len(v) == 1 && v[0].pump }) {
			t.Fatalf("the node made late has no pump: %+v", h.view("cv-a"))
		}
		time.Sleep(20 * time.Millisecond)
		if got := h.coverList(); len(got) != 0 {
			t.Errorf("a cover landed after the budget: %+v", got)
		}
	})
}

func TestCoverCameraShares_FailureLeavesTheRealCameraAndSaysWhy(t *testing.T) {
	t.Run("the cover fails", func(t *testing.T) {
		h := newCSHarness(t, csBrio())
		h.s.CreateCameraShares(context.Background(), "cv-a", "sh.wendy.cv-a")
		h.setCoverErr("/dev/video0", fmt.Errorf("move_mount(-> /dev/video0): %w", unix.EINVAL))

		errs := h.s.CoverCameraShares(context.Background(), "cv-a", 4242)
		if len(errs) != 1 || !errors.Is(errs[0], unix.EINVAL) {
			t.Fatalf("errors = %v, want one EINVAL", errs)
		}
		if n := h.logged("camera share: cv-a starts with the real camera Brio 101 (/dev/video0): move_mount(-> /dev/video0): invalid argument"); n != 1 {
			t.Errorf("failure line logged %d times, want 1; logs: %v", n, h.logs.All())
		}
		if n := h.logged("starts with the real camera"); n != 1 {
			t.Errorf("%d real-camera lines, want 1", n)
		}
	})

	t.Run("the namespace cannot be opened", func(t *testing.T) {
		h := newCSHarness(t, csBrio())
		h.s.CreateCameraShares(context.Background(), "cv-a", "sh.wendy.cv-a")
		h.mu.Lock()
		h.nsOpenErr = fmt.Errorf("open /proc/4242/ns/mnt: %w", syscall.ENOENT)
		h.mu.Unlock()

		errs := h.s.CoverCameraShares(context.Background(), "cv-a", 4242)
		if len(errs) != 1 || !errors.Is(errs[0], syscall.ENOENT) {
			t.Fatalf("errors = %v, want one ENOENT", errs)
		}
		if n := h.logged("camera share: cv-a starts with the real camera Brio 101 (/dev/video0): opening its mount namespace: open /proc/4242/ns/mnt: no such file or directory"); n != 1 {
			t.Errorf("namespace line logged %d times, want 1; logs: %v", n, h.logs.All())
		}
		if got := h.coverList(); len(got) != 0 {
			t.Errorf("covers = %+v, want none", got)
		}
	})

	for _, tc := range []struct {
		name    string
		pid     uint32
		hostErr error
		want    string
	}{
		{"a pid in the agent's own namespace", csAgentPID, nil, "pid 7001 is in the host's mount namespace (the agent's or init's)"},
		{"a pid in init's namespace", csInitPID, nil, "pid 1 is in the host's mount namespace (the agent's or init's)"},
		{"the host's namespace cannot be read", 4242, errors.New("stat /proc/1/ns/mnt: permission denied"),
			"the host's mount namespace could not be read to tell it from pid 4242's: stat /proc/1/ns/mnt: permission denied"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newCSHarness(t, csBrio())
			h.s.CreateCameraShares(context.Background(), "cv-a", "sh.wendy.cv-a")
			h.mu.Lock()
			h.hostNSErr = tc.hostErr
			h.mu.Unlock()
			errs := h.s.CoverCameraShares(context.Background(), "cv-a", tc.pid)
			if len(errs) != 1 || !strings.Contains(errs[0].Error(), tc.want) {
				t.Fatalf("errors = %v, want %q", errs, tc.want)
			}
			if got := h.coverList(); len(got) != 0 {
				t.Errorf("covered: %+v", got)
			}
			if !csWaitFor(time.Second, func() bool { return slices.Contains(h.nsCloses(), tc.pid) }) {
				t.Error("the refused namespace was not closed")
			}
		})
	}
}

func TestCoverCameraShares_RecreatesNodesLostInAReboot(t *testing.T) {
	stored := sharedMode{Width: 640, Height: 480, Fourcc: "YUYV", BytesPerLine: 1280, FPS: 15}
	rebooted := func(t *testing.T, containers, top string) *csHarness {
		h := newCSHarness(t, csBrio())
		h.writeState(`{"version":1,"containers":{"cv-a":{"app":"sh.wendy.cv-a","shares":[` +
			csStateRecord(csBrio(), "/dev/video0", stored, 254) + `]}` + containers + `}` + top + `}`)
		h.s.loadCameraShares()
		return h
	}

	t.Run("its own number is free", func(t *testing.T) {
		h := rebooted(t, "", "")
		made := h.clock.Now()
		if errs := h.s.CoverCameraShares(context.Background(), "cv-a", 11); len(errs) != 0 {
			t.Fatalf("errors: %v", errs)
		}
		if got, want := h.loop.shareCalls(), []auxCall{{254, "Brio 101"}}; !reflect.DeepEqual(got, want) {
			t.Errorf("EnsureShareNode calls = %+v, want %+v", got, want)
		}
		if n := h.loop.auxCreatedCount(); n != 0 {
			t.Errorf("EnsureAuxNode called %d times; share nodes come from EnsureShareNode", n)
		}
		nodes := h.openedNodes()
		if len(nodes) != 1 || nodes[0].path != "/dev/video254" || nodes[0].mode != stored {
			t.Fatalf("opened %+v, want /dev/video254 at the stored mode", nodes)
		}
		if got, want := coversWithout(h.coverList()), []csCover{{pid: 11, src: "/dev/video254", dst: "/dev/video0"}}; !reflect.DeepEqual(got, want) {
			t.Errorf("covers = %+v, want %+v", got, want)
		}
		events := h.eventLog()
		if i, j := slices.Index(events, "open /dev/video254"), slices.Index(events, "cover /dev/video254 /dev/video0"); i < 0 || j < i {
			t.Errorf("events = %v, want the node opened before it is put over the camera", events)
		}
		if v := h.view("cv-a"); len(v) != 1 || v[0].node != 254 || !v[0].pump || !v[0].made.Equal(made) {
			t.Errorf("record = %+v, want node 254 with a pump, made at %v", v, made)
		}
		if n := h.logged("camera share: made /dev/video254 again for cv-a's share of Brio 101"); n != 1 {
			t.Errorf("made-again line logged %d times; logs: %v", n, h.logs.All())
		}
		h.drainCovers()
		if node := h.stateShares("cv-a")[0]["node"]; node != float64(254) {
			t.Errorf("state file node = %v, want 254", node)
		}
		// The node is new: removing it waits for its age.
		h.s.RemoveCameraShares("cv-a")
		h.drainRemovals()
		if at, ok := h.removalTime(254); !ok || at.Sub(made) < shareNodeSettleAge {
			t.Errorf("removed %v after it was made (%v), want at least %s", at.Sub(made), ok, shareNodeSettleAge)
		}
	})

	t.Run("a node of another kind has its number", func(t *testing.T) {
		h := rebooted(t, "", "")
		h.loop.addLiveAux(254, "Wendy sensor 0") // as the two-plane path could
		if errs := h.s.CoverCameraShares(context.Background(), "cv-a", 11); len(errs) != 0 {
			t.Fatalf("errors: %v", errs)
		}
		if got, want := h.loop.shareCalls(), []auxCall{{255, "Brio 101"}}; !reflect.DeepEqual(got, want) {
			t.Errorf("EnsureShareNode calls = %+v, want %+v", got, want)
		}
		if got, want := coversWithout(h.coverList()), []csCover{{pid: 11, src: "/dev/video255", dst: "/dev/video0"}}; !reflect.DeepEqual(got, want) {
			t.Errorf("covers = %+v, want %+v", got, want)
		}
		if v := h.view("cv-a"); len(v) != 1 || v[0].node != 255 {
			t.Errorf("record = %+v, want node 255", v)
		}
		h.drainCovers() // the state file is saved off the start path
		if node := h.stateShares("cv-a")[0]["node"]; node != float64(255) {
			t.Errorf("state file node = %v, want 255", node)
		}
		if label, _ := h.loop.auxLabel(254); label != "Wendy sensor 0" || len(h.loop.removedAux()) != 0 || h.opensOf("/dev/video254") != 0 {
			t.Errorf("the other node was touched: label %q, removed %v, opened %d times", label, h.loop.removedAux(), h.opensOf("/dev/video254"))
		}
		if n := h.logged("camera share: made /dev/video255 again for cv-a's share of Brio 101 (its number 254 is taken)"); n != 1 {
			t.Errorf("made-again line logged %d times; logs: %v", n, h.logs.All())
		}
	})

	t.Run("another share holds its number", func(t *testing.T) {
		h := rebooted(t, `,"cv-b":{"app":"sh.wendy.cv-b","shares":[`+csStateRecord(csBrio(), "/dev/video0", stored, 254)+`]}`, "")
		if errs := h.s.CoverCameraShares(context.Background(), "cv-a", 11); len(errs) != 0 {
			t.Fatalf("cv-a errors: %v", errs)
		}
		if errs := h.s.CoverCameraShares(context.Background(), "cv-b", 12); len(errs) != 0 {
			t.Fatalf("cv-b errors: %v", errs)
		}
		if got, want := h.loop.shareCalls(), []auxCall{{255, "Brio 101"}, {254, "Brio 101"}}; !reflect.DeepEqual(got, want) {
			t.Errorf("EnsureShareNode calls = %+v, want %+v: the second share keeps 254", got, want)
		}
		if a, b := h.view("cv-a"), h.view("cv-b"); a[0].node != 255 || b[0].node != 254 {
			t.Errorf("nodes: cv-a %d, cv-b %d; want 255 and 254", a[0].node, b[0].node)
		}
		want := []csCover{{pid: 11, src: "/dev/video255", dst: "/dev/video0"}, {pid: 12, src: "/dev/video254", dst: "/dev/video0"}}
		if got := coversWithout(h.coverList()); !reflect.DeepEqual(got, want) {
			t.Errorf("covers = %+v, want %+v", got, want)
		}
	})

	t.Run("a removal holds its number", func(t *testing.T) {
		h := rebooted(t, "", `,"removing":[{"node":254,"name":"Brio 101"}]`)
		if errs := h.s.CoverCameraShares(context.Background(), "cv-a", 11); len(errs) != 0 {
			t.Fatalf("errors: %v", errs)
		}
		if got, want := h.loop.shareCalls(), []auxCall{{255, "Brio 101"}}; !reflect.DeepEqual(got, want) {
			t.Errorf("EnsureShareNode calls = %+v, want %+v", got, want)
		}
	})
}

// blockOpenOf makes the first open of path wait for release, and says when it
// is waiting; later opens go through at once.
func (h *csHarness) blockOpenOf(path string) (entered, release chan struct{}) {
	entered, release = make(chan struct{}), make(chan struct{})
	var first atomic.Bool
	h.mu.Lock()
	h.openHook = func(p string) {
		if p == path && first.CompareAndSwap(false, true) {
			close(entered)
			<-release
		}
	}
	h.mu.Unlock()
	return entered, release
}

// A start making a node again never waits for ops, so a removal or a sync can
// land in the middle; neither may leave the node to the kernel race or give
// it two writers.
func TestCoverCameraShares_RecreationMeetsARemovalOrASync(t *testing.T) {
	rebooted := func(t *testing.T) *csHarness {
		h := newCSHarness(t, csBrio())
		h.writeState(`{"version":1,"containers":{"cv-a":{"app":"sh.wendy.cv-a","shares":[` +
			csStateRecord(csBrio(), "/dev/video0", testSharedMode, 254) + `]}}}`)
		h.s.loadCameraShares()
		return h
	}

	t.Run("a removal takes the new node away once it has settled", func(t *testing.T) {
		h := rebooted(t)
		entered, release := h.blockOpenOf("/dev/video254")
		made := h.clock.Now()
		returned := make(chan []error, 1)
		go func() { returned <- h.s.CoverCameraShares(context.Background(), "cv-a", 11) }()
		<-entered
		h.s.RemoveCameraShares("cv-a") // does not wait for the start
		close(release)
		if errs := <-returned; len(errs) != 1 {
			t.Errorf("errors = %v, want one", errs)
		}
		h.drainCovers()
		h.drainRemovals()
		if at, ok := h.removalTime(254); !ok || at.Sub(made) < shareNodeSettleAge {
			t.Errorf("removed %v after it was made (%v), want at least %s", at.Sub(made), ok, shareNodeSettleAge)
		}
		if live, ids := h.loop.liveAux(), h.containerIDs(); len(live) != 0 || len(ids) != 0 {
			t.Errorf("live nodes %v, containers %v; want none", live, ids)
		}
		for _, n := range h.openedNodes() {
			if n.closes.Load() != 1 {
				t.Errorf("%s's writer closed %d times, want 1", n.path, n.closes.Load())
			}
		}
		if got := h.coverList(); len(got) != 0 {
			t.Errorf("covers = %+v, want none", got)
		}
	})

	t.Run("a sync leaves the node to the start", func(t *testing.T) {
		h := rebooted(t)
		entered, release := h.blockOpenOf("/dev/video254")
		returned := make(chan []error, 1)
		go func() { returned <- h.s.CoverCameraShares(context.Background(), "cv-a", 11) }()
		<-entered
		// The node is there now, a share node of the camera, without a writer.
		h.s.SyncCameraShares(context.Background(), []CameraShareContainer{{ID: "cv-a", Running: true, PID: 11}})
		close(release)
		if errs := <-returned; len(errs) != 0 {
			t.Fatalf("errors = %v", errs)
		}
		if n := h.opensOf("/dev/video254"); n != 1 {
			t.Errorf("/dev/video254 opened %d times, want once", n)
		}
		if v := h.view("cv-a"); len(v) != 1 || !v[0].pump {
			t.Errorf("record = %+v, want one with a pump", v)
		}
	})
}

func TestCoverCameraShares_AbsentCameraIsCoveredWhenPluggedIn(t *testing.T) {
	h := newCSHarness(t, csBrio())
	h.s.CreateCameraShares(context.Background(), "cv-a", "sh.wendy.cv-a")
	h.takeOut("video0")
	h.watch()

	if errs := h.s.CoverCameraShares(context.Background(), "cv-a", 11); len(errs) != 0 {
		t.Fatalf("errors: %v", errs)
	}
	if got := h.coverList(); len(got) != 0 {
		t.Fatalf("covers at start = %+v, want none", got)
	}
	if n := h.logged("camera share: Brio 101 is not plugged in; cv-a's node covers it when it is"); n != 1 {
		t.Errorf("not-plugged-in line logged %d times; logs: %v", n, h.logs.All())
	}

	h.plug(csBrio())
	got := h.waitCovers(1, time.Second)
	if want := (csCover{11, "/dev/video255", "/dev/video0", cameraShareRecoverTimeout}); len(got) != 1 || got[0] != want {
		t.Errorf("covers = %+v, want %+v", got, want)
	}
	h.waitLogged("camera share: covered /dev/video0 for cv-a again in ", 1)
	if n := h.logged("ms after the camera came back"); n != 1 {
		t.Errorf("re-cover line logged %d times; logs: %v", n, h.logs.All())
	}
}

func TestRecover_CoversEveryRunningSharingContainer(t *testing.T) {
	h := newCSHarness(t, csBrio())
	for i, id := range []string{"cv-a", "cv-b", "cv-c"} {
		h.s.CreateCameraShares(context.Background(), id, "sh.wendy."+id)
		if errs := h.s.CoverCameraShares(context.Background(), id, uint32(11+i)); len(errs) != 0 {
			t.Fatalf("%s: %v", id, errs)
		}
	}
	h.s.ReleaseCameraShares("cv-c") // stopped
	h.watch()

	for round := 1; round <= 2; round++ {
		h.unplug("video0")
		h.plug(csBrio())
		// The lines come after every cover of the event.
		h.waitLogged("ms after the camera came back", 2*round)
		covers := h.coverList()
		if len(covers) != 3+2*round {
			t.Fatalf("round %d: %d covers in all, want %d: %+v", round, len(covers), 3+2*round, covers)
		}
		got := map[uint32]int{}
		for _, c := range covers[3+2*(round-1):] {
			got[c.pid]++
			if c.timeout != cameraShareRecoverTimeout {
				t.Errorf("a re-cover had %s, want %s", c.timeout, cameraShareRecoverTimeout)
			}
		}
		if !reflect.DeepEqual(got, map[uint32]int{11: 1, 12: 1}) {
			t.Errorf("round %d: covers by pid = %v, want cv-a's and cv-b's once each", round, got)
		}
	}
}

func TestRecover_SameModelCamerasStayApart(t *testing.T) {
	p1 := csBrio()
	p2 := csCamera{"video2", "Brio 101", p1.formats, camera.Identity{Device: csPort("1-2.2"), Index: "0", Name: "Brio 101"}}
	h := newCSHarness(t, p1, p2)
	h.s.CreateCameraShares(context.Background(), "cv-a", "sh.wendy.cv-a")
	if errs := h.s.CoverCameraShares(context.Background(), "cv-a", 11); len(errs) != 0 {
		t.Fatalf("errors: %v", errs)
	}
	if got := coversWithout(h.coverList()); !reflect.DeepEqual(got, []csCover{{11, "/dev/video255", "/dev/video0", 0}, {11, "/dev/video254", "/dev/video2", 0}}) {
		t.Fatalf("covers at start = %+v", got)
	}
	h.watch()

	// Both go; p2's camera comes back at another node: only p2's record covers it.
	h.unplug("video0")
	h.unplug("video2")
	h.plug(csCamera{"video4", "Brio 101", p1.formats, p2.id})
	h.waitLogged("camera share: covered /dev/video4 for cv-a again in ", 1)
	if got := coversWithout(h.coverList()[2:]); !reflect.DeepEqual(got, []csCover{{11, "/dev/video254", "/dev/video4", 0}}) {
		t.Errorf("covers after p2's camera came back = %+v, want only p2's node", got)
	}

	h.plug(csCamera{"video6", "Brio 101", p1.formats, p1.id})
	h.waitLogged("camera share: covered /dev/video6 for cv-a again in ", 1)
	if got := coversWithout(h.coverList()[3:]); !reflect.DeepEqual(got, []csCover{{11, "/dev/video255", "/dev/video6", 0}}) {
		t.Errorf("covers after p1's camera came back = %+v, want only p1's node", got)
	}
}

func TestRecover_CameraMovedToAnotherPortIsFollowed(t *testing.T) {
	brio := csBrio()
	moved := csCamera{"video2", "Brio 101", brio.formats, camera.Identity{Device: csPort("1-3"), Index: "0", Name: "Brio 101"}}
	h := newCSHarness(t, brio)
	for i, id := range []string{"cv-a", "cv-b"} {
		h.s.CreateCameraShares(context.Background(), id, "sh.wendy."+id)
		if errs := h.s.CoverCameraShares(context.Background(), id, uint32(11+i)); len(errs) != 0 {
			t.Fatalf("%s: %v", id, errs)
		}
	}
	h.watch()

	h.unplug("video0")
	h.plug(moved)
	h.waitLogged("camera share: covered /dev/video2 for ", 2)
	want := []csCover{{11, "/dev/video255", "/dev/video2", 0}, {12, "/dev/video254", "/dev/video2", 0}}
	if got := coversWithout(h.coverList()[2:]); !reflect.DeepEqual(got, want) {
		t.Fatalf("covers after the move = %+v, want %+v: every share of the camera follows it", got, want)
	}
	// The records now name the camera's new port, in memory and on disk.
	for _, id := range []string{"cv-a", "cv-b"} {
		if v := h.view(id); v[0].camera != moved.id || v[0].path != "/dev/video2" {
			t.Errorf("%s's record = %+v, want the camera at %s on /dev/video2", id, v[0], moved.id.Device)
		}
		sh := h.stateShares(id)[0]
		if dev := sh["camera"].(map[string]any)["device"]; dev != moved.id.Device || sh["path"] != "/dev/video2" {
			t.Errorf("%s in the state file: device %v, path %v; want %s, /dev/video2", id, dev, sh["path"], moved.id.Device)
		}
	}

	// A camera of the same model in the old port is another camera.
	h.plug(csCamera{"video4", "Brio 101", brio.formats, brio.id})
	h.waitLogged("camera share: /dev/video4 appeared; no running app shares it", 1)
	if n := len(h.coverList()); n != 4 {
		t.Errorf("%d covers, want 4: the camera in the old port was covered", n)
	}
	if v := h.view("cv-a"); v[0].path != "/dev/video2" || !reflect.DeepEqual(v[0].siblings, []string{brio.id.Device}) {
		t.Errorf("cv-a's record = %+v, want it on /dev/video2 with %s as a sibling", v[0], brio.id.Device)
	}
}

func TestRecover_LearnsASameModelCameraPluggedInBesideAnyShare(t *testing.T) {
	brio := csBrio()
	twin := csCamera{"video2", "Brio 101", brio.formats, camera.Identity{Device: csPort("1-3"), Index: "0", Name: "Brio 101"}}
	h := newCSHarness(t, brio)
	h.s.CreateCameraShares(context.Background(), "cv-a", "sh.wendy.cv-a") // never started
	h.s.CreateCameraShares(context.Background(), "cv-b", "sh.wendy.cv-b")
	if errs := h.s.CoverCameraShares(context.Background(), "cv-b", 12); len(errs) != 0 {
		t.Fatalf("errors: %v", errs)
	}
	h.watch()

	h.plug(twin)
	h.waitLogged("camera share: /dev/video2 appeared; no running app shares it", 1)
	for _, id := range []string{"cv-a", "cv-b"} {
		if v := h.view(id); !reflect.DeepEqual(v[0].siblings, []string{twin.id.Device}) {
			t.Errorf("%s's siblings = %v, want [%s]", id, v[0].siblings, twin.id.Device)
		}
		if got := h.stateShares(id)[0]["sameModelPorts"]; !reflect.DeepEqual(got, []any{twin.id.Device}) {
			t.Errorf("%s's sameModelPorts in the state file = %v", id, got)
		}
	}
	// The shared camera goes: no share takes the twin for it.
	h.takeOut("video0")
	for _, id := range []string{"cv-a", "cv-b"} {
		if path, ok := h.s.currentSharePath(h.records(id)[0]); ok {
			t.Errorf("%s's share resolved to %s, the other camera", id, path)
		}
	}
	if n := len(h.coverList()); n != 1 {
		t.Errorf("%d covers, want only cv-b's start", n)
	}
}

func TestRecover_IgnoresShareNodesAndUnknownNodes(t *testing.T) {
	h := newCSHarness(t, csBrio())
	h.s.CreateCameraShares(context.Background(), "cv-a", "sh.wendy.cv-a")
	if errs := h.s.CoverCameraShares(context.Background(), "cv-a", 11); len(errs) != 0 {
		t.Fatalf("errors: %v", errs)
	}
	h.watch()
	h.unplug("video0")
	// A virtual node in the band named like the camera, as if it had a device
	// link; and a node with no identity at all.
	h.loop.addLiveAux(254, "Brio 101")
	h.plug(csCamera{"video254", "Brio 101", csBrio().formats,
		camera.Identity{Device: "/sys/devices/virtual/video4linux/video254", Index: "0", Name: "Brio 101"}})
	h.feed("video255", true)
	h.feed("video7", true)

	h.waitLogged("camera share: /dev/video254 appeared; it is a share node", 1)
	h.waitLogged("camera share: /dev/video255 appeared; it is a share node", 1)
	h.waitLogged("camera share: /dev/video7 appeared; it has no camera identity", 1)
	if got := h.coverList(); len(got) != 1 {
		t.Errorf("covers = %+v, want only the start's", got)
	}
}

func TestRecover_NamesAnAppThatTookTheRealCamera(t *testing.T) {
	h := newCSHarness(t, csBrio())
	installFakeProducers(h.s)
	for i, id := range []string{"cv-a", "cv-b"} {
		h.s.CreateCameraShares(context.Background(), id, "sh.wendy."+id)
		if errs := h.s.CoverCameraShares(context.Background(), id, uint32(11+i)); len(errs) != 0 {
			t.Fatalf("%s: %v", id, errs)
		}
	}
	h.watch()

	h.unplug("video0")
	// In the window: an app of cv-b's, and a host process, hold the real camera.
	h.setHeld("/dev/video0", deviceHolder{PID: 4321, Comm: "python3", MountNS: csNSInode(12)},
		deviceHolder{PID: 77, Comm: "wireplumber", MountNS: csAgentNS})
	plugged := time.Now()
	h.plug(csBrio())
	line := "camera share: cv-b took the real camera /dev/video0 while it was being covered after a replug, " +
		"and does not share it until it reopens the camera (pid 4321, python3)"
	h.waitLogged(line, 1)
	if took := time.Since(plugged); took < cameraShareLoserCheckDelay {
		t.Errorf("the check ran %s after the replug, want at least %s", took, cameraShareLoserCheckDelay)
	}
	time.Sleep(50 * time.Millisecond)
	if n := h.logged("took the real camera"); n != 1 {
		t.Errorf("%d took-the-real-camera lines, want 1; logs: %v", n, h.logs.All())
	}
	// A status read shows it for cv-b alone.
	b := h.view("cv-b")[0]
	if b.tookReal == "" || h.view("cv-a")[0].tookReal != "" {
		t.Fatalf("tookRealCamera: cv-b %q, cv-a %q; want only cv-b's set", b.tookReal, h.view("cv-a")[0].tookReal)
	}

	// cv-b reopens the camera, now its node: the next reading clears it.
	var node *csNode
	for _, n := range h.openedNodes() {
		if n.path == shareNodePath(b.node) {
			node = n
		}
	}
	node.send(t, 1)
	if !csWaitFor(2*time.Second, func() bool { return h.view("cv-b")[0].tookReal == "" }) {
		t.Errorf("tookRealCamera still %q after cv-b read its node", h.view("cv-b")[0].tookReal)
	}
}

func TestSyncCameraShares_RecoversAReplugMissedWhileTheAgentWasDown(t *testing.T) {
	for _, tc := range []struct {
		name    string
		covered bool
		want    []csCover
	}{
		{"the cover went with a replug", false, []csCover{{4242, "/dev/video254", "/dev/video0", cameraShareRecoverTimeout}}},
		{"the cover is still in place", true, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newCSHarness(t, csBrio())
			h.writeState(`{"version":1,"containers":{"cv-a":{"app":"sh.wendy.cv-a","shares":[` +
				csStateRecord(csBrio(), "/dev/video0", testSharedMode, 254) + `]}}}`)
			h.loop.addLiveAux(254, "Brio 101")
			h.s.loadCameraShares()
			h.setCovered(tc.covered)

			listed := []CameraShareContainer{{ID: "cv-a", Running: true, PID: 4242}}
			h.s.SyncCameraShares(context.Background(), listed)
			if got := h.coverList(); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("covers = %+v, want %+v", got, tc.want)
			}
			if tc.want != nil {
				if n := h.logged("camera share: covered /dev/video0 for cv-a again; the camera was replugged while the agent was down"); n != 1 {
					t.Errorf("missed-replug line logged %d times; logs: %v", n, h.logs.All())
				}
			}
			h.s.SyncCameraShares(context.Background(), listed)
			if n := h.nsOpens(4242); n != 1 || !reflect.DeepEqual(h.coverList(), tc.want) {
				t.Errorf("a second sync: %d namespaces opened, covers %+v", n, h.coverList())
			}
			// The namespace is held: a replug now is covered at once.
			h.setCovered(true)
			h.watch()
			h.unplug("video0")
			h.plug(csBrio())
			h.waitLogged("camera share: covered /dev/video0 for cv-a again in ", 1)
		})
	}
}

func TestSyncCameraShares_ReleasesTheNamespaceOfAStoppedContainer(t *testing.T) {
	h := newCSHarness(t, csBrio())
	h.s.CreateCameraShares(context.Background(), "cv-a", "sh.wendy.cv-a")
	if errs := h.s.CoverCameraShares(context.Background(), "cv-a", 11); len(errs) != 0 {
		t.Fatalf("errors: %v", errs)
	}
	stopped := []CameraShareContainer{{ID: "cv-a"}}
	// A listing taken before this start says it is stopped: the namespace stays.
	h.s.SyncCameraShares(context.Background(), stopped)
	if got := h.nsCloses(); len(got) != 0 {
		t.Fatalf("a sync closed a namespace opened just now: %v", got)
	}
	h.ageNamespaces(cameraShareSyncGrace)
	h.s.SyncCameraShares(context.Background(), stopped)
	if got := h.nsCloses(); !reflect.DeepEqual(got, []uint32{11}) {
		t.Fatalf("closed = %v, want 11's", got)
	}
	h.watch()
	h.unplug("video0")
	h.plug(csBrio())
	h.waitLogged("camera share: /dev/video0 appeared; no running app shares it", 1)
	if n := len(h.coverList()); n != 1 {
		t.Errorf("%d covers, want only the start's", n)
	}
}

func TestReleaseCameraShares_EndsRecoversForTheContainer(t *testing.T) {
	h := newCSHarness(t, csBrio())
	for i, id := range []string{"cv-a", "cv-b"} {
		h.s.CreateCameraShares(context.Background(), id, "sh.wendy."+id)
		if errs := h.s.CoverCameraShares(context.Background(), id, uint32(11+i)); len(errs) != 0 {
			t.Fatalf("%s: %v", id, errs)
		}
	}
	h.s.ReleaseCameraShares("cv-a")
	if got := h.nsCloses(); !reflect.DeepEqual(got, []uint32{11}) {
		t.Fatalf("closed = %v, want 11's", got)
	}
	h.watch()
	h.unplug("video0")
	h.plug(csBrio())
	h.waitLogged("camera share: covered /dev/video0 for cv-b again in ", 1)
	if covers := h.coverList(); len(covers) != 3 || covers[2].pid != 12 {
		t.Errorf("covers = %+v, want cv-b's alone after the replug", covers)
	}

	h.s.RemoveCameraShares("cv-b")
	if got := h.nsCloses(); !reflect.DeepEqual(got, []uint32{11, 12}) {
		t.Errorf("closed = %v, want 11's and 12's", got)
	}
	h.unplug("video0")
	h.plug(csBrio())
	h.waitLogged("camera share: /dev/video0 appeared; no running app shares it", 1)
	if n := len(h.coverList()); n != 3 {
		t.Errorf("%d covers, want no more", n)
	}
}

func TestCoverCameraShares_ClosesTheEarlierNamespaceOffTheStartPath(t *testing.T) {
	h := newCSHarness(t, csBrio())
	h.s.CreateCameraShares(context.Background(), "cv-a", "sh.wendy.cv-a")
	if errs := h.s.CoverCameraShares(context.Background(), "cv-a", 11); len(errs) != 0 {
		t.Fatalf("errors: %v", errs)
	}
	closing, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	h.mu.Lock()
	h.closeHook = func(pid uint32) {
		if pid == 11 {
			close(closing)
			<-release // tearing down a dead container's mounts takes milliseconds
		}
	}
	h.mu.Unlock()

	returned := make(chan []error, 1)
	go func() { returned <- h.s.CoverCameraShares(context.Background(), "cv-a", 12) }()
	select {
	case errs := <-returned:
		if len(errs) != 0 {
			t.Fatalf("errors: %v", errs)
		}
	case <-time.After(time.Second):
		t.Fatal("the restart's cover waited for the old namespace to close")
	}
	select {
	case <-closing:
	case <-time.After(2 * time.Second):
		t.Fatal("the earlier namespace was never closed")
	}
	releaseOnce.Do(func() { close(release) })
	if !csWaitFor(time.Second, func() bool { return reflect.DeepEqual(h.nsCloses(), []uint32{11}) }) {
		t.Errorf("closed = %v, want 11's", h.nsCloses())
	}
	h.watch()
	h.unplug("video0")
	h.plug(csBrio())
	h.waitLogged("camera share: covered /dev/video0 for cv-a again in ", 1)
	if covers := h.coverList(); covers[len(covers)-1].pid != 12 {
		t.Errorf("the re-cover went into %d's namespace, want 12's", covers[len(covers)-1].pid)
	}
}

// csTakenNumber is a reboot after which two-plane took the number of cv-a's
// share: node 254 is now "Wendy sensor 0", another device's stream.
func csTakenNumber(t *testing.T) *csHarness {
	t.Helper()
	h := newCSHarness(t, csBrio())
	h.writeState(`{"version":1,"containers":{"cv-a":{"app":"sh.wendy.cv-a","shares":[` +
		csStateRecord(csBrio(), "/dev/video0", testSharedMode, 254) + `]}}}`)
	h.loop.addLiveAux(254, "Wendy sensor 0")
	h.s.loadCameraShares()
	return h
}

func noCoverFrom(t *testing.T, h *csHarness, src string) {
	t.Helper()
	for _, c := range h.coverList() {
		if c.src == src {
			t.Errorf("%s was put over %s for pid %d: it is not the share's node", c.src, c.dst, c.pid)
		}
	}
}

// Fix round 1, I1 (start): a start while the previous start's goroutine is
// still making the node never covers with the number the record named.
func TestCoverCameraShares_ARestartWhileItsNodeIsBeingMadeCoversNothingStale(t *testing.T) {
	h := csTakenNumber(t)
	entered, release := h.blockOpenOf("/dev/video255") // made in 254's place
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	first := make(chan []error, 1)
	go func() { first <- h.s.CoverCameraShares(context.Background(), "cv-a", 11) }()
	<-entered
	if errs := <-first; len(errs) != 1 || !errors.Is(errs[0], devcover.ErrTimeout) {
		t.Fatalf("first start: %v, want the timeout", errs)
	}

	// A crash loop: the next start, while the node is still being made.
	errs := h.s.CoverCameraShares(context.Background(), "cv-a", 12)
	if len(errs) != 1 || !errors.Is(errs[0], errShareNodeMaking) {
		t.Fatalf("second start: %v, want %q", errs, errShareNodeMaking)
	}
	if n := h.logged("camera share: cv-a starts with the real camera Brio 101 (/dev/video0): its node is still being made"); n != 1 {
		t.Errorf("being-made line logged %d times; logs: %v", n, h.logs.All())
	}
	releaseOnce.Do(func() { close(release) })
	h.drainCovers()
	noCoverFrom(t, h, "/dev/video254")
	if got := h.coverList(); len(got) != 0 {
		t.Errorf("covers = %+v, want none: both starts went ahead without one", got)
	}
	if v := h.view("cv-a"); v[0].node != 255 || !v[0].pump || v[0].opening {
		t.Errorf("record = %+v, want node 255 with a pump", v[0])
	}
}

// Fix round 1, I1 (replug): a running container whose record still names a
// number that is not its node (making it again failed) gets nothing over the
// camera when it comes back.
func TestRecover_NeverPutsANodeThatIsNotTheSharesOverTheCamera(t *testing.T) {
	h := csTakenNumber(t)
	h.loop.mu.Lock()
	h.loop.shareErr = errors.New("CTL_ADD: no space left on device")
	h.loop.mu.Unlock()
	if errs := h.s.CoverCameraShares(context.Background(), "cv-a", 11); len(errs) != 1 {
		t.Fatalf("errors = %v, want the failed make", errs)
	}
	h.watch()
	h.unplug("video0")
	h.plug(csBrio())
	h.waitLogged("camera share: cv-a has the real camera Brio 101 (/dev/video0) after it came back: not covering it again: "+
		"its node /dev/video254 is gone or is now another node", 1)
	noCoverFrom(t, h, "/dev/video254")
	if got := h.coverList(); len(got) != 0 {
		t.Errorf("covers = %+v, want none", got)
	}
}

// Fix round 1, I1 (sync): a replug missed while the agent was down is not
// covered with a number that is not the record's node.
func TestSyncCameraShares_NeverPutsANodeThatIsNotTheSharesOverTheCamera(t *testing.T) {
	h := csTakenNumber(t)
	h.setCovered(false)
	h.s.SyncCameraShares(context.Background(), []CameraShareContainer{{ID: "cv-a", Running: true, PID: 4242}})
	noCoverFrom(t, h, "/dev/video254")
	if got := h.coverList(); len(got) != 0 {
		t.Errorf("covers = %+v, want none", got)
	}
	if n := h.logged("camera share: cv-a: not covering /dev/video0 (Brio 101) again: its node /dev/video254 is gone or is now another node"); n != 1 {
		t.Errorf("not-covering line logged %d times; logs: %v", n, h.logs.All())
	}
}

// Fix round 1, M2: a sync never swaps the namespace a start held moments ago
// for one opened through its (older) listing's pid.
func TestSyncCameraShares_KeepsANamespaceAStartHeldJustNow(t *testing.T) {
	h := newCSHarness(t, csBrio())
	h.s.CreateCameraShares(context.Background(), "cv-a", "sh.wendy.cv-a")
	if errs := h.s.CoverCameraShares(context.Background(), "cv-a", 11); len(errs) != 0 {
		t.Fatalf("errors: %v", errs)
	}
	listed := []CameraShareContainer{{ID: "cv-a", Running: true, PID: 99}} // the task before, its pid since reused
	h.s.SyncCameraShares(context.Background(), listed)
	if n, closed := h.nsOpens(99), h.nsCloses(); n != 0 || len(closed) != 0 {
		t.Fatalf("a sync opened %d namespaces through pid 99 and closed %v", n, closed)
	}
	h.ageNamespaces(cameraShareSyncGrace)
	h.s.SyncCameraShares(context.Background(), listed)
	if n := h.nsOpens(99); n != 1 {
		t.Errorf("an old namespace held through another pid: %d opened through 99, want 1", n)
	}
	if !csWaitFor(time.Second, func() bool { return reflect.DeepEqual(h.nsCloses(), []uint32{11}) }) {
		t.Errorf("closed = %v, want 11's", h.nsCloses())
	}
}

// Fix round 1, M3: a record followed to a new port learns, in the same event,
// the cameras of its model beside it there.
func TestRecover_AFollowedCameraLearnsTheCamerasBesideItsNewPort(t *testing.T) {
	brio := csBrio()
	other := csCamera{"video2", "Brio 101", brio.formats, camera.Identity{Device: csPort("1-4"), Index: "0", Name: "Brio 101"}}
	moved := csCamera{"video4", "Brio 101", brio.formats, camera.Identity{Device: csPort("1-3"), Index: "0", Name: "Brio 101"}}
	h := newCSHarness(t, brio)
	h.s.CreateCameraShares(context.Background(), "cv-a", "sh.wendy.cv-a")
	// cv-b shares another camera of the model, never seen beside cv-a's.
	h.mu.Lock()
	delete(h.cams, brio.base)
	h.cams[other.base] = other
	h.mu.Unlock()
	h.s.CreateCameraShares(context.Background(), "cv-b", "sh.wendy.cv-b")
	if a, b := h.view("cv-a"), h.view("cv-b"); len(a) != 1 || len(b) != 1 || len(a[0].siblings) != 0 {
		t.Fatalf("records: cv-a %+v, cv-b %+v", a, b)
	}
	h.watch()

	h.plug(moved)
	h.waitLogged("camera share: /dev/video4 appeared; no running app shares it", 1)
	if v := h.view("cv-a")[0]; v.camera != moved.id || !reflect.DeepEqual(v.siblings, []string{other.id.Device}) {
		t.Errorf("cv-a's record = %+v, want it at %s with %s as a sibling", v, moved.id.Device, other.id.Device)
	}
	if got := h.stateShares("cv-a")[0]["sameModelPorts"]; !reflect.DeepEqual(got, []any{other.id.Device}) {
		t.Errorf("cv-a's sameModelPorts in the state file = %v", got)
	}
}

// Fix round 1, M5: a replug while the watch could not run gave no event; once
// the watch starts, the cover it took is made again.
func TestRecover_AReplugWhileTheWatchWasDownIsCoveredWhenItStarts(t *testing.T) {
	h := newCSHarness(t, csBrio())
	h.s.CreateCameraShares(context.Background(), "cv-a", "sh.wendy.cv-a")
	if errs := h.s.CoverCameraShares(context.Background(), "cv-a", 11); len(errs) != 0 {
		t.Fatalf("errors: %v", errs)
	}
	h.watcher.mu.Lock()
	h.watcher.fails = 2
	h.watcher.mu.Unlock()
	h.setCovered(false) // the cover went with a replug nobody watched
	h.watch()
	h.waitLogged("camera share: covered /dev/video0 for cv-a again; it was replugged while /dev was not watched", 1)
	if covers := h.coverList(); len(covers) != 2 || covers[1] != (csCover{11, "/dev/video255", "/dev/video0", cameraShareRecoverTimeout}) {
		t.Errorf("covers = %+v, want the start's and one again", covers)
	}
	if n := h.logged("camera share: watching /dev for cameras failed: inotify_add_watch(/dev): no space left on device; trying again in "); n != 2 {
		t.Errorf("failure line logged %d times, want 2", n)
	}
}

// Fix round 1, M8: what a start saw before it waited for the node lock (a
// removal's /proc scan can hold it) is checked again before a node is made.
func TestCoverCameraShares_ARemakeChecksAgainAfterWaitingForTheNodeLock(t *testing.T) {
	rebooted := func(t *testing.T) *csHarness {
		h := newCSHarness(t, csBrio())
		h.writeState(`{"version":1,"containers":{"cv-a":{"app":"sh.wendy.cv-a","shares":[` +
			csStateRecord(csBrio(), "/dev/video0", testSharedMode, 254) + `]}}}`)
		h.s.loadCameraShares()
		return h
	}

	t.Run("the start's budget ran out", func(t *testing.T) {
		h := rebooted(t)
		h.s.twoPlaneStartMu.Lock()
		errs := h.s.CoverCameraShares(context.Background(), "cv-a", 11)
		h.s.twoPlaneStartMu.Unlock()
		if len(errs) != 1 || !errors.Is(errs[0], devcover.ErrTimeout) {
			t.Fatalf("errors = %v, want the timeout", errs)
		}
		h.drainCovers()
		if got := h.loop.shareCalls(); len(got) != 0 {
			t.Errorf("a node was made after the start went ahead: %+v", got)
		}
		// Nothing is left claimed: the next start makes the node.
		if errs := h.s.CoverCameraShares(context.Background(), "cv-a", 12); len(errs) != 0 {
			t.Fatalf("next start: %v", errs)
		}
		if got := h.loop.shareCalls(); !reflect.DeepEqual(got, []auxCall{{254, "Brio 101"}}) {
			t.Errorf("EnsureShareNode calls = %+v, want 254 at the next start", got)
		}
	})

	t.Run("the share was removed", func(t *testing.T) {
		h := rebooted(t)
		h.s.twoPlaneStartMu.Lock()
		returned := make(chan []error, 1)
		go func() { returned <- h.s.CoverCameraShares(context.Background(), "cv-a", 11) }()
		if !csWaitFor(time.Second, func() bool { v := h.view("cv-a"); return len(v) == 1 && v[0].opening }) {
			h.s.twoPlaneStartMu.Unlock()
			t.Fatal("the start never reached the node lock")
		}
		h.s.RemoveCameraShares("cv-a")
		h.s.twoPlaneStartMu.Unlock()
		<-returned
		h.drainCovers()
		h.drainRemovals()
		if got, live := h.loop.shareCalls(), h.loop.liveAux(); len(got) != 0 || len(live) != 0 {
			t.Errorf("made %+v, live %v; want nothing for a removed share", got, live)
		}
	})
}

// Fix round 1, M9: a node that is there is covered before another is made,
// so a slow make cannot cost it the start's budget.
func TestCoverCameraShares_CoversNodesThatAreThereBeforeMakingOthers(t *testing.T) {
	h := newCSHarness(t, csBrio(), csC922())
	h.writeState(`{"version":1,"containers":{"cv-a":{"app":"sh.wendy.cv-a","shares":[` +
		csStateRecord(csC922(), "/dev/video2", testSharedMode, 254) + `,` +
		csStateRecord(csBrio(), "/dev/video0", testSharedMode, 255) + `]}}}`)
	h.loop.addLiveAux(255, "Brio 101") // there; C922's 254 is not
	h.s.loadCameraShares()
	_, release := h.blockOpenOf("/dev/video254")
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })

	errs := h.s.CoverCameraShares(context.Background(), "cv-a", 11)
	if got := coversWithout(h.coverList()); !reflect.DeepEqual(got, []csCover{{pid: 11, src: "/dev/video255", dst: "/dev/video0"}}) {
		t.Errorf("covers = %+v, want the Brio's node over it although the C922's is still being made", got)
	}
	if len(errs) != 1 || !strings.Contains(errs[0].Error(), "C922 (/dev/video2)") || !errors.Is(errs[0], devcover.ErrTimeout) {
		t.Errorf("errors = %v, want the C922's timeout alone", errs)
	}
	releaseOnce.Do(func() { close(release) })
}
