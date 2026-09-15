package ros2camera

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/wendylabsinc/wendy/go/internal/rtps"
)

type fakeLoopback struct {
	mu    sync.Mutex
	paths map[uint32]string
}

func (f *fakeLoopback) EnsureNode(_ context.Context, id uint32, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.paths == nil {
		f.paths = map[uint32]string{}
	}
	f.paths[id] = fmt.Sprintf("/dev/video%d", id)
	return nil
}
func (f *fakeLoopback) NodePath(id uint32) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.paths[id]
	return p, ok
}

type fakeWriter struct {
	frames int
	width  int
	height int
	codec  Codec
	closed int
}

func (w *fakeWriter) WriteFrame(frame Frame) error {
	w.frames++
	w.width, w.height, w.codec = frame.Width, frame.Height, frame.Codec
	return nil
}
func (w *fakeWriter) Close() error {
	w.closed++
	return nil
}

func TestManagerRegistersROS2AndGo2CamerasWithStableIDs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ros2-cameras.json")
	loop := &fakeLoopback{}
	m := NewManager(context.Background(), zap.NewNop(), loop, path, nil)
	t.Cleanup(m.Shutdown)
	p := &participantState{iface: "eth0", domainID: 0}
	m.registerEndpoint(p, rtps.Endpoint{Topic: "rt/frontvideostream", Type: TypeGo2FrontVideo, GUID: rtps.GUID{EntityID: 1}})
	m.registerEndpoint(p, rtps.Endpoint{Topic: "rt/camera/image_raw", Type: TypeImage, GUID: rtps.GUID{EntityID: 2}})

	cameras := m.List()
	if len(cameras) != 2 {
		t.Fatalf("cameras = %+v", cameras)
	}
	if cameras[0].ID != IDBandStart || cameras[0].Name != "Unitree Go2 front camera" || cameras[0].Topic != "/frontvideostream" || cameras[0].Path != "/dev/video128" {
		t.Fatalf("Go2 camera = %+v", cameras[0])
	}

	// A fresh manager reading the registry must retain the topic's device ID.
	m2 := NewManager(context.Background(), zap.NewNop(), &fakeLoopback{}, path, nil)
	t.Cleanup(m2.Shutdown)
	m2.registerEndpoint(p, rtps.Endpoint{Topic: "rt/frontvideostream", Type: TypeGo2FrontVideo, GUID: rtps.GUID{EntityID: 3}})
	got := m2.List()
	if len(got) != 1 || got[0].ID != cameras[0].ID {
		t.Fatalf("reloaded camera = %+v, want stable ID %d", got, cameras[0].ID)
	}
}

func TestManagerPumpsCompressedImageToLoopback(t *testing.T) {
	loop := &fakeLoopback{}
	m := NewManager(context.Background(), zap.NewNop(), loop, filepath.Join(t.TempDir(), "registry.json"), nil)
	t.Cleanup(m.Shutdown)
	writer := &fakeWriter{}
	m.newWriter = func(string) cameraWriter { return writer }
	m.containerUse = true
	guid := rtps.GUID{EntityID: 7}
	p := &participantState{iface: "eth0", domainID: 0}
	m.registerEndpoint(p, rtps.Endpoint{Topic: "rt/camera/compressed", Type: TypeCompressedImage, GUID: guid})

	jpegFrame := testJPEG(t, 5, 4)
	c := newCDRBuilder()
	c.header()
	c.str("jpeg")
	c.bytes(jpegFrame)
	m.handleSample(p, rtps.Sample{Writer: guid, Payload: c.b})
	m.handleSample(p, rtps.Sample{Writer: guid, Payload: c.b})

	if writer.frames != 1 || writer.width != 5 || writer.height != 4 || writer.codec != CodecMJPEG {
		t.Fatalf("writer = %+v", writer)
	}
}

