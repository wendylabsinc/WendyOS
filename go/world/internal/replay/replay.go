// Package replay maps recorded simulated observations onto a new, explicit
// evaluation timeline. It validates structure and lineage, not capture identity
// signatures. The authenticated publisher is the replayer, not the old sensor.
package replay

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/sensing"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

const Type = "wendy.sensor.replay.v1"
const Schema = Type
const MaxEnvelopeBytes = 128 << 10
const AppVersion = "world-replay-v1"

var identifier = regexp.MustCompile(`^[A-Za-z0-9_-]{1,80}$`)

// Envelope retains the entire imported component. The hash covers its typed
// JSON encoding, including the original payload and relayed age hint. It is not
// an independent origin signature. RecordingSHA256 identifies the exact archive
// manifest selected by the caller.
type Envelope struct {
	SchemaVersion   string          `json:"schema_version"`
	RunID           string          `json:"run_id"`
	RecordingSHA256 string          `json:"recording_sha256"`
	OriginalSHA256  string          `json:"original_sha256"`
	CaptureAnchor   time.Time       `json:"capture_anchor"`
	ReplayAnchor    time.Time       `json:"replay_anchor"`
	Original        world.Component `json:"original"`
}

type Config struct {
	Tenant, Site, RunID, RecordingSHA256 string
	Source                               world.Source
	Epoch, Sequence                      uint64
	CaptureAnchor, ReplayAnchor          time.Time
	TTL                                  time.Duration
}

// ValidateOriginal accepts only raw simulated sensor observations. Historical
// age/expiry does not prevent an explicit replay, but its timing uncertainty and
// original lifetime remain bounded. Measured, inferred and recursive replay
// imports need separate contracts and cannot silently enter this path.
func ValidateOriginal(c world.Component) (sensing.Sample, error) {
	s, err := sensing.FromComponent(c)
	if err != nil {
		return s, err
	}
	if c.TTL > 24*time.Hour || !c.Timing.ErrorKnown || c.Timing.ErrorBound >= c.TTL {
		return s, errors.New("recorded observation needs bounded lifetime and known clock error")
	}
	if s.Audio != nil && time.Duration(s.Audio.DurationNS) >= c.TTL-c.Timing.ErrorBound {
		return s, errors.New("recorded audio interval exceeds observation lifetime")
	}
	if s.Camera != nil && c.Timing.ErrorBound >= time.Duration(s.Camera.CalibrationValidForNS) {
		return s, errors.New("recorded camera calibration has no valid interval")
	}
	return s, nil
}

func Digest(c world.Component) (string, error) {
	if _, err := ValidateOriginal(c); err != nil {
		return "", err
	}
	raw, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	if len(raw) > MaxEnvelopeBytes {
		return "", errors.New("recorded component too large")
	}
	h := sha256.Sum256(raw)
	return hex.EncodeToString(h[:]), nil
}

func (e Envelope) Validate() (sensing.Sample, error) {
	s, err := ValidateOriginal(e.Original)
	if err != nil {
		return s, err
	}
	if e.SchemaVersion != Schema || !identifier.MatchString(e.RunID) || e.RunID == s.RunID || !sensing.DigestValid(e.RecordingSHA256) || !sensing.DigestValid(e.OriginalSHA256) || e.CaptureAnchor.IsZero() || e.ReplayAnchor.IsZero() {
		return s, errors.New("invalid replay identity, digest or timeline")
	}
	delta := e.Original.Timing.SampleTime.Sub(e.CaptureAnchor)
	span := time.Duration(0)
	if s.Audio != nil {
		span = time.Duration(s.Audio.DurationNS)
	}
	if delta < span || delta > 24*time.Hour {
		return s, errors.New("recorded interval lies outside the replay excerpt")
	}
	digest, err := Digest(e.Original)
	if err != nil || digest != e.OriginalSHA256 {
		return s, errors.New("recorded component digest mismatch")
	}
	return s, nil
}

