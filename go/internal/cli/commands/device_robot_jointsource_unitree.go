package commands

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/wendylabsinc/wendy/go/internal/cli/robotwizard"
	"github.com/wendylabsinc/wendy/go/internal/shared/robotcal"
	"github.com/wendylabsinc/wendy/go/internal/shared/rosmsg"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
)

// unitreeLowStateJointSource reads a Unitree humanoid's joints through the
// agent's raw topic stream, and decodes them here.
//
// The listening happens on the device, not here. DDS discovery is multicast: a
// robot's ROS 2 graph is confined to the segment the robot is on, and a laptop
// cannot see it across a routed link or a cloud tunnel however reachable the
// device is. The agent is already standing inside it.
//
// The decoding, though, happens on this side. ROS2Service.StreamRawTopic hands
// back the bytes a writer published without interpreting them, so one RPC serves
// every vendor message on every robot — where a decoded joint RPC would have to
// be extended, redeployed and version-matched for each new message a robot
// speaks. rosmsg.DecodeHGLowState is the same decoder `wendy device robot
// inspect` uses, validated against bytes a real G1 sent, so there is one
// understanding of this layout rather than one per caller.
//
// It is read-only by construction: StreamRawTopic and ListRawTopics are the only
// RPCs it holds, the agent's participant has no writer, and there is no path
// from here to anything that moves a joint.
type unitreeLowStateJointSource struct {
	topic       string
	order       []string
	minInterval time.Duration
	cancel      context.CancelFunc

	mu      sync.Mutex
	latest  robotwizard.JointReading
	err     error
	ended   bool
	haveOne chan struct{}
	once    sync.Once
}

// unitreeLowStateParams are the parameters a profile may set on this backend.
//
// An unknown key is a refusal rather than a silently ignored line, for the
// reason the sweep method refuses one: a parameter nothing reads is a profile
// author believing something is happening that is not. That is worth more here
// than anywhere, because the thing being configured is which bytes get read as
// which joint.
type unitreeLowStateParams struct {
	Topic       string
	DomainID    *int32
	Interface   string
	MinInterval time.Duration
}

// unitreePositionField is the only field of the message this backend reads. A
// profile may state it — the G1's does — and stating a different one is refused
// rather than ignored, because a profile that names a field nothing reads is
// describing a decoder that does not exist.
const unitreePositionField = "motor_state.q"

// unitreeWireUnit is what unitree_hg publishes q in. It is checked against the
// profile rather than assumed, because the platform refuses a budget quoted in a
// different unit and that refusal is only worth anything if somebody has said
// what the numbers are.
const unitreeWireUnit = "rad"

const defaultUnitreeLowStateTopic = "/lowstate"

// unitreeSilenceWindow is how long a subscribed stream may go quiet before it is
// reported as ended. A body-state topic publishes at hundreds of hertz, so a few
// seconds of silence is already the publisher having stopped.
//
// A var only so a test can drive the quiet-stream rule in milliseconds rather
// than sitting out three real seconds; nothing in the CLI writes it.
var unitreeSilenceWindow = 3 * time.Second

// Timings this backend holds, now that it is the side that reads the wire.
const (
	// unitreeDefaultMinInterval is roughly the rate the calibration wizard polls
	// at. The robot publishes far faster than a hand moves, so the surplus is
	// dropped here.
	//
	// Note what that does and does not save. Measured on unitree-g1-nx-2,
	// /lowstate publishes 2092-byte messages at about 1053 Hz — 2.2 MB/s — and
	// StreamRawTopic takes no minimum interval, so every one of those bytes
	// crosses the tunnel and 95% of them are dropped on arrival. This saves the
	// CDR walk and nothing else. The throttle belongs at the source, which means
	// a min_interval field on StreamRawTopicRequest; until that exists a sweep
	// costs full rate.
	unitreeDefaultMinInterval = 20 * time.Millisecond
	// unitreeMaxMinInterval bounds what a profile may ask for. A sweep that
	// samples slower than this is not measuring a moving joint any more.
	unitreeMaxMinInterval = time.Second
	// unitreeSubscriptionWindow is what each StreamRawTopic call is asked for.
	// The agent caps a raw subscription at two minutes, and a sweep of a
	// humanoid's joints outlasts that many times over, so the subscription is
	// renewed when the agent ends it rather than being taken as the robot
	// stopping. The pool keeps the participant warm, so a renewal does not pay
	// for discovery again.
	unitreeSubscriptionWindow = 2 * time.Minute
)

