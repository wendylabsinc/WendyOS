package mcusource

import (
	"context"
	"reflect"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/agent/ros2camera"
	"github.com/wendylabsinc/wendy/go/internal/agent/sensorlink"
	sensorpb "github.com/wendylabsinc/wendy/go/proto/gen/sensorlinkpb"
	"go.uber.org/zap"
)

type worldMixedTransport struct{ liveSelectionTransport }

func (tr *worldMixedTransport) streamFrames(ctx context.Context, channels []uint32, sink frameSink) (func() error, error) {
	tr.channels = append([]uint32(nil), channels...)
	return tr.liveSelectionTransport.streamFrames(ctx, channels, sink)
}

func TestWorldCameraObserverKeepsGenericAndAudioChannels(t *testing.T) {
	for _, unavailable := range []bool{false, true} {
		t.Run(map[bool]string{false: "mounted", true: "observer-only"}[unavailable], func(t *testing.T) {
			camera, audio, generic := &selectionCamera{unavailable: unavailable}, &selectionAudio{}, &recordingGeneric{}
			tr := &worldMixedTransport{liveSelectionTransport: liveSelectionTransport{selectionTransport: selectionTransport{exclusive: true, manifest: &sensorpb.SensorManifest{Sensors: []*sensorpb.SensorDescriptor{
				{ChannelId: 1, InputId: 1, Format: &sensorpb.SensorDescriptor_Video{Video: &sensorpb.VideoFormat{Codec: sensorpb.VideoFormat_MJPEG}}},
				{ChannelId: 2, InputId: 2, Format: &sensorpb.SensorDescriptor_Audio{Audio: &sensorpb.AudioFormat{Codec: sensorpb.AudioFormat_PCM_S16LE}}},
				{ChannelId: 3, InputId: 3, Format: &sensorpb.SensorDescriptor_Sensor{Sensor: &sensorpb.SensorFormat{Schema: "test", SampleBytes: 5}}},
				{ChannelId: 4, InputId: 1, Format: &sensorpb.SensorDescriptor_Sensor{Sensor: &sensorpb.SensorFormat{Schema: "test", SampleBytes: 5}}},
			}}}}}
			supervisor := NewSupervisor(zap.NewNop(), camera, func(SensorPairing, string) (SensorTransport, error) { return tr, nil }, func(string) ros2camera.CameraWriter { return camera }, audio)
			supervisor.SetGenericSink(generic)
			observed, closed := 0, 0
			supervisor.SetCameraFrameObserver(func(context.Context, SensorPairing) (func(*sensorlink.SensorFrame, ros2camera.Frame), func()) {
				return func(f *sensorlink.SensorFrame, _ ros2camera.Frame) {
					if f.ChannelID != 1 {
						t.Fatal("observed another channel")
					}
					observed++
				}, func() { closed++ }
			}, func(_ SensorPairing, id uint32) bool { return id == 1 })
			delivered, err := supervisor.streamOnce(t.Context(), SensorPairing{SourceAssetID: 1, Transport: "wendycom"}, "source")
			wantCamera := 1
			if unavailable {
				wantCamera = 0
			}
			if err != nil || !delivered || !reflect.DeepEqual(tr.channels, []uint32{1, 2, 3}) || !reflect.DeepEqual(tr.latest, []uint32{1}) || camera.frames != wantCamera || audio.frames != 1 || generic.frames != 1 || !generic.released || observed != 1 || closed != 1 {
				t.Fatalf("delivered=%v error=%v channels=%v latest=%v camera=%d audio=%d generic=%+v observer=%d/%d", delivered, err, tr.channels, tr.latest, camera.frames, audio.frames, generic, observed, closed)
			}
		})
	}
}
