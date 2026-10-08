package commands

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/sensorlink"
	"github.com/wendylabsinc/wendy/go/internal/cli/providers"
	"github.com/wendylabsinc/wendy/go/internal/shared/models"
	sensorlinkpb "github.com/wendylabsinc/wendy/go/proto/gen/sensorlinkpb"
)

type fakeLiteCameraClient struct {
	mu           sync.Mutex
	manifest     *sensorlinkpb.SensorManifest
	manifestErr  error
	subscribeErr error
	onSubscribe  func()
	listener     func(*sensorlinkpb.SensorData)
	calls        []string
	subscribed   []uint32
	unsubscribed []uint32
	done         chan struct{}
	closeOnce    sync.Once
}

func liteVideoChannel(id uint32, codec sensorlinkpb.VideoFormat_Codec) *sensorlinkpb.SensorDescriptor {
	return &sensorlinkpb.SensorDescriptor{
		ChannelId: id, Name: "camera", InputId: 1,
		Format: &sensorlinkpb.SensorDescriptor_Video{Video: &sensorlinkpb.VideoFormat{
			Codec: codec, Width: 640, Height: 480, Fps: 15,
		}},
	}
}

func fakeLiteCamera() *fakeLiteCameraClient {
	return &fakeLiteCameraClient{
		manifest: &sensorlinkpb.SensorManifest{Sensors: []*sensorlinkpb.SensorDescriptor{
			{ChannelId: 1, Name: "mic", Format: &sensorlinkpb.SensorDescriptor_Audio{Audio: &sensorlinkpb.AudioFormat{}}},
			liteVideoChannel(4, sensorlinkpb.VideoFormat_MJPEG),
		}},
		done: make(chan struct{}),
	}
}

func (c *fakeLiteCameraClient) GetSensorManifest(time.Duration) (*sensorlinkpb.SensorManifest, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, "manifest")
	return c.manifest, c.manifestErr
}

func (c *fakeLiteCameraClient) SensorLinkSubscribe(ids []uint32, timeout time.Duration) error {
	c.mu.Lock()
	c.calls = append(c.calls, "subscribe")
	c.subscribed = ids
	c.mu.Unlock()
	if c.onSubscribe != nil {
		c.onSubscribe()
	}
	return c.subscribeErr
}

func (c *fakeLiteCameraClient) SensorLinkUnsubscribe(ids []uint32, timeout time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, "unsubscribe")
	c.unsubscribed = ids
	return nil
}

func (c *fakeLiteCameraClient) AddSensorDataListener(fn func(*sensorlinkpb.SensorData)) func() {
	c.mu.Lock()
	c.calls = append(c.calls, "listen")
	c.listener = fn
	c.mu.Unlock()
	return func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.calls = append(c.calls, "remove")
		c.listener = nil
	}
}

func (c *fakeLiteCameraClient) emit(d *sensorlinkpb.SensorData) {
	c.mu.Lock()
	fn := c.listener
	c.mu.Unlock()
	if fn != nil {
		fn(d)
	}
}

func (c *fakeLiteCameraClient) Done() <-chan struct{} { return c.done }

func (c *fakeLiteCameraClient) Close() error {
	c.closeOnce.Do(func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.calls = append(c.calls, "close")
		close(c.done)
	})
	return nil
}

func stubLiteCameraTarget(t *testing.T, c *fakeLiteCameraClient) {
	t.Helper()
	oldResolve, oldConnect := resolveCameraTargetFn, connectLiteCameraFn
	resolveCameraTargetFn = func(context.Context, ...resolveOption) (*SelectedDevice, error) {
		return &SelectedDevice{
			External: &models.ExternalDevice{ProviderKey: "wendy-lite", DisplayName: "esp32-camera"},
			Provider: &providers.MicroWendyProvider{},
		}, nil
	}
	connectLiteCameraFn = func(*SelectedDevice) (liteCameraClient, error) { return c, nil }
	t.Cleanup(func() { resolveCameraTargetFn, connectLiteCameraFn = oldResolve, oldConnect })
}

type stopAfterFrame struct {
	bytes.Buffer
	err error
}

func (w *stopAfterFrame) Write(p []byte) (int, error) {
	n, _ := w.Buffer.Write(p)
	return n, w.err
}

