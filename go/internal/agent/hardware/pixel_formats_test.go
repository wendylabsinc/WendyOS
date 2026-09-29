package hardware

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.uber.org/zap"

	"github.com/wendylabsinc/wendy/go/internal/agent/camera"
)

const v4l2PixFmtYUYV uint32 = 0x56595559 // 'YUYV'

// fakeVideoNodes points discovery at stand-in /dev/video* files and a fake
// format enumerator keyed by node base name.
func fakeVideoNodes(t *testing.T, formats map[string]func() ([]uint32, error)) *SystemHardwareDiscoverer {
	t.Helper()
	dir := t.TempDir()
	for base := range formats {
		if err := os.WriteFile(filepath.Join(dir, base), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	prevGlob, prevEnum := videoNodeGlob, enumeratePixelFormats
	t.Cleanup(func() { videoNodeGlob, enumeratePixelFormats = prevGlob, prevEnum })
	videoNodeGlob = filepath.Join(dir, "video*")
	enumeratePixelFormats = func(path string) ([]uint32, error) {
		fn, ok := formats[filepath.Base(path)]
		if !ok {
			t.Fatalf("enumerated unexpected node %s", path)
		}
		return fn()
	}

	d := NewSystemHardwareDiscoverer(zap.NewNop())
	d.classifyTransport = func(string) (camera.Transport, string) { return camera.TransportUSB, "uvcvideo" }
	d.enumerateLibcamera = func(context.Context) (map[string]string, error) { return nil, nil }
	return d
}

func camerasByBase(t *testing.T, d *SystemHardwareDiscoverer, want int) map[string]map[string]string {
	t.Helper()
	caps := d.discoverCamera(context.Background())
	if len(caps) != want {
		t.Fatalf("got %d camera capabilities, want %d", len(caps), want)
	}
	out := map[string]map[string]string{}
	for _, c := range caps {
		out[filepath.Base(c.GetDevicePath())] = c.GetProperties()
	}
	return out
}

func TestDiscoverCamera_DepthNodeAdvertisesZ16(t *testing.T) {
	d := fakeVideoNodes(t, map[string]func() ([]uint32, error){
		"video0": func() ([]uint32, error) { return []uint32{v4l2PixFmtZ16, v4l2PixFmtYUYV}, nil },
	})
	props := camerasByBase(t, d, 1)["video0"]
	if got := props["depth"]; got != "z16" {
		t.Errorf("depth = %q, want z16", got)
	}
	if got := props["pixel_formats"]; got != "YUYV,Z16" {
		t.Errorf("pixel_formats = %q, want YUYV,Z16", got)
	}
	// Existing properties are untouched.
	if props["transport"] != "usb" || props["driver"] != "uvcvideo" {
		t.Errorf("transport/driver lost: %v", props)
	}
}

func TestDiscoverCamera_ColourNodeHasNoDepth(t *testing.T) {
	d := fakeVideoNodes(t, map[string]func() ([]uint32, error){
		"video0": func() ([]uint32, error) { return []uint32{v4l2PixFmtYUYV}, nil },
	})
	props := camerasByBase(t, d, 1)["video0"]
	if _, ok := props["depth"]; ok {
		t.Errorf("depth set on a node without Z16: %v", props)
	}
	if got := props["pixel_formats"]; got != "YUYV" {
		t.Errorf("pixel_formats = %q, want YUYV", got)
	}
}

func TestDiscoverCamera_EnumerationErrorLeavesKeysAbsent(t *testing.T) {
	d := fakeVideoNodes(t, map[string]func() ([]uint32, error){
		"video0": func() ([]uint32, error) { return nil, errors.New("inappropriate ioctl for device") },
		"video1": func() ([]uint32, error) { return []uint32{v4l2PixFmtZ16}, nil },
	})
	byBase := camerasByBase(t, d, 2)
	refused := byBase["video0"]
	for _, key := range []string{"depth", "pixel_formats"} {
		if _, ok := refused[key]; ok {
			t.Errorf("%s set on a node that refused enumeration: %v", key, refused)
		}
	}
	if refused["transport"] != "usb" {
		t.Errorf("refusing node lost its transport: %v", refused)
	}
	if byBase["video1"]["depth"] != "z16" {
		t.Errorf("a refusing sibling must not affect the next node: %v", byBase["video1"])
	}

	// Discover as a whole still succeeds and still reports the refusing node.
	caps, err := d.Discover(context.Background(), "camera")
	if err != nil || len(caps) != 2 {
		t.Fatalf("Discover(camera) = %d caps, err %v; want 2, nil", len(caps), err)
	}
}

func TestDiscoverPixelFormatPropertiesSortDedupeAndBound(t *testing.T) {
	var formats []uint32
	// 20 distinct printable codes "A000".."A019" (reversed to prove sorting),
	// one duplicate, and one with the big-endian flag in bit 31.
	for i := 19; i >= 0; i-- {
		name := []byte{'A', '0', byte('0' + i/10), byte('0' + i%10)}
		formats = append(formats, uint32(name[0])|uint32(name[1])<<8|uint32(name[2])<<16|uint32(name[3])<<24)
	}
	formats = append(formats, formats[0], v4l2PixFmtYUYV|1<<31)

	props := map[string]string{}
	addPixelFormatProperties(props, formats)
	names := strings.Split(props["pixel_formats"], ",")
	if len(names) != maxListedPixelFormats {
		t.Fatalf("got %d names, want %d: %v", len(names), maxListedPixelFormats, names)
	}
	if names[0] != "A000" || names[15] != "A015" {
		t.Errorf("not sorted from the start: %v", names)
	}
	if _, ok := props["depth"]; ok {
		t.Errorf("depth set without Z16: %v", props)
	}

	empty := map[string]string{}
	addPixelFormatProperties(empty, nil)
	if len(empty) != 0 {
		t.Errorf("no formats must add no keys: %v", empty)
	}
}

func TestDiscoverFourccNameTrimsPadding(t *testing.T) {
	if got, ok := fourccName(v4l2PixFmtZ16); !ok || got != "Z16" {
		t.Errorf("fourccName(Z16) = %q, %v; want Z16, true", got, ok)
	}
	if got, ok := fourccName(v4l2PixFmtYUYV); !ok || got != "YUYV" {
		t.Errorf("fourccName(YUYV) = %q, %v; want YUYV, true", got, ok)
	}
	if _, ok := fourccName(0); ok {
		t.Error("a zero code must be skipped")
	}
}
