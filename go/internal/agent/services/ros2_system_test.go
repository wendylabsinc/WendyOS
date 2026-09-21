package services

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/shared/ros2inspection"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

type systemROS2Runtime struct {
	*fakeROS2Runtime
	appCalls    int
	systemCalls int
	systemErr   error
}

func (r *systemROS2Runtime) EnsureROS2Sidecars(ctx context.Context) ([]ROS2Sidecar, error) {
	r.appCalls++
	return r.fakeROS2Runtime.EnsureROS2Sidecars(ctx)
}

func (r *systemROS2Runtime) EnsureSystemROS2Sidecar(context.Context) (ROS2Sidecar, error) {
	r.systemCalls++
	return ROS2Sidecar{Name: "system", Distro: "humble", RMW: ros2inspection.FastRTPSRMW, DomainID: 0}, r.systemErr
}

func newSystemROS2Runtime() *systemROS2Runtime {
	return &systemROS2Runtime{fakeROS2Runtime: &fakeROS2Runtime{
		ensureErr: fmt.Errorf("discovering apps: %w", ErrNoRunningROS2Containers),
		outputs: map[string]string{
			"node list":                     "/robot\n",
			"topic list -t":                 "/odom [nav_msgs/msg/Odometry]\n",
			"doctor --report":               "system DDS healthy\n",
			"param get /robot use_sim_time": "Boolean value is: False\n",
			"topic echo /odom":              "frame_id: odom\n---\n",
			"topic hz /odom":                "average rate: 10.000\n\tmin: 0.100s max: 0.100s std dev: 0.00000s window: 10\n",
			"pkg list":                      "unitree_ros2\n",
		},
	}}
}

func TestROS2Service_SystemFallback(t *testing.T) {
	ctx := context.Background()
	commands := []struct {
		name string
		run  func(*testing.T, *ROS2Service, *int32)
	}{
		{"nodes", func(t *testing.T, svc *ROS2Service, domain *int32) {
			resp, err := svc.ListNodes(ctx, &agentpbv2.ListROS2NodesRequest{DomainId: domain})
			if err != nil || len(resp.GetNodes()) != 1 || resp.GetNodes()[0].GetName() != "robot" || resp.GetNodes()[0].GetRmw() != ros2inspection.FastRTPSRMW {
				t.Fatalf("system nodes: %v, %v", resp, err)
			}
		}},
		{"topics", func(t *testing.T, svc *ROS2Service, domain *int32) {
			resp, err := svc.ListTopics(ctx, &agentpbv2.ListROS2TopicsRequest{DomainId: domain})
			if err != nil || len(resp.GetTopics()) != 1 || resp.GetTopics()[0].GetName() != "/odom" || resp.GetTopics()[0].GetRmw() != ros2inspection.FastRTPSRMW {
				t.Fatalf("system topics: %v, %v", resp, err)
			}
		}},
		{"doctor", func(t *testing.T, svc *ROS2Service, domain *int32) {
			resp, err := svc.Doctor(ctx, &agentpbv2.ROS2DoctorRequest{DomainId: domain})
			if err != nil || !strings.Contains(resp.GetReport(), "system DDS healthy") {
				t.Fatalf("system doctor: %v, %v", resp, err)
			}
		}},
		{"targeted node", func(t *testing.T, svc *ROS2Service, domain *int32) {
			resp, err := svc.GetParam(ctx, &agentpbv2.GetROS2ParamRequest{DomainId: domain, Node: "/robot", Param: "use_sim_time"})
			if err != nil || resp.GetValue() != "Boolean value is: False" {
				t.Fatalf("system parameter: %v, %v", resp, err)
			}
		}},
		{"echo", func(t *testing.T, svc *ROS2Service, domain *int32) {
			stream := &fakeServerStream[agentpbv2.ROS2Message]{ctx: ctx}
			err := svc.EchoTopic(&agentpbv2.EchoROS2TopicRequest{DomainId: domain, Topic: "/odom"}, stream)
			if err != nil || len(stream.sent) != 1 {
				t.Fatalf("system echo sent %d messages: %v", len(stream.sent), err)
			}
		}},
		{"hz", func(t *testing.T, svc *ROS2Service, domain *int32) {
			stream := &fakeServerStream[agentpbv2.ROS2HzSample]{ctx: ctx}
			err := svc.MonitorHz(&agentpbv2.MonitorROS2HzRequest{DomainId: domain, Topic: "/odom"}, stream)
			if err != nil || len(stream.sent) != 1 {
				t.Fatalf("system hz sent %d samples: %v", len(stream.sent), err)
			}
		}},
		{"exec", func(t *testing.T, svc *ROS2Service, domain *int32) {
			stream := &fakeServerStream[agentpbv2.ROS2ExecOutput]{ctx: ctx}
			err := svc.Exec(&agentpbv2.ROS2ExecRequest{DomainId: domain, Args: []string{"pkg", "list"}}, stream)
			if err != nil || len(stream.sent) == 0 {
				t.Fatalf("system exec sent %d outputs: %v", len(stream.sent), err)
			}
		}},
	}
	for _, domain := range []*int32{nil, func() *int32 { d := int32(42); return &d }()} {
		wantDomain := 0
		if domain != nil {
			wantDomain = int(*domain)
		}
		for _, command := range commands {
			t.Run(fmt.Sprintf("%s/domain_%d", command.name, wantDomain), func(t *testing.T) {
				runtime := newSystemROS2Runtime()
				command.run(t, newTestROS2Service(t, runtime, t.TempDir()), domain)
				if runtime.appCalls != 1 || runtime.systemCalls != 1 {
					t.Fatalf("provisioning: app=%d system=%d", runtime.appCalls, runtime.systemCalls)
				}
				if len(runtime.calls) != 1 || runtime.calls[0].SidecarName != "system" || runtime.calls[0].DomainID != wantDomain {
					t.Fatalf("commands should execute directly in system sidecar, domain %d: %+v", wantDomain, runtime.calls)
				}
			})
		}
	}
}

