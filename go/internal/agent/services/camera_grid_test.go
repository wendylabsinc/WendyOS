package services

import (
	"context"
	"encoding/binary"
	"errors"
	"image"
	"math"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/wendylabsinc/wendy/go/internal/agent/data"
	"github.com/wendylabsinc/wendy/go/internal/agent/data/live"
	"github.com/wendylabsinc/wendy/go/internal/agent/ros2camera"
	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"github.com/wendylabsinc/wendy/go/proto/gen/datapb"
)

const (
	thermalSource = "v4l2:/dev/video4"
	thermalGridID = "agent/camera/grid/" + thermalSource
	thermalMarkID = "agent/camera/markers/" + thermalSource
	lumaSource    = "v4l2:/dev/video96"
	lumaGridID    = "agent/camera/grid/" + lumaSource
)

// gridRig runs the camera grid publisher on a real data manager. Its cameras
// are described, not opened: /dev/video4 offers the TC001's stacked mode and
// its producer is fake, so a test puts raw frames on its hub by hand;
// /dev/video0 is an ordinary webcam; /dev/video96 is a SensorLink camera fed
// through a real SensorFrameFeed.
type gridRig struct {
	manager  *data.Manager
	video    *VideoService
	fp       *fakeProducers
	feed     *SensorFrameFeed
	mu       sync.Mutex
	nodes    []string
	warnings []string
}

func newGridRig(t *testing.T, nodes ...string) *gridRig {
	t.Helper()
	rig := &gridRig{nodes: nodes}
	prevSizes, prevIdentity := enumerateRawFrameSizes, gridNodeIdentity
	enumerateRawFrameSizes = func(path string, pixfmt uint32) [][2]uint32 {
		if path != "/dev/video4" || pixfmt != v4l2PixFmtYUYV {
			return nil
		}
		return [][2]uint32{{256, 192}, {512, 384}, {512, 484}}
	}
	gridNodeIdentity = func(path string) (string, bool) { return path, true }
	t.Cleanup(func() { enumerateRawFrameSizes, gridNodeIdentity = prevSizes, prevIdentity })

	rig.video = newTestVideoService(func() ([]string, error) {
		rig.mu.Lock()
		defer rig.mu.Unlock()
		return append([]string(nil), rig.nodes...), nil
	}, nil)
	rig.fp = installFakeProducers(rig.video)
	rig.feed = NewSensorFrameFeed()
	rig.video.SetSensorFrameFeed(rig.feed)
	m, err := data.NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	m.SetWarnLogger(func(msg string) {
		rig.mu.Lock()
		rig.warnings = append(rig.warnings, msg)
		rig.mu.Unlock()
	})
	rig.manager = m

	g := newCameraGrids(m, rig.video)
	g.scanEvery, g.retryAfter = 10*time.Millisecond, 150*time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		g.run(ctx)
	}()
	t.Cleanup(func() { cancel(); <-done })
	return rig
}

func (r *gridRig) setNodes(nodes ...string) {
	r.mu.Lock()
	r.nodes = nodes
	r.mu.Unlock()
}

func (r *gridRig) warned(substr string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, w := range r.warnings {
		if strings.Contains(w, substr) {
			return true
		}
	}
	return false
}

func (r *gridRig) subscribe(t *testing.T, sel live.Selection) *live.Subscriber {
	t.Helper()
	s, err := r.manager.Live().Subscribe(sel)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s
}

// rawHub waits for the n-th producer the publisher started, and for its raw
// subscriber.
func (r *gridRig) rawHub(t *testing.T, n int) *deviceHub {
	t.Helper()
	eventually(t, "the camera producer starts", func() bool { return r.fp.count() > n })
	if req := r.fp.request(n); req.GetDeviceId() != 4 || req.GetCodec() != agentpb.VideoCodec_VIDEO_CODEC_RAW || streamRequestAssertsParams(req) {
		t.Fatalf("producer started for %v; want a parameter-less raw join of /dev/video4", req)
	}
	hub := r.fp.hub(n)
	eventually(t, "a raw subscriber joins", hub.wantRaw)
	return hub
}