func TestManagerPreservesGo2H264ForLoopback(t *testing.T) {
	loop := &fakeLoopback{}
	m := NewManager(context.Background(), zap.NewNop(), loop, filepath.Join(t.TempDir(), "registry.json"), nil)
	t.Cleanup(m.Shutdown)
	writer := &fakeWriter{}
	m.newWriter = func(string) cameraWriter { return writer }
	m.containerUse = true
	guid := rtps.GUID{EntityID: 8}
	p := &participantState{iface: "eth0", domainID: 0}
	m.registerEndpoint(p, rtps.Endpoint{Topic: "rt/frontvideostream", Type: TypeGo2FrontVideo, GUID: guid})

	c := newCDRBuilder()
	c.u64(42)
	c.u32(360)
	c.bytes([]byte{0, 0, 1, 0x41, 1, 2, 3})
	m.handleSample(p, rtps.Sample{Writer: guid, Payload: c.b})

	if writer.frames != 1 || writer.width != 640 || writer.height != 360 || writer.codec != CodecH264 {
		t.Fatalf("writer = %+v", writer)
	}
}

func TestManagerSkipsDecodeWhenLoopbackNodeIsMissing(t *testing.T) {
	loop := &fakeLoopback{}
	m := NewManager(context.Background(), zap.NewNop(), loop, filepath.Join(t.TempDir(), "registry.json"), nil)
	t.Cleanup(m.Shutdown)
	m.containerUse = true
	guid := rtps.GUID{EntityID: 7}
	p := &participantState{iface: "eth0", domainID: 0, graphKey: "host:eth0"}
	m.registerEndpoint(p, rtps.Endpoint{
		Topic: "rt/camera/compressed", Type: TypeCompressedImage, GUID: guid,
	})
	loop.mu.Lock()
	delete(loop.paths, IDBandStart)
	loop.mu.Unlock()

	m.handleSample(p, rtps.Sample{Writer: guid, Payload: []byte("not CDR")})
	if m.cameras[IDBandStart].loggedError {
		t.Fatal("missing loopback node should skip decoding without logging a decode error")
	}
}

func TestFrameIntervalBudgetsLargeRawFrames(t *testing.T) {
	if got := frameInterval(TypeCompressedImage, largeRawBytes+1); got != minFrameInterval {
		t.Fatalf("compressed interval = %s; want %s", got, minFrameInterval)
	}
	if got := frameInterval(TypeImage, mediumRawBytes+1); got != mediumRawInterval {
		t.Fatalf("medium raw interval = %s; want %s", got, mediumRawInterval)
	}
	if got := frameInterval(TypeImage, largeRawBytes+1); got != largeRawInterval {
		t.Fatalf("large raw interval = %s; want %s", got, largeRawInterval)
	}
}

func TestManagerKeepsHostInterfacesDistinct(t *testing.T) {
	m := NewManager(context.Background(), zap.NewNop(), &fakeLoopback{}, filepath.Join(t.TempDir(), "registry.json"), nil)
	t.Cleanup(m.Shutdown)
	topic := "rt/camera/compressed"
	m.registerEndpoint(&participantState{iface: "eth0", domainID: 0, graphKey: "host:eth0"}, rtps.Endpoint{
		Topic: topic, Type: TypeCompressedImage, GUID: rtps.GUID{EntityID: 1},
	})
	m.registerEndpoint(&participantState{iface: "eth1", domainID: 0, graphKey: "host:eth1"}, rtps.Endpoint{
		Topic: topic, Type: TypeCompressedImage, GUID: rtps.GUID{EntityID: 2},
	})
	if cameras := m.List(); len(cameras) != 2 || cameras[0].ID == cameras[1].ID {
		t.Fatalf("cameras = %+v; want distinct cameras for each host interface", cameras)
	}
}