func parseUnitreeLowStateParams(in map[string]string) (unitreeLowStateParams, error) {
	var p unitreeLowStateParams
	for key, raw := range in {
		var err error
		switch key {
		case "topic":
			p.Topic = raw
		case "interface":
			p.Interface = raw
		case "domain_id":
			var d int64
			if d, err = strconv.ParseInt(raw, 10, 32); err == nil {
				v := int32(d)
				p.DomainID = &v
			}
		case "min_interval_ms":
			var ms int
			if ms, err = strconv.Atoi(raw); err == nil {
				p.MinInterval = time.Duration(ms) * time.Millisecond
			}
		case "position_field":
			if raw != unitreePositionField {
				return p, fmt.Errorf("parameter %q for joint source %s is %q, and this backend reads %q: "+
					"it decodes the message's own layout rather than looking a field up by name, so a "+
					"different field is not something it can be pointed at",
					key, backendUnitreeLowState, raw, unitreePositionField)
			}
		default:
			return p, fmt.Errorf("unknown parameter %q for joint source %s: it takes topic, domain_id, "+
				"interface, min_interval_ms and position_field. A profile parameterises a backend; it "+
				"does not describe one", key, backendUnitreeLowState)
		}
		if err != nil {
			return p, fmt.Errorf("parameter %q for joint source %s: %w", key, backendUnitreeLowState, err)
		}
	}
	switch {
	case p.MinInterval <= 0:
		p.MinInterval = unitreeDefaultMinInterval
	case p.MinInterval > unitreeMaxMinInterval:
		p.MinInterval = unitreeMaxMinInterval
	}
	return p, nil
}

func newUnitreeLowStateJointSource(ctx context.Context, conn *grpc.ClientConn, joints robotcal.Joints) (robotwizard.JointSource, error) {
	params, err := parseUnitreeLowStateParams(joints.Source.Params)
	if err != nil {
		return nil, err
	}
	if len(joints.Order) == 0 {
		// The message carries positions and no names, so the profile's order is
		// the only thing that can say which joint a slot is. Without it there is
		// nothing to measure against — and inventing names here would produce a
		// sweep that looked like it worked.
		return nil, fmt.Errorf("the %s backend needs the profile's joint order to name what it reads: "+
			"the robot publishes a positionally indexed array with no joint names in it, and this "+
			"profile declares no `joints.order`", backendUnitreeLowState)
	}
	if joints.Unit != "" && joints.Unit != unitreeWireUnit {
		// The wire carries no unit, so this is the only place the two can be
		// reconciled. A profile that says degrees against a message in radians
		// would have every limit out by a factor of 57.
		return nil, fmt.Errorf("this profile declares joint positions in %q and %s reads a message "+
			"published in %s: nothing here converts, because a converted number invites a comparison "+
			"against a limit quoted in the other unit", joints.Unit, backendUnitreeLowState, unitreeWireUnit)
	}

	topic := rosTopicSpelling(params.Topic)
	if params.Topic == "" {
		topic = defaultUnitreeLowStateTopic
	}
	client := agentpbv2.NewROS2ServiceClient(conn)
	if err := requireUnitreeLowStateWriter(ctx, client, topic, params); err != nil {
		return nil, err
	}

	streamCtx, cancel := context.WithCancel(ctx)
	s := &unitreeLowStateJointSource{
		topic:       topic,
		order:       append([]string(nil), joints.Order...),
		minInterval: params.MinInterval,
		cancel:      cancel,
		haveOne:     make(chan struct{}),
	}
	go s.run(streamCtx, client, &agentpbv2.StreamRawTopicRequest{
		Topic:      topic,
		Type:       rosmsg.TypeHGLowState,
		DomainId:   params.DomainID,
		Interface:  params.Interface,
		DurationMs: uint32(unitreeSubscriptionWindow.Milliseconds()),
	})
	return s, nil
}