// thermalHub is rawHub, with the producer offering raw frames of format.
func (r *gridRig) thermalHub(t *testing.T, n int, format *agentpb.RawFormat, pictureTop uint32) *deviceHub {
	t.Helper()
	hub := r.rawHub(t, n)
	hub.rawOffered(format, pictureTop)
	return hub
}

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting until %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// never fails if ok becomes true within a few scans.
func never(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(100 * time.Millisecond)
	for time.Now().Before(deadline) {
		if ok() {
			t.Fatalf("unexpectedly %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func nextRecord(t *testing.T, s *live.Subscriber) *live.Record {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	d, err := s.Next(ctx)
	if err != nil {
		t.Fatalf("no live record: %v", err)
	}
	return d.Record
}

func requireNoRecord(t *testing.T, s *live.Subscriber) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if d, err := s.Next(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("unexpected live record %+v %v", d.Record, err)
	}
}

func gridCatalogEntry(t *testing.T, s *live.Subscriber) *agentpbv2.DataStreamInfo {
	t.Helper()
	info := &agentpbv2.DataStreamInfo{}
	if err := proto.Unmarshal(nextRecord(t, s).Payload, info); err != nil {
		t.Fatal(err)
	}
	return info
}

func unmarshalPayload[T proto.Message](t *testing.T, r *live.Record, m T) T {
	t.Helper()
	if err := proto.Unmarshal(r.Payload, m); err != nil {
		t.Fatal(err)
	}
	return m
}

// A camera's streams are in the catalog, idle, from the moment it is present,
// and nothing is decoded for a catalog viewer or for a viewer that takes every
// stream. A viewer that names the grid starts the camera as a parameter-less
// raw subscriber; it gets the radiometric block stamped with the raw frame's
// timestamp_ns, and so does the viewer of everything, meanwhile. When it
// leaves, the raw subscription goes, and with it the producer it started.
func TestCameraGridRunsOnlyWhileAViewerNamesIt(t *testing.T) {
	rig := newGridRig(t, "/dev/video0", "/dev/video1", "/dev/video4")
	catalog := rig.subscribe(t, live.Selection{StreamPrefixes: []string{live.CatalogPrefix + "agent/camera/"}, SendLatest: true})
	entries := map[string]*agentpbv2.DataStreamInfo{}
	for len(entries) < 2 {
		info := gridCatalogEntry(t, catalog)
		entries[info.GetStreamId()] = info
	}
	for id, schema := range map[string]string{thermalGridID: "wendy.data.v1.Grid", thermalMarkID: "wendy.data.v1.VideoMarkers"} {
		info := entries[id]
		if info.GetSchema() != schema || info.GetMediaType() != "application/protobuf" || info.GetPublisher() != "agent" || info.GetState() != agentpbv2.DataStreamInfo_STATE_IDLE {
			t.Fatalf("catalog entry for %s: %v", id, info)
		}
	}
	everything := rig.subscribe(t, live.Selection{})
	never(t, "a producer started with nobody naming a camera stream", func() bool { return rig.fp.count() > 0 })

	viewer, err := rig.manager.Live().Subscribe(live.Selection{StreamIDs: []string{thermalGridID}})
	if err != nil {
		t.Fatal(err)
	}
	hub := rig.thermalHub(t, 0, tc001Format, tc001PictureTop)
	frame := stackedThermalFrame(roomWith(200, 50, 61.5, 10, 180, 4.25))
	const frameTime = 1_792_000_000_123_456_789
	if !hub.publishRaw(frame, frameTime, tc001Format) {
		t.Fatal("hub has no subscribers")
	}
	for _, s := range []*live.Subscriber{viewer, everything} {
		r := nextRecord(t, s)
		if r.StreamID != thermalGridID || r.Schema != "wendy.data.v1.Grid" || r.TimestampUnixNanos != frameTime || r.SourceID != thermalSource || r.SampleID != 0 {
			t.Fatalf("grid record %+v", r)
		}
		grid := unmarshalPayload(t, r, &datapb.Grid{})
		if !proto.Equal(grid, thermalGrid(frame, "/dev/video4")) {
			t.Fatalf("grid %v is not the frame's block", grid.GetCamera())
		}
	}
	// Nobody asked for the markers, so none were made.
	requireNoRecord(t, everything)

	viewer.Close()
	eventually(t, "the raw subscription ends", func() bool { return hub.ctx.Err() != nil })
	if rig.fp.count() != 1 {
		t.Fatalf("%d producers started", rig.fp.count())
	}
}

// On a camera a video viewer already holds, the decoder joins the running
// producer as one more subscriber: nothing restarts, the raw tap copies frames
// only while a grid viewer is there, and the video viewer keeps the camera
// when it leaves.
func TestCameraGridJoinsTheCameraItsViewersHold(t *testing.T) {
	rig := newGridRig(t, "/dev/video4")
	hub, id, _, err := rig.video.joinHub(context.Background(), "/dev/video4", &agentpb.StreamVideoRequest{DeviceId: 4, Width: 1280, Height: 720})
	if err != nil {
		t.Fatal(err)
	}
	defer hub.unsubscribe(id)
	hub.rawOffered(tc001Format, tc001PictureTop)
	rig.subscribe(t, live.Selection{})
	never(t, "raw frames were wanted with nobody naming a grid", hub.wantRaw)

	viewer, err := rig.manager.Live().Subscribe(live.Selection{StreamPrefixes: []string{"agent/camera/grid/"}})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "the decoder joins the running producer", hub.wantRaw)
	hub.publishRaw(stackedThermalFrame(uniform(30)), 7, tc001Format)
	if r := nextRecord(t, viewer); r.StreamID != thermalGridID || r.TimestampUnixNanos != 7 {
		t.Fatalf("grid record %+v", r)
	}
	viewer.Close()
	eventually(t, "the decoder lets go", func() bool { return !hub.wantRaw() })
	if rig.fp.count() != 1 || hub.ctx.Err() != nil {
		t.Fatalf("%d producers, the viewer's ended: %v", rig.fp.count(), hub.ctx.Err())
	}
}

// A viewer that asks for stream parameters gets a camera the decoder alone
// opened at defaults: the camera restarts at the viewer's parameters, held by
// the viewer, and the decoder follows it. A camera a parameter-less viewer
// shares with the decoder is not taken from that viewer.
func TestCameraGridYieldsTheCameraToAViewerWithParameters(t *testing.T) {
	rig := newGridRig(t, "/dev/video4")
	viewer := rig.subscribe(t, live.Selection{StreamIDs: []string{thermalGridID}})
	defaulted := rig.rawHub(t, 0)
	ctx := context.Background()
	hub, id, _, err := rig.video.getOrCreateHub(ctx, "/dev/video4", &agentpb.StreamVideoRequest{DeviceId: 4, Width: 512, Height: 484, Framerate: 25}, hubHolderStreamClient)
	if err != nil {
		t.Fatalf("a viewer with parameters was refused the decoder's camera: %v", err)
	}
	if hub == defaulted || !defaulted.wasRestarted() || rig.fp.count() != 2 {
		t.Fatalf("the camera was not restarted for the viewer: %d producers", rig.fp.count())
	}
	if req := rig.fp.request(1); req.GetWidth() != 512 || req.GetHeight() != 484 || req.GetFramerate() != 25 {
		t.Fatalf("restarted with %v", req)
	}
	if holder, held := hub.heldExplicitly(); !held || holder != hubHolderStreamClient {
		t.Fatalf("held by %q (%v), want the viewer", holder, held)
	}
	eventually(t, "the decoder follows the camera", hub.wantRaw)
	hub.rawOffered(tc001Format, tc001PictureTop)
	hub.publishRaw(stackedThermalFrame(uniform(30)), 9, tc001Format)
	if r := nextRecord(t, viewer); r.StreamID != thermalGridID || r.TimestampUnixNanos != 9 {
		t.Fatalf("grid record after the restart %+v", r)
	}

	// With the viewer gone the decoder holds the camera alone, at the viewer's
	// parameters. A parameter-less viewer joins it, and from then on a viewer
	// that asks for other parameters is refused, as it would be without the
	// decoder.
	hub.unsubscribe(id)
	bare, bareID, _, err := rig.video.getOrCreateHub(ctx, "/dev/video4", &agentpb.StreamVideoRequest{DeviceId: 4}, hubHolderStreamClient)
	if err != nil || bare != hub {
		t.Fatalf("a parameter-less viewer did not join the camera: %v", err)
	}
	defer bare.unsubscribe(bareID)
	if _, _, _, err := rig.video.getOrCreateHub(ctx, "/dev/video4", &agentpb.StreamVideoRequest{DeviceId: 4, Width: 256, Height: 192, Framerate: 25}, hubHolderStreamClient); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("a camera a parameter-less viewer shares: got %v, want InvalidArgument", err)
	}
	if hub.wasRestarted() || rig.fp.count() != 2 {
		t.Fatalf("the shared camera was restarted: %d producers", rig.fp.count())
	}
}

