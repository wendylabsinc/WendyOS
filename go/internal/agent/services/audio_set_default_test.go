package services

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"testing"

	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	agentpb "github.com/wendylabsinc/wendy/proto/gen/agentpb"
)

// TestDecodeALSAID verifies that decodeALSAID correctly inverts the encoding
// applied by parseALSAOutput: id = ((card << 8) | device) + 1.
func TestDecodeALSAID(t *testing.T) {
	tests := []struct {
		name       string
		id         uint32
		wantCard   uint64
		wantDevice uint64
	}{
		{
			name:       "card 0 device 0",
			id:         1, // ((0<<8)|0)+1
			wantCard:   0,
			wantDevice: 0,
		},
		{
			name:       "card 0 device 1",
			id:         2, // ((0<<8)|1)+1
			wantCard:   0,
			wantDevice: 1,
		},
		{
			name:       "card 1 device 0",
			id:         257, // ((1<<8)|0)+1
			wantCard:   1,
			wantDevice: 0,
		},
		{
			name:       "card 1 device 1",
			id:         258, // ((1<<8)|1)+1
			wantCard:   1,
			wantDevice: 1,
		},
		{
			name:       "card 2 device 3",
			id:         516, // ((2<<8)|3)+1
			wantCard:   2,
			wantDevice: 3,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			card, device := decodeALSAID(tt.id)
			if card != tt.wantCard {
				t.Errorf("decodeALSAID(%d) card = %d; want %d", tt.id, card, tt.wantCard)
			}
			if device != tt.wantDevice {
				t.Errorf("decodeALSAID(%d) device = %d; want %d", tt.id, device, tt.wantDevice)
			}
		})
	}
}

// TestDecodeALSARoundTrip verifies that encoding and decoding are inverse operations.
func TestDecodeALSARoundTrip(t *testing.T) {
	for card := uint64(0); card < 4; card++ {
		for dev := uint64(0); dev < 4; dev++ {
			// Replicate the encoding from parseALSAOutput.
			encoded := ((card << 8) | dev) + 1
			id := uint32(encoded)

			gotCard, gotDevice := decodeALSAID(id)
			if gotCard != card || gotDevice != dev {
				t.Errorf("round-trip card=%d device=%d: got card=%d device=%d", card, dev, gotCard, gotDevice)
			}
		}
	}
}

// TestJSONPropMatches verifies that jsonPropMatches correctly handles both
// JSON string and JSON number values for pw-dump props.
func TestJSONPropMatches(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		key     string
		wantStr string
		want    bool
	}{
		{"string value matches", `{"alsa.card": "0"}`, "alsa.card", "0", true},
		{"string value no match", `{"alsa.card": "1"}`, "alsa.card", "0", false},
		{"number value matches", `{"alsa.card": 0}`, "alsa.card", "0", true},
		{"number value no match", `{"alsa.card": 2}`, "alsa.card", "0", false},
		{"key absent", `{"alsa.device": "0"}`, "alsa.card", "0", false},
		{"device string matches", `{"alsa.device": "2"}`, "alsa.device", "2", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var props map[string]json.RawMessage
			if err := json.Unmarshal([]byte(tt.raw), &props); err != nil {
				t.Fatalf("unmarshal props: %v", err)
			}
			got := jsonPropMatches(props, tt.key, tt.wantStr)
			if got != tt.want {
				t.Errorf("jsonPropMatches(%q, %q) = %v; want %v", tt.key, tt.wantStr, got, tt.want)
			}
		})
	}
}

// TestExtractPactlPropertyValue verifies parsing of pactl list output property lines.
func TestExtractPactlPropertyValue(t *testing.T) {
	tests := []struct {
		line string
		want string
	}{
		{`alsa.card = "0"`, "0"},
		{`alsa.card = "1"`, "1"},
		{`alsa.device = "0"`, "0"},
		{`alsa.device = "3"`, "3"},
		{`alsa.card = 0`, "0"},
		{`no equals sign`, ""},
	}

	for _, tt := range tests {
		got := extractPactlPropertyValue(tt.line)
		if got != tt.want {
			t.Errorf("extractPactlPropertyValue(%q) = %q; want %q", tt.line, got, tt.want)
		}
	}
}

