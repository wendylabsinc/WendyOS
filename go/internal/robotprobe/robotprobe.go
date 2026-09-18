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

// streamName reduces a camera topic to the stream it belongs to, so property IDs read
// "camera.color.fov.vertical" rather than carrying a whole topic path. The segment
// before the message name is the stream: /camera/color/camera_info is "color".
func streamName(topic string) string {
	segments := strings.Split(strings.Trim(topic, "/"), "/")
	if len(segments) < 2 {
		// A bare /camera_info names no stream, so it gets the generic one rather
		// than a property ID reading "camera.camera_info.fov.vertical".
		return "camera"
	}
	return segments[len(segments)-2]
}
