package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net"
	"strings"
	"testing"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/internal/shared/ros2inspection"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

type fakeROS2Server struct {
	agentpbv2.UnimplementedROS2ServiceServer
	topics func(context.Context, *agentpbv2.ListROS2TopicsRequest) (*agentpbv2.ListROS2TopicsResponse, error)
	info   func(context.Context, *agentpbv2.GetROS2TopicInfoRequest) (*agentpbv2.GetROS2TopicInfoResponse, error)
	echo   func(*agentpbv2.EchoROS2TopicRequest, grpc.ServerStreamingServer[agentpbv2.ROS2Message]) error
	hz     func(*agentpbv2.MonitorROS2HzRequest, grpc.ServerStreamingServer[agentpbv2.ROS2HzSample]) error
}

func (f *fakeROS2Server) ListTopics(ctx context.Context, req *agentpbv2.ListROS2TopicsRequest) (*agentpbv2.ListROS2TopicsResponse, error) {
	return f.topics(ctx, req)
}

func (f *fakeROS2Server) GetTopicInfo(ctx context.Context, req *agentpbv2.GetROS2TopicInfoRequest) (*agentpbv2.GetROS2TopicInfoResponse, error) {
	return f.info(ctx, req)
}

func (f *fakeROS2Server) EchoTopic(req *agentpbv2.EchoROS2TopicRequest, stream grpc.ServerStreamingServer[agentpbv2.ROS2Message]) error {
	return f.echo(req, stream)
}

func (f *fakeROS2Server) MonitorHz(req *agentpbv2.MonitorROS2HzRequest, stream grpc.ServerStreamingServer[agentpbv2.ROS2HzSample]) error {
	return f.hz(req, stream)
}

func ros2TestServer(t *testing.T, fake *fakeROS2Server) *mcpServer {
	t.Helper()
	listener := bufconn.Listen(1024 * 1024)
	g := grpc.NewServer()
	agentpbv2.RegisterROS2ServiceServer(g, fake)
	go func() { _ = g.Serve(listener) }()
	t.Cleanup(g.Stop)
	conn, err := grpc.NewClient("passthrough:///ros2-test", grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) {
		return listener.Dial()
	}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	s := New(&config.Config{}, nil)
	s.SetConn(&grpcclient.AgentConnection{Conn: conn})
	return s
}

func ros2Call(t *testing.T, s *mcpServer, ctx context.Context, name string, args map[string]any) *mcpgo.CallToolResult {
	t.Helper()
	srv := server.NewMCPServer("test", "test")
	s.registerROS2Tools(srv)
	tool, exists := srv.ListTools()[name]
	if !exists {
		t.Fatalf("tool %s is not registered", name)
	}
	result, err := tool.Handler(ctx, callToolReq(name, args))
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	return result
}

func TestROS2ToolsReadOnlyAndDisconnected(t *testing.T) {
	s := New(&config.Config{}, nil)
	srv := server.NewMCPServer("test", "test")
	s.registerROS2Tools(srv)
	if len(srv.ListTools()) != 5 {
		t.Fatalf("got %d ROS2 tools", len(srv.ListTools()))
	}
	for name, tool := range srv.ListTools() {
		a := tool.Tool.Annotations
		if a.ReadOnlyHint == nil || !*a.ReadOnlyHint || a.DestructiveHint == nil || *a.DestructiveHint || a.OpenWorldHint == nil || *a.OpenWorldHint {
			t.Errorf("%s must be read-only and closed-world: %+v", name, a)
		}
		result := ros2Call(t, s, context.Background(), name, map[string]any{"topic": "/lidar"})
		if !result.IsError || structuredMap(t, result)["error_code"] != string(errCodeNotConnected) {
			t.Errorf("%s: %+v", name, result)
		}
	}
}