// requireUnitreeLowStateWriter establishes, before an operator is asked to touch
// a robot, that this robot publishes the topic at all.
//
// It is here because of what the generic stream can and cannot say. The old
// device-side joint RPC separated four endings by code, and the one that mattered
// was "the device listened and heard nothing", carried as a machine-readable
// reason so it could not be confused with a cloud tunnel that lost the device —
// which answers NOT_FOUND too. StreamRawTopic answers a robot that publishes
// nothing with a bare NOT_FOUND and no reason, so on its own that distinction is
// gone.
//
// ListRawTopics gives it back, and gives it back as evidence rather than as a
// guess: if the listing succeeds, the device is reachable and its participant
// heard the graph, so a topic missing from it is the robot's silence and nothing
// else. It costs one round trip at open time, and it buys a refusal that can name
// what the robot does publish instead.
func requireUnitreeLowStateWriter(ctx context.Context, client agentpbv2.ROS2ServiceClient, topic string, params unitreeLowStateParams) error {
	req := &agentpbv2.ListRawTopicsRequest{Interface: params.Interface}
	if params.DomainID != nil {
		req.DomainId = uint32(*params.DomainID)
	}
	resp, err := client.ListRawTopics(ctx, req)
	if err != nil {
		return unitreeRawTopicError(topic, err)
	}

	var otherTypes []string
	for _, listed := range resp.GetTopics() {
		if rosTopicSpelling(listed.GetName()) != topic {
			continue
		}
		if listed.GetType() == rosmsg.TypeHGLowState {
			return nil
		}
		otherTypes = append(otherTypes, listed.GetType())
	}
	if len(otherTypes) > 0 {
		return fmt.Errorf("%s carries %s on this robot, and the %s backend reads %s: "+
			"decoding one layout as the other produces plausible wrong angles rather than an error",
			topic, strings.Join(otherTypes, ", "), backendUnitreeLowState, rosmsg.TypeHGLowState)
	}
	return fmt.Errorf("the device joined the robot's DDS domain and nothing published %s [%s]. "+
		"The device is reachable and it could hear — it saw %s — so the robot is simply not "+
		"talking. Check that it is powered and publishing, and that "+
		"`joints.source.params.interface` names the device interface the robot is on",
		topic, rosmsg.TypeHGLowState, describeVisibleTopics(resp.GetTopics()))
}

// describeVisibleTopics is what the device did hear, which is the difference
// between "your robot is quiet" and "we listened on the wrong wire". A graph with
// nothing in it is the second of those, and says so.
func describeVisibleTopics(topics []*agentpbv2.RawTopic) string {
	if len(topics) == 0 {
		return "no topics at all on that domain and interface"
	}
	names := make([]string, 0, len(topics))
	for _, topic := range topics {
		names = append(names, topic.GetName())
	}
	sort.Strings(names)
	if len(names) > 6 {
		return fmt.Sprintf("%d topics, including %s", len(names), strings.Join(names[:6], ", "))
	}
	return fmt.Sprintf("%d topics: %s", len(names), strings.Join(names, ", "))
}

// rosTopicSpelling undoes the mangling ROS 2 applies on the DDS wire, where a
// topic is published as "rt/" plus its name, so a profile may use either
// spelling. Trimming a bare "rt" instead would turn a native topic named
// rtps_status into /ps_status.
func rosTopicSpelling(topic string) string {
	name := strings.TrimPrefix(strings.TrimSpace(topic), "rt/")
	if name != "" && !strings.HasPrefix(name, "/") {
		name = "/" + name
	}
	return name
}

