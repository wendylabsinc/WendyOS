package ros2camera

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/wendylabsinc/wendy/go/internal/rtps"
)

const (
	reconcileInterval = time.Minute
	firstFrameTimeout = 15 * time.Second
	minFrameInterval  = 30 * time.Millisecond
	mediumRawInterval = 60 * time.Millisecond
	largeRawInterval  = 100 * time.Millisecond
	maxTopicLength    = 255
	mediumRawBytes    = 2 * 1024 * 1024
	largeRawBytes     = 4 * 1024 * 1024
)

// Loopback is the shared v4l2loopback control surface owned by ipcam.Loopback.
type Loopback interface {
	EnsureNode(ctx context.Context, id uint32, label string) error
	NodePath(id uint32) (string, bool)
}

// Graph identifies an app-local ROS 2 graph. Participants are created inside
// NetworkNamespacePID for both loopback-only and externally reachable ROS 2
// discovery, without exposing either graph to the host namespace.
type Graph struct {
	Key                 string
	InstanceKey         string
	DomainID            int
	NetworkNamespacePID uint32
	HostNetwork         bool
	// Verify confirms that InstanceKey still owns NetworkNamespacePID after a
	// stable namespace handle is captured but before it is entered, and once
	// more before discovery starts.
	Verify func(context.Context) bool
}

type GraphSource func(context.Context) ([]Graph, error)

type cameraWriter interface {
	WriteFrame(frame Frame) error
	Close() error
}

type Camera struct {
	ID        uint32
	Name      string
	Topic     string
	Type      string
	DomainID  int
	Interface string
	Path      string
}

type discoveryLease interface {
	Endpoints() []rtps.Endpoint
	Changed() <-chan struct{}
	Samples() <-chan rtps.Sample
	Done() <-chan struct{}
	Interface() string
	Subscribe(rtps.Endpoint) error
	Unsubscribe(rtps.GUID)
	Close() error
}

type writerKey struct {
	lease discoveryLease
	guid  rtps.GUID
}

type participantState struct {
	participant discoveryLease
	stopped     bool
	cancel      context.CancelFunc
	iface       string
	domainID    int
	netnsPID    uint32
	graphKey    string
}

type cameraState struct {
	Camera
	pumpMu      sync.Mutex
	endpoint    rtps.Endpoint
	participant discoveryLease
	subscribed  bool
	ready       chan struct{}
	readyClosed bool
	writer      cameraWriter
	loggedError bool
	active      bool
	viewRefs    int
	lastFrame   time.Time
}

// Manager owns DDS discovery and the frame pumps feeding ROS 2 loopback nodes.
type Manager struct {
	ctx       context.Context
	cancel    context.CancelFunc
	logger    *zap.Logger
	loopback  Loopback
	graphs    GraphSource
	registry  *registry
	newWriter func(string) cameraWriter

	reconcileMu     sync.Mutex
	pool            *rtps.Pool
	ownPool         bool
	hostInterfaces  func() ([]string, error)
	graphInterfaces func(rtps.Config) ([]string, error)
	acquire         func(context.Context, rtps.Config) (discoveryLease, error)
	mu              sync.Mutex
	participants    map[string]*participantState
	cameras         map[uint32]*cameraState
	byKey           map[string]*cameraState
	byWriter        map[writerKey]*cameraState
	containerUse    bool
	started         bool
	wg              sync.WaitGroup
}

func NewManager(ctx context.Context, logger *zap.Logger, loopback Loopback, registryPath string, graphs GraphSource, pool *rtps.Pool) *Manager {
	ctx, cancel := context.WithCancel(ctx)
	r := newRegistry(registryPath)
	if err := r.load(); err != nil {
		logger.Warn("loading ROS 2 camera registry failed", zap.Error(err))
	}
	ownPool := pool == nil
	if ownPool {
		pool = rtps.NewPool()
	}
	return &Manager{
		ctx: ctx, cancel: cancel, logger: logger, loopback: loopback, graphs: graphs,
		registry: r, newWriter: newFrameWriter, pool: pool, ownPool: ownPool,
		hostInterfaces: rtps.HostInterfaces, graphInterfaces: rtps.DiscoveryInterfaces,
		acquire:      func(ctx context.Context, cfg rtps.Config) (discoveryLease, error) { return pool.Acquire(ctx, cfg) },
		participants: map[string]*participantState{}, cameras: map[uint32]*cameraState{},
		byKey: map[string]*cameraState{}, byWriter: map[writerKey]*cameraState{},
	}
}

