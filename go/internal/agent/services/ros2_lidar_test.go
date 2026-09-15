package services

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/ros2inspection"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

func lidarServiceOptions() ros2inspection.LidarOptions {
	opts := ros2inspection.DefaultLidarOptions()
	opts.Topic = "/utlidar/cloud_deskewed"
	opts.TargetFrame = "base_link"
	opts.UseSimTime = true
	return opts
}

func lidarServiceMetadata(t *testing.T, opts ros2inspection.LidarOptions) metadata.MD {
	t.Helper()
	encoded, err := json.Marshal(opts)
	if err != nil {
		t.Fatal(err)
	}
	return metadata.Pairs(ros2inspection.LidarMetadata, ros2inspection.LidarVersion,
		ros2inspection.LidarOptionsMetadata, string(encoded))
}

func lidarServiceSummary(topic, state string) string {
	encoded, _ := json.Marshal(map[string]any{"schema_version": 1, "topic": topic, "status": state})
	return string(encoded)
}

func lidarServiceCalls(runtime *fakeROS2Runtime) []ROS2ExecOptions {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	return append([]ROS2ExecOptions(nil), runtime.calls...)
}

func requireOnlyLidarExec(t *testing.T, runtime *fakeROS2Runtime, opts ros2inspection.LidarOptions) ROS2ExecOptions {
	t.Helper()
	calls := lidarServiceCalls(runtime)
	if len(calls) != 1 || calls[0].Lidar == nil || len(calls[0].Args) != 0 {
		t.Fatalf("expected exactly one fixed LiDAR probe and no raw echo fallback: %+v", calls)
	}
	if !reflect.DeepEqual(*calls[0].Lidar, opts) {
		t.Fatalf("validated probe options changed: got %+v, want %+v", calls[0].Lidar, opts)
	}
	return calls[0]
}

func TestROS2LidarAcknowledgesAppAndHostBeforeProbeOutput(t *testing.T) {
	for _, scope := range []string{ros2inspection.AppScope, ros2inspection.HostScope} {
		t.Run(scope, func(t *testing.T) {
			opts := lidarServiceOptions()
			release := make(chan struct{})
			runtime := &hostInspectionRuntime{fakeROS2Runtime: &fakeROS2Runtime{
				sidecar: ROS2Sidecar{Name: "app-inspector", Distro: "humble", DomainID: 9},
				execFn: func(ctx context.Context, _ ROS2ExecOptions, stdout, _ io.Writer) (int, error) {
					select {
					case <-release:
						_, err := io.WriteString(stdout, lidarServiceSummary(opts.Topic, "observed")+"\n")
						return 0, err
					case <-ctx.Done():
						return 130, ctx.Err()
					}
				},
			}}
			client := hostInspectionClient(t, runtime)
			md := lidarServiceMetadata(t, opts)
			md.Set(ros2inspection.ScopeMetadata, scope)
			ctx, cancel := context.WithTimeout(metadata.NewOutgoingContext(context.Background(), md), 3*time.Second)
			defer cancel()
			domain := int32(23)
			stream, err := client.EchoTopic(ctx, &agentpbv2.EchoROS2TopicRequest{Topic: opts.Topic, Count: int32(opts.Count), DomainId: &domain})
			if err != nil {
				t.Fatal(err)
			}
			headers, err := stream.Header()
			if err != nil {
				t.Fatalf("feature acknowledgment blocked waiting for probe output: %v", err)
			}
			if got := headers.Get(ros2inspection.LidarMetadata); !reflect.DeepEqual(got, []string{ros2inspection.LidarVersion}) {
				t.Fatalf("missing LiDAR feature acknowledgment: %v", headers)
			}
			if got := headers.Get(ros2inspection.ScopeMetadata); scope == ros2inspection.HostScope && !reflect.DeepEqual(got, []string{scope}) {
				t.Fatalf("missing combined host acknowledgment: %v", headers)
			} else if scope == ros2inspection.AppScope && len(got) != 0 {
				t.Fatalf("app inspection incorrectly acknowledged a host scope: %v", headers)
			}
			close(release)
			message, err := stream.Recv()
			if err != nil || message.GetTopic() != opts.Topic || message.GetYaml() != lidarServiceSummary(opts.Topic, "observed") {
				t.Fatalf("summary: %+v, %v", message, err)
			}
			if _, err := stream.Recv(); err != io.EOF {
				t.Fatalf("expected clean count-limited EOF, got %v", err)
			}
			call := requireOnlyLidarExec(t, runtime.fakeROS2Runtime, opts)
			wantName, wantApp, wantHost := "app-inspector", int32(1), int32(0)
			if scope == ros2inspection.HostScope {
				wantName, wantApp, wantHost = "host-inspector", 0, 1
			}
			if call.SidecarName != wantName || call.DomainID != int(domain) || runtime.appCalls.Load() != wantApp || runtime.hostCalls.Load() != wantHost {
				t.Fatalf("wrong sidecar scope: call=%+v app=%d host=%d", call, runtime.appCalls.Load(), runtime.hostCalls.Load())
			}
		})
	}
}

