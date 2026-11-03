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

type legacyPullContract struct {
	cloudpb.UnimplementedDeploymentServiceServer
	denied bool
}

func (s legacyPullContract) GetPullCredentials(ctx context.Context, req *cloudpb.GetPullCredentialsRequest) (*cloudpb.GetPullCredentialsResponse, error) {
	md, _ := metadata.FromIncomingContext(ctx)
	if req.GetAppReleaseId() != 2 || len(md.Get("test-method")) != 1 || md.Get("test-method")[0] != "/wendycloud.v1.DeploymentService/GetPullCredentials" {
		return nil, fmt.Errorf("wrong pull request or proof")
	}
	if s.denied {
		return nil, fmt.Errorf("device credentials refused")
	}
	return &cloudpb.GetPullCredentialsResponse{RegistryUrl: "registry.test", FullArtifactPath: "registry.test/org-64/app", AccessToken: "test-secret"}, nil
}

type legacyInstallRecorder struct {
	app     string
	release int32
	digest  string
	calls   int
}

func (i *legacyInstallRecorder) InstallLegacyRelease(_ context.Context, app string, release int32, digest string, credentials *cloudpb.GetPullCredentialsResponse) error {
	if credentials.GetAccessToken() != "test-secret" {
		return fmt.Errorf("lost registry credentials")
	}
	i.calls++
	i.app, i.release, i.digest = app, release, digest
	return nil
}
func TestLegacyAssignedReleaseInstallsBeforeStartAndKeepsOldAppOnRefusal(t *testing.T) {
	for _, denied := range []bool{false, true} {
		t.Run(fmt.Sprint(denied), func(t *testing.T) {
			listener, err := net.Listen("tcp4", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			server := grpc.NewServer()
			cloudpb.RegisterDeploymentServiceServer(server, legacyPullContract{denied: denied})
			go func() { _ = server.Serve(listener) }()
			defer server.Stop()
			conn, err := grpc.NewClient(listener.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			installer := &legacyInstallRecorder{}
			control := &legacyControlRecorder{}
			reporter := &LegacyReportedState{apps: legacyReportApps{{AppID: "app", ReleaseID: "1", Running: true}}, control: control, install: installer}
			response := &cloudpb.DesiredStateResponse{DesiredAppStates: []*cloudpb.DesiredAppState{{AppId: "app", AppReleaseId: proto.Int32(2), DesiredState: cloudpb.Running_RUNNING, ImageDigest: proto.String("sha256:test")}}}
			proof := func(method string) (metadata.MD, error) { return metadata.Pairs("test-method", method), nil }
			err = reporter.apply(context.Background(), conn, proof, response)
			if denied {
				if err == nil || installer.calls != 0 || len(control.actions) != 0 {
					t.Fatalf("refused pull touched old app: %v %d %v", err, installer.calls, control.actions)
				}
				return
			}
			if err != nil || installer.calls != 1 || installer.app != "app" || installer.release != 2 || installer.digest != "sha256:test" || !reflect.DeepEqual(control.actions, []string{"start:app"}) {
				t.Fatalf("install/start: %v %+v %v", err, installer, control.actions)
			}
		})
	}
}

type cancelledLegacyInstall struct{ started chan struct{} }

func (i cancelledLegacyInstall) InstallLegacyRelease(ctx context.Context, _ string, _ int32, _ string, _ *cloudpb.GetPullCredentialsResponse) error {
	close(i.started)
	<-ctx.Done()
	return ctx.Err()
}

type closingLegacyReport struct {
	legacyPullContract
	started <-chan struct{}
}

func (s closingLegacyReport) HandleReportedState(stream cloudpb.DeploymentService_HandleReportedStateServer) error {
	if _, err := stream.Recv(); err != nil {
		return err
	}
	if err := stream.Send(&cloudpb.DesiredStateResponse{DesiredAppStates: []*cloudpb.DesiredAppState{{AppId: "app", AppReleaseId: proto.Int32(2), DesiredState: cloudpb.Running_RUNNING, ImageDigest: proto.String("sha256:test")}}}); err != nil {
		return err
	}
	select {
	case <-s.started:
		return fmt.Errorf("report connection lost during pull")
	case <-stream.Context().Done():
		return stream.Context().Err()
	}
}
func TestLegacyStreamFailureCancelsAnInFlightReleaseDownload(t *testing.T) {
	started := make(chan struct{})
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer()
	cloudpb.RegisterDeploymentServiceServer(server, closingLegacyReport{started: started})
	go func() { _ = server.Serve(listener) }()
	defer server.Stop()
	conn, err := grpc.NewClient(listener.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	provisioning := &ProvisioningService{enrolled: true, cloudHost: "legacy:443", orgID: 64, assetID: 674, certPEM: "leaf", keyPEM: []byte("key")}
	r := NewLegacyReportedState(zap.NewNop(), provisioning, legacyReportApps{{AppID: "app", ReleaseID: "1", Running: true}}, nil, cancelledLegacyInstall{started: started})
	r.dial = func(CloudEnrollment, string, string, string) (grpc.ClientConnInterface, io.Closer, brokerRequestMetadata, error) {
		return conn, conn, func(method string) (metadata.MD, error) { return metadata.Pairs("test-method", method), nil }, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- r.session(ctx) }()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("release download never started")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("dead reporting stream retained an active release download")
	}
}