func (m *Manager) Start() {
	m.reconcileMu.Lock()
	defer m.reconcileMu.Unlock()
	if m.ctx.Err() != nil {
		return
	}
	m.mu.Lock()
	if m.started {
		m.mu.Unlock()
		return
	}
	m.started = true
	m.mu.Unlock()
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		m.reconcile(m.ctx)
		ticker := time.NewTicker(reconcileInterval)
		defer ticker.Stop()
		for {
			select {
			case <-m.ctx.Done():
				return
			case <-ticker.C:
				m.reconcile(m.ctx)
			}
		}
	}()
}

func (m *Manager) Refresh(ctx context.Context) { m.reconcile(ctx) }

func (m *Manager) reconcile(ctx context.Context) {
	m.reconcileMu.Lock()
	defer m.reconcileMu.Unlock()
	if m.ctx.Err() != nil || ctx.Err() != nil {
		return
	}
	desired := map[string]bool{}
	// Domain zero on physical wired interfaces covers robot-native graphs,
	// including the Go2's /frontvideostream publisher.
	// SECURITY: Domain 0 on these interfaces is intentionally a trusted robot-LAN
	// boundary because standard ROS 2 discovery has no source authentication.
	// The operator-facing docs require untrusted peers to be segmented away.
	ifaces, ifaceErr := m.hostInterfaces()
	for _, iface := range ifaces {
		desired[participantKey(iface, 0, 0, "host")] = true
		m.ensureParticipant(iface, 0, 0, "host:"+iface, "host", nil)
	}
	complete := ifaceErr == nil
	if m.graphs != nil {
		if found, err := m.graphs(ctx); err == nil {
			for _, graph := range found {
				if graph.DomainID >= 0 && graph.DomainID <= 232 && graph.NetworkNamespacePID != 0 {
					if graph.HostNetwork {
						// Reuse host camera identities across app restarts.
						for _, iface := range append([]string{"lo"}, ifaces...) {
							desired[participantKey(iface, graph.DomainID, 0, "host")] = true
							m.ensureParticipant(iface, graph.DomainID, 0, "host:"+iface, "host", nil)
						}
						continue
					}
					var verify func() bool
					if graph.Verify != nil {
						verify = func() bool { return graph.Verify(ctx) }
					}
					ifaces, err := m.graphInterfaces(rtps.Config{NetworkNamespacePID: graph.NetworkNamespacePID, VerifyNetworkNamespace: verify})
					if err != nil {
						// Keep what this graph already has, but let every other
						// stale participant go: one container whose namespace
						// cannot be entered must not pin the rest until restart.
						m.retainGraph(desired, graph)
						m.logger.Debug("enumerating ROS 2 camera interfaces failed", zap.String("instance", graph.InstanceKey), zap.Uint32("network_namespace_pid", graph.NetworkNamespacePID), zap.Error(err))
						continue
					}
					for _, iface := range ifaces {
						desired[participantKey(iface, graph.DomainID, graph.NetworkNamespacePID, graph.InstanceKey)] = true
						m.ensureParticipant(iface, graph.DomainID, graph.NetworkNamespacePID, graph.Key, graph.InstanceKey, graph.Verify)
					}
				}
			}
		} else {
			complete = false
			m.logger.Debug("listing ROS 2 camera graphs failed", zap.Error(err))
		}
	}
	if complete {
		m.stopStaleParticipants(desired)
	}
}

// retainGraph marks the participants already serving graph as desired.
func (m *Manager) retainGraph(desired map[string]bool, graph Graph) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for key, state := range m.participants {
		if state.graphKey == graph.Key && state.netnsPID == graph.NetworkNamespacePID {
			desired[key] = true
		}
	}
}

func participantKey(iface string, domain int, netnsPID uint32, instanceKey string) string {
	return fmt.Sprintf("%s:%d:%s:%d", instanceKey, netnsPID, iface, domain)
}

