package services

// Tests for a camera node that follows its app's entitlement: the format
// chooses the node's declared mode and the hub subscription that fills it, and
// the stamp chooses what each buffer's timestamp field holds.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
	"unsafe"

	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

var (
	sfNativeMode = sharedMode{Width: 1280, Height: 720, Fourcc: "MJPG", FPS: 30, Format: appconfig.CameraFormatNative, MaxFrameBytes: maxFrameBytes}
	sfH264Mode   = sharedMode{Width: 1280, Height: 720, Fourcc: "H264", FPS: 30, Format: appconfig.CameraFormatH264, MaxFrameBytes: maxFrameBytes}
)

func sfCameraEntitlement(format, stamp string, allowlist ...string) appconfig.Entitlement {
	return appconfig.Entitlement{Type: appconfig.EntitlementCamera, Format: format, Stamp: stamp, Allowlist: allowlist}
}

// sfJPEGFrame is one of the camera's own frames as native capture hands it on.
func sfJPEGFrame(sampleID uint64, kernelNs int64) *videoFrame {
	return &videoFrame{
		data: []byte{0xFF, 0xD8, 0xFF, 0xE0, byte(sampleID), 0xFF, 0xD9}, codec: videoCodecMJPEG,
		native: true, auAligned: true, sampleID: sampleID,
		tsNs: uint64(time.Now().UnixNano()), nativeNs: kernelNs, nativeClock: v4l2MonotonicClock,
	}
}

var (
	sfH264ParameterSets = []byte{0, 0, 0, 1, 0x67, 0x42, 0, 0, 0, 1, 0x68, 0xCE}
	sfH264IDRSlice      = []byte{0, 0, 0, 1, 0x65, 0x88, 0x84}
	sfH264InterSlice    = []byte{0, 0, 0, 1, 0x41, 0x9A, 0x02}
)

// sfH264Frame is one whole H.264 access unit: a random-access one carries its
// parameter sets and an IDR slice.
func sfH264Frame(sampleID uint64, randomAccess bool) *videoFrame {
	data := append([]byte(nil), sfH264InterSlice...)
	if randomAccess {
		data = append(append([]byte(nil), sfH264ParameterSets...), sfH264IDRSlice...)
	}
	return &videoFrame{data: data, codec: agentpb.VideoCodec_VIDEO_CODEC_H264, auAligned: true, sampleID: sampleID,
		tsNs: uint64(time.Now().UnixNano())}
}

func sfPictureFrame(sampleID uint64) *videoFrame {
	f := spModeFrame(uint64(time.Now().UnixNano()))
	f.codec, f.sampleID = agentpb.VideoCodec_VIDEO_CODEC_RAW, sampleID
	return f
}

func sfPumpAs(mode sharedMode, stamp string) func(*sharePump) {
	return func(p *sharePump) { p.mode, p.stamp = mode, stamp }
}

func (h *sharePumpHarness) noWriteWithin(t *testing.T, d time.Duration) {
	t.Helper()
	select {
	case e := <-h.node.events:
		t.Fatalf("a frame was written to the node: %+v", e)
	case <-time.After(d):
	}
}

func TestShareDecision_EachFormatDeclaresItsOwnModeAtOneSizeAndRate(t *testing.T) {
	s := shareTestService(t, []uint32{v4l2PixFmtYUYV, v4l2PixFmtMJPEG}, brioSizes(), []uint32{5, 15, 30})
	want := map[string]sharedMode{
		appconfig.CameraFormatPixels: {1280, 720, "YUYV", 2560, 30, "pixels", 0},
		appconfig.CameraFormatNative: sfNativeMode,
		appconfig.CameraFormatH264:   sfH264Mode,
	}
	for _, format := range appconfig.CameraFormats {
		d := s.shareDecisionFor(context.Background(), usbCam(), format)
		if !d.Shared {
			t.Fatalf("%s: not shared: %s", format, d.Reason)
		}
		if d.Mode != want[format] {
			t.Errorf("%s: Mode = %+v, want %+v", format, d.Mode, want[format])
		}
		if d.Mode.pixelFormat() == 0 || d.Mode.bufferBytes() <= 0 {
			t.Errorf("%s: no node can declare %+v", format, d.Mode)
		}
		pixels := want[appconfig.CameraFormatPixels]
		if d.Mode.Width != pixels.Width || d.Mode.Height != pixels.Height || d.Mode.FPS != pixels.FPS {
			t.Errorf("%s: %s differs in size or rate from the picture mode %s", format, d.Mode, pixels)
		}
	}
	if got := map[string]uint32{"MJPG": sfNativeMode.pixelFormat(), "H264": sfH264Mode.pixelFormat()}; got["MJPG"] != v4l2PixFmtMJPEG || got["H264"] != v4l2PixFmtH264 {
		t.Errorf("pixel formats = %#x", got)
	}
	if got := describeShareNode(sfNativeMode, appconfig.CameraStampFrame); got != "1280x720 MJPG, 30 fps, format native, stamp frame" {
		t.Errorf("describeShareNode = %q", got)
	}
}

