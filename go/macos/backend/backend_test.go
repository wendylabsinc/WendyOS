package backend

import (
	"context"
	logscol "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	common "go.opentelemetry.io/proto/otlp/common/v1"
	logsdata "go.opentelemetry.io/proto/otlp/logs/v1"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

type fakeAgent struct {
	pb.UnimplementedWendyContainerServiceServer
	pb.UnimplementedWendyVideoServiceServer
	pb.UnimplementedWendyTelemetryServiceServer
	controls      chan string
	logsCancelled chan struct{}
	cancelled     chan struct{}
}

func (f *fakeAgent) StartContainer(req *pb.StartContainerRequest, stream grpc.ServerStreamingServer[pb.RunContainerLayersResponse]) error {
	if req.GetRestartPolicy().GetMode() != pb.RestartPolicyMode_UNLESS_STOPPED {
		return status.Error(codes.InvalidArgument, "missing restart policy")
	}
	f.controls <- "start:" + req.GetAppName()
	return stream.Send(&pb.RunContainerLayersResponse{ResponseType: &pb.RunContainerLayersResponse_Started_{Started: &pb.RunContainerLayersResponse_Started{}}})
}
func (f *fakeAgent) StopContainer(_ context.Context, req *pb.StopContainerRequest) (*pb.StopContainerResponse, error) {
	f.controls <- "stop:" + req.GetAppName()
	return &pb.StopContainerResponse{}, nil
}
func (f *fakeAgent) DeleteContainer(_ context.Context, req *pb.DeleteContainerRequest) (*pb.DeleteContainerResponse, error) {
	if req.GetDeleteImage() || req.GetDeleteVolumes() {
		return nil, status.Error(codes.InvalidArgument, "unexpected data deletion")
	}
	if req.GetAppName() == "denied" {
		return nil, status.Error(codes.PermissionDenied, "deletion denied")
	}
	f.controls <- "delete:" + req.GetAppName()
	return &pb.DeleteContainerResponse{}, nil
}
func (f *fakeAgent) ListContainers(_ *pb.ListContainersRequest, stream grpc.ServerStreamingServer[pb.ListContainersResponse]) error {
	return stream.Send(&pb.ListContainersResponse{Container: &pb.AppContainer{AppName: "test-app", RunningState: pb.AppRunningState_RUNNING}})
}
func (f *fakeAgent) ListVideoDevices(context.Context, *pb.ListVideoDevicesRequest) (*pb.ListVideoDevicesResponse, error) {
	return &pb.ListVideoDevicesResponse{Devices: []*pb.VideoDevice{{Id: 0, Name: "Camera zero"}}}, nil
}
func (f *fakeAgent) StreamVideo(req *pb.StreamVideoRequest, stream grpc.ServerStreamingServer[pb.VideoFrame]) error {
	// ESP cameras may offer only their native resolution. A generic viewer
	// must leave dimensions and rate unset unless the user chooses a mode.
	if req.GetWidth() != 0 || req.GetHeight() != 0 || req.GetFramerate() != 0 {
		return status.Error(codes.InvalidArgument, "resolution not advertised by this camera")
	}
	if req.GetDeviceId() != 99 {
		if err := stream.Send(&pb.VideoFrame{Data: []byte{0, 0, 1, 0x67, 1}, Codec: pb.VideoCodec_VIDEO_CODEC_H264}); err != nil {
			return err
		}
	}
	<-stream.Context().Done()
	f.cancelled <- struct{}{}
	return stream.Context().Err()
}
func TestAgentRPCsAndStreamCancellation(t *testing.T) {
	listener := bufconn.Listen(1 << 20)
	server := grpc.NewServer()
	fake := &fakeAgent{controls: make(chan string, 2), cancelled: make(chan struct{}, 2), logsCancelled: make(chan struct{})}
	pb.RegisterWendyContainerServiceServer(server, fake)
	pb.RegisterWendyVideoServiceServer(server, fake)
	pb.RegisterWendyTelemetryServiceServer(server, fake)
	go server.Serve(listener)
	defer server.Stop()
	defer listener.Close()
	conn, err := grpc.NewClient("passthrough:///fixture", grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return listener.DialContext(ctx) }))
	if err != nil {
		t.Fatal(err)
	}
	b := New()
	b.conn = grpcclient.NewFromConn(conn)
	b.life, b.stop = context.WithCancel(context.Background())
	b.session = 3
	defer b.Shutdown()
	if err := b.StartApp("test-app", 2); err == nil {
		t.Fatal("stale session accepted")
	}
	if err := b.StartApp("test-app", 3); err != nil {
		t.Fatal(err)
	}
	if got := <-fake.controls; got != "start:test-app" {
		t.Fatal(got)
	}
	if err := b.StopApp("test-app", 3); err != nil {
		t.Fatal(err)
	}
	if got := <-fake.controls; got != "stop:test-app" {
		t.Fatal(got)
	}

	for _, session := range []uint64{0, 2} {
		if err := b.DeleteApp("test-app", session); err == nil {
			t.Fatal("delete accepted missing or stale device session")
		}
	}
	if err := b.DeleteApp(" ", 3); err == nil {
		t.Fatal("delete accepted missing app")
	}
	if len(fake.controls) != 0 {
		t.Fatal("invalid deletion reached agent")
	}
	if err := b.DeleteApp("test-app", 3); err != nil {
		t.Fatal(err)
	}
	if got := <-fake.controls; got != "delete:test-app" {
		t.Fatal(got)
	}
	if err := b.DeleteApp("denied", 3); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("lost delete error: %v", err)
	}

	oldConnection := b.conn
	dialStarted := make(chan struct{})
	b.connectDevice = func(ctx context.Context, _ string) (*grpcclient.AgentConnection, error) {
		close(dialStarted)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	request := b.ReserveConnection()
	dialDone := make(chan error, 1)
	go func() { _, err := b.ConnectRequest("slow.local", false, request); dialDone <- err }()
	<-dialStarted
	readDone := make(chan error, 1)
	go func() { _, err := b.Apps(); readDone <- err }()
	select {
	case err := <-readDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("dial blocked the active device")
	}
	b.ReserveConnection()
	select {
	case err := <-dialDone:
		if err != context.Canceled {
			t.Fatalf("cancel = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("superseded dial did not cancel")
	}
	if b.conn != oldConnection || b.session != 3 {
		t.Fatal("cancel discarded active connection")
	}
	if _, err := b.ConnectRequest("stale.local", false, request); err != context.Canceled {
		t.Fatalf("stale queued attempt: %v", err)
	}
	apps, err := b.Apps()
	if err != nil {
		t.Fatal(err)
	}
	if len(apps) != 1 || apps[0].AppName != "test-app" {
		t.Fatal(apps)
	}
	cameras, err := b.Cameras()
	if err != nil {
		t.Fatal(err)
	}
	if len(cameras) != 1 || cameras[0].Id != 0 {
		t.Fatal("missing camera zero")
	}
	if err = b.StartLogs("test-app", 16); err != nil {
		t.Fatal(err)
	}
	select {
	case event := <-b.events:
		if event.Kind != EventLog || event.Stream != 16 || event.Log == nil || !strings.Contains(event.Log.Body, "fixture log") {
			t.Fatalf("wrong log event: %+v", event)
		}
	case <-time.After(time.Second):
		t.Fatal("no log")
	}
	if err = b.StartCamera(0, 17, 3); err != nil {
		t.Fatal(err)
	}
	select {
	case event := <-b.events:
		if event.Kind != EventVideo || event.Session != 3 || event.Stream != 17 {
			t.Fatalf("wrong stream envelope: %+v", event)
		}
	case <-time.After(time.Second):
		t.Fatal("no frame")
	}
	b.cameraFirstFrameTimeout = 50 * time.Millisecond
	if err = b.StartCamera(99, 18, 3); err != nil {
		t.Fatal(err)
	}
	select {
	case event := <-b.events:
		if event.Kind != EventCameraError || event.Stream != 18 {
			t.Fatalf("wrong timeout event: %+v", event)
		}
	case <-time.After(time.Second):
		t.Fatal("silent camera never timed out")
	}
	if err = b.Disconnect(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-fake.cancelled:
	case <-time.After(time.Second):
		t.Fatal("disconnect leaked stream")
	}
	select {
	case <-fake.logsCancelled:
	case <-time.After(time.Second):
		t.Fatal("disconnect leaked log stream")
	}
	if _, err = b.Apps(); err == nil {
		t.Fatal("read after disconnect succeeded")
	}
}
func TestBackpressureCancellation(t *testing.T) {
	b := New()
	ctx, cancel := context.WithCancel(context.Background())
	for i := 0; i < cap(b.events); i++ {
		b.emit(ctx, Event{Kind: EventVideo, Session: 1, Stream: 2, Bytes: []byte{1}})
	}
	done := make(chan struct{})
	go func() { b.emit(ctx, Event{Kind: EventVideo, Session: 1, Stream: 2, Bytes: []byte{2}}); close(done) }()
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("full frame queue prevented cancellation")
	}
}

func (f *fakeAgent) StreamLogs(req *pb.StreamLogsRequest, stream grpc.ServerStreamingServer[pb.StreamLogsResponse]) error {
	if req.GetAppName() != "test-app" || req.GetLastN() != 200 {
		return io.ErrUnexpectedEOF
	}
	record := &logsdata.LogRecord{SeverityText: "INFO", Body: &common.AnyValue{Value: &common.AnyValue_StringValue{StringValue: "fixture log"}}}
	response := &pb.StreamLogsResponse{Logs: &logscol.ExportLogsServiceRequest{ResourceLogs: []*logsdata.ResourceLogs{{ScopeLogs: []*logsdata.ScopeLogs{{LogRecords: []*logsdata.LogRecord{record}}}}}}}
	if err := stream.Send(response); err != nil {
		return err
	}
	<-stream.Context().Done()
	close(f.logsCancelled)
	return stream.Context().Err()
}

func TestUnpinRetryIsBoundToFailedSelection(t *testing.T) {
	b := New()
	b.pinSelector, b.pinTarget = "ci-iq8.local:50052", "ci-iq8.local"
	var cleared string
	b.unpinDevice = func(target string) error { cleared = target; return nil }
	dials := 0
	b.connectDevice = func(_ context.Context, selector string) (*grpcclient.AgentConnection, error) {
		dials++
		if selector != "ci-iq8.local:50052" {
			t.Fatal(selector)
		}
		return nil, status.Error(codes.Unavailable, "fixture offline")
	}
	if _, err := b.Connect("other.local", true); err == nil {
		t.Fatal("accepted unrelated target")
	}
	if cleared != "" || dials != 0 {
		t.Fatal("unrelated request changed trust")
	}
	if _, err := b.Connect("ci-iq8.local:50052", true); err == nil {
		t.Fatal("expected normal dial error")
	}
	if cleared != "ci-iq8.local" || dials != 1 {
		t.Fatal(cleared, dials)
	}
	if b.pinTarget != "" {
		t.Fatal("recovery survived retry")
	}
	if _, err := b.Connect("ci-iq8.local:50052", true); err == nil {
		t.Fatal("accepted repeated trust reset")
	}
	if dials != 1 {
		t.Fatal("unexpected second dial")
	}
}

func TestNormalConnectClearsOldRecovery(t *testing.T) {
	b := New()
	b.pinSelector, b.pinTarget = "old.local", "old.local"
	b.connectDevice = func(context.Context, string) (*grpcclient.AgentConnection, error) {
		return nil, status.Error(codes.Unavailable, "fixture offline")
	}
	b.Connect("new.local", false)
	if b.pinTarget != "" || b.pinSelector != "" {
		t.Fatal("old recovery retained")
	}
}

type wallAgent struct {
	pb.UnimplementedWendyVideoServiceServer
	started chan uint32
	stopped chan uint32
}

func (f *wallAgent) StreamVideo(req *pb.StreamVideoRequest, stream grpc.ServerStreamingServer[pb.VideoFrame]) error {
	f.started <- req.DeviceId
	defer func() { f.stopped <- req.DeviceId }()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-stream.Context().Done():
			return stream.Context().Err()
		case <-ticker.C:
			if err := stream.Send(&pb.VideoFrame{Data: []byte{byte(req.DeviceId)}, Codec: pb.VideoCodec_VIDEO_CODEC_H264}); err != nil {
				return err
			}
		}
	}
}