// A viewer of the markers schema gets the hottest and coldest pixel on every
// frame, and no grid is made for it. A frame the plausibility guard refuses
// clears the dots.
func TestCameraGridHotAndColdMarkers(t *testing.T) {
	rig := newGridRig(t, "/dev/video4")
	everything := rig.subscribe(t, live.Selection{})
	viewer := rig.subscribe(t, live.Selection{Schemas: []string{"wendy.data.v1.VideoMarkers"}})
	hub := rig.thermalHub(t, 0, tc001Format, tc001PictureTop)

	hub.publishRaw(stackedThermalFrame(roomWith(128, 96, 110, 10, 180, 4)), 1000, tc001Format)
	r := nextRecord(t, viewer)
	if r.StreamID != thermalMarkID || r.TimestampUnixNanos != 1000 || r.SourceID != thermalSource {
		t.Fatalf("markers record %+v", r)
	}
	markers := unmarshalPayload(t, r, &datapb.VideoMarkers{})
	if markers.GetCamera() != "/dev/video4" || len(markers.GetMarkers()) != 2 || markers.Markers[0].GetLabel() != "max" || !within(markers.Markers[0].GetX(), 128.5/256, 1e-6) {
		t.Fatalf("markers %v", markers)
	}
	if r := nextRecord(t, everything); r.StreamID != thermalMarkID {
		t.Fatalf("the viewer of everything got %s", r.StreamID)
	}
	requireNoRecord(t, everything)

	// Sixty-fourths: a room read as a 907 C furnace.
	furnace := stackedThermalFrame(uniform(22))
	for i := 0; i < thermalSensorPixels; i++ {
		binary.LittleEndian.PutUint16(furnace[2*i:], uint16(math.Round((22+kelvinZeroCelsius)*64)))
	}
	hub.publishRaw(furnace, 2000, tc001Format)
	if markers := unmarshalPayload(t, nextRecord(t, viewer), &datapb.VideoMarkers{}); len(markers.GetMarkers()) != 0 {
		t.Fatalf("an implausible frame placed markers: %v", markers)
	}
	eventually(t, "the refusal is logged", func() bool { return rig.warned("outside -40..600 °C") })
}

