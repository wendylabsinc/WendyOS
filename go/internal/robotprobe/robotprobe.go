// Package robotprobe holds the read-only probes a robot inspection runs. Each probe
// speaks one transport and lives beside it; the schema and the reconciliation are in
// shared/robotinspect and know nothing about any of this.
//
// Probes take their transport as a handle out of the inspection Env, so the same probe
// runs from the CLI over a LAN or inside the agent on the device, and a test can hand it
// recorded payloads instead.
package robotprobe

import (
	"context"
	"strings"
	"time"
)

// TopicReader samples a DDS or ROS 2 topic. It is the handle a probe collects from Env
// under robotinspect.RequirementDDSDomain.
type TopicReader interface {
	// Sample collects the payloads published on topic, identified by its DDS type
	// name, for at most window. It stops early once maxMessages have arrived; a
	// maxMessages of zero or less collects everything in the window.
	//
	// Returning fewer payloads than asked for, including none, is not an error: a
	// topic that nobody publishes on is a finding, not a failure.
	Sample(ctx context.Context, topic, typeName string, window time.Duration, maxMessages int) ([][]byte, error)
}

// RawTopic is a topic seen by DDS discovery, named the way ROS names it and typed the way
// DDS advertises it. The type stays in the DDS spelling because that is what the wire
// carries: translating it would mean claiming to know a message the transport cannot
// decode.
type RawTopic struct {
	Name string
	Type string
	// WriterCount is how many publishers were discovered. More than one usually means
	// two of them are fighting over a topic.
	WriterCount int
}

// RawTopicLister reports what a transport can see without any robot software installed.
// It is what decides which probes have something to read, so a robot that is not a
// humanoid gets no humanoid rows.
type RawTopicLister interface {
	RawTopics(ctx context.Context) ([]RawTopic, error)
}

// streamName identifies a camera stream from its topic, keeping the whole namespace
// rather than one segment.
//
// Taking only the segment before the message name looked tidier and was wrong: a stereo
// pair publishing /cam_left/color/camera_info and /cam_right/color/camera_info both
// reduced to "color", so the inspection folded two cameras onto one property and
// reported two correct-but-different fields of view as a disagreement. A wrong answer is
// worse than a verbose identifier, so the identifier is the full path with the message
// name dropped: "cam_left.color".
func streamName(topic string) string {
	segments := strings.Split(strings.Trim(topic, "/"), "/")
	// Drop the trailing message name (camera_info, image_raw), which is not part of
	// the stream's identity. A bare /camera_info is then left naming nothing, which
	// is the truth: it identifies a message, not a camera.
	segments = segments[:len(segments)-1]
	var kept []string
	for _, segment := range segments {
		if segment != "" {
			kept = append(kept, segment)
		}
	}
	// Property IDs are already namespaced under "camera", so a leading camera segment
	// would only repeat it: /camera/color/camera_info is "color", while
	// /cam_left/color/camera_info keeps both and stays distinct from cam_right.
	if len(kept) > 1 && kept[0] == "camera" {
		kept = kept[1:]
	}
	if len(kept) == 0 {
		// A bare /camera_info names no stream at all.
		return "default"
	}
	return strings.Join(kept, ".")
}
