package robotprobe

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/wendylabsinc/wendy/go/internal/shared/robotinspect"
)

// ROS2Topic is one topic on the robot's graph, as the agent reports it.
type ROS2Topic struct {
	Name            string
	Types           []string
	PublisherCount  int
	SubscriberCount int
	// RMW is the middleware implementation the publisher is using.
	RMW string
}

// ROS2Node is one running node.
type ROS2Node struct {
	Name      string
	Namespace string
	RMW       string
}

// ROS2Source reads the robot's ROS 2 graph. The agent already serves this through its
// ROS 2 sidecar, so this works over a cloud tunnel and needs nothing installed on the
// machine running the inspection.
type ROS2Source interface {
	ROS2Topics(ctx context.Context) ([]ROS2Topic, error)
	ROS2Nodes(ctx context.Context) ([]ROS2Node, error)
}

// ROS2Graph reports what the robot's ROS 2 graph contains.
//
// The counts are inventory, but three things here are findings in their own right:
// a topic nobody subscribes to, a topic nobody publishes, and more than one middleware
// implementation in use at once. The last is the subtle one — a robot running two RMWs
// has nodes that cannot see each other, and the symptom is a topic that looks present
// and never delivers.
type ROS2Graph struct{}

func (ROS2Graph) ID() string                { return "ros2-graph" }
func (ROS2Graph) Class() robotinspect.Class { return robotinspect.ClassPassive }
func (ROS2Graph) Requires() []robotinspect.Requirement {
	return []robotinspect.Requirement{robotinspect.RequirementROS2Graph}
}
func (ROS2Graph) Provides() []string {
	return []string{"ros2.topics", "ros2.nodes", "ros2.rmw", "ros2.topics.unsubscribed", "ros2.topics.unpublished"}
}

// rawGraph reports what DDS discovery alone can see. It is deliberately narrower than the
// sidecar listing: discovery names writers, not the nodes behind them, and it carries no
// subscriber counts — so the properties that need those read unknown with the reason,
// rather than being quietly reported as zero.
func rawGraph(ctx context.Context, probe string, lister RawTopicLister, sidecarErr error) ([]robotinspect.Property, bool) {
	topics, err := lister.RawTopics(ctx)
	if err != nil || len(topics) == 0 {
		return nil, false
	}

	const origin = "agent:rtps"
	names := map[string]struct{}{}
	for _, topic := range topics {
		names[topic.Name] = struct{}{}
	}

	properties := []robotinspect.Property{}
	if property, ok := declared(probe, origin, "ros2.topics",
		robotinspect.MustQuantity(float64(len(names)), robotinspect.Count)); ok {
		properties = append(properties, property)
	}

	unknown := robotinspect.NewUnknown(robotinspect.ReasonRequirementUnmet,
		fmt.Sprintf("read by DDS discovery, which sees writers rather than nodes; the node listing needs a ROS 2 container on the device: %v", sidecarErr))
	for _, id := range []string{"ros2.nodes", "ros2.rmw", "ros2.topics.unsubscribed", "ros2.topics.unpublished"} {
		properties = append(properties, robotinspect.Property{ID: id, Unknown: &unknown})
	}
	return properties, true
}

func (p ROS2Graph) Observe(ctx context.Context, env *robotinspect.Env) ([]robotinspect.Property, error) {
	handle, ok := env.Handle(robotinspect.RequirementROS2Graph)
	if !ok {
		return nil, fmt.Errorf("ros2-graph: no ROS 2 handle in the environment")
	}
	source, ok := handle.(ROS2Source)
	if !ok {
		return nil, fmt.Errorf("ros2-graph: ROS 2 handle is %T, not a ROS2Source", handle)
	}

	topics, err := source.ROS2Topics(ctx)
	if err != nil {
		// The sidecar listing needs a ROS 2 container deployed on the device, and a
		// robot publishing its whole body over DDS very often has none. Falling back
		// to raw discovery turns "this probe failed" into the part of the graph that
		// can still be seen, which on such a robot is most of it.
		if lister, ok := handle.(RawTopicLister); ok {
			if properties, ok := rawGraph(ctx, p.ID(), lister, err); ok {
				return properties, nil
			}
		}
		return nil, fmt.Errorf("ros2-graph: listing topics: %w", err)
	}
	nodes, err := source.ROS2Nodes(ctx)
	if err != nil {
		return nil, fmt.Errorf("ros2-graph: listing nodes: %w", err)
	}

	origin := "agent:ros2"
	source_ := robotinspect.Source{Probe: p.ID(), Origin: origin}
	properties := []robotinspect.Property{}

	add := func(id string, value float64) {
		if property, ok := declared(p.ID(), origin, id,
			robotinspect.MustQuantity(value, robotinspect.Count)); ok {
			properties = append(properties, property)
		}
	}
	add("ros2.topics", float64(len(topics)))
	add("ros2.nodes", float64(len(nodes)))

	if len(topics) == 0 && len(nodes) == 0 {
		unknown := robotinspect.NewUnknown(robotinspect.ReasonSourceAbsent,
			"the agent reached ROS 2 but the graph is empty")
		return append(properties, robotinspect.Property{ID: "ros2.rmw", Unknown: &unknown}), nil
	}

	// Every distinct middleware becomes an observation on one property, so two of them
	// come out as a disagreement rather than as a fact nobody reads.
	for _, rmw := range distinctRMW(topics, nodes) {
		observation, err := robotinspect.NewTextObservation(rmw, robotinspect.Declared, source_)
		if err != nil {
			continue
		}
		properties = append(properties, robotinspect.Property{
			ID: "ros2.rmw", Observations: []robotinspect.Observation{observation},
		})
	}

	// A topic with publishers and no subscribers is work nobody wanted; one with
	// subscribers and no publishers is a consumer waiting for something that never
	// comes. Both are ordinary in small numbers and worth seeing.
	var unsubscribed, unpublished []string
	for _, topic := range topics {
		switch {
		case topic.PublisherCount > 0 && topic.SubscriberCount == 0:
			unsubscribed = append(unsubscribed, topic.Name)
		case topic.SubscriberCount > 0 && topic.PublisherCount == 0:
			unpublished = append(unpublished, topic.Name)
		}
	}
	sort.Strings(unsubscribed)
	sort.Strings(unpublished)
	add("ros2.topics.unsubscribed", float64(len(unsubscribed)))
	add("ros2.topics.unpublished", float64(len(unpublished)))
	if len(unpublished) > 0 {
		properties = append(properties, textProperty(p.ID(), origin,
			"ros2.topics.unpublished.names", strings.Join(unpublished, " ")))
	}
	return properties, nil
}

// distinctRMW collects every middleware implementation named anywhere on the graph.
func distinctRMW(topics []ROS2Topic, nodes []ROS2Node) []string {
	seen := map[string]struct{}{}
	for _, topic := range topics {
		if topic.RMW != "" {
			seen[topic.RMW] = struct{}{}
		}
	}
	for _, node := range nodes {
		if node.RMW != "" {
			seen[node.RMW] = struct{}{}
		}
	}
	return sortedKeys(seen)
}
