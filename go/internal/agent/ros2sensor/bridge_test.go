package ros2sensor

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/agent/ros2camera"
	"github.com/wendylabsinc/wendy/go/internal/rtps"
	sensorlinkpb "github.com/wendylabsinc/wendy/go/proto/gen/sensorlinkpb"
	"go.uber.org/zap"
)

// fakeWriter records what one topic's writer on one graph was asked to send.
type fakeWriter struct {
	pub     rtps.Publication
	mu      sync.Mutex
	samples [][]byte
	closed  bool
	fail    error
}

func (w *fakeWriter) Write(payload []byte) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.fail != nil {
		return w.fail
	}
	w.samples = append(w.samples, append([]byte(nil), payload...))
	return nil
}
func (w *fakeWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.closed = true
	return nil
}
func (w *fakeWriter) count() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.samples)
}
func (w *fakeWriter) isClosed() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.closed
}

// fakeLease is one publishing participant the bridge acquired.
type fakeLease struct {
	cfg     rtps.Config
	done    chan struct{}
	mu      sync.Mutex
	writers []*fakeWriter
	closed  bool
}

func (l *fakeLease) NewWriter(pub rtps.Publication) (sampleWriter, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	w := &fakeWriter{pub: pub}
	l.writers = append(l.writers, w)
	return w, nil
}
func (l *fakeLease) Done() <-chan struct{} { return l.done }
func (l *fakeLease) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.closed {
		l.closed = true
		for _, w := range l.writers {
			_ = w.Close()
		}
	}
	return nil
}
func (l *fakeLease) open() []*fakeWriter {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []*fakeWriter
	for _, w := range l.writers {
		if !w.isClosed() {
			out = append(out, w)
		}
	}
	return out
}

// fakeDomain stands in for the pool and for the list of running ROS 2 apps.
type fakeDomain struct {
	mu      sync.Mutex
	graphs  []ros2camera.Graph
	leases  []*fakeLease
	refuse  bool
	listErr error
}

func (d *fakeDomain) list(context.Context) ([]ros2camera.Graph, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]ros2camera.Graph(nil), d.graphs...), d.listErr
}
func (d *fakeDomain) acquire(_ context.Context, cfg rtps.Config) (participantLease, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.refuse {
		return nil, errors.New("network namespace is gone")
	}
	l := &fakeLease{cfg: cfg, done: make(chan struct{})}
	d.leases = append(d.leases, l)
	return l, nil
}
func (d *fakeDomain) set(graphs ...ros2camera.Graph) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.graphs = graphs
}
func (d *fakeDomain) openLeases() []*fakeLease {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []*fakeLease
	for _, l := range d.leases {
		l.mu.Lock()
		if !l.closed {
			out = append(out, l)
		}
		l.mu.Unlock()
	}
	return out
}

func testBridge(graphs ...ros2camera.Graph) (*Bridge, *fakeDomain) {
	d := &fakeDomain{graphs: graphs}
	return newBridge(zap.NewNop(), d.list, d.acquire), d
}

func isolatedApp(id string, pid uint32, domain int) ros2camera.Graph {
	return ros2camera.Graph{Key: id, InstanceKey: id + "-container", DomainID: domain, NetworkNamespacePID: pid}
}

func hostApp(id string, pid uint32, domain int) ros2camera.Graph {
	return ros2camera.Graph{Key: id, InstanceKey: id + "-container", DomainID: domain, NetworkNamespacePID: pid, HostNetwork: true}
}

func temperatureChannel(id uint32, name string) *sensorlinkpb.SensorDescriptor {
	return &sensorlinkpb.SensorDescriptor{ChannelId: id, Name: name, Format: &sensorlinkpb.SensorDescriptor_Sensor{
		Sensor: &sensorlinkpb.SensorFormat{Schema: "wendy.temperature.celsius.f32le.v1", SampleBytes: 4, RateHz: 1}}}
}

func TestNoParticipantJoinsAnAppUntilAChannelIsOpen(t *testing.T) {
	b, d := testBridge(isolatedApp("robot", 4242, 17))
	b.reconcile(context.Background())
	if n := len(d.openLeases()); n != 0 {
		t.Fatalf("%d participants joined ROS 2 apps with nothing to publish", n)
	}

	ch, err := b.OpenChannel("asset:7", "porch-board", temperatureChannel(3, "temperature"))
	if err != nil {
		t.Fatal(err)
	}
	b.reconcile(context.Background())
	leases := d.openLeases()
	if len(leases) != 1 {
		t.Fatalf("%d participants, want 1", len(leases))
	}
	// Inside the app's own network, on loopback, which every ROS 2 scope
	// includes, and as a participant that publishes.
	cfg := leases[0].cfg
	if cfg.NetworkNamespacePID != 4242 || cfg.DomainID != 17 || cfg.Interface != "lo" || !cfg.Publish {
		t.Fatalf("participant config = %+v", cfg)
	}
	writers := leases[0].open()
	if len(writers) != 1 || writers[0].pub != (rtps.Publication{Topic: "rt/sensorlink/porch_board/temperature", Type: "sensor_msgs::msg::dds_::Temperature_"}) {
		t.Fatalf("writers = %+v", writers)
	}
	if got := ch.Topic(); got != "/sensorlink/porch_board/temperature" {
		t.Fatalf("topic = %q", got)
	}
}