func TestROS2StrictBudgets(t *testing.T) {
	for key, values := range map[string][]any{
		"count":            {0, -1, 21, 1.5, "2", true, nil, math.NaN(), math.Inf(1)},
		"duration_seconds": {0, 61, 0.5, "10", nil},
		"max_bytes":        {0, 2047, 100001, 3000.5, "32000"},
		"domain_id":        {-1, 233, 1.5, "0", nil},
	} {
		for _, value := range values {
			args := map[string]any{"topic": "/lidar", key: value}
			if _, err := parseROS2Options(callToolReq("", args), true, true); err == nil {
				t.Errorf("accepted %s=%v", key, value)
			}
		}
	}
	for _, topic := range []any{"", "/", "relative", "/lidar;stop", "/foo//bar", "/foo/1bar", "/" + strings.Repeat("a", 255), 42} {
		if _, err := parseROS2Options(callToolReq("", map[string]any{"topic": topic}), true, true); err == nil {
			t.Errorf("accepted invalid topic %v", topic)
		}
	}
	opts, err := parseROS2Options(callToolReq("", map[string]any{"topic": "/lidar/points", "count": json.Number("2"), "domain_id": float64(0)}), true, true)
	if err != nil || opts.domain == nil || *opts.domain != 0 || opts.count != 2 || opts.duration != 10*time.Second {
		t.Fatalf("valid arguments: %+v, %v", opts, err)
	}
	opts, err = parseROS2Options(callToolReq("", nil), false, false)
	if err != nil || opts.domain != nil || opts.duration != 45*time.Second {
		t.Fatalf("defaults: %+v, %v", opts, err)
	}
}

func TestROS2TopicsOmitUnverifiedCountsAndBoundOutput(t *testing.T) {
	s := ros2TestServer(t, &fakeROS2Server{topics: func(ctx context.Context, req *agentpbv2.ListROS2TopicsRequest) (*agentpbv2.ListROS2TopicsResponse, error) {
		if req.IncludeCounts || req.DomainId == nil || *req.DomainId != 42 {
			t.Errorf("unexpected request: %v", req)
		}
		if _, ok := ctx.Deadline(); !ok {
			t.Error("RPC has no deadline")
		}
		topics := make([]*agentpbv2.ROS2Topic, 100)
		for i := range topics {
			topics[i] = &agentpbv2.ROS2Topic{Name: "/lidar", Types: []string{"sensor_msgs/msg/PointCloud2"}, Rmw: "rmw_cyclonedds_cpp", PublisherCount: 99}
		}
		return &agentpbv2.ListROS2TopicsResponse{Topics: topics}, nil
	}})
	result := ros2Call(t, s, context.Background(), "ros2_topics", map[string]any{"domain_id": 42, "max_bytes": 2048})
	env := structuredMap(t, result)
	if result.IsError || env["truncated"] != true || env["status"] != "discovered" {
		t.Fatalf("unexpected result: %+v", env)
	}
	if strings.Contains(toolResultText(t, result), "publisher_count") || !proxiedResultFitsMaxBytes(result, 2048) {
		t.Fatalf("counts or output exceeded budget: %s", toolResultText(t, result))
	}
}

func TestROS2TopicInfoIncludesQoSAndZeroCounts(t *testing.T) {
	s := ros2TestServer(t, &fakeROS2Server{info: func(_ context.Context, req *agentpbv2.GetROS2TopicInfoRequest) (*agentpbv2.GetROS2TopicInfoResponse, error) {
		if req.Topic != "/lidar" || req.DomainId != nil {
			t.Errorf("unexpected request: %v", req)
		}
		return &agentpbv2.GetROS2TopicInfoResponse{Topic: &agentpbv2.ROS2Topic{Name: req.Topic, SubscriberCount: 1}, Verbose: "Publisher count: 0\nSubscription count: 1\nReliability: BEST_EFFORT"}, nil
	}})
	result := ros2Call(t, s, context.Background(), "ros2_topic_info", map[string]any{"topic": "/lidar"})
	env := structuredMap(t, result)
	if result.IsError || env["source_freshness"] != "unknown" || !strings.Contains(env["verbose"].(string), "BEST_EFFORT") {
		t.Fatalf("unexpected result: %+v", env)
	}
	if env["topic_info"].(map[string]any)["publisher_count"] != int32(0) {
		t.Fatal("zero publisher count must be explicit")
	}
}

