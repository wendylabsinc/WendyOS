package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"regexp"
	"strconv"
	"strings"
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

const ros2MaxReceiveBytes = 1024 * 1024

func (s *mcpServer) registerROS2Tools(srv *server.MCPServer) {
	s.registerLidarTool(srv)
	for _, tool := range []struct {
		name, description string
		topic, stream     bool
		handler           server.ToolHandlerFunc
	}{
		{"ros2_topics", "Discover ROS 2 topic names, types and RMW on the connected device. Discovery does not prove readings are arriving. Use ros2_topic_info for publisher counts and QoS.", false, false, s.handleROS2Topics},
		{"ros2_topic_info", "Inspect a ROS 2 topic's publisher/subscriber counts and verbose endpoint QoS. Counts do not prove sensor health or fresh readings.", true, false, s.handleROS2TopicInfo},
		{"ros2_topic_sample", "Collect a bounded YAML sample of a ROS 2 topic. Missing readings mean unknown, never clear or safe. Receive timestamps are local CLI times, not source freshness. Uses the agent's default echo QoS; custom message types must be available in its sidecar.", true, true, s.handleROS2TopicSample},
		{"ros2_topic_hz", "Observe a bounded set of ROS 2 topic rate reports measured by the device-side subscription, not cloud arrival timing. No reports mean unknown, not zero Hz. Rate does not establish source freshness or navigation readiness. Uses the agent's default subscription QoS.", true, true, s.handleROS2TopicHz},
	} {
		defaultDuration := 45
		if tool.stream {
			defaultDuration = 10
		}
		opts := []mcpgo.ToolOption{
			mcpgo.WithDescription(tool.description),
			mcpgo.WithString("scope", mcpgo.Enum("app", "host"), mcpgo.Description("Inspection network: app (default) preserves app isolation; host explicitly observes the device/subnet DDS graph without a running ROS 2 app. Host requires domain_id and uses stock ROS Humble/FastRTPS; first use may download its inspector image.")),
			mcpgo.WithNumber("domain_id", mcpgo.Description("Integer ROS_DOMAIN_ID (0..232), required for host scope; app scope otherwise uses the app configuration")),
			mcpgo.WithNumber("duration_seconds", mcpgo.Description(fmt.Sprintf("Maximum total RPC duration including discovery, integer 1..60 (default %d)", defaultDuration))),
			mcpgo.WithNumber("max_bytes", mcpgo.Description("Maximum serialized result bytes, integer 2048..100000 (default 32000); oversized samples are omitted whole")),
		}
		if tool.topic {
			opts = append(opts, mcpgo.WithString("topic", mcpgo.Required(), mcpgo.Description("Absolute ROS topic name from ros2_topics, at most 255 bytes")))
		}
		if tool.stream {
			opts = append(opts, mcpgo.WithNumber("count", mcpgo.Description("Maximum samples/rate reports, integer 1..20 (default 3)")))
		}
		opts = append(opts, readOnly()...)
		opts = append(opts, localOnly()...)
		srv.AddTool(mcpgo.NewTool(tool.name, opts...), tool.handler)
	}
}

type ros2Options struct {
	scope    string
	domain   *int32
	topic    string
	duration time.Duration
	maxBytes int
	count    int
}

var ros2TopicName = regexp.MustCompile(`^/([A-Za-z_][A-Za-z0-9_]*)(/[A-Za-z_][A-Za-z0-9_]*)*$`)

// GetInt silently truncates fractional numbers; diagnostic budgets must not.
func ros2Int(req mcpgo.CallToolRequest, key string, fallback, min, max int) (int, error) {
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
			return 0, fmt.Errorf("%s must be an integer in %d..%d", key, min, max)
		}
	default:
		return 0, fmt.Errorf("%s must be an integer in %d..%d", key, min, max)
	}
	if math.IsNaN(n) || math.IsInf(n, 0) || math.Trunc(n) != n || n < float64(min) || n > float64(max) {
		return 0, fmt.Errorf("%s must be an integer in %d..%d", key, min, max)
	}
	return int(n), nil
}