// samplePactlSinksOutput returns a representative "pactl list sinks" block
// containing two sink entries: one for ALSA card 0 device 0 and one for card 1
// device 2. It is shared across TestParsePulseAudioOutput sub-tests.
const samplePactlSinksOutput = `
Sink #0
	State: RUNNING
	Name: alsa_output.pci-0000_00_1f.3.analog-stereo
	Description: Built-in Audio Analog Stereo
	Driver: module-alsa-card.c
	Properties:
		alsa.card = "0"
		alsa.device = "0"
		alsa.card_name = "HDA Intel PCH"

Sink #1
	State: SUSPENDED
	Name: alsa_output.pci-0000_01_00.1.hdmi-stereo
	Description: HDMI / DisplayPort
	Driver: module-alsa-card.c
	Properties:
		alsa.card = "1"
		alsa.device = "2"
		alsa.card_name = "HDA Intel HDMI"
`

const samplePactlSourcesOutput = `
Source #0
	State: RUNNING
	Name: alsa_input.pci-0000_00_1f.3.analog-stereo
	Description: Built-in Audio Analog Stereo (microphone)
	Driver: module-alsa-card.c
	Properties:
		alsa.card = "0"
		alsa.device = "0"
		alsa.card_name = "HDA Intel PCH"

Source #1
	State: SUSPENDED
	Name: alsa_input.pci-0000_00_1f.3.analog-mono
	Description: Some other input
	Driver: module-alsa-card.c
	Properties:
		alsa.card = "0"
		alsa.device = "1"
		alsa.card_name = "HDA Intel PCH"
`

// samplePactlSourcesWithMonitorOutput contains a mix of a real capture source
// and a monitor source (auto-created by PulseAudio for the corresponding sink).
// Both share the same alsa.card and alsa.device, so without filtering the
// monitor would be erroneously treated as a valid capture device.
const samplePactlSourcesWithMonitorOutput = `
Source #0
	State: SUSPENDED
	Name: alsa_output.pci-0000_00_1f.3.analog-stereo.monitor
	Description: Monitor of Built-in Audio Analog Stereo
	Driver: module-alsa-card.c
	Properties:
		alsa.card = "0"
		alsa.device = "0"
		alsa.card_name = "HDA Intel PCH"

Source #1
	State: RUNNING
	Name: alsa_input.pci-0000_00_1f.3.analog-stereo
	Description: Built-in Audio Analog Stereo (microphone)
	Driver: module-alsa-card.c
	Properties:
		alsa.card = "0"
		alsa.device = "0"
		alsa.card_name = "HDA Intel PCH"
`

// TestParsePulseAudioOutput verifies that parsePulseAudioOutput correctly
// extracts matching sink/source names from representative pactl list output.
func TestParsePulseAudioOutput(t *testing.T) {
	tests := []struct {
		name     string
		output   string
		card     uint64
		device   uint64
		category string
		want     []string // expected .name fields in order
	}{
		{
			name:     "matches first sink by alsa.card and alsa.device",
			output:   samplePactlSinksOutput,
			card:     0,
			device:   0,
			category: "sinks",
			want:     []string{"alsa_output.pci-0000_00_1f.3.analog-stereo"},
		},
		{
			name:     "matches second sink by alsa.card and alsa.device",
			output:   samplePactlSinksOutput,
			card:     1,
			device:   2,
			category: "sinks",
			want:     []string{"alsa_output.pci-0000_01_00.1.hdmi-stereo"},
		},
		{
			name:     "no match when card does not exist",
			output:   samplePactlSinksOutput,
			card:     99,
			device:   0,
			category: "sinks",
			want:     nil,
		},
		{
			name:     "no match when device does not match",
			output:   samplePactlSinksOutput,
			card:     0,
			device:   5,
			category: "sinks",
			want:     nil,
		},
		{
			name:     "source match returned with correct category",
			output:   samplePactlSourcesOutput,
			card:     0,
			device:   0,
			category: "sources",
			want:     []string{"alsa_input.pci-0000_00_1f.3.analog-stereo"},
		},
		{
			name:     "source match on device 1 not device 0",
			output:   samplePactlSourcesOutput,
			card:     0,
			device:   1,
			category: "sources",
			want:     []string{"alsa_input.pci-0000_00_1f.3.analog-mono"},
		},
		{
			name:     "empty output returns no matches",
			output:   "",
			card:     0,
			device:   0,
			category: "sinks",
			want:     nil,
		},
		{
			// Monitor sources (name ending in ".monitor") share alsa.card/alsa.device
			// with the corresponding sink and must be excluded to avoid setting the
			// default input to a loopback monitor instead of a real capture device.
			name:     "monitor source excluded, real source included",
			output:   samplePactlSourcesWithMonitorOutput,
			card:     0,
			device:   0,
			category: "sources",
			want:     []string{"alsa_input.pci-0000_00_1f.3.analog-stereo"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parsePulseAudioOutput(tt.output, tt.card, tt.device, tt.category)

			if len(got) != len(tt.want) {
				t.Fatalf("parsePulseAudioOutput: got %d match(es) %v; want %d %v",
					len(got), got, len(tt.want), tt.want)
			}
			for i, m := range got {
				if m.name != tt.want[i] {
					t.Errorf("match[%d].name = %q; want %q", i, m.name, tt.want[i])
				}
				if m.category != tt.category {
					t.Errorf("match[%d].category = %q; want %q", i, m.category, tt.category)
				}
			}
		})
	}
}