func (m *Manager) ensureParticipant(iface string, domain int, netnsPID uint32, graphKey, instanceKey string, verify func(context.Context) bool) {
	key := participantKey(iface, domain, netnsPID, instanceKey)
	m.mu.Lock()
	if _, ok := m.participants[key]; ok {
		m.mu.Unlock()
		return
	}
	m.mu.Unlock()
	var verifyNamespace func() bool
	if verify != nil {
		verifyNamespace = func() bool { return verify(m.ctx) }
	}
	participantCtx, cancel := context.WithCancel(m.ctx)
	p, err := m.acquire(participantCtx, rtps.Config{
		DomainID: domain, Interface: iface, NetworkNamespacePID: netnsPID,
		VerifyNetworkNamespace: verifyNamespace,
	})
	if err != nil {
		cancel()
		m.logger.Debug("starting ROS 2 camera discovery failed", zap.String("interface", iface), zap.Int("domain_id", domain), zap.Uint32("network_namespace_pid", netnsPID), zap.Error(err))
		return
	}
	if verify != nil && !verify(m.ctx) {
		cancel()
		p.Close() //nolint:errcheck
		m.logger.Debug("discarding ROS 2 camera namespace after container changed", zap.String("instance", instanceKey), zap.Uint32("network_namespace_pid", netnsPID))
		return
	}
	state := &participantState{participant: p, cancel: cancel, iface: p.Interface(), domainID: domain, netnsPID: netnsPID, graphKey: graphKey}
	m.mu.Lock()
	if _, raced := m.participants[key]; raced {
		m.mu.Unlock()
		cancel()
		p.Close() //nolint:errcheck
		return
	}
	m.participants[key] = state
	m.mu.Unlock()

	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		defer p.Close()
		for {
			select {
			case <-participantCtx.Done():
				return
			case <-p.Done():
				return
			case <-p.Changed():
				m.syncEndpoints(state)
			case sample := <-p.Samples():
				m.handleSample(p, sample)
			}
		}
	}()
}

func (m *Manager) stopStaleParticipants(desired map[string]bool) {
	m.mu.Lock()
	var stale []*participantState
	type staleCamera struct {
		camera      *cameraState
		participant discoveryLease
	}
	var staleCameras []staleCamera
	for key, participant := range m.participants {
		if desired[key] {
			continue
		}
		delete(m.participants, key)
		participant.stopped = true
		stale = append(stale, participant)
		for _, cam := range m.cameras {
			if cam.participant == participant.participant {
				cam.active = false
				m.unsubscribeLocked(cam)
				staleCameras = append(staleCameras, staleCamera{camera: cam, participant: participant.participant})
			}
		}
	}
	m.mu.Unlock()
	for _, participant := range stale {
		participant.cancel()
		_ = participant.participant.Close()
	}
	for _, item := range staleCameras {
		item.camera.pumpMu.Lock()
		m.mu.Lock()
		if !item.camera.active && item.camera.participant == item.participant && item.camera.writer != nil {
			_ = item.camera.writer.Close()
			item.camera.writer = nil
			item.camera.ready = make(chan struct{})
			item.camera.readyClosed = false
		}
		m.mu.Unlock()
		item.camera.pumpMu.Unlock()
	}
	// A replacement lease may have replayed discovery while the old lease was
	// still selected. Replay survivors after retirement so a restart or one
	// container disappearing cannot leave a shared graph's camera inactive.
	if len(stale) != 0 {
		m.mu.Lock()
		remaining := make([]*participantState, 0, len(m.participants))
		for _, state := range m.participants {
			remaining = append(remaining, state)
		}
		m.mu.Unlock()
		for _, state := range remaining {
			m.syncEndpoints(state)
		}
	}
}

// Read the complete snapshot on each coalesced notification. Removals deactivate
// cameras and release subscriptions; late consumers see already discovered writers.
func (m *Manager) syncEndpoints(p *participantState) {
	endpoints := p.participant.Endpoints()
	present := make(map[rtps.GUID]bool, len(endpoints))
	for _, ep := range endpoints {
		present[ep.GUID] = true
	}
	m.mu.Lock()
	var removed []*cameraState
	for _, cam := range m.cameras {
		if cam.participant == p.participant && cam.active && !present[cam.endpoint.GUID] {
			cam.active = false
			m.unsubscribeLocked(cam)
			removed = append(removed, cam)
		}
	}
	m.mu.Unlock()
	for _, cam := range removed {
		cam.pumpMu.Lock()
		m.mu.Lock()
		if !cam.active {
			m.closeWriterLocked(cam)
		}
		m.mu.Unlock()
		cam.pumpMu.Unlock()
	}
	for _, ep := range endpoints {
		key := fmt.Sprintf("graph=%s;domain=%d;topic=%s", p.graphKey, p.domainID, TopicName(ep.Topic))
		m.mu.Lock()
		cam := m.byKey[key]
		// Keep the selected writer while it is present. Snapshot iteration order
		// must not switch subscriptions among multiple publishers of one topic.
		selected := cam != nil && cam.active && cam.endpoint.GUID != ep.GUID && (cam.participant != p.participant || present[cam.endpoint.GUID])
		m.mu.Unlock()
		if !selected {
			m.registerEndpoint(p, ep)
		}
	}
}

