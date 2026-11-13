package commands

import (
	"bytes"
	"context"
	"errors"
	"io"
	"reflect"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/cli/providers"
	"github.com/wendylabsinc/wendy/go/internal/shared/models"
	sensorlinkpb "github.com/wendylabsinc/wendy/go/proto/gen/sensorlinkpb"
)

func testLiteAudioChannel(id uint32) *sensorlinkpb.SensorDescriptor {
	return &sensorlinkpb.SensorDescriptor{ChannelId: id, Name: "microphone", Format: &sensorlinkpb.SensorDescriptor_Audio{Audio: &sensorlinkpb.AudioFormat{Codec: sensorlinkpb.AudioFormat_PCM_S16LE, SampleRate: 16000, Channels: 1}}}
}

func TestSelectLiteAudio(t *testing.T) {
	unsupported := testLiteAudioChannel(2)
	unsupported.GetAudio().Codec = sensorlinkpb.AudioFormat_OPUS
	zeroRate := testLiteAudioChannel(3)
	zeroRate.GetAudio().SampleRate = 0
	manifest := &sensorlinkpb.SensorManifest{Sensors: []*sensorlinkpb.SensorDescriptor{liteVideoChannel(4, sensorlinkpb.VideoFormat_MJPEG), unsupported, zeroRate, testLiteAudioChannel(0), testLiteAudioChannel(1)}}
	for _, tc := range []struct {
		name string
		opts liteAudioOptions
		want uint32
		fail bool
	}{
		{name: "auto skips video and unsupported", want: 0},
		{name: "explicit zero", opts: liteAudioOptions{idSet: true, id: 0}, want: 0},
		{name: "explicit microphone", opts: liteAudioOptions{idSet: true, id: 1}, want: 1},
		{name: "unsupported codec", opts: liteAudioOptions{idSet: true, id: 2}, fail: true},
		{name: "invalid rate", opts: liteAudioOptions{idSet: true, id: 3}, fail: true},
		{name: "missing channel", opts: liteAudioOptions{idSet: true, id: 9}, fail: true},
		{name: "rate mismatch", opts: liteAudioOptions{rateSet: true, sampleRate: 48000}, fail: true},
		{name: "channels mismatch", opts: liteAudioOptions{channelsSet: true, channels: 2}, fail: true},
		{name: "native rate by default", opts: liteAudioOptions{sampleRate: 48000}, want: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ch, err := selectLiteAudio(manifest, tc.opts)
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

func stubLiteAudioTarget(t *testing.T, c *fakeLiteCameraClient) {
	t.Helper()
	prev, prevConnect := resolveAudioTargetFn, connectLiteCameraFn
	resolveAudioTargetFn = func(context.Context, ...resolveOption) (*SelectedDevice, error) {
		return &SelectedDevice{External: &models.ExternalDevice{ProviderKey: "wendy-lite"}, Provider: &providers.MicroWendyProvider{}}, nil
	}
	connectLiteCameraFn = func(*SelectedDevice) (liteCameraClient, error) { return c, nil }
	t.Cleanup(func() { resolveAudioTargetFn, connectLiteCameraFn = prev, prevConnect })
}

func TestLiteAudioListenCommand(t *testing.T) {
	c := fakeLiteCamera()
	c.manifest.Sensors = append(c.manifest.Sensors[1:], testLiteAudioChannel(1))
	stubLiteAudioTarget(t, c)
	c.onSubscribe = func() {
		c.emit(&sensorlinkpb.SensorData{ChannelId: 4, Flags: 2, Payload: []byte("video")})
		c.emit(&sensorlinkpb.SensorData{ChannelId: 1, FrameSeq: 5, Payload: []byte{1, 2}})
		c.emit(&sensorlinkpb.SensorData{ChannelId: 1, FrameSeq: 5, ChunkSeq: 1, Flags: 2, Payload: []byte{3, 4}})
	}
	stop := errors.New("output closed")
	out := &stopAfterFrame{err: stop}
	cmd := newAudioListenCmd()
	cmd.SilenceUsage = true
	cmd.SetOut(out)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"--stdout", "--non-interactive"})
	err := cmd.ExecuteContext(context.Background())
	if !errors.Is(err, stop) || !bytes.Equal(out.Bytes(), []byte{1, 2, 3, 4}) {
		t.Fatalf("output=%v error=%v", out.Bytes(), err)
	}
	if !reflect.DeepEqual(c.subscribed, []uint32{1}) || !reflect.DeepEqual(c.unsubscribed, []uint32{1}) {
		t.Fatalf("subscriptions=%v cleanup=%v", c.subscribed, c.unsubscribed)
	}
}

func TestLiteAudioListCommand(t *testing.T) {
	c := fakeLiteCamera()
	c.manifest.Sensors = append(c.manifest.Sensors[1:], testLiteAudioChannel(1))
	stubLiteAudioTarget(t, c)
	var out bytes.Buffer
	cmd := newAudioListCmd()
	cmd.SetOut(&out)
	cmd.SetErr(io.Discard)
	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(out.Bytes(), []byte("microphone")) || bytes.Contains(out.Bytes(), []byte("camera")) {
		t.Fatal(out.String())
	}
}

func TestLiteAudioAssemblyAndBoundedQueue(t *testing.T) {
	c := fakeLiteCamera()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s, err := subscribeLiteAudio(ctx, c, testLiteAudioChannel(1))
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
		s, err := subscribeLiteAudio(ctx, c, testLiteAudioChannel(1))
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
		s, err := subscribeLiteAudio(context.Background(), c, testLiteAudioChannel(1))
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
		_, err := subscribeLiteAudio(context.Background(), c, testLiteAudioChannel(1))
		if !errors.Is(err, want) || c.listener != nil {
			t.Fatalf("error=%v listener retained=%v", err, c.listener != nil)
		}
	})
	t.Run("late callback", func(t *testing.T) {
		c := fakeLiteCamera()
		s, err := subscribeLiteAudio(context.Background(), c, testLiteAudioChannel(1))
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
