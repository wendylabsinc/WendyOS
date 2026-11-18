package mcusource

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/agent/ros2camera"
	"github.com/wendylabsinc/wendy/go/internal/agent/sensorlink"
	sensorlinkpb "github.com/wendylabsinc/wendy/go/proto/gen/sensorlinkpb"
	"go.uber.org/zap"
)

// recordingSensorChannel is the sink a generic sensor channel is handed to.
type recordingSensorChannel struct {
	samples [][]byte
	closed  bool
}

func (c *recordingSensorChannel) WriteSample(payload []byte) {
	c.samples = append(c.samples, append([]byte(nil), payload...))
}
func (c *recordingSensorChannel) Close() error { c.closed = true; return nil }

// sensorSink records which channels the supervisor asked to have delivered.
type sensorSink struct {
	opened   map[string]*recordingSensorChannel
	pairings []SensorPairing
	refuse   bool
}

func (s *sensorSink) open(p SensorPairing, d *sensorlinkpb.SensorDescriptor) (SensorChannelWriter, error) {
	if s.refuse {
		return nil, errors.New("no room for another channel")
	}
	if s.opened == nil {
		s.opened = map[string]*recordingSensorChannel{}
	}
	c := &recordingSensorChannel{}
	s.opened[d.GetName()] = c
	s.pairings = append(s.pairings, p)
	return c, nil
}

func TestSupervisorDeliversGenericSensorChannels(t *testing.T) {
	camera := func(channel, input uint32, name string) *sensorlinkpb.SensorDescriptor {
		return &sensorlinkpb.SensorDescriptor{ChannelId: channel, InputId: input, Name: name,
			Format: &sensorlinkpb.SensorDescriptor_Video{Video: &sensorlinkpb.VideoFormat{Codec: sensorlinkpb.VideoFormat_MJPEG}}}
	}
	microphone := func(channel, input uint32, name string) *sensorlinkpb.SensorDescriptor {
		return &sensorlinkpb.SensorDescriptor{ChannelId: channel, InputId: input, Name: name,
			Format: &sensorlinkpb.SensorDescriptor_Audio{Audio: &sensorlinkpb.AudioFormat{Codec: sensorlinkpb.AudioFormat_PCM_S16LE, SampleRate: 16000, Channels: 1}}}
	}
	sensor := func(channel, input uint32, name string) *sensorlinkpb.SensorDescriptor {
		return &sensorlinkpb.SensorDescriptor{ChannelId: channel, InputId: input, Name: name,
			Format: &sensorlinkpb.SensorDescriptor_Sensor{Sensor: &sensorlinkpb.SensorFormat{Schema: "wendy.temperature.celsius.f32le.v1", SampleBytes: 4}}}
	}
	opus := &sensorlinkpb.SensorDescriptor{ChannelId: 9, Name: "voice",
		Format: &sensorlinkpb.SensorDescriptor_Audio{Audio: &sensorlinkpb.AudioFormat{Codec: sensorlinkpb.AudioFormat_OPUS}}}

	for _, tc := range []struct {
		name          string
		transport     string
		sensors       []*sensorlinkpb.SensorDescriptor
		allow         []string
		noSink        bool
		refuse        bool
		want          []uint32 // subscriptions, in order; nil means the source is not streamed
		wantDelivered []string // sensor channels that received a sample
		wantCamera    int
		wantAudio     int
	}{
		{name: "a source with only a sensor is streamed", transport: "grpc",
			sensors: []*sensorlinkpb.SensorDescriptor{sensor(3, 0, "temperature")},
			want:    []uint32{3}, wantDelivered: []string{"temperature"}},
		{name: "sensors are delivered beside camera and microphone", transport: "grpc",
			sensors: []*sensorlinkpb.SensorDescriptor{sensor(3, 0, "temperature"), microphone(1, 0, "mic"), camera(2, 0, "cam")},
			want:    []uint32{2, 1, 3}, wantDelivered: []string{"temperature"}, wantCamera: 1, wantAudio: 1},
		{name: "a pairing that names other channels leaves the sensor out", transport: "grpc",
			sensors: []*sensorlinkpb.SensorDescriptor{sensor(3, 0, "temperature"), camera(2, 0, "cam")}, allow: []string{"cam"},
			want: []uint32{2}, wantCamera: 1},
		{name: "a pairing may name only the sensor", transport: "grpc",
			sensors: []*sensorlinkpb.SensorDescriptor{sensor(3, 0, "temperature"), camera(2, 0, "cam")}, allow: []string{"temperature"},
			want: []uint32{3}, wantDelivered: []string{"temperature"}},
		{name: "on a Lite source a sensor yields a shared input to the camera", transport: "wendycom",
			sensors: []*sensorlinkpb.SensorDescriptor{sensor(3, 0, "temperature"), camera(0, 0, "cam")},
			want:    []uint32{0}, wantCamera: 1},
		{name: "on a Lite source a sensor on its own input is delivered", transport: "wendycom",
			sensors: []*sensorlinkpb.SensorDescriptor{sensor(3, 1, "temperature"), camera(0, 0, "cam")},
			want:    []uint32{0, 3}, wantDelivered: []string{"temperature"}, wantCamera: 1},
		{name: "on a Lite source sensors sharing an input use the lowest channel", transport: "wendycom",
			sensors: []*sensorlinkpb.SensorDescriptor{sensor(7, 2, "fast"), sensor(5, 2, "slow")},
			want:    []uint32{5}, wantDelivered: []string{"slow"}},
		{name: "a sensor the sink refuses does not stop the other channels", transport: "grpc", refuse: true,
			sensors: []*sensorlinkpb.SensorDescriptor{sensor(3, 0, "temperature"), camera(2, 0, "cam")},
			want:    []uint32{2}, wantCamera: 1},
		{name: "audio in a codec the agent cannot decode is not handed to the sensor sink", transport: "grpc",
			sensors: []*sensorlinkpb.SensorDescriptor{opus, sensor(3, 0, "temperature")},
			want:    []uint32{3}, wantDelivered: []string{"temperature"}},
		{name: "without a sink sensors are not subscribed", transport: "grpc", noSink: true,
			sensors: []*sensorlinkpb.SensorDescriptor{sensor(3, 0, "temperature")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tr := &selectionTransport{manifest: &sensorlinkpb.SensorManifest{Sensors: tc.sensors}, exclusive: tc.transport == "wendycom"}
			cam := &selectionCamera{}
			audio := &selectionAudio{}
			sink := &sensorSink{refuse: tc.refuse}
			s := NewSupervisor(zap.NewNop(), cam, func(SensorPairing, string) (SensorTransport, error) { return tr, nil },
				func(string) ros2camera.CameraWriter { return cam }, audio)
			if !tc.noSink {
				s.SetSensorChannelOpener(sink.open)
			}
			pairing := SensorPairing{SourceAssetID: 1, Name: "porch", Transport: tc.transport, SensorAllowlist: tc.allow}
			delivered, err := s.streamOnce(context.Background(), pairing, "source")
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(tr.channels, tc.want) {
				t.Fatalf("subscriptions = %v, want %v", tr.channels, tc.want)
			}
			if delivered != (tc.want != nil) {
				t.Fatalf("delivered = %v with subscriptions %v", delivered, tr.channels)
			}
			var got []string
			for name, c := range sink.opened {
				if len(c.samples) != 1 || string(c.samples[0]) != "frame" {
					t.Errorf("channel %q received %q, want one sample", name, c.samples)
				}
				if !c.closed {
					t.Errorf("channel %q was left open after the stream ended", name)
				}
				got = append(got, name)
			}
			slices.Sort(got)
			if !slices.Equal(got, tc.wantDelivered) {
				t.Errorf("sensor channels delivered = %v, want %v", got, tc.wantDelivered)
			}
			for _, p := range sink.pairings {
				if p.Key() != pairing.Key() || p.Name != "porch" {
					t.Errorf("sink was told the channel belongs to %q (%q)", p.Key(), p.Name)
				}
			}
			if cam.frames != tc.wantCamera || audio.frames != tc.wantAudio {
				t.Errorf("camera frames = %d, audio frames = %d, want %d and %d", cam.frames, audio.frames, tc.wantCamera, tc.wantAudio)
			}
		})
	}
}

