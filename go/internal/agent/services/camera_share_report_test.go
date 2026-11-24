package services

import (
	"context"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/camera"
	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

func TestCameraShareReport_NamesModeAppsAndStates(t *testing.T) {
	h := newCSHarness(t, csBrio())
	h.writeState(`{"version":1,"containers":{` +
		`"cv-b":{"app":"sh.wendy.cv-b","shares":[` + csStateRecord(csBrio(), "/dev/video0", testSharedMode, 253) + `]},` +
		`"cv-a":{"app":"sh.wendy.cv-a","shares":[` + csStateRecord(csBrio(), "/dev/video0", testSharedMode, 254) + `]},` +
		`"cv-c":{"app":"sh.wendy.cv-c","shares":[` + csStateRecord(csBrio(), "/dev/video0", testSharedMode, 252) + `]}}}`)
	h.s.loadCameraShares()
	h.s.shares.mu.Lock()
	h.s.shares.containers["cv-a"].shares[0].pump = &sharePump{state: shareState{Phase: shareReading}}
	h.s.shares.containers["cv-b"].shares[0].pump = &sharePump{state: shareState{Phase: shareWaiting, Detail: "held by cv-x at 1920x1080"}}
	h.s.shares.containers["cv-c"].shares[0].pump = &sharePump{state: shareState{Phase: shareIdle}}
	h.s.shares.containers["cv-c"].shares[0].tookRealCamera = "python3"
	h.s.shares.mu.Unlock()

	got := h.s.CameraShareReport(context.Background())["/dev/video0"]
	dev := &agentpb.VideoDevice{Path: "/dev/video0", Transport: agentpb.VideoTransport_VIDEO_TRANSPORT_USB}
	want := map[string]string{
		"share_mode":               h.s.shareDecisionFor(context.Background(), dev, appconfig.CameraFormatPixels).Mode.String(),
		"share_unavailable_native": sfCannotDerive,
		"share_unavailable_h264":   sfCannotDerive,
		"shared_with":              "cv-a, cv-b, cv-c",
		"stable_id":                "",
		"share_state":              "cv-a [format pixels, stamp time] (reading), cv-b [format pixels, stamp time] (waiting: held by cv-x at 1920x1080), cv-c [format pixels, stamp time] (not sharing: took the real camera after a replug)",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("report = %#v, want %#v", got, want)
	}
}

func TestCameraShareReport_UnsharedCameraSaysWhy(t *testing.T) {
	h := newCSHarness(t, csFormatlessCam())
	got := h.s.CameraShareReport(context.Background())["/dev/video2"]
	want := map[string]string{"share_unavailable": "the camera's formats could not be read", "stable_id": ""}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("report = %#v, want %#v", got, want)
	}
}

func TestCameraShareReport_UnsupportedFreshStartFallsBackToRealCamera(t *testing.T) {
	h := newCSHarness(t, csBrio())
	h.openErr["/dev/video255"] = errShareFreshStartUnsupported
	h.s.CreateCameraShares(context.Background(), "cv-a", "sh.wendy.cv-a", nil)
	h.drainRemovals()
	if len(h.records("cv-a")) != 0 || len(h.loop.liveAux()) != 0 {
		t.Fatal("unsupported module left a share node covering the real camera")
	}
	got := h.s.CameraShareReport(context.Background())["/dev/video0"]
	want := map[string]string{"share_unavailable": errShareFreshStartUnsupported.Error(), "stable_id": ""}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("report = %#v, want %#v", got, want)
	}
	delete(h.openErr, "/dev/video255")
	h.s.CreateCameraShares(context.Background(), "cv-b", "sh.wendy.cv-b", nil)
	got = h.s.CameraShareReport(context.Background())["/dev/video0"]
	if got["share_unavailable"] != "" || got["share_mode"] == "" {
		t.Fatalf("successful new module open kept old refusal: %#v", got)
	}
}

// Two resolves read the same old camera; the first follows it for every
// record, so the second finds its record already moved and is still resolved.
func TestResolveShareCamera_ConcurrentFollowStillResolves(t *testing.T) {
	h := newCSHarness(t, csBrio())
	moved := camera.Identity{Device: csPort("1-2.9"), Index: "0", Name: "Brio 101"}
	h.writeState(`{"version":1,"containers":{"cv-a":{"app":"sh.wendy.cv-a","shares":[` +
		csStateRecord(csBrio(), "/dev/video0", testSharedMode, 254) + `]}}}`)
	h.s.loadCameraShares()
	r := h.records("cv-a")[0]
	present := map[string]camera.Identity{"video0": moved}

	prev := afterShareResolveRead.Load()
	t.Cleanup(func() { afterShareResolveRead.Store(prev) })
	hook := func() {
		afterShareResolveRead.Store(prev)
		if base, ok, _ := h.s.resolveShareCamera(r, present); !ok || base != "video0" {
			t.Errorf("inner resolve = %q, %v", base, ok)
		}
	}
	afterShareResolveRead.Store(&hook)
	base, ok, _ := h.s.resolveShareCamera(r, present)
	if !ok || base != "video0" {
		t.Errorf("outer resolve, whose record the inner one moved = %q, %v; want video0, true", base, ok)
	}
	h.s.shares.mu.Lock()
	got := r.camera
	h.s.shares.mu.Unlock()
	if got != moved {
		t.Errorf("record follows %+v, want %+v", got, moved)
	}
}

