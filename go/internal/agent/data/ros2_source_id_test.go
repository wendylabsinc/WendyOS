package data

import "testing"

func TestROS2SourceIDRoundTrip(t *testing.T) {
	for _, tc := range []struct {
		name     string
		rmw      string
		domainID int
		topic    string
		wantID   string
	}{
		{name: "domain", rmw: "rmw_cyclonedds_cpp", domainID: 42, wantID: "ros2:rmw_cyclonedds_cpp:domain-42"},
		{name: "topic", rmw: "rmw_cyclonedds_cpp", domainID: 42, topic: "/chatter", wantID: "ros2:rmw_cyclonedds_cpp:domain-42:/chatter"},
		// The reason the topic is the last field: it carries slashes and the
		// parser must not stop at the first one.
		{name: "nested topic", rmw: "rmw_fastrtps_cpp", domainID: 0, topic: "/camera/left/image_raw", wantID: "ros2:rmw_fastrtps_cpp:domain-0:/camera/left/image_raw"},
		{name: "deeply nested topic", rmw: "rmw_cyclonedds_cpp", domainID: 232, topic: "/a/b/c/d/e/f", wantID: "ros2:rmw_cyclonedds_cpp:domain-232:/a/b/c/d/e/f"},
		{name: "hidden topic", rmw: "rmw_cyclonedds_cpp", domainID: 7, topic: "/_hidden/status", wantID: "ros2:rmw_cyclonedds_cpp:domain-7:/_hidden/status"},
		{name: "empty rmw normalizes", rmw: "", domainID: 3, topic: "/scan", wantID: "ros2:default:domain-3:/scan"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wantDomain := ROS2DomainSourceID(tc.rmw, tc.domainID)
			id := wantDomain
			if tc.topic != "" {
				id = ROS2TopicSourceID(tc.rmw, tc.domainID, tc.topic)
			}
			if id != tc.wantID {
				t.Fatalf("id = %q, want %q", id, tc.wantID)
			}
			domainID, topic, ok := ParseROS2SourceID(id)
			if !ok {
				t.Fatalf("ParseROS2SourceID(%q) did not parse", id)
			}
			if domainID != wantDomain || topic != tc.topic {
				t.Fatalf("ParseROS2SourceID(%q) = (%q, %q), want (%q, %q)", id, domainID, topic, wantDomain, tc.topic)
			}
			// A topic identifier must resolve to the same domain identifier the
			// pre-per-topic spelling used, or a deployed campaign naming that
			// domain and a new campaign naming one of its topics would end up
			// on different graphs.
			if tc.topic != "" {
				if parsedDomain, parsedTopic, ok := ParseROS2SourceID(domainID); !ok || parsedTopic != "" || parsedDomain != domainID {
					t.Fatalf("domain prefix %q of %q does not parse as a domain identifier: (%q, %q, %v)", domainID, id, parsedDomain, parsedTopic, ok)
				}
			}
		})
	}
}

func TestParseROS2SourceIDRejectsMalformed(t *testing.T) {
	for _, id := range []string{
		"",
		"v4l2:/dev/video0",
		"ros2:",
		"ros2:rmw_cyclonedds_cpp",
		"ros2::domain-1",
		"ros2:rmw_cyclonedds_cpp:domain-",
		"ros2:rmw_cyclonedds_cpp:domain-abc",
		"ros2:rmw_cyclonedds_cpp:dom-1",
		// A topic must be absolute; a relative remainder is a truncated or
		// hand-typed identifier, not a topic.
		"ros2:rmw_cyclonedds_cpp:domain-1:chatter",
		"ros2:rmw_cyclonedds_cpp:domain-1:",
	} {
		if domainID, topic, ok := ParseROS2SourceID(id); ok {
			t.Errorf("ParseROS2SourceID(%q) = (%q, %q, true), want not ok", id, domainID, topic)
		}
	}
}
