package services

import (
	"bytes"
	"context"
	"net"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/unitree"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

func unitreeVideoFixture(t *testing.T, caller context.Context) (agentpb.WendyVideoServiceClient, *VideoService, *UnitreeService, *teleopFixtureLink) {
	t.Helper()
	pairing := testUnitreeService(t)
	link := &teleopFixtureLink{testUnitreeLink: newTestUnitreeLink(), messages: make(chan unitree.TopicMessage, 4), video: make(chan unitree.VideoFrame, 4)}
	pairing.dial = func(context.Context, string, string, string) (unitreeLink, error) { return link, nil }
	if _, err := pairing.PairUnitree(unitreeTestContext(), unitreeTestRequest()); err != nil {
		t.Fatal(err)
	}
	video := newTestVideoService(func() ([]string, error) { return []string{"/dev/video0"}, nil }, func(string) (string, error) { return "USB camera", nil })
	video.SetUnitreeCameras(NewUnitreeRobotService(pairing))
	t.Cleanup(video.Shutdown)
	listener := bufconn.Listen(1 << 20)
	server := grpc.NewServer(
		grpc.UnaryInterceptor(func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, h grpc.UnaryHandler) (any, error) {
			return h(caller, req)
		}),
		grpc.StreamInterceptor(func(srv any, ss grpc.ServerStream, _ *grpc.StreamServerInfo, h grpc.StreamHandler) error {
			// Retain request cancellation while supplying the authenticated peer.
			return h(srv, peerStream{ss, cameraPeerContext(ss.Context(), caller)})
		}),
	)
	agentpb.RegisterWendyVideoServiceServer(server, video)
	go server.Serve(listener)
	conn, err := grpc.NewClient("passthrough:///camera-test", grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close(); server.Stop(); listener.Close() })
	return agentpb.NewWendyVideoServiceClient(conn), video, pairing, link
}

