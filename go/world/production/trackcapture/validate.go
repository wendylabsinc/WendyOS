package trackcapture

import (
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"strings"
	"time"
	"unicode"

	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/sensing"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
	"github.com/wendylabsinc/wendy/go/world/production/proofref"
	"github.com/wendylabsinc/wendy/go/world/production/resultrecord"
	"github.com/wendylabsinc/wendy/go/world/production/sensorrecord"
	"github.com/wendylabsinc/wendy/go/world/production/serviceorigin"
)

func text(s string, n int) bool {
	return s != "" && len(s) <= n && strings.TrimSpace(s) == s && strings.IndexFunc(s, unicode.IsControl) < 0
}
func finite(v ...float64) bool {
	for _, x := range v {
		if math.IsNaN(x) || math.IsInf(x, 0) {
			return false
		}
	}
	return true
}
func covariance(c [4]float64) bool {
	return finite(c[:]...) && c[0] > 0 && c[3] > 0 && math.Abs(c[1]-c[2]) <= 1e-10 && c[0]*c[3] > c[1]*c[2]
}

// PhysicalValidUntil is an immutable interpretation bound. It does not read a
// clock or establish current origin authority or independent camera verification.
func (p Payload) PhysicalValidUntil() time.Time {
	var until time.Time
	for _, list := range [][]Member{p.Current, p.Previous} {
		for _, m := range list {
			until = minimum(until, m.PhysicalUntil)
			until = minimum(until, time.Unix(0, m.CapturedUnixNS).Add(sensorrecord.MaxPhysicalAge))
		}
	}
	return until
}

func fields(raw []byte, names []string, vectors map[string]int) error {
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &object) != nil {
		return errors.New("track object required")
	}
	for _, key := range names {
		v, ok := object[key]
		if !ok || string(v) == "null" {
			return errors.New("track field missing or null")
		}
	}
	for key, size := range vectors {
		v, ok := object[key]
		if !ok {
			continue
		}
		var array []json.RawMessage
		if json.Unmarshal(v, &array) != nil || len(array) != size {
			return errors.New("track vector shape differs")
		}
		for _, part := range array {
			if string(part) == "null" {
				return errors.New("null track coordinate")
			}
		}
	}
	return nil
}

// Decode enforces the closed anonymous geometry/lineage schema. Unknown fields,
// including boxes, pose pixels and appearance data, are not accepted.
func Decode(raw []byte) (Payload, error) {
	var p Payload
	if authority.DecodeStrictLimit(raw, &p, resultrecord.MaxResultBytes) != nil {
		return p, errors.New("invalid typed track payload")
	}
	if err := fields(raw, []string{"schema", "algorithm", "algorithm_version", "track_id", "revision", "run_id", "clock_domain", "frame", "observed_at", "valid_until", "position_xz", "covariance", "ambiguous", "motion_known", "current"}, map[string]int{"position_xz": 2, "covariance": 4, "velocity_xz": 2, "velocity_covariance": 4}); err != nil {
		return p, err
	}
	var object map[string]json.RawMessage
	_ = json.Unmarshal(raw, &object)
	for _, key := range []string{"current", "previous"} {
		if v, ok := object[key]; ok {
			var members []json.RawMessage
			if json.Unmarshal(v, &members) != nil {
				return p, errors.New("invalid track members")
			}
			for _, member := range members {
				if err := fields(member, []string{"proof", "sensor_id", "sample_id", "capture_sequence", "member_index", "sample_time", "valid_until", "physical_until", "position_xz", "covariance", "image", "model_id", "model_version", "model_sha256", "calibration_revision", "calibration_sha256", "scene_boot_id", "captured_unix_ns", "simulation_anchor_unix_ns"}, map[string]int{"position_xz": 2, "covariance": 4}); err != nil {
					return p, err
				}
			}
		}
	}
	return p, p.validate()
}

