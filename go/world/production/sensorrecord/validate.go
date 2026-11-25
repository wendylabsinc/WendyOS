package sensorrecord

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/wendylabsinc/wendy/go/world/internal/sensing"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
	"github.com/wendylabsinc/wendy/go/world/production/jws"
	"github.com/wendylabsinc/wendy/go/world/production/serviceorigin"
)

func digest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// CalibrationDigest binds all intrinsic/extrinsic camera fields together with
// the named revision, frame, placement and placement uncertainty. The original
// complete JSON bytes are independently covered by WireBinding.SampleSHA256.
func CalibrationDigest(sample sensing.Sample) (string, error) {
	if err := sample.Validate(); err != nil {
		return "", err
	}
	if sample.Camera == nil {
		return "", errors.New("SensorLink requires camera calibration")
	}
	raw, err := json.Marshal(struct {
		Frame       world.FrameRef `json:"frame"`
		Revision    string         `json:"revision"`
		Position    []float64      `json:"position_m"`
		Uncertainty float64        `json:"uncertainty_m"`
		Camera      sensing.Camera `json:"camera"`
	}{sample.Frame, sample.CalibrationRevision, sample.PositionM, sample.UncertaintyM, *sample.Camera})
	if err != nil {
		return "", err
	}
	return digest(raw), nil
}

func sample(raw []byte) (sensing.Sample, error) {
	var value sensing.Sample
	if len(raw) == 0 || len(raw) > MaxPayloadBytes {
		return value, errors.New("capture sample exceeds payload budget")
	}
	// sensing.Decode checks required fields, nulls, vector shape and semantics;
	// the production JSON decoder additionally refuses case aliases and invalid
	// Unicode rather than normalizing signed data during interpretation.
	if err := jws.DecodeStrict(raw, &value); err != nil {
		return value, err
	}
	return sensing.Decode(raw)
}

// Sample returns a validated owned copy of the original sample. A wire JPEG
// never replaces the original PNG reference in this accessor.
func (c Capture) Sample() (sensing.Sample, error) {
	if err := c.Validate(); err != nil {
		return sensing.Sample{}, err
	}
	return sample(c.SampleJSON)
}

func (c Capture) Validate() error {
	if c.Version != Version || c.SceneBootID == "" || len(c.SceneBootID) > 128 || !utf8.ValidString(c.SceneBootID) || strings.TrimSpace(c.SceneBootID) != c.SceneBootID || strings.IndexFunc(c.SceneBootID, unicode.IsControl) >= 0 || c.CapturedUnixNS <= 0 || c.SimulationAnchorUnixNS <= 0 {
		return errors.New("invalid capture version, scene boot or source clocks")
	}
	s, err := sample(c.SampleJSON)
	if err != nil {
		return err
	}
	if s.SensorLink != nil {
		return errors.New("legacy fixed-anchor SensorLink metadata is not a live capture binding")
	}
	if s.SampleNS > math.MaxInt64-c.SimulationAnchorUnixNS || (s.Audio != nil && c.CapturedUnixNS < s.Audio.DurationNS) {
		return errors.New("capture timestamp or interval exceeds clock bounds")
	}
	if c.SensorLink == nil {
		return nil
	}
	w := c.SensorLink
	calibration, err := CalibrationDigest(s)
	if err != nil {
		return err
	}
	if s.Kind != "camera" || s.Evidence == nil || s.Evidence.ContentType != "image/png" || w.Protocol != "wendylite.v1.sensorlink" || w.ChannelID == 0 || w.Flags & ^uint32(1) != 0 || w.SceneBootID != c.SceneBootID || w.CaptureSequence != s.Sequence || w.CapturedUnixNS != c.CapturedUnixNS || c.CapturedUnixNS%1000 != 0 || w.TimestampUS != uint64(c.CapturedUnixNS/1000) || w.SampleSHA256 != digest(c.SampleJSON) || w.CalibrationSHA256 != calibration || w.Evidence.ContentType != "image/jpeg" || w.Evidence.Validate() != nil {
		return errors.New("SensorLink frame does not bind the original capture, calibration and JPEG")
	}
	return nil
}

// Encode preserves the bytes inside SampleJSON and limits the entire payload,
// including base64 expansion, to the agent publication budget.
func Encode(c Capture) ([]byte, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(c)
	if err != nil {
		return nil, err
	}
	if len(raw) > MaxPayloadBytes {
		return nil, errors.New("capture exceeds agent payload budget")
	}
	return raw, nil
}

func required(raw []byte, names ...string) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return err
	}
	for _, name := range names {
		value, exists := fields[name]
		if !exists || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return fmt.Errorf("required capture field %s missing or null", name)
		}
	}
	return nil
}

