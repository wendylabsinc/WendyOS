package services

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/wendylabsinc/wendy/go/internal/agent/audio"
	"github.com/wendylabsinc/wendy/go/internal/agent/audioloop"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

// A graph with a Bluetooth speaker (the default sink), a Bluetooth microphone
// (the default source), an HDMI sink and a USB microphone. The Bluetooth
// endpoints are the ones ALSA cannot represent, and they are why enumeration
// goes through PipeWire.
const graphFixture = `[
  {
    "id": 30,
    "type": "PipeWire:Interface:Metadata",
    "props": { "metadata.name": "default" },
    "metadata": [
      { "subject": 0, "key": "default.audio.sink", "value": { "name": "bluez_output.78_2B_64_76_F3_CE.1" } },
      { "subject": 0, "key": "default.audio.source", "value": { "name": "bluez_input.78:2B:64:76:F3:CE" } }
    ]
  },
  {
    "id": 62,
    "type": "PipeWire:Interface:Node",
    "info": { "props": {
      "object.serial": 60, "media.class": "Audio/Sink",
      "node.name": "alsa_output.platform-fef00700.hdmi.hdmi-stereo",
      "node.description": "Built-in Audio"
    } }
  },
  {
    "id": 43,
    "type": "PipeWire:Interface:Node",
    "info": { "props": {
      "object.serial": 242, "media.class": "Audio/Sink",
      "node.name": "bluez_output.78_2B_64_76_F3_CE.1",
      "node.description": "Bose Revolve II SoundLink"
    } }
  },
  {
    "id": 44,
    "type": "PipeWire:Interface:Node",
    "info": { "props": {
      "object.serial": 251, "media.class": "Audio/Source",
      "node.name": "bluez_input.78:2B:64:76:F3:CE",
      "node.description": "Bose Revolve II SoundLink"
    } }
  },
  {
    "id": 55,
    "type": "PipeWire:Interface:Node",
    "info": { "props": {
      "object.serial": 88, "media.class": "Audio/Source",
      "node.name": "alsa_input.usb-046d_C920",
      "node.description": "HD Pro Webcam C920"
    } }
  }
]`

// stubGraph points the audio package at a fixture and records every wpctl call.
// It returns the recorded calls, which the caller reads after the exercise.
// Volume reads run concurrently, so the recorder is guarded. It also forces
// audio.Available() to true: without this, every test here would run in the
// sandbox's real "no PipeWire session" state and silently exercise the ALSA
// fallback instead of the stubbed PipeWire graph.
func stubGraph(t *testing.T, dump string, wpctl func(args ...string) ([]byte, error)) *[]string {
	t.Helper()
	origDump, origWpctl, origAvailable := audio.DumpRun, audio.WpctlRun, audio.Available
	t.Cleanup(func() { audio.DumpRun, audio.WpctlRun, audio.Available = origDump, origWpctl, origAvailable })

	var mu sync.Mutex
	var calls []string
	audio.Available = func() bool { return true }
	audio.DumpRun = func(context.Context) ([]byte, error) { return []byte(dump), nil }
	audio.WpctlRun = func(_ context.Context, args ...string) ([]byte, error) {
		mu.Lock()
		calls = append(calls, strings.Join(args, " "))
		mu.Unlock()
		if wpctl == nil {
			return nil, nil
		}
		return wpctl(args...)
	}
	return &calls
}

func testAudioService() *AudioService { return NewAudioService(zap.NewNop()) }

