// Package missiontrigger prepares fixed-location proposal inputs from original
// native inference records already verified by the caller. It grants no mission
// authority and does not establish current freshness.
package missiontrigger

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/fusion"
	"github.com/wendylabsinc/wendy/go/world/internal/tasking"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
	"github.com/wendylabsinc/wendy/go/world/production/fusioncapture"
	"github.com/wendylabsinc/wendy/go/world/production/jws"
	"github.com/wendylabsinc/wendy/go/world/production/resultrecord"
	"github.com/wendylabsinc/wendy/go/world/production/serviceorigin"
	"github.com/wendylabsinc/wendy/go/world/production/trackcapture"
)

const MaxEnvelopeBytes = 384 << 10

var ErrRefused = errors.New("native inference proposal input refused")

// Binding is the caller's commissioned scope and right-handed Y-up floor
// height. It is not itself signed policy or authorization to move.
type Binding struct {
	Tenant, Site string
	Frame        world.FrameRef
	FloorY       float64
}

// Input must pair the exact original envelope with the result of verifying that
// envelope's signatures and current source authority. A populated VerifiedRecord
// alone is not a proof: Prepare checks byte/projection binding, not cryptography.
type Input struct {
	Verified serviceorigin.VerifiedRecord
	Envelope []byte
}

// Prepared is proposal context, not an accepted or authorized mission. Target
// is a fixed observed location; track velocity never changes it. Deadlines are
// original bounds for the caller to evaluate with its own fresh clocks.
type Prepared struct {
	Trigger            tasking.TriggerRef `json:"trigger"`
	Target             [3]float64         `json:"target"`
	ResultSchema       string             `json:"result_schema"`
	Envelope           []byte             `json:"envelope_json"`
	EnvelopeSHA256     string             `json:"envelope_sha256"`
	SampleValidUntil   time.Time          `json:"sample_valid_until"`
	PhysicalValidUntil time.Time          `json:"physical_valid_until"`
}

func digest(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func minimum(a, b time.Time) time.Time {
	if b.Before(a) {
		return b
	}
	return a
}

// Prepare preserves the native result component and its exact signed envelope.
// Only the TriggerRef projection's AgeHint is canonicalized to zero, matching
// tasking's existing component hash. No clock is read, deadline rebased, evidence
// fetched, proposal submitted or mission signed. Tasking validates the same
// native context and fixed target through its separate operator approval path.
func Prepare(input Input, binding Binding) (Prepared, error) {
	var out Prepared
	if binding.Tenant == "" || binding.Site == "" || binding.Frame.Validate() != nil || math.IsNaN(binding.FloorY) || math.IsInf(binding.FloorY, 0) || len(input.Envelope) == 0 || len(input.Envelope) > MaxEnvelopeBytes {
		return out, ErrRefused
	}
	var signed serviceorigin.SignedRecord
	var record serviceorigin.Record
	var delegation serviceorigin.DelegationClaims
	v := input.Verified
	if authority.DecodeStrictLimit(input.Envelope, &signed, MaxEnvelopeBytes) != nil || !bytes.Equal(signed.Record, v.Bytes) || jws.DecodeStrict(signed.Record, &record) != nil || !reflect.DeepEqual(record, v.Record) || jws.DecodeStrict(signed.Delegation.Claims, &delegation) != nil || !reflect.DeepEqual(delegation, v.Delegation) {
		return out, ErrRefused
	}
	c := v.Component()
	if record.Scope.Validate() != nil || v.Device.Principal != record.Scope.Device || v.Device.Tenant != record.Scope.Tenant || c.Key.Tenant != binding.Tenant || c.Key.Site != binding.Site || c.Frame != binding.Frame || c.Key.Type != resultrecord.ComponentType || c.Key.Entity == "" || c.Key.Source.Validate() != nil || c.Version.Epoch == 0 || c.Version.Sequence == 0 || c.Tombstone || c.Timing.Validate() != nil || !c.Timing.ErrorKnown || c.TTL <= max(c.Timing.ErrorBound, c.Timing.AgeHint) {
		return out, ErrRefused
	}
	r, err := resultrecord.Decode(c.Payload)
	if err != nil {
		return out, ErrRefused
	}
	for _, ref := range r.Inputs {
		if ref.Key.Tenant != binding.Tenant || ref.Key.Site != binding.Site {
			return out, ErrRefused
		}
	}
	out.ResultSchema = r.ResultSchema
	out.SampleValidUntil = c.Timing.SampleTime.Add(c.TTL - max(c.Timing.ErrorBound, c.Timing.AgeHint))
	switch r.ResultSchema {
	case fusion.HazardType:
		hazard, err := fusioncapture.ValidateReplicaPublication(record.Publication)
		if err != nil {
			return Prepared{}, ErrRefused
		}
		out.Target = hazard.Hazard.PositionM
		out.SampleValidUntil = minimum(out.SampleValidUntil, hazard.SampleValidUntil)
		out.PhysicalValidUntil = hazard.PhysicalValidUntil
	case trackcapture.ResultSchema:
		track, err := trackcapture.ValidatePublication(record.Publication)
		if err != nil {
			return Prepared{}, ErrRefused
		}
		out.Target = [3]float64{track.PositionXZ[0], binding.FloorY, track.PositionXZ[1]}
		out.SampleValidUntil = minimum(out.SampleValidUntil, track.ValidUntil)
		out.PhysicalValidUntil = track.PhysicalValidUntil()
	default:
		return Prepared{}, ErrRefused
	}
	for _, until := range []time.Time{time.UnixMilli(record.ExpiresAtUnixMS), time.UnixMilli(delegation.ExpiresAtUnixMS), v.Device.ValidUntil} {
		out.PhysicalValidUntil = minimum(out.PhysicalValidUntil, until)
	}
	if out.PhysicalValidUntil.IsZero() || out.PhysicalValidUntil.UnixMilli() <= 0 {
		return Prepared{}, ErrRefused
	}
	c.Timing.AgeHint = 0
	canonical, err := json.Marshal(c)
	if err != nil {
		return Prepared{}, ErrRefused
	}
	out.Trigger = tasking.TriggerRef{SHA256: digest(canonical), Component: c}
	out.Envelope, out.EnvelopeSHA256 = bytes.Clone(input.Envelope), digest(input.Envelope)
	return out, nil
}