func TestROS2StreamsNoReadingsAreUnknown(t *testing.T) {
	s := ros2TestServer(t, &fakeROS2Server{
		echo: func(_ *agentpbv2.EchoROS2TopicRequest, stream grpc.ServerStreamingServer[agentpbv2.ROS2Message]) error {
			return stream.Send(&agentpbv2.ROS2Message{Yaml: "  \n"})
		},
		hz: func(_ *agentpbv2.MonitorROS2HzRequest, _ grpc.ServerStreamingServer[agentpbv2.ROS2HzSample]) error {
			return nil
		},
	})
	for _, name := range []string{"ros2_topic_sample", "ros2_topic_hz"} {
		result := ros2Call(t, s, context.Background(), name, map[string]any{"topic": "/lidar"})
		env := structuredMap(t, result)
		if result.IsError || env["status"] != "unknown" || env["sample_count"] != 0 || env["stop_reason"] != "end_of_stream" {
			t.Fatalf("%s: %+v", name, env)
		}
		if len(env["samples"].([]map[string]any)) != 0 {
			t.Fatalf("%s returned invented readings", name)
		}
	}
}

func TestROS2SamplePreservesPartialDataOnDecodeError(t *testing.T) {
	s := ros2TestServer(t, &fakeROS2Server{echo: func(req *agentpbv2.EchoROS2TopicRequest, stream grpc.ServerStreamingServer[agentpbv2.ROS2Message]) error {
		if req.Count != 3 || req.DomainId == nil || *req.DomainId != 0 {
			t.Errorf("unexpected request: %v", req)
		}
		if err := stream.Send(&agentpbv2.ROS2Message{Yaml: "range: 1.2\n"}); err != nil {
			return err
		}
		return status.Error(codes.Internal, "cannot decode custom message")
	}})
	result := ros2Call(t, s, context.Background(), "ros2_topic_sample", map[string]any{"topic": "/lidar", "domain_id": 0})
	env := structuredMap(t, result)
	if !result.IsError || env["status"] != "observed" || env["sample_count"] != 1 || env["stop_reason"] != "rpc_error" || env["source_freshness"] != "unknown" {
		t.Fatalf("unexpected result: %+v", env)
	}
	samples := env["samples"].([]map[string]any)
	if len(samples) != 1 || samples[0]["yaml"] != "range: 1.2\n" {
		t.Fatalf("partial samples lost: %+v", samples)
	}
	if _, err := time.Parse(time.RFC3339Nano, samples[0]["received_at"].(string)); err != nil {
		t.Fatal(err)
	}
}

func TestROS2StreamCountAndByteLimitsCancelSubscription(t *testing.T) {
	for _, large := range []bool{false, true} {
		t.Run(map[bool]string{false: "count", true: "bytes"}[large], func(t *testing.T) {
			cancelled := make(chan struct{})
			s := ros2TestServer(t, &fakeROS2Server{echo: func(_ *agentpbv2.EchoROS2TopicRequest, stream grpc.ServerStreamingServer[agentpbv2.ROS2Message]) error {
				yaml := "range: 1\n"
				if large {
					yaml = strings.Repeat("\"\n\\", 4000)
				}
				if err := stream.Send(&agentpbv2.ROS2Message{Yaml: yaml}); err != nil {
					return err
				}
				<-stream.Context().Done()
				close(cancelled)
				return nil
			}})
			result := ros2Call(t, s, context.Background(), "ros2_topic_sample", map[string]any{"topic": "/lidar", "count": 1, "max_bytes": 2048})
			env := structuredMap(t, result)
			wantReason := "count_limit"
			if large {
				wantReason = "byte_limit"
			}
			if result.IsError || env["stop_reason"] != wantReason || env["sample_count"] != 1 || env["truncated"] != large || !proxiedResultFitsMaxBytes(result, 2048) {
				t.Fatalf("unexpected result: %+v", env)
			}
			select {
			case <-cancelled:
			case <-time.After(time.Second):
				t.Fatal("device subscription was not cancelled")
			}
		})
	}
}

func TestROS2StreamDeadlineAndCallerCancellation(t *testing.T) {
	for _, callerCancel := range []bool{false, true} {
		t.Run(map[bool]string{false: "deadline", true: "caller_cancel"}[callerCancel], func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			cancelled := make(chan struct{})
			s := ros2TestServer(t, &fakeROS2Server{hz: func(_ *agentpbv2.MonitorROS2HzRequest, stream grpc.ServerStreamingServer[agentpbv2.ROS2HzSample]) error {
				if callerCancel {
					cancel()
				}
				<-stream.Context().Done()
				close(cancelled)
				return nil
			}})
			result := ros2Call(t, s, ctx, "ros2_topic_hz", map[string]any{"topic": "/lidar", "duration_seconds": 1})
			env := structuredMap(t, result)
			wantReason := "time_limit"
			if callerCancel {
				wantReason = "cancelled"
			}
			if result.IsError != callerCancel || env["status"] != "unknown" || env["stop_reason"] != wantReason {
				t.Fatalf("unexpected result: %+v", env)
			}
			select {
			case <-cancelled:
			case <-time.After(time.Second):
				t.Fatal("device subscription was not cancelled")
			}
		})
	}
}