func TestROS2LidarRejectsInvalidMetadataBeforeProvisioning(t *testing.T) {
	opts := lidarServiceOptions()
	valid := lidarServiceMetadata(t, opts)
	for _, tc := range []struct {
		name   string
		mutate func(metadata.MD, *agentpbv2.EchoROS2TopicRequest)
	}{
		{"missing version", func(md metadata.MD, _ *agentpbv2.EchoROS2TopicRequest) { md.Delete(ros2inspection.LidarMetadata) }},
		{"missing options", func(md metadata.MD, _ *agentpbv2.EchoROS2TopicRequest) {
			md.Delete(ros2inspection.LidarOptionsMetadata)
		}},
		{"unknown version", func(md metadata.MD, _ *agentpbv2.EchoROS2TopicRequest) { md.Set(ros2inspection.LidarMetadata, "2") }},
		{"duplicate version", func(md metadata.MD, _ *agentpbv2.EchoROS2TopicRequest) { md.Append(ros2inspection.LidarMetadata, "1") }},
		{"duplicate options", func(md metadata.MD, _ *agentpbv2.EchoROS2TopicRequest) {
			md.Append(ros2inspection.LidarOptionsMetadata, md.Get(ros2inspection.LidarOptionsMetadata)[0])
		}},
		{"malformed JSON", func(md metadata.MD, _ *agentpbv2.EchoROS2TopicRequest) {
			md.Set(ros2inspection.LidarOptionsMetadata, "{")
		}},
		{"null options", func(md metadata.MD, _ *agentpbv2.EchoROS2TopicRequest) {
			md.Set(ros2inspection.LidarOptionsMetadata, "null")
		}},
		{"unknown field", func(md metadata.MD, _ *agentpbv2.EchoROS2TopicRequest) {
			md.Set(ros2inspection.LidarOptionsMetadata, strings.TrimSuffix(md.Get(ros2inspection.LidarOptionsMetadata)[0], "}")+`,"code":"arbitrary()"}`)
		}},
		{"multiple objects", func(md metadata.MD, _ *agentpbv2.EchoROS2TopicRequest) {
			md.Set(ros2inspection.LidarOptionsMetadata, md.Get(ros2inspection.LidarOptionsMetadata)[0]+" {}")
		}},
		{"oversized options", func(md metadata.MD, _ *agentpbv2.EchoROS2TopicRequest) {
			md.Set(ros2inspection.LidarOptionsMetadata, md.Get(ros2inspection.LidarOptionsMetadata)[0]+strings.Repeat(" ", 4096))
		}},
		{"unsafe topic", func(md metadata.MD, _ *agentpbv2.EchoROS2TopicRequest) {
			md.Set(ros2inspection.LidarOptionsMetadata, strings.ReplaceAll(md.Get(ros2inspection.LidarOptionsMetadata)[0], opts.Topic, "/cloud;$(id)"))
		}},
		{"unsupported message type", func(md metadata.MD, _ *agentpbv2.EchoROS2TopicRequest) {
			md.Set(ros2inspection.LidarOptionsMetadata, strings.ReplaceAll(md.Get(ros2inspection.LidarOptionsMetadata)[0], opts.MessageType, "std_msgs/msg/String"))
		}},
		{"topic mismatch", func(_ metadata.MD, req *agentpbv2.EchoROS2TopicRequest) { req.Topic = "/other_cloud" }},
		{"count mismatch", func(_ metadata.MD, req *agentpbv2.EchoROS2TopicRequest) { req.Count++ }},
		{"host without domain", func(md metadata.MD, req *agentpbv2.EchoROS2TopicRequest) {
			md.Set(ros2inspection.ScopeMetadata, ros2inspection.HostScope)
			req.DomainId = nil
		}},
		{"invalid scope", func(md metadata.MD, _ *agentpbv2.EchoROS2TopicRequest) { md.Set(ros2inspection.ScopeMetadata, "all") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runtime := &hostInspectionRuntime{fakeROS2Runtime: &fakeROS2Runtime{}}
			client := hostInspectionClient(t, runtime)
			md := valid.Copy()
			req := &agentpbv2.EchoROS2TopicRequest{Topic: opts.Topic, Count: int32(opts.Count)}
			tc.mutate(md, req)
			ctx, cancel := context.WithTimeout(metadata.NewOutgoingContext(context.Background(), md), 3*time.Second)
			defer cancel()
			stream, err := client.EchoTopic(ctx, req)
			if err == nil {
				_, err = stream.Recv()
			}
			if status.Code(err) != codes.InvalidArgument {
				t.Fatalf("expected invalid request error, got %v", err)
			}
			if runtime.appCalls.Load() != 0 || runtime.hostCalls.Load() != 0 || len(lidarServiceCalls(runtime.fakeROS2Runtime)) != 0 {
				t.Fatal("invalid LiDAR metadata reached sidecar provisioning or execution")
			}
		})
	}
}

