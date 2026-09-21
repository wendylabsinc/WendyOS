package ros2camera

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"

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
}

func (w *fakeWriter) WriteFrame(frame Frame) error {
	w.frames++
	w.width, w.height, w.codec = frame.Width, frame.Height, frame.Codec
	return nil
}
func (*fakeWriter) Close() error { return nil }

func TestManagerRegistersROS2AndGo2CamerasWithStableIDs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ros2-cameras.json")
	loop := &fakeLoopback{}
	m := NewManager(context.Background(), zap.NewNop(), loop, path, nil, nil)
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
	m2 := NewManager(context.Background(), zap.NewNop(), &fakeLoopback{}, path, nil, nil)
	t.Cleanup(m2.Shutdown)
	m2.registerEndpoint(p, rtps.Endpoint{Topic: "rt/frontvideostream", Type: TypeGo2FrontVideo, GUID: rtps.GUID{EntityID: 3}})
	got := m2.List()
	if len(got) != 1 || got[0].ID != cameras[0].ID {
		t.Fatalf("reloaded camera = %+v, want stable ID %d", got, cameras[0].ID)
	}
}

func TestManagerPumpsCompressedImageToLoopback(t *testing.T) {
	loop := &fakeLoopback{}
	m := NewManager(context.Background(), zap.NewNop(), loop, filepath.Join(t.TempDir(), "registry.json"), nil, nil)
	t.Cleanup(m.Shutdown)
	writer := &fakeWriter{}
	m.newWriter = func(string) cameraWriter { return writer }
	m.containerUse = true
	guid := rtps.GUID{EntityID: 7}
	m.registerEndpoint(&participantState{iface: "eth0", domainID: 0}, rtps.Endpoint{Topic: "rt/camera/compressed", Type: TypeCompressedImage, GUID: guid})

	jpegFrame := testJPEG(t, 5, 4)
	c := newCDRBuilder()
	c.header()
	c.str("jpeg")
	c.bytes(jpegFrame)
	m.handleSample(nil, rtps.Sample{Writer: guid, Payload: c.b})
	m.handleSample(nil, rtps.Sample{Writer: guid, Payload: c.b})

	if writer.frames != 1 || writer.width != 5 || writer.height != 4 || writer.codec != CodecMJPEG {
		t.Fatalf("writer = %+v", writer)
	}
}

func TestManagerPreservesGo2H264ForLoopback(t *testing.T) {
	loop := &fakeLoopback{}
	m := NewManager(context.Background(), zap.NewNop(), loop, filepath.Join(t.TempDir(), "registry.json"), nil, nil)
	t.Cleanup(m.Shutdown)
	writer := &fakeWriter{}
	m.newWriter = func(string) cameraWriter { return writer }
	m.containerUse = true
	guid := rtps.GUID{EntityID: 8}
	m.registerEndpoint(&participantState{iface: "eth0", domainID: 0}, rtps.Endpoint{Topic: "rt/frontvideostream", Type: TypeGo2FrontVideo, GUID: guid})

	c := newCDRBuilder()
	c.u64(42)
	c.u32(360)
	c.bytes([]byte{0, 0, 1, 0x41, 1, 2, 3})
	m.handleSample(nil, rtps.Sample{Writer: guid, Payload: c.b})

	if writer.frames != 1 || writer.width != 640 || writer.height != 360 || writer.codec != CodecH264 {
		t.Fatalf("writer = %+v", writer)
	}
}

