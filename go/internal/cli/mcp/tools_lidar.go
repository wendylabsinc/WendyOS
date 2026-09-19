package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"strconv"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/wendylabsinc/wendy/go/internal/shared/ros2inspection"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

func (s *mcpServer) registerLidarTool(srv *server.MCPServer) {
	opts := []mcpgo.ToolOption{
		mcpgo.WithDescription("Inspect LiDAR as compact spatial data with sensor-compatible QoS. Start with the default app scope: Wendy uses the running ROS app's DDS middleware, domain and discovery configuration, including host-network robot sensors. Discover topics with ros2_topics and keep the same scope/domain here; prefer a discovered point cloud or scan with a publisher. Explicit host scope uses a separate generic inspector and requires domain_id; it is not needed merely because LiDAR is built into a robot. Prefer this tool over raw YAML sampling. First sample without target_frame; add a verified body frame afterward for robot-relative axes. Read outcome and sensor_messages to distinguish an inspection timeout from confirmed no_messages; use suggested_next_step when present. Returns sector minima, XYZ samples, filters and timestamps. Empty sectors remain unknown."),
		mcpgo.WithString("topic", mcpgo.Required(), mcpgo.Description("Absolute topic discovered with ros2_topics, such as /utlidar/cloud_deskewed or /scan")),
		mcpgo.WithString("message_type", mcpgo.Enum("sensor_msgs/msg/PointCloud2", "sensor_msgs/msg/LaserScan"), mcpgo.Description("Discovered topic type; default sensor_msgs/msg/PointCloud2")),
		mcpgo.WithString("scope", mcpgo.Enum("app", "host"), mcpgo.Description("app (default) inherits the running ROS app's DDS configuration, including host discovery when configured. host uses a separate generic Humble/Fast DDS inspector and requires domain_id; use it when independently inspecting a host graph. Keep scope/domain consistent with discovery.")),
		mcpgo.WithNumber("domain_id", mcpgo.Description("Integer ROS_DOMAIN_ID 0..232, required for host scope")),
		mcpgo.WithString("target_frame", mcpgo.Description("Optional verified TF frame, e.g. base_link. Transform at the source timestamp; missing TF returns unknown. Default: original sensor/cloud frame.")),
		mcpgo.WithBoolean("use_sim_time", mcpgo.Description("Use ROS /clock for timestamp diagnostics and TF; only enable for a graph using simulated time (default false)")),
		mcpgo.WithNumber("duration_seconds", mcpgo.Description("Total RPC budget including discovery/setup, integer 1..60 (default 10)")),
		mcpgo.WithNumber("count", mcpgo.Description("Maximum independently summarized messages, integer 1..5 (default 1)")),
		mcpgo.WithNumber("min_z", mcpgo.Description("Minimum height in output frame, meters -100..100 (default -1); filters affect all reported distances")),
		mcpgo.WithNumber("max_z", mcpgo.Description("Maximum height in output frame, meters -100..100 (default 2), greater than min_z")),
		mcpgo.WithNumber("min_range", mcpgo.Description("Minimum horizontal XY distance from output-frame origin, meters 0..1000 (default 0.05)")),
		mcpgo.WithNumber("max_range", mcpgo.Description("Maximum horizontal XY distance, meters 0..1000 (default 20), greater than min_range")),
		mcpgo.WithNumber("max_points", mcpgo.Description("Decode budget per message, integer 100..1000000 (default 200000). Oversized clouds fail explicitly; points are never skipped when computing minima.")),
		mcpgo.WithNumber("sample_points", mcpgo.Description("Bounded representative XYZ output, integer 0..128 (default 32); sector minima use all included points")),
		mcpgo.WithNumber("max_bytes", mcpgo.Description("Maximum serialized tool result, integer 2048..100000 (default 32000); summaries omitted whole when over budget")),
	}
	opts = append(opts, readOnly()...)
	opts = append(opts, localOnly()...)
	srv.AddTool(mcpgo.NewTool("ros2_lidar_summary", opts...), s.handleLidarSummary)
}

