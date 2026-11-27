package trackcapture

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"reflect"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/pose"
	"github.com/wendylabsinc/wendy/go/world/internal/sensing"
	"github.com/wendylabsinc/wendy/go/world/internal/tracking"
	"github.com/wendylabsinc/wendy/go/world/production/jws"
	"github.com/wendylabsinc/wendy/go/world/production/proofref"
	"github.com/wendylabsinc/wendy/go/world/production/resultrecord"
	"github.com/wendylabsinc/wendy/go/world/production/sensorrecord"
	"github.com/wendylabsinc/wendy/go/world/production/serviceorigin"
	"github.com/wendylabsinc/wendy/go/world/production/trustedtime"
)

type prepared struct {
	input       Input
	validated   resultrecord.ValidatedInput
	sample      sensing.Sample
	capture     sensorrecord.Capture
	calibration string
	projected   pose.Result
	deadline    time.Time
}

func digest(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
func minimum(a, b time.Time) time.Time {
	if a.IsZero() || b.Before(a) {
		return b
	}
	return a
}
func own(in Input) Input {
	// These types have no opaque runtime resources. Serialization owns all
	// exported slices without rewriting the signed envelope or record bytes.
	b, _ := json.Marshal(in)
	var out Input
	_ = json.Unmarshal(b, &out)
	return out
}

func (s *Session) prepare(in Input, now time.Time, w trustedtime.Interval) (prepared, error) {
	var out prepared
	if len(in.Envelope) == 0 || len(in.Envelope) > 256<<10 {
		return out, errors.New("camera proof exceeds bound")
	}
	var signed serviceorigin.SignedRecord
	var record serviceorigin.Record
	var delegation serviceorigin.DelegationClaims
	if authority.DecodeStrictLimit(in.Envelope, &signed, 256<<10) != nil || jws.DecodeStrict(signed.Record, &record) != nil || jws.DecodeStrict(signed.Delegation.Claims, &delegation) != nil || !bytes.Equal(signed.Record, in.Verified.Bytes) || !reflect.DeepEqual(record, in.Verified.Record) || !reflect.DeepEqual(delegation, in.Verified.Delegation) || record.DelegationSHA256 != digest(signed.Delegation.Claims) {
		return out, errors.New("camera proof differs from verified original")
	}
	c := in.Verified.Component()
	capture, err := sensorrecord.FromComponent(c)
	if err != nil {
		return out, err
	}
	sample, err := capture.NormalizedSample()
	if err != nil {
		return out, err
	}
	source, ok := s.cfg.Sources[sample.SensorID]
	if !ok || source != c.Key.Source || c.Key.Tenant != s.cfg.Tenant || c.Key.Site != s.cfg.Site || sample.Kind != "camera" || sample.RunID != s.cfg.Tracking.RunID || c.Frame != s.cfg.Tracking.Frame || c.Timing.ClockDomain != s.cfg.Tracking.ClockDomain {
		return out, errors.New("camera is outside configured capture scope")
	}
	sampleUntil, physicalUntil, err := sensorrecord.LiveDeadlines(c)
	if err != nil {
		return out, err
	}
	physicalUntil = minimum(physicalUntil, time.UnixMilli(record.ExpiresAtUnixMS))
	physicalUntil = minimum(physicalUntil, time.UnixMilli(delegation.ExpiresAtUnixMS))
	physicalUntil = minimum(physicalUntil, in.Verified.Device.ValidUntil)
	if in.Verified.Device.ValidUntil.IsZero() || in.Verified.Device.ValidFrom.After(w.Earliest) || time.UnixMilli(record.IssuedAtUnixMS).After(w.Earliest) || time.UnixMilli(delegation.IssuedAtUnixMS).After(w.Earliest) {
		return out, errors.New("camera verified authority interval differs")
	}
	ref := proofref.Reference{Key: c.Key, Version: c.Version, EnvelopeSHA256: digest(in.Envelope), RecordSHA256: digest(signed.Record)}
	if err := ref.Validate(); err != nil {
		return out, err
	}
	deadline := minimum(sampleUntil, c.Timing.SampleTime.Add(s.cfg.Tracking.MaxMiss))
	out = prepared{input: in, validated: resultrecord.ValidatedInput{Reference: ref, Component: c, SampleValidUntil: sampleUntil, PhysicalValidUntil: physicalUntil}, sample: sample, capture: capture, deadline: deadline}
	if c.Timing.SampleTime.After(now) {
		return out, errors.New("camera capture is in the future")
	}
	if !now.Before(deadline) || !w.Latest.Before(physicalUntil) {
		return out, errExpired
	}
	if err := sensorrecord.PhysicalCurrent(capture, w); err != nil {
		return out, err
	}
	if in.Poses.ModelID != s.cfg.ModelID || in.Poses.ModelVersion != s.cfg.ModelVersion || in.Poses.ModelSHA256 != s.cfg.ModelSHA256 || in.Poses.ImageSHA256 != sample.Evidence.SHA256 {
		return out, errors.New("inference model or original image differs")
	}
	if err := sensorrecord.ValidateMedia(capture, in.Image); err != nil {
		return out, err
	}
	calibration, err := sensorrecord.CalibrationDigest(sample)
	if err != nil {
		return out, err
	}
	out.calibration = calibration
	projection := s.cfg.Projection
	projection.CameraUncertaintyM += sample.UncertaintyM
	out.projected, err = pose.Project(*sample.Camera, in.Poses, projection)
	return out, err
}

func (p prepared) detection(d pose.Detection) tracking.Detection {
	return tracking.Detection{Source: p.sample.SensorID, SampleID: p.sample.ID, Version: p.validated.Component.Version, MemberIndex: d.MemberIndex, SampleTime: p.validated.Component.Timing.SampleTime, ValidUntil: p.deadline, RunID: p.sample.RunID, ClockDomain: p.validated.Component.Timing.ClockDomain, Frame: p.sample.Frame, PositionXZ: d.PositionXZ, Covariance: d.Covariance, ModelID: p.input.Poses.ModelID, ModelVersion: p.input.Poses.ModelVersion, ModelSHA256: p.input.Poses.ModelSHA256, CalibrationRevision: p.sample.CalibrationRevision, Evidence: []sensing.EvidenceRef{*p.sample.Evidence}}
}
func (p prepared) member(d tracking.Detection) Member {
	m := Member{Proof: p.validated.Reference, SensorID: p.sample.SensorID, SampleID: p.sample.ID, CaptureSequence: p.sample.Sequence, MemberIndex: d.MemberIndex, SampleTime: d.SampleTime, ValidUntil: p.deadline, PhysicalUntil: p.validated.PhysicalValidUntil, PositionXZ: d.PositionXZ, Covariance: d.Covariance, Image: *p.sample.Evidence, ModelID: d.ModelID, ModelVersion: d.ModelVersion, ModelSHA256: d.ModelSHA256, CalibrationRevision: p.sample.CalibrationRevision, CalibrationSHA256: p.calibration, SceneBootID: p.capture.SceneBootID, CapturedUnixNS: p.capture.CapturedUnixNS, SimulationAnchorUnixNS: p.capture.SimulationAnchorUnixNS}
	if p.capture.SensorLink != nil {
		original, _ := p.capture.Sample()
		m.OriginalImage = original.Evidence
	}
	return m
}
