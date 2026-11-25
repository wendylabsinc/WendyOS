package commands

import (
	"context"
	"io"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
	"github.com/wendylabsinc/wendy/go/internal/shared/camerashare"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

type hwCap = agentpb.ListHardwareCapabilitiesResponse_HardwareCapability

func shareCfg(mode string) *appconfig.AppConfig {
	return &appconfig.AppConfig{Entitlements: []appconfig.Entitlement{{Type: appconfig.EntitlementCamera, Mode: mode}}}
}

func shareRow(desc, mode string) *hwCap {
	props := map[string]string{}
	if mode != "" {
		props[camerashare.KeyMode] = mode
	}
	return &hwCap{Category: "camera", Description: desc, Properties: props}
}

func TestCameraShareNotice_OneLineNamingEachCameraAndDirect(t *testing.T) {
	got := cameraShareNotice([]*appconfig.AppConfig{shareCfg("")}, []*hwCap{
		shareRow("Brio 101 (video0)", "1280x720 YUYV, 30 fps"),
		shareRow("C270 HD WEBCAM (video2)", "640x480 YUYV, 30 fps"),
	})
	want := `Camera: this app shares Brio 101 (video0) at 1280x720 YUYV, 30 fps, format pixels, stamp time; C270 HD WEBCAM (video2) at 640x480 YUYV, 30 fps, format pixels, stamp time. For the real device (its own formats, sizes and controls), set "mode": "direct" on the camera entitlement.`
	if got != want {
		t.Fatalf("got %q\nwant %q", got, want)
	}
}

// shareRowAt is a camera row at a device path with the mode of every node
// format the agent can share it in.
func shareRowAt(desc, path string, modes map[string]string) *hwCap {
	props := map[string]string{}
	for format, mode := range modes {
		key := camerashare.KeyMode
		if format != appconfig.CameraFormatPixels {
			key = camerashare.KeyModeOfFormat(format)
		}
		props[key] = mode
	}
	return &hwCap{Category: "camera", Description: desc, DevicePath: path, Properties: props}
}

func TestCameraShareNotice_NamesEachCamerasFormatAndStampFromTheEntitlement(t *testing.T) {
	brio := shareRowAt("Brio 101 (video0)", "/dev/video0", map[string]string{
		"pixels": "1280x720 YUYV, 30 fps", "native": "1280x720 MJPG, 30 fps", "h264": "1280x720 H264, 30 fps"})
	c920 := shareRowAt("C920 (video4)", "/dev/video4", map[string]string{
		"pixels": "640x480 YUYV, 30 fps", "native": "640x480 MJPG, 30 fps", "h264": "640x480 H264, 30 fps"})
	rawOnly := shareRowAt("UYVY Cam (video6)", "/dev/video6", map[string]string{"pixels": "640x480 UYVY, 30 fps"})
	rawOnly.Properties[camerashare.KeyUnavailableOfFormat("native")] = "the camera's own frames are not captured whole: the camera offers no JPEG at 640x480"
	cfg := &appconfig.AppConfig{Entitlements: []appconfig.Entitlement{
		{Type: appconfig.EntitlementCamera, Allowlist: []string{"/dev/video0"}, Format: appconfig.CameraFormatNative, Stamp: appconfig.CameraStampFrame},
		{Type: appconfig.EntitlementCamera, Allowlist: []string{"C920"}, Format: appconfig.CameraFormatH264},
		{Type: appconfig.EntitlementCamera, Allowlist: []string{"UYVY Cam"}, Format: appconfig.CameraFormatNative},
	}}
	got := cameraShareNotice([]*appconfig.AppConfig{cfg}, []*hwCap{brio, c920, rawOnly})
	want := `Camera: this app shares Brio 101 (video0) at 1280x720 MJPG, 30 fps, format native, stamp frame; C920 (video4) at 640x480 H264, 30 fps, format h264, stamp time; UYVY Cam (video6) at 640x480 UYVY, 30 fps, format pixels, stamp time (asked for native; serving pixels because the camera's own frames are not captured whole: the camera offers no JPEG at 640x480). For the real device (its own formats, sizes and controls), set "mode": "direct" on the camera entitlement.`
	if got != want {
		t.Fatalf("got %q\nwant %q", got, want)
	}
}

// An agent from before node formats reports only share_mode. It shares every
// camera as pictures stamped with capture times and reads no format, stamp or
// allowlist, so the notice promises none of them and says so once.
func TestCameraShareNotice_AnAgentThatReadsNoNodeSettingsServesPicturesWithCaptureTimes(t *testing.T) {
	rows := []*hwCap{shareRowAt("Brio 101 (video0)", "/dev/video0", map[string]string{"pixels": "1280x720 YUYV, 30 fps"})}
	shared := `Camera: this app shares Brio 101 (video0) at 1280x720 YUYV, 30 fps, format pixels, stamp time. For the real device (its own formats, sizes and controls), set "mode": "direct" on the camera entitlement.`
	const readsNone = `Camera: the agent on this device does not read a camera entitlement's format, stamp, preset or allowlist: every shared camera's node carries pictures stamped with the capture time until the agent is updated.`
	for name, cfg := range map[string]*appconfig.AppConfig{
		"format and stamp":           cameraCfg(appconfig.CameraFormatH264, appconfig.CameraStampFrame),
		"allowlist naming the path":  cameraCfg(appconfig.CameraFormatNative, appconfig.CameraStampFrame, "/dev/video0"),
		"allowlist naming the name":  cameraCfg("", appconfig.CameraStampFrame, "Brio 101"),
		"allowlist naming no camera": cameraCfg("", "", "v4l2:/dev/video9", "by-id:usb-Typo-video-index0"),
		"two entries that disagree":  {Entitlements: append(cameraCfg(appconfig.CameraFormatNative, "", "/dev/video0").Entitlements, cameraCfg(appconfig.CameraFormatH264, "", "Brio 101").Entitlements...)},
	} {
		cfgs := []*appconfig.AppConfig{cfg}
		if got := cameraShareNotice(cfgs, rows); got != shared {
			t.Errorf("%s:\ngot  %q\nwant %q", name, got, shared)
		}
		if got := printedCameraNotice(t, rows, cfg); !slices.Equal(got, []string{shared, readsNone}) {
			t.Errorf("%s: printed %q\nwant the share line and %q", name, got, readsNone)
		}
	}
	if got := printedCameraNotice(t, rows, shareCfg("")); !slices.Equal(got, []string{shared}) {
		t.Errorf("an app that sets none of them printed %q, want only the share line", got)
	}
}

// A camera that produces H.264 on a device that cannot decode it has no
// pictures: an app that asks for them, or for nothing, gets the camera's own
// frames and is told why.
func TestCameraShareNotice_ACameraWithoutPicturesServesItsOwnFramesAndSaysWhy(t *testing.T) {
	const whyNot = "pictures cannot be decoded from its frames on this device: GStreamer lacks a decoder (nvv4l2decoder with nvvidconv, v4l2h264dec, avdec_h264, openh264dec)"
	row := shareRowAt("Logi H264 (video2)", "/dev/video2", map[string]string{"native": "1280x720 H264, 30 fps", "h264": "1280x720 H264, 30 fps"})
	row.Properties[camerashare.KeyUnavailableOfFormat("pixels")] = whyNot
	asksForH264 := &appconfig.AppConfig{Entitlements: []appconfig.Entitlement{{Type: appconfig.EntitlementCamera, Format: appconfig.CameraFormatH264, Stamp: appconfig.CameraStampFrame}}}
	got := cameraShareNotice([]*appconfig.AppConfig{shareCfg(""), asksForH264}, []*hwCap{row})
	want := `Camera: this app shares Logi H264 (video2) at 1280x720 H264, 30 fps, format native, stamp time (asked for pixels; serving native because ` + whyNot +
		`); Logi H264 (video2) at 1280x720 H264, 30 fps, format h264, stamp frame. For the real device (its own formats, sizes and controls), set "mode": "direct" on the camera entitlement.`
	if got != want {
		t.Fatalf("got %q\nwant %q", got, want)
	}
}

func TestCameraShareNotice_ServicesWithDifferentSettingsEachNameTheirs(t *testing.T) {
	row := shareRowAt("Brio 101 (video0)", "/dev/video0", map[string]string{"pixels": "1280x720 YUYV, 30 fps", "native": "1280x720 MJPG, 30 fps"})
	native := &appconfig.AppConfig{Entitlements: []appconfig.Entitlement{{Type: appconfig.EntitlementCamera, Format: appconfig.CameraFormatNative}}}
	got := cameraShareNotice([]*appconfig.AppConfig{shareCfg(""), native, shareCfg("")}, []*hwCap{row})
	want := `Camera: this app shares Brio 101 (video0) at 1280x720 YUYV, 30 fps, format pixels, stamp time; Brio 101 (video0) at 1280x720 MJPG, 30 fps, format native, stamp time. For the real device (its own formats, sizes and controls), set "mode": "direct" on the camera entitlement.`
	if got != want {
		t.Fatalf("got %q\nwant %q", got, want)
	}
}

func TestCameraShareNotice_SilentWhenNothingWillBeShared(t *testing.T) {
	rows := []*hwCap{shareRow("Brio 101 (video0)", "1280x720 YUYV, 30 fps")}
	if got := cameraShareNotice([]*appconfig.AppConfig{shareCfg("direct")}, rows); got != "" {
		t.Fatalf("direct app: %q", got)
	}
	noCam := &appconfig.AppConfig{Entitlements: []appconfig.Entitlement{{Type: appconfig.EntitlementNetwork}}}
	if got := cameraShareNotice([]*appconfig.AppConfig{noCam}, rows); got != "" {
		t.Fatalf("no camera entitlement: %q", got)
	}
	if got := cameraShareNotice([]*appconfig.AppConfig{shareCfg("")}, []*hwCap{shareRow("Brio 101 (video0)", "")}); got != "" {
		t.Fatalf("older agent: %q", got)
	}
}

func TestCameraShareNotice_ServicesShareOneLine(t *testing.T) {
	got := cameraShareNotice([]*appconfig.AppConfig{shareCfg(""), shareCfg("")}, []*hwCap{shareRow("Brio 101 (video0)", "1280x720 YUYV, 30 fps")})
	if strings.Contains(got, "\n") || strings.Count(got, "Camera:") != 1 {
		t.Fatalf("want one line, got %q", got)
	}
}

type blockingHW struct {
	agentpb.WendyAgentServiceClient
}

func (blockingHW) ListHardwareCapabilities(ctx context.Context, _ *agentpb.ListHardwareCapabilitiesRequest, _ ...grpc.CallOption) (*agentpb.ListHardwareCapabilitiesResponse, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestPrintCameraShareNotice_SlowAgentPrintsNothing(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stderr
	os.Stderr = w
	// Restored even when the test fails or panics before the read below.
	t.Cleanup(func() { os.Stderr = orig })
	start := time.Now()
	printCameraShareNotice(context.Background(), &grpcclient.AgentConnection{AgentService: blockingHW{}}, shareCfg(""))
	elapsed := time.Since(start)
	os.Stderr = orig
	_ = w.Close()
	out, _ := io.ReadAll(r)
	if elapsed > 3200*time.Millisecond {
		t.Fatalf("took %v", elapsed)
	}
	if len(out) != 0 {
		t.Fatalf("printed %q", out)
	}
}

const c920StableID = "by-id:usb-Sonix_Technology_Co.__Ltd._HD_Pro_Webcam_C920_HD_Pro_Webcam_C920-video-index0"

var c920Modes = map[string]string{"pixels": "1280x720 YUYV, 30 fps", "native": "1280x720 MJPG, 30 fps", "h264": "1280x720 H264, 30 fps"}

// c920Row is the camera's row from an agent that matches allowlist entries by
// stable id: it puts stable_id on the row.
func c920Row() *hwCap {
	row := c920RowFromAnAgentBeforeStableIDs()
	row.Properties[camerashare.KeyStableID] = c920StableID
	return row
}

func c920RowFromAnAgentBeforeStableIDs() *hwCap {
	return shareRowAt("HD Pro Webcam C920: HD Pro Webc (video0)", "/dev/video0", c920Modes)
}

func cameraCfg(format, stamp string, allowlist ...string) *appconfig.AppConfig {
	return &appconfig.AppConfig{Entitlements: []appconfig.Entitlement{
		{Type: appconfig.EntitlementCamera, Allowlist: allowlist, Format: format, Stamp: stamp},
	}}
}

const (
	c920SharedNative = `Camera: this app shares HD Pro Webcam C920: HD Pro Webc (video0) at 1280x720 MJPG, 30 fps, format native, stamp frame. For the real device (its own formats, sizes and controls), set "mode": "direct" on the camera entitlement.`
	c920SharedPlain  = `Camera: this app shares HD Pro Webcam C920: HD Pro Webc (video0) at 1280x720 YUYV, 30 fps, format pixels, stamp time. For the real device (its own formats, sizes and controls), set "mode": "direct" on the camera entitlement.`
	selectsNoNode    = ` selects no shared camera's node on this device, so its entitlement's format and stamp reach none. "wendy device camera list --json" prints each camera's stable_id, path and name.`
)

func TestCameraShareNotice_AnEntryNamingACameraByItsStableIDGivesItsFormatAndStamp(t *testing.T) {
	for _, entry := range []string{c920StableID, strings.TrimPrefix(c920StableID, "by-id:"), "v4l2:/dev/video0"} {
		cfgs := []*appconfig.AppConfig{cameraCfg(appconfig.CameraFormatNative, appconfig.CameraStampFrame, entry)}
		if got := cameraShareNotice(cfgs, []*hwCap{c920Row()}); got != c920SharedNative {
			t.Errorf("allowlist %q:\ngot  %q\nwant %q", entry, got, c920SharedNative)
		}
		if lines := cameraAllowlistNotices(cfgs, []*hwCap{c920Row()}); len(lines) != 0 {
			t.Errorf("allowlist %q names the camera, yet: %q", entry, lines)
		}
	}
}

// A camera without udev names has no stable id, and its agent still matches
// the "v4l2:" spelling: the row carries the key, empty.
func TestCameraShareNotice_ACameraWithoutAStableIDOnAnAgentThatMatchesThemIsResolvedByItsRules(t *testing.T) {
	row := c920RowFromAnAgentBeforeStableIDs()
	row.Properties[camerashare.KeyStableID] = ""
	cfgs := []*appconfig.AppConfig{cameraCfg(appconfig.CameraFormatNative, appconfig.CameraStampFrame, "v4l2:/dev/video0")}
	if got := cameraShareNotice(cfgs, []*hwCap{row}); got != c920SharedNative {
		t.Errorf("got  %q\nwant %q", got, c920SharedNative)
	}
	if lines := cameraAllowlistNotices(cfgs, []*hwCap{row}); len(lines) != 0 {
		t.Errorf("the entry names the camera, yet: %q", lines)
	}
}

// An agent from before stable ids matches an allowlist entry only against a
// camera's exact device path or name, and serves the defaults for any other
// spelling: the notice must not promise what that agent does not serve.
func TestCameraShareNotice_AnAgentFromBeforeStableIDsIsResolvedAsItResolves(t *testing.T) {
	rows := []*hwCap{c920RowFromAnAgentBeforeStableIDs()}
	bare := strings.TrimPrefix(c920StableID, "by-id:")
	for _, entry := range []string{"v4l2:/dev/video0", c920StableID, bare} {
		cfgs := []*appconfig.AppConfig{cameraCfg(appconfig.CameraFormatNative, appconfig.CameraStampFrame, entry)}
		if got := cameraShareNotice(cfgs, rows); got != c920SharedPlain {
			t.Errorf("allowlist %q:\ngot  %q\nwant %q", entry, got, c920SharedPlain)
		}
	}
	for _, entry := range []string{"/dev/video0", "HD Pro Webcam C920: HD Pro Webc"} {
		cfgs := []*appconfig.AppConfig{cameraCfg(appconfig.CameraFormatNative, appconfig.CameraStampFrame, entry)}
		if got := cameraShareNotice(cfgs, rows); got != c920SharedNative {
			t.Errorf("allowlist %q:\ngot  %q\nwant %q", entry, got, c920SharedNative)
		}
		if lines := cameraAllowlistNotices(cfgs, rows); len(lines) != 0 {
			t.Errorf("allowlist %q names the camera as that agent matches, yet: %q", entry, lines)
		}
	}

	cfgs := []*appconfig.AppConfig{
		cameraCfg(appconfig.CameraFormatNative, "", "v4l2:/dev/video0", c920StableID, "/dev/video9"),
		cameraCfg(appconfig.CameraFormatNative, "", bare, "v4l2:/dev/video0", "HD Pro Webcam C920"),
	}
	want := []string{
		`Camera: allowlist entry "/dev/video9"` + selectsNoNode,
		`Camera: allowlist entry "` + bare + `"` + selectsNoNode,
		`Camera: allowlist entry "HD Pro Webcam C920"` + selectsNoNode,
		`Camera: the agent on this device predates stable id and "v4l2:" allowlist entries and matches an entry only against a camera's device path or name, so these select no camera's node: "v4l2:/dev/video0", "` + c920StableID + `". Update the agent, or name the camera by its device path or name.`,
	}
	if got := cameraAllowlistNotices(cfgs, rows); !slices.Equal(got, want) {
		t.Errorf("got  %q\nwant %q", got, want)
	}
}

// Rows that say only why no camera is shared are an agent from before stable
// ids or one from before node settings: nothing is said that is true of only
// one of them.
func TestCameraAllowlistNotices_AnAgentOfUnknownGenerationIsToldOnlyWhatHoldsForEither(t *testing.T) {
	unshared := &hwCap{Category: "camera", Description: "Lepton (video4)", DevicePath: "/dev/video4",
		Properties: map[string]string{camerashare.KeyUnavailable: "a thermal camera is not shared"}}
	cfg := cameraCfg(appconfig.CameraFormatNative, "", "v4l2:/dev/video4", c920StableID, "/dev/video4")
	want := []string{
		`Camera: allowlist entry "v4l2:/dev/video4"` + selectsNoNode,
		`Camera: allowlist entry "` + c920StableID + `"` + selectsNoNode,
		`Camera: allowlist entry "/dev/video4" names Lepton (video4), which is not shared through a node (a thermal camera is not shared), so the entry configures nothing.`,
	}
	if got := printedCameraNotice(t, []*hwCap{unshared}, cfg); !slices.Equal(got, want) {
		t.Errorf("printed %q\nwant    %q", got, want)
	}
}

func TestCameraAllowlistNotices_OneLineForEachEntryThatSelectsNoSharedCamerasNode(t *testing.T) {
	typo := &appconfig.AppConfig{Entitlements: []appconfig.Entitlement{
		{Type: appconfig.EntitlementCamera, Allowlist: []string{c920StableID + "0", "/dev/video0"}, Format: appconfig.CameraFormatNative},
		{Type: appconfig.EntitlementCamera, Allowlist: []string{"HD Pro Webcam C920", "HD Pro Webcam C920: HD Pro Webc"}, Format: appconfig.CameraFormatNative},
	}}
	sameTypo := cameraCfg("", "", "HD Pro Webcam C920")
	direct := &appconfig.AppConfig{Entitlements: []appconfig.Entitlement{{Type: appconfig.EntitlementCamera, Mode: "direct", Allowlist: []string{"/dev/video9"}}}}
	want := []string{
		`Camera: allowlist entry "` + c920StableID + `0"` + selectsNoNode,
		`Camera: allowlist entry "HD Pro Webcam C920"` + selectsNoNode,
	}
	got := cameraAllowlistNotices([]*appconfig.AppConfig{typo, nil, sameTypo, direct}, []*hwCap{c920Row()})
	if !slices.Equal(got, want) {
		t.Fatalf("got  %q\nwant %q", got, want)
	}
}

// An entry that names a camera the app reads without a share node configures
// nothing, which is not the same as naming nothing.
func TestCameraAllowlistNotices_AnEntryNamingACameraThatIsNotSharedThroughANodeSaysSo(t *testing.T) {
	network := &hwCap{Category: "camera", Description: "RLC-520A (video200)", DevicePath: "/dev/video200",
		Properties: map[string]string{camerashare.KeyStableID: "ip-mac:aa:00:00:00:00:01"}}
	metadata := &hwCap{Category: "camera", Description: "HD Pro Webcam C920: HD Pro Webc (video1)", DevicePath: "/dev/video1", Properties: map[string]string{"transport": "usb"}}
	thermal := &hwCap{Category: "camera", Description: "Lepton (video4)", DevicePath: "/dev/video4",
		Properties: map[string]string{camerashare.KeyStableID: "by-path:platform-usb-0:1.3:1.0-video-index0", camerashare.KeyUnavailable: "a thermal camera is not shared"}}
	rows := []*hwCap{c920Row(), metadata, network, thermal}
	cfgs := []*appconfig.AppConfig{cameraCfg(appconfig.CameraFormatNative, "",
		"ip-mac:aa:00:00:00:00:01", "v4l2:/dev/video200", "/dev/video1", "by-path:platform-usb-0:1.3:1.0-video-index0", "HD Pro Webcam C920: HD Pro Webc")}
	want := []string{
		`Camera: allowlist entry "ip-mac:aa:00:00:00:00:01" names RLC-520A (video200), which is not shared through a node, so the entry configures nothing.`,
		`Camera: allowlist entry "v4l2:/dev/video200" names RLC-520A (video200), which is not shared through a node, so the entry configures nothing.`,
		`Camera: allowlist entry "/dev/video1" names HD Pro Webcam C920: HD Pro Webc (video1), which is not shared through a node, so the entry configures nothing.`,
		`Camera: allowlist entry "by-path:platform-usb-0:1.3:1.0-video-index0" names Lepton (video4), which is not shared through a node (a thermal camera is not shared), so the entry configures nothing.`,
	}
	if got := cameraAllowlistNotices(cfgs, rows); !slices.Equal(got, want) {
		t.Fatalf("got  %q\nwant %q", got, want)
	}
}

// Entries that name one camera by different identifiers pass validation, which
// has no device to ask, so the deploy says what the agent will make of them.
func TestCameraSettingsConflictNotices_OneLineForEachCameraWhoseEntriesDisagree(t *testing.T) {
	bare := strings.TrimPrefix(c920StableID, "by-id:")
	disagreeing := &appconfig.AppConfig{Entitlements: []appconfig.Entitlement{
		{Type: appconfig.EntitlementCamera, Allowlist: []string{"/dev/video0"}, Format: appconfig.CameraFormatH264, Stamp: appconfig.CameraStampFrame},
		{Type: appconfig.EntitlementCamera, Allowlist: []string{bare}, Format: appconfig.CameraFormatNative},
	}}
	brio := shareRowAt("Brio 101 (video2)", "/dev/video2", c920Modes)
	brio.Properties[camerashare.KeyStableID] = ""
	want := []string{`Camera: HD Pro Webcam C920: HD Pro Webc (video0): format is native (asked by allowlist entry "` + bare + `"); passed over: allowlist entry "/dev/video0" asks for h264; ` +
		`stamp is time (asked by allowlist entry "` + bare + `"); passed over: allowlist entry "/dev/video0" asks for frame.`}
	rows := []*hwCap{c920Row(), brio}
	if got := cameraSettingsConflictNotices([]*appconfig.AppConfig{disagreeing, shareCfg(""), disagreeing}, rows); !slices.Equal(got, want) {
		t.Fatalf("got  %q\nwant %q", got, want)
	}

	// The agent before stable ids does not match the stable id entry, so for it
	// nothing disagrees.
	if got := cameraSettingsConflictNotices([]*appconfig.AppConfig{disagreeing}, []*hwCap{c920RowFromAnAgentBeforeStableIDs()}); len(got) != 0 {
		t.Fatalf("an agent that matches one of the entries is told of a disagreement: %q", got)
	}
}

type listingHW struct {
	agentpb.WendyAgentServiceClient
	rows []*hwCap
}

func (hw listingHW) ListHardwareCapabilities(context.Context, *agentpb.ListHardwareCapabilitiesRequest, ...grpc.CallOption) (*agentpb.ListHardwareCapabilitiesResponse, error) {
	return &agentpb.ListHardwareCapabilitiesResponse{Capabilities: hw.rows}, nil
}

func printedCameraNotice(t *testing.T, rows []*hwCap, cfgs ...*appconfig.AppConfig) []string {
	t.Helper()
	conn := &grpcclient.AgentConnection{AgentService: listingHW{rows: rows}}
	out := strings.TrimSpace(captureStderr(t, func() { printCameraShareNotice(context.Background(), conn, cfgs...) }))
	if out == "" {
		return nil
	}
	return strings.Split(out, "\n")
}

func TestPrintCameraShareNotice_PrintsTheShareLineThenDisagreementsThenEntriesThatConfigureNothing(t *testing.T) {
	cfg := &appconfig.AppConfig{Entitlements: []appconfig.Entitlement{
		{Type: appconfig.EntitlementCamera, Allowlist: []string{"/dev/video0", "by-id:usb-Typo-video-index0"}, Format: appconfig.CameraFormatH264},
		{Type: appconfig.EntitlementCamera, Allowlist: []string{c920StableID}, Format: appconfig.CameraFormatNative},
	}}
	lines := printedCameraNotice(t, []*hwCap{c920Row()}, cfg)
	if len(lines) != 3 || !strings.Contains(lines[0], "this app shares") || !strings.Contains(lines[0], "format native, stamp time") ||
		!strings.Contains(lines[1], `passed over: allowlist entry "/dev/video0" asks for h264`) ||
		!strings.Contains(lines[2], `allowlist entry "by-id:usb-Typo-video-index0" selects no shared camera's node`) {
		t.Fatalf("printed %q, want the share line, the disagreement and the entry that selects nothing", lines)
	}
}

// With no camera that can be shared, an agent that shares cameras still says
// so in its rows, and an entry that then configures nothing is not passed over
// in silence.
func TestPrintCameraShareNotice_WithNoCameraToShareAnEntryThatConfiguresNothingIsStillSaid(t *testing.T) {
	thermal := &hwCap{Category: "camera", Description: "Lepton (video4)", DevicePath: "/dev/video4",
		Properties: map[string]string{camerashare.KeyStableID: "", camerashare.KeyUnavailable: "a thermal camera is not shared"}}
	cfg := cameraCfg(appconfig.CameraFormatNative, "", "/dev/video0", "/dev/video4")
	lines := printedCameraNotice(t, []*hwCap{thermal}, cfg)
	want := []string{
		`Camera: allowlist entry "/dev/video0"` + selectsNoNode,
		`Camera: allowlist entry "/dev/video4" names Lepton (video4), which is not shared through a node (a thermal camera is not shared), so the entry configures nothing.`,
	}
	if !slices.Equal(lines, want) {
		t.Fatalf("printed %q\nwant    %q", lines, want)
	}

	// An agent that says nothing of sharing gives no node for an entry to configure.
	if lines := printedCameraNotice(t, []*hwCap{shareRow("Brio 101 (video0)", "")}, cfg); lines != nil {
		t.Fatalf("an agent that reports no sharing printed %q", lines)
	}
	if lines := printedCameraNotice(t, nil, cfg); lines != nil {
		t.Fatalf("a device without camera rows printed %q", lines)
	}
}

const (
	c920SharedH264      = `Camera: this app shares HD Pro Webcam C920: HD Pro Webc (video0) at 1280x720 H264, 30 fps, format h264, stamp time. For the real device (its own formats, sizes and controls), set "mode": "direct" on the camera entitlement.`
	c920EncodedSoftware = `Camera: HD Pro Webcam C920: HD Pro Webc (video0): its H.264 is encoded in software (x264enc) on this device for as long as this app reads its node. "format": "native" or "pixels" run no encoder.`
)

// c920RowEncodedInSoftware is the camera's row from an agent on a device that
// makes the camera's H.264 with a software encoder.
func c920RowEncodedInSoftware() *hwCap {
	row := c920Row()
	row.Properties[camerashare.KeyH264Encoder] = "x264enc (software)"
	return row
}

// An app whose node on a camera is H.264 that the device makes in software is
// told so on one line under the share line, however many of its services read
// that node.
func TestPrintCameraShareNotice_AnH264NodeEncodedInSoftwareIsSaidUnderTheShareLine(t *testing.T) {
	rows := []*hwCap{c920RowEncodedInSoftware()}
	want := []string{c920SharedH264, c920EncodedSoftware}
	if got := printedCameraNotice(t, rows, cameraCfg(appconfig.CameraFormatH264, "")); !slices.Equal(got, want) {
		t.Errorf("printed %q\nwant    %q", got, want)
	}
	if got := printedCameraNotice(t, rows, cameraCfg(appconfig.CameraFormatH264, ""), cameraCfg(appconfig.CameraFormatH264, "", c920StableID)); !slices.Equal(got, want) {
		t.Errorf("two services with the same node printed %q\nwant %q", got, want)
	}
}

func TestCameraSoftwareEncoderNotices_NothingIsSaidWhereNoSoftwareEncoderFeedsTheAppsNode(t *testing.T) {
	withoutH264Node := c920RowEncodedInSoftware()
	delete(withoutH264Node.Properties, camerashare.KeyModeOfFormat(appconfig.CameraFormatH264))
	withoutH264Node.Properties[camerashare.KeyUnavailableOfFormat(appconfig.CameraFormatH264)] = "the camera's own frames are not captured whole"
	notSoftware := c920Row()
	notSoftware.Properties[camerashare.KeyH264Encoder] = "nvv4l2h264enc (hardware)"
	h264 := cameraCfg(appconfig.CameraFormatH264, "")
	for name, tc := range map[string]struct {
		row *hwCap
		cfg *appconfig.AppConfig
	}{
		"an agent that does not send the property":   {c920Row(), h264},
		"an agent from before stable ids":            {c920RowFromAnAgentBeforeStableIDs(), h264},
		"the app reads the camera's own frames":      {c920RowEncodedInSoftware(), cameraCfg(appconfig.CameraFormatNative, "")},
		"the app reads pictures":                     {c920RowEncodedInSoftware(), cameraCfg("", "")},
		"the app asked for h264 and is served other": {withoutH264Node, h264},
		"the property names no software encoder":     {notSoftware, h264},
		"the app takes the real device":              {c920RowEncodedInSoftware(), shareCfg("direct")},
	} {
		if got := cameraSoftwareEncoderNotices([]*appconfig.AppConfig{tc.cfg}, []*hwCap{tc.row}); len(got) != 0 {
			t.Errorf("%s: %q", name, got)
		}
		for _, line := range printedCameraNotice(t, []*hwCap{tc.row}, tc.cfg) {
			if strings.Contains(line, "encode") {
				t.Errorf("%s: printed %q", name, line)
			}
		}
	}
}

// Of two cameras in one app, only the one whose node the software encoder
// feeds gets the line.
func TestCameraSoftwareEncoderNotices_OneLineForEachCameraWhoseH264NodeIsEncodedInSoftware(t *testing.T) {
	logi := shareRowAt("Logi H264 (video2)", "/dev/video2", map[string]string{"pixels": "1280x720 YUYV, 30 fps", "native": "1280x720 H264, 30 fps", "h264": "1280x720 H264, 30 fps"})
	logi.Properties[camerashare.KeyStableID] = ""
	brio := shareRowAt("Brio 101 (video4)", "/dev/video4", c920Modes)
	brio.Properties[camerashare.KeyStableID] = ""
	brio.Properties[camerashare.KeyH264Encoder] = "openh264enc (software)"
	got := cameraSoftwareEncoderNotices([]*appconfig.AppConfig{cameraCfg(appconfig.CameraFormatH264, "")}, []*hwCap{c920RowEncodedInSoftware(), logi, brio})
	want := []string{
		c920EncodedSoftware,
		`Camera: Brio 101 (video4): its H.264 is encoded in software (openh264enc) on this device for as long as this app reads its node. "format": "native" or "pixels" run no encoder.`,
	}
	if !slices.Equal(got, want) {
		t.Errorf("got  %q\nwant %q", got, want)
	}
}

func h264CfgAt(preset string, allowlist ...string) *appconfig.AppConfig {
	return &appconfig.AppConfig{Entitlements: []appconfig.Entitlement{
		{Type: appconfig.EntitlementCamera, Allowlist: allowlist, Format: appconfig.CameraFormatH264, Preset: preset},
	}}
}

// c920RowEncodedAtAPreset is the camera's row from an agent that applies a
// stated preset to the software encoder of the camera's h264 node.
func c920RowEncodedAtAPreset() *hwCap {
	row := c920RowEncodedInSoftware()
	row.Properties[camerashare.KeyH264DefaultPreset] = "ultrafast"
	return row
}

const c920EncodedSoftwareAt = `Camera: HD Pro Webcam C920: HD Pro Webc (video0): its H.264 is encoded in software (x264enc), `

const noEncoderOtherwise = ` on this device for as long as this app reads its node. "format": "native" or "pixels" run no encoder.`

func TestCameraSoftwareEncoderNotices_NameThePresetTheEncoderWillRunAt(t *testing.T) {
	rows := []*hwCap{c920RowEncodedAtAPreset()}
	for name, tc := range map[string]struct {
		cfg  *appconfig.AppConfig
		want string
	}{
		"the app states a preset": {
			h264CfgAt("veryfast"),
			c920EncodedSoftwareAt + `at preset veryfast or a faster one another consumer of the camera states,` + noEncoderOtherwise,
		},
		"the app states none": {
			h264CfgAt(""),
			c920EncodedSoftwareAt + `at preset ultrafast or the fastest one a consumer of the camera states,` + noEncoderOtherwise,
		},
		"the entry naming the camera decides": {
			&appconfig.AppConfig{Entitlements: append(h264CfgAt("medium", c920StableID).Entitlements, h264CfgAt("fast", "/dev/video9").Entitlements...)},
			c920EncodedSoftwareAt + `at preset medium or a faster one another consumer of the camera states,` + noEncoderOtherwise,
		},
	} {
		if got := cameraSoftwareEncoderNotices([]*appconfig.AppConfig{tc.cfg}, rows); !slices.Equal(got, []string{tc.want}) {
			t.Errorf("%s:\ngot  %q\nwant %q", name, got, tc.want)
		}
	}
}

// The default the line names is the agent's, which may be built from another
// list than this command line.
func TestCameraSoftwareEncoderNotices_TheDefaultPresetIsTheOneTheAgentReports(t *testing.T) {
	row := c920RowEncodedAtAPreset()
	row.Properties[camerashare.KeyH264DefaultPreset] = "superfast"
	want := c920EncodedSoftwareAt + `at preset superfast or the fastest one a consumer of the camera states,` + noEncoderOtherwise
	if got := cameraSoftwareEncoderNotices([]*appconfig.AppConfig{h264CfgAt("")}, []*hwCap{row}); !slices.Equal(got, []string{want}) {
		t.Errorf("got  %q\nwant %q", got, want)
	}
}

// An agent that says nothing of presets is not said to apply one.
func TestCameraSoftwareEncoderNotices_NameNoPresetForAnAgentThatReportsNone(t *testing.T) {
	for _, cfg := range []*appconfig.AppConfig{h264CfgAt(""), h264CfgAt("veryfast")} {
		if got := cameraSoftwareEncoderNotices([]*appconfig.AppConfig{cfg}, []*hwCap{c920RowEncodedInSoftware()}); !slices.Equal(got, []string{c920EncodedSoftware}) {
			t.Errorf("got  %q\nwant %q", got, c920EncodedSoftware)
		}
	}
}

func TestCameraPresetNotices_AStatedPresetTheDeviceDoesNotApplyIsSaidWithTheAgentsReason(t *testing.T) {
	const why = "this device encodes with nvv4l2h264enc, which takes no preset"
	hardware := c920Row()
	hardware.Properties[camerashare.KeyH264PresetIgnored] = why
	want := []string{`Camera: HD Pro Webcam C920: HD Pro Webc (video0): preset veryfast is not applied: ` + why + `.`}
	if got := cameraPresetNotices([]*appconfig.AppConfig{h264CfgAt("veryfast")}, []*hwCap{hardware}); !slices.Equal(got, want) {
		t.Errorf("got  %q\nwant %q", got, want)
	}
	if got := printedCameraNotice(t, []*hwCap{hardware}, h264CfgAt("veryfast"), h264CfgAt("veryfast", c920StableID)); !slices.Equal(got, append([]string{c920SharedH264}, want...)) {
		t.Errorf("two services printed %q\nwant the share line and %q", got, want)
	}
	if got := cameraPresetNotices([]*appconfig.AppConfig{h264CfgAt("")}, []*hwCap{hardware}); len(got) != 0 {
		t.Errorf("an app that states no preset was told %q", got)
	}
}

// A row that says nothing of presets gets no line: the notice passes on what
// the agent says and infers nothing from what it does not say.
func TestCameraPresetNotices_NothingIsSaidWhereThePresetIsAppliedOrNoH264NodeIsServed(t *testing.T) {
	withoutH264Node := c920RowEncodedAtAPreset()
	delete(withoutH264Node.Properties, camerashare.KeyModeOfFormat(appconfig.CameraFormatH264))
	withoutH264Node.Properties[camerashare.KeyUnavailableOfFormat(appconfig.CameraFormatH264)] = "the camera's own frames are not captured whole"
	for name, tc := range map[string]struct {
		row *hwCap
		cfg *appconfig.AppConfig
	}{
		"the agent applies the preset":                {c920RowEncodedAtAPreset(), h264CfgAt("veryfast")},
		"a row that names its software encoder only":  {c920RowEncodedInSoftware(), h264CfgAt("veryfast")},
		"a row that says nothing of presets":          {c920Row(), h264CfgAt("veryfast")},
		"a row from an agent before stable ids":       {c920RowFromAnAgentBeforeStableIDs(), h264CfgAt("veryfast")},
		"the app asked for h264 and is served other":  {withoutH264Node, h264CfgAt("veryfast")},
		"the preset is stated for another camera":     {c920Row(), &appconfig.AppConfig{Entitlements: append(h264CfgAt("fast", "/dev/video9").Entitlements, h264CfgAt("", c920StableID).Entitlements...)}},
		"the app takes the real device":               {c920Row(), shareCfg("direct")},
		"an agent that reads no node settings at all": {shareRowAt("Brio 101 (video0)", "/dev/video0", map[string]string{"pixels": "1280x720 YUYV, 30 fps"}), h264CfgAt("veryfast")},
	} {
		if got := cameraPresetNotices([]*appconfig.AppConfig{tc.cfg}, []*hwCap{tc.row}); len(got) != 0 {
			t.Errorf("%s: %q", name, got)
		}
	}
}

func TestPrintCameraShareNotice_ThePresetLinesFollowTheEncoderLine(t *testing.T) {
	applied := printedCameraNotice(t, []*hwCap{c920RowEncodedAtAPreset()}, h264CfgAt("veryfast"))
	wantApplied := []string{c920SharedH264, c920EncodedSoftwareAt + `at preset veryfast or a faster one another consumer of the camera states,` + noEncoderOtherwise}
	if !slices.Equal(applied, wantApplied) {
		t.Errorf("printed %q\nwant    %q", applied, wantApplied)
	}
	software := c920RowEncodedInSoftware()
	software.Properties[camerashare.KeyH264Encoder] = "openh264enc (software)"
	software.Properties[camerashare.KeyH264PresetIgnored] = "this device encodes with openh264enc, which takes no preset"
	ignored := printedCameraNotice(t, []*hwCap{software}, h264CfgAt("fast"))
	wantIgnored := []string{
		c920SharedH264,
		`Camera: HD Pro Webcam C920: HD Pro Webc (video0): its H.264 is encoded in software (openh264enc)` + noEncoderOtherwise,
		`Camera: HD Pro Webcam C920: HD Pro Webc (video0): preset fast is not applied: this device encodes with openh264enc, which takes no preset.`,
	}
	if !slices.Equal(ignored, wantIgnored) {
		t.Errorf("printed %q\nwant    %q", ignored, wantIgnored)
	}
}

func TestAgentWithoutNodeSettingsNotice_CoversAStatedPreset(t *testing.T) {
	rows := []*hwCap{shareRowAt("Brio 101 (video0)", "/dev/video0", map[string]string{"pixels": "1280x720 YUYV, 30 fps"})}
	cfg := &appconfig.AppConfig{Entitlements: []appconfig.Entitlement{{Type: appconfig.EntitlementCamera, Preset: "veryfast"}}}
	if got := agentWithoutNodeSettingsNotice([]*appconfig.AppConfig{cfg}, rows); !strings.Contains(got, "does not read a camera entitlement's format, stamp, preset or allowlist") {
		t.Errorf("got %q", got)
	}
}