func TestManagerSkipsDecodeWhenLoopbackNodeIsMissing(t *testing.T) {
	loop := &fakeLoopback{}
	m := NewManager(context.Background(), zap.NewNop(), loop, filepath.Join(t.TempDir(), "registry.json"), nil, nil)
	t.Cleanup(m.Shutdown)
	m.containerUse = true
	guid := rtps.GUID{EntityID: 7}
	m.registerEndpoint(&participantState{iface: "eth0", domainID: 0, graphKey: "host:eth0"}, rtps.Endpoint{
		Topic: "rt/camera/compressed", Type: TypeCompressedImage, GUID: guid,
	})
	loop.mu.Lock()
	delete(loop.paths, IDBandStart)
	loop.mu.Unlock()

	m.handleSample(nil, rtps.Sample{Writer: guid, Payload: []byte("not CDR")})
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
	m := NewManager(context.Background(), zap.NewNop(), &fakeLoopback{}, filepath.Join(t.TempDir(), "registry.json"), nil, nil)
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
	m := NewManager(context.Background(), zap.NewNop(), &fakeLoopback{}, filepath.Join(t.TempDir(), "registry.json"), nil, nil)
	t.Cleanup(m.Shutdown)
	m.registerEndpoint(&participantState{iface: "eth0", domainID: 0, graphKey: "host:eth0"}, rtps.Endpoint{
		Topic: "rt/camera\nforged", Type: TypeCompressedImage, GUID: rtps.GUID{EntityID: 1},
	})
	if cameras := m.List(); len(cameras) != 0 {
		t.Fatalf("unsafe topic was registered: %+v", cameras)
	}
}

func TestManagerRediscoveryDoesNotResetSubscription(t *testing.T) {
	m := NewManager(context.Background(), zap.NewNop(), &fakeLoopback{}, filepath.Join(t.TempDir(), "registry.json"), nil, nil)
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
	if m.byWriter[writerKey{nil, first}] != nil || m.byWriter[writerKey{nil, second}] != cam {
		t.Fatalf("writer index was not replaced: old=%p new=%p camera=%p", m.byWriter[writerKey{nil, first}], m.byWriter[writerKey{nil, second}], cam)
	}
}

type fakeDiscoveryLease struct {
	mu        sync.Mutex
	iface     string
	endpoints []rtps.Endpoint
	changed   chan struct{}
	samples   chan rtps.Sample
	done      chan struct{}
	subs      map[rtps.GUID]bool
	once      sync.Once
}

func newFakeLease(iface string) *fakeDiscoveryLease {
	return &fakeDiscoveryLease{iface: iface, changed: make(chan struct{}, 1), samples: make(chan rtps.Sample, 4), done: make(chan struct{}), subs: map[rtps.GUID]bool{}}
}
func (l *fakeDiscoveryLease) Interface() string { return l.iface }
func (l *fakeDiscoveryLease) Endpoints() []rtps.Endpoint {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]rtps.Endpoint(nil), l.endpoints...)
}
func (l *fakeDiscoveryLease) Changed() <-chan struct{}    { return l.changed }
func (l *fakeDiscoveryLease) Samples() <-chan rtps.Sample { return l.samples }
func (l *fakeDiscoveryLease) Done() <-chan struct{}       { return l.done }
func (l *fakeDiscoveryLease) Subscribe(ep rtps.Endpoint) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.subs[ep.GUID] = true
	return nil
}
func (l *fakeDiscoveryLease) Unsubscribe(g rtps.GUID) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.subs, g)
}
func (l *fakeDiscoveryLease) Close() error { l.once.Do(func() { close(l.done) }); return nil }

func TestManagerRoutesSharedWriterToEachLogicalCamera(t *testing.T) {
	m := NewManager(context.Background(), zap.NewNop(), &fakeLoopback{}, filepath.Join(t.TempDir(), "registry.json"), nil, nil)
	t.Cleanup(m.Shutdown)
	writers := map[string]*fakeWriter{}
	m.newWriter = func(path string) cameraWriter { w := &fakeWriter{}; writers[path] = w; return w }
	m.containerUse = true
	guid := rtps.GUID{EntityID: 7}
	ep := rtps.Endpoint{GUID: guid, Topic: "rt/camera/compressed", Type: TypeCompressedImage}
	leases := []*fakeDiscoveryLease{newFakeLease("lo"), newFakeLease("lo"), newFakeLease("eth0")}
	for i, l := range leases {
		m.registerEndpoint(&participantState{participant: l, iface: l.iface, graphKey: fmt.Sprintf("app%d", i)}, ep)
	}
	c := newCDRBuilder()
	c.header()
	c.str("jpeg")
	c.bytes(testJPEG(t, 5, 4))
	for _, l := range leases {
		m.handleSample(l, rtps.Sample{Writer: guid, Payload: c.b})
	}
	if len(writers) != 3 || len(m.List()) != 3 {
		t.Fatalf("writers=%d cameras=%d", len(writers), len(m.List()))
	}
	for _, w := range writers {
		if w.frames != 1 {
			t.Fatalf("writer=%+v", w)
		}
	}
	// Disposal in one logical scope must leave other mappings and subscriptions.
	m.syncEndpoints(&participantState{participant: leases[0]})
	if len(m.List()) != 2 {
		t.Fatal("disposal affected unrelated camera")
	}
	if len(leases[0].subs) != 0 || len(leases[1].subs) != 1 {
		t.Fatal("disposal affected another lease's subscription")
	}
}