func TestROS2LidarCountLimitUnblocksRuntimeWrite(t *testing.T) {
	opts := lidarServiceOptions()
	type completion struct{ writeErr, contextErr error }
	completed := make(chan completion, 1)
	runtime := &fakeROS2Runtime{sidecar: ROS2Sidecar{Name: "app-inspector"},
		execFn: func(ctx context.Context, _ ROS2ExecOptions, stdout, _ io.Writer) (int, error) {
			// One write exceeds the pipe scanner's read buffer. After the first
			// summary satisfies Count, this write can finish only if the reader
			// is closed; merely cancelling ctx leaves the write blocked forever.
			_, err := io.WriteString(stdout, lidarServiceSummary(opts.Topic, "observed")+"\n"+strings.Repeat("x", 2*ros2inspection.LidarMaxSummaryBytes))
			completed <- completion{err, ctx.Err()}
			return 130, ctx.Err()
		}}
	client := hostInspectionClient(t, runtime)
	ctx, cancel := context.WithTimeout(metadata.NewOutgoingContext(context.Background(), lidarServiceMetadata(t, opts)), 3*time.Second)
	defer cancel()
	stream, err := client.EchoTopic(ctx, &agentpbv2.EchoROS2TopicRequest{Topic: opts.Topic, Count: int32(opts.Count)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := stream.Recv(); err != nil {
		t.Fatal(err)
	}
	if _, err := stream.Recv(); err != io.EOF {
		t.Fatalf("count limit failed to finish cleanly: %v", err)
	}
	select {
	case result := <-completed:
		if result.writeErr == nil || !errors.Is(result.contextErr, context.Canceled) {
			t.Fatalf("count limit did not cancel and unblock writer: %+v", result)
		}
	case <-ctx.Done():
		t.Fatal("runtime remained blocked after count limit")
	}
	requireOnlyLidarExec(t, runtime, opts)
}

func TestROS2LidarCancellationAndDeadlineStopRuntime(t *testing.T) {
	for _, deadline := range []bool{false, true} {
		name := "client cancellation"
		if deadline {
			name = "probe deadline"
		}
		t.Run(name, func(t *testing.T) {
			opts := lidarServiceOptions()
			opts.DurationSeconds = 1
			started, stopped := make(chan struct{}), make(chan struct{})
			runtime := &fakeROS2Runtime{sidecar: ROS2Sidecar{Name: "app-inspector"},
				execFn: func(ctx context.Context, _ ROS2ExecOptions, _, _ io.Writer) (int, error) {
					close(started)
					<-ctx.Done()
					close(stopped)
					return 130, ctx.Err()
				}}
			client := hostInspectionClient(t, runtime)
			ctx, cancel := context.WithTimeout(metadata.NewOutgoingContext(context.Background(), lidarServiceMetadata(t, opts)), 3*time.Second)
			defer cancel()
			stream, err := client.EchoTopic(ctx, &agentpbv2.EchoROS2TopicRequest{Topic: opts.Topic, Count: int32(opts.Count)})
			if err != nil {
				t.Fatal(err)
			}
			select {
			case <-started:
			case <-ctx.Done():
				t.Fatal("probe did not start")
			}
			want := codes.DeadlineExceeded
			if !deadline {
				cancel()
				want = codes.Canceled
			}
			if _, err := stream.Recv(); status.Code(err) != want {
				t.Fatalf("stream error = %v, want %v", err, want)
			}
			select {
			case <-stopped:
			case <-time.After(2 * time.Second):
				t.Fatal("runtime survived stream cancellation")
			}
			requireOnlyLidarExec(t, runtime, opts)
		})
	}
}

func TestROS2LidarRejectsInvalidOutputWithoutRawEchoFallback(t *testing.T) {
	opts := lidarServiceOptions()
	for _, tc := range []struct {
		name, payload string
		code          codes.Code
	}{
		{"raw YAML", "data: [1, 2, 3]\n", codes.Internal},
		{"malformed JSON", "{\n", codes.Internal},
		{"wrong schema", `{"schema_version":2,"topic":"` + opts.Topic + `","status":"observed"}` + "\n", codes.Internal},
		{"wrong topic", lidarServiceSummary("/other_cloud", "observed") + "\n", codes.Internal},
		{"invalid status", lidarServiceSummary(opts.Topic, "clear") + "\n", codes.Internal},
		{"oversized summary", `{"schema_version":1,"topic":"` + opts.Topic + `","status":"observed","padding":"` + strings.Repeat("x", ros2inspection.LidarMaxSummaryBytes) + "\"}\n", codes.Internal},
		{"no summary", "", codes.Unavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runtime := &fakeROS2Runtime{sidecar: ROS2Sidecar{Name: "app-inspector"},
				execFn: func(_ context.Context, _ ROS2ExecOptions, stdout, _ io.Writer) (int, error) {
					_, err := io.WriteString(stdout, tc.payload)
					return 0, err
				}}
			client := hostInspectionClient(t, runtime)
			ctx, cancel := context.WithTimeout(metadata.NewOutgoingContext(context.Background(), lidarServiceMetadata(t, opts)), 3*time.Second)
			defer cancel()
			stream, err := client.EchoTopic(ctx, &agentpbv2.EchoROS2TopicRequest{Topic: opts.Topic, Count: int32(opts.Count)})
			if err != nil {
				t.Fatal(err)
			}
			message, err := stream.Recv()
			if message != nil || status.Code(err) != tc.code {
				t.Fatalf("invalid probe output forwarded or misreported: message=%+v error=%v", message, err)
			}
			requireOnlyLidarExec(t, runtime, opts)
		})
	}
}

func TestROS2LidarUnknownSummaryEndsBeforeRequestedCount(t *testing.T) {
	opts := lidarServiceOptions()
	opts.Count = 5
	payload := lidarServiceSummary(opts.Topic, "unknown")
	runtime := &fakeROS2Runtime{sidecar: ROS2Sidecar{Name: "app-inspector"},
		execFn: func(ctx context.Context, _ ROS2ExecOptions, stdout, _ io.Writer) (int, error) {
			_, err := io.WriteString(stdout, payload+"\n"+strings.Repeat("x", 2*ros2inspection.LidarMaxSummaryBytes))
			return 130, errors.Join(err, ctx.Err())
		}}
	client := hostInspectionClient(t, runtime)
	ctx, cancel := context.WithTimeout(metadata.NewOutgoingContext(context.Background(), lidarServiceMetadata(t, opts)), 3*time.Second)
	defer cancel()
	stream, err := client.EchoTopic(ctx, &agentpbv2.EchoROS2TopicRequest{Topic: opts.Topic, Count: int32(opts.Count)})
	if err != nil {
		t.Fatal(err)
	}
	message, err := stream.Recv()
	if err != nil || message.GetYaml() != payload {
		t.Fatalf("unknown result was discarded: %+v, %v", message, err)
	}
	if _, err := stream.Recv(); err != io.EOF {
		t.Fatalf("unknown result did not terminate stream: %v", err)
	}
	requireOnlyLidarExec(t, runtime, opts)
}
