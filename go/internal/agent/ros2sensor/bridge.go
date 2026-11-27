package ros2sensor

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/mcusource"
	"github.com/wendylabsinc/wendy/go/internal/agent/ros2camera"
	"github.com/wendylabsinc/wendy/go/internal/agent/sensorlink"
	"github.com/wendylabsinc/wendy/go/internal/rtps"
	sensorlinkpb "github.com/wendylabsinc/wendy/go/proto/gen/sensorlinkpb"
	"go.uber.org/zap"
)

const (
	// reconcileInterval is how often the set of running ROS 2 apps is read
	// while at least one channel is open.
	reconcileInterval = 10 * time.Second
	// loopback is the interface every participant binds. Every ROS 2 scope
	// includes loopback discovery, and binding one interface per app keeps a
	// subscriber from matching the same channel twice.
	loopback = "lo"
	// warnInterval spaces out the warnings of one channel whose samples
	// cannot be published.
	warnInterval = time.Minute
	// listFailuresTolerated is how many times in a row the running apps may
	// fail to be listed before publishing stops. Who may read the topics is
	// only known from that list.
	listFailuresTolerated = 3
)

// sampleWriter writes one topic into one ROS 2 graph.
type sampleWriter interface {
	Write(payload []byte) error
	Close() error
}

// participantLease is a publishing participant in one ROS 2 graph.
type participantLease interface {
	NewWriter(pub rtps.Publication) (sampleWriter, error)
	Done() <-chan struct{}
	Close() error
}

type acquireFunc func(ctx context.Context, cfg rtps.Config) (participantLease, error)

// poolLease adapts a pool's lease to participantLease.
type poolLease struct{ *rtps.Lease }

func (l poolLease) NewWriter(pub rtps.Publication) (sampleWriter, error) {
	w, err := l.Lease.NewWriter(pub)
	if err != nil {
		return nil, err
	}
	return w, nil
}

// Bridge publishes open sensor channels into the running ROS 2 apps that hold
// the sensor-read entitlement. It joins an app's ROS 2 network only while
// there is a channel to publish.
type Bridge struct {
	logger  *zap.Logger
	graphs  ros2camera.GraphSource
	acquire acquireFunc
	// now is the device's clock; tests replace it.
	now func() time.Time
	// network names the network namespace of a process, and of the agent
	// itself for pid 0. Apps that share a network have the same name.
	network func(pid uint32) (string, error)

	// reconcileMu admits one reconcile at a time and guards listFailures.
	reconcileMu  sync.Mutex
	listFailures int
	wake         chan struct{}

	mu       sync.Mutex
	channels map[*Channel]struct{}
	// topics maps each topic in use to its channel, so no two share one.
	topics map[string]*Channel
	// leases holds the participant in each app's graph, by graph.
	leases map[string]*graphLease
	// announced remembers the topic last logged for a source's channel, so a
	// source that reconnects is not logged again.
	announced map[string]string
	// reserved maps a topic to the source's channel it was first given to. It
	// stays that channel's for as long as the agent runs, so that sources
	// reconnecting in another order do not swap topics.
	reserved map[string]string
	// segments remembers the last clock mapping segment of a source's channel,
	// so that numbering carries on when the source reconnects.
	segments map[string]int
}

// graphLease is the participant in one graph and its writers per channel.
type graphLease struct {
	lease   participantLease
	writers map[*Channel]*channelWriters
}

// channelWriters are the two topics of one channel in one graph: its readings,
// and how the time of each frame of readings is known. Either may be missing
// for a while when it could not be opened.
type channelWriters struct {
	readings, timing sampleWriter
}

// New returns a bridge that takes its participants from pool and the running
// ROS 2 apps from graphs. Call Run to keep it in step with those apps.
func New(logger *zap.Logger, pool *rtps.Pool, graphs ros2camera.GraphSource) *Bridge {
	return newBridge(logger, graphs, func(ctx context.Context, cfg rtps.Config) (participantLease, error) {
		lease, err := pool.Acquire(ctx, cfg)
		if err != nil {
			return nil, err
		}
		return poolLease{lease}, nil
	})
}

