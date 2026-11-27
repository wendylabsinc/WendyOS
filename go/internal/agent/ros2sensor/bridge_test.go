package ros2sensor

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/mcusource"
	"github.com/wendylabsinc/wendy/go/internal/agent/ros2camera"
	"github.com/wendylabsinc/wendy/go/internal/agent/sensorlink"
	"github.com/wendylabsinc/wendy/go/internal/rtps"
	sensorlinkpb "github.com/wendylabsinc/wendy/go/proto/gen/sensorlinkpb"
	"go.uber.org/zap"
)

// fakeWriter records what one topic's writer on one graph was asked to send.
type fakeWriter struct {
	pub     rtps.Publication
	onWrite func(topic string)
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
	if w.onWrite != nil {
		w.onWrite(w.pub.Topic)
	}
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
	cap     int // most writers the participant has room for; zero is no limit
	onWrite func(topic string)
	done    chan struct{}
	mu      sync.Mutex
	writers []*fakeWriter
	closed  bool
}

func (l *fakeLease) NewWriter(pub rtps.Publication) (sampleWriter, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.cap > 0 && len(l.writers) >= l.cap {
		return nil, errors.New("too many writers on one participant")
	}
	w := &fakeWriter{pub: pub, onWrite: l.onWrite}
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

// open returns the writers of the topics that carry readings; timing returns
// the writers of the topics that say how each frame's time is known.
func (l *fakeLease) open() []*fakeWriter   { return l.openWriters(false) }
func (l *fakeLease) timing() []*fakeWriter { return l.openWriters(true) }

func (l *fakeLease) openWriters(timing bool) []*fakeWriter {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []*fakeWriter
	for _, w := range l.writers {
		if !w.isClosed() && strings.HasSuffix(w.pub.Topic, "/timing") == timing {
			out = append(out, w)
		}
	}
	return out
}

// fakeDomain stands in for the pool and for the list of running ROS 2 apps.
type fakeDomain struct {
	mu      sync.Mutex
	cap     int               // writers each participant has room for
	shared  map[uint32]string // processes that share a network, by its name
	unknown map[uint32]bool   // processes whose network cannot be identified
	writes  []string          // topics, in the order they were written to
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
	l := &fakeLease{cfg: cfg, cap: d.cap, done: make(chan struct{})}
	// Called from a writer, never while d.mu is held.
	l.onWrite = func(topic string) {
		d.mu.Lock()
		d.writes = append(d.writes, topic)
		d.mu.Unlock()
	}
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

// network says which network a process is in: the host's for an app on the
// host's network, a named one for apps that share one, and otherwise one of
// its own.
func (d *fakeDomain) network(pid uint32) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.unknown[pid] {
		return "", errors.New("no such process")
	}
	if name, ok := d.shared[pid]; ok {
		return name, nil
	}
	for _, g := range d.graphs {
		if g.NetworkNamespacePID == pid && g.HostNetwork {
			return "host", nil
		}
	}
	if pid == 0 {
		return "host", nil
	}
	return fmt.Sprintf("net:%d", pid), nil
}

func testBridge(graphs ...ros2camera.Graph) (*Bridge, *fakeDomain) {
	d := &fakeDomain{graphs: graphs, shared: map[uint32]string{}, unknown: map[uint32]bool{}}
	b := newBridge(zap.NewNop(), d.list, d.acquire)
	b.network = d.network
	return b, d
}

func isolatedApp(id string, pid uint32, domain int) ros2camera.Graph {
	return ros2camera.Graph{Key: id, InstanceKey: id + "-container", DomainID: domain, NetworkNamespacePID: pid, SensorRead: true}
}

func hostApp(id string, pid uint32, domain int) ros2camera.Graph {
	return ros2camera.Graph{Key: id, InstanceKey: id + "-container", DomainID: domain, NetworkNamespacePID: pid, HostNetwork: true, SensorRead: true}
}

// withoutSensorRead is the same app without the sensor-read entitlement.
func withoutSensorRead(g ros2camera.Graph) ros2camera.Graph {
	g.SensorRead = false
	return g
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

func TestChannelIsPublishedIntoEveryROS2AppThatMayReadIt(t *testing.T) {
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

	ch.WriteFrame(&sensorlink.SensorFrame{Payload: f32le(21.5)})
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
	ch.WriteFrame(&sensorlink.SensorFrame{Payload: f32le(1)}) // no ROS 2 app is running: nowhere to publish

	d.set(isolatedApp("robot", 4242, 17))
	b.reconcile(context.Background())
	first := d.openLeases()
	if len(first) != 1 {
		t.Fatalf("%d participants after the app started, want 1", len(first))
	}
	ch.WriteFrame(&sensorlink.SensorFrame{Payload: f32le(2)})
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
	ch.WriteFrame(&sensorlink.SensorFrame{Payload: f32le(3)})
	if got := second[0].open()[0].count(); got != 1 {
		t.Fatalf("restarted app received %d samples, want 1", got)
	}

	d.set()
	b.reconcile(context.Background())
	if n := len(d.openLeases()); n != 0 {
		t.Fatalf("%d participants left after every app stopped", n)
	}
	ch.WriteFrame(&sensorlink.SensorFrame{Payload: f32le(4)})
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
	temperature.WriteFrame(&sensorlink.SensorFrame{Payload: f32le(1)}) // ignored, not a crash

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
	ch.WriteFrame(&sensorlink.SensorFrame{Payload: f32le(1)})
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

func TestAppListThatCannotBeReadKeepsWhatIsPublishingOnlyForAWhile(t *testing.T) {
	b, d := testBridge(isolatedApp("robot", 4242, 17))
	ch, _ := b.OpenChannel("asset:7", "porch", temperatureChannel(3, "temperature"))
	b.reconcile(context.Background())
	d.mu.Lock()
	d.graphs, d.listErr = nil, errors.New("containerd is restarting")
	d.mu.Unlock()
	// A listing that fails once or twice is not taken to mean every app
	// stopped: the topics would drop out on every hiccup.
	for i := 0; i < listFailuresTolerated; i++ {
		b.reconcile(context.Background())
	}
	leases := d.openLeases()
	if len(leases) != 1 {
		t.Fatal("a failed listing was taken to mean every app stopped")
	}
	ch.WriteFrame(&sensorlink.SensorFrame{Payload: f32le(1)})
	if got := leases[0].open()[0].count(); got != 1 {
		t.Fatalf("app received %d samples", got)
	}
	// But who may read the topics is only known from the listing. When it
	// stays unreadable, publishing stops rather than carrying on unchecked.
	b.reconcile(context.Background())
	if n := len(d.openLeases()); n != 0 {
		t.Fatalf("%d participants still publish although the apps could not be listed %d times running", n, listFailuresTolerated+1)
	}
	d.mu.Lock()
	d.graphs, d.listErr = []ros2camera.Graph{isolatedApp("robot", 4242, 17)}, nil
	d.mu.Unlock()
	b.reconcile(context.Background())
	if n := len(d.openLeases()); n != 1 {
		t.Fatalf("%d participants once the apps can be listed again, want 1", n)
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
	// A channel keeps its topic for as long as the agent runs, in whatever
	// order the sources come back: a subscriber must never find another
	// board's readings on the topic it reads.
	was := map[*Channel]string{first: first.Topic(), second: second.Topic(), third: third.Topic()}
	for c := range was {
		_ = c.Close()
	}
	thirdAgain, _ := b.OpenChannel("asset:8", "porch", temperatureChannel(3, "temp_1"))
	secondAgain, _ := b.OpenChannel("asset:7", "porch", temperatureChannel(4, "temp 1"))
	firstAgain, _ := b.OpenChannel("asset:7", "porch", temperatureChannel(3, "temp-1"))
	if firstAgain.Topic() != was[first] || secondAgain.Topic() != was[second] || thirdAgain.Topic() != was[third] {
		t.Fatalf("after reconnecting in another order the topics are %q, %q, %q; they were %q, %q, %q",
			firstAgain.Topic(), secondAgain.Topic(), thirdAgain.Topic(), was[first], was[second], was[third])
	}
}

func TestBadSampleOrFailingWriterDoesNotStopTheChannel(t *testing.T) {
	b, d := testBridge(isolatedApp("robot", 4242, 17), isolatedApp("arm", 4343, 18))
	ch, _ := b.OpenChannel("asset:7", "porch", temperatureChannel(3, "temperature"))
	b.reconcile(context.Background())
	leases := d.openLeases()
	broken, healthy := leases[0].open()[0], leases[1].open()[0]
	broken.fail = errors.New("message too long")

	ch.WriteFrame(&sensorlink.SensorFrame{Payload: []byte{1, 2, 3}}) // not a whole sample
	ch.WriteFrame(&sensorlink.SensorFrame{Payload: f32le(21.5)})
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

func TestMountPublishesASourcesSensorChannelsUntilReleased(t *testing.T) {
	b, d := testBridge(isolatedApp("robot", 4242, 17))
	pairing := mcusource.SensorPairing{SourceAssetID: 7, Name: "porch-board"}
	accepted, publish, release := b.Mount(pairing, []*sensorlinkpb.SensorDescriptor{
		temperatureChannel(3, "temperature"),
		// Never offered by the supervisor, and refused all the same.
		{ChannelId: 9, Name: "cam", Format: &sensorlinkpb.SensorDescriptor_Video{Video: &sensorlinkpb.VideoFormat{}}},
	})
	if len(accepted) != 1 || accepted[0] != 3 {
		t.Fatalf("accepted channels %v, want only the sensor channel 3", accepted)
	}
	b.reconcile(context.Background())
	leases := d.openLeases()
	if len(leases) != 1 || len(leases[0].open()) != 1 {
		t.Fatalf("%d participants after mounting one channel", len(leases))
	}
	writer := leases[0].open()[0]
	if writer.pub.Topic != "rt/sensorlink/porch_board/temperature" {
		t.Fatalf("topic = %q, want it named after the pairing", writer.pub.Topic)
	}

	if !publish(&sensorlink.SensorFrame{ChannelID: 3, Payload: f32le(21.5)}) {
		t.Fatal("a frame of the mounted channel was not taken")
	}
	if publish(&sensorlink.SensorFrame{ChannelID: 9, Payload: []byte{1}}) {
		t.Fatal("a frame of a channel that was not mounted was taken")
	}
	if got := writer.count(); got != 1 {
		t.Fatalf("app received %d samples, want 1", got)
	}

	release()
	release() // the supervisor's contract: releasing twice is harmless
	b.reconcile(context.Background())
	if n := len(d.openLeases()); n != 0 {
		t.Fatalf("%d participants left after the source went away", n)
	}
	if publish(&sensorlink.SensorFrame{ChannelID: 3, Payload: f32le(22)}) {
		t.Fatal("a frame was taken after the mount was released")
	}
}

// testClock is the device's clock under the test's control.
type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *testClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}
func (c *testClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func stampOf(t *testing.T, payload []byte) time.Time {
	t.Helper()
	m := decodeTemperature(t, payload)
	return time.Unix(int64(m.sec), int64(m.nanosec))
}

func TestReadingIsStampedWhenTheSourceTookItNotWhenItArrived(t *testing.T) {
	b, d := testBridge(isolatedApp("robot", 4242, 17))
	clock := &testClock{t: time.Unix(1_700_000_000, 0)}
	b.now = clock.now
	ch, err := b.OpenChannel("asset:7", "porch", temperatureChannel(3, "temperature"))
	if err != nil {
		t.Fatal(err)
	}
	b.reconcile(context.Background())
	writer := d.openLeases()[0].open()[0]

	// The board has been up five seconds and reads its sensor once a second.
	// Its second frame is held up 300 ms on the way.
	clock.advance(time.Second)
	first := clock.now()
	ch.WriteFrame(&sensorlink.SensorFrame{TsUs: 5_000_000, Payload: f32le(20)})
	clock.advance(1300 * time.Millisecond)
	ch.WriteFrame(&sensorlink.SensorFrame{Seq: 1, TsUs: 6_000_000, Payload: f32le(21)})
	clock.advance(700 * time.Millisecond)
	ch.WriteFrame(&sensorlink.SensorFrame{Seq: 2, TsUs: 7_000_000, Payload: f32le(22)})

	if writer.count() != 3 {
		t.Fatalf("app received %d readings, want 3", writer.count())
	}
	for i, want := range []time.Time{first, first.Add(time.Second), first.Add(2 * time.Second)} {
		if got := stampOf(t, writer.samples[i]); !got.Equal(want) {
			t.Fatalf("reading %d is stamped %s, want %s: one second after the one before, as the board took it",
				i, got.UTC().Format(time.RFC3339Nano), want.UTC().Format(time.RFC3339Nano))
		}
	}
}

func TestReadingWithoutASourceTimeIsStampedOnArrival(t *testing.T) {
	b, d := testBridge(isolatedApp("robot", 4242, 17))
	clock := &testClock{t: time.Unix(1_700_000_000, 0)}
	b.now = clock.now
	ch, _ := b.OpenChannel("asset:7", "porch", temperatureChannel(3, "temperature"))
	b.reconcile(context.Background())
	writer := d.openLeases()[0].open()[0]

	// ts_us is zero when a source does not stamp its frames, and a value that
	// cannot be a time is treated the same way.
	for i, ts := range []uint64{0, 0, ^uint64(0)} {
		clock.advance(1300 * time.Millisecond)
		ch.WriteFrame(&sensorlink.SensorFrame{Seq: uint32(i), TsUs: ts, Payload: f32le(20)})
		if got := stampOf(t, writer.samples[i]); !got.Equal(clock.now()) {
			t.Fatalf("reading %d (ts_us %d) is stamped %s, want its arrival %s", i, ts, got, clock.now())
		}
	}
}

func TestEveryFrameSaysHowItsSampleTimeIsKnown(t *testing.T) {
	b, d := testBridge(isolatedApp("robot", 4242, 17))
	clock := &testClock{t: time.Unix(1_700_000_000, 0)}
	b.now = clock.now
	ch, _ := b.OpenChannel("asset:7", "porch", temperatureChannel(3, "temperature"))
	b.reconcile(context.Background())
	lease := d.openLeases()[0]
	if len(lease.timing()) != 1 {
		t.Fatalf("%d timing topics for one channel", len(lease.timing()))
	}
	readings, timings := lease.open()[0], lease.timing()[0]
	if timings.pub != (rtps.Publication{Topic: "rt/sensorlink/porch/temperature/timing", Type: "diagnostic_msgs::msg::dds_::DiagnosticArray_"}) {
		t.Fatalf("timing topic = %+v", timings.pub)
	}

	ch.WriteFrame(&sensorlink.SensorFrame{Seq: 40, TsUs: 5_000_000, Payload: f32le(20)})
	clock.advance(1300 * time.Millisecond)
	ch.WriteFrame(&sensorlink.SensorFrame{Seq: 41, TsUs: 6_000_000, Payload: f32le(21, 22)})
	if readings.count() != 3 || timings.count() != 2 {
		t.Fatalf("%d readings and %d timing messages, want 3 and one per frame (2)", readings.count(), timings.count())
	}

	// One timing message per frame, stamped like the frame's first reading so
	// that the two can be matched, and published right before the readings.
	second := decodeTiming(t, timings.samples[1])
	if want := stampOf(t, readings.samples[1]); !second.stamp.Equal(want) {
		t.Fatalf("timing is stamped %s and the reading it describes %s", second.stamp, want)
	}
	wantOrder := []string{"rt/sensorlink/porch/temperature/timing", "rt/sensorlink/porch/temperature",
		"rt/sensorlink/porch/temperature/timing", "rt/sensorlink/porch/temperature", "rt/sensorlink/porch/temperature"}
	if !reflect.DeepEqual(d.writes, wantOrder) {
		t.Fatalf("published in the order %v", d.writes)
	}
	if second.frameID != "sensorlink/porch" || second.name != "/sensorlink/porch/temperature" || second.hardwareID != "asset:7" {
		t.Fatalf("timing names %q, %q on %q", second.frameID, second.name, second.hardwareID)
	}
	for key, want := range map[string]string{
		"sequence": "41", "readings": "2", "sample_period_nanos": "1000000000",
		"native_clock_domain": "SENSORLINK_SOURCE", "native_timestamp_nanos": "6000000000",
		"mapping_segment": "sensorlink-source-1",
	} {
		if second.values[key] != want {
			t.Errorf("%s = %q, want %q (all: %v)", key, second.values[key], want, second.values)
		}
	}
}

func TestRawByteChannelGetsItsSampleTimeFromTheTimingTopic(t *testing.T) {
	b, d := testBridge(isolatedApp("robot", 4242, 17))
	clock := &testClock{t: time.Unix(1_700_000_000, 0)}
	b.now = clock.now
	// A schema with no ROS 2 message of its own is published as bytes, and a
	// byte array has no header to carry a time in.
	ch, _ := b.OpenChannel("asset:7", "porch", &sensorlinkpb.SensorDescriptor{ChannelId: 4, Name: "imu",
		Format: &sensorlinkpb.SensorDescriptor_Sensor{Sensor: &sensorlinkpb.SensorFormat{Schema: "wendy.imu.v1", SampleBytes: 6, RateHz: 100}}})
	b.reconcile(context.Background())
	lease := d.openLeases()[0]
	// Each frame holds two readings 10 ms apart, so its first reading was
	// taken at least 10 ms before the frame arrived.
	first := clock.now().Add(-10 * time.Millisecond)
	ch.WriteFrame(&sensorlink.SensorFrame{Seq: 1, TsUs: 5_000_000, Payload: make([]byte, 12)})
	clock.advance(25 * time.Millisecond)
	ch.WriteFrame(&sensorlink.SensorFrame{Seq: 2, TsUs: 5_020_000, Payload: make([]byte, 12)})
	if len(lease.timing()) != 1 || lease.timing()[0].count() != 2 {
		t.Fatalf("raw-byte channel has %d timing topics", len(lease.timing()))
	}
	if got := decodeTiming(t, lease.timing()[0].samples[0]); !got.stamp.Equal(first) {
		t.Fatalf("first frame is stamped %s, want %s", got.stamp, first)
	}
	got := decodeTiming(t, lease.timing()[0].samples[1])
	if !got.stamp.Equal(first.Add(20*time.Millisecond)) || got.values["readings"] != "2" || got.values["sample_period_nanos"] != "10000000" {
		t.Fatalf("second frame: stamp %s, values %v", got.stamp, got.values)
	}
}

func TestFrameWithNothingToPublishHasNoTimingEither(t *testing.T) {
	b, d := testBridge(isolatedApp("robot", 4242, 17))
	ch, _ := b.OpenChannel("asset:7", "porch", temperatureChannel(3, "temperature"))
	b.reconcile(context.Background())
	lease := d.openLeases()[0]
	if ch.WriteFrame(&sensorlink.SensorFrame{TsUs: 5_000_000, Payload: []byte{1, 2, 3}}) {
		t.Fatal("a frame that is not whole readings was reported as published")
	}
	if n := lease.timing()[0].count(); n != 0 {
		t.Fatalf("%d timing messages for a frame none of which was published", n)
	}
}

func TestTimingTopicClosesWithItsChannel(t *testing.T) {
	b, d := testBridge(isolatedApp("robot", 4242, 17))
	first, _ := b.OpenChannel("asset:7", "porch", temperatureChannel(3, "temperature"))
	_, _ = b.OpenChannel("asset:7", "porch", temperatureChannel(4, "other"))
	b.reconcile(context.Background())
	lease := d.openLeases()[0]
	if len(lease.open()) != 2 || len(lease.timing()) != 2 {
		t.Fatalf("%d reading and %d timing topics for two channels", len(lease.open()), len(lease.timing()))
	}
	_ = first.Close()
	if len(lease.open()) != 1 || len(lease.timing()) != 1 || lease.timing()[0].pub.Topic != "rt/sensorlink/porch/other/timing" {
		t.Fatalf("after closing one channel: %d reading and %d timing topics", len(lease.open()), len(lease.timing()))
	}
}

func TestOnlyAppsWithTheSensorReadEntitlementGetTheTopics(t *testing.T) {
	// sensor-read is what lets an app read paired sensor channels. A topic in
	// an app's network would hand them over without it.
	b, d := testBridge(
		isolatedApp("entitled", 4242, 17),
		withoutSensorRead(isolatedApp("plain", 4343, 17)),
	)
	if _, err := b.OpenChannel("asset:7", "porch", temperatureChannel(3, "temperature")); err != nil {
		t.Fatal(err)
	}
	b.reconcile(context.Background())
	leases := d.openLeases()
	if len(leases) != 1 || leases[0].cfg.NetworkNamespacePID != 4242 {
		t.Fatalf("%d participants; want one, in the network of the app with sensor-read", len(leases))
	}

	// The entitlement is read each time the apps are listed: an app redeployed
	// with it gains the topics, and one redeployed without it loses them.
	d.set(withoutSensorRead(isolatedApp("entitled", 4242, 17)), isolatedApp("plain", 4444, 17))
	b.reconcile(context.Background())
	if !leases[0].closed {
		t.Fatal("participant stayed in the network of an app that no longer holds sensor-read")
	}
	leases = d.openLeases()
	if len(leases) != 1 || leases[0].cfg.NetworkNamespacePID != 4444 {
		t.Fatalf("%d participants after the redeploy; want one, in the newly entitled app", len(leases))
	}
}

func TestSharedNetworkGetsTheTopicsOnlyWhenEveryROS2AppOnItMayReadThem(t *testing.T) {
	// Apps on the host's network share it, and so do the services of one app
	// that share a network. A topic published there reaches all of them,
	// whatever ROS domain each one says it uses: an app can join any domain.
	b, d := testBridge(
		hostApp("nav", 5151, 0),
		withoutSensorRead(hostApp("viz", 6161, 9)),
		isolatedApp("arm-driver", 7171, 3),
		withoutSensorRead(isolatedApp("arm-ui", 7272, 3)),
		isolatedApp("alone", 8181, 3),
	)
	d.shared[7171], d.shared[7272] = "arm", "arm"
	if _, err := b.OpenChannel("asset:7", "porch", temperatureChannel(3, "temperature")); err != nil {
		t.Fatal(err)
	}
	b.reconcile(context.Background())
	leases := d.openLeases()
	if len(leases) != 1 || leases[0].cfg.NetworkNamespacePID != 8181 {
		t.Fatalf("%d participants; want one, in the only network where every ROS 2 app holds sensor-read", len(leases))
	}

	// The apps without the entitlement stop: both shared networks open.
	d.set(hostApp("nav", 5151, 0), isolatedApp("arm-driver", 7171, 3), isolatedApp("alone", 8181, 3))
	b.reconcile(context.Background())
	if n := len(d.openLeases()); n != 3 {
		t.Fatalf("%d participants once every ROS 2 app holds sensor-read, want 3", n)
	}
	// One comes back on the host's network: the host is left again.
	d.set(hostApp("nav", 5151, 0), withoutSensorRead(hostApp("viz", 6262, 9)), isolatedApp("arm-driver", 7171, 3), isolatedApp("alone", 8181, 3))
	b.reconcile(context.Background())
	for _, l := range d.openLeases() {
		if l.cfg.NetworkNamespacePID == 0 {
			t.Fatalf("still publishing on the host's network (domain %d) with an app there that lacks sensor-read", l.cfg.DomainID)
		}
	}
	if n := len(d.openLeases()); n != 2 {
		t.Fatalf("%d participants, want the two app networks", n)
	}
}

func TestAppsSharingANetworkGetEachReadingOnce(t *testing.T) {
	// Three services of one app share its network and all hold sensor-read.
	// One participant serves the network: a second and third would deliver
	// every reading again.
	b, d := testBridge(isolatedApp("a", 4242, 17), isolatedApp("b", 4343, 17), isolatedApp("c", 4444, 21))
	d.shared[4242], d.shared[4343], d.shared[4444] = "app", "app", "app"
	ch, _ := b.OpenChannel("asset:7", "porch", temperatureChannel(3, "temperature"))
	b.reconcile(context.Background())
	leases := d.openLeases()
	domains := map[int]int{}
	for _, l := range leases {
		domains[l.cfg.DomainID]++
	}
	// One per ROS domain in use on that network.
	if len(leases) != 2 || domains[17] != 1 || domains[21] != 1 {
		t.Fatalf("participants per domain = %v, want one each on 17 and 21", domains)
	}
	ch.WriteFrame(&sensorlink.SensorFrame{Payload: f32le(21.5)})
	for _, l := range leases {
		if len(l.open()) != 1 || l.open()[0].count() != 1 {
			t.Fatalf("domain %d: %d writers for the topic", l.cfg.DomainID, len(l.open()))
		}
	}

	// The service the network was joined through stops; the others go on
	// getting the topic, through one participant.
	d.set(isolatedApp("b", 4343, 17), isolatedApp("c", 4444, 21))
	b.reconcile(context.Background())
	if n := len(d.openLeases()); n != 2 {
		t.Fatalf("%d participants after one of the three services stopped, want 2", n)
	}
}

func TestAppWhoseNetworkCannotBeIdentifiedGetsNothing(t *testing.T) {
	b, d := testBridge(isolatedApp("robot", 4242, 17), isolatedApp("gone", 4343, 17))
	d.unknown[4343] = true
	_, _ = b.OpenChannel("asset:7", "porch", temperatureChannel(3, "temperature"))
	b.reconcile(context.Background())
	leases := d.openLeases()
	if len(leases) != 1 || leases[0].cfg.NetworkNamespacePID != 4242 {
		t.Fatalf("%d participants; want one, for the app whose network is known", len(leases))
	}
}

func TestMappingSegmentsAreNumberedOnWhenASourceReconnects(t *testing.T) {
	b, d := testBridge(isolatedApp("robot", 4242, 17))
	pairing := mcusource.SensorPairing{SourceAssetID: 7, Name: "porch"}
	channels := []*sensorlinkpb.SensorDescriptor{temperatureChannel(3, "temperature")}
	segmentOf := func() string {
		t.Helper()
		samples := d.openLeases()[0].timing()[0].samples
		return decodeTiming(t, samples[len(samples)-1]).values["mapping_segment"]
	}

	_, publish, release := b.Mount(pairing, channels)
	b.reconcile(context.Background())
	publish(&sensorlink.SensorFrame{ChannelID: 3, TsUs: 90_000_000, Payload: f32le(20)})
	if got := segmentOf(); got != "sensorlink-source-1" {
		t.Fatalf("first connection: segment %q", got)
	}
	release()

	// The board restarted and its clock with it. The link dropped, so this is
	// a new connection: its times must not look comparable with the old ones.
	_, publish, release = b.Mount(pairing, channels)
	defer release()
	b.reconcile(context.Background())
	publish(&sensorlink.SensorFrame{ChannelID: 3, TsUs: 2_000_000, Payload: f32le(21)})
	if got := segmentOf(); got != "sensorlink-source-2" {
		t.Fatalf("after reconnecting: segment %q, want sensorlink-source-2", got)
	}
}

func TestFrameTooLargeForOneROS2MessageIsNotPublished(t *testing.T) {
	b, d := testBridge(isolatedApp("robot", 4242, 17))
	ch, _ := b.OpenChannel("asset:7", "porch", &sensorlinkpb.SensorDescriptor{ChannelId: 4, Name: "scan",
		Format: &sensorlinkpb.SensorDescriptor_Sensor{Sensor: &sensorlinkpb.SensorFormat{Schema: "example.scan.v1", SampleBytes: 1}}})
	b.reconcile(context.Background())
	lease := d.openLeases()[0]
	// A source may send frames of up to 64 KiB; a ROS 2 message the agent
	// sends fits one datagram. Nothing of such a frame goes out, not even
	// its timing, and the frame is not counted as delivered.
	if ch.WriteFrame(&sensorlink.SensorFrame{TsUs: 5_000_000, Payload: make([]byte, 62_000)}) {
		t.Fatal("a frame too large to publish was reported as published")
	}
	if lease.open()[0].count() != 0 || lease.timing()[0].count() != 0 {
		t.Fatalf("%d readings and %d timing messages went out for a frame too large to publish", lease.open()[0].count(), lease.timing()[0].count())
	}
	if !ch.WriteFrame(&sensorlink.SensorFrame{Seq: 1, TsUs: 5_100_000, Payload: make([]byte, 50_000)}) || lease.open()[0].count() != 1 {
		t.Fatal("a frame that fits was not published")
	}
}

func TestReadingsGetAWriterBeforeAnyTimingTopicDoes(t *testing.T) {
	// A participant has room for a limited number of writers. When they run
	// out, every channel's readings are served before any channel's timing.
	b, d := testBridge(isolatedApp("robot", 4242, 17))
	d.cap = 2
	_, _ = b.OpenChannel("asset:7", "porch", temperatureChannel(3, "a"))
	_, _ = b.OpenChannel("asset:7", "porch", temperatureChannel(4, "b"))
	b.reconcile(context.Background())
	lease := d.openLeases()[0]
	if len(lease.open()) != 2 || len(lease.timing()) != 0 {
		t.Fatalf("%d reading and %d timing topics with room for 2 writers, want both readings and no timing", len(lease.open()), len(lease.timing()))
	}
}

func TestSourcesOwnUncertaintyBoundsTheStamp(t *testing.T) {
	b, d := testBridge(isolatedApp("robot", 4242, 17))
	clock := &testClock{t: time.Unix(1_700_000_000, 0)}
	b.now = clock.now
	ch, _ := b.OpenChannel("asset:7", "porch", temperatureChannel(3, "temperature"))
	b.reconcile(context.Background())
	timings := d.openLeases()[0].timing()[0]

	// The board read its sensor between two readings of its clock 300 us
	// apart, and says so. Older firmware says nothing.
	measured := uint32(150)
	ch.WriteFrame(&sensorlink.SensorFrame{Seq: 1, TsUs: 5_000_000, TsUncertaintyUs: &measured, Payload: f32le(20)})
	clock.advance(time.Second)
	ch.WriteFrame(&sensorlink.SensorFrame{Seq: 2, TsUs: 6_000_000, Payload: f32le(21)})

	with, without := decodeTiming(t, timings.samples[0]), decodeTiming(t, timings.samples[1])
	if with.values["native_uncertainty_nanos"] != "150000" || with.values["confidence"] != "source_reported" || with.level != 0 {
		t.Fatalf("frame with a reported uncertainty: level %d, values %v", with.level, with.values)
	}
	// On the first frame the offset in use is the best estimate there is, so
	// the bound on the stamp is the source's own.
	if with.values["mapping_uncertainty_nanos"] != "0" || with.values["timestamp_uncertainty_nanos"] != "150000" {
		t.Fatalf("frame with a reported uncertainty: values %v", with.values)
	}
	if without.values["native_uncertainty_nanos"] != "unreported" || without.values["confidence"] != "unbounded" || without.level != 1 {
		t.Fatalf("frame without one: level %d, values %v", without.level, without.values)
	}
}
