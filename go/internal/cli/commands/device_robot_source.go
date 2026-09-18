package commands

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/robotprobe"
)

// layeredTopicSource reads a robot's topics through the agent when it can, and through a
// participant on this machine when it cannot.
//
// The two are not interchangeable, and which one leads decides what the report can say.
// DDS discovery is multicast and does not leave the robot's own network, so a laptop sees
// only what the robot bridges outward — on a G1 that is the cameras and nothing else. The
// agent runs on the robot, inside that network, so it sees the whole graph including the
// body. Preferring it is what lets `inspect` report joints, hands and the pack from a desk
// with nothing deployed.
//
// The local participant stays as the fallback for exactly one case: an agent too old to
// serve raw samples. Every other failure is reported rather than retried locally, because
// an error from the machine standing inside the graph says more than silence from one
// standing outside it.
type layeredTopicSource struct {
	agent robotprobe.TopicReader
	local robotTopicSource

	// agentTopics is the robot's own graph, used to decide which probes have anything
	// to read. Fetched once, since discovery costs a round trip and a settle on the
	// device.
	agentTopics []robotprobe.RawTopic

	// degraded records why the agent path was not used, so the document can say so
	// rather than leaving the reader to wonder where the body went.
	degraded string
}

// newLayeredTopicSource pairs the two transports. Either may be absent: with no agent
// this is the local participant alone, which is what an inspection run on the robot's own
// network gets.
func newLayeredTopicSource(ctx context.Context, host robotprobe.HostFactsSource, local robotTopicSource) *layeredTopicSource {
	source := &layeredTopicSource{local: local}
	reader, ok := host.(robotprobe.TopicReader)
	if !ok {
		return source
	}
	source.agent = reader
	lister, ok := host.(robotprobe.RawTopicLister)
	if !ok {
		return source
	}
	topics, err := lister.RawTopics(ctx)
	switch {
	case err == nil:
		source.agentTopics = topics
	case errors.Is(err, errRawTopicUnsupported):
		// An agent too old for raw discovery is too old for raw sampling, so the
		// body cannot be read through it at all. Recorded here rather than waiting
		// for the first sample to fail, since with no listing no probe runs.
		source.agent = nil
		source.degraded = oldAgentDowngrade
	default:
		// The probes that read the robot itself are chosen from this listing, so a
		// failure here removes them from the report with nothing to show it
		// happened. Saying so is the difference between "this robot has no body to
		// read" and "the body could not be looked for".
		source.degraded = fmt.Sprintf("the robot's own topics could not be listed, so nothing that reads its body was run: %v", err)
	}
	return source
}

// oldAgentDowngrade is why a report has no body section. It names the cause rather than
// the symptom, because the symptom — an empty report — invites the reader to blame the
// robot.
const oldAgentDowngrade = "the agent on this device is older than raw topic reading, so topics were read from this machine instead — which sees only the graph the robot bridges off its own network"

// Sample reads through the agent, falling back to this machine's participant only when
// the agent does not implement raw sampling.
func (s *layeredTopicSource) Sample(ctx context.Context, topic, typeName string, window time.Duration, maxMessages int) ([][]byte, error) {
	if s.agent != nil {
		payloads, err := s.agent.Sample(ctx, topic, typeName, window, maxMessages)
		if !errors.Is(err, errRawTopicUnsupported) {
			return payloads, err
		}
		s.degraded = oldAgentDowngrade
	}
	if s.local == nil {
		return nil, nil
	}
	return s.local.Sample(ctx, topic, typeName, window, maxMessages)
}

// TopicsOfType returns every topic carrying the given DDS type, from both graphs. The two
// spellings of a type name are folded together here rather than at each call site, since a
// listing may arrive in either.
func (s *layeredTopicSource) TopicsOfType(typeName string) []string {
	seen := map[string]struct{}{}
	var topics []string
	add := func(name string) {
		if name == "" {
			return
		}
		if _, already := seen[name]; already {
			return
		}
		seen[name] = struct{}{}
		topics = append(topics, name)
	}

	rosName := rosTypeFromDDS(typeName)
	for _, topic := range s.agentTopics {
		// Raw discovery reports the DDS spelling, but this also accepts the ROS one
		// so the same matching holds if the listing ever comes from `ros2 topic
		// list -t` instead.
		if topic.Type == typeName || topic.Type == rosName {
			add(topic.Name)
		}
	}
	if s.local != nil {
		for _, name := range s.local.TopicsOfType(typeName) {
			add(name)
		}
	}
	return topics
}

// rosTypeFromDDS converts a DDS type name to the ROS spelling of the same type:
// "unitree_hg::msg::dds_::LowState_" becomes "unitree_hg/msg/LowState". A name that is not
// in the DDS form is returned unchanged, so this is safe to apply to either spelling.
func rosTypeFromDDS(typeName string) string {
	parts := strings.Split(typeName, "::")
	if len(parts) < 2 {
		return typeName
	}
	// The middle "dds_" segment exists only in the DDS spelling and has no ROS
	// counterpart, and the trailing underscore marks the generated struct.
	var kept []string
	for _, part := range parts {
		if part == "dds_" {
			continue
		}
		kept = append(kept, part)
	}
	if len(kept) == 0 {
		return typeName
	}
	kept[len(kept)-1] = strings.TrimSuffix(kept[len(kept)-1], "_")
	return strings.Join(kept, "/")
}