// run keeps a subscription on the topic for as long as the source is open.
//
// Two endings are deliberately not the same. The agent bounds a raw subscription
// — two minutes is its ceiling — and a sweep of a humanoid's joints runs far
// longer than that, so a stream the agent closed cleanly is renewed rather than
// reported as the robot stopping. A stream that goes quiet is the robot stopping,
// and that is what the silence watchdog is for: it fires across renewals as well
// as within one, so a reconnect loop cannot be mistaken for a live robot.
func (s *unitreeLowStateJointSource) run(ctx context.Context, client agentpbv2.ROS2ServiceClient, req *agentpbv2.StreamRawTopicRequest) {
	silence := time.AfterFunc(unitreeSilenceWindow, func() {
		s.fail(fmt.Errorf("%s stopped publishing (nothing for %s)", s.topic, unitreeSilenceWindow))
		s.cancel()
	})
	// Armed by the first sample, not by opening. Before one has arrived there is
	// nothing that could have gone quiet, and the wait is bounded by the agent's
	// own discovery window — which answers NOT_FOUND rather than letting this
	// side call a subscription that never started a robot that stopped.
	silence.Stop()
	defer silence.Stop()

	for {
		stream, err := client.StreamRawTopic(ctx, req)
		if err != nil {
			s.fail(unitreeRawTopicError(s.topic, err))
			return
		}
		if !s.pump(stream, silence) {
			return
		}
		if ctx.Err() != nil {
			s.fail(fmt.Errorf("the %s stream ended", s.topic))
			return
		}
	}
}

// pump reads one subscription, reporting whether it ended in a way worth
// renewing. A clean end is the agent's own duration bound; anything else is
// recorded and ends the source.
func (s *unitreeLowStateJointSource) pump(stream grpc.ServerStreamingClient[agentpbv2.RawTopicSample], silence *time.Timer) bool {
	var lastEmit time.Time
	var delivered int
	for {
		sample, err := stream.Recv()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return true
			}
			s.fail(unitreeRawTopicError(s.topic, err))
			return false
		}
		// Reset before the throttle: silence is a fact about the robot, and a
		// sample this source chose not to use is still the robot talking.
		silence.Reset(unitreeSilenceWindow)

		now := time.Now()
		if delivered > 0 && now.Sub(lastEmit) < s.minInterval {
			continue
		}
		state, err := rosmsg.DecodeHGLowState(sample.GetPayload())
		if err != nil {
			// A layout this decoder does not recognise is reported, never
			// decoded into plausible wrong angles. It is not an absence: the
			// topic is there and we could not read it.
			s.fail(fmt.Errorf("decoding %s [%s]: %w", s.topic, sample.GetType(), err))
			return false
		}
		reading := s.readingFrom(state)
		if len(reading.Positions) == 0 {
			// Every slot idle is not a pose. It means the robot published a
			// message with nothing behind any of its slots, which is worth
			// waiting past rather than recording as a body at rest.
			continue
		}
		s.publish(reading)
		delivered++
		lastEmit = now
	}
}

// readingFrom names the slots the robot reported.
//
// Order is deliberately left empty. The wizard compares a source's reported
// joint order against the profile's, and this source has no order of its own to
// report: the wire message carries indices and no names, and the names here are
// the profile's own. Handing the profile's order back as if the robot had said
// it would turn that check into a tautology that always agrees — which is worse
// than not running it, because it would look like evidence.
//
// Which slots are real is rosmsg's rule, next to the wire layout it is a fact
// about and shared with `wendy device robot inspect`. A slot that reports neither
// voltage nor temperature is idle and is left out entirely: it must not arrive as
// a zero angle, because a zero reads as a joint sitting perfectly still, which is
// indistinguishable from a joint that was swept and did not move — exactly the
// false calibration the wizard's rules exist to prevent.
//
// A slot the profile does not name is still reported, under a key that could not
// be mistaken for a joint name. Dropping it would hide a robot driving something
// the profile has never heard of, which is precisely the kind of disagreement
// the sweep exists to surface.
func (s *unitreeLowStateJointSource) readingFrom(state *rosmsg.HGLowState) robotwizard.JointReading {
	live := state.LiveMotors()
	reading := robotwizard.JointReading{
		At:        time.Now(),
		Positions: make(map[string]float64, len(live)),
	}
	for _, index := range live {
		name := fmt.Sprintf("slot[%d]", index)
		if index < len(s.order) && s.order[index] != "" {
			name = s.order[index]
		}
		reading.Positions[name] = float64(state.Motors[index].Position)
	}
	return reading
}

