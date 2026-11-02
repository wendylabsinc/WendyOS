package mcusource

import (
	"context"
	"slices"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/agent/ros2camera"
	"github.com/wendylabsinc/wendy/go/internal/agent/sensorlink"
	sensorlinkpb "github.com/wendylabsinc/wendy/go/proto/gen/sensorlinkpb"
	"go.uber.org/zap"
)

func TestSupervisorSkipsUnsupportedCodecs(t *testing.T) {
	video := func(id uint32, codec sensorlinkpb.VideoFormat_Codec) *sensorlinkpb.SensorDescriptor {
		return &sensorlinkpb.SensorDescriptor{ChannelId: id, InputId: id, Name: "video",
			Format: &sensorlinkpb.SensorDescriptor_Video{Video: &sensorlinkpb.VideoFormat{Codec: codec}}}
	}
	audio := func(id uint32, codec sensorlinkpb.AudioFormat_Codec) *sensorlinkpb.SensorDescriptor {
		return &sensorlinkpb.SensorDescriptor{ChannelId: id, InputId: id, Name: "audio",
			Format: &sensorlinkpb.SensorDescriptor_Audio{Audio: &sensorlinkpb.AudioFormat{Codec: codec, SampleRate: 16000, Channels: 1}}}
	}
	for _, transport := range []string{"wendycom", "grpc"} {
		t.Run(transport, func(t *testing.T) {
			tr := &selectionTransport{manifest: &sensorlinkpb.SensorManifest{Sensors: []*sensorlinkpb.SensorDescriptor{
				video(1, sensorlinkpb.VideoFormat_CODEC_UNSPECIFIED),
				video(2, sensorlinkpb.VideoFormat_Codec(99)),
				video(3, sensorlinkpb.VideoFormat_MJPEG),
				video(4, sensorlinkpb.VideoFormat_H264),
				audio(5, sensorlinkpb.AudioFormat_CODEC_UNSPECIFIED),
				audio(6, sensorlinkpb.AudioFormat_OPUS),
				audio(7, sensorlinkpb.AudioFormat_Codec(99)),
				audio(8, sensorlinkpb.AudioFormat_PCM_S16LE),
			}}}
			cam, mic := &selectionCamera{}, &selectionAudio{}
			s := NewSupervisor(zap.NewNop(), cam, func(SensorPairing, string) (SensorTransport, error) { return tr, nil },
				func(string) ros2camera.CameraWriter { return cam }, mic)
			delivered, err := s.streamOnce(context.Background(), SensorPairing{SourceAssetID: 1, Transport: transport}, "source")
			if err != nil || !delivered {
				t.Fatalf("stream delivered=%v, error=%v", delivered, err)
			}
			if want := []uint32{3, 4, 8}; !slices.Equal(tr.channels, want) {
				t.Errorf("subscribed channels=%v, want %v", tr.channels, want)
			}
			if cam.frames != 2 || mic.frames != 1 {
				t.Errorf("delivered camera=%d audio=%d, want camera=2 audio=1", cam.frames, mic.frames)
			}
		})
	}
}

func TestFrameToCameraRequiresSupportedManifestCodec(t *testing.T) {
	for _, tc := range []struct {
		name  string
		video *sensorlinkpb.VideoFormat
		codec ros2camera.Codec
		ok    bool
	}{
		{name: "MJPEG", video: &sensorlinkpb.VideoFormat{Codec: sensorlinkpb.VideoFormat_MJPEG, Width: 320, Height: 240}, codec: ros2camera.CodecMJPEG, ok: true},
		{name: "H264", video: &sensorlinkpb.VideoFormat{Codec: sensorlinkpb.VideoFormat_H264, Width: 640, Height: 480}, codec: ros2camera.CodecH264, ok: true},
		{name: "unspecified", video: &sensorlinkpb.VideoFormat{}},
		{name: "future", video: &sensorlinkpb.VideoFormat{Codec: sensorlinkpb.VideoFormat_Codec(99)}},
		{name: "non-video"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := &sensorlinkpb.SensorDescriptor{ChannelId: 7, Format: &sensorlinkpb.SensorDescriptor_Video{Video: tc.video}}
			f := &sensorlink.SensorFrame{ChannelID: 7, Payload: []byte("encoded frame")}
			frame, ok := frameToCamera(f, []*sensorlinkpb.SensorDescriptor{d})
			if ok != tc.ok {
				t.Fatalf("frame supported=%v, want %v", ok, tc.ok)
			}
			if ok && (frame.Codec != tc.codec || frame.Width != int(tc.video.Width) || frame.Height != int(tc.video.Height) || string(frame.Data) != string(f.Payload)) {
				t.Errorf("converted frame=%+v", frame)
			}
			f.ChannelID++
			if _, ok := frameToCamera(f, []*sensorlinkpb.SensorDescriptor{d}); ok {
				t.Fatal("frame without matching manifest channel was accepted")
			}
		})
	}
}