func TestLiteCameraViewAndWatch(t *testing.T) {
	for _, name := range []string{"view", "watch"} {
		t.Run(name, func(t *testing.T) {
			c := fakeLiteCamera()
			stubLiteCameraTarget(t, c)
			// Data arrives before the subscribe reply, including another channel.
			c.onSubscribe = func() {
				c.emit(&sensorlinkpb.SensorData{ChannelId: 1, Flags: 3, Payload: []byte("audio")})
				c.emit(&sensorlinkpb.SensorData{ChannelId: 4, FrameSeq: 9, Flags: 1, Payload: []byte("jpeg-")})
				c.emit(&sensorlinkpb.SensorData{ChannelId: 4, FrameSeq: 9, ChunkSeq: 1, Flags: 3, Payload: []byte("frame")})
			}
			stop := errors.New("output closed")
			out := &stopAfterFrame{err: stop}
			cmd := newCameraStreamCmd(name, name == "watch")
			cmd.SetOut(out)
			cmd.SetErr(io.Discard)
			cmd.SilenceErrors, cmd.SilenceUsage = true, true
			cmd.SetArgs([]string{"--stdout", "--non-interactive"})
			if err := cmd.ExecuteContext(context.Background()); !errors.Is(err, stop) {
				t.Fatalf("error = %v", err)
			}
			if out.String() != "jpeg-frame" {
				t.Fatalf("stdout = %q", out.String())
			}
			if !reflect.DeepEqual(c.subscribed, []uint32{4}) || !reflect.DeepEqual(c.unsubscribed, []uint32{4}) {
				t.Fatalf("subscribe = %v, unsubscribe = %v", c.subscribed, c.unsubscribed)
			}
			want := []string{"manifest", "listen", "subscribe", "remove", "unsubscribe", "close"}
			if !reflect.DeepEqual(c.calls, want) {
				t.Fatalf("calls = %v, want %v", c.calls, want)
			}
		})
	}
}

func TestLiteCameraList(t *testing.T) {
	for _, asJSON := range []bool{false, true} {
		t.Run(map[bool]string{false: "table", true: "json"}[asJSON], func(t *testing.T) {
			c := fakeLiteCamera()
			stubLiteCameraTarget(t, c)
			oldJSON := jsonOutput
			jsonOutput = asJSON
			t.Cleanup(func() { jsonOutput = oldJSON })
			var out bytes.Buffer
			cmd := newCameraListCmd()
			cmd.SetOut(&out)
			cmd.SetArgs([]string{"--refresh"})
			if err := cmd.ExecuteContext(context.Background()); err != nil {
				t.Fatal(err)
			}
			if asJSON {
				// Inspect the stable channel/input identifiers without unmarshaling a proto oneof.
				var rows []map[string]any
				if err := json.Unmarshal(out.Bytes(), &rows); err != nil || len(rows) != 1 || rows[0]["channel_id"] != float64(4) || rows[0]["input_id"] != float64(1) {
					t.Fatalf("JSON = %s, err = %v", out.String(), err)
				}
			} else if !strings.Contains(out.String(), "MJPEG") || !strings.Contains(out.String(), "640x480") || strings.Contains(out.String(), "mic") {
				t.Fatalf("table = %s", out.String())
			}
			if !reflect.DeepEqual(c.calls, []string{"manifest", "close"}) {
				t.Fatalf("calls = %v", c.calls)
			}
		})
	}
}