func newBridge(logger *zap.Logger, graphs ros2camera.GraphSource, acquire acquireFunc) *Bridge {
	return &Bridge{
		logger: logger, graphs: graphs, acquire: acquire, now: time.Now, network: rtps.NetworkNamespaceID,
		wake:     make(chan struct{}, 1),
		channels: map[*Channel]struct{}{}, topics: map[string]*Channel{},
		leases: map[string]*graphLease{}, announced: map[string]string{},
		reserved: map[string]string{}, segments: map[string]int{},
	}
}

// Run keeps the bridge's participants in step with the open channels and the
// running ROS 2 apps until ctx is cancelled, then leaves every app's network.
func (b *Bridge) Run(ctx context.Context) {
	ticker := time.NewTicker(reconcileInterval)
	defer ticker.Stop()
	for {
		b.reconcile(ctx)
		select {
		case <-ctx.Done():
			b.mu.Lock()
			leases := b.leases
			b.leases = map[string]*graphLease{}
			b.mu.Unlock()
			for _, gl := range leases {
				_ = gl.lease.Close()
			}
			return
		case <-ticker.C:
		case <-b.wake:
		}
	}
}

func (b *Bridge) wakeUp() {
	select {
	case b.wake <- struct{}{}:
	default:
	}
}

var _ mcusource.GenericSink = (*Bridge)(nil)

// Mount starts publishing the generic sensor channels of a paired source, as
// the supervisor asks of every sink for such channels. It returns the channels
// it took, a function that publishes one frame of them and a function that
// stops publishing them all.
func (b *Bridge) Mount(p mcusource.SensorPairing, descriptors []*sensorlinkpb.SensorDescriptor) ([]uint32, func(*sensorlink.SensorFrame) bool, func()) {
	channels := make(map[uint32]*Channel, len(descriptors))
	accepted := make([]uint32, 0, len(descriptors))
	for _, d := range descriptors {
		id := d.GetChannelId()
		if channels[id] != nil {
			continue
		}
		c, err := b.OpenChannel(p.Key(), p.Name, d)
		if err != nil {
			b.logger.Warn("sensor channel is not published to ROS 2 apps",
				zap.String("source", p.Key()), zap.Uint32("channel", id), zap.Error(err))
			continue
		}
		channels[id] = c
		accepted = append(accepted, id)
	}
	var mu sync.Mutex
	released := false
	publish := func(f *sensorlink.SensorFrame) bool {
		if f == nil {
			return false
		}
		mu.Lock()
		c, done := channels[f.ChannelID], released
		mu.Unlock()
		if c == nil || done {
			return false
		}
		return c.WriteFrame(f)
	}
	release := func() {
		mu.Lock()
		done := released
		released = true
		mu.Unlock()
		if done {
			return
		}
		for _, c := range channels {
			_ = c.Close()
		}
	}
	return accepted, publish, release
}

// Channel is one sensor channel being published. It is safe for concurrent
// use.
type Channel struct {
	b *Bridge
	// id names the channel of its source across connections.
	id    string
	topic string
	enc   encoding
	// source identifies the paired source, and frameID names it in message
	// headers.
	source, frameID string
	// sampleBytes is the size of one reading.
	sampleBytes int
	// period is the time between two readings, zero when the source does not
	// say how often it reads.
	period time.Duration
	// opened is the start of the steady time the source's clock is placed on.
	opened time.Time

	clockMu sync.Mutex
	clock   sourceClock

	// Guarded by b.mu.
	closed   bool
	lastWarn time.Time
}

// Topic is the ROS 2 topic the channel is published on.
func (c *Channel) Topic() string { return c.topic }