func TestShareDecision_CompressedFormatsNeedTheCamerasOwnFramesWhole(t *testing.T) {
	s := shareTestService(t, []uint32{v4l2PixFmtUYVY}, map[uint32][][2]uint32{v4l2PixFmtUYVY: {{640, 480}}}, nil)
	if d := s.shareDecisionFor(context.Background(), usbCam(), appconfig.CameraFormatPixels); !d.Shared {
		t.Fatalf("pixels: not shared: %s", d.Reason)
	}
	for _, format := range []string{appconfig.CameraFormatNative, appconfig.CameraFormatH264} {
		d := s.shareDecisionFor(context.Background(), usbCam(), format)
		want := fmt.Sprintf("no shared mode for format %q: the camera's own frames are not captured whole: the camera offers no JPEG at 640x480", format)
		if d.Shared || d.Reason != want {
			t.Errorf("%s: decision = %+v, want the refusal %q", format, d, want)
		}
	}
}

func TestShareDecision_ACameraNoFormatSharesGivesItsReasonForEveryFormat(t *testing.T) {
	s := shareTestService(t, []uint32{v4l2PixFmtYUYV, v4l2PixFmtMJPEG}, brioSizes(), []uint32{30})
	dev := usbCam()
	dev.Transport = agentpb.VideoTransport_VIDEO_TRANSPORT_CSI
	for format, d := range s.shareDecisionsFor(dev, appconfig.CameraFormats...) {
		if d.Shared || d.Reason != "CSI cameras are not shared yet" {
			t.Errorf("%s: decision = %+v", format, d)
		}
	}
}

func TestCreateCameraShares_EachCamerasNodeFollowsItsEntitlement(t *testing.T) {
	h := newCSHarness(t, csBrio(), csC920(), csCam6())
	fp := installFakeProducers(h.s)
	h.s.CreateCameraShares(context.Background(), "cv-a", "sh.wendy.cv-a", []appconfig.Entitlement{
		sfCameraEntitlement(appconfig.CameraFormatNative, appconfig.CameraStampFrame, "/dev/video0"),
		sfCameraEntitlement(appconfig.CameraFormatH264, "", "C920"),
		sfCameraEntitlement("", "", "Cam Six"),
	})

	type node struct {
		mode  sharedMode
		stamp string
		kind  frameKind
	}
	want := map[string]node{
		"Brio 101": {sfNativeMode, appconfig.CameraStampFrame, nativeFrames},
		"C920":     {sfH264Mode, appconfig.CameraStampTime, encodedFrames},
		"Cam Six":  {sharedMode{1280, 720, "YUYV", 2560, 30, "pixels", 0}, appconfig.CameraStampTime, pixelFrames},
	}
	records := h.records("cv-a")
	if len(records) != len(want) {
		t.Fatalf("records = %d, want %d; logs: %v", len(records), len(want), h.logs.All())
	}
	opened := map[string]sharedMode{}
	for _, n := range h.openedNodes() {
		opened[n.path] = n.mode
	}
	for i, r := range records {
		w := want[r.cameraName]
		if r.mode != w.mode || r.stamp != w.stamp {
			t.Errorf("%s: record holds %s, want %s", r.cameraName, describeShareNode(r.mode, r.stamp), describeShareNode(w.mode, w.stamp))
		}
		if got := opened[shareNodePath(r.node)]; got != w.mode {
			t.Errorf("%s: node declares %+v, want %+v", r.cameraName, got, w.mode)
		}
		if r.pump.mode != w.mode || r.pump.stamp != w.stamp {
			t.Errorf("%s: pump serves %s", r.cameraName, describeShareNode(r.pump.mode, r.pump.stamp))
		}

		r.io.(*csNode).send(t, 1)
		if !csWaitFor(2*time.Second, func() bool { return fp.count() == i+1 }) {
			t.Fatalf("%s: its camera did not start; logs: %v", r.cameraName, h.logs.All())
		}
		hub := fp.hub(i)
		hub.mu.Lock()
		var kinds []frameKind
		for _, sub := range hub.subs {
			kinds = append(kinds, sub.kind)
		}
		hub.mu.Unlock()
		if !reflect.DeepEqual(kinds, []frameKind{w.kind}) {
			t.Errorf("%s: hub subscriptions = %v, want one of kind %v", r.cameraName, kinds, w.kind)
		}
	}
	if n := h.logged("camera share: cv-a shares Brio 101 (/dev/video0) through /dev/video255 at 1280x720 MJPG, 30 fps, format native, stamp frame"); n != 1 {
		t.Errorf("shares line with the node's settings logged %d times; logs: %v", n, h.logs.All())
	}
	if got, want := h.reportOf("/dev/video0")["share_state"], "cv-a [format native, stamp frame] (reading)"; got != want {
		t.Errorf("share_state = %q, want %q", got, want)
	}

	shares := h.stateShares("cv-a")
	if len(shares) != 3 {
		t.Fatalf("state file shares = %v", shares)
	}
	for _, sh := range shares {
		w := want[sh["camera"].(map[string]any)["name"].(string)]
		mode := sh["mode"].(map[string]any)
		if sh["format"] != w.mode.Format || sh["stamp"] != w.stamp || mode["fourcc"] != w.mode.Fourcc {
			t.Errorf("state file share = %v, want %s", sh, describeShareNode(w.mode, w.stamp))
		}
		if max, compressed := mode["maxFrameBytes"]; compressed != w.mode.compressed() || (compressed && max != float64(maxFrameBytes)) {
			t.Errorf("state file mode = %v, want maxFrameBytes %d", mode, w.mode.MaxFrameBytes)
		}
	}
	if v := h.stateFile()["version"]; v != float64(2) {
		t.Errorf("state file version = %v, want 2", v)
	}
}