func TestListAudioDevices(t *testing.T) {
	stubGraph(t, graphFixture, nil)

	resp, err := testAudioService().ListAudioDevices(context.Background(), &agentpb.ListAudioDevicesRequest{})
	if err != nil {
		t.Fatalf("ListAudioDevices() error = %v", err)
	}

	// Sorted by node ID so repeated listings are stable even as the graph
	// reorders itself.
	var gotIDs []uint32
	for _, d := range resp.GetDevices() {
		gotIDs = append(gotIDs, d.GetId())
	}
	want := []uint32{43, 44, 55, 62}
	if fmt.Sprint(gotIDs) != fmt.Sprint(want) {
		t.Fatalf("device IDs = %v, want %v", gotIDs, want)
	}

	byID := map[uint32]*agentpb.AudioDevice{}
	for _, d := range resp.GetDevices() {
		byID[d.GetId()] = d
	}

	// The Bluetooth speaker: an output, the current default, and named by its
	// PipeWire node name rather than a card/device pair it does not have.
	speaker := byID[43]
	if speaker.GetType() != agentpb.AudioDeviceType_AUDIO_DEVICE_TYPE_OUTPUT {
		t.Errorf("Bluetooth sink type = %v, want OUTPUT", speaker.GetType())
	}
	if !speaker.GetIsDefault() {
		t.Error("Bluetooth sink should be the default output")
	}
	if speaker.GetName() != "bluez_output.78_2B_64_76_F3_CE.1" {
		t.Errorf("name = %q", speaker.GetName())
	}
	if speaker.GetDescription() != "Bose Revolve II SoundLink" {
		t.Errorf("description = %q", speaker.GetDescription())
	}

	if !byID[44].GetIsDefault() {
		t.Error("Bluetooth source should be the default input")
	}
	// Defaults are per-direction: the HDMI sink and the USB mic are not
	// defaults just because something else in their direction is.
	if byID[62].GetIsDefault() || byID[55].GetIsDefault() {
		t.Error("non-default devices marked as default")
	}
	if byID[55].GetType() != agentpb.AudioDeviceType_AUDIO_DEVICE_TYPE_INPUT {
		t.Errorf("USB mic type = %v, want INPUT", byID[55].GetType())
	}
}

func TestListAudioDevicesTypeFilter(t *testing.T) {
	stubGraph(t, graphFixture, nil)
	in := agentpb.AudioDeviceType_AUDIO_DEVICE_TYPE_INPUT

	resp, err := testAudioService().ListAudioDevices(context.Background(),
		&agentpb.ListAudioDevicesRequest{TypeFilter: &in})
	if err != nil {
		t.Fatalf("ListAudioDevices() error = %v", err)
	}
	if len(resp.GetDevices()) != 2 {
		t.Fatalf("got %d devices, want the 2 inputs", len(resp.GetDevices()))
	}
	for _, d := range resp.GetDevices() {
		if d.GetType() != in {
			t.Errorf("device %d has type %v, want INPUT", d.GetId(), d.GetType())
		}
	}
}

func TestListAudioDevicesQueryFailure(t *testing.T) {
	// A session is up but querying it failed outright (e.g. pw-dump wedged or
	// emitted garbage). This must still surface as a failure, not silently
	// fall back to ALSA and hide a real PipeWire problem.
	stubGraph(t, "", nil)
	orig := audio.DumpRun
	t.Cleanup(func() { audio.DumpRun = orig })
	audio.DumpRun = func(context.Context) ([]byte, error) {
		return nil, fmt.Errorf("pw-dump: exit status 1")
	}

	_, err := testAudioService().ListAudioDevices(context.Background(), &agentpb.ListAudioDevicesRequest{})
	// Reporting audio as unavailable is the point: an empty list would read as
	// "this device has no speakers".
	if status.Code(err) != codes.Internal {
		t.Fatalf("error = %v, want codes.Internal", err)
	}
}

func TestListAudioDevicesFallsBackToALSAWithNoSession(t *testing.T) {
	origAvailable, origAplay, origArecord := audio.Available, audio.AplayListRun, audio.ArecordListRun
	t.Cleanup(func() {
		audio.Available, audio.AplayListRun, audio.ArecordListRun = origAvailable, origAplay, origArecord
	})
	audio.Available = func() bool { return false }
	audio.AplayListRun = func(context.Context) ([]byte, error) {
		return []byte("card 0: vc4hdmi0 [vc4-hdmi-0], device 0: MAI PCM i2s-hifi-0 [MAI PCM i2s-hifi-0]\n"), nil
	}
	audio.ArecordListRun = func(context.Context) ([]byte, error) { return []byte(""), nil }

	resp, err := testAudioService().ListAudioDevices(context.Background(), &agentpb.ListAudioDevicesRequest{})
	if err != nil {
		t.Fatalf("ListAudioDevices() error = %v", err)
	}
	if len(resp.GetDevices()) != 1 {
		t.Fatalf("got %d devices, want the 1 ALSA card; got %+v", len(resp.GetDevices()), resp.GetDevices())
	}
	device := resp.GetDevices()[0]
	// ALSA has no default-device concept, so this must never be claimed true.
	if device.GetIsDefault() {
		t.Error("ALSA fallback device reported as default, but ALSA has no such concept")
	}
	if device.GetType() != agentpb.AudioDeviceType_AUDIO_DEVICE_TYPE_OUTPUT {
		t.Errorf("device type = %v, want OUTPUT", device.GetType())
	}
}

