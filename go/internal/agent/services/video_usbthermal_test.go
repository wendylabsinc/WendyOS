package services

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/wendylabsinc/wendy/go/internal/agent/rawvideo"
	"github.com/wendylabsinc/wendy/go/internal/agent/usbthermal"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

const testThermalStableID = "by-id:usb-TOPDON_TECHNOLOGY_Co.__Ltd._TC001_Max_YFDVS000000000001-thermal"

// fakeUSBThermal stands in for usbthermal.Manager: one attached TC001 Max
// whose frames the test pushes.
type fakeUSBThermal struct {
	listed []usbthermal.Listing // guarded by mu once the service runs; see plug
	feed   chan usbthermal.RawFrame
	pics   chan usbthermal.Frame
	err    error

	mu         sync.Mutex
	active     int // live raw subscriptions
	activePics int // live picture subscriptions
	closed     bool
}

func newFakeUSBThermal() *fakeUSBThermal {
	return &fakeUSBThermal{
		listed: []usbthermal.Listing{{
			ID: usbthermal.IDBandStart, StableID: testThermalStableID, Name: "TC001 Max (thermal)",
			Model: "TC001 Max", Serial: "YFDVS000000000001", Port: "1-2.3",
			Layout: usbthermal.Layout{Width: 256, Height: 192},
		}},
		feed: make(chan usbthermal.RawFrame),
		pics: make(chan usbthermal.Frame),
	}
}

func (f *fakeUSBThermal) List() []usbthermal.Listing {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]usbthermal.Listing(nil), f.listed...)
}

// plug sets which cameras are attached.
func (f *fakeUSBThermal) plug(listed ...usbthermal.Listing) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listed = listed
}