func TestManagerReconcilesHostAndAppCoverageAndRetainsLeasesOnErrors(t *testing.T) {
	graphs := []Graph{
		{Key: "app0", InstanceKey: "container0", NetworkNamespacePID: 10},
		{Key: "app1", InstanceKey: "container1", NetworkNamespacePID: 11},
		{Key: "app2", InstanceKey: "container2", NetworkNamespacePID: 12},
		{Key: "app3", InstanceKey: "container3", NetworkNamespacePID: 13, DomainID: 7},
		{Key: "isolated", InstanceKey: "container4", NetworkNamespacePID: 14},
	}
	var enumerateErr error
	m := NewManager(context.Background(), zap.NewNop(), nil, filepath.Join(t.TempDir(), "registry.json"), func(context.Context) ([]Graph, error) { return graphs, enumerateErr }, nil)
	t.Cleanup(m.Shutdown)
	m.hostInterfaces = func() ([]string, error) { return []string{"eth0", "eth1"}, nil }
	m.graphInterfaces = func(cfg rtps.Config) ([]string, error) {
		if cfg.NetworkNamespacePID == 14 {
			return []string{"lo", "veth0"}, nil
		}
		return []string{"lo", "eth0", "eth1"}, nil
	}
	var leases []*fakeDiscoveryLease
	m.acquire = func(ctx context.Context, cfg rtps.Config) (discoveryLease, error) {
		l := newFakeLease(cfg.Interface)
		leases = append(leases, l)
		return l, nil
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() { m.Refresh(context.Background()) })
	}
	wg.Wait()
	if len(leases) != 16 || len(m.participants) != 16 {
		t.Fatalf("leases=%d participants=%d; want 2 host + 12 shared-host app + 2 isolated", len(leases), len(m.participants))
	}
	if m.participants[participantKey("lo", 7, 13, "container3")] == nil {
		t.Fatal("nonzero domain loopback coverage missing")
	}
	if m.participants[participantKey("veth0", 0, 14, "container4")] == nil {
		t.Fatal("isolated interface missing")
	}
	enumerateErr = errors.New("containerd unavailable")
	graphs = nil
	m.Refresh(context.Background())
	if len(m.participants) != 16 {
		t.Fatal("enumeration failure removed active leases")
	}
	enumerateErr = nil
	m.Refresh(context.Background())
	if len(m.participants) != 2 {
		t.Fatal("successful reconciliation did not release app leases")
	}
	for _, l := range leases[2:] {
		select {
		case <-l.Done():
		default:
			t.Fatal("obsolete lease not closed")
		}
	}
}