func (m *Manager) unsubscribeLocked(cam *cameraState) {
	if cam.subscribed && cam.participant != nil {
		cam.participant.Unsubscribe(cam.endpoint.GUID)
	}
	cam.subscribed = false
}

func (m *Manager) closeWriterLocked(cam *cameraState) {
	if cam.writer != nil {
		_ = cam.writer.Close()
		cam.writer = nil
	}
	cam.ready = make(chan struct{})
	cam.readyClosed = false
}

func (m *Manager) registerEndpoint(p *participantState, endpoint rtps.Endpoint) {
	if !SupportsType(endpoint.Type) {
		return
	}
	topic := TopicName(endpoint.Topic)
	if !validTopicName(topic) {
		m.logger.Debug("ignoring ROS 2 camera with invalid topic name")
		return
	}
	// Namespace identity prevents two isolated apps that happen to use the same
	// domain and topic name from collapsing into one camera.
	key := fmt.Sprintf("graph=%s;domain=%d;topic=%s", p.graphKey, p.domainID, topic)
	id, err := m.registry.idFor(key)
	if err != nil {
		m.logger.Warn("allocating ROS 2 camera ID failed", zap.String("topic", topic), zap.Error(err))
		return
	}

	m.mu.Lock()
	if p.stopped || m.ctx.Err() != nil {
		m.mu.Unlock()
		return
	}
	cam := m.byKey[key]
	if cam == nil {
		name := "ROS 2 " + topic
		if endpoint.Type == TypeGo2FrontVideo || endpoint.Type == "unitree_go/msg/Go2FrontVideoData" {
			name = "Unitree Go2 front camera"
		}
		cam = &cameraState{Camera: Camera{ID: id, Name: name, Topic: topic, Type: endpoint.Type, DomainID: p.domainID, Interface: p.iface}, ready: make(chan struct{})}
		m.byKey[key], m.cameras[id] = cam, cam
	}
	if cam.active && cam.participant != p.participant && cam.endpoint.GUID == endpoint.GUID {
		m.mu.Unlock()
		return
	}
	if cam.participant == p.participant && cam.endpoint.GUID == endpoint.GUID {
		cam.active = true
		wanted := m.containerUse || cam.viewRefs > 0
		m.mu.Unlock()
		if wanted {
			_ = m.ensureSubscribed(cam)
		}
		return
	}
	if cam.endpoint.GUID != (rtps.GUID{}) {
		delete(m.byWriter, writerKey{cam.participant, cam.endpoint.GUID})
	}
	m.unsubscribeLocked(cam)
	cam.endpoint, cam.participant = endpoint, p.participant
	cam.Type, cam.Interface = endpoint.Type, p.iface
	cam.active = true
	cam.subscribed = false
	m.byWriter[writerKey{p.participant, endpoint.GUID}] = cam
	wanted := m.containerUse || cam.viewRefs > 0
	m.mu.Unlock()

	if m.loopback != nil {
		if err := m.loopback.EnsureNode(m.ctx, id, nameForNode(cam.Name, id)); err != nil {
			m.logger.Debug("creating ROS 2 camera loopback failed", zap.Uint32("camera_id", id), zap.Error(err))
		}
	}
	if wanted {
		m.ensureSubscribed(cam)
	}
}

func nameForNode(name string, id uint32) string {
	const max = 31
	label := fmt.Sprintf("Wendy ROS2 camera %d", id)
	if len(name) <= max {
		label = name
	}
	return label
}

