package commands

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/wendylabsinc/wendy/go/internal/cli/robotwizard"
	"github.com/wendylabsinc/wendy/go/internal/shared/robotcal"
	"github.com/wendylabsinc/wendy/go/internal/shared/streamreason"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
)

// unitreeLowStateJointSource reads a Unitree humanoid's joints through the
// agent's robot service.
//
// The listening happens on the device, not here. DDS discovery is multicast: a
// robot's ROS 2 graph is confined to the segment the robot is on, and a laptop
// cannot see it across a routed link or a cloud tunnel however reachable the
// device is. The agent is already standing inside it, so this is a gRPC stream
// like every other command's and `wendy device robot calibrate` works from a
// laptop.
//
// It is read-only by construction: StreamJointPositions is the only RPC it
// holds, and there is no path from here to anything that moves a joint.
type unitreeLowStateJointSource struct {
	topic  string
	order  []string
	cancel context.CancelFunc

	mu      sync.Mutex
	latest  robotwizard.JointReading
	err     error
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

	streamCtx, cancel := context.WithCancel(ctx)
	stream, err := agentpbv2.NewWendyRobotServiceClient(conn).StreamJointPositions(streamCtx,
		&agentpbv2.StreamJointPositionsRequest{
			Backend:           backendUnitreeLowState,
			Topic:             params.Topic,
			DomainId:          params.DomainID,
			Interface:         params.Interface,
			MinIntervalMillis: uint32(params.MinInterval.Milliseconds()),
		})
	if err != nil {
		cancel()
		return nil, unitreeJointStreamError(err)
	}

	topic := params.Topic
	if topic == "" {
		topic = "/lowstate"
	}
	s := &unitreeLowStateJointSource{
		topic:   topic,
		order:   append([]string(nil), joints.Order...),
		cancel:  cancel,
		haveOne: make(chan struct{}),
	}
	go s.pump(stream)
	return s, nil
}

func (s *unitreeLowStateJointSource) pump(stream grpc.ServerStreamingClient[agentpbv2.JointPositionSample]) {
	for {
		sample, err := stream.Recv()
		if err != nil {
			s.mu.Lock()
			if errors.Is(err, io.EOF) {
				s.err = fmt.Errorf("the %s stream ended", s.topic)
			} else {
				s.err = unitreeJointStreamError(err)
			}
			s.mu.Unlock()
			s.once.Do(func() { close(s.haveOne) })
			return
		}
		reading := s.readingFrom(sample)
		if len(reading.Positions) == 0 {
			// Every slot idle is not a pose. It means the robot published a
			// message with nothing behind any of its slots, which is worth
			// waiting past rather than recording as a body at rest.
			continue
		}
		s.mu.Lock()
		s.latest, s.err = reading, nil
		s.mu.Unlock()
		s.once.Do(func() { close(s.haveOne) })
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
// A slot the profile does not name is still reported, under a key that could not
// be mistaken for a joint name. Dropping it would hide a robot driving something
// the profile has never heard of, which is precisely the kind of disagreement
// the sweep exists to surface.
func (s *unitreeLowStateJointSource) readingFrom(sample *agentpbv2.JointPositionSample) robotwizard.JointReading {
	reading := robotwizard.JointReading{
		At:        time.Now(),
		Positions: make(map[string]float64, len(sample.GetSlots())),
	}
	for _, slot := range sample.GetSlots() {
		index := int(slot.GetIndex())
		name := fmt.Sprintf("slot[%d]", index)
		if index < len(s.order) && s.order[index] != "" {
			name = s.order[index]
		}
		reading.Positions[name] = slot.GetPosition()
	}
	return reading
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
	return fmt.Sprintf("%s on %s, read by the agent", backendUnitreeLowState, s.topic)
}

func (s *unitreeLowStateJointSource) Close() error {
	s.cancel()
	return nil
}

// unitreeJointStreamError turns the agent's answer into something an operator
// can act on.
//
// The distinction worth keeping is between a robot that is silent and a device
// that could not be reached, so the absent case is recognised by the reason the
// agent attaches rather than by its code: a cloud tunnel that has lost the
// device answers NOT_FOUND too, and telling somebody their robot is quiet when
// nothing reached it sends them to the wrong machine.
func unitreeJointStreamError(err error) error {
	if err == nil {
		return nil
	}
	if streamreason.Has(err, streamreason.RobotJointSourceAbsent) {
		return fmt.Errorf("%s — the device joined the robot's DDS domain and heard nothing. "+
			"Check that the robot is powered and publishing, and that `joints.source.params.interface` "+
			"names the device interface the robot is on", agentMessage(err))
	}
	switch status.Code(err) {
	case codes.Unimplemented:
		return fmt.Errorf("this device's agent does not serve joint positions (%s): "+
			"update it with `wendy device update` and try again", strings.ToLower(agentMessage(err)))
	case codes.InvalidArgument, codes.FailedPrecondition, codes.NotFound, codes.Internal:
		return errors.New(agentMessage(err))
	default:
		return err
	}
}
