package services

import (
	"context"
	"sort"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/wendylabsinc/wendy/go/internal/rtps"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
)

// Bounds on a raw subscription. A caller that asks for nothing gets these; one that asks
// for more than the maximum is held to it, since this keeps a DDS participant and a
// stream open on the device.
const (
	rawSettleDefault   = 8 * time.Second
	rawSettleMaximum   = 60 * time.Second
	rawDurationDefault = 5 * time.Second
	rawDurationMaximum = 120 * time.Second
)

// StreamRawTopic streams a topic's serialized payloads without interpreting them.
//
// Every other call on this service execs `ros2` inside the CLI sidecar, so it can only
// read messages whose definitions that image carries. A robot publishing a vendor type
// cannot be read there at all — on a Unitree G1, `ros2 topic echo /lowstate` answers
// "the message type 'unitree_hg/msg/LowState' is invalid", and that topic is where the
// humanoid keeps every joint, its battery and its hands.
//
// So this reads the bytes with the agent's own RTPS participant and lets the caller
// decode. One call serves every vendor message on every robot rather than a new RPC per
// message, and it is the only way to reach these from off the robot, because DDS
// discovery is multicast and never leaves the robot's own network.
//
// It cannot write: the participant has no writer, so subscribing is the only thing this
// can do to a robot.
func (s *ROS2Service) StreamRawTopic(req *agentpbv2.StreamRawTopicRequest, stream agentpbv2.ROS2Service_StreamRawTopicServer) error {
	if s.pool == nil {
		return status.Error(codes.Unimplemented, "this agent has no RTPS pool configured")
	}
	topic := strings.TrimSpace(req.GetTopic())
	if topic == "" {
		return status.Error(codes.InvalidArgument, "topic is required")
	}

	ctx := stream.Context()
	lease, err := s.pool.Acquire(ctx, rtps.Config{
		DomainID:  int(req.GetDomainId()),
		Interface: req.GetInterface(),
	})
	if err != nil {
		return status.Errorf(codes.Unavailable, "joining DDS domain %d: %v", req.GetDomainId(), err)
	}
	defer lease.Close()

	settle := boundedDuration(req.GetSettleMs(), rawSettleDefault, rawSettleMaximum)
	endpoint, err := waitForEndpoint(ctx, lease, topic, req.GetType(), settle)
	if err != nil {
		return err
	}
	if err := lease.Subscribe(endpoint); err != nil {
		return status.Errorf(codes.Unavailable, "subscribing to %s: %v", topic, err)
	}

	duration := boundedDuration(req.GetDurationMs(), rawDurationDefault, rawDurationMaximum)
	deadline := time.NewTimer(duration)
	defer deadline.Stop()

	maxSamples := int(req.GetMaxSamples())
	sent := 0
	for {
		if maxSamples > 0 && sent >= maxSamples {
			return nil
		}
		select {
		case <-ctx.Done():
			return nil // the caller stopped listening; that is not an error
		case <-deadline.C:
			return nil
		case <-lease.Done():
			return status.Errorf(codes.Unavailable, "the participant closed while reading %s", topic)
		case sample := <-lease.Samples():
			// One lease carries every subscription's samples, so a payload from
			// another reader on the same participant must not be credited here.
			if sample.Writer != endpoint.GUID {
				continue
			}
			if err := stream.Send(&agentpbv2.RawTopicSample{
				Payload:           sample.Payload,
				ReceivedUnixNanos: time.Now().UnixNano(),
				Topic:             rosTopicName(endpoint.Topic),
				Type:              endpoint.Type,
			}); err != nil {
				return err
			}
			sent++
		}
	}
}

// findRawEndpoint resolves a topic against the discovered graph. An empty type accepts
// whatever is published, which is what a caller discovering rather than decoding wants.
func findRawEndpoint(endpoints []rtps.Endpoint, topic, typeName string) (rtps.Endpoint, bool) {
	for _, endpoint := range endpoints {
		if typeName != "" && endpoint.Type != typeName {
			continue
		}
		if rosTopicName(endpoint.Topic) == rosTopicName(topic) {
			return endpoint, true
		}
	}
	return rtps.Endpoint{}, false
}

// rosTopicName undoes the mangling ROS 2 applies on the DDS wire, where a topic is
// published as "rt/" plus its name, so a caller may ask for either spelling. Trimming a
// bare "rt" instead would turn a native topic named rtps_status into /ps_status.
func rosTopicName(topic string) string {
	name := strings.TrimPrefix(topic, "rt/")
	if !strings.HasPrefix(name, "/") {
		name = "/" + name
	}
	return name
}

func boundedDuration(milliseconds uint32, fallback, maximum time.Duration) time.Duration {
	if milliseconds == 0 {
		return fallback
	}
	d := time.Duration(milliseconds) * time.Millisecond
	if d > maximum {
		return maximum
	}
	return d
}

// rawListSettleDefault is how long discovery runs for a listing. Shorter than a
// subscription's settle: a listing is a glance at the graph, and a caller that needs a
// slow-announcing writer can ask for longer.
const rawListSettleDefault = 4 * time.Second