func TestROS2HzUsesDeviceMeasurements(t *testing.T) {
	s := ros2TestServer(t, &fakeROS2Server{hz: func(req *agentpbv2.MonitorROS2HzRequest, stream grpc.ServerStreamingServer[agentpbv2.ROS2HzSample]) error {
		return stream.Send(&agentpbv2.ROS2HzSample{Hz: 20, MinDelta: 0.04, MaxDelta: 0.06, StdDev: 0.002, Window: 100})
	}})
	result := ros2Call(t, s, context.Background(), "ros2_topic_hz", map[string]any{"topic": "/lidar", "count": 1})
	env := structuredMap(t, result)
	if result.IsError || env["measurement"] != "device_subscription_rate" || env["source_freshness"] != "unknown" || env["samples"].([]map[string]any)[0]["hz"] != float64(20) {
		t.Fatalf("unexpected result: %+v", env)
	}
}

func TestROS2PeerDeadlineBeforeLocalTimerEndsObservation(t *testing.T) {
	fake := &fakeROS2Server{
		echo: func(*agentpbv2.EchoROS2TopicRequest, grpc.ServerStreamingServer[agentpbv2.ROS2Message]) error {
			return status.Error(codes.DeadlineExceeded, "peer observation deadline")
		},
		hz: func(*agentpbv2.MonitorROS2HzRequest, grpc.ServerStreamingServer[agentpbv2.ROS2HzSample]) error {
			return status.Error(codes.DeadlineExceeded, "peer observation deadline")
		},
	}
	s := ros2TestServer(t, fake)
	for _, name := range []string{"ros2_topic_sample", "ros2_topic_hz"} {
		result := ros2Call(t, s, context.Background(), name, map[string]any{"topic": "/lidar", "duration_seconds": 60})
		env := structuredMap(t, result)
		if result.IsError || env["status"] != "unknown" || env["stop_reason"] != "time_limit" {
			t.Fatalf("%s: peer deadline should end the observation window: %+v", name, env)
		}
	}
}

func TestROS2ReceiveLimitAndUnsupportedAgent(t *testing.T) {
	for _, code := range []codes.Code{codes.ResourceExhausted, codes.Unimplemented} {
		t.Run(code.String(), func(t *testing.T) {
			s := ros2TestServer(t, &fakeROS2Server{echo: func(_ *agentpbv2.EchoROS2TopicRequest, stream grpc.ServerStreamingServer[agentpbv2.ROS2Message]) error {
				if code == codes.Unimplemented {
					return status.Error(code, "unsupported")
				}
				return stream.Send(&agentpbv2.ROS2Message{Yaml: strings.Repeat("a", ros2MaxReceiveBytes+1)})
			}})
			result := ros2Call(t, s, context.Background(), "ros2_topic_sample", map[string]any{"topic": "/lidar"})
			env := structuredMap(t, result)
			if !result.IsError || env["status"] != "unknown" || env["sample_count"] != 0 || env["stop_reason"] != "rpc_error" {
				t.Fatalf("unexpected result: %+v", env)
			}
			if code == codes.Unimplemented && !strings.Contains(env["message"].(string), "wendy device update") {
				t.Fatal("missing agent update guidance")
			}
		})
	}
}

func TestROS2ErrorPreservesTracebackCauseAndBoundsEscapedOutput(t *testing.T) {
	message := "topic echo exited with status 1: Traceback:\n" + strings.Repeat("  intermediate stack frame\n", 100) + "ModuleNotFoundError: No module named 'unitree_go'"
	result := ros2Error(status.Error(codes.Internal, message))
	env := structuredMap(t, result)
	if !result.IsError || env["status"] != "unknown" || !strings.Contains(env["message"].(string), "ModuleNotFoundError") || !strings.Contains(env["message"].(string), "topic echo exited") {
		t.Fatalf("lost traceback context: %+v", env)
	}
	result = ros2Error(status.Error(codes.Internal, strings.Repeat("\x00", 10000)))
	if !result.IsError || !proxiedResultFitsMaxBytes(result, 2048) || structuredMap(t, result)["status"] != "unknown" {
		t.Fatalf("escaped error exceeds limit: %+v", result)
	}
}

