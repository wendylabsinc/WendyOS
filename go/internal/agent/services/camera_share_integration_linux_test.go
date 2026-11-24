//go:build linux && regression_camera_fixture

package services

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
	"golang.org/x/sys/unix"

	"github.com/wendylabsinc/wendy/go/internal/agent/camera"
	"github.com/wendylabsinc/wendy/go/internal/agent/ipcam"
	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
	"github.com/wendylabsinc/wendy/go/internal/shared/camerashare"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

// Two apps share one camera through their own share nodes, the whole way
// through real kernel nodes: a v4l2loopback node fed JPEG frames stands in for
// a USB camera, and from CreateCameraShares on everything is the production
// path (the share policy, CTL_ADD of announce_all_caps nodes, the node writer
// with keep_format, the pumps, the hub and its GStreamer pipelines, and the
// queued, udev-settled node removal). It proves what the unit tests cannot:
// that each app's raw V4L2 reader gets the camera's picture from one capture
// with no encoder, stamped with the capture time; that one app leaving does
// not stop the other; that the camera stops with its last reader; and that a
// new reader waits for a fresh picture, never black or a previous session's frame.
//
// What it fakes is the camera listing's view of the fixture: the device list
// holds only the fixture node, which is listed as a USB camera, and its camera
// identity is made up, because the node has no sysfs device link and so
// camera.IdentityOf finds none for it.
//
// The CI media job runs it after merge; on a device it runs as root beside the
// agent. Its share nodes come from the agent's band (as production's do), from
// a registry and a state file of its own, and it never builds the service with
// NewVideoService, which loads the agent's camera registry and share state.
func TestCameraShareTwoReadersDecodeRealFrames(t *testing.T) {
	requireShareCaptureFixture(t)

	containers := []string{"ci-app-a", "ci-app-b"}
	fx := startShareCaptureFixture(t, containers...)
	ctx, logs, path, feed, statePath, svc := fx.ctx, fx.logs, fx.path, fx.feed, fx.statePath, fx.svc

	// Step 3: two containers are created, and each shares the camera.
	nodes := map[string]string{}
	var mode sharedMode
	for i, id := range containers {
		svc.CreateCameraShares(ctx, id, fmt.Sprintf("sh.wendy.ci.%c", 'a'+i), nil)
		r, ok := shareRecordOf(svc, id)
		if !ok {
			t.Fatalf("camera was not shared: %s", shareRefusalReason(logs, id))
		}
		fx.made = append(fx.made, r.node)
		if r.cameraPath != path {
			t.Fatalf("%s shares %s, want the camera %s", id, r.cameraPath, path)
		}
		nodes[id] = shareNodePath(r.node)
		mode = r.mode
		t.Logf("%s shares %s through %s at %s", id, path, nodes[id], r.mode)
	}
	if nodes[containers[0]] == nodes[containers[1]] {
		t.Fatalf("both containers share through %s, want a node each", nodes[containers[0]])
	}
	if data, err := os.ReadFile(statePath); err != nil {
		t.Fatalf("reading the state file: %v", err)
	} else {
		var state cameraShareState
		if err := json.Unmarshal(data, &state); err != nil {
			t.Fatalf("the state file does not parse: %v: %s", err, data)
		}
		for i, id := range containers {
			if c := state.Containers[id]; len(c.Shares) != 1 || c.Shares[0].Node != fx.made[i] || c.Shares[0].Path != path {
				t.Fatalf("the state file does not hold %s's share of %s at node %d: %s", id, path, fx.made[i], data)
			}
		}
	}
	const frameBytes = pixelsFixtureWidth * pixelsFixtureHeight * 2
	if mode.Width != pixelsFixtureWidth || mode.Height != pixelsFixtureHeight || mode.Fourcc != "YUYV" ||
		mode.BytesPerLine != pixelsFixtureWidth*2 || mode.frameBytes() != frameBytes {
		t.Fatalf("shared mode is %s with %d bytes per line, want %dx%d YUYV with %d", mode, mode.BytesPerLine,
			pixelsFixtureWidth, pixelsFixtureHeight, pixelsFixtureWidth*2)
	}

	// Step 4: each node is a camera with the shared format, and nothing runs
	// the camera while no app reads.
	for _, id := range containers {
		probe := openShareTestReader(t, nodes[id])
		var got v4l2Format
		got.Type = v4l2BufTypeVideoCapture
		if err := probe.ioctl(vidiocGFmtForShareTest, unsafe.Pointer(&got)); err != nil {
			t.Fatalf("G_FMT(CAPTURE) on %s: %v", nodes[id], err)
		}
		probe.close()
		if got.Width != pixelsFixtureWidth || got.Height != pixelsFixtureHeight || got.PixelFormat != v4l2PixFmtYUYV ||
			got.BytesPerLine != pixelsFixtureWidth*2 {
			t.Fatalf("G_FMT(CAPTURE) on %s = %dx%d %s, %d bytes per line; want %dx%d YUYV, %d", nodes[id],
				got.Width, got.Height, fourccName(got.PixelFormat), got.BytesPerLine,
				pixelsFixtureWidth, pixelsFixtureHeight, pixelsFixtureWidth*2)
		}
	}
	if n := hubCount(svc); n != 0 {
		t.Fatalf("camera is running with no reader: %d hubs", n)
	}

	// Step 5: app B joins while app A is already receiving the source. Each
	// app owns a separate capture node, backed by the same physical capture.
	readerA := openShareAppReader(t, nodes[containers[0]])
	readerB := openShareAppReader(t, nodes[containers[1]])
	for _, r := range []*shareAppReader{readerA, readerB} {
		started := monotonicNs()
		if err := r.start(); err != nil {
			t.Fatalf("reader STREAMON on %s: %v", r.path, err)
		}
		frame, stamp, err := r.next(ctx, 10*time.Second)
		if err != nil {
			t.Fatalf("first source frame on %s: %v", r.path, err)
		}
		if problem := sharePictureProblem(frame, frameBytes); problem != "" || stamp < started/1000*1000 {
			t.Fatalf("FRESH_CAPTURE_CONTRACT: app's first frame on %s is not fresh source pixels: %s timestamp=%d start=%d", r.path, problem, stamp, started)
		}
	}

	const wantFrames = 10
	got, problem := readPictures(ctx, []*shareAppReader{readerA, readerB}, wantFrames, 10*time.Second, frameBytes)
	if problem != "" {
		t.Fatalf("%s; hubs: %d; feed: %v", problem, hubCount(svc), feed.err())
	}
	t.Logf("both readers received the source picture, stamped with its capture time: %v frames", got)

	// Step 6: app readers want pixels only, so the pipeline has no encoder.
	hub, ok := hubOf(svc, path)
	if !ok {
		t.Fatalf("no hub runs %s while both apps read", path)
	}
	if n := hubCount(svc); n != 1 {
		t.Fatalf("%d hubs run while both apps read one camera, want 1", n)
	}
	if hub.pipelineEncodes() {
		t.Fatal("pipeline is encoding with only app readers")
	}

	// Step 7: app A stops reading; app B keeps its frames. 10 frames at 15 fps
	// take under a second; the 5 s allow for a hosted runner under -race. A
	// reader that stalled when A left still yields at most the 2 frames its
	// buffers held, so this still catches it.
	readerA.close()
	got, problem = readPictures(ctx, []*shareAppReader{readerB}, wantFrames, 5*time.Second, frameBytes)
	if problem != "" {
		t.Fatalf("one app leaving stopped another app's frames: %s; hub error: %v", problem, hub.terminalErr())
	}
	t.Logf("after %s stopped reading, %s received %v more frames", readerA.path, readerB.path, got)

	// Step 8: app B stops too; the camera stops with its last reader.
	readerB.close()
	deadline := time.Now().Add(3 * time.Second)
	for hubCount(svc) != 0 {
		if time.Now().After(deadline) {
			t.Fatal("camera still running with no reader 3 s after the last one stopped")
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Step 9: reopening a dormant node waits for the next real camera picture.
	// Neither blank placeholders nor pictures from before STREAMON are valid.
	for attempt := 0; attempt < 3; attempt++ {
		again := openShareAppReader(t, nodes[containers[0]])
		restarted := monotonicNs()
		if err := again.start(); err != nil {
			t.Fatalf("new reader STREAMON on %s: %v", again.path, err)
		}
		frame, ts, err := again.next(ctx, 10*time.Second)
		if err != nil {
			t.Fatalf("new reader got no first frame: %v; feed: %v", err, feed.err())
		}
		if problem := sharePictureProblem(frame, frameBytes); problem != "" {
			t.Fatalf("FRESH_CAPTURE_CONTRACT: first frame on %s was not the source picture: %s", again.path, problem)
		}
		if ts < restarted/1000*1000 {
			t.Fatalf("FRESH_CAPTURE_CONTRACT: first frame on %s predates STREAMON by %s", again.path, time.Duration(restarted-ts))
		}
		again.close()
		deadline = time.Now().Add(3 * time.Second)
		for hubCount(svc) != 0 {
			if time.Now().After(deadline) {
				t.Fatal("camera still running after reopened reader stopped")
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	t.Log("FRESH_CAPTURE_FIRST_PICTURE_VERIFIED")

	// Step 10: both containers are removed; their nodes and records go.
	for _, id := range containers {
		svc.RemoveCameraShares(id)
	}
	if problem := waitShareNodesRemoved(svc, fx.made, statePath, 15*time.Second); problem != "" {
		t.Fatal(problem)
	}
	t.Logf("removing both containers removed %v and left no containers in the state file", fx.made)
}

// Each app's node follows its entitlement, the whole way through real kernel
// nodes: one capture of one JPEG camera fills a node of the camera's own
// frames and a node of H.264, every buffer stamped with its frame's sample ID,
// beside a node of decoded pictures stamped with capture times.
func TestCameraShareNodesFollowTheirEntitlements(t *testing.T) {
	requireShareCaptureFixture(t)
	cameraEntitlement := func(format, stamp string) []appconfig.Entitlement {
		return []appconfig.Entitlement{{Type: appconfig.EntitlementCamera, Format: format, Stamp: stamp}}
	}
	apps := []struct {
		id           string
		entitlements []appconfig.Entitlement
		format       string
		stamp        string
		pixelFormat  uint32
	}{
		{"ci-app-native", cameraEntitlement(appconfig.CameraFormatNative, appconfig.CameraStampFrame), appconfig.CameraFormatNative, appconfig.CameraStampFrame, v4l2PixFmtMJPEG},
		{"ci-app-h264", cameraEntitlement(appconfig.CameraFormatH264, appconfig.CameraStampFrame), appconfig.CameraFormatH264, appconfig.CameraStampFrame, v4l2PixFmtH264},
		{"ci-app-pixels", cameraEntitlement("", ""), appconfig.CameraFormatPixels, appconfig.CameraStampTime, v4l2PixFmtYUYV},
	}
	fx := startShareCaptureFixture(t, apps[0].id, apps[1].id, apps[2].id)
	ctx, svc, path := fx.ctx, fx.svc, fx.path

	readers := map[string]*shareAppReader{}
	for _, app := range apps {
		svc.CreateCameraShares(ctx, app.id, "sh.wendy."+app.id, app.entitlements)
		r, ok := shareRecordOf(svc, app.id)
		if !ok {
			t.Fatalf("%s: camera was not shared: %s\nentitlements passed: %+v\nagent log: %q",
				app.id, shareRefusalReason(fx.logs, app.id), app.entitlements, shareLogLines(fx.logs, app.id))
		}
		fx.made = append(fx.made, r.node)
		resolvedFormat, resolvedStamp := appconfig.CameraNodeSettings(app.entitlements, r.cameraPath, r.cameraName)
		diagnosis := fmt.Sprintf("entitlements passed: %+v\nsettings they resolve to for %q at %s: %s\nshare record: %+v\nagent log: %q",
			app.entitlements, r.cameraName, r.cameraPath, describeShareSettings(resolvedFormat, resolvedStamp), r, shareLogLines(fx.logs, app.id))
		if r.askedFormat == app.format {
			if os.Getenv("WENDY_GSTREAMER_REQUIRED") == "1" {
				t.Fatalf("required: a %s node: %s\n%s", app.format, r.askedWhyNot, diagnosis)
			}
			t.Skipf("this device serves pixels for %s: %s", app.format, r.askedWhyNot)
		}
		if r.mode.Format != app.format || r.stamp != app.stamp || r.mode.pixelFormat() != app.pixelFormat ||
			r.mode.Width != pixelsFixtureWidth || r.mode.Height != pixelsFixtureHeight {
			t.Fatalf("%s shares at %s, want %dx%d %s as %s\n%s", app.id, describeShareNode(r.mode, r.stamp),
				pixelsFixtureWidth, pixelsFixtureHeight, fourccName(app.pixelFormat), describeShareSettings(app.format, app.stamp), diagnosis)
		}
		node := shareNodePath(r.node)
		probe := openShareTestReader(t, node)
		var got v4l2Format
		got.Type = v4l2BufTypeVideoCapture
		if err := probe.ioctl(vidiocGFmtForShareTest, unsafe.Pointer(&got)); err != nil {
			t.Fatalf("G_FMT(CAPTURE) on %s: %v", node, err)
		}
		probe.close()
		if got.Width != pixelsFixtureWidth || got.Height != pixelsFixtureHeight || got.PixelFormat != app.pixelFormat ||
			got.SizeImage == 0 || got.SizeImage != uint32(r.mode.bufferBytes()) {
			t.Fatalf("G_FMT(CAPTURE) on %s = %dx%d %s, image of %d bytes; want %s with the %d bytes the share holds as its largest frame\n%s", node,
				got.Width, got.Height, fourccName(got.PixelFormat), got.SizeImage, r.mode, r.mode.bufferBytes(), diagnosis)
		}
		t.Logf("%s shares %s through %s at %s", app.id, path, node, describeShareNode(r.mode, r.stamp))
		readers[app.id] = openShareAppReader(t, node)
	}

	started := monotonicNs()
	for _, app := range apps {
		if err := readers[app.id].start(); err != nil {
			t.Fatalf("%s: reader STREAMON: %v", app.id, err)
		}
	}
	// Every reader dequeues from its STREAMON on, as an app does. A node keeps
	// only its newest shareNodeBuffers frames: a reader that streams and then
	// waits is moved on to the newest frame, which on an H.264 node is not one
	// it can start on.
	const wantFrames = 10
	type readFrames struct {
		frames [][]byte
		stamps []int64
		err    error
	}
	reading := map[string]chan readFrames{}
	for _, app := range apps {
		id, done := app.id, make(chan readFrames, 1)
		reading[id] = done
		go func() {
			var got readFrames
			for len(got.frames) < wantFrames && got.err == nil {
				frame, stamp, err := readers[id].next(ctx, 15*time.Second)
				if err != nil {
					got.err = fmt.Errorf("frame %d: %w", len(got.frames), err)
					break
				}
				got.frames, got.stamps = append(got.frames, frame), append(got.stamps, stamp)
			}
			done <- got
		}()
	}
	results := map[string]readFrames{}
	for _, app := range apps {
		results[app.id] = <-reading[app.id]
	}
	read := func(id string) (frames [][]byte, stamps []int64) {
		t.Helper()
		if err := results[id].err; err != nil {
			record, _ := shareRecordOf(svc, id)
			t.Fatalf("%s: %v; share state: %s; feed: %v\nshare record: %+v\nagent log: %q", id, err,
				svc.CameraShareReport(ctx)[path][camerashare.KeyState], fx.feed.err(), record, shareLogLines(fx.logs, id))
		}
		return results[id].frames, results[id].stamps
	}
	// A sample ID is read back as tv_sec*1e6 + tv_usec: the timestamp in
	// whole microseconds.
	sampleIDs := func(id string, stamps []int64) []uint64 {
		t.Helper()
		ids := make([]uint64, len(stamps))
		for i, stamp := range stamps {
			ids[i] = uint64(stamp / 1000)
			if stamp <= 0 || stamp%1000 != 0 || (i > 0 && ids[i] <= ids[i-1]) {
				t.Fatalf("%s: buffer timestamps %v are not increasing sample IDs", id, stamps)
			}
		}
		return ids
	}

	jpegs, jpegStamps := read(apps[0].id)
	for i, frame := range jpegs {
		if whole, ok := jpegFrameInBuffer(frame); !ok || len(whole) != len(frame) {
			t.Fatalf("native node frame %d (%d bytes) is not one whole JPEG image", i, len(frame))
		}
	}
	jpegIDs := sampleIDs(apps[0].id, jpegStamps)

	units, unitStamps := read(apps[1].id)
	if _, randomAccess := h264RandomAccessOffset(units[0]); !randomAccess {
		t.Fatalf("h264 node's first frame (%d bytes) cannot be decoded on its own: it holds no parameter sets followed by an IDR slice.\n%s",
			len(units[0]), describeH264Buffers(units[:3], readers[apps[1].id].sequences))
	}
	for i, unit := range units {
		if _, _, ok := h264NextNAL(unit, 0); !ok {
			t.Fatalf("h264 node frame %d (%d bytes) holds no NAL unit", i, len(unit))
		}
	}
	unitIDs := sampleIDs(apps[1].id, unitStamps)

	const frameBytes = pixelsFixtureWidth * pixelsFixtureHeight * 2
	pictures, pictureStamps := read(apps[2].id)
	for i, frame := range pictures {
		if problem := sharePictureProblem(frame, frameBytes); problem != "" || pictureStamps[i] < started/1000*1000 || pictureStamps[i] > monotonicNs() {
			t.Fatalf("pixels node frame %d: %s; timestamp %d is not a capture time between %d and now", i, problem, pictureStamps[i], started)
		}
	}

	// One capture numbers the frames of both compressed nodes.
	if n := hubCount(svc); n != 1 {
		t.Fatalf("%d hubs run while three apps read one camera, want 1", n)
	}
	svc.mu.Lock()
	minted := svc.sampleSeqs[path].Load()
	svc.mu.Unlock()
	for id, ids := range map[string][]uint64{apps[0].id: jpegIDs, apps[1].id: unitIDs} {
		if last := ids[len(ids)-1]; last > minted {
			t.Fatalf("%s: sample ID %d is beyond the %d the camera's capture has minted", id, last, minted)
		}
	}
	want := "ci-app-h264 [format h264, stamp frame] (reading), ci-app-native [format native, stamp frame] (reading), ci-app-pixels [format pixels, stamp time] (reading)"
	if state := svc.CameraShareReport(ctx)[path][camerashare.KeyState]; state != want {
		t.Fatalf("share state = %q\nwant        %q", state, want)
	}
	t.Logf("native frames %v, H.264 access units %v, and %d pictures came from one capture", jpegIDs, unitIDs, len(pictures))

	for _, app := range apps {
		readers[app.id].close()
		svc.RemoveCameraShares(app.id)
	}
	if problem := waitShareNodesRemoved(svc, fx.made, fx.statePath, 15*time.Second); problem != "" {
		t.Fatal(problem)
	}
}

// shareCaptureFixture is a JPEG camera on a v4l2loopback node and a video
// service wired for it as NewVideoService wires local cameras, with a loopback
// manager, registry and state file of its own. Its cleanup removes the shares
// of containers and waits for the nodes in made to go.
type shareCaptureFixture struct {
	ctx        context.Context
	logs       *observer.ObservedLogs
	path       string
	feed       *mjpegFeed
	statePath  string
	svc        *VideoService
	containers []string
	made       []int
}

func startShareCaptureFixture(t *testing.T, containers ...string) *shareCaptureFixture {
	t.Helper()
	fx := &shareCaptureFixture{containers: containers}
	core, logs := observer.New(zap.DebugLevel)
	logger := zap.New(core)
	// Cleanups run last to first. This one runs last: the agent's own account
	// of what it did, once everything has stopped.
	t.Cleanup(func() {
		if t.Failed() {
			for _, entry := range logs.All() {
				t.Logf("agent log: %s %s %v", entry.Level, entry.Message, entry.ContextMap())
			}
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	t.Cleanup(cancel)

	// Step 1: the camera, fed 320x240 red/green JPEG at 15 fps.
	path := createPixelsFixtureNode(t, ctx, logger)
	feed := feedMJPEG(t, ctx, path, redGreenJPEG(t, pixelsFixtureWidth, pixelsFixtureHeight))
	deadline := time.Now().Add(5 * time.Second)
	for {
		w, h := bestDefaultFrameSizeForDevice(path)
		if w == pixelsFixtureWidth && h == pixelsFixtureHeight && deviceSupportsMJPEGSize(path, w, h) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s did not become a %dx%d MJPEG camera: default size %dx%d; feed: %v", path, pixelsFixtureWidth, pixelsFixtureHeight, w, h, feed.err())
		}
		time.Sleep(20 * time.Millisecond)
	}
	base := filepath.Base(path)
	readName := func(base string) (string, error) {
		b, err := os.ReadFile(filepath.Join("/sys/class/video4linux", base, "name"))
		return strings.TrimSpace(string(b)), err
	}
	cameraName, err := readName(base)
	if err != nil {
		t.Fatalf("reading %s's name: %v", path, err)
	}
	fixtureID := camera.Identity{Device: "wendy-test-fixture:" + base, Index: "0", Name: cameraName}

	// Step 2: the service, as NewVideoService wires it for local cameras, with
	// a loopback manager, registry and state file of its own.
	prevStatePath := cameraSharesPath
	statePath := filepath.Join(t.TempDir(), "camera-shares.json")
	cameraSharesPath = statePath
	t.Cleanup(func() { cameraSharesPath = prevStatePath })
	store := t.TempDir()
	loopback := ipcam.NewLoopback(ctx, logger, ipcam.NewRegistry(filepath.Join(store, "ipcams.json")),
		ipcam.NewCredentialStore(filepath.Join(store, "ipcam-credentials.json")), nil)
	svcCtx, svcCancel := context.WithCancel(ctx)
	svc := &VideoService{
		logger:           logger,
		ctx:              svcCtx,
		cancel:           svcCancel,
		hubs:             map[string]*deviceHub{},
		sampleSeqs:       map[string]*atomic.Uint64{},
		encoderIdleGrace: 300 * time.Millisecond,
		loopback:         loopback,
		globDevices:      func() ([]string, error) { return []string{path}, nil },
		// NewVideoService's QUERYCAP check.
		hasVideoCapture: func(path string) bool {
			fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
			if err != nil {
				return false
			}
			defer unix.Close(fd) //nolint:errcheck
			var caps v4l2Capability
			_, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), vidiocQueryCap, uintptr(unsafe.Pointer(&caps)))
			return errno == 0 && caps.hasVideoCapture()
		},
		enumerateLibcamera: func(context.Context) (map[string]string, error) { return nil, nil },
		classifyTransport: func(b string) (camera.Transport, string) {
			if b == base {
				return camera.TransportUSB, "uvcvideo"
			}
			return camera.Classify(b)
		},
		capturePixelFormats: v4l2CapturePixelFormats,
		readDeviceName:      readName,
		// A node busy at start must fail this test, not reach the device's
		// PipeWire session through the contention fallback.
		findCameraSource: func(context.Context, string) (uint64, bool) { return 0, false },
		cameraIdentity: func(b string) (camera.Identity, error) {
			if b == base {
				return fixtureID, nil
			}
			return camera.IdentityOf(b)
		},
		presentCameras: func() map[string]camera.Identity { return map[string]camera.Identity{base: fixtureID} },
	}
	svc.startProducer = func(ctx context.Context, h *deviceHub, path string, req *agentpb.StreamVideoRequest) {
		svc.wg.Add(1)
		go svc.runProducer(ctx, h, path, req)
	}
	// Registered before any reader opens a node, so it runs after every reader
	// has closed its node. The shares go first and their nodes are removed
	// (Shutdown would cancel a removal still waiting for its node to settle),
	// then every pipeline exits and releases the camera, then the feed closes
	// it, and only then is the camera node removed.
	t.Cleanup(func() {
		for _, id := range fx.containers {
			svc.RemoveCameraShares(id)
		}
		// A drain that times out here (a node another process keeps open, say)
		// is reported, and Shutdown then gives up on the removal, which leaves
		// that share node behind. That is acceptable only because the CI runner
		// is disposable: this test's records are in a state file of its own, so
		// run on a device such a node stays until the device reboots.
		if problem := waitShareNodesRemoved(svc, fx.made, statePath, 15*time.Second); problem != "" {
			t.Errorf("cleanup: %s", problem)
		}
		svc.Shutdown()
		if err := feed.stop(); err != nil {
			t.Errorf("MJPEG feed failed: %v", err)
		}
		var launches []any
		for _, entry := range logs.FilterMessage("starting camera pipeline").All() {
			launches = append(launches, entry.ContextMap()["encodes"])
		}
		t.Logf("pipeline launches, whether each encodes: %v", launches)
	})
	// The element listing is memoised for the service's life. Warmed here, so
	// the create budget (cameraShareCreateBudget) measures the shares' own
	// setup and not a first listing of every installed GStreamer plugin.
	listStart := time.Now()
	if _, _, err := svc.gstElements(); err != nil {
		t.Fatalf("listing GStreamer elements: %v", err)
	}
	t.Logf("listed GStreamer elements in %s", time.Since(listStart).Round(time.Millisecond))
	fx.ctx, fx.logs, fx.path, fx.feed, fx.statePath, fx.svc = ctx, logs, path, feed, statePath, svc
	return fx
}

// requireShareCaptureFixture skips, or fails under WENDY_GSTREAMER_REQUIRED=1,
// unless GStreamer has the elements a shared MJPEG camera's pipeline uses, and
// the real v4l2loopback module and root are available.
func requireShareCaptureFixture(t *testing.T) {
	t.Helper()
	unavailable := func(format string, args ...any) {
		t.Helper()
		if os.Getenv("WENDY_GSTREAMER_REQUIRED") == "1" {
			t.Fatalf("required "+format, args...)
		}
		t.Skipf(format+" (set WENDY_GSTREAMER_REQUIRED=1 to fail instead of skipping)", args...)
	}
	if _, err := exec.LookPath("gst-launch-1.0"); err != nil {
		unavailable("GStreamer is not installed: %v", err)
	}
	inspect, err := exec.LookPath("gst-inspect-1.0")
	if err != nil {
		unavailable("GStreamer is not installed: %v", err)
	}
	for _, element := range []string{"v4l2src", "jpegdec", "videoconvert", "queue", "fdsink"} {
		if out, err := exec.Command(inspect, element).CombinedOutput(); err != nil {
			unavailable("GStreamer element %s is unavailable: %v: %s", element, err, bytes.TrimSpace(out))
		}
	}
	requireShareModule(t)
}

// shareRecordOf returns the one share record a container holds.
func shareRecordOf(svc *VideoService, containerID string) (shareRecordCopy, bool) {
	svc.shares.mu.Lock()
	defer svc.shares.mu.Unlock()
	c := svc.shares.containers[containerID]
	if c == nil || len(c.shares) != 1 {
		return shareRecordCopy{}, false
	}
	r := c.shares[0]
	return shareRecordCopy{cameraName: r.cameraName, cameraPath: r.cameraPath, mode: r.mode, stamp: r.stamp,
		askedFormat: r.askedFormat, askedWhyNot: r.askedWhyNot, node: r.node}, true
}

// shareRecordCopy is what shareRecordOf copies out of a share record under its
// lock. It is a type of its own so that a field not copied cannot be read as
// an empty one.
type shareRecordCopy struct {
	cameraName, cameraPath   string
	mode                     sharedMode
	stamp                    string
	askedFormat, askedWhyNot string
	node                     int
}

// shareLogLines is every camera share line the agent logged that names the
// container.
func shareLogLines(logs *observer.ObservedLogs, containerID string) []string {
	var lines []string
	for _, entry := range logs.All() {
		if strings.HasPrefix(entry.Message, "camera share: ") && strings.Contains(entry.Message, containerID) {
			lines = append(lines, entry.Message)
		}
	}
	return lines
}

// shareRefusalReason is the reason CreateCameraShares logged for giving a
// container the real camera, or the camera share lines it logged at all.
func shareRefusalReason(logs *observer.ObservedLogs, containerID string) string {
	prefix := "camera share: " + containerID + " gets the real camera"
	var said []string
	for _, entry := range logs.All() {
		msg := entry.Message
		if rest, ok := strings.CutPrefix(msg, prefix+"s: "); ok {
			return rest
		}
		if rest, ok := strings.CutPrefix(msg, prefix+" "); ok {
			if _, reason, ok := strings.Cut(rest, "): "); ok {
				return reason
			}
			return rest
		}
		if strings.HasPrefix(msg, "camera share: ") {
			said = append(said, msg)
		}
	}
	return fmt.Sprintf("no reason was logged (camera share lines: %q)", said)
}

func hubCount(svc *VideoService) int {
	svc.mu.Lock()
	defer svc.mu.Unlock()
	return len(svc.hubs)
}

func hubOf(svc *VideoService, path string) (*deviceHub, bool) {
	svc.mu.Lock()
	defer svc.mu.Unlock()
	h, ok := svc.hubs[path]
	return h, ok
}

// waitShareNodesRemoved waits up to within for the queued removals to drain,
// the nodes to be gone, and the state file to hold neither containers nor
// removals, and says what is left if they do not. A removal deferred because
// another process held the node (the device's WirePlumber can open a node that
// announces capture) is tried again by SyncCameraShares, as the agent's next
// sync would.
func waitShareNodesRemoved(svc *VideoService, nodes []int, statePath string, within time.Duration) string {
	deadline := time.Now().Add(within)
	nextSync := time.Now().Add(2 * time.Second)
	for {
		problem := shareRemovalProblem(svc, nodes, statePath)
		if problem == "" {
			return ""
		}
		if time.Now().After(deadline) {
			return fmt.Sprintf("%s %s after the containers were removed", problem, within)
		}
		if time.Now().After(nextSync) {
			svc.SyncCameraShares(context.Background(), nil)
			nextSync = time.Now().Add(2 * time.Second)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func shareRemovalProblem(svc *VideoService, nodes []int, statePath string) string {
	svc.shares.mu.Lock()
	queued, records := len(svc.shares.removing), len(svc.shares.containers)
	svc.shares.mu.Unlock()
	if records != 0 {
		return fmt.Sprintf("%d containers still have share records", records)
	}
	if queued != 0 {
		return fmt.Sprintf("%d share node removals are still queued", queued)
	}
	for _, nr := range nodes {
		_, devErr := os.Lstat(shareNodePath(nr))
		_, sysErr := os.Lstat(fmt.Sprintf("/sys/class/video4linux/video%d", nr))
		if !errors.Is(devErr, os.ErrNotExist) || !errors.Is(sysErr, os.ErrNotExist) {
			return fmt.Sprintf("share node %s remains", shareNodePath(nr))
		}
	}
	data, err := os.ReadFile(statePath)
	if errors.Is(err, os.ErrNotExist) {
		return "" // no share was ever stored
	}
	if err != nil {
		return fmt.Sprintf("reading the state file: %v", err)
	}
	var state cameraShareState
	if err := json.Unmarshal(data, &state); err != nil {
		return fmt.Sprintf("the state file does not parse: %v: %s", err, data)
	}
	if len(state.Containers) != 0 || len(state.Removing) != 0 {
		return fmt.Sprintf("the state file still holds containers or removals: %s", data)
	}
	return ""
}

// readPictures reads readers together until each has received want frames of
// the source picture, for at most within. Every frame must be frameBytes long
// and show the picture, and each reader's buffer timestamps must increase and
// be within a second of CLOCK_MONOTONIC as the frame is read: the capture
// time. It returns how many frames each received, and the first problem.
func readPictures(ctx context.Context, readers []*shareAppReader, want int, within time.Duration, frameBytes int) ([]int, string) {
	counts := make([]int, len(readers))
	last := make([]int64, len(readers))
	deadline := time.Now().Add(within)
	for {
		done := true
		for i := range readers {
			if counts[i] < want {
				done = false
			}
		}
		if done {
			return counts, ""
		}
		remaining := time.Until(deadline)
		if remaining <= 0 || ctx.Err() != nil {
			var short []string
			for i, r := range readers {
				if counts[i] < want {
					short = append(short, fmt.Sprintf("%s received %d", r.path, counts[i]))
				}
			}
			return counts, fmt.Sprintf("%s frames within %s, want at least %d each", strings.Join(short, " and "), within, want)
		}
		fds := make([]unix.PollFd, len(readers))
		for i, r := range readers {
			fds[i] = unix.PollFd{Fd: int32(r.fd), Events: unix.POLLIN}
		}
		if _, err := unix.Poll(fds, int(min(remaining, 100*time.Millisecond).Milliseconds())+1); err != nil && !errors.Is(err, unix.EINTR) {
			return counts, fmt.Sprintf("poll: %v", err)
		}
		for i, r := range readers {
			if fds[i].Revents&unix.POLLIN == 0 {
				continue
			}
			frame, ts, ok, err := r.dequeue()
			if err != nil {
				return counts, fmt.Sprintf("reader DQBUF on %s: %v", r.path, err)
			}
			if !ok {
				continue
			}
			now := monotonicNs()
			if problem := sharePictureProblem(frame, frameBytes); problem != "" {
				return counts, fmt.Sprintf("frame on %s does not show the source picture: %s", r.path, problem)
			}
			if age := time.Duration(now - ts); age < -time.Second || age > time.Second || ts <= last[i] {
				return counts, fmt.Sprintf("buffer timestamp is not the capture time: frame %d on %s is stamped %d µs, %s before CLOCK_MONOTONIC now; the frame before it %d µs",
					counts[i]+1, r.path, ts/1000, age, last[i]/1000)
			}
			last[i] = ts
			counts[i]++
		}
	}
}

// sharePictureProblem says why frame is not a 320x240 YUYV frame of the
// redGreenJPEG picture, or "" when it is: the luma at (10,120), in the red
// half, must differ from (310,120), in the green half, by more than 40.
func sharePictureProblem(frame []byte, frameBytes int) string {
	if len(frame) != frameBytes {
		return fmt.Sprintf("%d bytes, want %d", len(frame), frameBytes)
	}
	left, right := shareFrameLuma(frame)
	if diff := left - right; diff <= 40 && diff >= -40 {
		return fmt.Sprintf("luma at (10,120) is %d and at (310,120) is %d", left, right)
	}
	return ""
}

// shareFrameLuma reads a 320x240 YUYV frame's luma at (10,120) and (310,120);
// the luma of pixel x is byte 2x of its row.
func shareFrameLuma(frame []byte) (left, right int) {
	row := 120 * pixelsFixtureWidth * 2
	return int(frame[row+2*10]), int(frame[row+2*310])
}

func describeShareFrame(frame, blank []byte, frameBytes int) string {
	switch {
	case bytes.Equal(frame, blank):
		return "black"
	case sharePictureProblem(frame, frameBytes) == "":
		return "the picture"
	case len(frame) != frameBytes:
		return fmt.Sprintf("a %d-byte frame", len(frame))
	default:
		left, right := shareFrameLuma(frame)
		return fmt.Sprintf("a frame with luma %d at (10,120) and %d at (310,120)", left, right)
	}
}

// shareAppReader is an app's raw V4L2 capture client of a share node: it
// streams with mmap buffers and queues each buffer again once it has copied
// the frame out, as an app reading a camera does.
type shareAppReader struct {
	path      string
	fd        int
	bufs      [][]byte
	streaming bool
	// sequences are the kernel's sequence numbers of the frames dequeued so far.
	sequences []uint32
}

func openShareAppReader(t *testing.T, path string) *shareAppReader {
	t.Helper()
	fd, err := unix.Open(path, unix.O_RDWR|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	if err != nil {
		t.Fatalf("opening %s as a reader: %v", path, err)
	}
	r := &shareAppReader{path: path, fd: fd}
	t.Cleanup(r.close)
	return r
}

func (r *shareAppReader) ioctl(req uintptr, arg unsafe.Pointer) error {
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(r.fd), req, uintptr(arg)); errno != 0 {
		return errno
	}
	return nil
}

// start requests the node's buffers, maps and queues each, and runs STREAMON.
func (r *shareAppReader) start() error {
	req := v4l2ReqBuffers{Count: shareNodeBuffers, Type: v4l2BufTypeVideoCapture, Memory: v4l2MemoryMmap}
	if err := r.ioctl(vidiocReqbufs, unsafe.Pointer(&req)); err != nil {
		return fmt.Errorf("REQBUFS: %w", err)
	}
	if req.Count == 0 {
		return errors.New("REQBUFS allocated no buffers")
	}
	for i := uint32(0); i < req.Count; i++ {
		var b v4l2Buf
		b.setIndex(i)
		b.setType(v4l2BufTypeVideoCapture)
		b.setMemory(v4l2MemoryMmap)
		if err := r.ioctl(vidiocQuerybuf, unsafe.Pointer(&b)); err != nil {
			return fmt.Errorf("QUERYBUF %d: %w", i, err)
		}
		mem, err := unix.Mmap(r.fd, int64(b.offset()), int(v4l2BufLength(&b)), unix.PROT_READ, unix.MAP_SHARED)
		if err != nil {
			return fmt.Errorf("mmap buffer %d: %w", i, err)
		}
		r.bufs = append(r.bufs, mem)
		if err := r.ioctl(vidiocQbuf, unsafe.Pointer(&b)); err != nil {
			return fmt.Errorf("QBUF %d: %w", i, err)
		}
	}
	typ := int32(v4l2BufTypeVideoCapture)
	if err := r.ioctl(vidiocStreamon, unsafe.Pointer(&typ)); err != nil {
		return err
	}
	r.streaming = true
	return nil
}

// describeH264Buffers lists, for each buffer a reader dequeued, its size, the
// kernel's sequence number and the type and size of every NAL unit in it. A
// first sequence number above the node's first says the reader did not get the
// first frame written.
func describeH264Buffers(buffers [][]byte, sequences []uint32) string {
	var out strings.Builder
	for i, buffer := range buffers {
		fmt.Fprintf(&out, "buffer %d: %d bytes", i, len(buffer))
		if i < len(sequences) {
			fmt.Fprintf(&out, ", sequence %d", sequences[i])
		}
		out.WriteString(", NAL units (type:bytes)")
		start, header, ok := h264NextNAL(buffer, 0)
		if !ok {
			out.WriteString(" none")
		}
		for ok {
			next, nextHeader, more := h264NextNAL(buffer, header+1)
			end := len(buffer)
			if more {
				end = next
			}
			fmt.Fprintf(&out, " %d:%d", buffer[header]&0x1f, end-start)
			start, header, ok = next, nextHeader, more
		}
		out.WriteString("\n")
	}
	return out.String()
}

// dequeue takes one ready frame: a copy of it and its timestamp, with the
// buffer queued again. ok is false when no frame was ready.
func (r *shareAppReader) dequeue() (frame []byte, ts int64, ok bool, err error) {
	var b v4l2Buf
	b.setType(v4l2BufTypeVideoCapture)
	b.setMemory(v4l2MemoryMmap)
	if err := r.ioctl(vidiocDqbuf, unsafe.Pointer(&b)); err != nil {
		if errors.Is(err, unix.EAGAIN) {
			return nil, 0, false, nil
		}
		return nil, 0, false, err
	}
	if int(b.index()) >= len(r.bufs) {
		return nil, 0, false, fmt.Errorf("DQBUF returned buffer %d of %d", b.index(), len(r.bufs))
	}
	frame = bytes.Clone(r.bufs[b.index()][:b.bytesUsed()])
	ts = b.timestampNanos()
	r.sequences = append(r.sequences, b.sequence())
	if err := r.ioctl(vidiocQbuf, unsafe.Pointer(&b)); err != nil {
		return nil, 0, false, fmt.Errorf("QBUF %d again: %w", b.index(), err)
	}
	return frame, ts, true, nil
}

// next waits up to within for the next frame.
func (r *shareAppReader) next(ctx context.Context, within time.Duration) ([]byte, int64, error) {
	deadline := time.Now().Add(within)
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return nil, 0, fmt.Errorf("no frame within %s", within)
		}
		if err := ctx.Err(); err != nil {
			return nil, 0, err
		}
		fds := []unix.PollFd{{Fd: int32(r.fd), Events: unix.POLLIN}}
		if _, err := unix.Poll(fds, int(min(remaining, 100*time.Millisecond).Milliseconds())+1); err != nil && !errors.Is(err, unix.EINTR) {
			return nil, 0, fmt.Errorf("poll: %w", err)
		}
		if fds[0].Revents&unix.POLLIN == 0 {
			continue
		}
		frame, ts, ok, err := r.dequeue()
		if err != nil {
			return nil, 0, fmt.Errorf("DQBUF: %w", err)
		}
		if ok {
			return frame, ts, nil
		}
	}
}

// close stops streaming, unmaps the buffers and closes the node. It may be
// called more than once.
func (r *shareAppReader) close() {
	if r.fd < 0 {
		return
	}
	if r.streaming {
		typ := int32(v4l2BufTypeVideoCapture)
		_ = r.ioctl(vidiocStreamoff, unsafe.Pointer(&typ))
		r.streaming = false
	}
	for _, m := range r.bufs {
		_ = unix.Munmap(m)
	}
	r.bufs = nil
	_ = unix.Close(r.fd)
	r.fd = -1
}
