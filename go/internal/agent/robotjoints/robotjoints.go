// Package robotjoints reads a robot's live joint positions off the device's DDS
// graph.
//
// It exists on the agent rather than in the CLI because DDS discovery is
// multicast: a robot's ROS 2 graph is confined to the network segment the robot
// is on, and a laptop cannot see it across a routed link or a cloud tunnel
// however reachable the device is. The agent is already standing inside that
// segment — hoststats/rosbattery joins the same domain to read the battery —
// so this follows that package's shape: candidate interfaces tried in turn, a
// discovery window, a subscription, and a decoder that is a pure function of
// bytes.
//
// It is read-only. There is nothing here that writes to a DDS topic, which is
// what keeps a nothing-powered calibration nothing-powered.
package robotjoints

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/rtps"
	"github.com/wendylabsinc/wendy/go/internal/shared/rosmsg"
)

// BackendUnitreeLowState reads a Unitree humanoid's whole body off
// unitree_hg/msg/LowState. It is the name a robot profile selects with
// joints.source.backend, and the platform owns the set: a profile picks one of
// these and parameterises it, it does not describe a new one.
const BackendUnitreeLowState = "unitree-lowstate"

// Timings. The discovery window is what separates "the robot is not publishing"
// from "SEDP has not finished yet"; the silence window is what turns a stream
// that stops mid-sweep into an error rather than a frozen pose.
const (
	// DiscoverWindow is how long one interface is given to announce a writer
	// for the topic before the next one is tried. Discovery is announcement
	// driven, so a graph appears over a second or two rather than on request;
	// rosbattery waits minutes because nobody is watching it, and an operator
	// standing at a robot is.
	DiscoverWindow = 6 * time.Second
	// SilenceWindow is how long a subscribed stream may go quiet before it is
	// reported as absent. A body-state topic publishes at hundreds of hertz, so
	// a second of silence is already the publisher having stopped.
	SilenceWindow = 3 * time.Second
	// DefaultMinInterval throttles delivery to roughly the rate the calibration
	// wizard polls at. The robot publishes far faster than a hand moves and this
	// stream crosses a cloud tunnel, so the surplus is dropped at the source.
	DefaultMinInterval = 20 * time.Millisecond
	// MaxMinInterval bounds what a client may ask for. A sweep that samples
	// slower than this is not measuring a moving joint any more.
	MaxMinInterval = time.Second
)

// Sentinel errors. The callers that matter — the gRPC service, and through it
// the CLI — map these onto codes an operator can act on, so they are values
// rather than message text.
var (
	// ErrSourceAbsent is the device having listened and heard nothing. It is
	// deliberately not an error about the transport: the robot is reachable and
	// silent, which is a different problem from not being able to listen.
	ErrSourceAbsent = errors.New("nothing published the joint topic")
	// ErrNoInterface is the device having nowhere to listen — no multicast
	// capable interface survived the eligibility filter, or an interface the
	// caller named does not exist. Nothing was heard because nothing listened.
	ErrNoInterface = errors.New("no interface to join a DDS domain on")
	// ErrUnknownBackend is a joint source this agent cannot read.
	ErrUnknownBackend = errors.New("unknown joint source backend")
)

// Slot is one slot of the robot's joint array that is reporting.
//
// Only reporting slots are ever built. A vendor message carries a fixed number
// of slots and a given robot drives fewer of them, with no field saying which,
// so an idle slot is left out rather than emitted as a zero angle — a zero reads
// as a joint sitting perfectly still, which is exactly the false calibration the
// wizard's rules exist to prevent.
type Slot struct {
	// Index is the message's own index. It never shifts because a slot is idle,
	// which is what lets a client resolve it to a joint name through the robot's
	// profile, by index.
	Index int
	// Position is in Reading.Unit, as the robot published it. Nothing here
	// converts: a converted number invites a comparison against a limit quoted
	// in the other unit.
	Position float64
}

// Reading is one observation of the robot's joints.
type Reading struct {
	At time.Time
	// SlotCount is how many slots the message carries, including the idle ones
	// missing from Slots. Reporting both is what keeps the idle-slot rule from
	// hiding anything: a reader sees 35 slots and 27 reporting.
	SlotCount int
	Slots     []Slot
	Unit      string
}

// decoder turns one DDS payload into a Reading.
type decoder struct {
	// ddsType is what a writer for this backend advertises over SEDP.
	ddsType string
	// topic is the backend's default topic, in its ROS spelling.
	topic  string
	decode func([]byte) (Reading, error)
}

