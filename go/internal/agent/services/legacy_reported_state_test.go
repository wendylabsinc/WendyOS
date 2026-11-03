package services

import (
	"context"
	"fmt"
	"io"
	"net"
	"reflect"
	"testing"
	"time"

	cloudpb "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/proto"
)

type legacyReportApps []AppReport

func (a legacyReportApps) ReportApps(context.Context) ([]AppReport, error) { return a, nil }

type legacyControlRecorder struct{ actions []string }

func (c *legacyControlRecorder) StartApp(_ context.Context, id string) error {
	c.actions = append(c.actions, "start:"+id)
	return nil
}
func (c *legacyControlRecorder) StopApp(_ context.Context, id string) error {
	c.actions = append(c.actions, "stop:"+id)
	return nil
}
func (c *legacyControlRecorder) RemoveApp(_ context.Context, id string) error {
	c.actions = append(c.actions, "remove:"+id)
	return nil
}

func TestLegacyDesiredStateOnlyAppliesExplicitRows(t *testing.T) {
	control := &legacyControlRecorder{}
	r := &LegacyReportedState{apps: legacyReportApps{{AppID: "local", Running: true}, {AppID: "stop", Running: true}, {AppID: "start"}, {AppID: "remove"}}, control: control}
	response := &cloudpb.DesiredStateResponse{DesiredAppStates: []*cloudpb.DesiredAppState{
		{AppId: "stop", DesiredState: cloudpb.Running_STOPPED}, {AppId: "start", DesiredState: cloudpb.Running_RUNNING}, {AppId: "remove", DesiredState: cloudpb.Running_ABSENT},
	}}
	if err := r.apply(context.Background(), nil, nil, response); err != nil {
		t.Fatal(err)
	}
	if want := []string{"stop:stop", "start:start", "remove:remove"}; !reflect.DeepEqual(control.actions, want) {
		t.Fatalf("actions=%v", control.actions)
	}
	control.actions = nil
	if err := r.apply(context.Background(), nil, nil, &cloudpb.DesiredStateResponse{}); err != nil {
		t.Fatal(err)
	}
	if len(control.actions) != 0 {
		t.Fatal("empty response touched local apps")
	}
}

type v1ReportContract struct {
	cloudpb.UnimplementedDeploymentServiceServer
	reports chan *cloudpb.UpdateReportedStateRequest
}

func (s *v1ReportContract) HandleReportedState(stream cloudpb.DeploymentService_HandleReportedStateServer) error {
	req, err := stream.Recv()
	if err != nil {
		return err
	}
	select {
	case s.reports <- req:
	case <-stream.Context().Done():
		return stream.Context().Err()
	}
	if err := stream.Send(&cloudpb.DesiredStateResponse{}); err != nil {
		return err
	}
	<-stream.Context().Done()
	return stream.Context().Err()
}
func TestLegacyReportedStateUsesV1AndStopsOnIdentityChange(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer(grpc.StreamInterceptor(func(srv any, stream grpc.ServerStream, info *grpc.StreamServerInfo, next grpc.StreamHandler) error {
		if info.FullMethod != "/wendycloud.v1.DeploymentService/HandleReportedState" {
			return fmt.Errorf("wrong protocol %s", info.FullMethod)
		}
		md, _ := metadata.FromIncomingContext(stream.Context())
		if got := md.Get("test-method"); len(got) != 1 || got[0] != info.FullMethod {
			return fmt.Errorf("proof not bound to v1 method")
		}
		return next(srv, stream)
	}))
	contract := &v1ReportContract{reports: make(chan *cloudpb.UpdateReportedStateRequest, 1)}
	cloudpb.RegisterDeploymentServiceServer(server, contract)
	go func() { _ = server.Serve(listener) }()
	defer server.Stop()
	conn, err := grpc.NewClient(listener.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	provisioning := &ProvisioningService{enrolled: true, cloudHost: "legacy:443", orgID: 64, assetID: 674, certPEM: "leaf", keyPEM: []byte("key")}
	r := NewLegacyReportedState(zap.NewNop(), provisioning, legacyReportApps{{AppID: "numeric", ReleaseID: "123", Running: true}, {AppID: "uuid", ReleaseID: "0b6f2c1e-8a3d-4c5e-9f00-1234567890ab"}}, nil, nil)
	r.dial = func(CloudEnrollment, string, string, string) (grpc.ClientConnInterface, io.Closer, brokerRequestMetadata, error) {
		return conn, conn, func(method string) (metadata.MD, error) { return metadata.Pairs("test-method", method), nil }, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- r.session(ctx) }()
	select {
	case report := <-contract.reports:
		if len(report.CurrentStates) != 2 || report.CurrentStates[0].AppReleaseId != nil || report.CurrentStates[1].AppReleaseId != nil {
			t.Fatalf("report=%v", report)
		}
	case <-ctx.Done():
		t.Fatal("no v1 report")
	}
	r.UpdateIdentity()
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("old enrollment session survived identity change")
	}
}
func TestLegacySnapshotRejectsMixedReleaseIDs(t *testing.T) {
	got := legacyAppSnapshot([]AppReport{{AppID: "numeric", ReleaseID: "12"}, {AppID: "leading-zero", ReleaseID: "012"}, {AppID: "zero", ReleaseID: "0"}, {AppID: "negative", ReleaseID: "-1"}, {AppID: "overflow", ReleaseID: "2147483648"}})
	want := &cloudpb.UpdateReportedStateRequest{CurrentStates: []*cloudpb.CurrentState{{AppId: "numeric", AppReleaseId: proto.Int32(12), ReportedState: cloudpb.Running_STOPPED}, {AppId: "leading-zero", ReportedState: cloudpb.Running_STOPPED}, {AppId: "zero", ReportedState: cloudpb.Running_STOPPED}, {AppId: "negative", ReportedState: cloudpb.Running_STOPPED}, {AppId: "overflow", ReportedState: cloudpb.Running_STOPPED}}}
	if !proto.Equal(got, want) {
		t.Fatalf("snapshot=%v", got)
	}
}

func TestLegacyReportCannotOverwriteANewerDesiredRelease(t *testing.T) {
	report := legacyAppSnapshot([]AppReport{{AppID: "older", ReleaseID: "1", RestartCount: 3}, {AppID: "matching", ReleaseID: "2"}, {AppID: "local", ReleaseID: "4"}})
	protectLegacyDesiredReleases(report, map[string]int32{"older": 3, "matching": 2})
	if report.CurrentStates[0].AppReleaseId != nil || report.CurrentStates[1].GetAppReleaseId() != 2 || report.CurrentStates[2].AppReleaseId != nil {
		t.Fatalf("report clobbers desired release: %v", report)
	}
	if report.CurrentStates[0].ReportedRestartCount != 3 {
		t.Fatal("restart count lost")
	}
}