func TestROS2HostScopeRequiresDomainAndValidScope(t *testing.T) {
	for _, args := range []map[string]any{{"scope": "host"}, {"scope": "HOST", "domain_id": 0}, {"scope": true}, {"scope": nil}} {
		if _, err := parseROS2Options(callToolReq("", args), false, false); err == nil {
			t.Errorf("accepted host options: %v", args)
		}
	}
	opts, err := parseROS2Options(callToolReq("", map[string]any{"scope": "host", "domain_id": 0}), false, false)
	if err != nil || opts.scope != "host" || opts.domain == nil || *opts.domain != 0 {
		t.Fatalf("explicit host domain rejected: %+v, %v", opts, err)
	}
	opts, err = parseROS2Options(callToolReq("", map[string]any{"domain_id": 0}), false, false)
	if err != nil || opts.scope != "app" {
		t.Fatal("a domain override silently enabled host inspection")
	}
}

func TestROS2HostScopeRequiresAcknowledgementForAllTools(t *testing.T) {
	for _, ack := range []bool{false, true} {
		for _, name := range []string{"ros2_topics", "ros2_topic_info", "ros2_topic_sample", "ros2_topic_hz"} {
			t.Run(fmt.Sprintf("%s/ack=%t", name, ack), func(t *testing.T) {
				prepare := func(ctx context.Context, domain *int32) {
					md, _ := metadata.FromIncomingContext(ctx)
					if scopes := md.Get(ros2inspection.ScopeMetadata); len(scopes) != 1 || scopes[0] != "host" || domain == nil || *domain != 0 {
						t.Errorf("wrong inspection request: %v, domain=%v", md, domain)
					}
					if ack {
						if err := grpc.SendHeader(ctx, metadata.Pairs(ros2inspection.ScopeMetadata, "host")); err != nil {
							t.Error(err)
						}
					}
				}
				s := ros2TestServer(t, &fakeROS2Server{
					topics: func(ctx context.Context, req *agentpbv2.ListROS2TopicsRequest) (*agentpbv2.ListROS2TopicsResponse, error) {
						prepare(ctx, req.DomainId)
						return &agentpbv2.ListROS2TopicsResponse{Topics: []*agentpbv2.ROS2Topic{{Name: "/odom", Types: []string{"nav_msgs/msg/Odometry"}}}}, nil
					},
					info: func(ctx context.Context, req *agentpbv2.GetROS2TopicInfoRequest) (*agentpbv2.GetROS2TopicInfoResponse, error) {
						prepare(ctx, req.DomainId)
						return &agentpbv2.GetROS2TopicInfoResponse{Topic: &agentpbv2.ROS2Topic{Name: "/odom"}, Verbose: "Publisher count: 1"}, nil
					},
					echo: func(req *agentpbv2.EchoROS2TopicRequest, stream grpc.ServerStreamingServer[agentpbv2.ROS2Message]) error {
						prepare(stream.Context(), req.DomainId)
						return stream.Send(&agentpbv2.ROS2Message{Yaml: "position: 1"})
					},
					hz: func(req *agentpbv2.MonitorROS2HzRequest, stream grpc.ServerStreamingServer[agentpbv2.ROS2HzSample]) error {
						prepare(stream.Context(), req.DomainId)
						return stream.Send(&agentpbv2.ROS2HzSample{Hz: 10, MinDelta: 0.1, MaxDelta: 0.1, Window: 3})
					},
				})
				result := ros2Call(t, s, context.Background(), name, map[string]any{"scope": "host", "domain_id": 0, "topic": "/odom", "count": 1})
				env := structuredMap(t, result)
				if !ack {
					if !result.IsError || !strings.Contains(env["message"].(string), "did not acknowledge") {
						t.Fatalf("accepted unacknowledged host result: %+v", env)
					}
					return
				}
				if result.IsError || env["inspection_scope"] != "host" || env["inspection_rmw"] != "rmw_fastrtps_cpp" || env["inspection_distro"] != "humble" {
					t.Fatalf("host scope missing from result: %+v", env)
				}
			})
		}
	}
}
