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

// An agent from before node formats shares every camera as pixels and says
// nothing about the others; the notice does not promise the format asked for.
func TestCameraShareNotice_AnAgentThatDoesNotKnowTheFormatServesPixels(t *testing.T) {
	cfg := &appconfig.AppConfig{Entitlements: []appconfig.Entitlement{{Type: appconfig.EntitlementCamera, Format: appconfig.CameraFormatH264, Stamp: appconfig.CameraStampFrame}}}
	got := cameraShareNotice([]*appconfig.AppConfig{cfg}, []*hwCap{shareRow("Brio 101 (video0)", "1280x720 YUYV, 30 fps")})
	want := `Camera: this app shares Brio 101 (video0) at 1280x720 YUYV, 30 fps, format pixels, stamp frame (asked for h264; serving pixels because the agent on this device does not offer that format). For the real device (its own formats, sizes and controls), set "mode": "direct" on the camera entitlement.`
	if got != want {
		t.Fatalf("got %q\nwant %q", got, want)
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

// c920Row is the camera's row as an agent that reports stable ids lists it.
func c920Row() *hwCap {
	row := shareRowAt("HD Pro Webcam C920: HD Pro Webc (video0)", "/dev/video0", map[string]string{
		"pixels": "1280x720 YUYV, 30 fps", "native": "1280x720 MJPG, 30 fps", "h264": "1280x720 H264, 30 fps"})
	row.Properties[camerashare.KeyStableID] = c920StableID
	return row
}

func TestCameraShareNotice_AnEntryNamingACameraByItsStableIDGivesItsFormatAndStamp(t *testing.T) {
	for _, entry := range []string{c920StableID, strings.TrimPrefix(c920StableID, "by-id:"), "v4l2:/dev/video0"} {
		cfg := &appconfig.AppConfig{Entitlements: []appconfig.Entitlement{
			{Type: appconfig.EntitlementCamera, Allowlist: []string{entry}, Format: appconfig.CameraFormatNative, Stamp: appconfig.CameraStampFrame},
		}}
		got := cameraShareNotice([]*appconfig.AppConfig{cfg}, []*hwCap{c920Row()})
		want := `Camera: this app shares HD Pro Webcam C920: HD Pro Webc (video0) at 1280x720 MJPG, 30 fps, format native, stamp frame. For the real device (its own formats, sizes and controls), set "mode": "direct" on the camera entitlement.`
		if got != want {
			t.Errorf("allowlist %q:\ngot  %q\nwant %q", entry, got, want)
		}
		if lines := cameraAllowlistNotices([]*appconfig.AppConfig{cfg}, []*hwCap{c920Row()}); len(lines) != 0 {
			t.Errorf("allowlist %q names the camera, yet: %q", entry, lines)
		}
	}
}

func TestCameraAllowlistNotices_OneLineForEachEntryThatNamesNoCameraOnTheDevice(t *testing.T) {
	typo := &appconfig.AppConfig{Entitlements: []appconfig.Entitlement{
		{Type: appconfig.EntitlementCamera, Allowlist: []string{c920StableID + "0", "/dev/video0"}, Format: appconfig.CameraFormatNative},
		{Type: appconfig.EntitlementCamera, Allowlist: []string{"HD Pro Webcam C920", "HD Pro Webcam C920: HD Pro Webc"}, Format: appconfig.CameraFormatNative},
	}}
	sameTypo := &appconfig.AppConfig{Entitlements: []appconfig.Entitlement{{Type: appconfig.EntitlementCamera, Allowlist: []string{"HD Pro Webcam C920"}}}}
	direct := &appconfig.AppConfig{Entitlements: []appconfig.Entitlement{{Type: appconfig.EntitlementCamera, Mode: "direct", Allowlist: []string{"/dev/video9"}}}}
	const hint = `, so its entitlement's format and stamp reach none. "wendy device camera list --json" prints each camera's stable_id, path and name.`
	want := []string{
		`Camera: allowlist entry "` + c920StableID + `0" names no camera on this device` + hint,
		`Camera: allowlist entry "HD Pro Webcam C920" names no camera on this device` + hint,
	}
	got := cameraAllowlistNotices([]*appconfig.AppConfig{typo, nil, sameTypo, direct}, []*hwCap{c920Row()})
	if !slices.Equal(got, want) {
		t.Fatalf("got  %q\nwant %q", got, want)
	}
}

type listingHW struct {
	agentpb.WendyAgentServiceClient
	rows []*hwCap
}

func (hw listingHW) ListHardwareCapabilities(context.Context, *agentpb.ListHardwareCapabilitiesRequest, ...grpc.CallOption) (*agentpb.ListHardwareCapabilitiesResponse, error) {
	return &agentpb.ListHardwareCapabilitiesResponse{Capabilities: hw.rows}, nil
}

func TestPrintCameraShareNotice_AnEntryThatNamesNoCameraIsSaidUnderTheShareLine(t *testing.T) {
	cfg := &appconfig.AppConfig{Entitlements: []appconfig.Entitlement{
		{Type: appconfig.EntitlementCamera, Allowlist: []string{"by-id:usb-Typo-video-index0"}, Format: appconfig.CameraFormatNative},
	}}
	conn := &grpcclient.AgentConnection{AgentService: listingHW{rows: []*hwCap{c920Row()}}}
	lines := strings.Split(strings.TrimSpace(captureStderr(t, func() { printCameraShareNotice(context.Background(), conn, cfg) })), "\n")
	if len(lines) != 2 || !strings.Contains(lines[0], "format pixels, stamp time") ||
		!strings.Contains(lines[1], `allowlist entry "by-id:usb-Typo-video-index0" names no camera on this device`) {
		t.Fatalf("printed %q, want the share line with the defaults and one line for the entry", lines)
	}

	// An agent that shares nothing gives no node for an entry to configure.
	conn = &grpcclient.AgentConnection{AgentService: listingHW{rows: []*hwCap{shareRow("Brio 101 (video0)", "")}}}
	if out := captureStderr(t, func() { printCameraShareNotice(context.Background(), conn, cfg) }); out != "" {
		t.Fatalf("an agent that shares no camera printed %q", out)
	}
}