// Decode refuses unknown, duplicate, aliased, missing and null required fields.
// Neither Decode nor ValidatePublication establishes signing authority.
func Decode(raw []byte) (Capture, error) {
	var c Capture
	if len(raw) > MaxPayloadBytes {
		return c, errors.New("capture exceeds agent payload budget")
	}
	if err := jws.DecodeStrict(raw, &c); err != nil {
		return c, err
	}
	if err := required(raw, "version", "scene_boot_id", "captured_unix_ns", "simulation_anchor_unix_ns", "sample_json"); err != nil {
		return c, err
	}
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(raw, &fields)
	if wire, exists := fields["sensorlink"]; exists {
		if err := required(wire, "protocol", "channel_id", "frame_sequence", "timestamp_us", "flags", "scene_boot_id", "capture_sequence", "captured_unix_ns", "sample_sha256", "calibration_sha256", "evidence"); err != nil {
			return c, err
		}
		var wireFields map[string]json.RawMessage
		_ = json.Unmarshal(wire, &wireFields)
		if err := required(wireFields["evidence"], "sha256", "content_type", "size"); err != nil {
			return c, err
		}
	}
	return c, c.Validate()
}

// ValidatePublication verifies the payload's binding to the submitted metadata.
// Source, permissions, current source time, media availability and proof validity
// are enforced by the agent separately. Native sequence is not an agent version.
func ValidatePublication(p serviceorigin.PublishRequest) (Capture, error) {
	c, err := Decode(p.Payload)
	if err != nil {
		return c, err
	}
	s, _ := sample(c.SampleJSON)
	if p.ComponentType != ComponentType || p.Tombstone || p.Entity != "sensor/"+s.SensorID || p.Stream != s.SensorID || p.Frame != s.Frame || p.TTL <= 0 || p.TTL > MaxLifetime || p.Timing.Validate() != nil || !p.Timing.ErrorKnown || p.Timing.ErrorBound >= p.TTL || p.Timing.AgeHint > MaxLifetime || !p.Timing.SampleTime.Equal(time.Unix(0, c.SimulationAnchorUnixNS+s.SampleNS)) || p.Timing.ClockDomain != "simulation:"+s.RunID {
		return c, errors.New("publication disagrees with capture scope or simulation timing")
	}
	provenance := p.Provenance
	if provenance.Validate() != nil || provenance.Origin != world.Simulated || provenance.Simulation == nil || provenance.Simulation.RunID != s.RunID || provenance.Simulation.ScenarioVersion != s.ScenarioVersion || provenance.Simulation.Seed != s.Seed || provenance.Simulation.ClockDomain != p.Timing.ClockDomain || provenance.ModelID != s.SensorModel || provenance.ModelVersion != s.ScenarioVersion {
		return c, errors.New("publication disagrees with original simulation lineage")
	}
	return c, nil
}

// FromComponent validates a projection of an already verified agent record. It
// checks actual outer source completeness and version without comparing either
// to the native sample ID/sequence. It does not synthesize or rewrite identity.
func FromComponent(c world.Component) (Capture, error) {
	if c.Key.Tenant == "" || c.Key.Site == "" || c.Key.Source.Validate() != nil || c.Version.Epoch == 0 || c.Version.Sequence == 0 {
		return Capture{}, errors.New("agent source, site and publication version required")
	}
	capture, err := ValidatePublication(serviceorigin.PublishRequest{Entity: c.Key.Entity, ComponentType: c.Key.Type, Stream: c.Key.Source.Stream, Frame: c.Frame, Timing: c.Timing, TTL: c.TTL, Provenance: c.Provenance, Payload: c.Payload, Tombstone: c.Tombstone})
	if err != nil {
		return capture, err
	}
	s, _ := sample(capture.SampleJSON)
	if c.Key.Site != s.SiteID {
		return capture, errors.New("agent site disagrees with captured site")
	}
	return capture, nil
}

// NormalizedSample returns interpretation metadata for the bytes actually
// delivered. For SensorLink it selects the received JPEG; callers retain this
// Capture as the source link to the original sample and PNG reference.
func (c Capture) NormalizedSample() (sensing.Sample, error) {
	s, err := c.Sample()
	if err != nil {
		return s, err
	}
	if c.SensorLink != nil {
		ref := c.SensorLink.Evidence
		s.Evidence = &ref
	}
	return s, nil
}

// Evidence identifies the bytes required for interpretation, not every source
// link in the payload. A SensorLink capture requires its received JPEG; its PNG
// link is retained provenance and does not assert that PNG was transferred.
func (c Capture) Evidence() (*sensing.EvidenceRef, error) {
	s, err := c.NormalizedSample()
	return s.Evidence, err
}