func (s *unitreeLowStateJointSource) publish(reading robotwizard.JointReading) {
	s.mu.Lock()
	if s.ended {
		// The stream has already been declared over; a sample that was in flight
		// when that happened must not revive it.
		s.mu.Unlock()
		return
	}
	s.latest, s.err = reading, nil
	s.mu.Unlock()
	s.once.Do(func() { close(s.haveOne) })
}

// fail records how the stream ended. The first ending wins: cancelling the
// stream is how the silence watchdog stops it, and the context error that
// follows says less than the reason it was cancelled for.
func (s *unitreeLowStateJointSource) fail(err error) {
	s.mu.Lock()
	if !s.ended {
		s.ended, s.err = true, err
	}
	s.mu.Unlock()
	s.once.Do(func() { close(s.haveOne) })
}

// Read returns the newest observation.
//
// Once the stream has ended, that is what comes back — not the last pose it
// carried. A sweep reads continuously while an operator moves an arm, so a
// frozen last reading would be measured as a joint holding perfectly still, and
// the sweep would report that the joint does not travel. It is the same failure
// an idle slot reported as a zero angle would cause, in time rather than in
// space, and it gets the same answer: absent rather than plausible.
func (s *unitreeLowStateJointSource) Read(ctx context.Context) (robotwizard.JointReading, error) {
	select {
	case <-s.haveOne:
	case <-ctx.Done():
		return robotwizard.JointReading{}, ctx.Err()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return robotwizard.JointReading{}, s.err
	}
	if s.latest.Positions == nil {
		return robotwizard.JointReading{}, fmt.Errorf("no joint sample arrived from %s", s.topic)
	}
	return s.latest, nil
}

func (s *unitreeLowStateJointSource) Describe() string {
	return fmt.Sprintf("%s on %s, sampled by the agent and decoded here", backendUnitreeLowState, s.topic)
}

func (s *unitreeLowStateJointSource) Close() error {
	s.cancel()
	return nil
}

// unitreeRawTopicError turns the agent's answer into something an operator can
// act on.
//
// NOT_FOUND is the one that cannot be resolved here. StreamRawTopic answers it
// both for "no writer for this topic" and — when the call never reached the
// device — for a cloud tunnel that has lost it, and it attaches no
// machine-readable reason to tell them apart. What this backend can say is that
// the topic was being published when the source opened, because
// requireUnitreeLowStateWriter saw it listed, so the message names both endings
// rather than picking one. Attaching a reason to the agent's NOT_FOUND would
// settle it; that belongs in the raw topic RPC, not here.
func unitreeRawTopicError(topic string, err error) error {
	if err == nil {
		return nil
	}
	switch status.Code(err) {
	case codes.Unimplemented:
		return fmt.Errorf("this device's agent does not serve raw topic samples (%s): "+
			"update it with `wendy device update` and try again", strings.ToLower(agentMessage(err)))
	case codes.NotFound:
		return fmt.Errorf("%s — %s was being published when this source opened, so either the "+
			"robot has stopped or the device became unreachable. The raw topic stream answers "+
			"NOT_FOUND for both and carries no reason to tell them apart", agentMessage(err), topic)
	case codes.InvalidArgument, codes.FailedPrecondition, codes.Unavailable, codes.Internal:
		return errors.New(agentMessage(err))
	default:
		return err
	}
}