func TestChannelIsPublishedIntoEveryROS2App(t *testing.T) {
	b, d := testBridge(
		isolatedApp("robot", 4242, 17),
		hostApp("nav", 5151, 0),
		hostApp("viz", 6161, 0), // shares the host's network and domain with nav
		hostApp("other-domain", 7171, 9),
	)
	ch, err := b.OpenChannel("asset:7", "porch", temperatureChannel(3, "temperature"))
	if err != nil {
		t.Fatal(err)
	}
	b.reconcile(context.Background())
	leases := d.openLeases()
	if len(leases) != 3 {
		t.Fatalf("%d participants, want one per network and domain (3)", len(leases))
	}
	seen := map[[2]int]bool{}
	for _, l := range leases {
		seen[[2]int{int(l.cfg.NetworkNamespacePID), l.cfg.DomainID}] = true
	}
	// Apps on the host's network are reached from the host's own loopback.
	for _, want := range [][2]int{{4242, 17}, {0, 0}, {0, 9}} {
		if !seen[want] {
			t.Fatalf("no participant in network of pid %d on domain %d; have %v", want[0], want[1], seen)
		}
	}

	ch.WriteSample(f32le(21.5))
	for _, l := range leases {
		writers := l.open()
		if len(writers) != 1 || writers[0].count() != 1 {
			t.Fatalf("participant %+v: %d writers, sample count %d", l.cfg, len(writers), writers[0].count())
		}
		if got := decodeTemperature(t, writers[0].samples[0]); got.celsius != 21.5 || got.frameID != "sensorlink/porch" {
			t.Fatalf("published %+v", got)
		}
	}
}

func TestAppsThatStartAndStopGainAndLoseTheChannel(t *testing.T) {
	b, d := testBridge()
	ch, err := b.OpenChannel("asset:7", "porch", temperatureChannel(3, "temperature"))
	if err != nil {
		t.Fatal(err)
	}
	b.reconcile(context.Background())
	ch.WriteSample(f32le(1)) // no ROS 2 app is running: nowhere to publish

	d.set(isolatedApp("robot", 4242, 17))
	b.reconcile(context.Background())
	first := d.openLeases()
	if len(first) != 1 {
		t.Fatalf("%d participants after the app started, want 1", len(first))
	}
	ch.WriteSample(f32le(2))
	if got := first[0].open()[0].count(); got != 1 {
		t.Fatalf("app received %d samples, want only the one sent after it started", got)
	}

	// The app restarts: same app, new process, new network namespace.
	d.set(isolatedApp("robot", 4343, 17))
	b.reconcile(context.Background())
	if !first[0].closed {
		t.Fatal("participant in the stopped app's network was kept")
	}
	second := d.openLeases()
	if len(second) != 1 || second[0].cfg.NetworkNamespacePID != 4343 {
		t.Fatalf("participants after restart = %d", len(second))
	}
	ch.WriteSample(f32le(3))
	if got := second[0].open()[0].count(); got != 1 {
		t.Fatalf("restarted app received %d samples, want 1", got)
	}

	d.set()
	b.reconcile(context.Background())
	if n := len(d.openLeases()); n != 0 {
		t.Fatalf("%d participants left after every app stopped", n)
	}
	ch.WriteSample(f32le(4))
}

func TestClosingTheLastChannelReleasesEveryParticipant(t *testing.T) {
	b, d := testBridge(isolatedApp("robot", 4242, 17))
	temperature, _ := b.OpenChannel("asset:7", "porch", temperatureChannel(3, "temperature"))
	raw, _ := b.OpenChannel("asset:7", "porch", &sensorlinkpb.SensorDescriptor{ChannelId: 4, Name: "imu",
		Format: &sensorlinkpb.SensorDescriptor_Sensor{Sensor: &sensorlinkpb.SensorFormat{Schema: "wendy.imu.v1"}}})
	b.reconcile(context.Background())
	lease := d.openLeases()[0]
	if got := len(lease.open()); got != 2 {
		t.Fatalf("%d writers for two channels", got)
	}

	if err := temperature.Close(); err != nil {
		t.Fatal(err)
	}
	b.reconcile(context.Background())
	open := lease.open()
	if lease.closed || len(open) != 1 || open[0].pub.Topic != "rt/sensorlink/porch/imu" {
		t.Fatalf("after closing one channel: lease closed=%v, %d writers", lease.closed, len(open))
	}
	temperature.WriteSample(f32le(1)) // ignored, not a crash

	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	b.reconcile(context.Background())
	if !lease.closed {
		t.Fatal("participant kept in the app's network with nothing left to publish")
	}
}

