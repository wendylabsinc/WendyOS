package hardware

import (
	"context"
	"errors"
	"testing"

	"go.uber.org/zap"

	"github.com/wendylabsinc/wendy/go/internal/agent/audio"
	"github.com/wendylabsinc/wendy/go/internal/agent/camera"
)

func TestSystemHardwareDiscoverer_Discover(t *testing.T) {
	logger := zap.NewNop()
	d := NewSystemHardwareDiscoverer(logger)

	caps, err := d.Discover(context.Background(), "")
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}

	// On macOS, most Linux sysfs paths won't exist, so we may get zero results.
	// The test verifies that the function runs without error.
	t.Logf("Discovered %d hardware capabilities", len(caps))
}

func TestSystemHardwareDiscoverer_CategoryFilter(t *testing.T) {
	logger := zap.NewNop()
	d := NewSystemHardwareDiscoverer(logger)

	// Request only "gpu" category.
	caps, err := d.Discover(context.Background(), "gpu")
	if err != nil {
		t.Fatalf("Discover with filter: %v", err)
	}

	// Verify all returned capabilities are in the "gpu" category.
	for _, cap := range caps {
		if cap.Category != "gpu" {
			t.Errorf("expected category gpu, got %q", cap.Category)
		}
	}
}

func TestSystemHardwareDiscoverer_UnknownCategory(t *testing.T) {
	logger := zap.NewNop()
	d := NewSystemHardwareDiscoverer(logger)

	caps, err := d.Discover(context.Background(), "nonexistent")
	if err != nil {
		t.Fatalf("Discover with unknown filter: %v", err)
	}

	if len(caps) != 0 {
		t.Errorf("expected 0 results for unknown category, got %d", len(caps))
	}
}

func TestSystemHardwareDiscoverer_AllCategories(t *testing.T) {
	logger := zap.NewNop()
	d := NewSystemHardwareDiscoverer(logger)

	categories := []string{"gpu", "usb", "i2c", "spi", "gpio", "camera", "audio", "network", "storage"}
	for _, cat := range categories {
		caps, err := d.Discover(context.Background(), cat)
		if err != nil {
			t.Errorf("Discover(%q): %v", cat, err)
			continue
		}
		for _, cap := range caps {
			if cap.Category != cat {
				t.Errorf("category %q: got capability with category %q", cat, cap.Category)
			}
		}
		t.Logf("  %s: %d capabilities", cat, len(caps))
	}
}

func TestDiscoverCamera_TransportPropertyUSB(t *testing.T) {
	logger := zap.NewNop()
	d := NewSystemHardwareDiscoverer(logger)
	// Force classifier to return USB for any base name.
	d.classifyTransport = func(base string) (camera.Transport, string) {
		return camera.TransportUSB, "uvcvideo"
	}
	d.enumerateLibcamera = func(context.Context) (map[string]string, error) { return nil, nil }

	// We can't fabricate /dev/video* nodes in tests, so call discoverCamera
	// directly on whatever the host happens to have. If the host has none,
	// the assertions only run when caps != nil.
	caps := d.discoverCamera(context.Background())
	for _, c := range caps {
		if got := c.GetProperties()["transport"]; got != "usb" {
			t.Errorf("expected transport=usb, got %q", got)
		}
		if got := c.GetProperties()["driver"]; got != "uvcvideo" {
			t.Errorf("expected driver=uvcvideo, got %q", got)
		}
	}
}

func TestDiscoverCamera_TransportPropertyCSI_WithLibcameraID(t *testing.T) {
	logger := zap.NewNop()
	d := NewSystemHardwareDiscoverer(logger)
	d.classifyTransport = func(base string) (camera.Transport, string) {
		return camera.TransportCSI, "tegra-capture-vi"
	}
	d.enumerateLibcamera = func(context.Context) (map[string]string, error) {
		return map[string]string{"/base/cam@1a": "Sensor"}, nil
	}

	caps := d.discoverCamera(context.Background())
	if len(caps) == 0 {
		t.Skip("no /dev/video* on this host; CSI assertions skipped")
	}
	if len(caps) > 1 {
		// Ambiguous case — id must remain unset on every cap.
		for _, c := range caps {
			if _, ok := c.GetProperties()["libcamera_id"]; ok {
				t.Errorf("ambiguous mapping must not populate libcamera_id: %v", c.GetProperties())
			}
		}
		return
	}
	if got := caps[0].GetProperties()["transport"]; got != "csi" {
		t.Errorf("expected transport=csi, got %q", got)
	}
	if got := caps[0].GetProperties()["libcamera_id"]; got != "/base/cam@1a" {
		t.Errorf("expected libcamera_id=/base/cam@1a, got %q", got)
	}
}