// TestParseALSAOutputIDEncoding verifies that parseALSAOutput uses the same
// encoding that decodeALSAID expects.
func TestParseALSAOutputIDEncoding(t *testing.T) {
	// Simulate aplay -l output with two devices.
	output := `**** List of PLAYBACK Hardware Devices ****
card 0: PCH [HDA Intel PCH], device 0: ALC236 Analog [ALC236 Analog]
  Subdevices: 1/1
  Subdevice #0: subdevice #0
card 1: HDMI [HDA Intel HDMI], device 3: HDMI 0 [HDMI 0]
  Subdevices: 1/1
  Subdevice #0: subdevice #0
`
	devices := parseALSAOutput(output, agentpb.AudioDeviceType_AUDIO_DEVICE_TYPE_OUTPUT)

	if len(devices) != 2 {
		t.Fatalf("parseALSAOutput: len = %d; want 2", len(devices))
	}

	// Verify card 0 device 0 → id 1.
	card, dev := decodeALSAID(devices[0].Id)
	if card != 0 || dev != 0 {
		t.Errorf("devices[0]: decoded card=%d device=%d; want card=0 device=0", card, dev)
	}

	// Verify card 1 device 3 → id ((1<<8)|3)+1 = 260.
	card, dev = decodeALSAID(devices[1].Id)
	if card != 1 || dev != 3 {
		t.Errorf("devices[1]: decoded card=%d device=%d; want card=1 device=3", card, dev)
	}
}

// makePWNode is a test helper that builds a pwDumpNode with the given id, type,
// and property key/value pairs (alternating key, value strings).
func makePWNode(id uint32, nodeType string, props ...string) pwDumpNode {
	node := pwDumpNode{ID: id, Type: nodeType}
	if len(props) > 0 {
		node.Info.Props = make(map[string]json.RawMessage)
		for i := 0; i+1 < len(props); i += 2 {
			raw, _ := json.Marshal(props[i+1])
			node.Info.Props[props[i]] = raw
		}
	}
	return node
}

// TestFilterPipeWireNodeIDs_LegacyProps verifies matching via the legacy
// alsa.card / alsa.device property names.
func TestFilterPipeWireNodeIDs_LegacyProps(t *testing.T) {
	nodes := []pwDumpNode{
		makePWNode(10, "PipeWire:Interface:Node",
			"media.class", "Audio/Sink",
			"alsa.card", "0",
			"alsa.device", "0",
		),
	}
	got := filterPipeWireNodeIDs(nodes, 0, 0)
	if len(got) != 1 || got[0] != "10" {
		t.Errorf("legacy props: got %v; want [10]", got)
	}
}

// TestFilterPipeWireNodeIDs_APIProps verifies matching via the newer
// api.alsa.card / api.alsa.pcm.device property names.
func TestFilterPipeWireNodeIDs_APIProps(t *testing.T) {
	nodes := []pwDumpNode{
		makePWNode(20, "PipeWire:Interface:Node",
			"media.class", "Audio/Source",
			"api.alsa.card", "1",
			"api.alsa.pcm.device", "2",
		),
	}
	got := filterPipeWireNodeIDs(nodes, 1, 2)
	if len(got) != 1 || got[0] != "20" {
		t.Errorf("api props: got %v; want [20]", got)
	}
}

