package services

import (
	"context"
	"errors"
	"net"
	"sync/atomic"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/shared/ros2inspection"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

type hostInspectionRuntime struct {
	*fakeROS2Runtime
	hostCalls atomic.Int32
	appCalls  atomic.Int32
}

func (r *hostInspectionRuntime) EnsureROS2Sidecars(ctx context.Context) ([]ROS2Sidecar, error) {
	r.appCalls.Add(1)
	return r.fakeROS2Runtime.EnsureROS2Sidecars(ctx)
}
func (r *hostInspectionRuntime) EnsureHostROS2Sidecar(_ context.Context, opts ros2inspection.HostOptions) (ROS2Sidecar, error) {
	r.hostCalls.Add(1)
	return ROS2Sidecar{Name: "host-inspector", Distro: ros2inspection.HostDistro, RMW: ros2inspection.FastRTPSRMW, DomainID: opts.DomainID}, nil
}
func hostInspectionClient(t *testing.T, runtime ROS2Runtime) agentpbv2.ROS2ServiceClient {
	t.Helper()
	listener := bufconn.Listen(1024 * 1024)
	server := grpc.NewServer()
	agentpbv2.RegisterROS2ServiceServer(server, newTestROS2Service(t, runtime, t.TempDir()))
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)
	conn, err := grpc.NewClient("passthrough:///host-inspection-test", grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return agentpbv2.NewROS2ServiceClient(conn)
}
func hostInspectionContext() context.Context {
	return metadata.AppendToOutgoingContext(context.Background(), ros2inspection.ScopeMetadata, ros2inspection.HostScope)
}

func TestROS2HostInspectionWorksWithoutAppAndAcknowledgesScope(t *testing.T) {
	runtime := &hostInspectionRuntime{fakeROS2Runtime: &fakeROS2Runtime{ensureErr: errors.New("no running ROS 2 containers"), outputs: map[string]string{
		"topic list -t":       "/odom [nav_msgs/msg/Odometry]\n/lidar [sensor_msgs/msg/PointCloud2]\n",
		"topic info -v /odom": "Type: nav_msgs/msg/Odometry\nPublisher count: 1\nSubscription count: 0\n",
	}}}
	client := hostInspectionClient(t, runtime)
	domain := int32(0)
	var headers metadata.MD
	resp, err := client.ListTopics(hostInspectionContext(), &agentpbv2.ListROS2TopicsRequest{DomainId: &domain}, grpc.Header(&headers))
	if err != nil || len(resp.GetTopics()) != 2 {
		t.Fatalf("host topics: %+v, %v", resp, err)
	}
	if got := headers.Get(ros2inspection.ScopeMetadata); len(got) != 1 || got[0] != ros2inspection.HostScope {
		t.Fatalf("missing scope acknowledgement: %v", headers)
	}
	info, err := client.GetTopicInfo(hostInspectionContext(), &agentpbv2.GetROS2TopicInfoRequest{DomainId: &domain, Topic: "/odom"})
	if err != nil || info.GetTopic().GetPublisherCount() != 1 {
		t.Fatalf("host topic info: %+v, %v", info, err)
	}
	if runtime.appCalls.Load() != 0 || runtime.hostCalls.Load() != 2 {
		t.Fatalf("unexpected provisioning: app=%d host=%d", runtime.appCalls.Load(), runtime.hostCalls.Load())
	}
	for _, call := range runtime.calls {
		if call.SidecarName != "host-inspector" || call.DomainID != 0 {
			t.Fatalf("inspection did not route to explicit host domain: %+v", call)
		}
	}
}

func TestROS2HostInspectionNeverChangesDefaultAppScope(t *testing.T) {
	runtime := &hostInspectionRuntime{fakeROS2Runtime: &fakeROS2Runtime{ensureErr: errors.New("no running ROS 2 containers")}}
	client := hostInspectionClient(t, runtime)
	domain := int32(0)
	_, err := client.ListTopics(context.Background(), &agentpbv2.ListROS2TopicsRequest{DomainId: &domain})
	if status.Code(err) != codes.FailedPrecondition || runtime.hostCalls.Load() != 0 || runtime.appCalls.Load() != 1 {
		t.Fatalf("domain alone must not enable host fallback: %v", err)
	}
}

func TestROS2HostInspectionRequiresExplicitValidScopeAndDomain(t *testing.T) {
	for _, tc := range []struct {
		name   string
		scope  []string
		domain *int32
	}{
		{"no domain", []string{"host"}, nil},
		{"negative domain", []string{"host"}, func() *int32 { d := int32(-1); return &d }()},
		{"high domain", []string{"host"}, func() *int32 { d := int32(233); return &d }()},
		{"unknown scope", []string{"all"}, nil},
		{"duplicate scope", []string{"host", "app"}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runtime := &hostInspectionRuntime{fakeROS2Runtime: &fakeROS2Runtime{}}
			client := hostInspectionClient(t, runtime)
			ctx := metadata.NewOutgoingContext(context.Background(), metadata.MD{ros2inspection.ScopeMetadata: tc.scope})
			_, err := client.ListTopics(ctx, &agentpbv2.ListROS2TopicsRequest{DomainId: tc.domain})
			if status.Code(err) != codes.InvalidArgument || runtime.hostCalls.Load() != 0 || runtime.appCalls.Load() != 0 {
				t.Fatalf("invalid request reached runtime: %v", err)
			}
		})
	}
}

func TestROS2HostInspectionRejectsServiceCallsAndRawExec(t *testing.T) {
	runtime := &hostInspectionRuntime{fakeROS2Runtime: &fakeROS2Runtime{}}
	client := hostInspectionClient(t, runtime)
	domain := int32(0)
	_, err := client.CallService(hostInspectionContext(), &agentpbv2.CallROS2ServiceRequest{DomainId: &domain, Service: "/move"})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("host service call was not rejected: %v", err)
	}
	stream, err := client.Exec(hostInspectionContext(), &agentpbv2.ROS2ExecRequest{DomainId: &domain, Args: []string{"topic", "pub", "/cmd_vel"}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = stream.Recv()
	if status.Code(err) != codes.InvalidArgument || runtime.hostCalls.Load() != 0 || runtime.appCalls.Load() != 0 {
		t.Fatalf("host raw exec reached runtime: %v", err)
	}
}
