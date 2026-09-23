package litevideo

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/proto/gen/sensorlinkpb"
)

// fakeClient stands in for the WendyCom client. It delivers frames by calling
// the listener straight from the test goroutine, which is exactly the contract
// the real client offers: one frame at a time, on its read loop, with no
// goroutine hop. A fake that spawned goroutines would be testing a promise
// liteclient does not make.
type fakeClient struct {
	manifest     *sensorlinkpb.SensorManifest
	manifestErr  error
	subscribeErr error

	listener func(*sensorlinkpb.SensorFrame)
	calls    []string
}

func (f *fakeClient) GetSensorManifest(time.Duration) (*sensorlinkpb.SensorManifest, error) {
	f.calls = append(f.calls, "manifest")
	return f.manifest, f.manifestErr
}

func (f *fakeClient) SensorLinkSubscribe(ids []uint32, _ time.Duration) error {
	f.calls = append(f.calls, fmt.Sprintf("subscribe:%v", ids))
	return f.subscribeErr
}

func (f *fakeClient) SensorLinkUnsubscribe(ids []uint32, _ time.Duration) error {
	f.calls = append(f.calls, fmt.Sprintf("unsubscribe:%v", ids))
	return nil
}

func (f *fakeClient) AddSensorFrameListener(fn func(*sensorlinkpb.SensorFrame)) func() {
	f.calls = append(f.calls, "listen")
	f.listener = fn
	return func() {
		f.calls = append(f.calls, "unregister")
		f.listener = nil
	}
}

func (f *fakeClient) Close() error {
	f.calls = append(f.calls, "close")
	return nil
}

// send delivers one frame the way the read loop would.
func (f *fakeClient) send(channel uint32, seq uint32, keyframe bool, payload string) {
	var flags uint32
	if keyframe {
		flags = flagKeyframe
	}
	f.listener(&sensorlinkpb.SensorFrame{
		ChannelId: channel,
		Seq:       seq,
		Flags:     flags,
		Payload:   []byte(payload),
	})
}

func camera(id uint32, name string, codec sensorlinkpb.VideoFormat_Codec) *sensorlinkpb.SensorDescriptor {
	return &sensorlinkpb.SensorDescriptor{
		ChannelId: id,
		Kind:      sensorlinkpb.SensorDescriptor_CAMERA,
		Name:      name,
		Format: &sensorlinkpb.SensorDescriptor_Video{
			Video: &sensorlinkpb.VideoFormat{Codec: codec, Width: 640, Height: 480, Fps: 15},
		},
	}
}

func nonCamera(id uint32, name string, kind sensorlinkpb.SensorDescriptor_Kind) *sensorlinkpb.SensorDescriptor {
	return &sensorlinkpb.SensorDescriptor{ChannelId: id, Kind: kind, Name: name}
}

func manifestOf(sensors ...*sensorlinkpb.SensorDescriptor) *sensorlinkpb.SensorManifest {
	return &sensorlinkpb.SensorManifest{Sensors: sensors}
}

