// Package liveapp publishes validated captures through an ordinary app socket.
// It holds no device signing key, scene actuator token or mission authority.
package liveapp

import (
	"errors"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

var (
	ErrConfig         = errors.New("invalid bounded live sensor configuration")
	ErrDuplicate      = errors.New("capture already acknowledged")
	ErrCapture        = errors.New("capture changed identity or regressed")
	ErrUncertain      = errors.New("mutation outcome unknown; no automatic retry")
	ErrAcknowledgment = errors.New("agent acknowledgment does not match submitted capture")
)

const MaxResultBytes = 32 << 20

type Config struct {
	SceneURL               string         `json:"scene_url"`
	ReadTokenFile          string         `json:"read_token_file"`
	RunID                  string         `json:"run_id"`
	Site                   string         `json:"site"`
	Frame                  world.FrameRef `json:"frame"`
	SensorID               string         `json:"sensor_id"`
	SimulationAnchorUnixNS int64          `json:"simulation_anchor_unix_ns"`
	DurationMS             int64          `json:"duration_ms"`
	MaxSamples             int            `json:"max_samples"`
	// Faster source polling does not bypass the publisher's independent
	// eight-RPC-per-second ceiling or its capture/byte/result bounds.
	PollIntervalMS   int64 `json:"poll_interval_ms,omitempty"`
	TTLMS            int64 `json:"ttl_ms,omitempty"`
	RequestTimeoutMS int64 `json:"request_timeout_ms,omitempty"`
}

func (c Config) normalized() (Config, error) {
	if c.PollIntervalMS == 0 {
		c.PollIntervalMS = 1000
	}
	if c.TTLMS == 0 {
		c.TTLMS = 3000
	}
	if c.RequestTimeoutMS == 0 {
		c.RequestTimeoutMS = 1500
	}
	if c.SceneURL == "" || c.ReadTokenFile == "" || c.RunID == "" || len(c.RunID) > 128 || c.Site == "" || len(c.Site) > 256 || c.Frame.Validate() != nil || c.SimulationAnchorUnixNS <= 0 || c.DurationMS < 1 || c.DurationMS > 600000 || c.MaxSamples < 1 || c.MaxSamples > 64 || c.PollIntervalMS < 50 || c.PollIntervalMS > 10000 || c.TTLMS < 1000 || c.TTLMS > 10000 || c.RequestTimeoutMS < 10 || c.RequestTimeoutMS > 5000 {
		return c, ErrConfig
	}
	switch c.SensorID {
	case "camera-a", "camera-b", "gas-1", "audio-1":
	default:
		return c, ErrConfig
	}
	return c, nil
}

// Receipt records an exact private-socket acknowledgment. Its proof still
// requires an independent verifier with commissioned roots, policy and time.
// Byte fields use base64 in JSON, preserving whitespace in the original proof.
type Receipt struct {
	CaptureID        string `json:"capture_id"`
	NativeSequence   uint64 `json:"native_sequence"`
	SceneBootID      string `json:"scene_boot_id"`
	CapturedUnixNS   int64  `json:"captured_unix_ns"`
	SampleNS         int64  `json:"sample_ns"`
	EvidenceSHA256   string `json:"evidence_sha256,omitempty"`
	RequestJSON      []byte `json:"request_json"`
	SignedRecordJSON []byte `json:"signed_record_json"`
	RecordSHA256     string `json:"record_sha256"`
	ProofSHA256      string `json:"proof_sha256"`
}
type Result struct {
	Version    int       `json:"version"`
	RunID      string    `json:"run_id"`
	SensorID   string    `json:"sensor_id"`
	Status     string    `json:"status"`
	ErrorCode  string    `json:"error,omitempty"`
	Duplicates int       `json:"duplicates"`
	Receipts   []Receipt `json:"receipts"`
}
type PublisherConfig struct {
	RequestTimeout time.Duration
	// Each request and reply charges the broker. 125ms limits this app to
	// eight RPCs / sixteen message charges per second before publication polls.
	MinRPCInterval time.Duration
}