func parseROS2Options(req mcpgo.CallToolRequest, topic, stream bool) (ros2Options, error) {
	var opts ros2Options
	opts.scope = ros2inspection.AppScope
	if value, exists := req.GetArguments()["scope"]; exists {
		scope, ok := value.(string)
		if !ok || (scope != ros2inspection.AppScope && scope != ros2inspection.HostScope) {
			return opts, fmt.Errorf("scope must be app or host")
		}
		opts.scope = scope
	}
	if topic {
		opts.topic = stringParam(req, "topic")
		if len(opts.topic) > 255 || !ros2TopicName.MatchString(opts.topic) {
			return opts, fmt.Errorf("topic must be an absolute ROS topic name of at most 255 bytes")
		}
	}
	if _, exists := req.GetArguments()["domain_id"]; exists {
		domain, err := ros2Int(req, "domain_id", 0, 0, 232)
		if err != nil {
			return opts, err
		}
		domain32 := int32(domain)
		opts.domain = &domain32
	}
	if opts.scope == ros2inspection.HostScope && opts.domain == nil {
		return opts, fmt.Errorf("host inspection requires an explicit domain_id (0..232)")
	}
	defaultDuration := 45
	if stream {
		defaultDuration = 10
	}
	duration, err := ros2Int(req, "duration_seconds", defaultDuration, 1, 60)
	if err != nil {
		return opts, err
	}
	opts.duration = time.Duration(duration) * time.Second
	opts.maxBytes, err = ros2Int(req, "max_bytes", 32000, 2048, 100000)
	if err != nil {
		return opts, err
	}
	if stream {
		opts.count, err = ros2Int(req, "count", 3, 1, 20)
	}
	return opts, err
}

func (opts ros2Options) rpcContext(ctx context.Context) context.Context {
	md, _ := metadata.FromOutgoingContext(ctx)
	md = md.Copy()
	md.Set(ros2inspection.ScopeMetadata, opts.scope)
	return metadata.NewOutgoingContext(ctx, md)
}

func (opts ros2Options) verifyScope(headers metadata.MD) error {
	if opts.scope != ros2inspection.HostScope {
		return nil
	}
	scopes := headers.Get(ros2inspection.ScopeMetadata)
	if len(scopes) != 1 || scopes[0] != ros2inspection.HostScope {
		return status.Error(codes.FailedPrecondition, "The device did not acknowledge host ROS 2 inspection; update its agent before retrying. App-scoped results were not accepted.")
	}
	return nil
}

func (opts ros2Options) addScope(env map[string]any) {
	env["inspection_scope"] = opts.scope
	if opts.domain != nil {
		env["inspection_domain_id"] = *opts.domain
	}
	if opts.scope == ros2inspection.HostScope {
		env["inspection_rmw"] = ros2inspection.FastRTPSRMW
		env["inspection_distro"] = ros2inspection.HostDistro
	}
}

func ros2Error(err error) *mcpgo.CallToolResult {
	code, message := codeFromGRPC(err), grpcErrString(err)
	if status.Code(err) == codes.Unimplemented {
		message = "This device's agent does not support ROS 2 inspection; update it with `wendy device update`."
	} else if status.Code(err) == codes.DeadlineExceeded || err == context.DeadlineExceeded {
		code = errCodeTimeout
	}
	// Error details can contain unbounded device command output.
	if len(message) > 400 {
		// Python tracebacks put the missing message/typesupport cause at the end.
		message = strings.ToValidUTF8(message[:100], "") + " ... (truncated) ... " + strings.ToValidUTF8(message[len(message)-280:], "")
	}
	return ros2Result(map[string]any{"error_code": string(code), "message": message, "status": "unknown"}, 2048)
}

// Keep metadata when payloads exceed the limit; a generic truncation envelope
// would lose the distinction between absent readings and omitted readings.
func ros2Result(env map[string]any, maxBytes int) *mcpgo.CallToolResult {
	result := okResult(env)
	_, result.IsError = env["error_code"]
	if proxiedResultFitsMaxBytes(result, maxBytes) {
		return result
	}
	env["truncated"] = true
	for _, key := range []string{"verbose", "topics", "samples", "topic_info", "message"} {
		if key == "topics" || key == "samples" {
			if _, exists := env[key]; exists {
				env[key] = []map[string]any{}
			}
		} else {
			delete(env, key)
		}
		result = okResult(env)
		_, result.IsError = env["error_code"]
		if proxiedResultFitsMaxBytes(result, maxBytes) {
			return result
		}
	}
	return result // The remaining fixed metadata fits the minimum 2048-byte budget.
}