func TestCreateCameraShares_NoEntitlementSettingsIsPixelsWithCaptureTimes(t *testing.T) {
	for name, entitlements := range map[string][]appconfig.Entitlement{
		"no entitlements":      nil,
		"camera without keys":  {sfCameraEntitlement("", "")},
		"values not known yet": {sfCameraEntitlement("raw16", "sequence")},
	} {
		t.Run(name, func(t *testing.T) {
			h := newCSHarness(t, csBrio())
			h.s.CreateCameraShares(context.Background(), "cv-a", "sh.wendy.cv-a", entitlements)
			records := h.records("cv-a")
			if len(records) != 1 {
				t.Fatalf("records = %d; logs: %v", len(records), h.logs.All())
			}
			if r := records[0]; r.mode != (sharedMode{1280, 720, "YUYV", 2560, 30, "pixels", 0}) || r.stamp != appconfig.CameraStampTime {
				t.Fatalf("record holds %s", describeShareNode(r.mode, r.stamp))
			}
		})
	}
}

func TestCreateCameraShares_AFormatTheCameraCannotCarryGivesTheRealCamera(t *testing.T) {
	h := newCSHarness(t, csBrio())
	prev := deviceSupportsMJPEGSize
	deviceSupportsMJPEGSize = func(string, uint32, uint32) bool { return false }
	t.Cleanup(func() { deviceSupportsMJPEGSize = prev })
	h.s.CreateCameraShares(context.Background(), "cv-a", "sh.wendy.cv-a", []appconfig.Entitlement{sfCameraEntitlement(appconfig.CameraFormatNative, "")})

	want := `camera share: cv-a gets the real camera Brio 101 (/dev/video0): no shared mode for format "native": the camera's own frames are not captured whole: the camera offers no JPEG at 1280x720`
	if n := h.logged(want); n != 1 {
		t.Errorf("refusal logged %d times; logs: %v", n, h.logs.All())
	}
	if len(h.records("cv-a")) != 0 || len(h.openedNodes()) != 0 {
		t.Error("a node was made for a format the camera cannot carry")
	}
}

func TestCameraShareState_Version1LoadsAsPixelsWithCaptureTimes(t *testing.T) {
	h := newCSHarness(t, csBrio())
	stored := sharedMode{Width: 640, Height: 480, Fourcc: "YUYV", BytesPerLine: 1280, FPS: 15}
	h.writeState(`{"version":1,"containers":{"cv-a":{"app":"sh.wendy.cv-a","shares":[` +
		csStateRecord(csBrio(), "/dev/video0", stored, 254) + `]}}}`)
	h.s.loadCameraShares()

	records := h.records("cv-a")
	if len(records) != 1 {
		t.Fatalf("records = %d; logs: %v", len(records), h.logs.All())
	}
	stored.Format = appconfig.CameraFormatPixels
	if r := records[0]; r.mode != stored || r.stamp != appconfig.CameraStampTime {
		t.Fatalf("record holds %s, want %s", describeShareNode(r.mode, r.stamp), describeShareNode(stored, appconfig.CameraStampTime))
	}
}

func TestCameraShareState_Version2KeepsEachSharesSettingsAcrossAnAgentRestart(t *testing.T) {
	h := newCSHarness(t, csBrio(), csC920())
	h.s.CreateCameraShares(context.Background(), "cv-a", "sh.wendy.cv-a", []appconfig.Entitlement{
		sfCameraEntitlement(appconfig.CameraFormatNative, appconfig.CameraStampFrame, "Brio 101"),
		sfCameraEntitlement(appconfig.CameraFormatH264, appconfig.CameraStampFrame, "C920"),
	})
	type settings struct {
		mode  sharedMode
		stamp string
	}
	before := map[string]settings{}
	for _, r := range h.records("cv-a") {
		before[r.cameraName] = settings{r.mode, r.stamp}
	}
	if want := (map[string]settings{"Brio 101": {sfNativeMode, "frame"}, "C920": {sfH264Mode, "frame"}}); !reflect.DeepEqual(before, want) {
		t.Fatalf("created %+v, want %+v", before, want)
	}

	// The agent restarts: its records come from the file, and the nodes are
	// still there.
	state, err := os.ReadFile(cameraSharesPath)
	if err != nil {
		t.Fatal(err)
	}
	nodes := map[int]string{}
	for _, r := range h.records("cv-a") {
		nodes[r.node] = r.cameraName
	}
	h.s.Shutdown()
	h = newCSHarness(t, csBrio(), csC920())
	fp := installFakeProducers(h.s)
	for nr, label := range nodes {
		h.loop.aux[nr] = label
	}
	h.writeState(string(state))
	h.s.loadCameraShares()
	after := map[string]settings{}
	for _, r := range h.records("cv-a") {
		after[r.cameraName] = settings{r.mode, r.stamp}
	}
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("loaded %+v, want %+v", after, before)
	}

	// Adoption opens each node at its stored mode and serves it as stored,
	// whatever a share made today would get.
	h.s.SyncCameraShares(context.Background(), []CameraShareContainer{{ID: "cv-a"}})
	for _, r := range h.records("cv-a") {
		want := before[r.cameraName]
		if r.pump == nil {
			t.Fatalf("%s was not adopted; logs: %v", r.cameraName, h.logs.All())
		}
		if r.pump.mode != want.mode || r.pump.stamp != want.stamp {
			t.Errorf("%s: adopted pump serves %s", r.cameraName, describeShareNode(r.pump.mode, r.pump.stamp))
		}
		if got := r.io.(*csNode).mode; got != want.mode {
			t.Errorf("%s: adopted node opened at %+v, want %+v", r.cameraName, got, want.mode)
		}
	}
	if n := h.logged("adopted /dev/video255 for Brio 101 at 1280x720 MJPG, 30 fps, format native, stamp frame"); n != 1 {
		t.Errorf("adoption line with the node's settings logged %d times; logs: %v", n, h.logs.All())
	}
	for _, r := range h.records("cv-a") {
		if r.cameraName != "C920" {
			continue
		}
		r.io.(*csNode).send(t, 1)
		if !csWaitFor(2*time.Second, func() bool { return fp.count() == 1 }) {
			t.Fatal("the adopted h264 node's camera did not start")
		}
		fp.hub(0).broadcast(sfH264Frame(41, true))
		if e := r.io.(*csNode).next(t); e.stamp != stampWithSampleID(41) {
			t.Errorf("adopted h264 node's buffer stamp = %+v, want sample ID 41", e.stamp)
		}
	}
}