func TestManagerRejectsUnsafeTopicNames(t *testing.T) {
	m := NewManager(context.Background(), zap.NewNop(), &fakeLoopback{}, filepath.Join(t.TempDir(), "registry.json"), nil)
	t.Cleanup(m.Shutdown)
	m.registerEndpoint(&participantState{iface: "eth0", domainID: 0, graphKey: "host:eth0"}, rtps.Endpoint{
		Topic: "rt/camera\nforged", Type: TypeCompressedImage, GUID: rtps.GUID{EntityID: 1},
	})
	if cameras := m.List(); len(cameras) != 0 {
		t.Fatalf("unsafe topic was registered: %+v", cameras)
	}
}

func TestManagerRediscoveryDoesNotResetSubscription(t *testing.T) {
	m := NewManager(context.Background(), zap.NewNop(), &fakeLoopback{}, filepath.Join(t.TempDir(), "registry.json"), nil)
	t.Cleanup(m.Shutdown)
	p := &participantState{iface: "eth0", domainID: 0}
	first := rtps.GUID{EntityID: 7}
	m.registerEndpoint(p, rtps.Endpoint{Topic: "rt/camera/compressed", Type: TypeCompressedImage, GUID: first})

	cam := m.cameras[IDBandStart]
	cam.subscribed = true
	m.registerEndpoint(p, rtps.Endpoint{Topic: "rt/camera/compressed", Type: TypeCompressedImage, GUID: first})
	if !cam.subscribed {
		t.Fatal("duplicate endpoint announcement reset the active subscription")
	}

	second := rtps.GUID{EntityID: 8}
	m.registerEndpoint(p, rtps.Endpoint{Topic: "rt/camera/compressed", Type: TypeCompressedImage, GUID: second})
	firstKey := cameraWriterKey{source: p, guid: first}
	secondKey := cameraWriterKey{source: p, guid: second}
	if m.byWriter[firstKey] != nil || m.byWriter[secondKey] != cam {
		t.Fatalf("writer index was not replaced: old=%p new=%p camera=%p", m.byWriter[firstKey], m.byWriter[secondKey], cam)
	}
}

func TestManagerRoutesMatchingGUIDsWithinTheirSource(t *testing.T) {
	for _, tc := range []struct {
		name   string
		graph  string
		domain int
	}{
		{name: "isolated apps in one domain", graph: "app-b", domain: 0},
		{name: "different domains", graph: "app-a", domain: 7},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := NewManager(context.Background(), zap.NewNop(), &fakeLoopback{}, filepath.Join(t.TempDir(), "registry.json"), nil)
			t.Cleanup(m.Shutdown)
			writers := map[string]*fakeWriter{}
			m.newWriter = func(path string) cameraWriter {
				w := &fakeWriter{}
				writers[path] = w
				return w
			}
			first := &participantState{iface: "lo", graphKey: "app-a", netnsPID: 100}
			second := &participantState{iface: "lo", graphKey: tc.graph, domainID: tc.domain, netnsPID: 200}
			guid := rtps.GUID{EntityID: 7}
			endpoint := rtps.Endpoint{Topic: "rt/camera/compressed", Type: TypeCompressedImage, GUID: guid}
			m.registerEndpoint(first, endpoint)
			m.registerEndpoint(second, endpoint)
			cameras := m.List()
			if len(cameras) != 2 {
				t.Fatalf("cameras = %+v; want two isolated sources", cameras)
			}
			m.cameras[cameras[0].ID].viewRefs = 1
			m.handleSample(first, compressedCameraSample(t, guid, 5, 4))
			m.handleSample(second, compressedCameraSample(t, guid, 9, 8))
			firstWriter := writers[cameras[0].Path]
			if firstWriter == nil || firstWriter.frames != 1 || firstWriter.width != 5 || firstWriter.height != 4 {
				t.Fatalf("first camera writer = %+v; its source must reach its viewer", firstWriter)
			}
			if w := writers[cameras[1].Path]; w != nil {
				t.Fatalf("camera without a viewer received frames: %+v", w)
			}

			m.cameras[cameras[1].ID].viewRefs = 1
			m.handleSample(second, compressedCameraSample(t, guid, 9, 8))
			secondWriter := writers[cameras[1].Path]
			if secondWriter == nil || secondWriter.frames != 1 || secondWriter.width != 9 || secondWriter.height != 8 {
				t.Fatalf("second camera writer = %+v; its source must reach its viewer", secondWriter)
			}
			if firstWriter.frames != 1 {
				t.Fatalf("second source wrote to the first camera: %+v", firstWriter)
			}
		})
	}
}

