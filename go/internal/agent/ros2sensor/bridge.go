package ros2sensor

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/ros2camera"
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

// Bridge publishes open sensor channels into every running ROS 2 app. It
// joins an app's ROS 2 network only while there is a channel to publish.
type Bridge struct {
	logger  *zap.Logger
	graphs  ros2camera.GraphSource
	acquire acquireFunc

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

// graphLease is the participant in one graph and its writer per channel.
type graphLease struct {
	lease   participantLease
	writers map[*Channel]sampleWriter
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
		logger: logger, graphs: graphs, acquire: acquire,
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

// Channel is one sensor channel being published. It is safe for concurrent
// use.
type Channel struct {
	b     *Bridge
	topic string
	enc   encoding

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
	c := &Channel{b: b, enc: encodingFor(d.GetSensor(), "sensorlink/"+source)}

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
			zap.String("topic", c.topic), zap.String("type", c.enc.rosType))
	}
	b.wakeUp()
	return c, nil
}

// WriteSample publishes the payload of one frame of the channel. A frame
// that cannot be published is dropped and reported in the log; the channel
// stays open.
func (c *Channel) WriteSample(payload []byte) {
	messages, err := c.enc.encode(time.Now(), payload)
	b := c.b
	b.mu.Lock()
	if c.closed {
		b.mu.Unlock()
		return
	}
	writers := make([]sampleWriter, 0, len(b.leases))
	for _, gl := range b.leases {
		if w := gl.writers[c]; w != nil {
			writers = append(writers, w)
		}
	}
	b.mu.Unlock()
	for _, message := range messages {
		for _, w := range writers {
			if werr := w.Write(message); werr != nil && err == nil {
				err = werr
			}
		}
	}
	if err == nil {
		return
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
			writers = append(writers, w)
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
		for _, g := range graphs {
			if g.DomainID < 0 || g.DomainID > 232 || g.NetworkNamespacePID == 0 {
				continue
			}
			cfg := rtps.Config{DomainID: g.DomainID, Interface: loopback, Publish: true}
			key := fmt.Sprintf("host:%d", g.DomainID)
			if !g.HostNetwork {
				cfg.NetworkNamespacePID = g.NetworkNamespacePID
				if verify := g.Verify; verify != nil {
					cfg.VerifyNetworkNamespace = func() bool { return verify(ctx) }
				}
				key = fmt.Sprintf("%s:%d:%d", g.InstanceKey, g.NetworkNamespacePID, g.DomainID)
			}
			desired[key] = cfg
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
		b.leases[key] = &graphLease{lease: lease, writers: map[*Channel]sampleWriter{}}
		b.mu.Unlock()
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	for key, gl := range b.leases {
		for c := range b.channels {
			if gl.writers[c] != nil {
				continue
			}
			w, err := gl.lease.NewWriter(rtps.Publication{Topic: "rt" + c.topic, Type: c.enc.ddsType})
			if err != nil {
				b.logger.Debug("opening a sensor channel in a ROS 2 app failed; will retry",
					zap.String("graph", key), zap.String("topic", c.topic), zap.Error(err))
				continue
			}
			gl.writers[c] = w
		}
	}
}
