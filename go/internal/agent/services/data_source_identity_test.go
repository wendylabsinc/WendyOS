package services

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/agent/audio"
	"github.com/wendylabsinc/wendy/go/internal/agent/data"
	"github.com/wendylabsinc/wendy/go/internal/agent/ipcam"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
)

// mesh1Graph renders the capture half of the PipeWire graph on Mesh 1, a
// Jetson Orin Nano with a Logitech C920 plugged in, as `pw-dump` reports it.
// The object ids are parameters because they are exactly what changes: the
// first boot measured listed the C920 as 54 and the board's own input as 56.
func mesh1Graph(c920ID, builtinID string) string {
	return `[
  {
    "id": ` + c920ID + `,
    "type": "PipeWire:Interface:Node",
    "info": { "props": {
      "object.serial": 2101, "media.class": "Audio/Source",
      "node.name": "alsa_input.usb-046d_HD_Pro_Webcam_C920_C2DD0F9F-02.analog-stereo",
      "node.description": "C920 PRO HD Webcam Analog Stereo"
    } }
  },
  {
    "id": ` + builtinID + `,
    "type": "PipeWire:Interface:Node",
    "info": { "props": {
      "object.serial": 2102, "media.class": "Audio/Source",
      "node.name": "alsa_input.platform-sound.analog-stereo",
      "node.description": "Built-in Audio Analog Stereo"
    } }
  }
]`
}

const (
	mesh1C920ID    = "audio:alsa_input.usb-046d_HD_Pro_Webcam_C920_C2DD0F9F-02.analog-stereo"
	mesh1BuiltinID = "audio:alsa_input.platform-sound.analog-stereo"
)

// TestMicrophoneIDSurvivesRenumbering is the reason microphones have new IDs.
// A replug or a reboot hands the same two microphones different PipeWire object
// ids; the source IDs must not move with them, and the old number must still
// select the microphone it names at the time.
func TestMicrophoneIDSurvivesRenumbering(t *testing.T) {
	for _, boot := range []struct{ c920, builtin string }{{"54", "56"}, {"61", "47"}} {
		stubGraph(t, mesh1Graph(boot.c920, boot.builtin), nil)
		sources := (&audioDataAdapter{audio: &AudioService{}}).Discover(context.Background())

		c920, ok := sourceByID(sources, mesh1C920ID)
		if !ok {
			t.Fatalf("boot with C920=%s: no source %s in %+v", boot.c920, mesh1C920ID, sources)
		}
		if got, want := c920.Aliases, []string{"audio:" + boot.c920}; len(got) != 1 || got[0] != want[0] {
			t.Errorf("C920 aliases = %q, want %q: the old number must still select it", got, want)
		}
		if c920.Name != "C920 PRO HD Webcam" {
			t.Errorf("C920 name = %q, want the card without its profile", c920.Name)
		}
		// Detail is unchanged, so a campaign's name-fragment selector still
		// matches it.
		if !strings.Contains(c920.Detail, "C920 PRO HD Webcam Analog Stereo") || !strings.Contains(c920.Detail, "alsa_input.usb-046d") {
			t.Errorf("C920 detail = %q, want the description and node name it always carried", c920.Detail)
		}
		builtin, ok := sourceByID(sources, mesh1BuiltinID)
		if !ok {
			t.Fatalf("no source %s in %+v", mesh1BuiltinID, sources)
		}
		if builtin.Name != "Built-in Audio" {
			t.Errorf("built-in name = %q, want %q", builtin.Name, "Built-in Audio")
		}
		if !c920.Healthy || !builtin.Healthy || c920.UnusableReason != "" {
			t.Errorf("health changed: %+v %+v", c920, builtin)
		}
	}
}

