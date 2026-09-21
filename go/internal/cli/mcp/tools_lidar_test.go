package mcp

import (
	"context"
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/internal/shared/ros2inspection"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

func lidarTestSummary(topic, state string) map[string]any {
	return map[string]any{"schema_version": 1, "topic": topic, "message_type": "sensor_msgs/msg/PointCloud2", "status": state, "source_freshness": "unknown",
		"frame_id": "base_link", "included_points": 2, "sectors": []map[string]any{{"axis": "-y", "point_count": 2, "min_distance_m": 0.45, "nearest_xyz_m": []float64{0, -0.45, 0.1}}}}
}

func sendLidarSummary(stream grpc.ServerStreamingServer[agentpbv2.ROS2Message], sample map[string]any) error {
	data, _ := json.Marshal(sample)
	return stream.Send(&agentpbv2.ROS2Message{Topic: "/points", Yaml: string(data)})
}

func TestLidarToolStrictOptions(t *testing.T) {
	for key, values := range map[string][]any{
		"min_z": {true, "1", math.NaN(), math.Inf(1), -101, 2},
		"max_z": {-1, 101, nil}, "min_range": {-1, 20}, "max_range": {0.01, 1001},
		"count": {0, 6, 1.5}, "max_points": {99, 1000001, "100"}, "sample_points": {-1, 129, 2.5},
		"message_type": {"std_msgs/msg/String", 5}, "target_frame": {"/base_link", "x;echo test", 5},
		"use_sim_time": {"true", 1}, "scope": {"host"},
	} {
		for _, value := range values {
			args := map[string]any{"topic": "/points", key: value}
			result := ros2Call(t, New(&config.Config{}, nil), context.Background(), "ros2_lidar_summary", args)
			if !result.IsError || structuredMap(t, result)["error_code"] != string(errCodeInvalidArgument) {
				t.Errorf("accepted %s=%v: %+v", key, value, result)
			}
		}
	}
}

func TestLidarToolTransportsOptionsAndRequiresBothAcknowledgments(t *testing.T) {
	for _, scope := range []string{"app", "host"} {
		t.Run(scope, func(t *testing.T) {
			fake := &fakeROS2Server{echo: func(req *agentpbv2.EchoROS2TopicRequest, stream grpc.ServerStreamingServer[agentpbv2.ROS2Message]) error {
				md, _ := metadata.FromIncomingContext(stream.Context())
				if req.GetTopic() != "/points" || req.GetCount() != 1 || req.GetDomainId() != 42 || md.Get(ros2inspection.ScopeMetadata)[0] != scope || md.Get(ros2inspection.LidarMetadata)[0] != "1" {
					t.Errorf("wrong request: %+v %v", req, md)
				}
				var probe ros2inspection.LidarOptions
				if err := json.Unmarshal([]byte(md.Get(ros2inspection.LidarOptionsMetadata)[0]), &probe); err != nil || probe.Validate() != nil || probe.TargetFrame != "base_link" || probe.SamplePoints != 0 || !probe.UseSimTime || probe.MinZ != 0.1 {
					t.Errorf("wrong probe options: %+v %v", probe, err)
				}
				if err := stream.SendHeader(metadata.Pairs(ros2inspection.LidarMetadata, "1", ros2inspection.ScopeMetadata, scope)); err != nil {
					return err
				}
				return sendLidarSummary(stream, lidarTestSummary(req.Topic, "observed"))
			}}
			result := ros2Call(t, ros2TestServer(t, fake), context.Background(), "ros2_lidar_summary", map[string]any{"topic": "/points", "scope": scope, "domain_id": 42, "target_frame": "base_link", "sample_points": 0, "use_sim_time": true, "min_z": 0.1})
			env := structuredMap(t, result)
			if result.IsError || env["status"] != "observed" || env["source_freshness"] != "unknown" || env["sample_count"] != 1 || env["inspection_scope"] != scope {
				t.Fatalf("bad result: %+v", env)
			}
			samples := env["samples"].([]map[string]any)
			if len(samples) != 1 || samples[0]["frame_id"] != "base_link" {
				t.Fatalf("lost summary: %+v", samples)
			}
		})
	}
}

func TestLidarToolRejectsLegacyAndWrongScope(t *testing.T) {
	for _, test := range []struct {
		name    string
		headers metadata.MD
		scope   string
	}{
		{"legacy", nil, "app"},
		{"wrong version", metadata.Pairs(ros2inspection.LidarMetadata, "2"), "app"},
		{"host not acknowledged", metadata.Pairs(ros2inspection.LidarMetadata, "1"), "host"},
	} {
		t.Run(test.name, func(t *testing.T) {
			fake := &fakeROS2Server{echo: func(req *agentpbv2.EchoROS2TopicRequest, stream grpc.ServerStreamingServer[agentpbv2.ROS2Message]) error {
				if err := stream.SendHeader(test.headers); err != nil {
					return err
				}
				return sendLidarSummary(stream, lidarTestSummary(req.Topic, "observed"))
			}}
			result := ros2Call(t, ros2TestServer(t, fake), context.Background(), "ros2_lidar_summary", map[string]any{"topic": "/points", "scope": test.scope, "domain_id": 0})
			if !result.IsError || structuredMap(t, result)["status"] != "unknown" {
				t.Fatalf("accepted unacknowledged summary: %+v", result)
			}
		})
	}
}

func TestLidarToolUnknownAndMalformedResults(t *testing.T) {
	for _, test := range []struct {
		name      string
		sample    map[string]any
		wantError bool
	}{
		{"missing sensor", map[string]any{"schema_version": 1, "topic": "/points", "message_type": "sensor_msgs/msg/PointCloud2", "status": "unknown", "error": map[string]any{"code": "no_messages", "message": "No data"}}, false},
		{"wrong topic", lidarTestSummary("/other", "observed"), true},
		{"not a summary", map[string]any{"data": []byte{1, 2, 3}}, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			fake := &fakeROS2Server{echo: func(_ *agentpbv2.EchoROS2TopicRequest, stream grpc.ServerStreamingServer[agentpbv2.ROS2Message]) error {
				if err := stream.SendHeader(metadata.Pairs(ros2inspection.LidarMetadata, "1")); err != nil {
					return err
				}
				return sendLidarSummary(stream, test.sample)
			}}
			result := ros2Call(t, ros2TestServer(t, fake), context.Background(), "ros2_lidar_summary", map[string]any{"topic": "/points"})
			if result.IsError != test.wantError || structuredMap(t, result)["status"] != "unknown" {
				t.Fatalf("bad result: %+v", result)
			}
		})
	}
}