// TestFilterPipeWireNodeIDs_NonNodeTypeFiltered verifies that objects whose
// type is not PipeWire:Interface:Node are excluded even when ALSA properties match.
func TestFilterPipeWireNodeIDs_NonNodeTypeFiltered(t *testing.T) {
	nodes := []pwDumpNode{
		// PipeWire:Interface:Device — should be filtered out.
		makePWNode(30, "PipeWire:Interface:Device",
			"media.class", "Audio/Sink",
			"alsa.card", "0",
			"alsa.device", "0",
		),
		// PipeWire:Interface:Port — should be filtered out.
		makePWNode(31, "PipeWire:Interface:Port",
			"media.class", "Audio/Source",
			"alsa.card", "0",
			"alsa.device", "0",
		),
		// PipeWire:Interface:Node with correct props — should be included.
		makePWNode(32, "PipeWire:Interface:Node",
			"media.class", "Audio/Sink",
			"alsa.card", "0",
			"alsa.device", "0",
		),
	}
	got := filterPipeWireNodeIDs(nodes, 0, 0)
	if len(got) != 1 || got[0] != "32" {
		t.Errorf("non-node filter: got %v; want [32]", got)
	}
}

// TestFilterPipeWireNodeIDs_MultipleMatches verifies that both an Audio/Sink
// and an Audio/Source node for the same ALSA card/device are returned.
func TestFilterPipeWireNodeIDs_MultipleMatches(t *testing.T) {
	nodes := []pwDumpNode{
		makePWNode(40, "PipeWire:Interface:Node",
			"media.class", "Audio/Sink",
			"alsa.card", "2",
			"alsa.device", "1",
		),
		makePWNode(41, "PipeWire:Interface:Node",
			"media.class", "Audio/Source",
			"alsa.card", "2",
			"alsa.device", "1",
		),
		// Different card — should not be included.
		makePWNode(42, "PipeWire:Interface:Node",
			"media.class", "Audio/Sink",
			"alsa.card", "3",
			"alsa.device", "1",
		),
	}
	got := filterPipeWireNodeIDs(nodes, 2, 1)
	sort.Strings(got)
	if len(got) != 2 || got[0] != "40" || got[1] != "41" {
		t.Errorf("multiple matches: got %v; want [40 41]", got)
	}
}

// TestFilterPipeWireNodeIDs_NoMatch verifies that an empty slice is returned
// when no nodes match the requested card/device.
func TestFilterPipeWireNodeIDs_NoMatch(t *testing.T) {
	nodes := []pwDumpNode{
		makePWNode(50, "PipeWire:Interface:Node",
			"media.class", "Audio/Sink",
			"alsa.card", "5",
			"alsa.device", "0",
		),
	}
	got := filterPipeWireNodeIDs(nodes, 0, 0)
	if len(got) != 0 {
		t.Errorf("no match: got %v; want []", got)
	}
}

// TestFilterPipeWireNodeIDs_NonAudioMediaClassFiltered verifies that Node objects
// with a non-Audio media.class (e.g. Video/Source) are excluded.
func TestFilterPipeWireNodeIDs_NonAudioMediaClassFiltered(t *testing.T) {
	nodes := []pwDumpNode{
		makePWNode(60, "PipeWire:Interface:Node",
			"media.class", "Video/Source",
			"alsa.card", "0",
			"alsa.device", "0",
		),
		makePWNode(61, "PipeWire:Interface:Node",
			"media.class", "Midi/Sink",
			"alsa.card", "0",
			"alsa.device", "0",
		),
		makePWNode(62, "PipeWire:Interface:Node",
			"media.class", "Audio/Sink",
			"alsa.card", "0",
			"alsa.device", "0",
		),
	}
	got := filterPipeWireNodeIDs(nodes, 0, 0)
	if len(got) != 1 || got[0] != "62" {
		t.Errorf("non-audio filter: got %v; want [62]", got)
	}
}