func TestCameraShareState_AShareWithSettingsThisAgentDoesNotKnowIsDropped(t *testing.T) {
	share := func(format, stamp, fourcc string, maxFrameBytes int) string {
		return fmt.Sprintf(`{"camera":{"device":%q,"index":"0","name":"Brio 101"},"path":"/dev/video0",`+
			`"mode":{"width":1280,"height":720,"fourcc":%q,"bytesPerLine":0,"fps":30,"maxFrameBytes":%d},"format":%q,"stamp":%q,"node":254}`,
			csPort("1-2.1"), fourcc, maxFrameBytes, format, stamp)
	}
	for name, tc := range map[string]struct {
		record string
		kept   bool
	}{
		"native":                {share("native", "frame", "MJPG", maxFrameBytes), true},
		"unknown format":        {share("raw16", "time", "MJPG", maxFrameBytes), false},
		"unknown stamp":         {share("native", "sequence", "MJPG", maxFrameBytes), false},
		"no settings":           {share("", "", "MJPG", maxFrameBytes), false},
		"compressed, no limit":  {share("native", "time", "MJPG", 0), false},
		"codec no node carries": {share("native", "time", "VP80", maxFrameBytes), false},
	} {
		t.Run(name, func(t *testing.T) {
			h := newCSHarness(t, csBrio())
			h.loop.aux[254] = "Brio 101"
			h.writeState(`{"version":2,"containers":{"cv-a":{"app":"sh.wendy.cv-a","shares":[` + tc.record + `]}}}`)
			h.s.loadCameraShares()
			if got := len(h.records("cv-a")) == 1; got != tc.kept {
				t.Fatalf("kept = %v, want %v; logs: %v", got, tc.kept, h.logs.All())
			}
			if !tc.kept && h.logged("this agent could not have made it") != 1 {
				t.Errorf("the dropped share was not logged: %v", h.logs.All())
			}
		})
	}
}

func TestCameraShares_AppsWithDifferentFormatsOnOneCameraAllGetFrames(t *testing.T) {
	h := newCSHarness(t, csBrio())
	fp := installFakeProducers(h.s)
	apps := []struct{ id, format, stamp string }{
		{"cv-a", appconfig.CameraFormatPixels, appconfig.CameraStampTime},
		{"cv-b", appconfig.CameraFormatNative, appconfig.CameraStampFrame},
		{"cv-c", appconfig.CameraFormatH264, appconfig.CameraStampFrame},
	}
	nodes := map[string]*csNode{}
	for _, app := range apps {
		h.s.CreateCameraShares(context.Background(), app.id, "sh.wendy."+app.id, []appconfig.Entitlement{sfCameraEntitlement(app.format, app.stamp)})
		records := h.records(app.id)
		if len(records) != 1 || records[0].mode.Format != app.format {
			t.Fatalf("%s: records = %+v; logs: %v", app.id, records, h.logs.All())
		}
		nodes[app.id] = records[0].io.(*csNode)
		nodes[app.id].send(t, 1)
	}
	if !csWaitFor(2*time.Second, func() bool {
		if fp.count() != 1 {
			return false
		}
		hub := fp.hub(0)
		hub.mu.Lock()
		defer hub.mu.Unlock()
		return len(hub.subs) == len(apps)
	}) {
		t.Fatalf("the apps do not share one capture: %d producers; logs: %v", fp.count(), h.logs.All())
	}
	if got := hubParams(fp.hub(0)); got != [3]uint32{} {
		t.Fatalf("the camera runs at %v, want its defaults", got)
	}

	// One camera frame, as the hub delivers it on each plane.
	hub := fp.hub(0)
	jpeg, picture, accessUnit := sfJPEGFrame(7, 9_000_000_000), sfPictureFrame(7), sfH264Frame(7, true)
	for _, f := range []*videoFrame{jpeg, picture, accessUnit} {
		hub.broadcast(f)
	}
	if e := nodes["cv-a"].next(t); e.size != len(picture.data) || e.stamp.holdsSampleID {
		t.Errorf("pixels node got %+v, want the %d-byte picture with its capture time", e, len(picture.data))
	}
	if e := nodes["cv-b"].next(t); e.size != len(jpeg.data) || e.stamp != stampWithSampleID(7) {
		t.Errorf("native node got %+v, want the %d-byte JPEG stamped with sample ID 7", e, len(jpeg.data))
	}
	if e := nodes["cv-c"].next(t); e.size != len(accessUnit.data) || e.stamp != stampWithSampleID(7) {
		t.Errorf("h264 node got %+v, want the %d-byte access unit stamped with sample ID 7", e, len(accessUnit.data))
	}

	// The camera's one shared mode, for viewers and recordings, is its size
	// and rate whatever each app's format.
	mode, sharers, ok := h.s.sharedModeOf("/dev/video0")
	if !ok || !reflect.DeepEqual(sharers, []string{"cv-a", "cv-b", "cv-c"}) {
		t.Fatalf("sharedModeOf = %v, %v", sharers, ok)
	}
	if _, err := h.s.applySharedMode("/dev/video0", &agentpb.StreamVideoRequest{Width: mode.Width, Height: mode.Height, Framerate: mode.FPS}); err != nil {
		t.Errorf("a viewer asking the shared size and rate was refused: %v", err)
	}
	if _, err := h.s.applySharedMode("/dev/video0", &agentpb.StreamVideoRequest{Width: 1920, Height: 1080}); err == nil {
		t.Error("a viewer asking another size was admitted")
	}
	want := "cv-a [format pixels, stamp time] (reading), cv-b [format native, stamp frame] (reading), cv-c [format h264, stamp frame] (reading)"
	if got := h.reportOf("/dev/video0")["share_state"]; got != want {
		t.Errorf("share_state = %q\nwant         %q", got, want)
	}
}