func (f *fakeUSBThermal) SubscribePictures(ctx context.Context, stableID string) (<-chan usbthermal.Frame, error) {
	if stableID != testThermalStableID {
		return nil, errors.New("no such camera")
	}
	out := make(chan usbthermal.Frame, 2)
	f.mu.Lock()
	f.activePics++
	f.mu.Unlock()
	go func() {
		defer func() {
			f.mu.Lock()
			f.activePics--
			f.mu.Unlock()
			close(out)
		}()
		for {
			select {
			case <-ctx.Done():
				return
			case p := <-f.pics:
				select {
				case out <- p:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return out, nil
}

func (f *fakeUSBThermal) pictureSubscriptions() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.activePics
}

func (f *fakeUSBThermal) SubscribeRaw(ctx context.Context, stableID string) (<-chan usbthermal.RawFrame, error) {
	if stableID != testThermalStableID {
		return nil, errors.New("no such camera")
	}
	out := make(chan usbthermal.RawFrame, 2)
	f.mu.Lock()
	f.active++
	f.mu.Unlock()
	go func() {
		defer func() {
			f.mu.Lock()
			f.active--
			f.mu.Unlock()
			close(out)
		}()
		for {
			select {
			case <-ctx.Done():
				return
			case fr := <-f.feed:
				select {
				case out <- fr:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return out, nil
}

func (f *fakeUSBThermal) LastError(string) error { return f.err }

func (f *fakeUSBThermal) Close() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
}

func (f *fakeUSBThermal) subscriptions() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.active
}

// thermalStream is a ServerStreamingServer that keeps whole frames.
type thermalStream struct {
	grpc.ServerStreamingServer[agentpb.VideoFrame]
	ctx    context.Context
	frames chan *agentpb.VideoFrame
}

func (s *thermalStream) Context() context.Context { return s.ctx }

func (s *thermalStream) Send(f *agentpb.VideoFrame) error {
	select {
	case s.frames <- f:
		return nil
	case <-s.ctx.Done():
		return s.ctx.Err()
	}
}

func newUSBThermalTestService(t *testing.T) (*VideoService, *fakeUSBThermal) {
	t.Helper()
	svc := newTestVideoService(func() ([]string, error) { return []string{"/dev/video0"}, nil }, nil)
	fake := newFakeUSBThermal()
	useFakeUSBThermal(svc, fake)
	t.Cleanup(svc.Shutdown)
	return svc, fake
}

// useFakeUSBThermal swaps the service's driver for fake, and its live view for
// one on fake with no plug poll: StreamVideo's own sync is what registers the
// camera, so every stream test also runs with the camera registered for its
// encoded live view, as on a device.
func useFakeUSBThermal(svc *VideoService, fake *fakeUSBThermal) {
	if svc.usbThermalLive != nil {
		svc.usbThermalLive.stop() // the real driver's; it has nothing attached
	}
	svc.usbThermal = fake
	svc.usbThermalLive = newUSBThermalLiveView(svc.ctx, zap.NewNop(), fake, func(id uint32, feed *rawvideo.Feed) (func(), error) {
		return svc.registerInProcessCamera(id, feed, true)
	})
}

func testRawFrame(fill byte) usbthermal.RawFrame {
	return usbthermal.RawFrame{
		Data:  bytes.Repeat([]byte{fill}, 256*384*2),
		Width: 256, Height: 384, BytesPerLine: 512,
		Sequence:  7,
		Timestamp: time.Unix(1_800_000_000, 123),
	}
}

// startStream runs StreamVideo in the background and returns its frames and
// its result.
func startStream(svc *VideoService, req *agentpb.StreamVideoRequest) (*thermalStream, context.CancelFunc, <-chan error) {
	ctx, cancel := context.WithCancel(context.Background())
	stream := &thermalStream{ctx: ctx, frames: make(chan *agentpb.VideoFrame, 4)}
	done := make(chan error, 1)
	go func() { done <- svc.StreamVideo(req, stream) }()
	return stream, cancel, done
}

func TestListVideoDevicesIncludesUSBThermalCamera(t *testing.T) {
	svc, _ := newUSBThermalTestService(t)
	resp, err := svc.ListVideoDevices(context.Background(), &agentpb.ListVideoDevicesRequest{})
	if err != nil {
		t.Fatal(err)
	}
	var thermal *agentpb.VideoDevice
	for _, d := range resp.GetDevices() {
		if d.GetId() == usbthermal.IDBandStart {
			thermal = d
		}
	}
	if thermal == nil {
		t.Fatalf("the TC001 Max is not listed: %v", resp.GetDevices())
	}
	if thermal.GetName() != "TC001 Max (thermal)" || thermal.GetStableId() != testThermalStableID ||
		thermal.GetTransport() != agentpb.VideoTransport_VIDEO_TRANSPORT_USB || thermal.GetModel() != "TC001 Max" ||
		thermal.GetDriver() != "usbfs" || !thermal.GetOnline() || thermal.GetPath() != "" {
		t.Fatalf("listed as %v", thermal)
	}
	// The internal listing feeds episode capture and the two-plane path, which
	// address cameras by /dev/video node: the thermal camera must stay out.
	internal, err := svc.listCameras(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range internal {
		if d.GetId() == usbthermal.IDBandStart {
			t.Fatal("the TC001 Max leaked into listCameras")
		}
	}
}

// TestStreamVideoServesUSBThermalRawFrames: a raw request gets the camera's
// frames described as the stacked TC001 layout, and leaving releases the
// camera.
func TestStreamVideoServesUSBThermalRawFrames(t *testing.T) {
	svc, fake := newUSBThermalTestService(t)
	for _, req := range []*agentpb.StreamVideoRequest{
		{DeviceId: usbthermal.IDBandStart, Codec: agentpb.VideoCodec_VIDEO_CODEC_RAW},
		{StableId: testThermalStableID, Codec: agentpb.VideoCodec_VIDEO_CODEC_RAW, Width: 256, Height: 384},
	} {
		stream, cancel, done := startStream(svc, req)
		want := testRawFrame(0x4b)
		select {
		case fake.feed <- want:
		case err := <-done:
			t.Fatalf("StreamVideo(%v) ended: %v", req, err)
		case <-time.After(5 * time.Second):
			t.Fatal("the producer never subscribed to the camera")
		}
		var got *agentpb.VideoFrame
		select {
		case got = <-stream.frames:
		case <-time.After(5 * time.Second):
			t.Fatal("no frame reached the client")
		}
		if got.GetCodec() != agentpb.VideoCodec_VIDEO_CODEC_RAW || !bytes.Equal(got.GetData(), want.Data) ||
			got.GetTimestampNs() != uint64(want.Timestamp.UnixNano()) {
			t.Fatalf("frame codec %v, %d bytes, ts %d", got.GetCodec(), len(got.GetData()), got.GetTimestampNs())
		}
		f := got.GetRawFormat()
		if f.GetWidth() != 256 || f.GetHeight() != 384 || f.GetFourcc() != "YUYV" || f.GetBytesPerLine() != 512 {
			t.Fatalf("raw format %v, want 256x384 YUYV, 512 bytes per line", f)
		}
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) && status.Code(err) != codes.Canceled {
			t.Fatalf("StreamVideo returned %v after the client left", err)
		}
		deadline := time.Now().Add(5 * time.Second)
		for fake.subscriptions() != 0 {
			if time.Now().After(deadline) {
				t.Fatal("the camera subscription outlived the last client")
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
}

func TestStreamVideoRefusesWhatUSBThermalCannotDo(t *testing.T) {
	svc, _ := newUSBThermalTestService(t)
	cases := []struct {
		name string
		req  *agentpb.StreamVideoRequest
		code codes.Code
		says string
	}{
		{"another size", &agentpb.StreamVideoRequest{DeviceId: usbthermal.IDBandStart, Codec: agentpb.VideoCodec_VIDEO_CODEC_RAW, Width: 640, Height: 480}, codes.InvalidArgument, "256x384"},
		{"a frame rate", &agentpb.StreamVideoRequest{DeviceId: usbthermal.IDBandStart, Codec: agentpb.VideoCodec_VIDEO_CODEC_RAW, Framerate: 25}, codes.InvalidArgument, "framerate 0"},
	}
	for _, c := range cases {
		stream := &thermalStream{ctx: context.Background(), frames: make(chan *agentpb.VideoFrame, 1)}
		err := svc.StreamVideo(c.req, stream)
		if status.Code(err) != c.code || !strings.Contains(err.Error(), c.says) {
			t.Errorf("%s: %v, want %v mentioning %q", c.name, err, c.code, c.says)
		}
	}

	// Encoded video is the live view's. Without one (the camera could not be
	// registered for it), an encoded request is refused with the way out.
	svc.usbThermalLive.stop()
	svc.usbThermalLive = nil
	for _, codec := range []agentpb.VideoCodec{agentpb.VideoCodec_VIDEO_CODEC_H264, agentpb.VideoCodec_VIDEO_CODEC_VP8} {
		stream := &thermalStream{ctx: context.Background(), frames: make(chan *agentpb.VideoFrame, 1)}
		err := svc.StreamVideo(&agentpb.StreamVideoRequest{DeviceId: usbthermal.IDBandStart, Codec: codec}, stream)
		if status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "VIDEO_CODEC_RAW") {
			t.Errorf("%v with no live view: %v, want FailedPrecondition naming VIDEO_CODEC_RAW", codec, err)
		}
	}
}

// TestUSBThermalStreamSaysWhyNothingArrived: a camera that never delivers
// ends the stream with the driver's reason instead of leaving it waiting.
func TestUSBThermalStreamSaysWhyNothingArrived(t *testing.T) {
	first := usbThermalFirstFrameTimeout
	usbThermalFirstFrameTimeout = 50 * time.Millisecond
	t.Cleanup(func() { usbThermalFirstFrameTimeout = first })
	svc, fake := newUSBThermalTestService(t) // its Shutdown runs before the restore above
	fake.err = errors.New("TC001 Max at /dev/bus/usb/001/015 is held by another process")
	_, cancel, done := startStream(svc, &agentpb.StreamVideoRequest{DeviceId: usbthermal.IDBandStart, Codec: agentpb.VideoCodec_VIDEO_CODEC_RAW})
	defer cancel()
	select {
	case err := <-done:
		if status.Code(err) != codes.Unavailable || !strings.Contains(err.Error(), "held by another process") {
			t.Fatalf("StreamVideo = %v, want Unavailable with the driver's reason", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a camera that never delivered kept the stream waiting")
	}
}

// TestUSBThermalBandWithoutACamera: an id in the band that no thermal camera
// holds still means /dev/videoN, as before.
func TestUSBThermalBandWithoutACamera(t *testing.T) {
	svc, _ := newUSBThermalTestService(t)
	src, err := svc.resolveSource(usbthermal.IDBandStart + 1)
	if err != nil || src.kind != sourceV4L2 || src.path != "/dev/video89" {
		t.Fatalf("resolveSource(89) = %+v, %v; want /dev/video89", src, err)
	}
	src, err = svc.resolveSource(usbthermal.IDBandStart)
	if err != nil || src.kind != sourceUSBThermal || src.key != usbThermalHubKeyPrefix+testThermalStableID {
		t.Fatalf("resolveSource(88) = %+v, %v; want the thermal camera", src, err)
	}
	svc.usbThermal = nil
	if src, err := svc.resolveSource(usbthermal.IDBandStart); err != nil || src.kind != sourceV4L2 {
		t.Fatalf("with no driver, resolveSource(88) = %+v, %v", src, err)
	}
}

func TestStreamDeviceIDResolvesUSBThermalStableID(t *testing.T) {
	svc, _ := newUSBThermalTestService(t)
	id, err := svc.streamDeviceID(&agentpb.StreamVideoRequest{StableId: " " + testThermalStableID + " "})
	if err != nil || id != usbthermal.IDBandStart {
		t.Fatalf("streamDeviceID = %d, %v", id, err)
	}
	_, err = svc.streamDeviceID(&agentpb.StreamVideoRequest{StableId: "by-id:nope"})
	if status.Code(err) != codes.NotFound || !strings.Contains(err.Error(), testThermalStableID) {
		t.Fatalf("an unknown stable id: %v; want NotFound listing the thermal camera's id", err)
	}
}

// TestCameraControlsRefuseUSBThermal: the camera has no V4L2 controls, and its
// id must not be read as /dev/video88.
func TestCameraControlsRefuseUSBThermal(t *testing.T) {
	svc, _ := newUSBThermalTestService(t)
	ctx := context.Background()
	id := uint32(usbthermal.IDBandStart)
	_, getErr := svc.GetCameraControls(ctx, &agentpb.GetCameraControlsRequest{DeviceId: id})
	_, setErr := svc.SetCameraControls(ctx, &agentpb.SetCameraControlsRequest{DeviceId: id,
		Controls: []*agentpb.CameraControlSetting{{Name: "gain", Value: 1}}, Persist: true})
	_, resetErr := svc.ResetCameraControls(ctx, &agentpb.ResetCameraControlsRequest{DeviceId: id})
	for name, err := range map[string]error{"get": getErr, "set": setErr, "reset": resetErr} {
		if status.Code(err) != codes.InvalidArgument || !strings.Contains(err.Error(), "no V4L2 controls") {
			t.Errorf("%s: %v, want InvalidArgument naming the reason", name, err)
		}
	}
}
