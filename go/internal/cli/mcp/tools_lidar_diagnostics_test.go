package mcp

import (
	"context"
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/shared/ros2inspection"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

func TestLidarDiagnosticsDistinguishReceiptFromInspectionFailure(t *testing.T) {
	for _, test := range []struct {
		name, scope, targetFrame, probeError  string
		rpcCode                               codes.Code
		observed                              bool
		duration                              int
		outcome, sensorMessages, nextContains string
	}{
		{name: "app timeout", rpcCode: codes.DeadlineExceeded, outcome: "inspection_timeout", sensorMessages: "unknown", nextContains: "localhost-only"},
		{name: "host timeout", scope: "host", rpcCode: codes.DeadlineExceeded, outcome: "inspection_timeout", sensorMessages: "unknown", nextContains: "duration_seconds=60"},
		{name: "host transform timeout", scope: "host", targetFrame: "base_link", rpcCode: codes.DeadlineExceeded, outcome: "inspection_timeout", sensorMessages: "unknown", nextContains: "without target_frame"},
		{name: "full budget timeout", scope: "host", duration: 60, rpcCode: codes.DeadlineExceeded, outcome: "inspection_timeout", sensorMessages: "unknown", nextContains: "before repeating"},
		{name: "confirmed absent messages", scope: "host", probeError: "no_messages", outcome: "no_messages", sensorMessages: "none_in_observation_window", nextContains: "QoS"},
		{name: "confirmed absent messages in app", probeError: "no_messages", outcome: "no_messages", sensorMessages: "none_in_observation_window", nextContains: "ros2_topic_info"},
		{name: "missing transform", targetFrame: "base_link", probeError: "transform_unavailable", outcome: "transform_unavailable", sensorMessages: "received", nextContains: "without target_frame"},
		{name: "invalid transform", targetFrame: "base_link", probeError: "invalid_transform", outcome: "transform_unavailable", sensorMessages: "received", nextContains: "TF path"},
		{name: "oversized cloud", probeError: "point_limit", outcome: "decode_error", sensorMessages: "received", nextContains: "point limit"},
		{name: "malformed cloud", probeError: "invalid_cloud", outcome: "decode_error", sensorMessages: "received", nextContains: "message format"},
		{name: "successful summary", observed: true, outcome: "summary_received", sensorMessages: "received"},
		{name: "summary before timeout", observed: true, rpcCode: codes.DeadlineExceeded, outcome: "summary_received", sensorMessages: "received"},
		{name: "transport error", rpcCode: codes.Internal, outcome: "inspection_error", sensorMessages: "unknown", nextContains: "ros2_topic_info"},
	} {
		t.Run(test.name, func(t *testing.T) {
			fake := &fakeROS2Server{echo: func(req *agentpbv2.EchoROS2TopicRequest, stream grpc.ServerStreamingServer[agentpbv2.ROS2Message]) error {
				if err := stream.SendHeader(metadata.Pairs(ros2inspection.LidarMetadata, "1", ros2inspection.ScopeMetadata, test.scope)); err != nil {
					return err
				}
				if test.observed || test.probeError != "" {
					sample := lidarTestSummary(req.Topic, "observed")
					if test.probeError != "" {
						sample["status"] = "unknown"
						sample["error"] = map[string]any{"code": test.probeError, "message": "Probe diagnosis"}
					}
					if err := sendLidarSummary(stream, sample); err != nil {
						return err
					}
				}
				return status.Error(test.rpcCode, "Inspection stopped")
			}}
			args := map[string]any{"topic": "/points", "count": 2}
			if test.scope != "" {
				args["scope"], args["domain_id"] = test.scope, 42
			}
			if test.targetFrame != "" {
				args["target_frame"] = test.targetFrame
			}
			if test.duration != 0 {
				args["duration_seconds"] = test.duration
			}
			result := ros2Call(t, ros2TestServer(t, fake), context.Background(), "ros2_lidar_summary", args)
			env := structuredMap(t, result)
			if env["outcome"] != test.outcome || env["sensor_messages"] != test.sensorMessages {
				t.Fatalf("incorrect sensor evidence: %+v", env)
			}
			next, _ := env["suggested_next_step"].(string)
			if test.nextContains != "" && !strings.Contains(next, test.nextContains) {
				t.Fatalf("missing recovery guidance %q: %+v", test.nextContains, env)
			}
			if test.observed && (env["status"] != "observed" || env["sample_count"] != 1) {
				t.Fatalf("lost partial evidence: %+v", env)
			}
			if test.rpcCode == codes.Internal && (!result.IsError || !strings.Contains(env["message"].(string), "Inspection stopped")) {
				t.Fatalf("lost original RPC error: %+v", env)
			}
		})
	}
}

func TestLidarDiagnosticsFitMinimumBudget(t *testing.T) {
	for _, scope := range []string{"app", "host"} {
		for _, oversized := range []bool{false, true} {
			t.Run(scope+map[bool]string{false: "/timeout", true: "/omitted"}[oversized], func(t *testing.T) {
				fake := &fakeROS2Server{echo: func(req *agentpbv2.EchoROS2TopicRequest, stream grpc.ServerStreamingServer[agentpbv2.ROS2Message]) error {
					if err := stream.SendHeader(metadata.Pairs(ros2inspection.LidarMetadata, "1", ros2inspection.ScopeMetadata, scope)); err != nil {
						return err
					}
					if oversized {
						sample := lidarTestSummary(req.Topic, "observed")
						sample["extra"] = strings.Repeat("x", 10000)
						return sendLidarSummary(stream, sample)
					}
					return status.Error(codes.DeadlineExceeded, "Inspection stopped")
				}}
				result := ros2Call(t, ros2TestServer(t, fake), context.Background(), "ros2_lidar_summary", map[string]any{
					"topic": "/" + strings.Repeat("x", 254), "scope": scope, "domain_id": 42, "max_bytes": 2048,
				})
				env := structuredMap(t, result)
				wantOutcome := "inspection_timeout"
				if oversized {
					wantOutcome = "summaries_omitted"
				}
				if !proxiedResultFitsMaxBytes(result, 2048) || env["outcome"] != wantOutcome || env["sensor_messages"] != "unknown" || env["sample_count"] != 0 || env["status"] != "unknown" {
					t.Fatalf("lost or contradictory diagnostics within minimum budget: %+v", env)
				}
			})
		}
	}
}