func TestDiscoverAudio(t *testing.T) {
	// A Bluetooth speaker (the default sink) and a USB microphone. The speaker
	// has no ALSA card, so reading /proc/asound/cards never found it.
	orig := audio.DumpRun
	t.Cleanup(func() { audio.DumpRun = orig })
	audio.DumpRun = func(context.Context) ([]byte, error) {
		return []byte(`[
		  {
		    "id": 30, "type": "PipeWire:Interface:Metadata",
		    "props": { "metadata.name": "default" },
		    "metadata": [
		      { "key": "default.audio.sink", "value": { "name": "bluez_output.78_2B_64_76_F3_CE.1" } }
		    ]
		  },
		  {
		    "id": 55, "type": "PipeWire:Interface:Node",
		    "info": { "props": {
		      "media.class": "Audio/Source",
		      "node.name": "alsa_input.usb-046d_C920",
		      "node.description": "HD Pro Webcam C920"
		    } }
		  },
		  {
		    "id": 43, "type": "PipeWire:Interface:Node",
		    "info": { "props": {
		      "media.class": "Audio/Sink",
		      "node.name": "bluez_output.78_2B_64_76_F3_CE.1",
		      "node.description": "Bose Revolve II SoundLink"
		    } }
		  }
		]`), nil
	}

	caps := NewSystemHardwareDiscoverer(zap.NewNop()).discoverAudio(context.Background())
	if len(caps) != 2 {
		t.Fatalf("got %d capabilities, want 2", len(caps))
	}

	// Sorted by node ID, so the speaker comes first.
	speaker := caps[0]
	if speaker.GetCategory() != "audio" {
		t.Errorf("category = %q", speaker.GetCategory())
	}
	if speaker.GetDevicePath() != "bluez_output.78_2B_64_76_F3_CE.1" {
		t.Errorf("device = %q, want the PipeWire node name", speaker.GetDevicePath())
	}
	if speaker.GetDescription() != "Bose Revolve II SoundLink" {
		t.Errorf("description = %q", speaker.GetDescription())
	}
	want := map[string]string{"node_id": "43", "direction": "sink", "default": "true"}
	for k, v := range want {
		if got := speaker.GetProperties()[k]; got != v {
			t.Errorf("property %s = %q, want %q", k, got, v)
		}
	}

	mic := caps[1]
	if mic.GetProperties()["direction"] != "source" {
		t.Errorf("mic direction = %q", mic.GetProperties()["direction"])
	}
	// Only the current default carries the marker.
	if _, ok := mic.GetProperties()["default"]; ok {
		t.Error("non-default device marked as default")
	}
}

func TestDiscoverAudioNoSession(t *testing.T) {
	// No PipeWire session: report nothing rather than failing the whole
	// hardware listing.
	orig := audio.DumpRun
	t.Cleanup(func() { audio.DumpRun = orig })
	audio.DumpRun = func(context.Context) ([]byte, error) {
		return nil, errors.New("no PipeWire session found")
	}

	if caps := NewSystemHardwareDiscoverer(zap.NewNop()).discoverAudio(context.Background()); caps != nil {
		t.Errorf("got %d capabilities, want none", len(caps))
	}
}

type fakeShareReporter struct {
	nodes  map[string]bool
	report map[string]map[string]string
	calls  int
}

func (f *fakeShareReporter) IsShareNode(path string) bool { return f.nodes[path] }
func (f *fakeShareReporter) CameraShareReport(context.Context) map[string]map[string]string {
	f.calls++
	return f.report
}

func TestDiscoverCamera_SkipsShareNodesAndAddsShareProperties(t *testing.T) {
	d := NewSystemHardwareDiscoverer(zap.NewNop())
	d.classifyTransport = func(string) (camera.Transport, string) { return camera.TransportUSB, "uvcvideo" }
	d.enumerateLibcamera = func(context.Context) (map[string]string, error) { return nil, nil }
	d.listVideoNodes = func() ([]string, error) { return []string{"/dev/video0", "/dev/video254"}, nil }

	rep := &fakeShareReporter{
		nodes: map[string]bool{"/dev/video254": true},
		report: map[string]map[string]string{"/dev/video0": {
			"share_mode":  "1280x720 YUYV, 30 fps",
			"shared_with": "cv-a, cv-b",
			"share_state": "cv-a (reading), cv-b (idle)",
		}},
	}
	d.SetCameraShareReporter(rep)

	caps := d.discoverCamera(context.Background())
	if len(caps) != 1 || caps[0].GetDevicePath() != "/dev/video0" {
		t.Fatalf("want only /dev/video0, got %v", caps)
	}
	p := caps[0].GetProperties()
	if p["share_mode"] != "1280x720 YUYV, 30 fps" || p["shared_with"] != "cv-a, cv-b" || p["share_state"] != "cv-a (reading), cv-b (idle)" {
		t.Errorf("share properties not merged: %v", p)
	}
	if p["transport"] != "usb" {
		t.Errorf("transport lost: %v", p)
	}
	if rep.calls != 1 {
		t.Errorf("reporter called %d times, want 1", rep.calls)
	}

	d.SetCameraShareReporter(nil)
	if caps = d.discoverCamera(context.Background()); len(caps) != 2 {
		t.Errorf("nil reporter must list every node, got %d", len(caps))
	}
}