func (s *mcpServer) handleROS2Topics(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	opts, err := parseROS2Options(req, false, false)
	if err != nil {
		return errResult(errCodeInvalidArgument, err.Error()), nil
	}
	conn := s.GetConn()
	if conn == nil {
		return errNotConnected(), nil
	}
	ctx, cancel := context.WithTimeout(ctx, opts.duration)
	defer cancel()
	ctx = opts.rpcContext(ctx)
	var headers metadata.MD
	resp, err := agentpbv2.NewROS2ServiceClient(conn.Conn).ListTopics(ctx, &agentpbv2.ListROS2TopicsRequest{DomainId: opts.domain}, grpc.MaxCallRecvMsgSize(ros2MaxReceiveBytes), grpc.Header(&headers))
	if err != nil {
		return ros2Error(err), nil
	}
	if err := opts.verifyScope(headers); err != nil {
		return ros2Error(err), nil
	}
	// IncludeCounts can silently leave zeroes after failed per-topic probes.
	// Omit counts entirely here and use the targeted GetTopicInfo RPC instead.
	topics := make([]map[string]any, 0)
	env := map[string]any{"topics": topics, "status": "unknown", "total_topics": len(resp.GetTopics()), "truncated": false}
	opts.addScope(env)
	if len(resp.GetTopics()) > 0 {
		env["status"] = "discovered"
	}
	for _, topic := range resp.GetTopics() {
		candidate := append(topics, map[string]any{"name": topic.GetName(), "types": listOrEmpty(topic.GetTypes()), "rmw": topic.GetRmw()})
		env["topics"] = candidate
		if !proxiedResultFitsMaxBytes(okResult(env), opts.maxBytes) {
			env["truncated"] = true
			break
		}
		topics = candidate
	}
	env["topics"] = topics
	return ros2Result(env, opts.maxBytes), nil
}

func (s *mcpServer) handleROS2TopicInfo(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	opts, err := parseROS2Options(req, true, false)
	if err != nil {
		return errResult(errCodeInvalidArgument, err.Error()), nil
	}
	conn := s.GetConn()
	if conn == nil {
		return errNotConnected(), nil
	}
	ctx, cancel := context.WithTimeout(ctx, opts.duration)
	defer cancel()
	ctx = opts.rpcContext(ctx)
	var headers metadata.MD
	resp, err := agentpbv2.NewROS2ServiceClient(conn.Conn).GetTopicInfo(ctx, &agentpbv2.GetROS2TopicInfoRequest{DomainId: opts.domain, Topic: opts.topic}, grpc.MaxCallRecvMsgSize(ros2MaxReceiveBytes), grpc.Header(&headers))
	if err != nil {
		return ros2Error(err), nil
	}
	if err := opts.verifyScope(headers); err != nil {
		return ros2Error(err), nil
	}
	topic := resp.GetTopic()
	if topic == nil || strings.TrimSpace(resp.GetVerbose()) == "" {
		return ros2Error(status.Error(codes.Internal, "The agent returned no topic endpoint information.")), nil
	}
	env := map[string]any{
		"topic": opts.topic, "status": "discovered", "source_freshness": "unknown", "truncated": false,
		"topic_info": map[string]any{"types": listOrEmpty(topic.GetTypes()), "rmw": topic.GetRmw(), "publisher_count": topic.GetPublisherCount(), "subscriber_count": topic.GetSubscriberCount()},
		"verbose":    resp.GetVerbose(),
	}
	opts.addScope(env)
	return ros2Result(env, opts.maxBytes), nil
}

func (s *mcpServer) handleROS2TopicSample(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	return s.handleROS2Stream(ctx, req, false)
}

func (s *mcpServer) handleROS2TopicHz(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	return s.handleROS2Stream(ctx, req, true)
}