// TestMicrophoneIDOnALSAFallback covers a host with no PipeWire session, where
// the old number packed the ALSA card index. The new ID carries the card's id
// instead, and the audio-hub endpoints keep their health but gain a short
// reason.
func TestMicrophoneIDOnALSAFallback(t *testing.T) {
	sources := discoverWithAlsaFixtures(t, jetsonAplayFixture, jetsonArecordFixture)

	c920, ok := sourceByID(sources, "audio:alsa:C920:0")
	if !ok {
		t.Fatalf("no audio:alsa:C920:0 in %+v", sources)
	}
	if c920.Name != "HD Pro Webcam C920" {
		t.Errorf("C920 name = %q, want the card's long name", c920.Name)
	}
	if len(c920.Aliases) != 1 || c920.Aliases[0] != "audio:16777217" {
		t.Errorf("C920 aliases = %q, want the ALSA-encoded number it used to be listed as", c920.Aliases)
	}

	hub, ok := sourceByID(sources, "audio:alsa:APE:3")
	if !ok {
		t.Fatalf("no audio:alsa:APE:3 in %+v", sources)
	}
	if hub.Healthy {
		t.Fatal("an audio-hub endpoint became healthy; this change must not alter health")
	}
	if hub.UnusableReason != audioHubUnusableReason {
		t.Errorf("hub reason = %q, want %q", hub.UnusableReason, audioHubUnusableReason)
	}
	if hub.Name != "NVIDIA Jetson Orin Nano APE, device 3" {
		t.Errorf("hub name = %q", hub.Name)
	}
	if !strings.Contains(hub.Detail, audioHubDMAEndpointReason) {
		t.Errorf("hub detail lost the full reason: %q", hub.Detail)
	}
}

// TestAudioSourceIDs pins the ID grammar: safe characters only, never the old
// numeric form, unique within one listing, and the old number when a node has
// no stable name at all.
func TestAudioSourceIDs(t *testing.T) {
	nodes := []audio.Node{
		{ID: 44, StableName: "bluez_input.78:2B:64:76:F3:CE"},
		{ID: 45, StableName: "odd name/with $pecials"},
		{ID: 46, StableName: "1234"},
		{ID: 47, StableName: "dup mic"},
		{ID: 48, StableName: "dup/mic"},
		{ID: 49},
		{ID: 50, StableName: "sensor-link:" + stableNamePart("urn:wendy:x") + ":" + stableNamePart("mic 0")},
	}
	want := []string{
		"audio:bluez_input.78:2B:64:76:F3:CE",
		"audio:odd_name_with__pecials",
		"audio:node-1234",
		"audio:dup_mic",
		"audio:dup_mic.2",
		"audio:49",
		"audio:sensor-link:urn_wendy_x:mic_0",
	}
	got := audioSourceIDs(nodes)
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("node %d: id = %q, want %q", nodes[i].ID, got[i], want[i])
		}
	}
	for _, id := range got[:5] {
		if data.IsLegacyAudioSourceID(id) {
			t.Errorf("stable id %q reads as the old numeric form", id)
		}
	}
}

func TestAudioSourceName(t *testing.T) {
	for _, tc := range []struct {
		node audio.Node
		want string
	}{
		{audio.Node{Description: "C920 PRO HD Webcam Analog Stereo"}, "C920 PRO HD Webcam"},
		{audio.Node{Description: "USB Audio Device Mono"}, "USB Audio Device"},
		{audio.Node{Description: "HDA Intel PCH Digital Stereo (IEC958)"}, "HDA Intel PCH"},
		{audio.Node{Description: "Bose Revolve II SoundLink"}, "Bose Revolve II SoundLink"},
		{audio.Node{Description: "C920 [HD Pro Webcam C920], device 0: USB Audio [USB Audio]"}, "HD Pro Webcam C920"},
		{audio.Node{Description: "sensor-link: esp32-cam · microphone"}, "esp32-cam · microphone"},
		{audio.Node{Description: "Stereo", Name: "alsa_input.x"}, "Stereo"},
		{audio.Node{Name: "alsa_input.x"}, "alsa_input.x"},
	} {
		if got := audioSourceName(tc.node); got != tc.want {
			t.Errorf("audioSourceName(%q) = %q, want %q", tc.node.Description, got, tc.want)
		}
	}
}