// TestFilterPipeWireNodeIDs_VirtualSourceFiltered verifies that virtual/monitor
// nodes (media.class == "Audio/Source/Virtual") are excluded even when their
// ALSA card/device properties match. Only exact "Audio/Sink" and "Audio/Source"
// classes should be accepted.
func TestFilterPipeWireNodeIDs_VirtualSourceFiltered(t *testing.T) {
	nodes := []pwDumpNode{
		// Virtual/monitor source — must be excluded.
		makePWNode(70, "PipeWire:Interface:Node",
			"media.class", "Audio/Source/Virtual",
			"alsa.card", "0",
			"alsa.device", "0",
		),
		// Real sink — must be included.
		makePWNode(71, "PipeWire:Interface:Node",
			"media.class", "Audio/Sink",
			"alsa.card", "0",
			"alsa.device", "0",
		),
		// Real source — must be included.
		makePWNode(72, "PipeWire:Interface:Node",
			"media.class", "Audio/Source",
			"alsa.card", "0",
			"alsa.device", "0",
		),
	}
	got := filterPipeWireNodeIDs(nodes, 0, 0)
	sort.Strings(got)
	if len(got) != 2 || got[0] != "71" || got[1] != "72" {
		t.Errorf("virtual source filter: got %v; want [71 72]", got)
	}
}

// TestFilterPipeWireNodeIDs_ZeroIDFiltered verifies that nodes with ID 0 are
// excluded because valid PipeWire node IDs are positive.
func TestFilterPipeWireNodeIDs_ZeroIDFiltered(t *testing.T) {
	nodes := []pwDumpNode{
		// ID 0 — must be excluded.
		makePWNode(0, "PipeWire:Interface:Node",
			"media.class", "Audio/Sink",
			"alsa.card", "0",
			"alsa.device", "0",
		),
		// ID 1 — must be included.
		makePWNode(1, "PipeWire:Interface:Node",
			"media.class", "Audio/Source",
			"alsa.card", "0",
			"alsa.device", "0",
		),
	}
	got := filterPipeWireNodeIDs(nodes, 0, 0)
	if len(got) != 1 || got[0] != "1" {
		t.Errorf("zero ID filter: got %v; want [1]", got)
	}
}

// TestFilterPipeWireNodeIDs_CrossFamilyMixingRejected verifies that a node whose
// properties span both property families (e.g. "alsa.card" from the legacy family
// but "api.alsa.pcm.device" from the native PipeWire family) is NOT matched.
// Each family must match as a pair; mixing across families must be rejected.
func TestFilterPipeWireNodeIDs_CrossFamilyMixingRejected(t *testing.T) {
	nodes := []pwDumpNode{
		// alsa.card (legacy) paired with api.alsa.pcm.device (native) — cross-family, must be rejected.
		makePWNode(80, "PipeWire:Interface:Node",
			"media.class", "Audio/Sink",
			"alsa.card", "0",
			"api.alsa.pcm.device", "0",
		),
		// api.alsa.card (native) paired with alsa.device (legacy) — cross-family, must be rejected.
		makePWNode(81, "PipeWire:Interface:Node",
			"media.class", "Audio/Source",
			"api.alsa.card", "0",
			"alsa.device", "0",
		),
		// Correct legacy pair — must be included.
		makePWNode(82, "PipeWire:Interface:Node",
			"media.class", "Audio/Sink",
			"alsa.card", "0",
			"alsa.device", "0",
		),
	}
	got := filterPipeWireNodeIDs(nodes, 0, 0)
	if len(got) != 1 || got[0] != "82" {
		t.Errorf("cross-family mixing: got %v; want [82]", got)
	}
}

// --- SetDefaultAudioDevice flow tests ---
//
// These tests exercise the main control-flow branches of SetDefaultAudioDevice
// by injecting mock implementations of the package-level command runner vars
// (wpctlSetDefault, pactlSetDefault, resolvePipeWireNodeIDsFn,
// resolvePulseAudioSinkOrSourceFn) so no real system commands are executed.

// restoreSetDefaultVars saves and restores the package-level injectable vars
// used by SetDefaultAudioDevice and setPulseAudioDefaultByALSA.
func restoreSetDefaultVars(t *testing.T) {
	t.Helper()
	origWpctl := wpctlSetDefault
	origPactl := pactlSetDefault
	origPW := resolvePipeWireNodeIDsFn
	origPA := resolvePulseAudioSinkOrSourceFn
	t.Cleanup(func() {
		wpctlSetDefault = origWpctl
		pactlSetDefault = origPactl
		resolvePipeWireNodeIDsFn = origPW
		resolvePulseAudioSinkOrSourceFn = origPA
	})
}