// OpenChannel starts publishing a generic sensor channel of a paired source.
// sourceKey identifies the source; sourceName is the name it was paired under
// and may be empty. Audio and video channels are refused: they are delivered
// as microphones and cameras, which an app needs an entitlement to read, and
// a ROS 2 topic has no such gate.
func (b *Bridge) OpenChannel(sourceKey, sourceName string, d *sensorlinkpb.SensorDescriptor) (*Channel, error) {
	if d.GetSensor() == nil {
		return nil, errors.New("ros2sensor: only generic sensor channels are published to ROS 2")
	}
	source := sourceToken(sourceName, sourceKey)
	base := topicFor(sourceName, sourceKey, d.GetName(), d.GetChannelId())
	frameID := "sensorlink/" + source
	c := &Channel{b: b, enc: encodingFor(d.GetSensor(), frameID), source: sourceKey, frameID: frameID,
		sampleBytes: int(d.GetSensor().GetSampleBytes()), opened: b.now()}
	if rate := d.GetSensor().GetRateHz(); rate > 0 {
		c.period = time.Second / time.Duration(rate)
	}

	c.id = fmt.Sprintf("%s/%d", sourceKey, d.GetChannelId())

	b.mu.Lock()
	// The first name that is this channel's own or nobody's, and not in use
	// by another connection of the same channel.
	c.topic = base
	for n := 2; ; n++ {
		owner, taken := b.reserved[c.topic]
		if (!taken || owner == c.id) && b.topics[c.topic] == nil {
			break
		}
		c.topic = fmt.Sprintf("%s_%d", base, n)
	}
	b.reserved[c.topic] = c.id
	b.topics[c.topic] = c
	b.channels[c] = struct{}{}
	c.clock.segment = b.segments[c.id]
	fresh := b.announced[c.id] != c.topic
	b.announced[c.id] = c.topic
	b.mu.Unlock()

	if fresh {
		b.logger.Info("publishing sensor channel to ROS 2 apps",
			zap.String("source", sourceKey), zap.String("channel", d.GetName()),
			zap.String("topic", c.topic), zap.String("type", c.enc.rosType),
			zap.String("timing_topic", c.topic+timingSuffix), zap.String("timing_type", timingROSType))
	}
	b.wakeUp()
	return c, nil
}

// place works out when the first reading of a frame was taken, on this
// device's wall clock, and how that is known. It is the time the source
// stamped the frame with, moved to this device's clock. A frame its source did
// not stamp, or stamped with a value that cannot be a time, is placed at its
// arrival: nothing else is known about it.
func (c *Channel) place(f *sensorlink.SensorFrame, arrival time.Time) timing {
	t := timing{stamp: arrival, sequence: f.Seq, period: c.period, sourceUs: f.TsUs, uncertaintyUs: f.TsUncertaintyUs}
	if c.sampleBytes > 0 {
		t.readings = len(f.Payload) / c.sampleBytes
	}
	if f.TsUs == 0 || f.TsUs > math.MaxInt64/uint64(time.Microsecond) {
		return t
	}
	// The frame's last reading was taken this long after its first.
	var span time.Duration
	if t.readings > 1 {
		span = time.Duration(t.readings-1) * c.period
	}
	steady := arrival.Sub(c.opened)
	c.clockMu.Lock()
	taken, uncertainty := c.clock.place(time.Duration(f.TsUs)*time.Microsecond, span, steady, c.period)
	t.segment = c.clock.segment
	c.clockMu.Unlock()
	t.stamp, t.placed, t.mapping = arrival.Add(taken-steady), true, uncertainty
	return t
}

