package framesource

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.uber.org/zap"
)

// The case that matters most: a RealSense attached to an image with no capture
// helper. Reporting nothing would tell the operator their depth camera does not
// exist — the same silence that let a robot read a name off a wall poster.
func TestRealSenseProvider_AttachedCameraWithNoHelperIsListedAndExplained(t *testing.T) {
	p := &RealSenseProvider{
		Logger: zap.NewNop(),
		Detect: func() []string { return []string{"Intel(R) RealSense(TM) Depth Camera 435i"} },
		FindLauncher: func() (Launcher, error) {
			return nil, ErrHelperNotInstalled
		},
	}
	sources, err := p.Sources(context.Background())
	if err != nil {
		t.Fatalf("Sources: %v", err)
	}
	if len(sources) != 1 {
		t.Fatalf("got %d sources, want the attached camera reported as unusable", len(sources))
	}
	desc, err := sources[0].Describe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if desc.GetAvailable() {
		t.Error("a camera with no helper was reported as available")
	}
	reason := desc.GetUnavailableReason()
	for _, want := range []string{"435i", HelperName, "Z16"} {
		if !strings.Contains(reason, want) {
			t.Errorf("reason does not mention %q: %s", want, reason)
		}
	}
	if len(desc.GetProvides()) != 0 {
		t.Errorf("an unusable source claims to provide %v", Slugs(desc.GetProvides()))
	}
}

// No camera and no helper is an ordinary device, not a problem to report.
func TestRealSenseProvider_NoCameraAndNoHelperReportsNothing(t *testing.T) {
	p := &RealSenseProvider{
		Logger:       zap.NewNop(),
		Detect:       func() []string { return nil },
		FindLauncher: func() (Launcher, error) { return nil, ErrHelperNotInstalled },
	}
	sources, err := p.Sources(context.Background())
	if err != nil {
		t.Fatalf("Sources: %v", err)
	}
	if len(sources) != 0 {
		t.Errorf("a device with no RealSense reported %d sources", len(sources))
	}
}

func TestRealSenseProvider_EnumeratesThroughTheHelperWhenItIsInstalled(t *testing.T) {
	launcher := execLauncherForMode(t, "describe")
	p := &RealSenseProvider{
		Logger:   zap.NewNop(),
		Detect:   func() []string { return []string{"Intel(R) RealSense(TM) Depth Camera 435i"} },
		Launcher: launcher,
	}
	sources, err := p.Sources(context.Background())
	if err != nil {
		t.Fatalf("Sources: %v", err)
	}
	if len(sources) != 2 {
		t.Fatalf("got %d sources, want 2 from the helper", len(sources))
	}
	desc, err := sources[0].Describe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !desc.GetAvailable() || desc.GetSource() != "realsense:111" {
		t.Errorf("first source = %+v", desc)
	}
}

// A helper that is installed but cannot see the camera the kernel reports is a
// third state, and it is the one most likely to be a permissions problem. It
// must not be silently reported as "no RealSense here".
func TestRealSenseProvider_HelperThatSeesNothingStillReportsTheAttachedCamera(t *testing.T) {
	p := &RealSenseProvider{
		Logger:   zap.NewNop(),
		Detect:   func() []string { return []string{"Intel(R) RealSense(TM) Depth Camera 435i"} },
		Launcher: &memoryLauncher{}, // describes no cameras at all
	}
	sources, err := p.Sources(context.Background())
	if err != nil {
		t.Fatalf("Sources: %v", err)
	}
	if len(sources) != 1 {
		t.Fatalf("got %d sources", len(sources))
	}
	desc, _ := sources[0].Describe(context.Background())
	if desc.GetAvailable() || !strings.Contains(desc.GetUnavailableReason(), "enumerated no device") {
		t.Errorf("descriptor = %+v", desc)
	}
}

// --- detection ---

func TestDetectRealSense_ReadsTheDriverNamesFromSysfs(t *testing.T) {
	dir := t.TempDir()
	write := func(node, name string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Join(dir, node), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, node, "name"), []byte(name+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// A RealSense presents several nodes under one model name, and an ordinary
	// webcam sits beside it.
	write("video0", "Intel(R) RealSense(TM) Depth Camera 435i Depth")
	write("video1", "Intel(R) RealSense(TM) Depth Camera 435i RGB")
	write("video4", "HD Pro Webcam C920")

	prev := v4l2NameDir
	v4l2NameDir = dir
	t.Cleanup(func() { v4l2NameDir = prev })

	got := DetectRealSense()
	if len(got) != 2 {
		t.Fatalf("detected %v; want the two RealSense nodes and not the webcam", got)
	}
	for _, name := range got {
		if !strings.Contains(name, "RealSense") {
			t.Errorf("detected a non-RealSense node: %q", name)
		}
	}
}

func TestDetectRealSense_MissingSysfsIsNotAnError(t *testing.T) {
	prev := v4l2NameDir
	v4l2NameDir = filepath.Join(t.TempDir(), "absent")
	t.Cleanup(func() { v4l2NameDir = prev })
	if got := DetectRealSense(); got != nil {
		t.Errorf("DetectRealSense = %v on a host with no video4linux class", got)
	}
}