func TestLiteCameraSelection(t *testing.T) {
	for _, tc := range []struct {
		name     string
		channels []*sensorlinkpb.SensorDescriptor
		opts     liteCameraOptions
		want     uint32
		err      string
	}{
		{name: "empty", err: "no cameras"},
		{name: "automatic", channels: []*sensorlinkpb.SensorDescriptor{liteVideoChannel(8, sensorlinkpb.VideoFormat_H264)}, want: 8},
		{name: "explicit zero", channels: []*sensorlinkpb.SensorDescriptor{liteVideoChannel(0, sensorlinkpb.VideoFormat_MJPEG), liteVideoChannel(8, sensorlinkpb.VideoFormat_H264)}, opts: liteCameraOptions{idSet: true}, want: 0},
		{name: "ambiguous", channels: []*sensorlinkpb.SensorDescriptor{liteVideoChannel(0, sensorlinkpb.VideoFormat_MJPEG), liteVideoChannel(8, sensorlinkpb.VideoFormat_H264)}, err: "multiple cameras"},
		{name: "unknown ID", channels: []*sensorlinkpb.SensorDescriptor{liteVideoChannel(8, sensorlinkpb.VideoFormat_H264)}, opts: liteCameraOptions{idSet: true, id: 1}, err: "no SensorLink video channel matches"},
		{name: "matching format", channels: []*sensorlinkpb.SensorDescriptor{liteVideoChannel(8, sensorlinkpb.VideoFormat_H264)}, opts: liteCameraOptions{width: 640, height: 480, fps: 15}, want: 8},
		{name: "unsupported width", channels: []*sensorlinkpb.SensorDescriptor{liteVideoChannel(8, sensorlinkpb.VideoFormat_H264)}, opts: liteCameraOptions{width: 1280}, err: "no SensorLink video channel matches"},
		{name: "unsupported codec", channels: []*sensorlinkpb.SensorDescriptor{liteVideoChannel(8, sensorlinkpb.VideoFormat_CODEC_UNSPECIFIED)}, err: "unsupported video codec"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ch, err := selectLiteCamera(&sensorlinkpb.SensorManifest{Sensors: tc.channels}, tc.opts, nil)
			if tc.err != "" {
				if err == nil || !strings.Contains(err.Error(), tc.err) {
					t.Fatalf("error = %v", err)
				}
			} else if err != nil || ch.GetChannelId() != tc.want {
				t.Fatalf("channel = %v, error = %v", ch, err)
			}
		})
	}
}

func TestLiteCameraFailuresCloseConnection(t *testing.T) {
	for _, tc := range []struct {
		name                      string
		args                      []string
		manifestErr, subscribeErr error
		want                      string
	}{
		{name: "raw", args: []string{"--raw"}, want: "--raw is unavailable"},
		{name: "stable ID", args: []string{"--stable-id", "usb-front"}, want: "use --id"},
		{name: "manifest", manifestErr: errors.New("manifest rejected"), want: "manifest rejected"},
		{name: "subscribe", subscribeErr: errors.New("camera busy"), want: "camera busy"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := fakeLiteCamera()
			c.manifestErr, c.subscribeErr = tc.manifestErr, tc.subscribeErr
			stubLiteCameraTarget(t, c)
			cmd := newCameraViewCmd()
			cmd.SetOut(io.Discard)
			cmd.SetErr(io.Discard)
			cmd.SetArgs(append([]string{"--stdout", "--non-interactive"}, tc.args...))
			if err := cmd.ExecuteContext(context.Background()); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v", err)
			}
			if len(tc.args) == 0 && c.calls[len(c.calls)-1] != "close" {
				t.Fatalf("connection leaked: %v", c.calls)
			}
			if c.listener != nil {
				t.Fatal("listener leaked")
			}
		})
	}
}

func TestLiteCameraStreamDropsIncompleteFrames(t *testing.T) {
	c := fakeLiteCamera()
	s, err := subscribeLiteCamera(context.Background(), c, liteVideoChannel(4, sensorlinkpb.VideoFormat_MJPEG))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	c.emit(&sensorlinkpb.SensorData{ChannelId: 4, FrameSeq: 1, Payload: []byte("missing tail")})
	c.emit(&sensorlinkpb.SensorData{ChannelId: 4, FrameSeq: 1, ChunkSeq: 2, Flags: 3, Payload: []byte("gap")})
	c.emit(&sensorlinkpb.SensorData{ChannelId: 4, FrameSeq: 2, Flags: 3, Payload: []byte("complete")})
	f, err := s.Recv()
	if err != nil || string(f.Payload) != "complete" {
		t.Fatalf("frame = %v, err = %v", f, err)
	}
	if len(s.frames) != 0 {
		t.Fatal("incomplete frame was queued")
	}
}

func TestLiteCameraBackpressureAndH264Recovery(t *testing.T) {
	c := fakeLiteCamera()
	s, err := subscribeLiteCamera(context.Background(), c, liteVideoChannel(4, sensorlinkpb.VideoFormat_H264))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	send := func(seq uint32, key bool) {
		flags := sensorlink.FlagLastChunk
		if key {
			flags |= sensorlink.FlagKeyframe
		}
		c.emit(&sensorlinkpb.SensorData{ChannelId: 4, FrameSeq: seq, Flags: flags, Payload: []byte{byte(seq)}})
	}
	finished := make(chan struct{})
	go func() {
		for i := uint32(0); i < 100; i++ {
			send(i, i == 0)
		}
		close(finished)
	}()
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("full queue blocked WendyCom")
	}
	for len(s.frames) > 0 {
		<-s.frames
	}
	send(100, false)
	if len(s.frames) != 0 {
		t.Fatal("dependent H.264 frame queued after overflow")
	}
	send(101, true)
	f, err := s.Recv()
	if err != nil || f.Seq != 101 {
		t.Fatalf("recovery = %v, %v", f, err)
	}
	// A missing frame must also suppress dependent frames.
	send(103, false)
	if len(s.frames) != 0 {
		t.Fatal("dependent H.264 frame queued after sequence gap")
	}
	send(104, true)
	if f, err := s.Recv(); err != nil || f.Seq != 104 {
		t.Fatalf("recovery = %v, %v", f, err)
	}
}