func TestROS2Service_SystemFallbackPreservesAppScope(t *testing.T) {
	for _, scope := range []string{"", ros2inspection.AppScope} {
		t.Run("scope_"+scope, func(t *testing.T) {
			runtime := newSystemROS2Runtime()
			runtime.ensureErr = nil
			runtime.sidecar = ROS2Sidecar{Name: "app", DomainID: 7}
			svc := newTestROS2Service(t, runtime, t.TempDir())
			ctx := context.Background()
			if scope != "" {
				ctx = metadata.NewIncomingContext(ctx, metadata.Pairs(ros2inspection.ScopeMetadata, scope))
			}
			if _, err := svc.ListNodes(ctx, &agentpbv2.ListROS2NodesRequest{}); err != nil {
				t.Fatal(err)
			}
			if runtime.systemCalls != 0 || len(runtime.calls) != 1 || runtime.calls[0].SidecarName != "app" || runtime.calls[0].DomainID != 7 {
				t.Fatalf("app graph was replaced by system fallback: %+v", runtime.calls)
			}
		})
	}
}

func TestROS2Service_SystemFallbackErrorBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name        string
		scope       string
		appErr      error
		systemErr   error
		systemCalls int
	}{
		{name: "explicit app", scope: ros2inspection.AppScope, appErr: ErrNoRunningROS2Containers},
		{name: "discovery failure", appErr: errors.New("containerd unavailable")},
		{name: "sidecar failure", appErr: errors.New("pulling ROS 2 sidecar image failed")},
		{name: "system failure", appErr: ErrNoRunningROS2Containers, systemErr: errors.New("system ROS 2 unavailable"), systemCalls: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runtime := newSystemROS2Runtime()
			runtime.ensureErr = tc.appErr
			runtime.systemErr = tc.systemErr
			svc := newTestROS2Service(t, runtime, t.TempDir())
			ctx := context.Background()
			if tc.scope != "" {
				ctx = metadata.NewIncomingContext(ctx, metadata.Pairs(ros2inspection.ScopeMetadata, tc.scope))
			}
			_, err := svc.ListTopics(ctx, &agentpbv2.ListROS2TopicsRequest{})
			wantErr := tc.appErr
			if tc.systemErr != nil {
				wantErr = tc.systemErr
			}
			if status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), wantErr.Error()) {
				t.Fatalf("error = %v, want FailedPrecondition containing %q", err, wantErr)
			}
			if runtime.appCalls != 1 || runtime.systemCalls != tc.systemCalls || len(runtime.calls) != 0 {
				t.Fatalf("unexpected work after failure: app=%d system=%d exec=%d", runtime.appCalls, runtime.systemCalls, len(runtime.calls))
			}
		})
	}
}