// WriteFrame publishes one frame of the channel, stamped with the time its
// source took it, and reports whether any of it could be turned into a
// message. How that time is known goes out on the channel's timing topic
// first, one message per frame. A frame that cannot be published is dropped
// and reported in the log; the channel stays open.
func (c *Channel) WriteFrame(f *sensorlink.SensorFrame) bool {
	// Read the clock before anything else: the frame is placed by when it
	// arrived, and time spent waiting for a lock below is not part of that.
	arrival := c.b.now()
	when := c.place(f, arrival)
	messages, err := c.enc.encode(when.stamp, c.period, f.Payload)
	// A message has to fit one datagram. One that does not would fail on
	// every writer, so it is dropped here, before its timing goes out.
	fits := messages[:0]
	for _, message := range messages {
		if len(message) > rtps.MaxPayload {
			err = fmt.Errorf("a message of %d bytes is larger than the %d bytes this agent can publish at once", len(message), rtps.MaxPayload)
			continue
		}
		fits = append(fits, message)
	}
	messages = fits
	var timingMessage []byte
	if len(messages) > 0 {
		timingMessage = when.encode(c.frameID, c.topic, c.source)
	}
	b := c.b
	b.mu.Lock()
	if c.closed {
		b.mu.Unlock()
		return false
	}
	writers := make([]channelWriters, 0, len(b.leases))
	for _, gl := range b.leases {
		if w := gl.writers[c]; w != nil {
			writers = append(writers, *w)
		}
	}
	b.mu.Unlock()
	for _, w := range writers {
		if w.timing != nil && timingMessage != nil {
			if werr := w.timing.Write(timingMessage); werr != nil && err == nil {
				err = werr
			}
		}
		if w.readings == nil {
			continue
		}
		for _, message := range messages {
			if werr := w.readings.Write(message); werr != nil && err == nil {
				err = werr
			}
		}
	}
	if err == nil {
		return true
	}
	b.mu.Lock()
	warn := time.Since(c.lastWarn) >= warnInterval
	if warn {
		c.lastWarn = time.Now()
	}
	b.mu.Unlock()
	if warn {
		b.logger.Warn("sensor sample was not published to ROS 2", zap.String("topic", c.topic), zap.Error(err))
	}
	return len(messages) > 0
}

// Close stops publishing the channel and frees its topic name.
func (c *Channel) Close() error {
	b := c.b
	b.mu.Lock()
	if c.closed {
		b.mu.Unlock()
		return nil
	}
	c.closed = true
	delete(b.channels, c)
	delete(b.topics, c.topic)
	c.clockMu.Lock()
	b.segments[c.id] = c.clock.segment
	c.clockMu.Unlock()
	var writers []sampleWriter
	for _, gl := range b.leases {
		if w := gl.writers[c]; w != nil {
			for _, topic := range []sampleWriter{w.readings, w.timing} {
				if topic != nil {
					writers = append(writers, topic)
				}
			}
			delete(gl.writers, c)
		}
	}
	b.mu.Unlock()
	for _, w := range writers {
		_ = w.Close()
	}
	b.wakeUp()
	return nil
}

// networksToJoin works out where the open channels may be published: one
// participant per network and ROS domain of the running ROS 2 apps, and only
// in networks where every such app holds sensor-read.
//
// A topic is readable by everything in the network it is published in. Apps
// on the host's network share it, and so do the services of an app that share
// a network; any of them can join any ROS domain. So the unit is the network,
// told apart by its namespace rather than by what an app's configuration
// says, and one app there without the entitlement keeps the topics out of it.
func (b *Bridge) networksToJoin(ctx context.Context, graphs []ros2camera.Graph) map[string]rtps.Config {
	type network struct {
		everyAppMayRead bool
		// through is the app each domain's participant enters the network by.
		through map[int]ros2camera.Graph
	}
	networks := map[string]*network{}
	for _, g := range graphs {
		if g.DomainID < 0 || g.DomainID > 232 || g.NetworkNamespacePID == 0 {
			continue
		}
		name, err := b.network(g.NetworkNamespacePID)
		if err != nil {
			// Most likely the app is just stopping. Without knowing its
			// network nothing can be published for it.
			b.logger.Debug("the network of a ROS 2 app could not be identified", zap.String("app", g.Key), zap.Error(err))
			continue
		}
		n := networks[name]
		if n == nil {
			n = &network{everyAppMayRead: true, through: map[int]ros2camera.Graph{}}
			networks[name] = n
		}
		if !g.SensorRead {
			n.everyAppMayRead = false
		}
		// The lowest instance key, so that the choice does not flap.
		if current, ok := n.through[g.DomainID]; !ok || g.InstanceKey < current.InstanceKey {
			n.through[g.DomainID] = g
		}
	}
	own, ownErr := b.network(0)
	desired := map[string]rtps.Config{}
	for name, n := range networks {
		if !n.everyAppMayRead {
			continue
		}
		for domain, g := range n.through {
			cfg := rtps.Config{DomainID: domain, Interface: loopback, Publish: true}
			// The agent is already in the host's network; any other is
			// entered through one of its apps.
			if ownErr != nil || name != own {
				cfg.NetworkNamespacePID = g.NetworkNamespacePID
				if verify := g.Verify; verify != nil {
					cfg.VerifyNetworkNamespace = func() bool { return verify(ctx) }
				}
			}
			desired[fmt.Sprintf("%s:%d", name, domain)] = cfg
		}
	}
	return desired
}