// Without a session the agent cannot set a default, but the refusal has to say
// which precondition failed. The old message asserted "no PipeWire session is
// running", which is more than the agent established — it only knows it found
// no session it would trust where it looked — and it left an operator with
// nothing to check.
func TestSetDefaultAudioDeviceReportsWhyPipeWireIsUnavailable(t *testing.T) {
	origAvailable, origReason := audio.Available, audio.UnavailableReason
	t.Cleanup(func() { audio.Available, audio.UnavailableReason = origAvailable, origReason })
	audio.Available = func() bool { return false }
	audio.UnavailableReason = func() string {
		return `/run/user/1000/pipewire-0 is owned by uid 0, expected the "wendy" user (uid 1000)`
	}

	resp, err := testAudioService().SetDefaultAudioDevice(context.Background(),
		&agentpb.SetDefaultAudioDeviceRequest{DeviceId: 1})
	if err != nil {
		t.Fatalf("SetDefaultAudioDevice() error = %v", err)
	}
	if resp.GetSuccess() {
		t.Fatal("success = true with no PipeWire session")
	}
	msg := resp.GetErrorMessage()
	if !strings.Contains(msg, "owned by uid 0") {
		t.Errorf("error = %q, want it to carry the specific reason", msg)
	}
	if strings.Contains(msg, "no PipeWire session is running") {
		t.Errorf("error = %q, must not assert that no session is running", msg)
	}
}

func TestSetDefaultAudioDevice(t *testing.T) {
	calls := stubGraph(t, graphFixture, nil)

	resp, err := testAudioService().SetDefaultAudioDevice(context.Background(),
		&agentpb.SetDefaultAudioDeviceRequest{DeviceId: 62})
	if err != nil {
		t.Fatalf("SetDefaultAudioDevice() error = %v", err)
	}
	if !resp.GetSuccess() {
		t.Fatalf("success = false, error = %q", resp.GetErrorMessage())
	}
	// The node ID goes straight through: it is what wpctl takes, and one call
	// covers the device because wpctl infers the direction from the node.
	if want := "set-default 62"; fmt.Sprint(*calls) != "["+want+"]" {
		t.Errorf("wpctl calls = %v, want [%s]", *calls, want)
	}
}

func TestSetDefaultAudioDeviceRejectsUnknown(t *testing.T) {
	calls := stubGraph(t, graphFixture, nil)
	svc := testAudioService()

	// Zero is the "unspecified" sentinel elsewhere in the API and is never a
	// device, so it is rejected outright rather than reported as a failure.
	if _, err := svc.SetDefaultAudioDevice(context.Background(),
		&agentpb.SetDefaultAudioDeviceRequest{DeviceId: 0}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("device ID 0: error = %v, want codes.InvalidArgument", err)
	}

	resp, err := svc.SetDefaultAudioDevice(context.Background(),
		&agentpb.SetDefaultAudioDeviceRequest{DeviceId: 999})
	if err != nil {
		t.Fatalf("unknown device: error = %v, want a failure response", err)
	}
	if resp.GetSuccess() || !strings.Contains(resp.GetErrorMessage(), "999") {
		t.Errorf("unknown device: success = %v, message = %q", resp.GetSuccess(), resp.GetErrorMessage())
	}
	if len(*calls) != 0 {
		t.Errorf("wpctl was called for a device that does not exist: %v", *calls)
	}
}

func TestSetDefaultAudioDeviceReportsWpctlFailure(t *testing.T) {
	stubGraph(t, graphFixture, func(...string) ([]byte, error) {
		return []byte("Object not found"), fmt.Errorf("exit status 1")
	})

	resp, err := testAudioService().SetDefaultAudioDevice(context.Background(),
		&agentpb.SetDefaultAudioDeviceRequest{DeviceId: 43})
	if err != nil {
		t.Fatalf("SetDefaultAudioDevice() error = %v", err)
	}
	if resp.GetSuccess() {
		t.Fatal("success = true despite wpctl failing")
	}
	if !strings.Contains(resp.GetErrorMessage(), "Object not found") {
		t.Errorf("message = %q, want wpctl's own output", resp.GetErrorMessage())
	}
}