// Decode rejects missing, null, duplicate and unknown contract fields. The
// nested sensor payload receives the sensor schema's own strict validation.
func Decode(raw []byte) (Envelope, error) {
	var e Envelope
	if err := authority.DecodeStrictLimit(raw, &e, MaxEnvelopeBytes); err != nil {
		return e, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return e, err
	}
	if err := present(fields, "schema_version", "run_id", "recording_sha256", "original_sha256", "capture_anchor", "replay_anchor", "original"); err != nil {
		return e, err
	}
	original, err := DecodeOriginal(fields["original"])
	if err != nil {
		return e, err
	}
	e.Original = original
	_, err = e.Validate()
	return e, err
}

// DecodeOriginal validates a raw archived component before decoding can turn
// missing/null uncertainty, age or seed into zero. It checks structure and
// simulated sensor lineage, not an origin signature or current freshness.
func DecodeOriginal(raw []byte) (world.Component, error) {
	var c world.Component
	if err := authority.DecodeStrictLimit(raw, &c, MaxEnvelopeBytes); err != nil {
		return c, err
	}
	var original map[string]json.RawMessage
	if err := json.Unmarshal(raw, &original); err != nil {
		return c, err
	}
	if err := present(original, "key", "version", "frame", "timing", "ttl_ns", "provenance", "payload", "tombstone"); err != nil {
		return c, err
	}
	for name, keys := range map[string][]string{
		"key":        {"tenant", "site", "entity", "type", "source"},
		"version":    {"epoch", "sequence"},
		"frame":      {"id", "revision"},
		"timing":     {"sample_time", "clock_domain", "error_known", "error_bound_ns", "age_hint_ns"},
		"provenance": {"origin", "app_version", "model_id", "model_version", "simulation"},
	} {
		var nested map[string]json.RawMessage
		if err := json.Unmarshal(original[name], &nested); err != nil {
			return c, err
		}
		if err := present(nested, keys...); err != nil {
			return c, fmt.Errorf("original %s: %w", name, err)
		}
	}
	var provenance map[string]json.RawMessage
	_ = json.Unmarshal(original["provenance"], &provenance)
	var simulation map[string]json.RawMessage
	if err := json.Unmarshal(provenance["simulation"], &simulation); err != nil {
		return c, err
	}
	if err := present(simulation, "run_id", "scenario_version", "seed", "clock_domain"); err != nil {
		return c, err
	}
	_, err := ValidateOriginal(c)
	return c, err
}

func present(fields map[string]json.RawMessage, names ...string) error {
	for _, name := range names {
		raw, ok := fields[name]
		if !ok || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return fmt.Errorf("missing or null replay field %s", name)
		}
	}
	return nil
}

