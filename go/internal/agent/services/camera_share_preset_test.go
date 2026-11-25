package services

import (
	"context"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

func spH264At(preset string, allowlist ...string) appconfig.Entitlement {
	e := sfCameraEntitlement(appconfig.CameraFormatH264, "", allowlist...)
	e.Preset = preset
	return e
}

// spRunningHub puts a hub at path whose producer reported its encoder runs at
// preset.
func spRunningHub(t *testing.T, s *VideoService, path string, preset appconfig.EncodePreset) *deviceHub {
	t.Helper()
	hub, cancel := newTestHub(t)
	t.Cleanup(cancel)
	hub.setEncoderPreset(preset)
	s.mu.Lock()
	s.hubs[path] = hub
	s.mu.Unlock()
	return hub
}

func spSetPhase(h *csHarness, containerID string, phase sharePhase) {
	h.s.shares.mu.Lock()
	defer h.s.shares.mu.Unlock()
	r := h.s.shares.containers[containerID].shares[0]
	r.pump.mu.Lock()
	r.pump.state = shareState{Phase: phase}
	r.pump.mu.Unlock()
}

const (
	spNotRunning = "; asked for preset %s; the camera's one encoder serves all its consumers and runs at %s"
	spOrFaster   = ", at preset %s or a faster one another consumer of the camera states"
)

// A camera's row says whether a preset stated for its h264 node is applied:
// under one key the preset the encoder runs at when no consumer states one,
// under the other why a stated one is ignored. A camera without an h264 mode
// has neither.
func TestCameraShareReport_SaysWhatBecomesOfAPresetOnACamerasH264Node(t *testing.T) {
	for name, tc := range map[string]struct {
		camera   csCamera
		elements []string
		fallback string
		ignored  string
	}{
		"a webcam's JPEG frames and x264enc":                  {csC920(), []string{"jpegdec", "x264enc"}, "ultrafast", ""},
		"x264enc chosen before an installed vaapih264enc":     {csC920(), []string{"jpegdec", "x264enc", "vaapih264enc"}, "ultrafast", ""},
		"a software encoder without a preset":                 {csC920(), []string{"jpegdec", "openh264enc"}, "", "this device encodes H.264 with openh264enc, which takes no preset"},
		"a hardware encoder chosen before x264enc":            {csC920(), []string{"jpegdec", "v4l2h264enc", "x264enc"}, "", "this device encodes H.264 with v4l2h264enc, which takes no preset"},
		"an encoder that is hardware or software as it likes": {csC920(), []string{"jpegdec", "vtenc_h264"}, "", "this device encodes H.264 with vtenc_h264, which takes no preset"},
		"a camera that produces H.264":                        {csH264Cam(), []string{"avdec_h264", "jpegdec", "x264enc"}, "", "the camera produces H.264 itself, so no encoder runs for its node"},
	} {
		t.Run(name, func(t *testing.T) {
			h := newCSHarness(t, tc.camera)
			hcCanDecode(h.s, tc.elements...)
			row := h.reportOf("/dev/" + tc.camera.base)
			if row["share_mode_h264"] == "" {
				t.Fatalf("the camera has no h264 node to speak of: %#v", row)
			}
			fallback, hasFallback := row["h264_default_preset"]
			ignored, isIgnored := row["h264_preset_ignored"]
			if fallback != tc.fallback || hasFallback != (tc.fallback != "") || ignored != tc.ignored || isIgnored != (tc.ignored != "") {
				t.Errorf("h264_default_preset = %q (present %v), h264_preset_ignored = %q (present %v)\nwant %q and %q; row: %#v",
					fallback, hasFallback, ignored, isIgnored, tc.fallback, tc.ignored, row)
			}
		})
	}
	t.Run("no h264 mode", func(t *testing.T) {
		h := newCSHarness(t, csC920())
		hcCanDecode(h.s, "jpegdec")
		row := h.reportOf("/dev/video4")
		_, hasFallback := row["h264_default_preset"]
		_, isIgnored := row["h264_preset_ignored"]
		if row["share_mode_h264"] != "" || hasFallback || isIgnored {
			t.Errorf("a camera without an h264 mode says of a preset: %#v", row)
		}
	})
}

// The node keeps the preset its app stated, shows it with its settings, and
// says what the encoder will run at when the app reads: one encoder serves
// every consumer of the camera, so another may state a faster one by then.
func TestCreateCameraShares_AnH264NodeStatesItsAppsPreset(t *testing.T) {
	h := newCSHarness(t, csC920())
	sfCanDeriveFrames(h.s)
	h.s.CreateCameraShares(context.Background(), "cv-a", "sh.wendy.cv-a", []appconfig.Entitlement{spH264At("veryfast")})
	records := h.records("cv-a")
	if len(records) != 1 || records[0].mode != sfH264Mode || records[0].preset != appconfig.EncodePresetVeryfast {
		t.Fatalf("records = %+v, want one h264 node at veryfast; logs: %v", records, h.logs.All())
	}
	want := "cv-a [format h264, stamp time, preset veryfast] (idle; " + sfSoftwareEncoderNote + " when the app reads" + fmt.Sprintf(spOrFaster, "veryfast") + ")"
	if got := h.reportOf("/dev/video4")["share_state"]; got != want {
		t.Errorf("share_state = %q\nwant         %q", got, want)
	}
	if n := h.logged("camera share: cv-a shares C920 (/dev/video4) through /dev/video255 at 1280x720 H264, 30 fps, format h264, stamp time, preset veryfast"); n != 1 {
		t.Errorf("the share line names the preset %d times, want once; logs: %v", n, h.logs.All())
	}
	warning := `camera share: cv-a: C920 (/dev/video4): its h264 node is fed by the software encoder x264enc, at preset veryfast or a faster one another consumer of the camera states, ` +
		`for as long as the app reads it; "format": "native" or "pixels" run no encoder`
	if n := sfWarnings(h, warning); n != 1 {
		t.Errorf("warned %d times of the software encoder and its preset, want once; logs: %v", n, h.logs.All())
	}
	if n := h.logged("not applied"); n != 0 {
		t.Errorf("a preset the device applies is said not to be: %v", h.logs.All())
	}
}

// While the app reads, the node names the preset the encoder runs at, and
// says so when that is not the one its app stated.
func TestCameraShareReport_AnH264NodeThatIsReadNamesThePresetItsEncoderRunsAt(t *testing.T) {
	const reading = "cv-a [format h264, stamp time%s] (reading; " + sfSoftwareEncoderNote + "%s)"
	for name, tc := range map[string]struct {
		stated  string
		running appconfig.EncodePreset
		want    string
	}{
		"the stated preset runs": {"veryfast", appconfig.EncodePresetVeryfast,
			fmt.Sprintf(reading, ", preset veryfast", " at preset veryfast")},
		"another consumer stated a faster one": {"veryfast", appconfig.EncodePresetUltrafast,
			fmt.Sprintf(reading, ", preset veryfast", " at preset ultrafast"+fmt.Sprintf(spNotRunning, "veryfast", "ultrafast"))},
		"the app states none": {"", appconfig.EncodePresetMedium,
			fmt.Sprintf(reading, "", " at preset medium")},
		"the producer has reported no preset": {"veryfast", noPreset,
			fmt.Sprintf(reading, ", preset veryfast", "")},
	} {
		t.Run(name, func(t *testing.T) {
			h := newCSHarness(t, csC920())
			sfCanDeriveFrames(h.s)
			h.s.CreateCameraShares(context.Background(), "cv-a", "sh.wendy.cv-a", []appconfig.Entitlement{spH264At(tc.stated)})
			spRunningHub(t, h.s, "/dev/video4", tc.running)
			if got := h.reportOf("/dev/video4")["share_state"]; strings.Contains(got, "asked for preset") || strings.Contains(got, "(reading") {
				t.Errorf("a node nobody reads speaks of the preset the encoder runs at for others: %q", got)
			}
			spSetPhase(h, "cv-a", shareReading)
			if got := h.reportOf("/dev/video4")["share_state"]; got != tc.want {
				t.Errorf("share_state = %q\nwant         %q", got, tc.want)
			}
		})
	}
}

// A preset the device does not apply is said on the node, with the encoder,
// and warned of at its create; a node whose app states none says nothing.
func TestCreateCameraShares_AStatedPresetTheDeviceDoesNotApplyIsSaidOnTheNode(t *testing.T) {
	for name, tc := range map[string]struct {
		camera   csCamera
		elements []string
		encoder  string
		why      string
	}{
		"a hardware encoder":                  {csC920(), []string{"jpegdec", "v4l2h264enc", "x264enc"}, "", "this device encodes H.264 with v4l2h264enc, which takes no preset"},
		"a software encoder without a preset": {csC920(), []string{"jpegdec", "openh264enc"}, "; fed by the software encoder openh264enc", "this device encodes H.264 with openh264enc, which takes no preset"},
		"a camera that produces H.264":        {csH264Cam(), []string{"avdec_h264", "jpegdec", "x264enc"}, "", "the camera produces H.264 itself, so no encoder runs for its node"},
	} {
		t.Run(name, func(t *testing.T) {
			path := "/dev/" + tc.camera.base
			h := newCSHarness(t, tc.camera)
			hcCanDecode(h.s, tc.elements...)
			h.s.CreateCameraShares(context.Background(), "cv-a", "sh.wendy.cv-a", []appconfig.Entitlement{spH264At("fast")})
			if records := h.records("cv-a"); len(records) != 1 || records[0].preset != appconfig.EncodePresetFast {
				t.Fatalf("records = %+v, want one h264 node that states fast; logs: %v", records, h.logs.All())
			}
			idle := tc.encoder
			if idle != "" {
				idle += " when the app reads"
			}
			want := "cv-a [format h264, stamp time, preset fast] (idle" + idle + "; preset fast is not applied: " + tc.why + ")"
			if got := h.reportOf(path)["share_state"]; got != want {
				t.Errorf("share_state = %q\nwant         %q", got, want)
			}
			spSetPhase(h, "cv-a", shareReading)
			want = "cv-a [format h264, stamp time, preset fast] (reading" + tc.encoder + "; preset fast is not applied: " + tc.why + ")"
			if got := h.reportOf(path)["share_state"]; got != want {
				t.Errorf("share_state = %q\nwant         %q", got, want)
			}
			spSetPhase(h, "cv-a", shareNoFrames)
			if got := h.reportOf(path)["share_state"]; strings.Contains(got, "not applied") {
				t.Errorf("a node that gets no frames speaks of its encoder: %q", got)
			}
			if n := sfWarnings(h, fmt.Sprintf("camera share: cv-a: %s (%s): preset fast is not applied: %s", tc.camera.id.Name, path, tc.why)); n != 1 {
				t.Errorf("warned %d times that the preset is not applied, want once; logs: %v", n, h.logs.All())
			}

			h.s.RemoveCameraShares("cv-a")
			h.drainRemovals()
			h.s.CreateCameraShares(context.Background(), "cv-b", "sh.wendy.cv-b", []appconfig.Entitlement{spH264At("")})
			if got := h.reportOf(path)["share_state"]; strings.Contains(got, "preset") && !strings.Contains(got, "fed by the software encoder") {
				t.Errorf("a node whose app states no preset speaks of one: %q", got)
			}
			if n := h.logged("cv-b: " + tc.camera.id.Name + " (" + path + "): preset"); n != 0 {
				t.Errorf("an app that states no preset was warned of one: %v", h.logs.All())
			}
		})
	}
}

// A preset is the encoder's, so a node that is served in another format than
// h264 keeps none.
func TestCreateCameraShares_ANodeServedInAnotherFormatKeepsNoPreset(t *testing.T) {
	h := newCSHarness(t, csC920())
	h.s.CreateCameraShares(context.Background(), "cv-a", "sh.wendy.cv-a", []appconfig.Entitlement{spH264At("veryfast")})
	records := h.records("cv-a")
	if len(records) != 1 || records[0].mode.Format == appconfig.CameraFormatH264 || records[0].askedFormat != appconfig.CameraFormatH264 {
		t.Fatalf("records = %+v, want one node served in another format than the h264 asked for", records)
	}
	if records[0].preset != noPreset {
		t.Errorf("the %s node keeps preset %q", records[0].mode.Format, records[0].preset)
	}
	if got := h.reportOf("/dev/video4")["share_state"]; strings.Contains(got, "preset") {
		t.Errorf("share_state speaks of a preset: %q", got)
	}
	if _, held := h.stateShares("cv-a")[0]["preset"]; held {
		t.Errorf("the state file holds a preset for a node that is not h264: %v", h.stateShares("cv-a")[0])
	}
}

func TestCameraShareState_ThePresetIsSavedWithFormatAndStampAndReadBack(t *testing.T) {
	h := newCSHarness(t, csC920())
	sfCanDeriveFrames(h.s)
	h.s.CreateCameraShares(context.Background(), "cv-a", "sh.wendy.cv-a", []appconfig.Entitlement{spH264At("veryfast")})
	if version := h.stateFile()["version"]; version != float64(cameraShareStateVersion) {
		t.Fatalf("the state file is version %v, want %d", version, cameraShareStateVersion)
	}
	saved := h.stateShares("cv-a")[0]
	if saved["preset"] != "veryfast" || saved["format"] != "h264" || saved["stamp"] != "time" {
		t.Fatalf("the saved share is %v, want format h264, stamp time, preset veryfast", saved)
	}
	state, err := os.ReadFile(cameraSharesPath)
	if err != nil {
		t.Fatal(err)
	}
	h.s.Shutdown()

	reload := func(t *testing.T, content string) *shareRecord {
		t.Helper()
		h := newCSHarness(t, csC920())
		sfCanDeriveFrames(h.s)
		h.writeState(content)
		h.s.loadCameraShares()
		records := h.records("cv-a")
		if len(records) != 1 {
			t.Fatalf("loaded %d records, want the share; logs: %v", len(records), h.logs.All())
		}
		return records[0]
	}
	if r := reload(t, string(state)); r.preset != appconfig.EncodePresetVeryfast || r.mode != sfH264Mode {
		t.Errorf("read back %s with preset %q, want the h264 node at veryfast", describeShareNode(r.mode, r.stamp), r.preset)
	}
	for name, content := range map[string]string{
		"a version 2 record from before presets":   strings.Replace(string(state), `"preset":"veryfast",`, "", 1),
		"a preset this agent does not know":        strings.Replace(string(state), `"preset":"veryfast"`, `"preset":"placebo"`, 1),
		"a preset on a node that is not h264":      strings.NewReplacer(`"format":"h264"`, `"format":"native"`, `"fourcc":"H264"`, `"fourcc":"MJPG"`).Replace(string(state)),
		"a preset that would inject an argument":   strings.Replace(string(state), `"preset":"veryfast"`, `"preset":"veryfast ! filesink location=/tmp/x"`, 1),
		"a preset in another case than its name's": strings.Replace(string(state), `"preset":"veryfast"`, `"preset":"VeryFast"`, 1),
	} {
		if content == string(state) {
			t.Fatalf("%s: the fixture does not differ from the saved state: %s", name, state)
		}
		if r := reload(t, content); r.preset != noPreset {
			t.Errorf("%s: read back preset %q, want none", name, r.preset)
		}
	}
}

func TestCameraShareState_AShareWithoutAPresetSavesNoKey(t *testing.T) {
	h := newCSHarness(t, csC920())
	sfCanDeriveFrames(h.s)
	h.s.CreateCameraShares(context.Background(), "cv-a", "sh.wendy.cv-a", []appconfig.Entitlement{spH264At("")})
	if saved := h.stateShares("cv-a")[0]; saved["format"] != "h264" || reflect.ValueOf(saved).MapIndex(reflect.ValueOf("preset")).IsValid() {
		t.Errorf("the saved share is %v, want an h264 share without a preset key", saved)
	}
}

// A container created again with another preset keeps its node: the node
// declares nothing of the preset. Its record, its state file entry and the
// preset its pump states at its next join take the new one.
func TestCreateCameraShares_ACreateWithAnotherPresetKeepsTheNodeAndStatesTheNewOne(t *testing.T) {
	h := newCSHarness(t, csC920())
	sfCanDeriveFrames(h.s)
	installFakeProducers(h.s)
	h.s.CreateCameraShares(context.Background(), "cv-a", "sh.wendy.cv-a", []appconfig.Entitlement{spH264At("veryfast")})
	before := h.records("cv-a")[0]
	node, opened := before.node, len(h.openedNodes())
	statedAtAJoin := func() appconfig.EncodePreset {
		t.Helper()
		sub, err := h.s.joinShare(context.Background(), before)
		if err != nil {
			t.Fatal(err)
		}
		defer sub.Leave()
		return currentHub(h.s, "/dev/video4").statedPreset()
	}
	if got := statedAtAJoin(); got != appconfig.EncodePresetVeryfast {
		t.Fatalf("the pump's join states %q, want veryfast", got)
	}

	for _, step := range []struct {
		preset string
		want   appconfig.EncodePreset
		line   string
	}{
		{"medium", appconfig.EncodePresetMedium, "camera share: cv-a was created again: its share of C920 was 1280x720 H264, 30 fps, format h264, stamp time, preset veryfast " +
			"and is now 1280x720 H264, 30 fps, format h264, stamp time, preset medium; its node stays as it is"},
		{"", noPreset, "camera share: cv-a was created again: its share of C920 was 1280x720 H264, 30 fps, format h264, stamp time, preset medium " +
			"and is now 1280x720 H264, 30 fps, format h264, stamp time; its node stays as it is"},
	} {
		h.s.CreateCameraShares(context.Background(), "cv-a", "sh.wendy.cv-a", []appconfig.Entitlement{spH264At(step.preset)})
		records := h.records("cv-a")
		if len(records) != 1 || records[0] != before || records[0].node != node || len(h.openedNodes()) != opened {
			t.Fatalf("preset %q: the share was made again: %+v, %d nodes opened; logs: %v", step.preset, records, len(h.openedNodes()), h.logs.All())
		}
		if got := h.s.sharePreset(before); got != step.want {
			t.Errorf("preset %q: the record states %q", step.preset, got)
		}
		if got := statedAtAJoin(); got != step.want {
			t.Errorf("preset %q: the pump's next join states %q", step.preset, got)
		}
		if saved, _ := h.stateShares("cv-a")[0]["preset"].(string); saved != step.want.String() {
			t.Errorf("preset %q: the state file holds %q", step.preset, saved)
		}
		if n := h.logged(step.line); n != 1 {
			t.Errorf("preset %q: %d lines say the preset changed, want one; logs: %v", step.preset, n, h.logs.All())
		}
	}
	lines := h.logged("was created again")
	h.s.CreateCameraShares(context.Background(), "cv-a", "sh.wendy.cv-a", []appconfig.Entitlement{spH264At("")})
	if n := h.logged("was created again"); n != lines {
		t.Errorf("a create with the same preset logged a change: %v", h.logs.All())
	}
	if n := h.logged("is made again"); n != 0 {
		t.Errorf("the node was made again for a preset: %v", h.logs.All())
	}
}

func TestShareDecision_SaysWhyAPresetIsIgnoredOnlyOfAnH264NodeWhoseEncoderTakesNone(t *testing.T) {
	jpeg, h264 := ownFrames{codec: videoCodecMJPEG}, ownFrames{codec: agentpb.VideoCodec_VIDEO_CODEC_H264}
	for name, tc := range map[string]struct {
		mode   sharedMode
		frames nodeFrames
		want   string
	}{
		"x264enc":                        {sfH264Mode, nodeFrames{own: jpeg, h264Encoder: "x264enc"}, ""},
		"a hardware encoder":             {sfH264Mode, nodeFrames{own: jpeg, h264Encoder: "nvv4l2h264enc"}, "this device encodes H.264 with nvv4l2h264enc, which takes no preset"},
		"no encoder":                     {sfH264Mode, nodeFrames{own: jpeg}, "no H.264 encoder that takes one was found on this device"},
		"the camera's own H.264":         {sfH264Mode, nodeFrames{own: h264, h264Encoder: "x264enc"}, "the camera produces H.264 itself, so no encoder runs for its node"},
		"a node of the camera's frames":  {sfNativeMode, nodeFrames{own: jpeg, h264Encoder: "nvv4l2h264enc"}, ""},
		"a node of pictures":             {sfPixelsMode, nodeFrames{own: jpeg, h264Encoder: "nvv4l2h264enc"}, ""},
		"an h264 node that is not there": {sharedMode{}, nodeFrames{own: jpeg, h264Encoder: "nvv4l2h264enc"}, ""},
	} {
		decision := shareDecision{Shared: tc.mode != (sharedMode{}), Mode: tc.mode}
		if got := decision.sayingWhyAPresetIsIgnoredOf(tc.frames).PresetIgnored; got != tc.want {
			t.Errorf("%s: PresetIgnored = %q, want %q", name, got, tc.want)
		}
	}
}
