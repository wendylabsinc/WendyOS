package services

import (
	"context"
	"strings"
	"testing"

	"go.uber.org/zap"

	"github.com/wendylabsinc/wendy/go/internal/agent/audio"
	"github.com/wendylabsinc/wendy/go/internal/agent/audioloop"
	"github.com/wendylabsinc/wendy/go/internal/agent/data"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

// iq9ArecordFixture is "arecord -l" from a Dragonwing IQ-9075 (demo-iq9) with
// snd-aloop loaded as card 1, captured while an ESP32-S3 microphone was paired
// and streaming into hw:Loopback,0,0. PipeWire was running on the same board.
const iq9ArecordFixture = `**** List of CAPTURE Hardware Devices ****
card 0: LEMANSEVK [LEMANS-EVK], device 1: MultiMedia2 Capture (*) []
  Subdevices: 0/1
  Subdevice #0: subdevice #0
card 1: Loopback [Loopback], device 0: Loopback PCM [Loopback PCM]
  Subdevices: 8/8
  Subdevice #0: subdevice #0
card 1: Loopback [Loopback], device 1: Loopback PCM [Loopback PCM]
  Subdevices: 8/8
  Subdevice #0: subdevice #0
`

const iq9AplayFixture = `**** List of PLAYBACK Hardware Devices ****
card 0: LEMANSEVK [LEMANS-EVK], device 0: MultiMedia1 Playback (*) []
card 1: Loopback [Loopback], device 0: Loopback PCM [Loopback PCM]
card 1: Loopback [Loopback], device 1: Loopback PCM [Loopback PCM]
`

const esp32MicPrincipal = "urn:wendy:tenant:5c9e14bb:asset:0f135af7"

// stubAlsaLists points the ALSA enumeration at fixtures and counts how often it
// runs, without touching audio.Available: callers pick the path separately.
func stubAlsaLists(t *testing.T, aplayOut, arecordOut string) *int {
	t.Helper()
	origAplay, origArecord := audio.AplayListRun, audio.ArecordListRun
	t.Cleanup(func() { audio.AplayListRun, audio.ArecordListRun = origAplay, origArecord })
	calls := 0
	audio.AplayListRun = func(context.Context) ([]byte, error) { calls++; return []byte(aplayOut), nil }
	audio.ArecordListRun = func(context.Context) ([]byte, error) { return []byte(arecordOut), nil }
	return &calls
}

// sensorLinkAudioService is an AudioService with one paired microphone mounted
// on Loopback subdevice 0, named the way the pairing store names the esp32-cam.
func sensorLinkAudioService(mounts ...audioloop.Mount) *AudioService {
	svc := NewAudioService(zap.NewNop())
	svc.SetSensorLinkNaming(func() []audioloop.Mount { return mounts }, nil)
	svc.SetSensorLinkPrincipalNaming(func(uri string) (string, bool) {
		return "esp32-cam", uri == esp32MicPrincipal
	})
	return svc
}

var esp32MicMount = audioloop.Mount{Sub: 0, SourcePrincipalURI: esp32MicPrincipal, ChannelID: 1, SensorName: "microphone"}

// healthyAudioMatching returns the healthy audio sources whose detail mentions
// selector. A campaign's "audio: <selector>" resolves only when this is
// exactly one source (data.resolveKindSelector's substring rule).
func healthyAudioMatching(sources []data.Source, selector string) []data.Source {
	var out []data.Source
	for _, s := range sources {
		if s.Kind == "audio" && s.Healthy && strings.Contains(strings.ToLower(s.Detail), strings.ToLower(selector)) {
			out = append(out, s)
		}
	}
	return out
}

// TestListAudioDevicesListsSensorLinkMicOnPipeWire is the regression for a
// paired microphone being invisible on every PipeWire host: the listing used to
// be the PipeWire graph alone, which has no node for the capture side of the
// Loopback subdevice the mount writes to.
func TestListAudioDevicesListsSensorLinkMicOnPipeWire(t *testing.T) {
	stubGraph(t, graphFixture, nil)
	stubAlsaLists(t, iq9AplayFixture, iq9ArecordFixture)

	resp, err := sensorLinkAudioService(esp32MicMount).ListAudioDevices(context.Background(), &agentpb.ListAudioDevicesRequest{})
	if err != nil {
		t.Fatalf("ListAudioDevices() error = %v", err)
	}

	var mic *agentpb.AudioDevice
	var pipewireIDs []uint32
	for _, d := range resp.GetDevices() {
		if strings.HasPrefix(d.GetDescription(), "sensor-link:") {
			if mic != nil {
				t.Fatalf("two sensor-link rows for one mount: %v and %v", mic, d)
			}
			mic = d
			continue
		}
		pipewireIDs = append(pipewireIDs, d.GetId())
	}
	if mic == nil {
		t.Fatalf("no sensor-link microphone in %v", resp.GetDevices())
	}
	if got, want := mic.GetDescription(), "sensor-link: esp32-cam · microphone"; got != want {
		t.Errorf("description = %q, want %q", got, want)
	}
	if got, want := mic.GetName(), "plughw:1,1,0"; got != want {
		t.Errorf("name = %q, want %q (the capture side of the cable the mount plays into)", got, want)
	}
	if mic.GetType() != agentpb.AudioDeviceType_AUDIO_DEVICE_TYPE_INPUT {
		t.Errorf("type = %v, want INPUT", mic.GetType())
	}
	// The PipeWire graph is still the whole of the rest of the listing; no raw
	// ALSA card rows leak in alongside it.
	if got, want := len(pipewireIDs), 4; got != want {
		t.Errorf("PipeWire rows = %v, want the fixture's %d nodes and nothing else", pipewireIDs, want)
	}
}

// TestListAudioDevicesSkipsALSAWithoutMounts keeps the PipeWire path free of
// aplay/arecord processes on the common device that has nothing paired.
func TestListAudioDevicesSkipsALSAWithoutMounts(t *testing.T) {
	stubGraph(t, graphFixture, nil)
	calls := stubAlsaLists(t, iq9AplayFixture, iq9ArecordFixture)

	resp, err := sensorLinkAudioService().ListAudioDevices(context.Background(), &agentpb.ListAudioDevicesRequest{})
	if err != nil {
		t.Fatalf("ListAudioDevices() error = %v", err)
	}
	if got := len(resp.GetDevices()); got != 4 {
		t.Errorf("devices = %d, want the fixture's 4", got)
	}
	if *calls != 0 {
		t.Errorf("ALSA enumerated %d times with no mounts, want 0", *calls)
	}
}

// TestCaptureTargetSensorLinkMicOnPipeWire pins the capture half of the fix:
// a mount row records through arecord from plughw:<card>,1,<sub> even when a
// PipeWire session is up, because pw-record has no target for it.
func TestCaptureTargetSensorLinkMicOnPipeWire(t *testing.T) {
	stubGraph(t, graphFixture, nil)

	got, err := captureTarget(context.Background(), audio.EncodeAlsaSubdeviceID(1, 3))
	if err != nil {
		t.Fatalf("captureTarget(mount) error = %v", err)
	}
	if got.alsaDevice != "plughw:1,1,3" || got.pwTarget != "" {
		t.Errorf("captureTarget(mount) = %+v, want alsaDevice plughw:1,1,3 and no PipeWire target", got)
	}

	// Ordinary PipeWire node IDs still resolve through the graph.
	if got, err := captureTarget(context.Background(), 55); err != nil || got.pwTarget != "88" {
		t.Errorf("captureTarget(55) = %+v, %v; want the USB mic's serial 88", got, err)
	}
}

// TestDataSourcesListSensorLinkMicOnPipeWire is the wendy data view of the same
// bug: "wendy data sources" on demo-iq9 offered only the PipeWire Loopback
// source, which recorded zeros, and no way to select the paired microphone.
func TestDataSourcesListSensorLinkMicOnPipeWire(t *testing.T) {
	stubGraph(t, graphFixture, nil)
	stubAlsaLists(t, iq9AplayFixture, iq9ArecordFixture)

	adapter := &audioDataAdapter{audio: sensorLinkAudioService(esp32MicMount)}
	sources := adapter.Discover(context.Background())

	var mic []string
	for _, s := range sources {
		if strings.Contains(s.Detail, "sensor-link") {
			mic = append(mic, s.ID)
			if s.ClockDomain != alsaCaptureClockDomain {
				t.Errorf("%s clock domain = %q, want %q (it is recorded through arecord)", s.ID, s.ClockDomain, alsaCaptureClockDomain)
			}
			if !s.Healthy {
				t.Errorf("%s reported unhealthy: %s", s.ID, s.Detail)
			}
			continue
		}
		if s.ClockDomain != pipewireCaptureClockDomain {
			t.Errorf("%s clock domain = %q, want %q", s.ID, s.ClockDomain, pipewireCaptureClockDomain)
		}
	}
	if len(mic) != 1 {
		t.Fatalf("sensor-link sources = %v, want exactly one in %+v", mic, sources)
	}

	// A campaign names it by its pairing, and that selector picks exactly the
	// row whose ID decodes to the right capture handle.
	matches := healthyAudioMatching(sources, "esp32-cam")
	if len(matches) != 1 {
		t.Fatalf("audio selector esp32-cam matches %+v, want exactly one source", matches)
	}
	if got, want := matches[0].ID, "audio:sensor-link:esp32-cam:microphone"; got != want {
		t.Errorf("sensor-link source ID = %q, want %q (named for the pairing, not the subdevice)", got, want)
	}
	devices, err := adapter.resolveAudioDeviceIDs(context.Background(), matches)
	if err != nil {
		t.Fatalf("resolving %q to a capture node: %v", matches[0].ID, err)
	}
	devID, ok := devices[matches[0].ID]
	if !ok {
		t.Fatalf("selected source %q did not resolve to a capture node", matches[0].ID)
	}
	src, ok := sensorLinkCaptureSource(devID)
	if !ok || src.alsaDevice != "plughw:1,1,0" {
		t.Errorf("selected source %q captures from %+v, want plughw:1,1,0", matches[0].ID, src)
	}
}

// TestDataSourcesNameSensorLinkMicOnALSA covers the fallback path, which used
// to list only the generic "Loopback PCM" row: recording it let arecord pick
// whichever subdevice was free rather than the one carrying the microphone.
func TestDataSourcesNameSensorLinkMicOnALSA(t *testing.T) {
	origAvailable := audio.Available
	t.Cleanup(func() { audio.Available = origAvailable })
	audio.Available = func() bool { return false }
	stubAlsaLists(t, iq9AplayFixture, iq9ArecordFixture)

	adapter := &audioDataAdapter{audio: sensorLinkAudioService(esp32MicMount)}
	sources := adapter.Discover(context.Background())

	var details []string
	for _, s := range sources {
		details = append(details, s.Detail)
		if strings.Contains(s.Detail, "plughw:1,1") && !strings.Contains(s.Detail, "sensor-link") {
			t.Errorf("generic Loopback capture row %q is still listed beside the named mount", s.Detail)
		}
	}
	if matches := healthyAudioMatching(sources, "esp32-cam"); len(matches) != 1 {
		t.Errorf("audio selector esp32-cam matches %+v, want exactly one (sources: %q)", matches, details)
	}
}