// TestSetDefaultAudioDevice_OutOfRangeIDReturnsInvalidArgument verifies that a
// device ID encoding a card number > 255 (outside the ALSA encoding range) is
// rejected with codes.InvalidArgument before any system commands are attempted.
func TestSetDefaultAudioDevice_OutOfRangeIDReturnsInvalidArgument(t *testing.T) {
	restoreSetDefaultVars(t)

	resolvePipeWireNodeIDsFn = func(_ context.Context, _, _ uint64) []string {
		t.Error("resolvePipeWireNodeIDsFn should not be called for out-of-range device ID")
		return nil
	}
	resolvePulseAudioSinkOrSourceFn = func(_ context.Context, _, _ uint64) []pulseAudioMatch {
		t.Error("resolvePulseAudioSinkOrSourceFn should not be called for out-of-range device ID")
		return nil
	}

	svc := NewAudioService(zap.NewNop())
	// ID 65537: encoded = 65536 = 0x10000, card = 256 (out of range), device = 0.
	_, err := svc.SetDefaultAudioDevice(context.Background(), &agentpb.SetDefaultAudioDeviceRequest{DeviceId: 65537})
	if err == nil {
		t.Fatal("expected error for out-of-range device ID, got nil")
	}
	st, ok := status.FromError(err)
	if !ok {
		t.Fatalf("expected gRPC status error, got %T: %v", err, err)
	}
	if st.Code() != codes.InvalidArgument {
		t.Errorf("got code %v; want %v", st.Code(), codes.InvalidArgument)
	}
}

// TestSetDefaultAudioDevice_ZeroIDReturnsInvalidArgument verifies that passing
// DeviceId 0 — which is not a valid encoded ALSA device ID — causes
// SetDefaultAudioDevice to return a gRPC status error with code
// codes.InvalidArgument without invoking any system commands.
func TestSetDefaultAudioDevice_ZeroIDReturnsInvalidArgument(t *testing.T) {
	restoreSetDefaultVars(t)

	resolvePipeWireNodeIDsFn = func(_ context.Context, _, _ uint64) []string {
		t.Error("resolvePipeWireNodeIDsFn should not be called for device_id=0")
		return nil
	}
	resolvePulseAudioSinkOrSourceFn = func(_ context.Context, _, _ uint64) []pulseAudioMatch {
		t.Error("resolvePulseAudioSinkOrSourceFn should not be called for device_id=0")
		return nil
	}
	wpctlSetDefault = func(_ context.Context, _ string) ([]byte, error) {
		t.Error("wpctlSetDefault should not be called for device_id=0")
		return nil, nil
	}
	pactlSetDefault = func(_ context.Context, _, _ string) ([]byte, error) {
		t.Error("pactlSetDefault should not be called for device_id=0")
		return nil, nil
	}

	svc := NewAudioService(zap.NewNop())
	_, err := svc.SetDefaultAudioDevice(context.Background(), &agentpb.SetDefaultAudioDeviceRequest{DeviceId: 0})
	if err == nil {
		t.Fatal("SetDefaultAudioDevice(DeviceId=0): expected error, got nil")
	}
	st, ok := status.FromError(err)
	if !ok {
		t.Fatalf("SetDefaultAudioDevice(DeviceId=0): expected gRPC status error, got %T: %v", err, err)
	}
	if st.Code() != codes.InvalidArgument {
		t.Errorf("SetDefaultAudioDevice(DeviceId=0): got status code %v; want %v", st.Code(), codes.InvalidArgument)
	}
}

// TestSetDefaultAudioDevice_AllPipeWireSucceed verifies that when all wpctl
// calls succeed the response is Success:true and PulseAudio is never consulted.
func TestSetDefaultAudioDevice_AllPipeWireSucceed(t *testing.T) {
	restoreSetDefaultVars(t)

	// Two PipeWire nodes for card 0 device 0 (sink + source).
	resolvePipeWireNodeIDsFn = func(_ context.Context, card, device uint64) []string {
		if card == 0 && device == 0 {
			return []string{"10", "11"}
		}
		return nil
	}

	var wpctlCalls []string
	wpctlSetDefault = func(_ context.Context, id string) ([]byte, error) {
		wpctlCalls = append(wpctlCalls, id)
		return []byte(""), nil
	}

	pactlSetDefault = func(_ context.Context, _, _ string) ([]byte, error) {
		t.Error("pactlSetDefault should not be called when all wpctl calls succeed")
		return nil, errors.New("unexpected pactl call")
	}
	resolvePulseAudioSinkOrSourceFn = func(_ context.Context, _, _ uint64) []pulseAudioMatch {
		t.Error("resolvePulseAudioSinkOrSourceFn should not be called when all wpctl calls succeed")
		return nil
	}

	svc := NewAudioService(zap.NewNop())
	// deviceID for card 0 device 0: ((0<<8)|0)+1 = 1
	resp, err := svc.SetDefaultAudioDevice(context.Background(), &agentpb.SetDefaultAudioDeviceRequest{DeviceId: 1})
	if err != nil {
		t.Fatalf("SetDefaultAudioDevice returned unexpected error: %v", err)
	}
	if !resp.GetSuccess() {
		msg := ""
		if resp.ErrorMessage != nil {
			msg = *resp.ErrorMessage
		}
		t.Errorf("expected Success=true, got false: %s", msg)
	}
	if len(wpctlCalls) != 2 {
		t.Errorf("expected 2 wpctl calls, got %d: %v", len(wpctlCalls), wpctlCalls)
	}
}