// backends is the fixed set. A switch on vendor names anywhere else is one
// refactor away from a switch that changes behaviour, so this is the only place
// the platform knows which robot family it is reading.
var backends = map[string]decoder{
	BackendUnitreeLowState: {
		ddsType: rosmsg.TypeHGLowState,
		topic:   "/lowstate",
		decode:  decodeUnitreeLowState,
	},
}

// Backends names every joint source this build can read, sorted, for a refusal
// that tells the operator what to do next.
func Backends() []string {
	names := make([]string, 0, len(backends))
	for name := range backends {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// decodeUnitreeLowState reads a unitree_hg/msg/LowState into a Reading.
//
// The wire decoding itself is rosmsg's, shared with `wendy device robot inspect`
// and validated against bytes a real G1 sent, so there is one decoder for this
// layout rather than one per caller. Which slots are real is rosmsg's rule too,
// for the same reason: this side and the CLI must agree about what "idle" means
// or a calibration is taken against a joint that was never there.
func decodeUnitreeLowState(payload []byte) (Reading, error) {
	state, err := rosmsg.DecodeHGLowState(payload)
	if err != nil {
		return Reading{}, err
	}
	live := state.LiveMotors()
	reading := Reading{
		SlotCount: len(state.Motors),
		Slots:     make([]Slot, 0, len(live)),
		// unitree_hg publishes q in radians. Stated rather than assumed, because
		// the platform refuses a budget quoted in a different unit and that
		// refusal is only worth anything if the unit is carried.
		Unit: "rad",
	}
	for _, index := range live {
		reading.Slots = append(reading.Slots, Slot{
			Index:    index,
			Position: float64(state.Motors[index].Position),
		})
	}
	return reading, nil
}

// Config parameterises one stream.
type Config struct {
	// Backend is the profile's joints.source.backend. Required: guessing which
	// robot this is, is the failure the whole design avoids.
	Backend string
	// Topic overrides the backend's default, in its ROS spelling.
	Topic string
	// DomainID is the DDS domain. Zero is ROS 2's default.
	DomainID int
	// Interface pins discovery to one of the device's interfaces. Empty tries
	// every eligible one in turn — a robot's DDS often lives on a private
	// segment beside the device's other networks, so picking the first
	// multicast-capable interface finds it only by luck.
	Interface string
	// MinInterval throttles delivery. Zero takes DefaultMinInterval.
	MinInterval time.Duration
}

// Lease is the part of an rtps.Lease this package uses. Taking it as an
// interface is what lets the discovery, throttling and silence rules be driven
// against recorded endpoints and samples, with no DDS domain and no robot — the
// same seam robotprobe's DDS reader takes for the same reason.
type Lease interface {
	Endpoints() []rtps.Endpoint
	Changed() <-chan struct{}
	Subscribe(rtps.Endpoint) error
	Samples() <-chan rtps.Sample
	Done() <-chan struct{}
	Close() error
}

// Reader streams joint readings from whichever interface can hear the robot.
type Reader struct {
	acquire func(context.Context, rtps.Config) (Lease, error)
	cfg     Config
	dec     decoder
	logf    func(string, ...any)
	// now is the clock, injected so a test can drive the throttle without
	// sleeping through it.
	now func() time.Time
	// discoverWindow and silenceWindow default to the package constants and are
	// fields so a test can exercise "the robot never spoke" and "the robot
	// stopped speaking" in milliseconds rather than in seconds.
	discoverWindow time.Duration
	silenceWindow  time.Duration
	// candidateInterfaces enumerates the device's eligible interfaces, injected
	// so the try-each-in-turn rule can be driven without a host that happens to
	// have the right NICs.
	candidateInterfaces func() ([]string, error)
}

// NewReader validates cfg against the backends this build has and binds it to a
// participant pool. An unknown backend is refused here rather than at the first
// sample, so the caller can say so before an operator is asked to touch a robot.
func NewReader(cfg Config, pool *rtps.Pool, logf func(string, ...any)) (*Reader, error) {
	if pool == nil {
		return nil, errors.New("robotjoints: no participant pool: this agent cannot join a DDS domain")
	}
	if cfg.Backend == "" {
		return nil, fmt.Errorf("%w: no backend named (this agent can read: %s)",
			ErrUnknownBackend, strings.Join(Backends(), ", "))
	}
	dec, ok := backends[cfg.Backend]
	if !ok {
		return nil, fmt.Errorf("%w %q (this agent can read: %s)",
			ErrUnknownBackend, cfg.Backend, strings.Join(Backends(), ", "))
	}
	if cfg.Topic == "" {
		cfg.Topic = dec.topic
	}
	switch {
	case cfg.MinInterval <= 0:
		cfg.MinInterval = DefaultMinInterval
	case cfg.MinInterval > MaxMinInterval:
		cfg.MinInterval = MaxMinInterval
	}
	if logf == nil {
		logf = func(string, ...any) {}
	}
	acquire := func(ctx context.Context, target rtps.Config) (Lease, error) {
		return pool.Acquire(ctx, target)
	}
	return &Reader{
		acquire:             acquire,
		cfg:                 cfg,
		dec:                 dec,
		logf:                logf,
		now:                 time.Now,
		discoverWindow:      DiscoverWindow,
		silenceWindow:       SilenceWindow,
		candidateInterfaces: rtps.HostInterfaces,
	}, nil
}

// Describe names what this reader is reading, for an error message that tells an
// operator where to look.
func (r *Reader) Describe() string {
	return fmt.Sprintf("%s on %s [%s]", r.cfg.Backend, r.cfg.Topic, r.dec.ddsType)
}

// Stream delivers readings to emit until ctx is done, emit returns an error, or
// the robot stops publishing.
//
// It returns nil only when ctx ended it: every other ending is something the
// caller has to be able to tell apart. ErrNoInterface and ErrSourceAbsent are
// the two that matter — "we could not listen" and "we listened and heard
// nothing" send an operator to different machines — and a decode failure comes
// back as itself, because a topic that is there and unreadable is neither.
func (r *Reader) Stream(ctx context.Context, emit func(Reading) error) error {
	ifaces, err := r.candidates()
	if err != nil {
		return err
	}

	// Whether anything has been delivered decides what a later silence means.
	// Before the first reading, silence on one interface just means the robot is
	// on another; after it, this interface was the robot and it has gone quiet,
	// which the caller needs told now rather than after every other interface
	// has been given its discovery window.
	var delivered bool
	deliver := func(reading Reading) error {
		delivered = true
		return emit(reading)
	}

	var lastAbsent, lastJoin error
	for _, iface := range ifaces {
		if err := ctx.Err(); err != nil {
			return nil
		}
		err := r.streamOn(ctx, iface, deliver)
		switch {
		case err == nil:
			return nil
		case errors.Is(err, ErrSourceAbsent):
			if delivered {
				return err
			}
			// Not fatal on its own: the robot may be on the next interface.
			lastAbsent = err
			r.logf("robot joints: %v", err)
		case errors.Is(err, ErrNoInterface):
			// This one could not be joined; another still might. A device with
			// a robot on one NIC and a container bridge on another must not be
			// stopped by the bridge.
			lastJoin = err
			r.logf("robot joints: %v", err)
		default:
			return err
		}
	}
	// Having listened anywhere is the stronger fact, so it is the one reported:
	// "we heard nothing" beats "one of the interfaces would not open".
	switch {
	case lastAbsent != nil:
		return lastAbsent
	case lastJoin != nil:
		return lastJoin
	default:
		return fmt.Errorf("%w: %s, on %s", ErrSourceAbsent, r.Describe(), strings.Join(ifaces, ", "))
	}
}

// candidates is the interface list to try, in order.
//
// A named interface is taken verbatim — naming one is how an operator overrides
// the eligibility filter, including to force a wireless interface on a robot
// whose link really is WiFi. There is deliberately no "let rtps auto-select"
// fallback: auto-select takes the first multicast-capable interface, which on a
// host with nothing eligible means WiFi or a container bridge, and emitting
// discovery traffic there is worse than saying we have nowhere to listen.
func (r *Reader) candidates() ([]string, error) {
	if r.cfg.Interface != "" {
		return []string{r.cfg.Interface}, nil
	}
	ifaces, _ := r.candidateInterfaces()
	if len(ifaces) == 0 {
		return nil, fmt.Errorf("%w: no wired multicast-capable interface on this device; "+
			"name one in the joint source's `interface` parameter to override the filter",
			ErrNoInterface)
	}
	return ifaces, nil
}

// streamOn runs one interface: join, wait for the writer, subscribe, decode.
func (r *Reader) streamOn(ctx context.Context, iface string, emit func(Reading) error) error {
	lease, err := r.acquire(ctx, rtps.Config{
		DomainID:  r.cfg.DomainID,
		Interface: iface,
		Logf:      r.logf,
	})
	if err != nil {
		if ctx.Err() != nil {
			return nil
		}
		// Joining failed, which is the device being unable to listen rather
		// than the robot being silent.
		return fmt.Errorf("%w: joining DDS domain %d on %q: %w", ErrNoInterface, r.cfg.DomainID, iface, err)
	}
	defer func() { _ = lease.Close() }()

	endpoint, ok := r.await(ctx, lease)
	if !ok {
		if ctx.Err() != nil {
			return nil
		}
		return fmt.Errorf("%w: %s did not appear on %q within %s", ErrSourceAbsent, r.Describe(), iface, r.discoverWindow)
	}
	if err := lease.Subscribe(endpoint); err != nil {
		return fmt.Errorf("subscribing to %s on %q: %w", r.cfg.Topic, iface, err)
	}
	r.logf("robot joints: reading %s [%s] on %q", endpoint.Topic, endpoint.Type, iface)
	return r.consume(ctx, lease, endpoint, iface, emit)
}

// await waits for a writer matching the configured topic and type to be
// announced, returning as soon as one is rather than sitting out the window.
func (r *Reader) await(ctx context.Context, lease Lease) (rtps.Endpoint, bool) {
	deadline := time.NewTimer(r.discoverWindow)
	defer deadline.Stop()
	for {
		if endpoint, ok := r.match(lease.Endpoints()); ok {
			return endpoint, true
		}
		select {
		case <-lease.Changed():
		case <-deadline.C:
			// One last look: a writer announced between the final Changed()
			// and the timer firing is present, and losing it would report a
			// publishing robot as silent.
			return r.match(lease.Endpoints())
		case <-lease.Done():
			return rtps.Endpoint{}, false
		case <-ctx.Done():
			return rtps.Endpoint{}, false
		}
	}
}

// match finds the writer for the configured topic and type.
//
// Both spellings of the topic are accepted: ROS 2 mangles topic names on the DDS
// wire by prefixing "rt/", so /lowstate is published as rt/lowstate and a caller
// should never have to know that.
func (r *Reader) match(endpoints []rtps.Endpoint) (rtps.Endpoint, bool) {
	wire := "rt/" + strings.TrimPrefix(r.cfg.Topic, "/")
	for _, ep := range endpoints {
		if ep.Type != r.dec.ddsType {
			continue
		}
		if ep.Topic == r.cfg.Topic || ep.Topic == wire {
			return ep, true
		}
	}
	return rtps.Endpoint{}, false
}

// consume decodes samples until the topic goes quiet, the client stops
// listening, or a payload does not decode.
//
// Silence is an ending rather than a pause. A sweep reads "the newest position"
// continuously while an operator moves an arm, so a stream that stops and leaves
// the last pose standing would be read as a joint holding perfectly still — the
// same failure an idle slot reported as zero would cause, in time rather than in
// space.
func (r *Reader) consume(ctx context.Context, lease Lease, endpoint rtps.Endpoint, iface string, emit func(Reading) error) error {
	silence := time.NewTimer(r.silenceWindow)
	defer silence.Stop()

	var delivered int
	var lastEmit time.Time
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-lease.Done():
			return fmt.Errorf("%w: the DDS participant reading %s on %q went away after %d samples",
				ErrSourceAbsent, r.cfg.Topic, iface, delivered)
		case <-silence.C:
			return fmt.Errorf("%w: %s stopped publishing after %d samples (nothing for %s)",
				ErrSourceAbsent, r.Describe(), delivered, r.silenceWindow)
		case sample := <-lease.Samples():
			if sample.Writer != endpoint.GUID {
				continue
			}
			silence.Reset(r.silenceWindow)
			// Throttle before decoding: the surplus of a several-hundred-hertz
			// body-state topic is not worth the CDR walk, let alone the tunnel.
			now := r.now()
			if delivered > 0 && now.Sub(lastEmit) < r.cfg.MinInterval {
				continue
			}
			reading, err := r.dec.decode(sample.Payload)
			if err != nil {
				// A layout this decoder does not recognise is reported, never
				// decoded into plausible wrong angles. It is not an absence:
				// the topic is there and we could not read it.
				return fmt.Errorf("decoding %s on %q: %w", r.cfg.Topic, iface, err)
			}
			reading.At = now
			if err := emit(reading); err != nil {
				return err
			}
			delivered++
			lastEmit = now
		}
	}
}