func TestSetAudioVolume(t *testing.T) {
	// wpctl takes a fraction and reports one back; the API speaks percentages.
	calls := stubGraph(t, graphFixture, func(args ...string) ([]byte, error) {
		if args[0] == "get-volume" {
			return []byte("Volume: 0.35\n"), nil
		}
		return nil, nil
	})

	got, err := testAudioService().setAudioVolume(context.Background(), 43, 35)
	if err != nil {
		t.Fatalf("setAudioVolume() error = %v", err)
	}
	if got != 35 {
		t.Errorf("volume = %d, want 35", got)
	}
	// The unmute is part of setting a volume: the reported volume does not
	// reflect mute, so a muted node would report a volume while staying silent.
	if want := "[set-volume 43 0.35 set-mute 43 0 get-volume 43]"; fmt.Sprint(*calls) != want {
		t.Errorf("wpctl calls = %v, want %s", *calls, want)
	}
}

func TestSetAudioVolumeReportsWhatTookEffect(t *testing.T) {
	// A device with a coarse hardware mixer lands on a nearby step. Report the
	// value the graph actually holds, not the one that was asked for.
	stubGraph(t, graphFixture, func(args ...string) ([]byte, error) {
		if args[0] == "get-volume" {
			return []byte("Volume: 0.48\n"), nil
		}
		return nil, nil
	})

	got, err := testAudioService().setAudioVolume(context.Background(), 43, 50)
	if err != nil {
		t.Fatalf("setAudioVolume() error = %v", err)
	}
	if got != 48 {
		t.Errorf("volume = %d, want the 48 the graph reports", got)
	}
}

func TestSetAudioVolumeValidates(t *testing.T) {
	stubGraph(t, graphFixture, nil)
	svc := testAudioService()

	if _, err := svc.setAudioVolume(context.Background(), 43, 101); status.Code(err) != codes.InvalidArgument {
		t.Errorf("volume 101: error = %v, want codes.InvalidArgument", err)
	}
	if _, err := svc.setAudioVolume(context.Background(), 0, 50); status.Code(err) != codes.InvalidArgument {
		t.Errorf("device ID 0: error = %v, want codes.InvalidArgument", err)
	}
	if _, err := svc.setAudioVolume(context.Background(), 999, 50); status.Code(err) != codes.NotFound {
		t.Errorf("unknown device: error = %v, want codes.NotFound", err)
	}
}

func TestNodeVolumes(t *testing.T) {
	// Volume is unavailable on some nodes; those are omitted rather than
	// reported as zero, which would look like silence.
	stubGraph(t, graphFixture, func(args ...string) ([]byte, error) {
		if args[1] == "43" {
			return []byte("Volume: 0.50\n"), nil
		}
		return []byte("Node has no volume\n"), nil
	})

	svc := testAudioService()
	resp, err := svc.ListAudioDevices(context.Background(), &agentpb.ListAudioDevicesRequest{})
	if err != nil {
		t.Fatalf("ListAudioDevices() error = %v", err)
	}

	volumes := svc.nodeVolumes(context.Background(), resp.GetDevices())
	if len(volumes) != 1 {
		t.Fatalf("got %d volumes, want only the one node that has a volume", len(volumes))
	}
	if v, ok := volumes[43]; !ok || *v != 50 {
		t.Errorf("volumes[43] = %v, want 50", volumes[43])
	}
}

func TestCaptureTarget(t *testing.T) {
	stubGraph(t, graphFixture, nil)
	ctx := context.Background()

	// pw-record takes an object serial, not the object ID used everywhere else.
	if got, err := captureTarget(ctx, 55); err != nil || got.pwTarget != "88" {
		t.Errorf("captureTarget(55) = %+v, %v; want the USB mic's serial 88", got, err)
	}

	// Unspecified means the default source.
	if got, err := captureTarget(ctx, 0); err != nil || got.pwTarget != "251" {
		t.Errorf("captureTarget(0) = %+v, %v; want the default source's serial 251", got, err)
	}

	// Recording from a speaker is not a thing; say so rather than letting
	// pw-record fail obscurely.
	_, err := captureTarget(ctx, 43)
	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("captureTarget(sink) error = %v, want codes.InvalidArgument", err)
	}

	if _, err := captureTarget(ctx, 999); status.Code(err) != codes.NotFound {
		t.Errorf("captureTarget(unknown) error = %v, want codes.NotFound", err)
	}
}

