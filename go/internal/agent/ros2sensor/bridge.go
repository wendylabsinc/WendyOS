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

	// reconcileMu admits one reconcile at a time.
	reconcileMu sync.Mutex
	wake        chan struct{}

	mu       sync.Mutex
	channels map[*Channel]struct{}
	// topics maps each topic in use to its channel, so no two share one.
	topics map[string]*Channel
	// leases holds the participant in each app's graph, by graph.
	leases map[string]*graphLease
	// announced remembers the topic last logged for a source's channel, so a
	// source that reconnects is not logged again.
	announced map[string]string
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
		logger: logger, graphs: graphs, acquire: acquire, now: time.Now,
		wake:     make(chan struct{}, 1),
		channels: map[*Channel]struct{}{}, topics: map[string]*Channel{},
		leases: map[string]*graphLease{}, announced: map[string]string{},
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
	b     *Bridge
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

	b.mu.Lock()
	c.topic = base
	for n := 2; b.topics[c.topic] != nil; n++ {
		c.topic = fmt.Sprintf("%s_%d", base, n)
	}
	b.topics[c.topic] = c
	b.channels[c] = struct{}{}
	id := fmt.Sprintf("%s/%d", sourceKey, d.GetChannelId())
	fresh := b.announced[id] != c.topic
	b.announced[id] = c.topic
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
	t := timing{stamp: arrival, sequence: f.Seq, period: c.period, sourceUs: f.TsUs}
	if c.sampleBytes > 0 {
		t.readings = len(f.Payload) / c.sampleBytes
	}
	if f.TsUs == 0 || f.TsUs > math.MaxInt64/uint64(time.Microsecond) {
		return t
	}
	steady := arrival.Sub(c.opened)
	c.clockMu.Lock()
	taken, uncertainty := c.clock.place(time.Duration(f.TsUs)*time.Microsecond, steady, c.period)
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

// reconcile brings the participants in step with the open channels and the
// running ROS 2 apps: one participant per app network and domain while any
// channel is open, with a writer for every channel, and none otherwise.
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
			// Not knowing which apps run is no reason to leave the ones we
			// are publishing into.
			listed = false
			b.logger.Debug("listing ROS 2 apps for sensor channels failed", zap.Error(err))
		}
		// A topic is readable by everything in the network it is published
		// in, so it only goes where sensor-read allows it: into the network
		// of an app that holds the entitlement, and onto the host's network
		// only while every ROS 2 app there, on that domain, holds it.
		hostDomainOpen := map[int]bool{}
		for _, g := range graphs {
			if g.DomainID < 0 || g.DomainID > 232 || g.NetworkNamespacePID == 0 {
				continue
			}
			if g.HostNetwork {
				open, seen := hostDomainOpen[g.DomainID]
				hostDomainOpen[g.DomainID] = g.SensorRead && (open || !seen)
				continue
			}
			if !g.SensorRead {
				continue
			}
			cfg := rtps.Config{DomainID: g.DomainID, Interface: loopback, Publish: true, NetworkNamespacePID: g.NetworkNamespacePID}
			if verify := g.Verify; verify != nil {
				cfg.VerifyNetworkNamespace = func() bool { return verify(ctx) }
			}
			desired[fmt.Sprintf("%s:%d:%d", g.InstanceKey, g.NetworkNamespacePID, g.DomainID)] = cfg
		}
		for domain, open := range hostDomainOpen {
			if open {
				desired[fmt.Sprintf("host:%d", domain)] = rtps.Config{DomainID: domain, Interface: loopback, Publish: true}
			}
		}
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

	b.mu.Lock()
	defer b.mu.Unlock()
	for key, gl := range b.leases {
		for c := range b.channels {
			w := gl.writers[c]
			if w == nil {
				w = &channelWriters{}
				gl.writers[c] = w
			}
			for _, topic := range []struct {
				writer *sampleWriter
				pub    rtps.Publication
			}{
				{&w.readings, rtps.Publication{Topic: "rt" + c.topic, Type: c.enc.ddsType}},
				{&w.timing, rtps.Publication{Topic: "rt" + c.topic + timingSuffix, Type: timingDDSType}},
			} {
				if *topic.writer != nil {
					continue
				}
				opened, err := gl.lease.NewWriter(topic.pub)
				if err != nil {
					b.logger.Debug("opening a sensor channel in a ROS 2 app failed; will retry",
						zap.String("graph", key), zap.String("topic", topic.pub.Topic), zap.Error(err))
					continue
				}
				*topic.writer = opened
			}
		}
	}
}
