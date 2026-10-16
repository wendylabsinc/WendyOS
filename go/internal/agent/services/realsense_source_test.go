package services

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.uber.org/zap"

	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
)

// fakeRealSenseSysfs builds the minimal sysfs shape Detect needs for one
// D435i: depth on interface 00, colour on 03.
func fakeRealSenseSysfs(t *testing.T, pid string) string {
	t.Helper()
	root := t.TempDir()
	devDir := filepath.Join(root, "sys/devices/usb1/1-4")
	classDir := filepath.Join(root, "sys/class/video4linux")
	write := func(path, content string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(devDir, "idVendor"), "8086")
	write(filepath.Join(devDir, "idProduct"), pid)
	write(filepath.Join(devDir, "serial"), "337122071091")
	for node, iface := range map[string]string{"video0": "00", "video4": "03"} {
		ifaceDir := filepath.Join(devDir, "1-4:1."+iface)
		write(filepath.Join(ifaceDir, "bInterfaceNumber"), iface)
		nodeDir := filepath.Join(ifaceDir, "video4linux", node)
		write(filepath.Join(nodeDir, "name"), "Intel(R) RealSense(TM) Depth Camera 435i")
		if err := os.Symlink(ifaceDir, filepath.Join(nodeDir, "device")); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(classDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(nodeDir, filepath.Join(classDir, node)); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// With no live camera behind /dev/video0, the calibration read fails — and the
// source must be LISTED as unavailable with the reason, never hidden. That is
// the native analogue of the old "helper not installed" honesty, with nothing
// left to install.
func TestRealSenseProvider_ListsUncalibratableCameraAsUnavailable(t *testing.T) {
	root := fakeRealSenseSysfs(t, "0b3a")
	p := NewRealSenseProvider(zap.NewNop(), &VideoService{logger: zap.NewNop()}, root)
	sources, err := p.Sources(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(sources) != 1 {
		t.Fatalf("got %d sources, want 1", len(sources))
	}
	desc, err := sources[0].Describe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if desc.GetSource() != "realsense:337122071091" {
		t.Errorf("source = %q", desc.GetSource())
	}
	if desc.GetAvailable() {
		t.Fatal("a camera whose calibration cannot be read is listed as available")
	}
	if !strings.Contains(desc.GetUnavailableReason(), "calibration could not be read") {
		t.Errorf("reason = %q", desc.GetUnavailableReason())
	}
}

func TestRealSenseProvider_NoCameraMeansNoSources(t *testing.T) {
	p := NewRealSenseProvider(zap.NewNop(), &VideoService{logger: zap.NewNop()}, t.TempDir())
	sources, err := p.Sources(context.Background())
	if err != nil || len(sources) != 0 {
		t.Fatalf("got %d sources, err %v; a device without a RealSense is not a problem to report", len(sources), err)
	}
}

func TestDepthModeForAspectRatios(t *testing.T) {
	for _, tc := range []struct{ cw, ch, dw, dh uint32 }{
		{1280, 720, 848, 480},
		{1920, 1080, 848, 480},
		{640, 480, 640, 480},
	} {
		if dw, dh := depthModeFor(tc.cw, tc.ch); dw != tc.dw || dh != tc.dh {
			t.Errorf("depthModeFor(%dx%d) = %dx%d, want %dx%d", tc.cw, tc.ch, dw, dh, tc.dw, tc.dh)
		}
	}
}

// The unrecognised-model honesty: it enumerates, offers measured intrinsics
// only, and says why depth is off — new hardware fails visibly, not wrongly.
func TestRealSenseProvider_UnknownModelDisablesDepthByName(t *testing.T) {
	root := fakeRealSenseSysfs(t, "0bff")
	p := NewRealSenseProvider(zap.NewNop(), &VideoService{logger: zap.NewNop()}, root)
	sources, err := p.Sources(context.Background())
	if err != nil || len(sources) != 1 {
		t.Fatalf("sources = %d, err %v", len(sources), err)
	}
	desc, err := sources[0].Describe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// Calibration read fails in this environment either way; the point this
	// test pins is that an unknown PID is enumerated rather than skipped.
	if desc.GetSource() != "realsense:337122071091" || desc.GetKind() != "realsense" {
		t.Errorf("unknown model vanished from the listing: %+v", desc)
	}
	for _, p := range desc.GetProvides() {
		if p == agentpbv2.FrameRequirement_FRAME_REQUIREMENT_ALIGNED_DEPTH {
			t.Error("an unrecognised model must not promise aligned depth")
		}
	}
}