func TestLiteCameraStreamCancellationDisconnectAndLateDelivery(t *testing.T) {
	for _, disconnect := range []bool{false, true} {
		c := fakeLiteCamera()
		ctx, cancel := context.WithCancel(context.Background())
		s, err := subscribeLiteCamera(ctx, c, liteVideoChannel(4, sensorlinkpb.VideoFormat_MJPEG))
		if err != nil {
			t.Fatal(err)
		}
		late := c.listener
		if disconnect {
			c.Close()
		} else {
			cancel()
		}
		result := make(chan error, 1)
		go func() { _, err := s.Recv(); result <- err }()
		select {
		case err := <-result:
			want := error(context.Canceled)
			if disconnect {
				want = io.ErrUnexpectedEOF
			}
			if !errors.Is(err, want) {
				t.Fatalf("error = %v, want %v", err, want)
			}
		case <-time.After(time.Second):
			t.Fatal("Recv did not stop")
		}
		s.Close()
		s.Close()
		late(&sensorlinkpb.SensorData{ChannelId: 4, Flags: 3, Payload: []byte("late")})
		if len(s.frames) != 0 {
			t.Fatal("late callback queued a frame after close")
		}
		cancel()
	}
}

func TestLiteCameraCancelledDialClosesLateConnection(t *testing.T) {
	c := fakeLiteCamera()
	old := connectLiteCameraFn
	t.Cleanup(func() { connectLiteCameraFn = old })
	started, release := make(chan struct{}), make(chan struct{})
	connectLiteCameraFn = func(*SelectedDevice) (liteCameraClient, error) {
		close(started)
		<-release
		return c, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() { _, _, err := openLiteCamera(ctx, &SelectedDevice{}); result <- err }()
	<-started
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v", err)
	}
	close(release)
	select {
	case <-c.done:
	case <-time.After(time.Second):
		t.Fatal("late connection leaked")
	}
}

func TestLiteCameraCancelBeforeFirstFrame(t *testing.T) {
	c := fakeLiteCamera()
	stubLiteCameraTarget(t, c)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.onSubscribe = cancel
	cmd := newCameraViewCmd()
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"--non-interactive"})
	if err := cmd.ExecuteContext(ctx); err != nil {
		t.Fatalf("cancelled playback: %v", err)
	}
	if c.listener != nil {
		t.Fatal("listener leaked")
	}
	select {
	case <-c.done:
	default:
		t.Fatal("connection leaked")
	}
}

func TestLiteCameraConcurrentDeliveryAndClose(t *testing.T) {
	c := fakeLiteCamera()
	s, err := subscribeLiteCamera(context.Background(), c, liteVideoChannel(4, sensorlinkpb.VideoFormat_MJPEG))
	if err != nil {
		t.Fatal(err)
	}
	late := c.listener
	started, finished := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(finished)
		close(started)
		for i := uint32(0); i < 1000; i++ {
			late(&sensorlinkpb.SensorData{ChannelId: 4, FrameSeq: i, Flags: 3, Payload: []byte("jpeg")})
		}
	}()
	<-started
	s.Close()
	<-finished
	if !reflect.DeepEqual(c.unsubscribed, []uint32{4}) {
		t.Fatalf("unsubscribe = %v", c.unsubscribed)
	}
}

func TestLiteCameraMJPEGPipeline(t *testing.T) {
	pipeline := strings.Join(liteCameraPipeline(sensorlinkpb.VideoFormat_MJPEG), " ")
	if !strings.Contains(pipeline, "jpegparse") || !strings.Contains(pipeline, "jpegdec") || strings.Contains(pipeline, "h264") {
		t.Fatalf("pipeline = %s", pipeline)
	}
}
