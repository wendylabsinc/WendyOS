package services

import (
	"encoding/json"
	"sort"
	"testing"

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