func lidarNumber(req mcpgo.CallToolRequest, key string, fallback float64) (float64, error) {
	v, exists := req.GetArguments()[key]
	if !exists {
		return fallback, nil
	}
	var n float64
	switch v := v.(type) {
	case float64:
		n = v
	case int:
		n = float64(v)
	case int32:
		n = float64(v)
	case int64:
		n = float64(v)
	case json.Number:
		var err error
		n, err = strconv.ParseFloat(string(v), 64)
		if err != nil {
			return 0, fmt.Errorf("%s must be a finite number", key)
		}
	default:
		return 0, fmt.Errorf("%s must be a finite number", key)
	}
	if math.IsNaN(n) || math.IsInf(n, 0) {
		return 0, fmt.Errorf("%s must be a finite number", key)
	}
	return n, nil
}

func parseLidarOptions(req mcpgo.CallToolRequest) (ros2Options, ros2inspection.LidarOptions, error) {
	opts, err := parseROS2Options(req, true, true)
	p := ros2inspection.DefaultLidarOptions()
	if err != nil {
		return opts, p, err
	}
	p.Topic, p.DurationSeconds = opts.topic, int(opts.duration/time.Second)
	for key, dst := range map[string]*string{"message_type": &p.MessageType, "target_frame": &p.TargetFrame} {
		if value, exists := req.GetArguments()[key]; exists {
			var ok bool
			*dst, ok = value.(string)
			if !ok {
				return opts, p, fmt.Errorf("%s must be a string", key)
			}
		}
	}
	if value, exists := req.GetArguments()["use_sim_time"]; exists {
		var ok bool
		p.UseSimTime, ok = value.(bool)
		if !ok {
			return opts, p, fmt.Errorf("use_sim_time must be a boolean")
		}
	}
	for _, field := range []struct {
		key      string
		dst      *int
		min, max int
	}{
		{"count", &p.Count, 1, 5}, {"max_points", &p.MaxPoints, 100, 1000000}, {"sample_points", &p.SamplePoints, 0, 128},
	} {
		*field.dst, err = ros2Int(req, field.key, *field.dst, field.min, field.max)
		if err != nil {
			return opts, p, err
		}
	}
	opts.count = p.Count
	for key, dst := range map[string]*float64{"min_z": &p.MinZ, "max_z": &p.MaxZ, "min_range": &p.MinRange, "max_range": &p.MaxRange} {
		*dst, err = lidarNumber(req, key, *dst)
		if err != nil {
			return opts, p, err
		}
	}
	return opts, p, p.Validate()
}