// framesTransport offers a manifest and then delivers a fixed run of frames.
type framesTransport struct {
	manifest *sensorlinkpb.SensorManifest
	frames   []*sensorlink.SensorFrame
}

func (*framesTransport) Close() error { return nil }
func (tr *framesTransport) FetchManifest(context.Context) (*sensorlinkpb.SensorManifest, error) {
	return tr.manifest, nil
}
func (tr *framesTransport) Stream(context.Context, []uint32) (<-chan *sensorlink.SensorFrame, func() error, error) {
	frames := make(chan *sensorlink.SensorFrame, len(tr.frames))
	for _, f := range tr.frames {
		frames <- f
	}
	close(frames)
	return frames, func() error { return nil }, nil
}

func TestSourceWithOnlyASensorShowsAsConnectedWhileItDelivers(t *testing.T) {
	sensor := &sensorlinkpb.SensorDescriptor{ChannelId: 3, Name: "temperature",
		Format: &sensorlinkpb.SensorDescriptor_Sensor{Sensor: &sensorlinkpb.SensorFormat{Schema: "wendy.temperature.celsius.f32le.v1", SampleBytes: 4}}}
	tr := &framesTransport{manifest: &sensorlinkpb.SensorManifest{Sensors: []*sensorlinkpb.SensorDescriptor{sensor}},
		frames: []*sensorlink.SensorFrame{{ChannelID: 3, Payload: []byte{1}}, {ChannelID: 3, Payload: []byte{2}}}}
	s := NewSupervisor(zap.NewNop(), &selectionCamera{}, func(SensorPairing, string) (SensorTransport, error) { return tr, nil },
		func(string) ros2camera.CameraWriter { return &selectionCamera{} }, &selectionAudio{})
	pairing := SensorPairing{SourceAssetID: 1}
	// The pairing list shows a source as connected only once it has delivered
	// something, so record the state seen as each sample arrives.
	var connected []bool
	s.SetSensorChannelOpener(func(SensorPairing, *sensorlinkpb.SensorDescriptor) (SensorChannelWriter, error) {
		return sampleFunc(func() { connected = append(connected, s.IsPairingConnected(pairing)) }), nil
	})
	if _, err := s.streamOnce(context.Background(), pairing, "source"); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(connected, []bool{false, true}) {
		t.Fatalf("connected as the two samples arrived = %v, want false until the first is delivered and true after", connected)
	}
	if s.IsPairingConnected(pairing) {
		t.Fatal("pairing still reported connected after the stream ended")
	}
}

// sampleFunc calls a function for every sample.
type sampleFunc func()

func (f sampleFunc) WriteSample([]byte) { f() }
func (sampleFunc) Close() error         { return nil }