// ListRawTopics reports what the agent's own participant can see on the DDS domain.
//
// ListTopics answers the same question by running `ros2 topic list` inside a sidecar, so
// it needs a ROS 2 container deployed and running. This needs nothing on the device, which
// matters because the robots whose topics only this can read — vendor types a stock ROS
// image cannot decode — are exactly the ones where no ROS 2 app has been deployed.
func (s *ROS2Service) ListRawTopics(ctx context.Context, req *agentpbv2.ListRawTopicsRequest) (*agentpbv2.ListRawTopicsResponse, error) {
	if s.pool == nil {
		return nil, status.Error(codes.Unimplemented, "this agent has no RTPS pool configured")
	}
	lease, err := s.pool.Acquire(ctx, rtps.Config{
		DomainID:  int(req.GetDomainId()),
		Interface: req.GetInterface(),
	})
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "joining DDS domain %d: %v", req.GetDomainId(), err)
	}
	defer lease.Close()

	settle := boundedDuration(req.GetSettleMs(), rawListSettleDefault, rawSettleMaximum)
	if err := waitForQuietGraph(ctx, lease, settle); err != nil {
		return nil, err
	}
	return &agentpbv2.ListRawTopicsResponse{Topics: summarizeEndpoints(lease.Endpoints())}, nil
}

// summarizeEndpoints folds the discovered writers into one row per topic and type. Two
// writers on one topic collapse to a count rather than two rows, since a caller wants to
// know a topic exists — and, separately, that more than one thing is publishing it.
func summarizeEndpoints(endpoints []rtps.Endpoint) []*agentpbv2.RawTopic {
	index := map[string]*agentpbv2.RawTopic{}
	var topics []*agentpbv2.RawTopic
	for _, endpoint := range endpoints {
		name := rosTopicName(endpoint.Topic)
		key := name + "\x00" + endpoint.Type
		if existing, ok := index[key]; ok {
			existing.WriterCount++
			continue
		}
		topic := &agentpbv2.RawTopic{Name: name, Type: endpoint.Type, WriterCount: 1}
		index[key] = topic
		topics = append(topics, topic)
	}
	sort.Slice(topics, func(i, j int) bool {
		if topics[i].Name != topics[j].Name {
			return topics[i].Name < topics[j].Name
		}
		return topics[i].Type < topics[j].Type
	})
	return topics
}

// discoveryQuiet is how long the graph must stop changing before a listing is considered
// complete. Announcements arrive in a burst, so a gap this long means the burst is over.
const discoveryQuiet = 750 * time.Millisecond

// waitForEndpoint returns as soon as a topic's writer is discovered, giving up after
// settle.
//
// Sleeping out the full settle and then looking once would be simpler and much worse. The
// pool keeps a participant warm across calls, so after the first read the graph is already
// known — and an inspection reads four topics, which would pay the discovery wait four
// times over for a writer it had already found. That is the difference between a report
// that takes a minute and one that takes seconds.
func waitForEndpoint(ctx context.Context, lease *rtps.Lease, topic, typeName string, settle time.Duration) (rtps.Endpoint, error) {
	deadline := time.NewTimer(settle)
	defer deadline.Stop()
	for {
		if endpoint, ok := findRawEndpoint(lease.Endpoints(), topic, typeName); ok {
			return endpoint, nil
		}
		select {
		case <-lease.Changed():
			// Something joined or left; look again.
		case <-deadline.C:
			// Nothing published it. That is an answer about the robot rather than
			// a failure of this call, and the caller tells them apart by the code.
			return rtps.Endpoint{}, status.Errorf(codes.NotFound, "no writer for %s after %s", topic, settle)
		case <-ctx.Done():
			return rtps.Endpoint{}, ctx.Err()
		case <-lease.Done():
			return rtps.Endpoint{}, status.Error(codes.Unavailable, "the participant closed during discovery")
		}
	}
}

// waitForQuietGraph waits for discovery to stop producing new participants, capped at
// settle. A listing cannot stop at the first arrival the way waitForEndpoint does — it has
// no particular thing to wait for — so it waits for the announcements to go quiet instead,
// which on a warm participant is immediate.
func waitForQuietGraph(ctx context.Context, lease *rtps.Lease, settle time.Duration) error {
	deadline := time.NewTimer(settle)
	defer deadline.Stop()
	quiet := time.NewTimer(discoveryQuiet)
	defer quiet.Stop()
	for {
		select {
		case <-lease.Changed():
			if !quiet.Stop() {
				select {
				case <-quiet.C:
				default:
				}
			}
			quiet.Reset(discoveryQuiet)
		case <-quiet.C:
			return nil
		case <-deadline.C:
			// Still arriving when time ran out. Reporting what was seen so far
			// beats reporting nothing.
			return nil
		case <-ctx.Done():
			return ctx.Err()
		case <-lease.Done():
			return status.Error(codes.Unavailable, "the participant closed during discovery")
		}
	}
}