// A camera that is not in the measured mode, because a viewer pinned its plain
// picture mode or the picture is not cropped, gets no temperatures: the
// decoder lets go of the camera, says why, and tries again later.
func TestCameraGridLetsGoOfACameraWithoutTemperatures(t *testing.T) {
	rig := newGridRig(t, "/dev/video4")
	rig.subscribe(t, live.Selection{StreamIDs: []string{thermalGridID}})
	plain := &agentpb.RawFormat{Width: 512, Height: 384, Fourcc: fourccYUYV, BytesPerLine: 1024}
	hub := rig.thermalHub(t, 0, plain, 0)
	hub.publishRaw(make([]byte, 1024*384), 1, plain)
	eventually(t, "the decoder lets go", func() bool { return hub.ctx.Err() != nil })
	eventually(t, "the reason is logged", func() bool { return rig.warned("512x384 YUYV") })

	// Retried after the back-off; this time the producer refuses raw frames.
	rig.rawHub(t, 1).rawNotOffered(fixedReason("camera is captured as MJPEG"))
	eventually(t, "the refusal is logged", func() bool { return rig.warned("captured as MJPEG") })
}

// A SensorLink camera's brightness comes from the frames the agent receives
// in-process; the camera's node and hub are never touched.
func TestCameraGridBrightnessFromTheSensorLinkFeed(t *testing.T) {
	rig := newGridRig(t, "/dev/video96")
	viewer := rig.subscribe(t, live.Selection{StreamIDs: []string{lumaGridID}})
	eventually(t, "the decoder subscribes to the feed", func() bool {
		rig.feed.mu.Lock()
		defer rig.feed.mu.Unlock()
		return len(rig.feed.subs["/dev/video96"]) == 1
	})
	before := time.Now().UnixNano()
	jpegFrame := greyJPEG(t, 800, 600, image.Pt(600, 100), image.Pt(100, 500), true)
	rig.feed.publish("/dev/video96", ros2camera.Frame{Data: jpegFrame, Width: 800, Height: 600, Codec: ros2camera.CodecMJPEG})
	r := nextRecord(t, viewer)
	if r.StreamID != lumaGridID || r.Schema != "wendy.data.v1.Grid" || r.SourceID != lumaSource || r.TimestampUnixNanos < before || r.TimestampUnixNanos > time.Now().UnixNano() {
		t.Fatalf("brightness record %+v", r)
	}
	grid := unmarshalPayload(t, r, &datapb.Grid{})
	want, err := lumaGrid(jpegFrame, "/dev/video96")
	if err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(grid, want) {
		t.Fatalf("grid %dx%d %q is not the frame's brightness", grid.GetWidth(), grid.GetHeight(), grid.GetCamera())
	}
	if rig.fp.count() != 0 {
		t.Fatal("the brightness grid started the camera's producer")
	}

	// A board that sends H.264 has no brightness grid.
	rig.feed.publish("/dev/video96", ros2camera.Frame{Data: []byte{0, 0, 0, 1}, Codec: ros2camera.CodecH264})
	eventually(t, "the refusal is logged", func() bool { return rig.warned(errSensorFeedNotJPEG.Error()) })
}