func TestJoinHubForShare_TakesAViewersCameraOverForEveryNodeFormat(t *testing.T) {
	for _, tc := range []struct {
		mode   sharedMode
		own    *videoFrame
		others []*videoFrame
	}{
		{sfNativeMode, sfJPEGFrame(1, 1), []*videoFrame{sfPictureFrame(1), sfH264Frame(1, true)}},
		{sfH264Mode, sfH264Frame(1, true), []*videoFrame{sfPictureFrame(1), sfJPEGFrame(1, 1)}},
		{spMode, sfPictureFrame(1), []*videoFrame{sfJPEGFrame(1, 1), sfH264Frame(1, true)}},
	} {
		t.Run(tc.mode.Format, func(t *testing.T) {
			svc := newTestVideoService(nil, nil)
			fp := installFakeProducers(svc)
			ctx := context.Background()
			viewerReq := &agentpb.StreamVideoRequest{DeviceId: 0, Width: 640, Height: 480}
			oldHub, viewerID, _, err := svc.getOrCreateHub(ctx, "/dev/video0", viewerReq, hubHolderStreamClient)
			if err != nil {
				t.Fatal(err)
			}
			defer oldHub.unsubscribe(viewerID)

			share, err := svc.joinHubForShare(ctx, "/dev/video0", 0, tc.mode)
			if err != nil {
				t.Fatalf("share join: %v", err)
			}
			defer share.Leave()
			newHub := currentHub(svc, "/dev/video0")
			if newHub == nil || newHub == oldHub || fp.count() != 2 || hubParams(newHub) != [3]uint32{} {
				t.Fatalf("the camera was not restarted at its defaults: %d producers", fp.count())
			}
			for _, f := range append(tc.others, tc.own) {
				newHub.broadcast(f)
			}
			select {
			case f := <-share.Frames():
				if f != tc.own {
					t.Fatalf("the share received a %s frame, want only its format's", describeNodeFrame(f))
				}
			case <-time.After(2 * time.Second):
				t.Fatal("the share received no frame of its format")
			}
		})
	}
}

func TestSharePump_TimeStampIsTheKernelCaptureTimeWhenTheFrameHasOne(t *testing.T) {
	const nowMono, nowWall = int64(50_000_000_000), int64(1_700_000_000_000_000_000)
	clocks := func(p *sharePump) {
		p.nowMono = func() int64 { return nowMono }
		p.nowWall = func() int64 { return nowWall }
	}
	sub := newSpFakeSub()
	h := startSharePump(t, newSpJoiner(spSubResult(sub)), clocks)
	h.node.send(t, 1)

	onKernelClock := spModeFrame(uint64(nowWall - 40_000_000))
	onKernelClock.nativeNs, onKernelClock.nativeClock = 49_990_000_000, v4l2MonotonicClock
	sub.frames <- onKernelClock
	if e := h.node.next(t); e.stamp != stampWithCaptureTime(49_990_000_000) {
		t.Errorf("stamp = %+v, want the kernel capture time", e.stamp)
	}

	for name, f := range map[string]func(*videoFrame){
		"agent wall clock": func(f *videoFrame) { f.nativeNs, f.nativeClock = nowWall-40_000_000, "CLOCK_REALTIME_AGENT_CAPTURE" },
		"unknown clock":    func(f *videoFrame) { f.nativeNs, f.nativeClock = 123, "V4L2_TIMESTAMP_UNKNOWN" },
		"no kernel time":   func(f *videoFrame) { f.nativeClock = v4l2MonotonicClock },
	} {
		frame := spModeFrame(uint64(nowWall - 40_000_000))
		f(frame)
		sub.frames <- frame
		if e := h.node.next(t); e.stamp != stampWithCaptureTime(nowMono-40_000_000) {
			t.Errorf("%s: stamp = %+v, want the wall-clock time rebased", name, e.stamp)
		}
	}
}

