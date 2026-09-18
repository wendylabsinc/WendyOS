package commands

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/robotprobe"
	"github.com/wendylabsinc/wendy/go/internal/shared/rosmsg"
)

// fakeReader records what it was asked for and returns what it was told to.
type fakeReader struct {
	payloads [][]byte
	err      error
	calls    int
}

func (f *fakeReader) Sample(context.Context, string, string, time.Duration, int) ([][]byte, error) {
	f.calls++
	return f.payloads, f.err
}

// fakeLocal is a reader that also answers discovery, standing in for the DDS participant.
type fakeLocal struct {
	fakeReader
	topics map[string][]string
}

func (f *fakeLocal) TopicsOfType(typeName string) []string { return f.topics[typeName] }

func TestLayeredSourcePrefersTheAgent(t *testing.T) {
	agent := &fakeReader{payloads: [][]byte{{1}}}
	local := &fakeLocal{fakeReader: fakeReader{payloads: [][]byte{{2}}}}
	source := &layeredTopicSource{agent: agent, local: local}

	payloads, err := source.Sample(context.Background(), "/lowstate", rosmsg.TypeHGLowState, time.Second, 1)
	if err != nil {
		t.Fatalf("sample: %v", err)
	}
	if len(payloads) != 1 || payloads[0][0] != 1 {
		t.Fatalf("read the local participant instead of the agent: %v", payloads)
	}
	if local.calls != 0 {
		t.Fatalf("local participant was consulted %d times with a working agent", local.calls)
	}
}

func TestLayeredSourceFallsBackOnlyForAnOldAgent(t *testing.T) {
	t.Run("old agent", func(t *testing.T) {
		agent := &fakeReader{err: errRawTopicUnsupported}
		local := &fakeLocal{fakeReader: fakeReader{payloads: [][]byte{{2}}}}
		source := &layeredTopicSource{agent: agent, local: local}

		payloads, err := source.Sample(context.Background(), "/lowstate", rosmsg.TypeHGLowState, time.Second, 1)
		if err != nil {
			t.Fatalf("sample: %v", err)
		}
		if len(payloads) != 1 || payloads[0][0] != 2 {
			t.Fatalf("did not fall back to the local participant: %v", payloads)
		}
		if source.degraded == "" {
			t.Fatal("fell back silently; the document would not say why the body is missing")
		}
	})

	// A real failure on the robot is worth more than a guess from off it: retrying
	// locally would turn "the agent could not read this" into "nothing publishes it".
	t.Run("real failure", func(t *testing.T) {
		boom := errors.New("sampling /lowstate: domain 0 has no participant")
		agent := &fakeReader{err: boom}
		local := &fakeLocal{fakeReader: fakeReader{payloads: [][]byte{{2}}}}
		source := &layeredTopicSource{agent: agent, local: local}

		if _, err := source.Sample(context.Background(), "/lowstate", rosmsg.TypeHGLowState, time.Second, 1); !errors.Is(err, boom) {
			t.Fatalf("swallowed the agent's error: %v", err)
		}
		if local.calls != 0 {
			t.Fatal("retried locally after a real agent failure")
		}
	})
}

func TestLayeredSourceMatchesBothSpellingsOfAType(t *testing.T) {
	source := &layeredTopicSource{
		// The agent lists the graph with `ros2 topic list -t`, so its type names
		// arrive in the ROS spelling, not the DDS one the probes ask with.
		agentTopics: []robotprobe.RawTopic{
			// Raw discovery carries the DDS spelling; the ROS one is accepted too,
			// so a listing from either source matches.
			{Name: "/lowstate", Type: rosmsg.TypeHGLowState},
			{Name: "/odom", Type: "nav_msgs/msg/Odometry"},
		},
		local: &fakeLocal{topics: map[string][]string{
			rosmsg.TypeHGLowState: {"/lowstate", "/secondary/lowstate"},
		}},
	}

	got := source.TopicsOfType(rosmsg.TypeHGLowState)
	want := []string{"/lowstate", "/secondary/lowstate"}
	if len(got) != len(want) {
		t.Fatalf("topics = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("topics = %v, want %v", got, want)
		}
	}
	if topics := source.TopicsOfType(rosmsg.TypeHGBmsState); len(topics) != 0 {
		t.Fatalf("a type nobody publishes matched %v", topics)
	}
}

func TestRosTypeFromDDS(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{rosmsg.TypeHGLowState, "unitree_hg/msg/LowState"},
		{rosmsg.TypeCameraInfo, "sensor_msgs/msg/CameraInfo"},
		// Already in the ROS spelling, and a bare name: both pass through, so this
		// is safe to apply without knowing which spelling you hold.
		{"sensor_msgs/msg/Image", "sensor_msgs/msg/Image"},
		{"Image", "Image"},
	} {
		if got := rosTypeFromDDS(tc.in); got != tc.want {
			t.Errorf("rosTypeFromDDS(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestNameHandTopicsLabelsBySide(t *testing.T) {
	named := nameHandTopics([]string{"/dex3/left/state", "/dex3/right/state", "/gripper/state"})
	if named["left"] != "/dex3/left/state" || named["right"] != "/dex3/right/state" {
		t.Fatalf("hands not labelled by side: %v", named)
	}
	// Naming neither side, it keeps its topic: better an unhelpful label than a wrong one.
	if named["/gripper/state"] != "/gripper/state" {
		t.Fatalf("an unsided hand was guessed at: %v", named)
	}
}

// fakeLister answers discovery the way the agent does, without a ROS container anywhere.
type fakeLister struct {
	fakeReader
	topics []robotprobe.RawTopic
	err    error
}

func (f *fakeLister) RawTopics(context.Context) ([]robotprobe.RawTopic, error) {
	return f.topics, f.err
}
func (f *fakeLister) HostFacts(context.Context) (*robotprobe.HostFacts, error) { return nil, nil }

func TestNewLayeredSourceGivesUpOnTheAgentWhenItCannotDiscover(t *testing.T) {
	// An agent too old for raw discovery is too old for raw sampling, so there is no
	// point waiting for the first sample to fail — with no listing, no probe runs.
	host := &fakeLister{err: errRawTopicUnsupported}
	source := newLayeredTopicSource(context.Background(), host, &fakeLocal{})
	if source.agent != nil {
		t.Fatal("kept an agent that cannot serve raw topics")
	}
	if source.degraded == "" {
		t.Fatal("downgraded silently; the document would not say why the body is missing")
	}
}

func TestNewLayeredSourceKeepsTheAgentWhenDiscoveryMerelyFails(t *testing.T) {
	// A transient discovery failure is not evidence the agent lacks the RPC, so the
	// sampling path stays available.
	host := &fakeLister{err: errors.New("joining DDS domain 0: no such interface")}
	source := newLayeredTopicSource(context.Background(), host, &fakeLocal{})
	if source.agent == nil {
		t.Fatal("dropped the agent over a transient discovery failure")
	}
	// It still has to be recorded. The probes that read the robot itself are chosen
	// from this listing, so a failure here removes every one of them from the report —
	// and an unexplained absence reads as "this robot has no body" rather than "the
	// body was never looked for".
	if source.degraded == "" {
		t.Fatal("a failed listing left the body out of the report with nothing to say why")
	}
	if !strings.Contains(source.degraded, "no such interface") {
		t.Fatalf("the reason does not carry the cause: %q", source.degraded)
	}
}