// A decision that never arrives in time costs the camera its mode, not its
// sharers: the camera after the slow one still reports who shares it.
func TestCameraShareReport_SlowDecisionStillNamesSharers(t *testing.T) {
	h := newCSHarness(t, csBrio(), csC920())
	h.writeState(`{"version":1,"containers":{` +
		`"cv-a":{"app":"sh.wendy.cv-a","shares":[` +
		csStateRecord(csBrio(), "/dev/video0", testSharedMode, 254) + `,` +
		csStateRecord(csC920(), "/dev/video4", testSharedMode, 253) + `]}}}`)
	h.s.loadCameraShares()
	h.s.shares.mu.Lock()
	for _, r := range h.s.shares.containers["cv-a"].shares {
		r.pump = &sharePump{state: shareState{Phase: shareReading}}
	}
	h.s.shares.mu.Unlock()

	// The slow camera's decision blocks until the test ends, then gives up
	// at once (no formats), so it touches no package state the test's other
	// cleanups restore; the cleanup waits for it to return.
	var release sync.Mutex // held by the test until the cleanup
	release.Lock()
	var slow sync.WaitGroup
	slow.Add(1) // the one slow decision
	t.Cleanup(func() {
		release.Unlock()
		slow.Wait()
	})
	fast := h.s.capturePixelFormats
	h.s.capturePixelFormats = func(path string) ([]uint32, bool) {
		if path == "/dev/video0" {
			defer slow.Done()
			release.Lock()
			release.Unlock()
			return nil, false
		}
		return fast(path)
	}
	prev := cameraShareReportBudget
	cameraShareReportBudget = 300 * time.Millisecond
	t.Cleanup(func() { cameraShareReportBudget = prev })

	start := time.Now()
	got := h.s.CameraShareReport(context.Background())
	if took := time.Since(start); took > cameraShareReportBudget+500*time.Millisecond {
		t.Errorf("report took %s, budget %s", took, cameraShareReportBudget)
	}
	for _, path := range []string{"/dev/video0", "/dev/video4"} {
		props := got[path]
		if props["shared_with"] != "cv-a" || props["share_state"] != "cv-a [format pixels, stamp time] (reading)" {
			t.Errorf("%s: %#v, want cv-a sharing and reading", path, props)
		}
		if _, ok := props["share_mode"]; path == "/dev/video0" && ok {
			t.Errorf("%s has a mode though its decision never arrived: %#v", path, props)
		}
	}
}

// A row names the encoder of its camera's H.264 only where this device makes
// that H.264 in software.
func TestCameraShareReport_NamesTheSoftwareEncoderOfACamerasH264(t *testing.T) {
	const h264Mode = "1280x720 H264, 30 fps"
	for name, tc := range map[string]struct {
		camera   csCamera
		elements []string
		encoder  string
	}{
		"a webcam's JPEG frames and x264enc":                  {csC920(), []string{"jpegdec", "x264enc"}, "x264enc (software)"},
		"a webcam's JPEG frames and openh264enc":              {csC920(), []string{"jpegdec", "openh264enc"}, "openh264enc (software)"},
		"a camera that produces H.264":                        {csH264Cam(), []string{"avdec_h264", "jpegdec", "x264enc"}, ""},
		"a hardware encoder chosen before x264enc":            {csC920(), []string{"jpegdec", "v4l2h264enc", "x264enc"}, ""},
		"x264enc chosen before an installed vaapih264enc":     {csC920(), []string{"jpegdec", "x264enc", "vaapih264enc"}, "x264enc (software)"},
		"an encoder that is hardware or software as it likes": {csC920(), []string{"jpegdec", "vtenc_h264"}, ""},
	} {
		t.Run(name, func(t *testing.T) {
			h := newCSHarness(t, tc.camera)
			hcCanDecode(h.s, tc.elements...)
			row := h.reportOf("/dev/" + tc.camera.base)
			if row["share_mode_h264"] != h264Mode {
				t.Fatalf("the camera has no h264 node to speak of: %#v", row)
			}
			if got, said := row["h264_encoder"]; got != tc.encoder || said != (tc.encoder != "") {
				t.Errorf("h264_encoder = %q (present %v), want %q; row: %#v", got, said, tc.encoder, row)
			}
		})
	}

	t.Run("no encoder at all", func(t *testing.T) {
		h := newCSHarness(t, csC920())
		hcCanDecode(h.s, "jpegdec")
		row := h.reportOf("/dev/video4")
		if _, said := row["h264_encoder"]; said || row["share_mode_h264"] != "" || row["share_mode"] == "" {
			t.Errorf("a device without an H.264 encoder: row = %#v, want pictures shared and no h264_encoder", row)
		}
	})

	t.Run("the whole row", func(t *testing.T) {
		h := newCSHarness(t, csC920())
		sfCanDeriveFrames(h.s)
		want := map[string]string{
			"share_mode":        "1280x720 YUYV, 30 fps",
			"share_mode_native": "1280x720 MJPG, 30 fps",
			"share_mode_h264":   h264Mode,
			"h264_encoder":      "x264enc (software)",
			"stable_id":         "",
		}
		if got := h.reportOf("/dev/video4"); !reflect.DeepEqual(got, want) {
			t.Errorf("report = %#v\nwant     %#v", got, want)
		}
	})
}