func TestLidarToolLegacyLargeCloudSuggestsAgentUpdate(t *testing.T) {
	fake := &fakeROS2Server{echo: func(_ *agentpbv2.EchoROS2TopicRequest, stream grpc.ServerStreamingServer[agentpbv2.ROS2Message]) error {
		return stream.Send(&agentpbv2.ROS2Message{Topic: "/points", Yaml: strings.Repeat("data: 0\n", 10000)})
	}}
	result := ros2Call(t, ros2TestServer(t, fake), context.Background(), "ros2_lidar_summary", map[string]any{"topic": "/points"})
	env := structuredMap(t, result)
	if !result.IsError || env["status"] != "unknown" || !strings.Contains(env["message"].(string), "update its agent") {
		t.Fatalf("missing compatibility diagnostic: %+v", env)
	}
}

func TestLidarToolByteBudgetNeverTurnsOmittedDataIntoObservation(t *testing.T) {
	fake := &fakeROS2Server{echo: func(req *agentpbv2.EchoROS2TopicRequest, stream grpc.ServerStreamingServer[agentpbv2.ROS2Message]) error {
		if err := stream.SendHeader(metadata.Pairs(ros2inspection.LidarMetadata, "1")); err != nil {
			return err
		}
		sample := lidarTestSummary(req.Topic, "observed")
		sample["extra"] = strings.Repeat("x", 10000)
		return sendLidarSummary(stream, sample)
	}}
	result := ros2Call(t, ros2TestServer(t, fake), context.Background(), "ros2_lidar_summary", map[string]any{"topic": "/points", "max_bytes": 2048})
	env := structuredMap(t, result)
	if env["status"] != "unknown" || env["truncated"] != true || env["sample_count"] != 0 || env["received_count"] != 1 || len(env["samples"].([]map[string]any)) != 0 || !proxiedResultFitsMaxBytes(result, 2048) {
		t.Fatalf("bad truncation: %+v", env)
	}
}

func TestLidarToolTimeoutAndCancellation(t *testing.T) {
	for _, cancelParent := range []bool{false, true} {
		ctx, cancel := context.WithCancel(context.Background())
		fake := &fakeROS2Server{echo: func(_ *agentpbv2.EchoROS2TopicRequest, stream grpc.ServerStreamingServer[agentpbv2.ROS2Message]) error {
			if err := stream.SendHeader(metadata.Pairs(ros2inspection.LidarMetadata, "1")); err != nil {
				return err
			}
			if cancelParent {
				cancel()
			}
			<-stream.Context().Done()
			return status.Error(codes.DeadlineExceeded, "window elapsed")
		}}
		start := time.Now()
		result := ros2Call(t, ros2TestServer(t, fake), ctx, "ros2_lidar_summary", map[string]any{"topic": "/points", "duration_seconds": 1})
		cancel()
		env := structuredMap(t, result)
		if env["status"] != "unknown" || time.Since(start) > 3*time.Second {
			t.Fatalf("timeout lost: %+v", env)
		}
		if !cancelParent && env["stop_reason"] != "time_limit" {
			t.Fatalf("bad stop reason: %+v", env)
		}
	}
}

func TestLidarToolFinalErrorBudgetKeepsObservationMetadataConsistent(t *testing.T) {
	fake := &fakeROS2Server{echo: func(req *agentpbv2.EchoROS2TopicRequest, stream grpc.ServerStreamingServer[agentpbv2.ROS2Message]) error {
		if err := stream.SendHeader(metadata.Pairs(ros2inspection.LidarMetadata, "1")); err != nil {
			return err
		}
		sample := lidarTestSummary(req.Topic, "observed")
		sample["extra"] = strings.Repeat("x", 600)
		if err := sendLidarSummary(stream, sample); err != nil {
			return err
		}
		return status.Error(codes.Internal, strings.Repeat("failure", 60))
	}}
	result := ros2Call(t, ros2TestServer(t, fake), context.Background(), "ros2_lidar_summary", map[string]any{"topic": "/points", "count": 2, "max_bytes": 3500})
	env := structuredMap(t, result)
	samples := env["samples"].([]map[string]any)
	if !result.IsError || !proxiedResultFitsMaxBytes(result, 3500) || env["sample_count"] != len(samples) {
		t.Fatalf("inconsistent final budget: %+v", env)
	}
	if len(samples) == 0 && env["status"] != "unknown" {
		t.Fatalf("omitted observation reported as available: %+v", env)
	}
	if env["stop_reason"] != "rpc_error" || env["truncated"] != true || len(samples) != 0 {
		t.Fatalf("fixture must fit before the RPC error, then require dropping the summary: %+v", env)
	}
}