func TestROS2Service_SystemFallbackValidatesDomainBeforeProvisioning(t *testing.T) {
	for _, domain := range []int32{-1, 233} {
		t.Run(fmt.Sprint(domain), func(t *testing.T) {
			runtime := newSystemROS2Runtime()
			svc := newTestROS2Service(t, runtime, t.TempDir())
			_, err := svc.ListNodes(context.Background(), &agentpbv2.ListROS2NodesRequest{DomainId: &domain})
			if status.Code(err) != codes.InvalidArgument || runtime.appCalls != 0 || runtime.systemCalls != 0 {
				t.Fatalf("invalid domain reached runtime: %v (app=%d system=%d)", err, runtime.appCalls, runtime.systemCalls)
			}
		})
	}
}

type namedSystemROS2Runtime struct {
	*systemROS2Runtime
	verifiedName string
	namedErr     error
}

func (r *namedSystemROS2Runtime) VerifyROS2SidecarNamed(_ context.Context, name string) error {
	r.verifiedName = name
	return r.namedErr
}

func TestROS2Service_RecordBagVerifiesSelectedSidecar(t *testing.T) {
	for _, tc := range []struct {
		name       string
		globalErr  error
		namedErr   error
		wantDetail string
	}{
		{name: "system", globalErr: errors.New("unrelated app stopped"), wantDetail: "recorder exited unexpectedly"},
		{name: "app", namedErr: errors.New("recording app anchor stopped"), wantDetail: "stopped or redeployed while recording"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runtime := &namedSystemROS2Runtime{systemROS2Runtime: newSystemROS2Runtime(), namedErr: tc.namedErr}
			runtime.verifyErr = tc.globalErr
			if tc.name == "app" {
				runtime.ensureErr = nil
				runtime.sidecar = ROS2Sidecar{Name: "app"}
			}
			runtime.execFn = func(_ context.Context, _ ROS2ExecOptions, _, stderr io.Writer) (int, error) {
				_, _ = io.WriteString(stderr, "storage full\n")
				return 1, nil
			}
			svc := newTestROS2Service(t, runtime, t.TempDir())
			stream := &fakeBidiStream[agentpbv2.RecordROS2BagRequest, agentpbv2.RecordROS2BagResponse]{
				ctx:  context.Background(),
				recv: make(chan *agentpbv2.RecordROS2BagRequest, 1),
			}
			defer close(stream.recv)
			stream.recv <- &agentpbv2.RecordROS2BagRequest{
				Command: &agentpbv2.RecordROS2BagRequest_Start{
					Start: &agentpbv2.RecordROS2BagRequest_RecordStart{OutputName: "test-bag"},
				},
			}
			if err := svc.RecordBag(stream); err != nil {
				t.Fatal(err)
			}
			if runtime.verifiedName != tc.name {
				t.Fatalf("verified sidecar %q, want selected %q", runtime.verifiedName, tc.name)
			}
			if len(stream.sent) != 2 || stream.sent[1].GetState() != agentpbv2.RecordROS2BagResponse_STATE_ERROR {
				t.Fatalf("expected recording then error responses: %v", stream.sent)
			}
			if msg := stream.sent[1].GetMessage(); !strings.Contains(msg, tc.wantDetail) || !strings.Contains(msg, "storage full") {
				t.Fatalf("incorrect recorder exit diagnosis: %s", msg)
			}
		})
	}
}