// TestSetDefaultAudioDevice_PartialWpctlFailurePulseAudioSucceeds verifies that
// when one wpctl call fails (partial failure) the function falls through to
// PulseAudio and returns Success:true if PulseAudio succeeds.
func TestSetDefaultAudioDevice_PartialWpctlFailurePulseAudioSucceeds(t *testing.T) {
	restoreSetDefaultVars(t)

	// Two PipeWire nodes; the second one fails.
	resolvePipeWireNodeIDsFn = func(_ context.Context, card, device uint64) []string {
		if card == 0 && device == 0 {
			return []string{"10", "11"}
		}
		return nil
	}

	wpctlSetDefault = func(_ context.Context, id string) ([]byte, error) {
		if id == "11" {
			return []byte("wpctl: error"), errors.New("wpctl failed")
		}
		return []byte(""), nil
	}

	resolvePulseAudioSinkOrSourceFn = func(_ context.Context, card, device uint64) []pulseAudioMatch {
		if card == 0 && device == 0 {
			return []pulseAudioMatch{
				{name: "alsa_output.pci-0.analog-stereo", category: "sinks"},
			}
		}
		return nil
	}

	var pactlCalls []string
	pactlSetDefault = func(_ context.Context, subcmd, name string) ([]byte, error) {
		pactlCalls = append(pactlCalls, subcmd+":"+name)
		return []byte(""), nil
	}

	svc := NewAudioService(zap.NewNop())
	resp, err := svc.SetDefaultAudioDevice(context.Background(), &agentpb.SetDefaultAudioDeviceRequest{DeviceId: 1})
	if err != nil {
		t.Fatalf("SetDefaultAudioDevice returned unexpected error: %v", err)
	}
	if !resp.GetSuccess() {
		msg := ""
		if resp.ErrorMessage != nil {
			msg = *resp.ErrorMessage
		}
		t.Errorf("expected Success=true after PulseAudio fallback, got false: %s", msg)
	}
	if len(pactlCalls) != 1 {
		t.Errorf("expected 1 pactl call, got %d: %v", len(pactlCalls), pactlCalls)
	}
}

// TestSetDefaultAudioDevice_AllFail verifies that when both PipeWire and
// PulseAudio fail the response is Success:false with a non-empty error message.
func TestSetDefaultAudioDevice_AllFail(t *testing.T) {
	restoreSetDefaultVars(t)

	// No PipeWire nodes found.
	resolvePipeWireNodeIDsFn = func(_ context.Context, _, _ uint64) []string {
		return nil
	}

	// No PulseAudio matches found either.
	resolvePulseAudioSinkOrSourceFn = func(_ context.Context, _, _ uint64) []pulseAudioMatch {
		return nil
	}

	wpctlSetDefault = func(_ context.Context, _ string) ([]byte, error) {
		t.Error("wpctlSetDefault should not be called when there are no PipeWire nodes")
		return nil, nil
	}
	pactlSetDefault = func(_ context.Context, _, _ string) ([]byte, error) {
		t.Error("pactlSetDefault should not be called when there are no PulseAudio matches")
		return nil, nil
	}

	svc := NewAudioService(zap.NewNop())
	resp, err := svc.SetDefaultAudioDevice(context.Background(), &agentpb.SetDefaultAudioDeviceRequest{DeviceId: 1})
	if err != nil {
		t.Fatalf("SetDefaultAudioDevice returned unexpected gRPC error: %v", err)
	}
	if resp.GetSuccess() {
		t.Error("expected Success=false when both PipeWire and PulseAudio fail, got true")
	}
	if resp.ErrorMessage == nil || *resp.ErrorMessage == "" {
		t.Error("expected a non-empty ErrorMessage when all attempts fail")
	}
}