// A camera that goes away takes its streams out of the catalog, and its
// decoder with them.
func TestCameraGridFollowsTheCameras(t *testing.T) {
	rig := newGridRig(t, "/dev/video4")
	catalog := rig.subscribe(t, live.Selection{StreamPrefixes: []string{live.CatalogPrefix + "agent/camera/grid/"}, SendLatest: true})
	if info := gridCatalogEntry(t, catalog); info.GetStreamId() != thermalGridID || info.GetState() != agentpbv2.DataStreamInfo_STATE_IDLE {
		t.Fatalf("catalog %v", info)
	}
	rig.subscribe(t, live.Selection{StreamPrefixes: []string{"agent/camera/"}})
	hub := rig.thermalHub(t, 0, tc001Format, tc001PictureTop)
	rig.setNodes()
	if info := gridCatalogEntry(t, catalog); info.GetStreamId() != thermalGridID || info.GetState() != agentpbv2.DataStreamInfo_STATE_GONE {
		t.Fatalf("catalog after the camera left: %v", info)
	}
	eventually(t, "the decoder stops", func() bool { return hub.ctx.Err() != nil })
	rig.setNodes("/dev/video4")
	if info := gridCatalogEntry(t, catalog); info.GetStreamId() != thermalGridID || info.GetState() != agentpbv2.DataStreamInfo_STATE_IDLE {
		t.Fatalf("catalog after the camera came back: %v", info)
	}
	rig.thermalHub(t, 1, tc001Format, tc001PictureTop)
}

// A raw subscriber learns why a camera has no raw frames, as a raw viewer does.
func TestRawFrameSubscriptionReportsARefusal(t *testing.T) {
	svc := newTestVideoService(nil, nil)
	fp := installFakeProducers(svc)
	sub, err := svc.subscribeRawFrames(context.Background(), "v4l2:/dev/video4")
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	fp.hub(0).rawNotOffered(fixedReason("camera streams H.264 natively; raw frames are not offered"))
	_, err = sub.Next(context.Background())
	if status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "H.264 natively") {
		t.Fatalf("Next: %v", err)
	}
	if _, err := svc.subscribeRawFrames(context.Background(), "v4l2:/dev/video04"); err == nil {
		t.Fatal("an alias of a camera's source id was accepted")
	}
}

// apps serves System API sockets from the rig's live data, as the agent does
// for its apps.
func (r *gridRig) apps(t *testing.T) *systemDataTest {
	t.Helper()
	useSystemAPIRoot(t)
	ctx, cancel := context.WithCancel(context.Background())
	sockets := NewAppSystemAPISocketManager(ctx, zap.NewNop(), &recordingNotificationSender{}, r.manager.Live())
	t.Cleanup(func() { cancel(); sockets.stopAll() })
	return &systemDataTest{t: t, capture: r.manager, sockets: sockets}
}

func (r *gridRig) feedSubscribers(path string) int {
	r.feed.mu.Lock()
	defer r.feed.mu.Unlock()
	return len(r.feed.subs[path])
}

