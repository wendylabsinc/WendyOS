package robotprobe

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/shared/robotinspect"
)

type fakeROS2 struct {
	topics    []ROS2Topic
	nodes     []ROS2Node
	topicsErr error
	nodesErr  error
}

func (f fakeROS2) ROS2Topics(context.Context) ([]ROS2Topic, error) { return f.topics, f.topicsErr }
func (f fakeROS2) ROS2Nodes(context.Context) ([]ROS2Node, error)   { return f.nodes, f.nodesErr }

func ros2Env(source ROS2Source) *robotinspect.Env {
	return robotinspect.NewEnv().Offer(robotinspect.RequirementROS2Graph, source)
}

// Shaped after what unitree-g1-nx-2 actually reports through the agent.
func g1Graph() fakeROS2 {
	return fakeROS2{
		topics: []ROS2Topic{
			{Name: "/lowstate", Types: []string{"unitree_hg/msg/LowState"}, PublisherCount: 1, SubscriberCount: 2, RMW: "rmw_fastrtps_cpp"},
			{Name: "/api/arm/request", Types: []string{"unitree_api/msg/Request"}, PublisherCount: 1, SubscriberCount: 1, RMW: "rmw_fastrtps_cpp"},
			{Name: "/secondary_imu", Types: []string{"unitree_hg/msg/IMUState"}, PublisherCount: 1, SubscriberCount: 0, RMW: "rmw_fastrtps_cpp"},
		},
		nodes: []ROS2Node{{Name: "unitree_bridge", Namespace: "/", RMW: "rmw_fastrtps_cpp"}},
	}
}

func TestROS2GraphCountsTheGraphAndNamesTheMiddleware(t *testing.T) {
	properties, err := ROS2Graph{}.Observe(context.Background(), ros2Env(g1Graph()))
	if err != nil {
		t.Fatal(err)
	}
	if got := findIn(t, properties, "ros2.topics").Observations[0].Quantity.Value(); got != 3 {
		t.Errorf("topics = %v, want 3", got)
	}
	if got := findIn(t, properties, "ros2.nodes").Observations[0].Quantity.Value(); got != 1 {
		t.Errorf("nodes = %v, want 1", got)
	}
	rmw := findIn(t, properties, "ros2.rmw")
	if got := rmw.Observations[0].Text; got != "rmw_fastrtps_cpp" {
		t.Errorf("rmw = %q", got)
	}
	if got := rmw.Assess().Verdict; got != robotinspect.VerdictSingle {
		t.Errorf("verdict = %q, want %q for one consistent middleware", got, robotinspect.VerdictSingle)
	}
}

// Two middlewares on one graph means nodes that cannot see each other, and the symptom
// is a topic that looks present and never delivers. Reported as a disagreement, because
// that is what it is.
func TestROS2GraphReportsMixedMiddlewareAsADisagreement(t *testing.T) {
	graph := g1Graph()
	graph.topics = append(graph.topics, ROS2Topic{
		Name: "/detections", Types: []string{"vision_msgs/msg/Detection2DArray"},
		PublisherCount: 1, SubscriberCount: 1, RMW: "rmw_cyclonedds_cpp",
	})

	properties, err := ROS2Graph{}.Observe(context.Background(), ros2Env(graph))
	if err != nil {
		t.Fatal(err)
	}

	// Two probes' worth of observations land on one property, as an inspection merges them.
	var merged robotinspect.Property
	merged.ID = "ros2.rmw"
	for _, p := range properties {
		if p.ID == "ros2.rmw" {
			merged.Observations = append(merged.Observations, p.Observations...)
		}
	}
	assessment := merged.Assess()
	if assessment.Verdict != robotinspect.VerdictDisagree {
		t.Fatalf("verdict = %q, want %q for two middlewares on one graph",
			assessment.Verdict, robotinspect.VerdictDisagree)
	}
	for _, want := range []string{"fastrtps", "cyclonedds"} {
		if !strings.Contains(assessment.Detail, want) {
			t.Errorf("detail %q should name both middlewares", assessment.Detail)
		}
	}
}