func validTopicName(topic string) bool {
	if len(topic) < 2 || len(topic) > maxTopicLength || topic[0] != '/' {
		return false
	}
	for i := 1; i < len(topic); i++ {
		c := topic[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_' || c == '/' {
			continue
		}
		return false
	}
	return true
}

func (m *Manager) ensureSubscribed(cam *cameraState) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if cam.subscribed || (!m.containerUse && cam.viewRefs == 0) {
		return nil
	}
	if cam.participant == nil || !cam.active {
		return errors.New("ROS 2 camera publisher is unavailable")
	}
	if err := cam.participant.Subscribe(cam.endpoint); err != nil {
		return err
	}
	cam.subscribed = true
	return nil
}

func (m *Manager) handleSample(p discoveryLease, sample rtps.Sample) {
	m.mu.Lock()
	cam := m.byWriter[writerKey{p, sample.Writer}]
	wanted := cam != nil && cam.active && (cam.viewRefs > 0 || m.containerUse)
	typeName := ""
	if cam != nil {
		typeName = cam.Type
	}
	m.mu.Unlock()
	if !wanted {
		return
	}
	if m.loopback == nil {
		return
	}
	cam.pumpMu.Lock()
	defer cam.pumpMu.Unlock()
	path, ok := m.loopback.NodePath(cam.ID)
	if !ok {
		return
	}
	now := time.Now()
	m.mu.Lock()
	if !cam.active || cam.participant != p || cam.endpoint.GUID != sample.Writer || (cam.viewRefs == 0 && !m.containerUse) {
		m.mu.Unlock()
		return
	}
	interval := frameInterval(typeName, len(sample.Payload))
	if !cam.lastFrame.IsZero() && now.Sub(cam.lastFrame) < interval {
		m.mu.Unlock()
		return
	}
	cam.lastFrame = now
	m.mu.Unlock()
	frame, err := DecodeFrame(typeName, sample.Payload)
	if err != nil {
		m.logCameraErrorOnce(cam, "decoding ROS 2 camera frame failed", err)
		return
	}
	m.mu.Lock()
	if cam.writer == nil {
		cam.writer = m.newWriter(path)
	}
	w := cam.writer
	m.mu.Unlock()
	if err := w.WriteFrame(frame); err != nil {
		m.logCameraErrorOnce(cam, "writing ROS 2 camera loopback failed", err)
		return
	}
	m.mu.Lock()
	if !cam.readyClosed {
		close(cam.ready)
		cam.readyClosed = true
	}
	m.mu.Unlock()
}

func frameInterval(typeName string, payloadBytes int) time.Duration {
	if typeName != TypeImage && typeName != "sensor_msgs/msg/Image" {
		return minFrameInterval
	}
	// SECURITY: Large raw samples are expensive to convert and encode. Apply a
	// per-writer byte-size budget while leaving compressed JPEG passthrough at
	// normal camera frame rates.
	if payloadBytes > largeRawBytes {
		return largeRawInterval
	}
	if payloadBytes > mediumRawBytes {
		return mediumRawInterval
	}
	return minFrameInterval
}

func (m *Manager) logCameraErrorOnce(cam *cameraState, message string, err error) {
	m.mu.Lock()
	if cam.loggedError {
		m.mu.Unlock()
		return
	}
	cam.loggedError = true
	m.mu.Unlock()
	m.logger.Warn(message, zap.Uint32("camera_id", cam.ID), zap.String("topic", cam.Topic), zap.Error(err))
}