// Build preserves original capture intervals at 1x speed. CaptureAnchor is the
// start of the selected excerpt, which may be partway through a simulation run.
// The output owns its serialized payload; later caller mutations cannot rewrite
// the imported component inside it. Build never checks current authorization.
func Build(original world.Component, cfg Config) (world.Component, error) {
	digest, err := Digest(original)
	if err != nil {
		return world.Component{}, err
	}
	e := Envelope{SchemaVersion: Schema, RunID: cfg.RunID, RecordingSHA256: cfg.RecordingSHA256, OriginalSHA256: digest, CaptureAnchor: cfg.CaptureAnchor, ReplayAnchor: cfg.ReplayAnchor, Original: original}
	s, err := e.Validate()
	if err != nil {
		return world.Component{}, err
	}
	if cfg.Tenant != original.Key.Tenant || cfg.Site != original.Key.Site || cfg.Source.Validate() != nil || cfg.Source.Service != s.SensorID || cfg.Source == original.Key.Source || cfg.Epoch == 0 || cfg.Sequence == 0 || cfg.TTL <= 0 || cfg.TTL > 24*time.Hour {
		return world.Component{}, errors.New("invalid replay publisher scope, version or lifetime")
	}
	ttl := min(cfg.TTL, original.TTL)
	span := time.Duration(0)
	if s.Audio != nil {
		span = time.Duration(s.Audio.DurationNS)
	}
	if ttl <= original.Timing.ErrorBound+span {
		return world.Component{}, errors.New("replay lifetime excludes the original interval or clock error")
	}
	payload, err := json.Marshal(e)
	if err != nil || len(payload) > MaxEnvelopeBytes {
		return world.Component{}, errors.New("replay envelope too large or invalid")
	}
	// Decode the private encoding so context pointers cannot alias caller input.
	e, err = Decode(payload)
	if err != nil {
		return world.Component{}, err
	}
	captureTime := e.Original.Timing.SampleTime
	out := world.Component{
		Key:     world.Key{Tenant: cfg.Tenant, Site: cfg.Site, Entity: cfg.RunID + "/" + s.SensorID, Type: Type, Source: cfg.Source},
		Version: world.Version{Epoch: cfg.Epoch, Sequence: cfg.Sequence}, Frame: original.Frame,
		Timing:     world.Timing{SampleTime: cfg.ReplayAnchor.Add(captureTime.Sub(cfg.CaptureAnchor)), ClockDomain: "simulation:" + cfg.RunID, ErrorKnown: e.Original.Timing.ErrorKnown, ErrorBound: e.Original.Timing.ErrorBound},
		TTL:        ttl,
		Provenance: world.Provenance{Origin: world.Replayed, AppVersion: AppVersion, ModelID: s.SensorModel, ModelVersion: s.ScenarioVersion, Simulation: e.Original.Provenance.Simulation, OriginalCaptureTime: &captureTime, OriginalClockDomain: e.Original.Timing.ClockDomain},
		Payload:    payload,
	}
	if _, err := json.Marshal(out); err != nil {
		return world.Component{}, err
	}
	return out, nil
}

// FromComponent validates the envelope against the authenticated replay record.
// It returns the unchanged original sensor sample; callers must use e.RunID and
// the outer clock for replay identity, not mistake the sample's old run for it.
func FromComponent(c world.Component) (sensing.Sample, Envelope, error) {
	var zero sensing.Sample
	var e Envelope
	if c.Key.Type != Type || c.Tombstone || c.Version.Epoch == 0 || c.Version.Sequence == 0 || c.TTL <= 0 || c.TTL > 24*time.Hour || c.Key.Source.Validate() != nil || c.Timing.Validate() != nil || c.Provenance.Validate() != nil {
		return zero, e, errors.New("invalid replay component")
	}
	e, err := Decode(c.Payload)
	if err != nil {
		return zero, e, err
	}
	s, err := e.Validate()
	if err != nil {
		return zero, e, err
	}
	expected, err := Build(e.Original, Config{Tenant: c.Key.Tenant, Site: c.Key.Site, RunID: e.RunID, RecordingSHA256: e.RecordingSHA256, Source: c.Key.Source, Epoch: c.Version.Epoch, Sequence: c.Version.Sequence, CaptureAnchor: e.CaptureAnchor, ReplayAnchor: e.ReplayAnchor, TTL: c.TTL})
	if err != nil {
		return zero, e, err
	}
	p, _ := json.Marshal(c.Provenance)
	wantP, _ := json.Marshal(expected.Provenance)
	if c.Key != expected.Key || c.Frame != expected.Frame || c.TTL != expected.TTL || !c.Timing.SampleTime.Equal(expected.Timing.SampleTime) || c.Timing.ClockDomain != expected.Timing.ClockDomain || c.Timing.ErrorKnown != expected.Timing.ErrorKnown || c.Timing.ErrorBound != expected.Timing.ErrorBound || !bytes.Equal(p, wantP) {
		return zero, e, errors.New("replay payload does not match authenticated component")
	}
	return s, e, nil
}