func TestSharePump_FrameStampHoldsTheSampleIDAndNeverWritesAFrameWithoutOne(t *testing.T) {
	without, with := newSpFakeSub(), newSpFakeSub()
	j := newSpJoiner(spSubResult(without), spSubResult(with))
	h := startSharePump(t, j, sfPumpAs(spMode, appconfig.CameraStampFrame))

	h.node.send(t, 1)
	j.waitJoin(t)
	without.frames <- sfPictureFrame(0)
	spWaitClosed(t, without.left, "the pump to leave a capture whose frames have no sample ID")
	if s := h.waitState(t, shareNoFrames); s.Detail != `the camera's frames carry no frame number for the "frame" stamp` {
		t.Fatalf("Detail = %q", s.Detail)
	}
	if n := h.logged(`camera share: app1: cam0 cannot serve its node (format pixels, stamp frame): the camera's frames carry no frame number for the "frame" stamp; not writing its frames, retrying in`); n != 1 {
		t.Errorf("reason logged %d times; logs: %v", n, h.logs.All())
	}

	// The camera is captured another way by the next join.
	j.waitJoin(t)
	if s := h.pump.State(); s.Phase != shareNoFrames {
		t.Errorf("state before a frame is written = %+v, want it to stay no frames", s)
	}
	with.frames <- sfPictureFrame(1234567890123)
	e := h.node.next(t)
	if e.stamp != stampWithSampleID(1234567890123) {
		t.Errorf("stamp = %+v, want sample ID 1234567890123", e.stamp)
	}
	h.waitState(t, shareReading)
	if all := h.node.all(); len(all) != 1 {
		t.Errorf("node writes = %+v, want only the frame with a sample ID", all)
	}
}

func TestSharePump_H264StartsOnARandomAccessFrameAndWritesWholeAccessUnits(t *testing.T) {
	sub := newSpFakeSub()
	h := startSharePump(t, newSpJoiner(spSubResult(sub)), sfPumpAs(sfH264Mode, appconfig.CameraStampFrame))
	h.node.send(t, 1)

	sub.frames <- sfH264Frame(1, false)
	sub.frames <- sfH264Frame(2, false)
	idr, inter := sfH264Frame(3, true), sfH264Frame(4, false)
	sub.frames <- idr
	sub.frames <- inter
	if e := h.node.next(t); e.stamp != stampWithSampleID(3) || e.size != len(idr.data) {
		t.Fatalf("first write = %+v, want the random-access frame 3 whole", e)
	}
	if e := h.node.next(t); e.stamp != stampWithSampleID(4) || e.size != len(inter.data) {
		t.Fatalf("second write = %+v, want frame 4 whole", e)
	}
	h.waitState(t, shareReading)

	// A frame that does not reach the app breaks the chain: the next written
	// is again one the app can decode alone.
	h.node.setWriteErr(fmt.Errorf("share node: %w", errShareFrameTooLarge))
	sub.frames <- sfH264Frame(5, false)
	if s := h.waitState(t, shareNoFrames); !strings.Contains(s.Detail, errShareFrameTooLarge.Error()) {
		t.Fatalf("Detail = %q, want the dropped frame's reason", s.Detail)
	}
	h.node.setWriteErr(nil)
	sub.frames <- sfH264Frame(6, false)
	sub.frames <- sfH264Frame(7, true)
	if e := h.node.next(t); e.stamp != stampWithSampleID(7) {
		t.Fatalf("write after a dropped frame = %+v, want the next random-access frame, 7", e)
	}
	h.waitState(t, shareReading)
}

func TestSharePump_H264ThatIsNotWholeAccessUnitsIsNeverWritten(t *testing.T) {
	chunked, whole := newSpFakeSub(), newSpFakeSub()
	j := newSpJoiner(spSubResult(chunked), spSubResult(whole))
	h := startSharePump(t, j, sfPumpAs(sfH264Mode, appconfig.CameraStampTime))

	h.node.send(t, 1)
	j.waitJoin(t)
	chunk := sfH264Frame(1, true)
	chunk.auAligned = false
	chunked.frames <- chunk
	spWaitClosed(t, chunked.left, "the pump to leave a capture whose H.264 is a byte stream")
	want := "the camera's H.264 is not arriving as whole frames, so there is none to put in a buffer"
	if s := h.waitState(t, shareNoFrames); s.Detail != want {
		t.Fatalf("Detail = %q, want %q", s.Detail, want)
	}

	// The camera is back on the path that makes whole access units.
	j.waitJoin(t)
	whole.frames <- sfH264Frame(2, true)
	if e := h.node.next(t); e.size != len(sfH264Frame(2, true).data) || e.stamp.holdsSampleID {
		t.Fatalf("write = %+v, want the whole access unit with its capture time", e)
	}
	h.waitState(t, shareReading)
	if all := h.node.all(); len(all) != 1 {
		t.Errorf("node writes = %+v, want only the whole access unit", all)
	}
}