func (m *Manager) List() []Camera {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Camera, 0, len(m.cameras))
	for _, state := range m.cameras {
		if !state.active {
			continue
		}
		cam := state.Camera
		if m.loopback != nil {
			cam.Path, _ = m.loopback.NodePath(cam.ID)
		}
		out = append(out, cam)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (m *Manager) Get(id uint32) (Camera, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	state := m.cameras[id]
	if state == nil || !state.active {
		return Camera{}, false
	}
	cam := state.Camera
	if m.loopback != nil {
		cam.Path, _ = m.loopback.NodePath(id)
	}
	return cam, true
}

// Acquire starts the DDS subscription and waits until the loopback writer has
// configured the node with its first frame. Waiting avoids opening an
// exclusive-caps v4l2loopback node while it still advertises OUTPUT-only.
func (m *Manager) Acquire(ctx context.Context, id uint32) (Camera, func(), error) {
	m.mu.Lock()
	cam := m.cameras[id]
	if cam != nil && cam.active {
		cam.viewRefs++
	} else {
		cam = nil
	}
	m.mu.Unlock()
	if cam == nil {
		return Camera{}, nil, fmt.Errorf("ROS 2 camera %d not found", id)
	}
	if m.loopback == nil {
		m.release(cam)
		return Camera{}, nil, errors.New("ROS 2 loopback camera support unavailable")
	}
	if err := m.loopback.EnsureNode(ctx, id, nameForNode(cam.Name, id)); err != nil {
		m.release(cam)
		return Camera{}, nil, err
	}
	if err := m.ensureSubscribed(cam); err != nil {
		m.release(cam)
		return Camera{}, nil, err
	}
	m.mu.Lock()
	ready := cam.ready
	m.mu.Unlock()
	timer := time.NewTimer(firstFrameTimeout)
	defer timer.Stop()
	select {
	case <-ready:
	case <-ctx.Done():
		m.release(cam)
		return Camera{}, nil, ctx.Err()
	case <-timer.C:
		m.release(cam)
		return Camera{}, nil, fmt.Errorf("timed out waiting for the first frame on %s", cam.Topic)
	}
	result, ok := m.Get(id)
	if !ok {
		m.release(cam)
		return Camera{}, nil, errors.New("ROS 2 camera publisher stopped before streaming began")
	}
	var once sync.Once
	return result, func() { once.Do(func() { m.release(cam) }) }, nil
}

func (m *Manager) release(cam *cameraState) {
	cam.pumpMu.Lock()
	defer cam.pumpMu.Unlock()
	m.mu.Lock()
	if cam.viewRefs > 0 {
		cam.viewRefs--
	}
	wanted := cam.viewRefs > 0 || m.containerUse
	if !wanted {
		m.unsubscribeLocked(cam)
	}
	if !wanted && cam.writer != nil {
		_ = cam.writer.Close()
		cam.writer = nil
		cam.ready = make(chan struct{})
		cam.readyClosed = false
	}
	m.mu.Unlock()
}

func (m *Manager) EnsureNodes(ctx context.Context) {
	if m.loopback == nil {
		return
	}
	for _, cam := range m.List() {
		if err := m.loopback.EnsureNode(ctx, cam.ID, nameForNode(cam.Name, cam.ID)); err != nil {
			m.logger.Debug("ensuring ROS 2 camera loopback failed", zap.Uint32("camera_id", cam.ID), zap.Error(err))
		}
	}
}

func (m *Manager) SetContainerConsumers(names []string) {
	m.mu.Lock()
	m.containerUse = len(names) != 0
	wanted := m.containerUse
	cameras := make([]*cameraState, 0, len(m.cameras))
	for _, cam := range m.cameras {
		if cam.active {
			cameras = append(cameras, cam)
		}
	}
	m.mu.Unlock()
	if wanted {
		for _, cam := range cameras {
			if err := m.ensureSubscribed(cam); err != nil {
				m.logger.Debug("subscribing to ROS 2 camera failed", zap.Uint32("camera_id", cam.ID), zap.Error(err))
			}
		}
	} else {
		for _, cam := range cameras {
			cam.pumpMu.Lock()
			m.mu.Lock()
			if cam.viewRefs == 0 {
				m.unsubscribeLocked(cam)
			}
			if cam.viewRefs == 0 && cam.writer != nil {
				_ = cam.writer.Close()
				cam.writer = nil
				cam.ready = make(chan struct{})
				cam.readyClosed = false
			}
			m.mu.Unlock()
			cam.pumpMu.Unlock()
		}
	}
}

func (m *Manager) Shutdown() {
	m.reconcileMu.Lock()
	m.cancel()
	m.stopStaleParticipants(nil)
	m.reconcileMu.Unlock()
	m.wg.Wait()
	if m.ownPool {
		_ = m.pool.Close()
	}
	m.mu.Lock()
	cameras := make([]*cameraState, 0, len(m.cameras))
	for _, cam := range m.cameras {
		cameras = append(cameras, cam)
	}
	m.mu.Unlock()
	for _, cam := range cameras {
		cam.pumpMu.Lock()
		m.mu.Lock()
		if cam.writer != nil {
			_ = cam.writer.Close()
			cam.writer = nil
		}
		m.mu.Unlock()
		cam.pumpMu.Unlock()
	}
}
