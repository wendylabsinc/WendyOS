package lite

import (
	"context"
	"errors"
	sensorlinkpb "github.com/wendylabsinc/WendyOS/go/wendy-core/core/lite/pb"
	"io"
	"sync"
	"testing"
	"time"
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

func liteVideoChannel(ID uint32, codec sensorlinkpb.VideoFormat_Codec) *sensorlinkpb.SensorDescriptor {
	return &sensorlinkpb.SensorDescriptor{
		ChannelId: ID, Name: "camera", InputId: 1,
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

func testLiteAudioChannel(ID uint32) *sensorlinkpb.SensorDescriptor {
	return &sensorlinkpb.SensorDescriptor{ChannelId: ID, Name: "microphone", Format: &sensorlinkpb.SensorDescriptor_Audio{Audio: &sensorlinkpb.AudioFormat{Codec: sensorlinkpb.AudioFormat_PCM_S16LE, SampleRate: 16000, Channels: 1}}}
}

func TestSelectLiteAudio(t *testing.T) {
	unsupported := testLiteAudioChannel(2)
	unsupported.GetAudio().Codec = sensorlinkpb.AudioFormat_OPUS
	zeroRate := testLiteAudioChannel(3)
	zeroRate.GetAudio().SampleRate = 0
	manifest := &sensorlinkpb.SensorManifest{Sensors: []*sensorlinkpb.SensorDescriptor{liteVideoChannel(4, sensorlinkpb.VideoFormat_MJPEG), unsupported, zeroRate, testLiteAudioChannel(0), testLiteAudioChannel(1)}}
	for _, tc := range []struct {
		name string
		opts AudioOptions
		want uint32
		fail bool
	}{
		{name: "auto skips video and unsupported", want: 0},
		{name: "explicit zero", opts: AudioOptions{IDSet: true, ID: 0}, want: 0},
		{name: "explicit microphone", opts: AudioOptions{IDSet: true, ID: 1}, want: 1},
		{name: "unsupported codec", opts: AudioOptions{IDSet: true, ID: 2}, fail: true},
		{name: "invalid rate", opts: AudioOptions{IDSet: true, ID: 3}, fail: true},
		{name: "missing channel", opts: AudioOptions{IDSet: true, ID: 9}, fail: true},
		{name: "rate mismatch", opts: AudioOptions{RateSet: true, SampleRate: 48000}, fail: true},
		{name: "Channels mismatch", opts: AudioOptions{ChannelsSet: true, Channels: 2}, fail: true},
		{name: "native rate by default", opts: AudioOptions{SampleRate: 48000}, want: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ch, err := SelectAudio(manifest, tc.opts)
			if tc.fail {
				if err == nil {
					t.Fatal("expected format/selection error")
				}
				return
			}
			if err != nil || ch.GetChannelId() != tc.want {
				t.Fatalf("channel=%v error=%v", ch, err)
			}
		})
	}
}

func TestLiteAudioAssemblyAndBoundedQueue(t *testing.T) {
	c := fakeLiteCamera()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s, err := SubscribeAudio(ctx, c, testLiteAudioChannel(1))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	// A missing chunk and an incomplete sample must not reach playback.
	c.emit(&sensorlinkpb.SensorData{ChannelId: 1, FrameSeq: 1, Payload: []byte{1, 2}})
	c.emit(&sensorlinkpb.SensorData{ChannelId: 1, FrameSeq: 1, ChunkSeq: 2, Flags: 2, Payload: []byte{3, 4}})
	c.emit(&sensorlinkpb.SensorData{ChannelId: 1, FrameSeq: 2, Flags: 2, Payload: []byte{1}})
	if len(s.frames) != 0 {
		t.Fatal("malformed frame delivered")
	}
	for i := uint32(3); i < 15; i++ {
		c.emit(&sensorlinkpb.SensorData{ChannelId: 1, FrameSeq: i, Flags: 2, Payload: []byte{byte(i), 0}})
	}
	if len(s.frames) != 8 {
		t.Fatalf("queue depth=%d", len(s.frames))
	}
	chunk, err := s.Recv()
	if err != nil || chunk.PcmData[0] != 7 || chunk.SampleRate != 16000 || chunk.Channels != 1 {
		t.Fatalf("chunk=%v err=%v", chunk, err)
	}
}

func TestLiteAudioLifecycle(t *testing.T) {
	t.Run("cancel", func(t *testing.T) {
		c := fakeLiteCamera()
		ctx, cancel := context.WithCancel(context.Background())
		s, err := SubscribeAudio(ctx, c, testLiteAudioChannel(1))
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		cancel()
		if _, err := s.Recv(); !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	})
	t.Run("disconnect", func(t *testing.T) {
		c := fakeLiteCamera()
		s, err := SubscribeAudio(context.Background(), c, testLiteAudioChannel(1))
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		c.Close()
		if _, err := s.Recv(); !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Fatal(err)
		}
	})
	t.Run("subscribe failure", func(t *testing.T) {
		c := fakeLiteCamera()
		want := errors.New("busy")
		c.subscribeErr = want
		_, err := SubscribeAudio(context.Background(), c, testLiteAudioChannel(1))
		if !errors.Is(err, want) || c.listener != nil {
			t.Fatalf("error=%v listener retained=%v", err, c.listener != nil)
		}
	})
	t.Run("late callback", func(t *testing.T) {
		c := fakeLiteCamera()
		s, err := SubscribeAudio(context.Background(), c, testLiteAudioChannel(1))
		if err != nil {
			t.Fatal(err)
		}
		late := c.listener
		s.Close()
		s.Close()
		late(&sensorlinkpb.SensorData{ChannelId: 1, Flags: 2, Payload: []byte{1, 2}})
		if len(s.frames) != 0 {
			t.Fatal("delivered after close")
		}
	})
}