func TestROS2GraphCountsTopicsNobodyIsUsing(t *testing.T) {
	graph := g1Graph()
	graph.topics = append(graph.topics, ROS2Topic{
		Name: "/waiting_for_input", PublisherCount: 0, SubscriberCount: 3, RMW: "rmw_fastrtps_cpp",
	})

	properties, err := ROS2Graph{}.Observe(context.Background(), ros2Env(graph))
	if err != nil {
		t.Fatal(err)
	}
	// /secondary_imu has a publisher and no subscribers.
	if got := findIn(t, properties, "ros2.topics.unsubscribed").Observations[0].Quantity.Value(); got != 1 {
		t.Errorf("unsubscribed = %v, want 1", got)
	}
	if got := findIn(t, properties, "ros2.topics.unpublished").Observations[0].Quantity.Value(); got != 1 {
		t.Errorf("unpublished = %v, want 1", got)
	}
	// A consumer waiting for something that never comes is worth naming, not just counting.
	if got := findIn(t, properties, "ros2.topics.unpublished.names").Observations[0].Text; got != "/waiting_for_input" {
		t.Errorf("unpublished names = %q", got)
	}
}

// A robot with no ROS at all — an arm on a USB cable — reports zeroes and says the graph
// was empty, rather than leaving the section out.
func TestROS2GraphReportsAnEmptyGraphAsZeroNotAbsence(t *testing.T) {
	properties, err := ROS2Graph{}.Observe(context.Background(), ros2Env(fakeROS2{}))
	if err != nil {
		t.Fatal(err)
	}
	if got := findIn(t, properties, "ros2.topics").Observations[0].Quantity.Value(); got != 0 {
		t.Errorf("topics = %v, want a recorded zero", got)
	}
	rmw := findIn(t, properties, "ros2.rmw")
	if rmw.Unknown == nil || rmw.Unknown.Reason != robotinspect.ReasonSourceAbsent {
		t.Errorf("rmw = %+v, want an unknown", rmw.Unknown)
	}
}

func TestROS2GraphSurfacesAFailure(t *testing.T) {
	for _, source := range []fakeROS2{
		{topicsErr: errors.New("no ros2 sidecar on this device")},
		{nodesErr: errors.New("sidecar not running")},
	} {
		if _, err := (ROS2Graph{}).Observe(context.Background(), ros2Env(source)); err == nil {
			t.Error("a ROS 2 failure was swallowed")
		}
	}
}

func TestROS2GraphIsPassive(t *testing.T) {
	if got := (ROS2Graph{}).Class(); got != robotinspect.ClassPassive {
		t.Errorf("class = %q, want passive", got)
	}
}

// graphWithoutSidecar answers raw discovery but not the ROS 2 listing, which is what a
// robot with no ROS 2 container deployed looks like — the common case for a robot that
// publishes its body over DDS from its own firmware.
type graphWithoutSidecar struct {
	raw []RawTopic
}

func (g graphWithoutSidecar) ROS2Topics(context.Context) ([]ROS2Topic, error) {
	return nil, errors.New("no running ROS 2 containers found")
}
func (g graphWithoutSidecar) ROS2Nodes(context.Context) ([]ROS2Node, error) {
	return nil, errors.New("no running ROS 2 containers found")
}
func (g graphWithoutSidecar) RawTopics(context.Context) ([]RawTopic, error) { return g.raw, nil }

func TestROS2GraphFallsBackToRawDiscovery(t *testing.T) {
	env := robotinspect.NewEnv()
	env.Offer(robotinspect.RequirementROS2Graph, graphWithoutSidecar{raw: []RawTopic{
		{Name: "/lowstate", Type: "unitree_hg::msg::dds_::LowState_", WriterCount: 1},
		{Name: "/lf/bmsstate", Type: "unitree_hg::msg::dds_::BmsState_", WriterCount: 1},
		// One topic carrying two types still counts once, since the question is how
		// many topics the robot has.
		{Name: "/lowstate", Type: "unitree_go::msg::dds_::LowState_", WriterCount: 1},
	}})

	properties, err := ROS2Graph{}.Observe(context.Background(), env)
	if err != nil {
		t.Fatalf("the probe failed although discovery could see the graph: %v", err)
	}

	byID := map[string]robotinspect.Property{}
	for _, property := range properties {
		byID[property.ID] = property
	}
	topics, ok := byID["ros2.topics"]
	if !ok || len(topics.Observations) != 1 {
		t.Fatalf("no topic count: %+v", byID)
	}
	if got := topics.Observations[0].Quantity.Value(); got != 2 {
		t.Fatalf("topic count = %v, want 2 distinct names", got)
	}
	// Discovery names writers, not nodes. Reporting zero nodes would be a lie; this
	// has to read unknown, and the reason has to say why.
	nodes, ok := byID["ros2.nodes"]
	if !ok || nodes.Unknown == nil {
		t.Fatalf("node count was not reported unknown: %+v", nodes)
	}
	if !strings.Contains(nodes.Unknown.Detail, "ROS 2 container") {
		t.Fatalf("the reason does not name the cause: %q", nodes.Unknown.Detail)
	}
}