// TestSetDefaultAudioDevice_AllWpctlFailPulseAudioResponseFail verifies that
// when PipeWire nodes are found but all wpctl calls fail, and PulseAudio
// subsequently finds matches but all pactl calls also fail, the response
// includes context from both the PipeWire and PulseAudio failures.
func TestSetDefaultAudioDevice_AllWpctlFailPulseAudioResponseFail(t *testing.T) {
	restoreSetDefaultVars(t)

	resolvePipeWireNodeIDsFn = func(_ context.Context, card, device uint64) []string {
		if card == 0 && device == 0 {
			return []string{"10"}
		}
		return nil
	}
	wpctlSetDefault = func(_ context.Context, _ string) ([]byte, error) {
		return []byte("wpctl: no such node"), errors.New("wpctl failed")
	}

	resolvePulseAudioSinkOrSourceFn = func(_ context.Context, card, device uint64) []pulseAudioMatch {
		if card == 0 && device == 0 {
			return []pulseAudioMatch{{name: "alsa_output.test", category: "sinks"}}
		}
		return nil
	}
	pactlSetDefault = func(_ context.Context, _, _ string) ([]byte, error) {
		return []byte("pactl: error"), errors.New("pactl failed")
	}

	svc := NewAudioService(zap.NewNop())
	resp, err := svc.SetDefaultAudioDevice(context.Background(), &agentpb.SetDefaultAudioDeviceRequest{DeviceId: 1})
	if err != nil {
		t.Fatalf("expected nil gRPC error, got: %v", err)
	}
	if resp.GetSuccess() {
		t.Error("expected Success=false when both PipeWire and PulseAudio fail")
	}
	if resp.ErrorMessage == nil || *resp.ErrorMessage == "" {
		t.Fatal("expected non-empty ErrorMessage")
	}
	// The error message must reference both failure contexts.
	if !strings.Contains(*resp.ErrorMessage, "PipeWire") {
		t.Errorf("error message missing PipeWire context: %s", *resp.ErrorMessage)
	}
	if !strings.Contains(*resp.ErrorMessage, "PulseAudio") {
		t.Errorf("error message missing PulseAudio context: %s", *resp.ErrorMessage)
	}
}

// TestSetDefaultAudioDevice_NoPipeWirePulseAudioSucceeds verifies the common
// PulseAudio-only path: no PipeWire nodes, PulseAudio resolves and sets the default.
func TestSetDefaultAudioDevice_NoPipeWirePulseAudioSucceeds(t *testing.T) {
	restoreSetDefaultVars(t)

	resolvePipeWireNodeIDsFn = func(_ context.Context, _, _ uint64) []string {
		return nil
	}

	resolvePulseAudioSinkOrSourceFn = func(_ context.Context, card, device uint64) []pulseAudioMatch {
		if card == 1 && device == 2 {
			return []pulseAudioMatch{
				{name: "alsa_output.pci-1.hdmi-stereo", category: "sinks"},
				{name: "alsa_input.pci-1.hdmi-stereo", category: "sources"},
			}
		}
		return nil
	}

	var pactlCalls []string
	pactlSetDefault = func(_ context.Context, subcmd, name string) ([]byte, error) {
		pactlCalls = append(pactlCalls, subcmd+":"+name)
		return []byte(""), nil
	}

	svc := NewAudioService(zap.NewNop())
	// deviceID for card 1 device 2: ((1<<8)|2)+1 = 259
	resp, err := svc.SetDefaultAudioDevice(context.Background(), &agentpb.SetDefaultAudioDeviceRequest{DeviceId: 259})
	if err != nil {
		t.Fatalf("SetDefaultAudioDevice returned unexpected error: %v", err)
	}
	if !resp.GetSuccess() {
		msg := ""
		if resp.ErrorMessage != nil {
			msg = *resp.ErrorMessage
		}
		t.Errorf("expected Success=true, got false: %s", msg)
	}
	// Both sink and source must have been set.
	if len(pactlCalls) != 2 {
		t.Errorf("expected 2 pactl calls (sink+source), got %d: %v", len(pactlCalls), pactlCalls)
	}
}