func TestParticipantThatCannotJoinIsRetried(t *testing.T) {
	b, d := testBridge(isolatedApp("robot", 4242, 17))
	d.refuse = true
	ch, _ := b.OpenChannel("asset:7", "porch", temperatureChannel(3, "temperature"))
	b.reconcile(context.Background())
	ch.WriteSample(f32le(1))
	if n := len(d.openLeases()); n != 0 {
		t.Fatalf("%d participants", n)
	}
	d.mu.Lock()
	d.refuse = false
	d.mu.Unlock()
	b.reconcile(context.Background())
	if n := len(d.openLeases()); n != 1 {
		t.Fatalf("%d participants after the next attempt, want 1", n)
	}
}

func TestAppListThatCannotBeReadKeepsWhatIsPublishing(t *testing.T) {
	b, d := testBridge(isolatedApp("robot", 4242, 17))
	ch, _ := b.OpenChannel("asset:7", "porch", temperatureChannel(3, "temperature"))
	b.reconcile(context.Background())
	d.mu.Lock()
	d.graphs, d.listErr = nil, errors.New("containerd is restarting")
	d.mu.Unlock()
	b.reconcile(context.Background())
	leases := d.openLeases()
	if len(leases) != 1 {
		t.Fatal("a failed listing was taken to mean every app stopped")
	}
	ch.WriteSample(f32le(1))
	if got := leases[0].open()[0].count(); got != 1 {
		t.Fatalf("app received %d samples", got)
	}
}

func TestTwoChannelsNeverShareATopic(t *testing.T) {
	b, _ := testBridge()
	first, _ := b.OpenChannel("asset:7", "porch", temperatureChannel(3, "temp-1"))
	second, _ := b.OpenChannel("asset:7", "porch", temperatureChannel(4, "temp 1"))
	third, _ := b.OpenChannel("asset:8", "porch", temperatureChannel(3, "temp_1"))
	topics := map[string]bool{first.Topic(): true, second.Topic(): true, third.Topic(): true}
	if len(topics) != 3 {
		t.Fatalf("topics collide: %q, %q, %q", first.Topic(), second.Topic(), third.Topic())
	}
	// The name is free again once its channel closes, so a reconnecting
	// source keeps the topic it had.
	_ = first.Close()
	again, _ := b.OpenChannel("asset:7", "porch", temperatureChannel(3, "temp-1"))
	if again.Topic() != first.Topic() {
		t.Fatalf("reopened channel moved from %q to %q", first.Topic(), again.Topic())
	}
}

func TestBadSampleOrFailingWriterDoesNotStopTheChannel(t *testing.T) {
	b, d := testBridge(isolatedApp("robot", 4242, 17), isolatedApp("arm", 4343, 18))
	ch, _ := b.OpenChannel("asset:7", "porch", temperatureChannel(3, "temperature"))
	b.reconcile(context.Background())
	leases := d.openLeases()
	broken, healthy := leases[0].open()[0], leases[1].open()[0]
	broken.fail = errors.New("message too long")

	ch.WriteSample([]byte{1, 2, 3}) // not a whole sample
	ch.WriteSample(f32le(21.5))
	if healthy.count() != 1 {
		t.Fatalf("healthy app received %d samples, want the one valid sample", healthy.count())
	}
}

func TestOnlyGenericSensorChannelsAreAccepted(t *testing.T) {
	b, _ := testBridge()
	// Audio and video stay on the microphone and camera paths, where an app
	// needs an entitlement to read them. A ROS 2 topic has no such gate.
	for name, d := range map[string]*sensorlinkpb.SensorDescriptor{
		"audio": {ChannelId: 1, Name: "voice", Format: &sensorlinkpb.SensorDescriptor_Audio{Audio: &sensorlinkpb.AudioFormat{Codec: sensorlinkpb.AudioFormat_OPUS}}},
		"video": {ChannelId: 2, Name: "cam", Format: &sensorlinkpb.SensorDescriptor_Video{Video: &sensorlinkpb.VideoFormat{}}},
		"none":  {ChannelId: 3, Name: "unknown"},
	} {
		if _, err := b.OpenChannel("asset:7", "porch", d); err == nil {
			t.Errorf("%s channel was accepted for publishing to ROS 2", name)
		}
	}
}