// An app reads the agent's camera streams with data-read only while it holds
// camera too, and only for the cameras a camera allowlist names. Without it
// they are refused by name, missing from wider selections and the catalog,
// and never start a camera. A stream opened with camera ends when the app
// loses it.
func TestCameraGridNeedsTheCameraEntitlement(t *testing.T) {
	rig := newGridRig(t, "/dev/video4", "/dev/video96")
	agent := rig.apps(t)
	readAll := appconfig.Entitlement{Type: appconfig.EntitlementDataRead}
	camera := appconfig.Entitlement{Type: appconfig.EntitlementCamera}
	agent.capture.AdvertiseAppStream("com.example.detector", "", "markers", liveMarkersStream)

	blindConn := agent.app("com.example.blind", "", readAll)
	for _, id := range []string{thermalGridID, thermalMarkID, lumaGridID, live.CatalogPrefix + thermalGridID} {
		if got := streamDataError(t, blindConn, id); got != codes.PermissionDenied {
			t.Fatalf("%s without camera: got %v, want PermissionDenied", id, got)
		}
	}
	blind := agentpbv2.NewDataServiceClient(blindConn)
	views := []agentpbv2.DataService_StreamDataClient{
		openLiveStreamWith(t, blind, &agentpbv2.StreamDataRequest{StreamPrefixes: []string{"agent/camera/", "apps/"}}),
		openLiveStreamWith(t, blind, &agentpbv2.StreamDataRequest{Schemas: []string{"wendy.data.v1.Grid", "wendy.data.v1.VideoMarkers"}}),
		openLiveStream(t, blind),
	}
	catalog := openLiveStreamWith(t, blind, &agentpbv2.StreamDataRequest{StreamPrefixes: []string{"_streams/"}, SendLatest: true})
	if r := recvLive(t, catalog); r.GetStreamId() != "_streams/apps/com.example.detector/markers" {
		t.Fatalf("catalog without camera: %v", r)
	}
	never(t, "an app without camera started a camera", func() bool { return rig.fp.count() > 0 || rig.feedSubscribers("/dev/video96") > 0 })

	// With camera, naming the grid starts the thermal camera.
	seeing := agentpbv2.NewDataServiceClient(agent.app("com.example.seeing", "", readAll, camera))
	grid := openLiveStream(t, seeing, thermalGridID)
	hub := rig.thermalHub(t, 0, tc001Format, tc001PictureTop)
	hub.publishRaw(stackedThermalFrame(uniform(30)), 7, tc001Format)
	if r := recvLive(t, grid); r.GetStreamId() != thermalGridID || r.GetTimestampUnixNanos() != 7 {
		t.Fatalf("grid with camera: %v", r)
	}
	// Meanwhile the app without camera saw neither the grid nor its catalog
	// entry going active: the next thing it sees is an app's record.
	agent.record("com.example.detector", "markers", "boxes")
	for _, view := range views {
		if r := recvLive(t, view); r.GetStreamId() != "apps/com.example.detector/markers" {
			t.Fatalf("an app without camera got %v", r)
		}
	}
	if r := recvLive(t, catalog); r.GetStreamId() != "_streams/apps/com.example.detector/markers" {
		t.Fatalf("catalog without camera: %v", r)
	}

	// A camera allowlist admits the cameras it names and no others.
	narrowConn := agent.app("com.example.narrow", "", readAll, appconfig.Entitlement{Type: appconfig.EntitlementCamera, Allowlist: []string{"/dev/video96"}})
	if got := streamDataError(t, narrowConn, thermalGridID); got != codes.PermissionDenied {
		t.Fatalf("a camera outside the allowlist: got %v, want PermissionDenied", got)
	}
	openLiveStream(t, agentpbv2.NewDataServiceClient(narrowConn), lumaGridID)
	eventually(t, "the allowed camera's decoder starts", func() bool { return rig.feedSubscribers("/dev/video96") == 1 })

	// Redeployed without camera, the app's grid stream ends, and with it the
	// thermal decoder nobody else asked for.
	if _, err := agent.sockets.Ensure("com.example.seeing", "", SystemAPIGrantFor([]appconfig.Entitlement{readAll})); err != nil {
		t.Fatal(err)
	}
	if got := endOf(t, grid); got != codes.Aborted {
		t.Fatalf("grid stream after camera was withdrawn: got %v, want Aborted", got)
	}
	eventually(t, "the thermal decoder stops", func() bool { return hub.ctx.Err() != nil })
}

