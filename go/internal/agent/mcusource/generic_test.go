package mcusource

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/agent/ros2camera"
	"github.com/wendylabsinc/wendy/go/internal/agent/sensorlink"
	sensorpb "github.com/wendylabsinc/wendy/go/proto/gen/sensorlinkpb"
	"go.uber.org/zap"
)

type recordingGeneric struct {
	frames   int
	released bool
}

func (g *recordingGeneric) Mount(_ SensorPairing, ds []*sensorpb.SensorDescriptor) ([]uint32, func(*sensorlink.SensorFrame) bool, func()) {
	ids := make([]uint32, 0, len(ds))
	for _, d := range ds {
		ids = append(ids, d.ChannelId)
	}
	return ids, func(*sensorlink.SensorFrame) bool { g.frames++; return true }, func() { g.released = true }
}
func TestSupervisorGenericAllowlistAndInputExclusion(t *testing.T) {
	desc := func(id, input uint32, name string) *sensorpb.SensorDescriptor {
		return &sensorpb.SensorDescriptor{ChannelId: id, InputId: input, Name: name, Format: &sensorpb.SensorDescriptor_Sensor{Sensor: &sensorpb.SensorFormat{Schema: "example", SampleBytes: 5}}}
	}
	for _, tc := range []struct {
		allow []string
		want  []uint32
	}{{nil, []uint32{1, 3}}, {[]string{"other"}, []uint32{2}}} {
		tr := &selectionTransport{exclusive: true, manifest: &sensorpb.SensorManifest{Sensors: []*sensorpb.SensorDescriptor{desc(2, 1, "other"), desc(1, 1, "first"), desc(3, 3, "third")}}}
		sink := &recordingGeneric{}
		s := NewSupervisor(zap.NewNop(), nil, func(SensorPairing, string) (SensorTransport, error) { return tr, nil }, nil, nil)
		s.SetGenericSink(sink)
		ok, err := s.streamOnce(context.Background(), SensorPairing{SourceAssetID: 1, Transport: "wendycom", SensorAllowlist: tc.allow}, "source")
		if err != nil || !ok || !reflect.DeepEqual(tr.channels, tc.want) || sink.frames != len(tc.want) || !sink.released {
			t.Fatalf("channels=%v sink=%+v delivered=%v err=%v", tr.channels, sink, ok, err)
		}
		if s.IsConnected(1) {
			t.Fatal("connected after stream closed")
		}
	}
}

func TestSupervisorGenericDoesNotDisplaceMedia(t *testing.T) {
	camera := &selectionCamera{}
	audio := &selectionAudio{}
	generic := &recordingGeneric{}
	descriptor := func(id, input uint32) *sensorpb.SensorDescriptor {
		return &sensorpb.SensorDescriptor{ChannelId: id, InputId: input, Format: &sensorpb.SensorDescriptor_Sensor{Sensor: &sensorpb.SensorFormat{Schema: "unknown", SampleBytes: 5}}}
	}
	tr := &selectionTransport{exclusive: true, manifest: &sensorpb.SensorManifest{Sensors: []*sensorpb.SensorDescriptor{
		descriptor(10, 0), descriptor(11, 1), descriptor(12, 2),
		{ChannelId: 1, InputId: 0, Format: &sensorpb.SensorDescriptor_Video{Video: &sensorpb.VideoFormat{Codec: sensorpb.VideoFormat_MJPEG}}},
		{ChannelId: 2, InputId: 1, Format: &sensorpb.SensorDescriptor_Audio{Audio: &sensorpb.AudioFormat{Codec: sensorpb.AudioFormat_PCM_S16LE}}},
	}}}
	supervisor := NewSupervisor(zap.NewNop(), camera, func(SensorPairing, string) (SensorTransport, error) { return tr, nil }, func(string) ros2camera.CameraWriter { return camera }, audio)
	supervisor.SetGenericSink(generic)
	delivered, err := supervisor.streamOnce(context.Background(), SensorPairing{SourceAssetID: 1, Transport: "wendycom"}, "source")
	if err != nil || !delivered || !reflect.DeepEqual(tr.channels, []uint32{1, 2, 12}) || camera.frames != 1 || audio.frames != 1 || generic.frames != 1 {
		t.Fatalf("channels=%v camera=%d audio=%d generic=%d delivered=%v error=%v", tr.channels, camera.frames, audio.frames, generic.frames, delivered, err)
	}
}

func TestGenericDescriptorValidation(t *testing.T) {
	for _, tc := range []struct {
		schema string
		size   uint32
		valid  bool
	}{
		{"vendor.imu.v9", 24, true}, {"event.button.v1", 1, true}, {"", 1, false}, {"x", 0, false}, {"x", 65536, true}, {"x", 65537, false}, {strings.Repeat("x", 1025), 1, false},
	} {
		d := &sensorpb.SensorDescriptor{Format: &sensorpb.SensorDescriptor_Sensor{Sensor: &sensorpb.SensorFormat{Schema: tc.schema, SampleBytes: tc.size}}}
		if ValidGenericDescriptor(d) != tc.valid {
			t.Fatalf("schema length=%d size=%d", len(tc.schema), tc.size)
		}
	}
	if ValidGenericDescriptor(nil) {
		t.Fatal("nil accepted")
	}
}
