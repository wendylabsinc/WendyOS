package services

import (
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/rtps"
)

func TestFindRawEndpointMatchesEitherTopicSpelling(t *testing.T) {
	endpoints := []rtps.Endpoint{
		{Topic: "rt/lowstate", Type: "unitree_hg::msg::dds_::LowState_"},
		{Topic: "rt/lf/bmsstate", Type: "unitree_hg::msg::dds_::BmsState_"},
	}

	// ROS names a topic "/lowstate" and DDS carries it as "rt/lowstate"; a caller may
	// hold either, and both must resolve to the same writer.
	for _, ask := range []string{"/lowstate", "rt/lowstate", "lowstate"} {
		endpoint, ok := findRawEndpoint(endpoints, ask, "unitree_hg::msg::dds_::LowState_")
		if !ok {
			t.Fatalf("%q did not resolve", ask)
		}
		if endpoint.Topic != "rt/lowstate" {
			t.Fatalf("%q resolved to %q", ask, endpoint.Topic)
		}
	}

	// The right topic carrying the wrong type is not a match: decoding it would
	// produce numbers rather than an error, which is the worse failure.
	if _, ok := findRawEndpoint(endpoints, "/lowstate", "unitree_go::msg::dds_::LowState_"); ok {
		t.Fatal("matched a topic of a different type")
	}
	// No type asked for accepts whatever is published, for a caller that is looking
	// rather than decoding.
	if _, ok := findRawEndpoint(endpoints, "/lowstate", ""); !ok {
		t.Fatal("an untyped request found nothing")
	}
	if _, ok := findRawEndpoint(endpoints, "/joint_states", ""); ok {
		t.Fatal("matched a topic nobody publishes")
	}
}

// A topic named rtps_status is why the prefix trimmed is "rt/" and not "rt".
func TestRosTopicNameKeepsNamesBeginningWithRT(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"rt/lowstate", "/lowstate"},
		{"rt/lf/bmsstate", "/lf/bmsstate"},
		{"rtps_status", "/rtps_status"},
		{"/lowstate", "/lowstate"},
	} {
		if got := rosTopicName(tc.in); got != tc.want {
			t.Errorf("rosTopicName(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestBoundedDurationHoldsACallerToTheMaximum(t *testing.T) {
	if got := boundedDuration(0, rawDurationDefault, rawDurationMaximum); got != rawDurationDefault {
		t.Errorf("asking for nothing gave %s, want the default %s", got, rawDurationDefault)
	}
	if got := boundedDuration(2000, rawDurationDefault, rawDurationMaximum); got != 2*time.Second {
		t.Errorf("asking for 2s gave %s", got)
	}
	// This keeps a participant and a stream open on the device, so a caller cannot
	// ask for an hour of it.
	if got := boundedDuration(3_600_000, rawDurationDefault, rawDurationMaximum); got != rawDurationMaximum {
		t.Errorf("asking for an hour gave %s, want the cap %s", got, rawDurationMaximum)
	}
}

func TestSummarizeEndpointsCountsWritersPerTopic(t *testing.T) {
	topics := summarizeEndpoints([]rtps.Endpoint{
		{Topic: "rt/lowstate", Type: "unitree_hg::msg::dds_::LowState_"},
		{Topic: "rt/lowstate", Type: "unitree_hg::msg::dds_::LowState_"},
		{Topic: "rt/lf/bmsstate", Type: "unitree_hg::msg::dds_::BmsState_"},
		// Same topic, different type. Kept apart: that is a robot with two things
		// disagreeing about what a topic carries, which is worth seeing.
		{Topic: "rt/lowstate", Type: "unitree_go::msg::dds_::LowState_"},
	})

	if len(topics) != 3 {
		t.Fatalf("got %d rows, want 3: %+v", len(topics), topics)
	}
	// Sorted by name then type, so two runs of one robot diff cleanly.
	if topics[0].Name != "/lf/bmsstate" {
		t.Fatalf("not sorted by name: %+v", topics)
	}
	if topics[1].Name != "/lowstate" || topics[1].WriterCount != 1 {
		t.Fatalf("row 1 = %+v, want the unitree_go writer alone", topics[1])
	}
	if topics[2].WriterCount != 2 {
		t.Fatalf("two writers on one topic were not counted: %+v", topics[2])
	}
}