func TestUnitreeCameraListUsesPairingAndRetainsIdentity(t *testing.T) {
	client, video, _, link := unitreeVideoFixture(t, unitreeTestContext())
	listed, err := client.ListVideoDevices(context.Background(), &agentpb.ListVideoDevicesRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(listed.Devices) != 2 {
		t.Fatalf("cameras: %v", listed.Devices)
	}
	camera := listed.Devices[1]
	if camera.Name != "My Go2 front camera" || camera.StableId != "unitree:GO2TEST1:front" || camera.Path != "" || camera.Address != "192.0.2.20" || !camera.Online || !camera.HasCredentials || camera.Transport != agentpb.VideoTransport_VIDEO_TRANSPORT_IP || camera.Id <= maxVideoDeviceID {
		t.Fatalf("Go2 camera: %v", camera)
	}
	link.mu.Lock()
	subscribed := len(link.subscribed)
	link.mu.Unlock()
	if subscribed != 0 {
		t.Fatal("listing started video")
	}
	link.online.Store(false)
	offline, err := client.ListVideoDevices(context.Background(), &agentpb.ListVideoDevicesRequest{IncludeUnpaired: true})
	if err != nil || len(offline.GetDevices()) != 2 {
		t.Fatalf("offline list: %v %v", offline, err)
	}
	if got := offline.Devices[1]; got.Id != camera.Id || got.StableId != camera.StableId || got.Online {
		t.Fatalf("offline camera: %v", got)
	}
	// A new video service resolves the same serial without an ephemeral ID map.
	restarted := newTestVideoService(func() ([]string, error) { return nil, nil }, nil)
	restarted.SetUnitreeCameras(video.unitreeCameras.Load())
	t.Cleanup(restarted.Shutdown)
	again, err := restarted.listUnitreeCameras(unitreeTestContext())
	if err != nil || len(again) != 1 || again[0].Id != camera.Id {
		t.Fatalf("restart: %v %v", again, err)
	}
	sources := newCameraDataAdapter(video).Discover(unitreeTestContext())
	if len(sources) != 1 || sources[0].ID != "v4l2:/dev/video0" {
		t.Fatalf("advertised an unsupported RTSP/data source: %v", sources)
	}
}

func TestUnitreeCameraStreamPreservesFramesAndReleasesViewer(t *testing.T) {
	for _, stable := range []bool{false, true} {
		t.Run(map[bool]string{false: "numeric", true: "stable"}[stable], func(t *testing.T) {
			client, _, _, link := unitreeVideoFixture(t, unitreeTestContext())
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			req := &agentpb.StreamVideoRequest{DeviceId: unitreeCameraID("GO2TEST1")}
			if stable {
				req.StableId = unitreeCameraIdentity("GO2TEST1")
				req.DeviceId = unitreeCameraID("WRONG")
			}
			stream, err := client.StreamVideo(ctx, req)
			if err != nil {
				t.Fatal(err)
			}
			payload := []byte{0, 0, 0, 1, 0x65, 1, 2, 3}
			link.video <- unitree.VideoFrame{AnnexB: payload, Received: time.Unix(0, 123456789)}
			frame, err := stream.Recv()
			if err != nil || !bytes.Equal(frame.GetData(), payload) || frame.GetCodec() != agentpb.VideoCodec_VIDEO_CODEC_H264 || frame.GetTimestampNs() != 123456789 {
				t.Fatalf("frame: %v %v", frame, err)
			}
			cancel()
			if _, err := stream.Recv(); status.Code(err) != codes.Canceled {
				t.Fatalf("cancel: %v", err)
			}
			deadline := time.Now().Add(time.Second)
			for {
				link.mu.Lock()
				released := link.cancelled
				topics := append([]string(nil), link.subscribed...)
				link.mu.Unlock()
				if released {
					if len(topics) != 1 || topics[0] != "video" || !link.Connected() || len(link.log()) != 0 {
						t.Fatalf("viewer affected robot connection or commands: %v", topics)
					}
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("video subscription leaked")
				}
				time.Sleep(time.Millisecond)
			}
		})
	}
}

func TestUnitreeCameraAccessChecks(t *testing.T) {
	for _, tc := range []struct {
		name string
		ctx  context.Context
		code codes.Code
	}{
		{"plaintext", context.Background(), codes.Unauthenticated},
		{"foreign-owner", unitreeTLSContext(t, "urn:wendy:org:43:user:fixture"), codes.PermissionDenied},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, _, _, link := unitreeVideoFixture(t, tc.ctx)
			listed, err := client.ListVideoDevices(context.Background(), &agentpb.ListVideoDevicesRequest{})
			if err != nil || len(listed.GetDevices()) != 1 || listed.Devices[0].Id != 0 {
				t.Fatalf("pairing exposed or local camera hidden: %v %v", listed, err)
			}
			for _, req := range []*agentpb.StreamVideoRequest{{DeviceId: unitreeCameraID("GO2TEST1")}, {StableId: unitreeCameraIdentity("GO2TEST1")}} {
				stream, err := client.StreamVideo(context.Background(), req)
				if err == nil {
					_, err = stream.Recv()
				}
				if status.Code(err) != tc.code {
					t.Fatalf("access: %v", err)
				}
			}
			link.mu.Lock()
			defer link.mu.Unlock()
			if len(link.subscribed) != 0 {
				t.Fatal("unauthorized viewer subscribed")
			}
		})
	}
}

