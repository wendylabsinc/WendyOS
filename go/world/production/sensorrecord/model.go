// Package sensorrecord binds the existing synthetic sensor contract to the
// agent's publication API. It validates capture data, not publisher authority.
// Physical capture and SensorLink device authentication are outside this format.
package sensorrecord

import (
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/sensing"
)

const (
	Version         = 1
	ComponentType   = "wendy.sensor.capture.v1"
	MaxPayloadBytes = 32 << 10
	AppVersion      = "world-sensor-capture-v1"
	MaxLifetime     = 24 * time.Hour
	MaxPhysicalAge  = 5 * time.Second
)

// Capture retains the original sample JSON as base64-encoded bytes. Using a
// nested JSON object would let encoding/json compact or escape those bytes.
// CapturedUnixNS is the original scene wall-clock assertion; it is neither the
// simulation clock nor authenticated physical time. SimulationAnchorUnixNS is
// fixed by commissioning for the run, never derived again from receipt time.
type Capture struct {
	Version                int          `json:"version"`
	SceneBootID            string       `json:"scene_boot_id"`
	CapturedUnixNS         int64        `json:"captured_unix_ns"`
	SimulationAnchorUnixNS int64        `json:"simulation_anchor_unix_ns"`
	SampleJSON             []byte       `json:"sample_json"`
	SensorLink             *WireBinding `json:"sensorlink,omitempty"`
}

// WireBinding describes the reassembled JPEG actually received from SensorLink.
// The original sample and its PNG reference remain in Capture.SampleJSON. The
// hashes bind this individual frame to that capture and its moving calibration;
// there is deliberately no assumed fixed wall/simulation-clock offset.
type WireBinding struct {
	Protocol          string              `json:"protocol"`
	ChannelID         uint32              `json:"channel_id"`
	FrameSequence     uint32              `json:"frame_sequence"`
	TimestampUS       uint64              `json:"timestamp_us"`
	Flags             uint32              `json:"flags"`
	SceneBootID       string              `json:"scene_boot_id"`
	CaptureSequence   uint64              `json:"capture_sequence"`
	CapturedUnixNS    int64               `json:"captured_unix_ns"`
	SampleSHA256      string              `json:"sample_sha256"`
	CalibrationSHA256 string              `json:"calibration_sha256"`
	Evidence          sensing.EvidenceRef `json:"evidence"`
}