func TestSharePump_NativeRefusedByTheHubSaysWhyAndRecovers(t *testing.T) {
	refusal := errNativeUnavailable("JPEG: the camera offers no JPEG at 1280x720")
	refusedLater, served := newSpFakeSub(), newSpFakeSub()
	refusedLater.err = refusal
	close(refusedLater.frames)
	j := newSpJoiner(spErrResult(refusal), spSubResult(refusedLater), spSubResult(served))
	h := startSharePump(t, j, sfPumpAs(sfNativeMode, appconfig.CameraStampTime))

	h.node.send(t, 1)
	want := "native frames are not available for this camera: JPEG: the camera offers no JPEG at 1280x720"
	for join, how := range []string{"refused at the join", "turned away by the producer"} {
		j.waitJoin(t)
		if !csWaitFor(spWait, func() bool { return h.pump.State() == shareState{shareNoFrames, want} }) {
			t.Fatalf("join %d (%s): state = %+v, want no frames with %q", join+1, how, h.pump.State(), want)
		}
	}
	h.noWriteWithin(t, 20*time.Millisecond)

	j.waitJoin(t)
	served.frames <- sfJPEGFrame(9, 77_000_000_000)
	if e := h.node.next(t); e.stamp != stampWithCaptureTime(77_000_000_000) || e.size != len(sfJPEGFrame(9, 0).data) {
		t.Fatalf("write = %+v, want the camera's own frame at its kernel capture time", e)
	}
	h.waitState(t, shareReading)
}

func TestSharePump_FramesOfAnotherCodecThanTheNodeDeclaresAreNotWritten(t *testing.T) {
	sub := newSpFakeSub()
	h := startSharePump(t, newSpJoiner(spSubResult(sub)), sfPumpAs(sfNativeMode, appconfig.CameraStampTime))
	h.node.send(t, 1)

	ownH264 := sfH264Frame(1, true)
	ownH264.native = true
	sub.frames <- ownH264
	want := "camera frames are H.264, the node declares 1280x720 MJPG, 30 fps"
	if s := h.waitState(t, shareNoFrames); s.Detail != want {
		t.Fatalf("Detail = %q, want %q", s.Detail, want)
	}
	sub.frames <- sfPictureFrame(2)
	h.noWriteWithin(t, 20*time.Millisecond)
	sub.frames <- sfJPEGFrame(3, 1)
	h.node.next(t)
	h.waitState(t, shareReading)
}

// sfWriter is a node writer over memory, with its queue ioctl recorded.
func sfWriter(mode sharedMode) (w *shareNodeWriter, queued *[]v4l2Buf) {
	queued = &[]v4l2Buf{}
	w = &shareNodeWriter{mode: mode, path: "/dev/video255", fd: -1, wakeR: -1, wakeW: -1, done: closedChan(),
		buffers: [][]byte{make([]byte, mode.bufferBytes()), make([]byte, mode.bufferBytes())}}
	w.ioctlFn = func(req uintptr, arg unsafe.Pointer) error {
		if req == vidiocQbuf {
			*queued = append(*queued, *(*v4l2Buf)(arg))
		}
		return nil
	}
	return w, queued
}

func sfBufferTimeval(b *v4l2Buf) (seconds, microseconds int64) {
	return *(*int64)(unsafe.Pointer(&b[24])), *(*int64)(unsafe.Pointer(&b[32]))
}

func TestShareNodeWriter_DeclaresACompressedModeByItsMaximumFrameSize(t *testing.T) {
	stopAtBuffers := errors.New("stop before the buffers")
	for name, tc := range map[string]struct {
		mode      sharedMode
		taken     func(*v4l2Format)
		wantError string
	}{
		"jpeg":                           {mode: sfNativeMode},
		"h264":                           {mode: sfH264Mode},
		"driver sizes the image larger":  {mode: sfNativeMode, taken: func(f *v4l2Format) { f.SizeImage = 1280 * 720 * 4 }},
		"driver sizes the image smaller": {mode: sfNativeMode, taken: func(f *v4l2Format) { f.SizeImage = maxFrameBytes - 1 }, wantError: "instead of 1280x720 MJPG, 30 fps"},
		"driver takes another format":    {mode: sfH264Mode, taken: func(f *v4l2Format) { f.PixelFormat = v4l2PixFmtMJPEG }, wantError: "instead of 1280x720 H264, 30 fps"},
	} {
		t.Run(name, func(t *testing.T) {
			var asked v4l2Format
			w := &shareNodeWriter{mode: tc.mode, path: "/dev/video255", fd: -1, wakeR: -1, wakeW: -1}
			w.ioctlFn = func(req uintptr, arg unsafe.Pointer) error {
				switch req {
				case vidiocSFmt:
					asked = *(*v4l2Format)(arg)
					if tc.taken != nil {
						tc.taken((*v4l2Format)(arg))
					}
				case vidiocReqbufs:
					return stopAtBuffers
				}
				return nil
			}
			err := w.configure()
			if asked.Type != v4l2BufTypeVideoOutput || asked.Width != 1280 || asked.Height != 720 ||
				asked.PixelFormat != tc.mode.pixelFormat() || asked.BytesPerLine != 0 || asked.SizeImage != maxFrameBytes {
				t.Errorf("declared %dx%d %s, %d bytes per line, %d bytes; want 1280x720 %s, 0, %d",
					asked.Width, asked.Height, fourccName(asked.PixelFormat), asked.BytesPerLine, asked.SizeImage, tc.mode.Fourcc, maxFrameBytes)
			}
			switch {
			case tc.wantError == "" && !errors.Is(err, stopAtBuffers):
				t.Errorf("the mode the node took was refused: %v", err)
			case tc.wantError != "" && (err == nil || !strings.Contains(err.Error(), tc.wantError)):
				t.Errorf("error = %v, want one saying %q", err, tc.wantError)
			}
		})
	}
}