// reconcile brings the participants in step with the open channels and the
// running ROS 2 apps: one participant per network and domain that may be
// published into while any channel is open, with writers for every channel,
// and none otherwise.
func (b *Bridge) reconcile(ctx context.Context) {
	b.reconcileMu.Lock()
	defer b.reconcileMu.Unlock()

	b.mu.Lock()
	publishing := len(b.channels) > 0
	b.mu.Unlock()

	desired := map[string]rtps.Config{}
	listed := true
	if publishing {
		graphs, err := b.graphs(ctx)
		if err != nil {
			// One failed listing is no reason to leave the apps being
			// published into. But the list is also what says who may read
			// the topics, so when it stays unreadable publishing stops.
			b.listFailures++
			listed = b.listFailures > listFailuresTolerated
			graphs = nil
			b.logger.Debug("listing ROS 2 apps for sensor channels failed", zap.Int("times", b.listFailures), zap.Error(err))
		} else {
			b.listFailures = 0
		}
		desired = b.networksToJoin(ctx, graphs)
	}

	b.mu.Lock()
	var stale []participantLease
	for key, gl := range b.leases {
		_, wanted := desired[key]
		gone := false
		select {
		case <-gl.lease.Done():
			gone = true
		default:
		}
		if gone || (listed && !wanted) {
			stale = append(stale, gl.lease)
			delete(b.leases, key)
		}
	}
	b.mu.Unlock()
	for _, lease := range stale {
		_ = lease.Close()
	}

	for key, cfg := range desired {
		b.mu.Lock()
		_, have := b.leases[key]
		b.mu.Unlock()
		if have {
			continue
		}
		lease, err := b.acquire(ctx, cfg)
		if err != nil {
			b.logger.Debug("joining a ROS 2 app to publish sensor channels failed; will retry",
				zap.String("graph", key), zap.Error(err))
			continue
		}
		b.mu.Lock()
		b.leases[key] = &graphLease{lease: lease, writers: map[*Channel]*channelWriters{}}
		b.mu.Unlock()
	}

	// A participant has room for a limited number of writers. Every channel's
	// readings are opened before any channel's timing, so that when they run
	// out it is timing that goes without.
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, timingTopic := range []bool{false, true} {
		for key, gl := range b.leases {
			for c := range b.channels {
				w := gl.writers[c]
				if w == nil {
					w = &channelWriters{}
					gl.writers[c] = w
				}
				writer, pub := &w.readings, rtps.Publication{Topic: "rt" + c.topic, Type: c.enc.ddsType}
				if timingTopic {
					writer, pub = &w.timing, rtps.Publication{Topic: "rt" + c.topic + timingSuffix, Type: timingDDSType}
				}
				if *writer != nil {
					continue
				}
				opened, err := gl.lease.NewWriter(pub)
				if err != nil {
					b.logger.Debug("opening a sensor channel in a ROS 2 app failed; will retry",
						zap.String("graph", key), zap.String("topic", pub.Topic), zap.Error(err))
					continue
				}
				*writer = opened
			}
		}
	}
}