func TestUnitreeCameraRefusesUnavailableModesAndMissingRobots(t *testing.T) {
	client, _, _, link := unitreeVideoFixture(t, unitreeTestContext())
	for _, tc := range []struct {
		name string
		req  *agentpb.StreamVideoRequest
		code codes.Code
	}{
		{"raw", &agentpb.StreamVideoRequest{DeviceId: unitreeCameraID("GO2TEST1"), Codec: agentpb.VideoCodec_VIDEO_CODEC_RAW}, codes.FailedPrecondition},
		{"vp8", &agentpb.StreamVideoRequest{DeviceId: unitreeCameraID("GO2TEST1"), Codec: agentpb.VideoCodec_VIDEO_CODEC_VP8}, codes.InvalidArgument},
		{"resize", &agentpb.StreamVideoRequest{DeviceId: unitreeCameraID("GO2TEST1"), Width: 640, Height: 480}, codes.InvalidArgument},
		{"framerate", &agentpb.StreamVideoRequest{DeviceId: unitreeCameraID("GO2TEST1"), Framerate: 30}, codes.InvalidArgument},
		{"missing-id", &agentpb.StreamVideoRequest{DeviceId: unitreeCameraID("MISSING")}, codes.NotFound},
		{"missing-stable", &agentpb.StreamVideoRequest{DeviceId: unitreeCameraID("GO2TEST1"), StableId: unitreeCameraIdentity("MISSING")}, codes.NotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stream, err := client.StreamVideo(context.Background(), tc.req)
			if err == nil {
				_, err = stream.Recv()
			}
			if status.Code(err) != tc.code {
				t.Fatalf("error: %v", err)
			}
		})
	}
	if _, err := client.GetCameraControls(context.Background(), &agentpb.GetCameraControlsRequest{DeviceId: unitreeCameraID("GO2TEST1")}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("controls: %v", err)
	}
	link.mu.Lock()
	subscribed := len(link.subscribed)
	link.mu.Unlock()
	if subscribed != 0 {
		t.Fatal("invalid request started video")
	}
	link.online.Store(false)
	stream, err := client.StreamVideo(context.Background(), &agentpb.StreamVideoRequest{DeviceId: unitreeCameraID("GO2TEST1")})
	if err == nil {
		_, err = stream.Recv()
	}
	if status.Code(err) != codes.Unavailable {
		t.Fatalf("disconnected: %v", err)
	}
}

func cameraPeerContext(ctx, caller context.Context) context.Context {
	if p, ok := peer.FromContext(caller); ok {
		return peer.NewContext(ctx, p)
	}
	return ctx
}

func TestUnitreeCameraDisconnectEndsStream(t *testing.T) {
	client, _, pairing, link := unitreeVideoFixture(t, unitreeTestContext())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stream, err := client.StreamVideo(ctx, &agentpb.StreamVideoRequest{StableId: unitreeCameraIdentity("GO2TEST1")})
	if err != nil {
		t.Fatal(err)
	}
	link.video <- unitree.VideoFrame{AnnexB: []byte{0, 0, 0, 1, 0x65}, Received: time.Now()}
	if _, err := stream.Recv(); err != nil {
		t.Fatal(err)
	}
	if err := pairing.Rebind(unitree.PairingOwner{OrgID: 43}); err != nil {
		t.Fatal(err)
	}
	if _, err := stream.Recv(); status.Code(err) != codes.Unavailable {
		t.Fatalf("rebound stream: %v", err)
	}
	listed, err := client.ListVideoDevices(ctx, &agentpb.ListVideoDevicesRequest{})
	if err != nil || len(listed.GetDevices()) != 1 {
		t.Fatalf("rebound camera list: %v %v", listed, err)
	}
}

func TestUnitreeCameraIDCollisionCannotSelectWrongRobot(t *testing.T) {
	_, video, pairing, _ := unitreeVideoFixture(t, unitreeTestContext())
	// These serials share the first 31 digest bits. Neither may be selected by
	// list order when the user passes that numeric camera ID.
	for _, serial := range []string{"GO2COLLIDE023801", "GO2COLLIDE072029"} {
		if err := pairing.store.Save(unitree.Pairing{Device: unitree.Device{Serial: serial, Name: serial, Address: "192.0.2.21", EncryptionKey: unitreeTestKey}, PairedAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	if unitreeCameraID("GO2COLLIDE023801") != unitreeCameraID("GO2COLLIDE072029") {
		t.Fatal("fixture no longer collides")
	}
	if _, err := video.listUnitreeCameras(unitreeTestContext()); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("ambiguous IDs: %v", err)
	}
}