func (s *mcpServer) handleROS2Stream(parent context.Context, req mcpgo.CallToolRequest, hz bool) (*mcpgo.CallToolResult, error) {
	opts, err := parseROS2Options(req, true, true)
	if err != nil {
		return errResult(errCodeInvalidArgument, err.Error()), nil
	}
	conn := s.GetConn()
	if conn == nil {
		return errNotConnected(), nil
	}
	ctx, cancel := context.WithTimeout(parent, opts.duration)
	defer cancel() // Release the device subscription on every exit, including count/byte limits.
	ctx = opts.rpcContext(ctx)
	client := agentpbv2.NewROS2ServiceClient(conn.Conn)
	var recv func() (map[string]any, error)
	if hz {
		stream, streamErr := client.MonitorHz(ctx, &agentpbv2.MonitorROS2HzRequest{DomainId: opts.domain, Topic: opts.topic}, grpc.MaxCallRecvMsgSize(ros2MaxReceiveBytes))
		if streamErr != nil {
			return ros2Error(streamErr), nil
		}
		if opts.scope == ros2inspection.HostScope {
			headers, err := stream.Header()
			if err != nil {
				return ros2Error(err), nil
			}
			if err := opts.verifyScope(headers); err != nil {
				return ros2Error(err), nil
			}
		}
		recv = func() (map[string]any, error) {
			msg, err := stream.Recv()
			if err != nil {
				return nil, err
			}
			if !ros2ValidHz(msg) {
				return nil, status.Error(codes.Internal, "The agent returned an invalid ROS 2 rate report.")
			}
			return map[string]any{"hz": msg.GetHz(), "min_delta_seconds": msg.GetMinDelta(), "max_delta_seconds": msg.GetMaxDelta(), "std_dev_seconds": msg.GetStdDev(), "window": msg.GetWindow()}, nil
		}
	} else {
		stream, streamErr := client.EchoTopic(ctx, &agentpbv2.EchoROS2TopicRequest{DomainId: opts.domain, Topic: opts.topic, Count: int32(opts.count)}, grpc.MaxCallRecvMsgSize(ros2MaxReceiveBytes))
		if streamErr != nil {
			return ros2Error(streamErr), nil
		}
		if opts.scope == ros2inspection.HostScope {
			headers, err := stream.Header()
			if err != nil {
				return ros2Error(err), nil
			}
			if err := opts.verifyScope(headers); err != nil {
				return ros2Error(err), nil
			}
		}
		recv = func() (map[string]any, error) {
			msg, err := stream.Recv()
			if err != nil || strings.TrimSpace(msg.GetYaml()) == "" {
				return nil, err
			}
			return map[string]any{"yaml": msg.GetYaml(), "received_at": time.Now().UTC().Format(time.RFC3339Nano)}, nil
		}
	}
	env := map[string]any{
		"topic": opts.topic, "status": "unknown", "source_freshness": "unknown",
		"samples": []map[string]any{}, "sample_count": 0, "stop_reason": "count_limit", "truncated": false,
	}
	opts.addScope(env)
	if hz {
		env["measurement"] = "device_subscription_rate"
	} else {
		env["measurement"] = "yaml_samples_with_cli_receive_times"
	}
	samples := make([]map[string]any, 0)
	for received := 0; received < opts.count; {
		sample, recvErr := recv()
		if recvErr != nil {
			switch {
			case parent.Err() != nil:
				env["stop_reason"] = "cancelled"
				err = parent.Err()
			case ctx.Err() == context.DeadlineExceeded || status.Code(recvErr) == codes.DeadlineExceeded:
				// The peer can report the propagated RPC deadline before the
				// local context timer fires. Both end the observation window.
				env["stop_reason"] = "time_limit"
			case recvErr == io.EOF:
				env["stop_reason"] = "end_of_stream"
			default:
				env["stop_reason"] = "rpc_error"
				err = recvErr
			}
			break
		}
		if sample == nil {
			continue
		}
		received++
		env["sample_count"], env["status"] = received, "observed"
		candidate := append(samples, sample)
		env["samples"] = candidate
		// Reserve room for terminal error details without discarding good samples.
		if !proxiedResultFitsMaxBytes(okResult(env), opts.maxBytes-1024) {
			env["stop_reason"], env["truncated"] = "byte_limit", true
			break
		}
		samples = candidate
	}
	env["samples"] = samples
	if err != nil {
		failure := ros2Error(err).StructuredContent.(map[string]any)
		env["error_code"], env["message"] = failure["error_code"], failure["message"]
	}
	return ros2Result(env, opts.maxBytes), nil
}

func ros2ValidHz(msg *agentpbv2.ROS2HzSample) bool {
	if msg == nil || msg.GetWindow() < 2 || msg.GetHz() <= 0 {
		return false
	}
	for _, value := range []float64{msg.GetHz(), msg.GetMinDelta(), msg.GetMaxDelta(), msg.GetStdDev()} {
		if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
			return false
		}
	}
	return msg.GetMaxDelta() >= msg.GetMinDelta()
}