func (p Payload) validate() error {
	if p.Schema != ResultSchema || p.Algorithm != AlgorithmID || p.AlgorithmVersion != AlgorithmVersion || !text(p.TrackID, 160) || p.Revision == 0 || !text(p.RunID, 80) || p.ClockDomain != "simulation:"+p.RunID || p.Frame.Validate() != nil || p.ObservedAt.IsZero() || !p.ValidUntil.After(p.ObservedAt) || !finite(p.PositionXZ[:]...) || !covariance(p.Covariance) || len(p.Current) < 1 || len(p.Current) > MaxMembers || len(p.Previous) > MaxMembers {
		return errors.New("invalid anonymous track geometry or metadata")
	}
	if p.MotionKnown != (p.VelocityXZ != nil && p.VelocityCovariance != nil && p.PreviousObservedAt != nil && len(p.Previous) > 0) {
		return errors.New("motion metadata is incomplete")
	}
	if !p.MotionKnown && (p.VelocityXZ != nil || p.VelocityCovariance != nil || p.PreviousObservedAt != nil || len(p.Previous) > 0) {
		return errors.New("unknown motion carries motion fields")
	}
	if p.MotionKnown && (!finite(p.VelocityXZ[:]...) || !covariance(*p.VelocityCovariance) || !p.PreviousObservedAt.Before(p.ObservedAt)) {
		return errors.New("invalid two-observation motion")
	}
	var until time.Time
	for index, list := range [][]Member{p.Current, p.Previous} {
		var newest time.Time
		seen := map[string]bool{}
		for _, m := range list {
			if m.Proof.Validate() != nil || m.Proof.Key.Type != sensorrecord.ComponentType || m.Proof.Key.Source.Stream != m.SensorID || m.Proof.Key.Entity != "sensor/"+m.SensorID || seen[m.SensorID] || !text(m.SensorID, 80) || !text(m.SampleID, 512) || m.CaptureSequence == 0 || m.MemberIndex >= 64 || m.SampleTime.IsZero() || !m.ValidUntil.After(m.SampleTime) || m.ValidUntil.After(m.SampleTime.Add(DefaultLifetime)) || m.PhysicalUntil.IsZero() || !finite(m.PositionXZ[:]...) || !covariance(m.Covariance) || m.Image.Validate() != nil || m.Image.ContentType != "image/png" && m.Image.ContentType != "image/jpeg" || !text(m.ModelID, 256) || !text(m.ModelVersion, 256) || !sensing.DigestValid(m.ModelSHA256) || !text(m.CalibrationRevision, 256) || !sensing.DigestValid(m.CalibrationSHA256) || !text(m.SceneBootID, 128) || m.CapturedUnixNS <= 0 || m.CapturedUnixNS > math.MaxInt64-int64(sensorrecord.MaxPhysicalAge) || m.SimulationAnchorUnixNS <= 0 || m.PhysicalUntil.After(time.Unix(0, m.CapturedUnixNS).Add(sensorrecord.MaxPhysicalAge)) {
				return errors.New("invalid original track member")
			}
			if m.OriginalImage != nil && (m.OriginalImage.Validate() != nil || m.OriginalImage.ContentType != "image/png" || m.Image.ContentType != "image/jpeg") {
				return errors.New("invalid original SensorLink image")
			}
			seen[m.SensorID] = true
			until = minimum(until, m.ValidUntil)
			if m.SampleTime.After(newest) {
				newest = m.SampleTime
			}
		}
		if index == 0 && !newest.Equal(p.ObservedAt) || index == 1 && p.MotionKnown && !newest.Equal(*p.PreviousObservedAt) {
			return errors.New("track observation timestamp differs from its members")
		}
	}
	if !until.Equal(p.ValidUntil) {
		return errors.New("track deadline differs from required original inputs")
	}
	return nil
}

// ValidatePublication checks typed origin-attested output and its exact generic
// reference/provenance set. A remote caller first verifies origin signatures and
// current authority. This function does NOT independently verify camera proofs;
// local admission continues to perform resultrecord's full archived-input path.
func ValidatePublication(request serviceorigin.PublishRequest) (Payload, error) {
	r, err := resultrecord.Decode(request.Payload)
	if err != nil {
		return Payload{}, err
	}
	p, err := Decode(r.ResultJSON)
	if err != nil {
		return p, err
	}
	if r.ResultSchema != ResultSchema || request.ComponentType != resultrecord.ComponentType || request.Tombstone || request.Entity != p.RunID+"/track/"+p.TrackID || !text(request.Stream, 256) || request.Frame != p.Frame || request.Timing.Validate() != nil || !request.Timing.ErrorKnown || request.Timing.ClockDomain != p.ClockDomain || request.Provenance.Validate() != nil || request.Provenance.Origin != world.Inferred || request.Provenance.AppVersion != AppVersion || request.Provenance.ModelID != AlgorithmID || request.Provenance.ModelVersion != AlgorithmVersion {
		return p, errors.New("track publication metadata differs")
	}
	refs := []proofref.Reference{}
	seen := map[string]bool{}
	exact := map[string]proofref.Reference{}
	var first *Member
	var oldest time.Time
	for _, list := range [][]Member{p.Current, p.Previous} {
		for _, m := range list {
			if previous, ok := exact[m.Proof.EnvelopeSHA256]; ok && previous != m.Proof {
				return p, errors.New("repeated track proof changes source or version")
			}
			exact[m.Proof.EnvelopeSHA256] = m.Proof
			if first == nil {
				copy := m
				first = &copy
			} else if m.Proof.Key.Tenant != first.Proof.Key.Tenant || m.Proof.Key.Site != first.Proof.Key.Site || m.ModelID != first.ModelID || m.ModelVersion != first.ModelVersion || m.ModelSHA256 != first.ModelSHA256 {
				return p, errors.New("track mixes site or inference model")
			}
			oldest = minimum(oldest, m.SampleTime)
			if !seen[m.Proof.EnvelopeSHA256] {
				refs = append(refs, m.Proof)
				seen[m.Proof.EnvelopeSHA256] = true
			}
		}
	}
	if !reflect.DeepEqual(refs, r.Inputs) || len(refs) != len(request.Provenance.Inputs) || !oldest.Equal(request.Timing.SampleTime) || request.TTL <= 0 || request.TTL > DefaultLifetime || request.Timing.ErrorBound > DefaultLifetime || request.Timing.SampleTime.Add(request.TTL+request.Timing.ErrorBound).After(p.ValidUntil) {
		return p, errors.New("track proof ancestry or publication deadline differs")
	}
	for i, ref := range refs {
		leaf := request.Provenance.Inputs[i]
		if leaf.ID != ref.ID() || leaf.Origin != world.Simulated || leaf.Simulation == nil || leaf.Simulation.RunID != p.RunID || leaf.Simulation.ClockDomain != p.ClockDomain {
			return p, errors.New("track simulation ancestry differs")
		}
	}
	return p, nil
}