func TestManagerRestartPreservesCameraIDAndRejectsStaleEvents(t *testing.T) {
	m := NewManager(context.Background(), zap.NewNop(), nil, filepath.Join(t.TempDir(), "registry.json"), nil, nil)
	t.Cleanup(m.Shutdown)
	oldLease, newLease := newFakeLease("lo"), newFakeLease("lo")
	old := &participantState{participant: oldLease, iface: "lo", graphKey: "app", cancel: func() {}}
	m.participants["old"] = old
	ep := rtps.Endpoint{GUID: rtps.GUID{EntityID: 7}, Topic: "rt/camera", Type: TypeImage}
	m.registerEndpoint(old, ep)
	id := m.List()[0].ID
	m.stopStaleParticipants(nil)
	m.registerEndpoint(old, ep)
	if len(m.List()) != 0 {
		t.Fatal("late event revived stopped participant")
	}
	newState := &participantState{participant: newLease, iface: "lo", graphKey: "app"}
	ep.GUID.EntityID++
	m.registerEndpoint(newState, ep)
	if cameras := m.List(); len(cameras) != 1 || cameras[0].ID != id {
		t.Fatalf("restart changed camera ID: %+v", cameras)
	}
}

func TestManagerSharedGraphSurvivesSelectedContainerDisappearance(t *testing.T) {
	m := NewManager(context.Background(), zap.NewNop(), nil, filepath.Join(t.TempDir(), "registry.json"), nil, nil)
	t.Cleanup(m.Shutdown)
	a, b := newFakeLease("lo"), newFakeLease("lo")
	ep := rtps.Endpoint{GUID: rtps.GUID{EntityID: 7}, Topic: "rt/camera", Type: TypeImage}
	a.endpoints = []rtps.Endpoint{ep}
	b.endpoints = []rtps.Endpoint{ep}
	first := &participantState{participant: a, iface: "lo", graphKey: "app", cancel: func() {}}
	second := &participantState{participant: b, iface: "lo", graphKey: "app", cancel: func() {}}
	m.participants["first"], m.participants["second"] = first, second
	m.syncEndpoints(first)
	m.syncEndpoints(second)
	id := m.List()[0].ID
	m.stopStaleParticipants(map[string]bool{"second": true})
	if cams := m.List(); len(cams) != 1 || cams[0].ID != id || m.cameras[id].participant != b {
		t.Fatal("retiring selected container lost shared camera")
	}
}

func TestManagerOneGraphEnumerationFailureDoesNotPinOtherStaleLeases(t *testing.T) {
	// A container whose namespace cannot be enumerated keeps the coverage it
	// already has, but must not stop every other stale participant from being
	// released until the agent restarts.
	graphs := []Graph{
		{Key: "app0", InstanceKey: "container0", NetworkNamespacePID: 10},
		{Key: "app1", InstanceKey: "container1", NetworkNamespacePID: 11},
	}
	m := NewManager(context.Background(), zap.NewNop(), nil, filepath.Join(t.TempDir(), "registry.json"), func(context.Context) ([]Graph, error) { return graphs, nil }, nil)
	t.Cleanup(m.Shutdown)
	m.hostInterfaces = func() ([]string, error) { return []string{"eth0"}, nil }
	var brokenPID uint32
	m.graphInterfaces = func(cfg rtps.Config) ([]string, error) {
		if cfg.NetworkNamespacePID == brokenPID {
			return nil, errors.New("setns: operation not permitted")
		}
		return []string{"lo"}, nil
	}
	leases := map[uint32]*fakeDiscoveryLease{}
	m.acquire = func(ctx context.Context, cfg rtps.Config) (discoveryLease, error) {
		l := newFakeLease(cfg.Interface)
		leases[cfg.NetworkNamespacePID] = l
		return l, nil
	}
	m.Refresh(context.Background())
	if len(m.participants) != 3 {
		t.Fatalf("participants=%d; want host + two app loopbacks", len(m.participants))
	}
	// container0 stops; container1 still runs but its namespace can no longer be entered.
	graphs = graphs[1:]
	brokenPID = 11
	m.Refresh(context.Background())
	if m.participants[participantKey("lo", 0, 10, "container0")] != nil {
		t.Fatal("a stale lease survived because another graph failed to enumerate")
	}
	select {
	case <-leases[10].Done():
	default:
		t.Fatal("stale lease not closed")
	}
	if m.participants[participantKey("lo", 0, 11, "container1")] == nil {
		t.Fatal("the failing graph lost the coverage it already had")
	}
}