// TestAudioStartOpensTheNodeAStableIDNames proves an episode that selected a
// stable ID records the microphone it names now, whatever its object id became.
func TestAudioStartOpensTheNodeAStableIDNames(t *testing.T) {
	stubGraph(t, mesh1Graph("61", "47"), nil)
	var opened []uint32
	adapter := &audioDataAdapter{
		audio: &AudioService{},
		openStream: func(_ context.Context, id uint32) (audioStream, error) {
			opened = append(opened, id)
			return &fakeAudioStream{chunks: [][]byte{make([]byte, 960)}}, nil
		},
	}
	source := data.Source{ID: mesh1C920ID, Kind: "audio", ClockDomain: pipewireCaptureClockDomain, Healthy: true}
	session := data.CaptureSession{ID: "ep", Directory: t.TempDir(), RequestBootNanos: 1}
	running, err := adapter.Start(context.Background(), session, []data.Source{source, {ID: "telemetry", Kind: "telemetry"}})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if _, err := running.Stop(context.Background()); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if len(opened) != 1 || opened[0] != 61 {
		t.Fatalf("opened nodes %v, want only the C920's current object id 61", opened)
	}
}

func TestAudioStartRefusesAMicrophoneThatIsGone(t *testing.T) {
	stubGraph(t, mesh1Graph("61", "47"), nil)
	adapter := &audioDataAdapter{
		audio: &AudioService{},
		openStream: func(context.Context, uint32) (audioStream, error) {
			t.Fatal("opened a stream for a microphone that is not present")
			return nil, nil
		},
	}
	source := data.Source{ID: "audio:alsa_input.usb-unplugged.analog-stereo", Kind: "audio", Healthy: true}
	_, err := adapter.Start(context.Background(), data.CaptureSession{Directory: t.TempDir()}, []data.Source{source})
	if err == nil || !strings.Contains(err.Error(), "no longer present") {
		t.Fatalf("err = %v, want the microphone named as gone", err)
	}
}