func TestSystemAPIGrantForCamera(t *testing.T) {
	readAll := appconfig.Entitlement{Type: appconfig.EntitlementDataRead}
	dataRead := []string{SystemAPICapabilityDataRead}
	for _, tc := range []struct {
		name         string
		entitlements []appconfig.Entitlement
		want         SystemAPIGrant
	}{
		{"camera without data-read", []appconfig.Entitlement{{Type: appconfig.EntitlementCamera}, {Type: appconfig.EntitlementNotifications}}, SystemAPIGrant{Capabilities: []string{SystemAPICapabilityNotifications}}},
		{"data-read without camera", []appconfig.Entitlement{readAll}, SystemAPIGrant{Capabilities: dataRead}},
		{"data-read and camera", []appconfig.Entitlement{readAll, {Type: appconfig.EntitlementCamera}}, SystemAPIGrant{Capabilities: dataRead, Camera: true}},
		{"a camera allowlist", []appconfig.Entitlement{{Type: appconfig.EntitlementCamera, Allowlist: []string{"/dev/video0"}}, readAll}, SystemAPIGrant{Capabilities: dataRead, Camera: true, CameraAllowlist: []string{"/dev/video0"}}},
		{"the deprecated video entitlement", []appconfig.Entitlement{readAll, {Type: appconfig.EntitlementVideo, Allowlist: []string{"/dev/video2"}}}, SystemAPIGrant{Capabilities: dataRead, Camera: true, CameraAllowlist: []string{"/dev/video2"}}},
		// One camera entitlement without an allowlist admits every camera.
		{"app and service camera", []appconfig.Entitlement{readAll, {Type: appconfig.EntitlementCamera, Allowlist: []string{"/dev/video0"}}, {Type: appconfig.EntitlementCamera}}, SystemAPIGrant{Capabilities: dataRead, Camera: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := SystemAPIGrantFor(tc.entitlements); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestCameraScope(t *testing.T) {
	none, every := cameraScope{}, cameraScope{granted: true}
	sensorLink := cameraScope{granted: true, devices: []string{"/dev/video96"}}
	for _, tc := range []struct {
		name  string
		scope cameraScope
		id    string
		want  bool
	}{
		{"no camera, grid", none, thermalGridID, false},
		{"no camera, markers", none, thermalMarkID, false},
		{"no camera, a later camera stream", none, "agent/camera/depth/v4l2:/dev/video4", false},
		{"no camera, campaign markers", none, "agent/campaign/door/markers/v4l2:/dev/video4", true},
		{"no camera, an app stream", none, "apps/com.example.detector/markers", true},
		{"every camera", every, thermalMarkID, true},
		{"every camera, a later camera stream", every, "agent/camera/depth/v4l2:/dev/video4", true},
		{"an allowed camera", sensorLink, lumaGridID, true},
		{"an allowed camera's markers", sensorLink, "agent/camera/markers/" + lumaSource, true},
		{"another camera", sensorLink, thermalGridID, false},
		{"a longer device path", sensorLink, "agent/camera/grid/v4l2:/dev/video960", false},
		{"a later camera stream", sensorLink, "agent/camera/depth/" + lumaSource, false},
	} {
		if got := tc.scope.admits(tc.id); got != tc.want {
			t.Errorf("%s: admits(%s) = %v, want %v", tc.name, tc.id, got, tc.want)
		}
	}
	for _, tc := range []struct {
		scope, within cameraScope
		want          bool
	}{
		{none, none, true},
		{none, sensorLink, false},
		{sensorLink, none, true},
		{every, sensorLink, true},
		{sensorLink, every, false},
		{sensorLink, sensorLink, true},
		{sensorLink, cameraScope{granted: true, devices: []string{"/dev/video4", "/dev/video96"}}, false},
	} {
		if got := tc.scope.covers(tc.within); got != tc.want {
			t.Errorf("%+v covers %+v = %v, want %v", tc.scope, tc.within, got, tc.want)
		}
	}
	if every.readable() != nil || none.readable() == nil || sensorLink.readable() == nil {
		t.Error("readable is nil only for a scope that admits every stream")
	}
}