func (s *mcpServer) handleLidarSummary(parent context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	opts, probe, err := parseLidarOptions(req)
	if err != nil {
		return errResult(errCodeInvalidArgument, err.Error()), nil
	}
	conn := s.GetConn()
	if conn == nil {
		return errNotConnected(), nil
	}
	ctx, cancel := context.WithTimeout(opts.rpcContext(parent), opts.duration)
	defer cancel()
	encoded, _ := json.Marshal(probe)
	ctx = metadata.AppendToOutgoingContext(ctx, ros2inspection.LidarMetadata, ros2inspection.LidarVersion, ros2inspection.LidarOptionsMetadata, string(encoded))
	stream, err := agentpbv2.NewROS2ServiceClient(conn.Conn).EchoTopic(ctx, &agentpbv2.EchoROS2TopicRequest{Topic: opts.topic, DomainId: opts.domain, Count: int32(opts.count)}, grpc.MaxCallRecvMsgSize(ros2inspection.LidarMaxSummaryBytes+1024))
	if err != nil {
		return ros2Error(err), nil
	}
	headers, err := stream.Header()
	if err != nil {
		return ros2Error(err), nil
	}
	versions := headers.Get(ros2inspection.LidarMetadata)
	if len(versions) != 1 || versions[0] != ros2inspection.LidarVersion {
		// Inspect terminal status first: an unavailable device should not be
		// misreported as old. Never accept an unacknowledged payload.
		if _, recvErr := stream.Recv(); recvErr != nil && recvErr != io.EOF && status.Code(recvErr) != codes.ResourceExhausted {
			return ros2Error(recvErr), nil
		}
		return ros2Error(status.Error(codes.FailedPrecondition, "The device did not acknowledge LiDAR summaries; update its agent with `wendy device update`. Raw topic data was not accepted.")), nil
	}
	if err := opts.verifyScope(headers); err != nil {
		return ros2Error(err), nil
	}
	env := map[string]any{"topic": opts.topic, "status": "unknown", "source_freshness": "unknown", "measurement": "device_lidar_summary", "samples": []map[string]any{}, "sample_count": 0, "received_count": 0, "stop_reason": "count_limit", "truncated": false}
	opts.addScope(env)
	samples := make([]map[string]any, 0)
	for i := 0; i < opts.count; i++ {
		msg, recvErr := stream.Recv()
		if recvErr != nil {
			switch {
			case parent.Err() != nil:
				env["stop_reason"], err = "cancelled", parent.Err()
			case status.Code(recvErr) == codes.DeadlineExceeded || ctx.Err() == context.DeadlineExceeded:
				env["stop_reason"] = "time_limit"
			case recvErr == io.EOF:
				env["stop_reason"] = "end_of_stream"
			default:
				env["stop_reason"], err = "rpc_error", recvErr
			}
			break
		}
		var sample map[string]any
		if json.Unmarshal([]byte(msg.GetYaml()), &sample) != nil || sample["schema_version"] != float64(1) || sample["topic"] != opts.topic || sample["message_type"] != probe.MessageType || (sample["status"] != "observed" && sample["status"] != "unknown") {
			env["stop_reason"], err = "invalid_summary", status.Error(codes.Internal, "The agent returned an invalid LiDAR summary")
			break
		}
		env["received_count"] = i + 1
		candidate := append(samples, sample)
		env["samples"] = candidate
		if !proxiedResultFitsMaxBytes(okResult(env), opts.maxBytes-768) {
			env["stop_reason"], env["truncated"] = "byte_limit", true
			break
		}
		samples = candidate
		env["sample_count"] = len(samples)
		if sample["status"] == "observed" {
			env["status"] = "observed"
		}
		if sample["error"] != nil {
			env["stop_reason"] = "probe_error"
			break
		}
	}
	env["samples"] = samples
	if err != nil {
		failure := ros2Error(err).StructuredContent.(map[string]any)
		env["error_code"], env["message"] = failure["error_code"], failure["message"]
	}
	addLidarDiagnostics(env, opts, probe)
	// Optional prose must not displace an otherwise usable sensor summary.
	if !proxiedResultFitsMaxBytes(okResult(env), opts.maxBytes) {
		delete(env, "suggested_next_step")
		if _, hasError := env["error_code"]; !hasError {
			delete(env, "message")
		}
	}
	// Error details can grow when JSON-escaped. Apply the final budget to the
	// complete envelope and keep counts/status tied to retained observations.
	// The generic ROS result limiter cannot recompute LiDAR sample metadata.
	for len(samples) > 0 && !proxiedResultFitsMaxBytes(okResult(env), opts.maxBytes) {
		samples = samples[:len(samples)-1]
		env["samples"], env["sample_count"], env["status"], env["truncated"] = samples, len(samples), "unknown", true
		for _, sample := range samples {
			if sample["status"] == "observed" {
				env["status"] = "observed"
				break
			}
		}
		addLidarDiagnostics(env, opts, probe)
	}
	// Preserve the machine-readable outcome even at the minimum output budget.
	// Recovery guidance can be longer than the fixed metadata in ros2Result.
	if !proxiedResultFitsMaxBytes(okResult(env), opts.maxBytes) {
		delete(env, "suggested_next_step")
	}
	return ros2Result(env, opts.maxBytes), nil
}
