package mcp

import (
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/ros2inspection"
)

// received_count counts summaries, not DDS messages. In particular, an RPC
// timeout can interrupt setup, subscription, decoding or TF before the probe
// returns its own diagnosis. Keep that distinct from a confirmed no_messages
// result so Chat does not report an unhealthy sensor on transport evidence.
func addLidarDiagnostics(env map[string]any, opts ros2Options, probe ros2inspection.LidarOptions) {
	outcome, sensorMessages := "inspection_incomplete", "unknown"
	message := "No usable LiDAR summary was returned; sensor message arrival is unknown."
	next := "Check the topic type, publishers and QoS with ros2_topic_info using the same scope/domain."
	samples, _ := env["samples"].([]map[string]any)
	for _, sample := range samples {
		if sample["status"] == "observed" {
			sensorMessages = "received"
			outcome, message, next = "summary_received", "LiDAR data was decoded; inspect the reported frame, filters and coverage before interpreting distances.", ""
		}
	}
	if sensorMessages != "received" {
		switch env["stop_reason"] {
		case "time_limit":
			outcome = "inspection_timeout"
			message = "Inspection timed out before a summary returned. This does not establish whether sensor messages arrived."
			next = "Verify scope/domain against discovery. Retry once with duration_seconds=60; setup and observation share this budget."
			if opts.duration >= 60*time.Second {
				next = "The maximum inspection budget elapsed. Check the inspector and publisher diagnostics on this scope/domain before repeating the sample."
			}
		case "cancelled":
			outcome, message, next = "cancelled", "LiDAR inspection was cancelled; sensor message arrival is unknown.", ""
		case "rpc_error", "invalid_summary":
			outcome = "inspection_error"
		}
		// Only the device-side subscriber can establish these outcomes.
		for _, sample := range samples {
			failure, _ := sample["error"].(map[string]any)
			switch failure["code"] {
			case "no_messages":
				outcome, sensorMessages = "no_messages", "none_in_observation_window"
				message = "The device-side subscriber received no sensor messages in the observation window on this scope/domain."
			case "transform_unavailable", "invalid_transform":
				outcome, sensorMessages = "transform_unavailable", "received"
				message = "A sensor message arrived, but the requested coordinate transform could not be used."
				next = "Retry the same topic/scope/domain without target_frame to inspect the source frame; verify its TF path before requesting body-relative coordinates."
			case "invalid_cloud", "invalid_scan", "invalid_stamp", "invalid_frame", "point_limit":
				outcome, sensorMessages = "decode_error", "received"
				message = "A sensor message arrived, but the probe could not summarize it; inspect samples[].error for the decoding or size limit."
				next = "Resolve the reported message format or point limit; increasing the observation duration will not fix a decode error."
			}
		}
		receivedCount, _ := env["received_count"].(int)
		if len(samples) == 0 && receivedCount > 0 && env["truncated"] == true {
			outcome, sensorMessages = "summaries_omitted", "unknown"
			message = "Summary output was omitted by max_bytes; the empty samples list does not establish missing sensor data."
			next = "Reduce sample_points or count, or increase max_bytes, then inspect the retained summary."
		}
	}
	if (outcome == "inspection_timeout" || outcome == "no_messages" || outcome == "inspection_incomplete") && opts.scope == ros2inspection.AppScope {
		next = "Check ros2_topic_info on this app graph for a publisher and the discovered message type. Wendy inherits the app's DDS domain, middleware and discovery mode; older agents forced localhost-only discovery, hiding host-network sensors. Do not switch to the generic host inspector merely because this is robot LiDAR."
	} else if outcome == "inspection_timeout" && probe.TargetFrame != "" {
		next = "Retry the same topic/scope/domain without target_frame and with duration_seconds=60 to separate sensor receipt from transform lookup."
	}
	env["outcome"], env["sensor_messages"] = outcome, sensorMessages
	// Retain an actual RPC error message when one is present.
	if _, hasError := env["error_code"]; !hasError {
		env["message"] = message
	}
	delete(env, "suggested_next_step")
	if next != "" {
		env["suggested_next_step"] = next
	}
}
