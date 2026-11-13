package mcusource

import (
	"context"
	"fmt"
	"slices"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/agent/audioloop"
	"github.com/wendylabsinc/wendy/go/internal/agent/ros2camera"
	"github.com/wendylabsinc/wendy/go/internal/agent/sensorlink"
	sensorlinkpb "github.com/wendylabsinc/wendy/go/proto/gen/sensorlinkpb"
	"go.uber.org/zap"
)

// Mirror a Lite source's rejection of subscriptions that share one input.
// Deliver a frame for every accepted channel to verify the selected sinks too.
type selectionTransport struct {
	manifest  *sensorlinkpb.SensorManifest
	exclusive bool
	channels  []uint32
}

func (tr *selectionTransport) Close() error { return nil }
func (tr *selectionTransport) FetchManifest(context.Context) (*sensorlinkpb.SensorManifest, error) {
	return tr.manifest, nil
}
func (tr *selectionTransport) Stream(_ context.Context, channels []uint32) (<-chan *sensorlink.SensorFrame, func() error, error) {
	tr.channels = slices.Clone(channels)
	inputs := make(map[uint32]bool)
	frames := make(chan *sensorlink.SensorFrame, len(channels))
	for _, id := range channels {
		var descriptor *sensorlinkpb.SensorDescriptor
		for _, d := range tr.manifest.Sensors {
			if d.ChannelId == id {
				descriptor = d
				break
			}
		}
		if descriptor == nil {
			return nil, nil, fmt.Errorf("unknown channel %d", id)
		}
		if tr.exclusive && inputs[descriptor.InputId] {
			return nil, nil, fmt.Errorf("input %d already subscribed", descriptor.InputId)
		}
		inputs[descriptor.InputId] = true
		frames <- &sensorlink.SensorFrame{ChannelID: id, Payload: []byte("frame")}
	}
	close(frames)
	return frames, func() error { return nil }, nil
}

type selectionCamera struct {
	frames      int
	unavailable bool
}

func (c *selectionCamera) EnsureNode(context.Context, uint32, string) error {
	if c.unavailable {
		return fmt.Errorf("camera loopback unavailable")
	}
	return nil
}
func (*selectionCamera) NodePath(uint32) (string, bool)      { return "camera", true }
func (*selectionCamera) RemoveCamera(uint32)                 {}
func (c *selectionCamera) WriteFrame(ros2camera.Frame) error { c.frames++; return nil }
func (*selectionCamera) Close() error                        { return nil }

type selectionAudio struct{ frames int }

func (*selectionAudio) ReleaseSource(int32)                         {}
func (*selectionAudio) Allocate(int32, uint32, string) (int, error) { return 0, nil }
func (a *selectionAudio) OpenWriter(context.Context, int, audioloop.PCMFormat) (audioloop.AudioWriter, error) {
	return a, nil
}
func (a *selectionAudio) WritePCM([]byte) error { a.frames++; return nil }
func (*selectionAudio) Close() error            { return nil }

func TestSupervisorSelectsCompatibleChannels(t *testing.T) {
	camera := func(channel, input uint32, name string) *sensorlinkpb.SensorDescriptor {
		return &sensorlinkpb.SensorDescriptor{ChannelId: channel, InputId: input, Name: name,
			Format: &sensorlinkpb.SensorDescriptor_Video{Video: &sensorlinkpb.VideoFormat{Codec: sensorlinkpb.VideoFormat_MJPEG}}}
	}
	microphone := func(channel, input uint32, name string) *sensorlinkpb.SensorDescriptor {
		return &sensorlinkpb.SensorDescriptor{ChannelId: channel, InputId: input, Name: name,
			Format: &sensorlinkpb.SensorDescriptor_Audio{Audio: &sensorlinkpb.AudioFormat{Codec: sensorlinkpb.AudioFormat_PCM_S16LE, SampleRate: 16000, Channels: 1}}}
	}
	for _, tc := range []struct {
		name                  string
		transport             string
		sensors               []*sensorlinkpb.SensorDescriptor
		allow                 []string
		cameraUnavailable     bool
		want                  []uint32
		wantCamera, wantAudio int
	}{
		{name: "shared input prefers camera even when microphone is first", transport: "wendycom",
			sensors: []*sensorlinkpb.SensorDescriptor{microphone(1, 0, "mic"), camera(0, 0, "cam")},
			want:    []uint32{0}, wantCamera: 1},
		{name: "independent inputs keep camera and microphone", transport: "wendycom",
			sensors: []*sensorlinkpb.SensorDescriptor{microphone(1, 1, "mic"), camera(0, 0, "cam")},
			want:    []uint32{0, 1}, wantCamera: 1, wantAudio: 1},
		{name: "explicit microphone excludes camera", transport: "wendycom",
			sensors: []*sensorlinkpb.SensorDescriptor{camera(0, 0, "cam"), microphone(1, 0, "mic")}, allow: []string{"mic"},
			want: []uint32{1}, wantAudio: 1},
		{name: "camera alternatives use lowest channel", transport: "wendycom",
			sensors: []*sensorlinkpb.SensorDescriptor{camera(4, 0, "cam-large"), camera(2, 0, "cam-small")},
			want:    []uint32{2}, wantCamera: 1},
		{name: "microphone alternatives use lowest channel", transport: "wendycom",
			sensors: []*sensorlinkpb.SensorDescriptor{microphone(5, 0, "mic-stereo"), microphone(3, 0, "mic-mono")},
			want:    []uint32{3}, wantAudio: 1},
		{name: "unavailable camera permits microphone", transport: "wendycom",
			sensors: []*sensorlinkpb.SensorDescriptor{camera(0, 0, "cam"), microphone(1, 0, "mic")}, cameraUnavailable: true,
			want: []uint32{1}, wantAudio: 1},
		{name: "agent sources without input IDs retain concurrent streams", transport: "grpc",
			sensors: []*sensorlinkpb.SensorDescriptor{microphone(1, 0, "mic"), camera(2, 0, "cam")},
			want:    []uint32{2, 1}, wantCamera: 1, wantAudio: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tr := &selectionTransport{manifest: &sensorlinkpb.SensorManifest{Sensors: tc.sensors}, exclusive: tc.transport == "wendycom"}
			cam := &selectionCamera{unavailable: tc.cameraUnavailable}
			audio := &selectionAudio{}
			s := NewSupervisor(zap.NewNop(), cam, func(SensorPairing, string) (SensorTransport, error) { return tr, nil },
				func(string) ros2camera.CameraWriter { return cam }, audio)
			delivered, err := s.streamOnce(context.Background(), SensorPairing{SourceAssetID: 1, Transport: tc.transport, SensorAllowlist: tc.allow}, "source")
			if err != nil || !delivered {
				t.Fatalf("stream delivered=%v, error=%v, subscriptions=%v", delivered, err, tr.channels)
			}
			if !slices.Equal(tr.channels, tc.want) {
				t.Errorf("subscriptions=%v, want %v", tr.channels, tc.want)
			}
			if cam.frames != tc.wantCamera || audio.frames != tc.wantAudio {
				t.Errorf("delivered camera=%d audio=%d, want camera=%d audio=%d", cam.frames, audio.frames, tc.wantCamera, tc.wantAudio)
			}
		})
	}
}