func TestCaptureTargetFallsBackToAnySource(t *testing.T) {
	// No default.audio.source metadata at all: still record from something.
	noDefaults := strings.Replace(graphFixture,
		`{ "subject": 0, "key": "default.audio.source", "value": { "name": "bluez_input.78:2B:64:76:F3:CE" } }`,
		`{ "subject": 0, "key": "default.video.source", "value": { "name": "v4l2_input.platform" } }`, 1)
	stubGraph(t, noDefaults, nil)

	got, err := captureTarget(context.Background(), 0)
	if err != nil {
		t.Fatalf("captureTarget(0) error = %v", err)
	}
	// The lowest node id wins, so an unspecified device records from the same
	// microphone every time rather than following pw-dump's graph order.
	if got.pwTarget != "251" {
		t.Errorf("captureTarget(0) = %+v, want the lowest-id source's serial 251", got)
	}
}

func TestCaptureTargetNoInputs(t *testing.T) {
	stubGraph(t, `[]`, nil)

	_, err := captureTarget(context.Background(), 0)
	if status.Code(err) != codes.FailedPrecondition {
		t.Errorf("error = %v, want codes.FailedPrecondition", err)
	}
}

// TestNameSensorLinkMounts asserts the ALSA-fallback listing replaces the one
// generic snd-aloop Loopback capture row with one named row per active mount —
// using the pairing name when set and asset-<id> otherwise — and leaves every
// other device untouched.
func TestNameSensorLinkMounts(t *testing.T) {
	const loopCard = 2
	loopback := audio.Node{
		ID:          audio.EncodeAlsaID(loopCard, 1, false),
		Name:        "plughw:2,1",
		Description: "card 2: Loopback [Loopback], device 1: Loopback PCM [Loopback PCM]",
	}
	usb := audio.Node{
		ID:          audio.EncodeAlsaID(0, 0, false),
		Name:        "plughw:0,0",
		Description: "card 0: USB [USB Audio Device], device 0: USB Audio [USB Audio]",
	}

	svc := NewAudioService(zap.NewNop())
	svc.SetSensorLinkNaming(
		func() []audioloop.Mount {
			return []audioloop.Mount{
				{Sub: 0, SourceAssetID: 286, ChannelID: 1, SensorName: "mic0"},
				{Sub: 3, SourceAssetID: 999, ChannelID: 1, SensorName: "mic0"},
			}
		},
		func(id int32) (string, bool) {
			if id == 286 {
				return "parakeet-demo", true
			}
			return "", false
		},
	)

	got := svc.nameSensorLinkMounts([]audio.Node{usb, loopback})
	if len(got) != 3 {
		t.Fatalf("expected 3 rows (usb + 2 mounts), got %d: %+v", len(got), got)
	}
	if got[0] != usb {
		t.Errorf("first row = %+v, want the USB device unchanged", got[0])
	}

	byName := map[string]audio.Node{}
	for _, n := range got {
		byName[n.Name] = n
	}
	paired, ok := byName["plughw:2,1,0"]
	if !ok {
		t.Fatalf("missing named row plughw:2,1,0; got %+v", got)
	}
	if paired.Description != "sensor-link: parakeet-demo · mic0" {
		t.Errorf("paired description = %q, want %q", paired.Description, "sensor-link: parakeet-demo · mic0")
	}
	if paired.IsSink {
		t.Errorf("named mount row must be a capture (input) device")
	}
	if sub, ok := audio.AlsaSubdevice(paired.ID); !ok || sub != 0 {
		t.Errorf("paired ID subdevice = (%d,%v), want (0,true)", sub, ok)
	}
	unpaired, ok := byName["plughw:2,1,3"]
	if !ok {
		t.Fatalf("missing named row plughw:2,1,3; got %+v", got)
	}
	if unpaired.Description != "sensor-link: asset-999 · mic0" {
		t.Errorf("unpaired description = %q, want %q", unpaired.Description, "sensor-link: asset-999 · mic0")
	}

	// No mounts: the Loopback row is returned untouched.
	svc.SetSensorLinkNaming(func() []audioloop.Mount { return nil }, nil)
	same := svc.nameSensorLinkMounts([]audio.Node{usb, loopback})
	if len(same) != 2 || same[1] != loopback {
		t.Errorf("with no mounts, nodes must be unchanged, got %+v", same)
	}
}