func TestOpenPicksTheFirstCameraAndSubscribesToOnlyIt(t *testing.T) {
	client := &fakeClient{manifest: manifestOf(
		nonCamera(1, "mic0", sensorlinkpb.SensorDescriptor_MICROPHONE),
		camera(7, "front", sensorlinkpb.VideoFormat_MJPEG),
		camera(9, "rear", sensorlinkpb.VideoFormat_MJPEG),
	)}

	src, err := Open(context.Background(), client, Options{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer src.Close() //nolint:errcheck

	if got := src.Channel().GetChannelId(); got != 7 {
		t.Errorf("channel = %d, want the first camera (7)", got)
	}
	if got := src.Cameras(); got != 2 {
		t.Errorf("Cameras() = %d, want 2", got)
	}
	if got := src.Format().GetWidth(); got != 640 {
		t.Errorf("Format().Width = %d, want 640", got)
	}
	// Only the chosen channel: subscribing to the rear camera too would make
	// the device encode a stream nothing reads.
	if !slices.Contains(client.calls, "subscribe:[7]") {
		t.Errorf("calls = %v, want a subscribe to [7] alone", client.calls)
	}
	// Listening has to precede subscribing, or a fast device's first frames
	// land with nothing registered to catch them.
	listen, sub := slices.Index(client.calls, "listen"), slices.Index(client.calls, "subscribe:[7]")
	if listen < 0 || sub < 0 || listen > sub {
		t.Errorf("calls = %v, want listen before subscribe", client.calls)
	}
}

func TestOpenWithoutACameraNamesWhatTheDeviceHasAndClosesTheClient(t *testing.T) {
	client := &fakeClient{manifest: manifestOf(
		nonCamera(1, "mic0", sensorlinkpb.SensorDescriptor_MICROPHONE),
		nonCamera(2, "imu", sensorlinkpb.SensorDescriptor_SENSOR),
	)}

	_, err := Open(context.Background(), client, Options{})
	if err == nil {
		t.Fatal("Open succeeded on a manifest with no camera")
	}
	var noCam *NoCameraError
	if !errors.As(err, &noCam) {
		t.Fatalf("error = %v, want a *NoCameraError", err)
	}
	for _, want := range []string{`microphone "mic0"`, `sensor "imu"`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to mention %s", err, want)
		}
	}
	// The leak test: a failed Open that left the client open would hold the
	// board's serial port until the process exits.
	if !slices.Contains(client.calls, "close") {
		t.Errorf("calls = %v, want the client closed on the error path", client.calls)
	}
}

func TestOpenClosesTheClientWhenSubscribeFails(t *testing.T) {
	client := &fakeClient{
		manifest:     manifestOf(camera(7, "front", sensorlinkpb.VideoFormat_MJPEG)),
		subscribeErr: errors.New("device said no"),
	}

	if _, err := Open(context.Background(), client, Options{}); err == nil {
		t.Fatal("Open succeeded despite a failed subscribe")
	}
	if !slices.Contains(client.calls, "unregister") {
		t.Errorf("calls = %v, want the listener unregistered", client.calls)
	}
	if !slices.Contains(client.calls, "close") {
		t.Errorf("calls = %v, want the client closed", client.calls)
	}
}

// A full buffer is the read loop's only option, so what matters is what
// happens next: an H.264 decoder handed the frames that follow a gap produces
// smear, so delivery must not resume until a keyframe.
func TestH264DropsThenWaitsForAKeyframe(t *testing.T) {
	client := &fakeClient{manifest: manifestOf(camera(7, "front", sensorlinkpb.VideoFormat_H264))}
	src, err := Open(context.Background(), client, Options{Buffer: 1})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer src.Close() //nolint:errcheck

	client.send(7, 1, true, "K1")  // buffered
	client.send(7, 2, false, "P2") // buffer full — dropped, resync armed
	client.send(7, 3, false, "P3") // suppressed: not a keyframe

	first := recvNow(t, src)
	if string(first.Data) != "K1" {
		t.Fatalf("first frame = %q, want K1", first.Data)
	}
	client.send(7, 4, true, "K4") // buffer drained and a keyframe: delivered

	second := recvNow(t, src)
	if string(second.Data) != "K4" {
		t.Errorf("second frame = %q, want K4 — never a frame that follows a gap", second.Data)
	}
	if !second.Keyframe {
		t.Error("K4.Keyframe = false, want the flag carried through")
	}
	if got := src.Dropped(); got != 2 {
		t.Errorf("Dropped() = %d, want 2 (P2 and P3)", got)
	}
}

// MJPEG needs no resync: every JPEG is independent, so the frame after a drop
// is the freshest picture available and holding it back would only add latency.
func TestMJPEGDeliversTheFrameAfterADrop(t *testing.T) {
	client := &fakeClient{manifest: manifestOf(camera(7, "front", sensorlinkpb.VideoFormat_MJPEG))}
	src, err := Open(context.Background(), client, Options{Buffer: 1})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer src.Close() //nolint:errcheck

	client.send(7, 1, false, "J1") // buffered
	client.send(7, 2, false, "J2") // dropped

	if got := recvNow(t, src); string(got.Data) != "J1" {
		t.Fatalf("first frame = %q, want J1", got.Data)
	}
	client.send(7, 3, false, "J3")
	if got := recvNow(t, src); string(got.Data) != "J3" {
		t.Errorf("frame after a drop = %q, want J3 delivered without waiting for a keyframe", got.Data)
	}
	if got := src.Dropped(); got != 1 {
		t.Errorf("Dropped() = %d, want 1", got)
	}
}

func TestFramesFromAnotherChannelAreIgnored(t *testing.T) {
	client := &fakeClient{manifest: manifestOf(
		camera(7, "front", sensorlinkpb.VideoFormat_MJPEG),
		camera(9, "rear", sensorlinkpb.VideoFormat_MJPEG),
	)}
	src, err := Open(context.Background(), client, Options{Buffer: 2})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer src.Close() //nolint:errcheck

	client.send(9, 1, false, "rear")
	client.send(7, 2, false, "front")

	if got := recvNow(t, src); string(got.Data) != "front" {
		t.Errorf("frame = %q, want only the subscribed channel's", got.Data)
	}
	// A frame from a channel we never asked for is not this Source falling
	// behind, so it must not show up as a drop.
	if got := src.Dropped(); got != 0 {
		t.Errorf("Dropped() = %d, want 0", got)
	}
}

func TestCloseUnsubscribesBeforeClosingTheClient(t *testing.T) {
	client := &fakeClient{manifest: manifestOf(camera(9, "front", sensorlinkpb.VideoFormat_MJPEG))}
	src, err := Open(context.Background(), client, Options{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	before := len(client.calls)
	if err := src.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// The unsubscribe waits for a device reply, which only arrives while the
	// read loop is alive — so it cannot come after the client closes.
	want := []string{"unregister", "unsubscribe:[9]", "close"}
	if got := client.calls[before:]; !slices.Equal(got, want) {
		t.Errorf("teardown = %v, want %v", got, want)
	}

	before = len(client.calls)
	if err := src.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if got := client.calls[before:]; len(got) != 0 {
		t.Errorf("second Close made %v, want nothing", got)
	}
}

func TestRecvAfterCloseReportsClosed(t *testing.T) {
	client := &fakeClient{manifest: manifestOf(camera(7, "front", sensorlinkpb.VideoFormat_MJPEG))}
	src, err := Open(context.Background(), client, Options{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := src.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := src.Recv(context.Background()); !errors.Is(err, ErrClosed) {
		t.Errorf("Recv after Close = %v, want ErrClosed", err)
	}
}

// Unregistering only swaps the client's published listener slice, so a
// dispatch that already loaded the old one still calls onFrame afterwards.
// That is why the frame channel is never closed — a send on a closed channel
// here would panic the read loop. Run under -race.
func TestAFrameDeliveredAfterCloseDoesNotPanic(t *testing.T) {
	client := &fakeClient{manifest: manifestOf(camera(7, "front", sensorlinkpb.VideoFormat_MJPEG))}
	src, err := Open(context.Background(), client, Options{Buffer: 1})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	inFlight := client.listener
	if err := src.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	inFlight(&sensorlinkpb.SensorFrame{ChannelId: 7, Seq: 1, Payload: []byte("late")})
	inFlight(&sensorlinkpb.SensorFrame{ChannelId: 7, Seq: 2, Payload: []byte("later")})
}

// A dead link never reaches the listener: the client's read loop exits without
// telling it anything. Without this timeout, Recv would block on a frozen
// picture for as long as the operator was willing to stare at it.
func TestRecvGivesUpWhenTheDeviceGoesQuiet(t *testing.T) {
	client := &fakeClient{manifest: manifestOf(camera(7, "front", sensorlinkpb.VideoFormat_MJPEG))}
	src, err := Open(context.Background(), client, Options{Idle: 20 * time.Millisecond})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer src.Close() //nolint:errcheck

	_, err = src.Recv(context.Background())
	if err == nil {
		t.Fatal("Recv returned no error on a silent device")
	}
	for _, want := range []string{"channel 7", `"front"`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to name %s", err, want)
		}
	}
}

func TestRecvHonoursContextCancellation(t *testing.T) {
	client := &fakeClient{manifest: manifestOf(camera(7, "front", sensorlinkpb.VideoFormat_MJPEG))}
	src, err := Open(context.Background(), client, Options{Idle: -1})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer src.Close() //nolint:errcheck

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := src.Recv(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("Recv = %v, want context.Canceled", err)
	}
}

func recvNow(t *testing.T, src *Source) Frame {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	f, err := src.Recv(ctx)
	if err != nil {
		t.Fatalf("Recv: %v", err)
	}
	return f
}