func TestShareNodeWriter_CompressedFramesAreWrittenAtTheirOwnLength(t *testing.T) {
	mode := sharedMode{Width: 4, Height: 2, Fourcc: "MJPG", FPS: 30, Format: appconfig.CameraFormatNative, MaxFrameBytes: 16}
	w, queued := sfWriter(mode)

	for i, frame := range [][]byte{{1, 2, 3}, []byte("0123456789abcdef"), {9}} {
		if err := w.WriteFrame(frame, stampWithCaptureTime(int64(i+1)*1_000_000_000)); err != nil {
			t.Fatalf("frame of %d bytes: %v", len(frame), err)
		}
		q := (*queued)[i]
		if q.bytesUsed() != uint32(len(frame)) || q.index() != uint32(i%2) || string(w.buffers[i%2][:len(frame)]) != string(frame) {
			t.Errorf("frame %d queued as %d bytes in buffer %d, want its %d bytes in buffer %d", i, q.bytesUsed(), q.index(), len(frame), i%2)
		}
	}

	for dropped := uint64(1); dropped <= 2; dropped++ {
		err := w.WriteFrame(make([]byte, 17), stampWithCaptureTime(1))
		if !errors.Is(err, errShareFrameTooLarge) || w.oversizedFrames() != dropped {
			t.Fatalf("oversize frame: error %v, counted %d, want errShareFrameTooLarge and %d", err, w.oversizedFrames(), dropped)
		}
	}
	if err := w.WriteFrame(nil, stampWithCaptureTime(1)); err == nil {
		t.Error("an empty frame was accepted")
	}
	if len(*queued) != 3 {
		t.Errorf("%d buffers queued, want only the 3 frames that fit", len(*queued))
	}
	if err := w.WriteFrame([]byte{7}, stampWithCaptureTime(1)); err != nil {
		t.Errorf("a frame that fits after dropped ones: %v", err)
	}
}

func TestShareNodeWriter_PicturesStillNeedTheirExactSize(t *testing.T) {
	mode := sharedMode{Width: 4, Height: 2, Fourcc: "YUYV", BytesPerLine: 8, FPS: 30, Format: appconfig.CameraFormatPixels}
	w, queued := sfWriter(mode)
	for _, size := range []int{0, 15, 17} {
		if err := w.WriteFrame(make([]byte, size), stampWithCaptureTime(1)); err == nil || errors.Is(err, errShareFrameTooLarge) {
			t.Errorf("a %d-byte picture: error %v, want the exact-size refusal", size, err)
		}
	}
	if err := w.WriteFrame(make([]byte, 16), stampWithCaptureTime(1)); err != nil {
		t.Fatal(err)
	}
	if len(*queued) != 1 || (*queued)[0].bytesUsed() != 16 || w.oversizedFrames() != 0 {
		t.Errorf("queued %d buffers, counted %d oversize", len(*queued), w.oversizedFrames())
	}
}

func TestShareNodeWriter_BufferTimestampHoldsTheCaptureTimeOrTheSampleID(t *testing.T) {
	mode := sharedMode{Width: 4, Height: 2, Fourcc: "MJPG", FPS: 30, Format: appconfig.CameraFormatNative, MaxFrameBytes: 16}
	w, queued := sfWriter(mode)

	if err := w.WriteFrame([]byte{1}, stampWithCaptureTime(12_345_678_901_234)); err != nil {
		t.Fatal(err)
	}
	if sec, usec := sfBufferTimeval(&(*queued)[0]); sec != 12_345 || usec != 678_901 {
		t.Errorf("capture time stamped as %d s %d µs, want 12345 s 678901 µs", sec, usec)
	}

	for _, id := range []uint64{1, 999_999, 1_000_000, 1234567890123, 1<<63 + 5} {
		if err := w.WriteFrame([]byte{1}, stampWithSampleID(id)); err != nil {
			t.Fatalf("sample ID %d: %v", id, err)
		}
		last := (*queued)[len(*queued)-1]
		sec, usec := sfBufferTimeval(&last)
		if uint64(sec) != id/1_000_000 || uint64(usec) != id%1_000_000 || uint64(sec)*1_000_000+uint64(usec) != id {
			t.Errorf("sample ID %d stamped as %d s %d µs", id, sec, usec)
		}
		if last.flags()&v4l2BufFlagTimestampCopy == 0 {
			t.Errorf("sample ID %d: the buffer does not say its timestamp is the writer's", id)
		}
	}

	before := len(*queued)
	if err := w.WriteFrame([]byte{1}, stampWithSampleID(0)); !errors.Is(err, errShareFrameWithoutSampleID) {
		t.Errorf("sample ID 0: error %v, want errShareFrameWithoutSampleID", err)
	}
	if len(*queued) != before {
		t.Error("a frame with sample ID 0 was queued")
	}
	var viaTwoPlane, viaShare v4l2Buf
	setLoopbackSampleID(&viaTwoPlane, 1234567890123)
	if err := stampWithSampleID(1234567890123).writeTo(&viaShare); err != nil || viaShare != viaTwoPlane {
		t.Errorf("a share node's sample ID encoding differs from the two-plane writer's (%v)", err)
	}
}