func TestManagerRetiredSourceCannotReviveCameraOrStealReplacementFrames(t *testing.T) {
	m := NewManager(context.Background(), zap.NewNop(), &fakeLoopback{}, filepath.Join(t.TempDir(), "registry.json"), nil)
	t.Cleanup(m.Shutdown)
	var writers []*fakeWriter
	m.newWriter = func(string) cameraWriter {
		w := &fakeWriter{}
		writers = append(writers, w)
		return w
	}
	canceled := false
	old := &participantState{iface: "lo", graphKey: "app-a", netnsPID: 100, cancel: func() { canceled = true }}
	m.participants["old"] = old
	guid := rtps.GUID{EntityID: 7}
	endpoint := rtps.Endpoint{Topic: "rt/camera/compressed", Type: TypeCompressedImage, GUID: guid}
	m.registerEndpoint(old, endpoint)
	cam := m.cameras[IDBandStart]
	cam.viewRefs = 1
	m.handleSample(old, compressedCameraSample(t, guid, 5, 4))
	if len(writers) != 1 || writers[0].frames != 1 {
		t.Fatalf("original source did not stream: %+v", writers)
	}
	// Keep the rate limiter from hiding an incorrectly accepted queued frame.
	cam.lastFrame = time.Time{}
	m.stopStaleParticipants(map[string]bool{})
	if !canceled || writers[0].closed != 1 {
		t.Fatalf("retired source canceled=%v writer=%+v", canceled, writers[0])
	}
	if _, ok := m.Get(IDBandStart); ok {
		t.Fatal("retired camera is still available")
	}
	m.registerEndpoint(old, endpoint)
	m.handleSample(old, compressedCameraSample(t, guid, 9, 8))
	if cameras := m.List(); len(cameras) != 0 {
		t.Fatalf("queued discovery revived a retired camera: %+v", cameras)
	}
	if len(writers) != 1 || writers[0].frames != 1 {
		t.Fatalf("queued frame reopened or wrote to retired camera: %+v", writers)
	}

	fresh := &participantState{iface: "lo", graphKey: "app-a", netnsPID: 200}
	m.registerEndpoint(fresh, endpoint)
	if cameras := m.List(); len(cameras) != 1 || cameras[0].ID != IDBandStart {
		t.Fatalf("replacement source changed camera ID: %+v", cameras)
	}
	m.registerEndpoint(old, endpoint)
	m.handleSample(old, compressedCameraSample(t, guid, 9, 8))
	if len(writers) != 1 {
		t.Fatal("retired source reopened the replacement camera")
	}
	m.handleSample(fresh, compressedCameraSample(t, guid, 6, 3))
	if len(writers) != 2 || writers[1].frames != 1 || writers[1].width != 6 || writers[1].height != 3 {
		t.Fatalf("replacement source failed to resume streaming: %+v", writers)
	}
	if writers[0].frames != 1 {
		t.Fatalf("retired writer received replacement frames: %+v", writers[0])
	}
}

func compressedCameraSample(t *testing.T, guid rtps.GUID, width, height int) rtps.Sample {
	t.Helper()
	c := newCDRBuilder()
	c.header()
	c.str("jpeg")
	c.bytes(testJPEG(t, width, height))
	return rtps.Sample{Writer: guid, Payload: c.b}
}