func TestCameraWallIndependentStreams(t *testing.T) {
	listener := bufconn.Listen(1 << 20)
	server := grpc.NewServer()
	fake := &wallAgent{started: make(chan uint32, 8), stopped: make(chan uint32, 8)}
	pb.RegisterWendyVideoServiceServer(server, fake)
	go server.Serve(listener)
	defer server.Stop()
	defer listener.Close()
	conn, err := grpc.NewClient("passthrough:///wall", grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return listener.DialContext(ctx) }))
	if err != nil {
		t.Fatal(err)
	}
	b := New()
	b.conn = grpcclient.NewFromConn(conn)
	b.life, b.stop = context.WithCancel(context.Background())
	b.session = 7
	defer b.Shutdown()
	call := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	waitID := func(ch <-chan uint32, want uint32) {
		t.Helper()
		select {
		case got := <-ch:
			if got != want {
				t.Fatalf("camera %d, want %d", got, want)
			}
		case <-time.After(time.Second):
			t.Fatalf("camera %d did not respond", want)
		}
	}
	call(b.StartCamera(96, 1, 7))
	waitID(fake.started, 96)
	call(b.StartCamera(97, 2, 7))
	waitID(fake.started, 97)
	seen := map[uint64]bool{}
	deadline := time.After(time.Second)
	for len(seen) < 2 {
		select {
		case e := <-b.events:
			seen[e.Stream] = true
		case <-deadline:
			t.Fatal("did not receive both cameras")
		}
	}
	call(b.StopCamera(96, 1, 7))
	waitID(fake.stopped, 96)
	select {
	case id := <-fake.stopped:
		t.Fatalf("stopping 96 also stopped %d", id)
	case <-time.After(30 * time.Millisecond):
	}
	call(b.StartCamera(96, 3, 7))
	waitID(fake.started, 96)
	call(b.StopCamera(96, 1, 7)) // Late stop from the previous tile generation.
	if err := b.StopCamera(97, 2, 6); err == nil {
		t.Fatal("accepted stale device session")
	}
	select {
	case id := <-fake.stopped:
		t.Fatalf("stale stop cancelled %d", id)
	case <-time.After(30 * time.Millisecond):
	}
	call(b.StopCameras(7))
	stopped := map[uint32]bool{}
	for len(stopped) < 2 {
		select {
		case id := <-fake.stopped:
			stopped[id] = true
		case <-time.After(time.Second):
			t.Fatal("pause all leaked a stream")
		}
	}
	if !stopped[96] || !stopped[97] {
		t.Fatal(stopped)
	}
}

func TestStructuredLogPreservesMetadataAndMultilineBody(t *testing.T) {
	record := &logsdata.LogRecord{
		TimeUnixNano: 1234567890123456789, SeverityNumber: logsdata.SeverityNumber_SEVERITY_NUMBER_ERROR2,
		Body:       &common.AnyValue{Value: &common.AnyValue_StringValue{StringValue: "error\n  stack frame"}},
		Attributes: []*common.KeyValue{{Key: "service_name", Value: &common.AnyValue{Value: &common.AnyValue_StringValue{StringValue: "api"}}}},
	}
	result := desktopLog(record, []*common.KeyValue{{Key: "service.namespace", Value: &common.AnyValue{Value: &common.AnyValue_StringValue{StringValue: "fixture"}}}})
	if result.Timestamp != record.TimeUnixNano || result.Level != LogError || result.Body != "error\n  stack frame" || result.App != "fixture" || result.Service != "api" {
		t.Fatalf("lost structured log fields: %+v", result)
	}
}