func TestCameraSourceName(t *testing.T) {
	for _, tc := range []struct{ name, path, want string }{
		{"HD Pro Webcam C920: HD Pro Webc", "/dev/video0", "HD Pro Webcam C920"},
		{"HD Pro Webcam C920", "/dev/video0", "HD Pro Webcam C920"},
		{"imx219 10-0010", "/dev/video0", "imx219 10-0010"},
		{"Cam: Other", "/dev/video0", "Cam: Other"},
		{"", "/dev/video4", "/dev/video4"},
	} {
		if got := cameraSourceName(tc.name, tc.path); got != tc.want {
			t.Errorf("cameraSourceName(%q) = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestNetworkCameraSourcesSayWhyTheyAreNotUsable keeps today's health rule
// (online and a stored login) and checks each failing half has its own reason.
func TestNetworkCameraSourcesSayWhyTheyAreNotUsable(t *testing.T) {
	video := newIPTestService(t)
	ready, err := video.registry.Upsert(ipcam.Camera{MAC: "aa:00:00:00:00:01", Model: "RLC-520A", Address: "10.98.0.50"})
	if err != nil {
		t.Fatal(err)
	}
	if err := video.credentials.Set(ready.MAC, ipcam.Credential{Username: "admin"}); err != nil {
		t.Fatal(err)
	}
	noLogin, err := video.registry.Upsert(ipcam.Camera{MAC: "aa:00:00:00:00:02", Address: "10.98.0.51"})
	if err != nil {
		t.Fatal(err)
	}
	offline, err := video.registry.Upsert(ipcam.Camera{MAC: "aa:00:00:00:00:03", Model: "RLC-810A", Address: "10.98.0.52"})
	if err != nil {
		t.Fatal(err)
	}
	if err := video.credentials.Set(offline.MAC, ipcam.Credential{Username: "admin"}); err != nil {
		t.Fatal(err)
	}
	video.registry.MarkSeen(offline.MAC, offline.Address, false)

	sources := (&cameraDataAdapter{video: video}).Discover(context.Background())
	for _, tc := range []struct {
		camera  ipcam.Camera
		healthy bool
		name    string
		reason  string
	}{
		{ready, true, "RLC-520A", ""},
		{noLogin, false, "Network camera", "no login stored"},
		{offline, false, "RLC-810A", "camera offline"},
	} {
		id := fmt.Sprintf("ipcamera:%d", tc.camera.ID)
		source, ok := sourceByID(sources, id)
		if !ok {
			t.Fatalf("no %s in %+v", id, sources)
		}
		if source.Healthy != tc.healthy || source.Name != tc.name || source.UnusableReason != tc.reason {
			t.Errorf("%s = healthy %v, name %q, reason %q; want %v, %q, %q", id, source.Healthy, source.Name, source.UnusableReason, tc.healthy, tc.name, tc.reason)
		}
	}
}

func TestROS2SourcesAreNamed(t *testing.T) {
	runtime := newRecordingROS2Runtime(testSidecar())
	sources := testAdapter(t, runtime).Discover(context.Background())
	domain, ok := sourceByID(sources, "ros2:rmw_cyclonedds_cpp:domain-42")
	if !ok {
		t.Fatalf("no domain source in %+v", sources)
	}
	if domain.Name != "Domain 42 (Cyclone DDS), all topics" {
		t.Errorf("domain name = %q", domain.Name)
	}
	topic, ok := sourceByID(sources, "ros2:rmw_cyclonedds_cpp:domain-42:/chatter")
	if !ok || topic.Name != "/chatter" {
		t.Errorf("topic source = %+v, want it named for its topic", topic)
	}

	runtime.failListing(true)
	failed := testAdapter(t, runtime).Discover(context.Background())
	if len(failed) != 1 || failed[0].Healthy || failed[0].UnusableReason != "topic listing failed" || failed[0].Name != "Domain 42 (Cyclone DDS)" {
		t.Errorf("failed domain = %+v, want an unhealthy, named domain whose reason is the failed listing", failed)
	}
}

// TestSourcesRPCCarriesNameAndReason checks the wire: every source has a name,
// so a client can tell a current agent from an older one, and a reason travels
// only with an unhealthy source.
func TestSourcesRPCCarriesNameAndReason(t *testing.T) {
	m, err := data.NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	service := NewDataService(m)
	m.SetSourceProvider(func(context.Context) []data.Source {
		return []data.Source{
			{ID: "x:named", Kind: "camera", Healthy: true, Name: "Named", UnusableReason: "stale reason"},
			{ID: "x:unnamed", Kind: "camera", Healthy: false, UnusableReason: "camera offline"},
		}
	})
	response, err := service.Sources(context.Background(), &agentpbv2.DataSourcesRequest{})
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]*agentpbv2.DataSource{}
	for _, source := range response.GetSources() {
		byID[source.GetId()] = source
		if source.GetName() == "" {
			t.Errorf("%s has no name", source.GetId())
		}
	}
	if got := byID["x:named"]; got.GetName() != "Named" || got.GetUnusableReason() != "" {
		t.Errorf("healthy source = %+v, want its name and no reason", got)
	}
	if got := byID["x:unnamed"]; got.GetName() != "x:unnamed" || got.GetUnusableReason() != "camera offline" {
		t.Errorf("unhealthy unnamed source = %+v, want the ID as its name and its reason", got)
	}
	if got := byID["applications"]; got.GetName() != "Application records" {
		t.Errorf("applications = %+v", got)
	}
}
