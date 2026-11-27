package fusioncapture

import (
	"bytes"
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/fusion"
	"github.com/wendylabsinc/wendy/go/world/internal/sensing"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
	"github.com/wendylabsinc/wendy/go/world/production/jws"
	"github.com/wendylabsinc/wendy/go/world/production/resultrecord"
	"github.com/wendylabsinc/wendy/go/world/production/sensorrecord"
	"github.com/wendylabsinc/wendy/go/world/production/serviceorigin"
)

// ReplicaInput accepts the two existing PublicationJSON representations. A
// compact LiDAR summary does not contain or independently verify the raw scan.
type ReplicaInput struct {
	Sample       *sensing.Sample       `json:"sample,omitempty"`
	LidarSummary *LidarSummary         `json:"lidar_summary,omitempty"`
	SampleID     string                `json:"sample_id"`
	SensorID     string                `json:"sensor_id"`
	Kind         string                `json:"kind"`
	Source       world.Source          `json:"source"`
	Version      world.Version         `json:"version"`
	SampleTime   time.Time             `json:"sample_time"`
	CaptureStart time.Time             `json:"capture_start"`
	ValidUntil   time.Time             `json:"valid_until"`
	Evidence     *sensing.EvidenceRef  `json:"evidence,omitempty"`
	Capture      *fusion.CaptureOrigin `json:"capture"`
}

type LidarSummary struct {
	PointCount             int     `json:"point_count"`
	MaxRangeM              float64 `json:"max_range_m"`
	NormalizedSampleSHA256 string  `json:"normalized_sample_sha256"`
}

// ReplicaHazard is the origin's typed interpretation, including its immutable
// physical input cap. It is not a reconstruction from independently read media.
type ReplicaHazard struct {
	Label              string                  `json:"label"`
	Confidence         float64                 `json:"confidence"`
	ConfidenceMeaning  string                  `json:"confidence_meaning"`
	PositionM          [3]float64              `json:"position_m"`
	UncertaintyM       float64                 `json:"uncertainty_m"`
	LocationBasis      string                  `json:"location_basis"`
	GasValuePPM        float64                 `json:"gas_value_ppm"`
	GasUncertaintyPPM  float64                 `json:"gas_uncertainty_ppm"`
	GasSpecies         string                  `json:"gas_species"`
	Cameras            []fusion.CameraFeature  `json:"cameras"`
	Audio              fusion.AudioFeatures    `json:"audio"`
	Inputs             []ReplicaInput          `json:"inputs"`
	Evidence           []sensing.EvidenceRef   `json:"evidence"`
	ValidUntil         time.Time               `json:"valid_until"`
	InputDigest        string                  `json:"input_digest"`
	Spatial            *fusion.SpatialFeatures `json:"spatial,omitempty"`
	PhysicalValidUntil time.Time               `json:"physical_valid_until"`
}

type ReplicaResult struct {
	Hazard             ReplicaHazard
	Result             resultrecord.Result
	SampleValidUntil   time.Time
	PhysicalValidUntil time.Time
}

var errReplica = errors.New("origin-attested fusion hazard refused")

func replicaTime(t time.Time) bool { return !t.IsZero() && t.UnixMilli() > 0 && t.Year() <= 9999 }
func replicaFinite(values ...float64) bool {
	for _, v := range values {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return false
		}
	}
	return true
}

func replicaFields(raw []byte, required []string, vectors map[string]int) (map[string]json.RawMessage, error) {
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil {
		return nil, errReplica
	}
	for _, name := range required {
		if value := fields[name]; len(value) == 0 || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return nil, errReplica
		}
	}
	for name, size := range vectors {
		var values []json.RawMessage
		if json.Unmarshal(fields[name], &values) != nil || len(values) != size {
			return nil, errReplica
		}
	}
	return fields, nil
}

// ValidateReplicaPublication validates the known synthetic hazard shape and
// exact ordered generic proof ancestry. The caller must verify the original
// result signature and current source authority, bind its tenant/site/device,
// and check both returned deadlines with fresh clocks after blocking work.
// No input proof, image, audio or LiDAR scan is fetched or independently verified.
// Older hazard bodies without physical_valid_until cannot be interpreted live
// at a remote replica; local archived-input validation remains unchanged.
func ValidateReplicaPublication(request serviceorigin.PublishRequest) (ReplicaResult, error) {
	var out ReplicaResult
	r, err := resultrecord.Decode(request.Payload)
	if err != nil || r.ResultSchema != fusion.HazardType {
		return out, errReplica
	}
	var h ReplicaHazard
	if jws.DecodeStrict(r.ResultJSON, &h) != nil {
		return out, errReplica
	}
	fields, err := replicaFields(r.ResultJSON, []string{"label", "confidence", "confidence_meaning", "position_m", "uncertainty_m", "location_basis", "gas_value_ppm", "gas_uncertainty_ppm", "gas_species", "cameras", "audio", "inputs", "evidence", "valid_until", "input_digest", "physical_valid_until"}, map[string]int{"position_m": 3})
	if err != nil || h.Label != "possible-gas-hazard" || h.Confidence != .8 || h.ConfidenceMeaning != "fixed synthetic rule score, not a calibrated probability" || !replicaFinite(h.PositionM[:]...) || !replicaFinite(h.UncertaintyM, h.GasValuePPM, h.GasUncertaintyPPM, h.Audio.RMS, h.Audio.ToneFraction) || h.UncertaintyM < 0 || h.GasValuePPM < 0 || h.GasUncertaintyPPM < 0 || h.Audio.RMS < .05 || h.Audio.RMS > 1 || h.Audio.ToneFraction < .6 || h.Audio.ToneFraction > 1 || !sensing.DigestValid(h.InputDigest) || !replicaTime(h.ValidUntil) || !replicaTime(h.PhysicalValidUntil) || len(h.Inputs) < 4 || len(h.Inputs) > resultrecord.MaxInputs || len(h.Inputs) != len(r.Inputs) || len(h.Cameras) != 2 || len(h.Evidence) != 3 {
		return out, errReplica
	}
	if _, err = replicaFields(fields["audio"], []string{"rms", "tone_fraction"}, nil); err != nil {
		return out, err
	}
	if request.ComponentType != resultrecord.ComponentType || request.Tombstone || request.Stream == "" || request.Frame.Validate() != nil || request.Timing.Validate() != nil || !request.Timing.ErrorKnown || request.TTL <= 0 || request.TTL > sensorrecord.MaxLifetime || max(request.Timing.ErrorBound, request.Timing.AgeHint) >= request.TTL || request.Provenance.Validate() != nil || request.Provenance.Origin != world.Inferred || request.Provenance.AppVersion != fusion.AppVersion || request.Provenance.ModelID != fusion.ModelID || request.Provenance.ModelVersion != fusion.ModelVersion || len(request.Provenance.Inputs) != len(r.Inputs) {
		return out, errReplica
	}
	var rawInputs []json.RawMessage
	if json.Unmarshal(fields["inputs"], &rawInputs) != nil {
		return out, errReplica
	}
	counts := map[string]int{}
	seen := map[string]bool{}
	samples := map[string]*sensing.Sample{}
	var base *sensing.Sample
	var oldest, newest, until, anchor time.Time
	var boot string
	var evidence []sensing.EvidenceRef
	totalPoints := 0
	for i, input := range h.Inputs {
		ref := r.Inputs[i]
		f, e := replicaFields(rawInputs[i], []string{"sample_id", "sensor_id", "kind", "source", "version", "sample_time", "capture_start", "valid_until", "capture"}, nil)
		if e != nil || input.Capture == nil || seen[input.SensorID] || input.Source != ref.Key.Source || input.Version != ref.Version || input.Capture.ProofSHA256 != ref.EnvelopeSHA256 || ref.Key.Type != sensorrecord.ComponentType || ref.Key.Entity != "sensor/"+input.SensorID || ref.Key.Source.Stream != input.SensorID || ref.Key.Tenant != r.Inputs[0].Key.Tenant || ref.Key.Site != r.Inputs[0].Key.Site || !replicaTime(input.SampleTime) || !replicaTime(input.CaptureStart) || !replicaTime(input.ValidUntil) || !input.ValidUntil.After(input.SampleTime) || input.ValidUntil.Sub(input.SampleTime) > sensorrecord.MaxLifetime || !replicaTime(input.Capture.SimulationAnchor) || !time.Unix(0, input.Capture.SimulationAnchor.UnixNano()).Equal(input.Capture.SimulationAnchor) || input.Capture.SceneBootID == "" || len(input.Capture.SceneBootID) > 128 {
			return out, errReplica
		}
		if i > 0 && h.Inputs[i-1].SensorID >= input.SensorID {
			return out, errReplica
		}
		if _, e = replicaFields(f["capture"], []string{"proof_sha256", "scene_boot_id", "simulation_anchor"}, nil); e != nil {
			return out, e
		}
		if i == 0 {
			anchor, boot = input.Capture.SimulationAnchor, input.Capture.SceneBootID
		} else if !anchor.Equal(input.Capture.SimulationAnchor) || boot != input.Capture.SceneBootID {
			return out, errReplica
		}
		seen[input.SensorID] = true
		counts[input.Kind]++
		start := input.SampleTime
		if input.Kind == "lidar" {
			v := input.LidarSummary
			if _, present := f["sample"]; present {
				return out, errReplica
			}
			if input.Sample != nil || v == nil || !sensing.LidarSensorID(input.SensorID) || input.Evidence != nil || input.Capture.OriginalEvidence != nil || v.PointCount < 0 || v.PointCount > sensing.MaxLidarPoints || !replicaFinite(v.MaxRangeM) || v.MaxRangeM <= 0 || !sensing.DigestValid(v.NormalizedSampleSHA256) {
				return out, errReplica
			}
			if _, e = replicaFields(f["lidar_summary"], []string{"point_count", "max_range_m", "normalized_sample_sha256"}, nil); e != nil {
				return out, e
			}
			totalPoints += v.PointCount
		} else {
			if _, present := f["lidar_summary"]; present {
				return out, errReplica
			}
			if input.Sample == nil || input.LidarSummary != nil {
				return out, errReplica
			}
			sample, e := sensing.Decode(f["sample"])
			if e != nil || sample.SensorLink != nil || sample.Kind != input.Kind || sample.ID != input.SampleID || sample.SensorID != input.SensorID || sample.SiteID != ref.Key.Site || sample.Frame != request.Frame || !input.SampleTime.Equal(anchor.Add(time.Duration(sample.SampleNS))) || !reflect.DeepEqual(input.Evidence, sample.Evidence) {
				return out, errReplica
			}
			if base == nil {
				copy := sample
				base = &copy
			} else if sample.RunID != base.RunID || sample.ScenarioVersion != base.ScenarioVersion || sample.Seed != base.Seed || sample.PackageSHA256 != base.PackageSHA256 {
				return out, errReplica
			}
			samples[input.SensorID] = input.Sample
			if sample.Audio != nil {
				start = start.Add(-time.Duration(sample.Audio.DurationNS))
			}
			if sample.Camera != nil && input.ValidUntil.After(input.SampleTime.Add(time.Duration(sample.Camera.CalibrationValidForNS))) {
				return out, errReplica
			}
			if sample.Gas != nil && (h.GasValuePPM != sample.Gas.ValuePPM || h.GasUncertaintyPPM != sample.Gas.UncertaintyPPM || h.GasSpecies != sample.Gas.Species) {
				return out, errReplica
			}
			if sample.Evidence != nil {
				evidence = append(evidence, *sample.Evidence)
			}
			if original := input.Capture.OriginalEvidence; original != nil && (original.Validate() != nil || sample.Camera == nil || sample.Evidence.ContentType != "image/jpeg" || original.ContentType != "image/png") {
				return out, errReplica
			}
		}
		if !start.Equal(input.CaptureStart) {
			return out, errReplica
		}
		if oldest.IsZero() || start.Before(oldest) {
			oldest = start
		}
		if input.SampleTime.After(newest) {
			newest = input.SampleTime
		}
		if until.IsZero() || input.ValidUntil.Before(until) {
			until = input.ValidUntil
		}
	}
	if base == nil || counts["camera"] != 2 || counts["gas"] != 1 || counts["audio"] != 1 || counts["lidar"] != len(h.Inputs)-4 || newest.Sub(oldest) > time.Second || !reflect.DeepEqual(evidence, h.Evidence) || request.Entity != base.RunID+"/possible-gas-hazard" || request.Timing.ClockDomain != "simulation:"+base.RunID || !request.Timing.SampleTime.Equal(oldest) || !h.ValidUntil.Equal(until) || request.Timing.SampleTime.Add(request.TTL+request.Timing.ErrorBound).After(until) {
		return out, errReplica
	}
	for i, input := range h.Inputs {
		parts := strings.Split(input.SampleID, "/")
		if len(parts) != 3 || parts[0] != base.RunID || parts[1] != input.SensorID {
			return out, errReplica
		}
		if n, e := strconv.ParseUint(parts[2], 10, 64); e != nil || n == 0 || strconv.FormatUint(n, 10) != parts[2] {
			return out, errReplica
		}
		if input.SampleTime.Before(anchor) || input.SampleTime.Sub(anchor) > 24*time.Hour {
			return out, errReplica
		}
		leaf := request.Provenance.Inputs[i]
		want := world.Input{ID: r.Inputs[i].ID(), Origin: world.Simulated, Simulation: &world.SimulationContext{RunID: base.RunID, ScenarioVersion: base.ScenarioVersion, Seed: base.Seed, ClockDomain: request.Timing.ClockDomain}}
		if !reflect.DeepEqual(leaf, want) {
			return out, errReplica
		}
	}
	if err = replicaFeatures(h, fields, samples, counts["lidar"], totalPoints); err != nil {
		return out, err
	}
	out = ReplicaResult{Hazard: h, Result: r, SampleValidUntil: request.Timing.SampleTime.Add(request.TTL - max(request.Timing.ErrorBound, request.Timing.AgeHint)), PhysicalValidUntil: h.PhysicalValidUntil}
	return out, nil
}

func replicaCameras(raw []byte, cameras []fusion.CameraFeature, samples map[string]*sensing.Sample, spatial bool) error {
	var rows []json.RawMessage
	if json.Unmarshal(raw, &rows) != nil || len(rows) != 2 || len(cameras) != 2 {
		return errReplica
	}
	for i, camera := range cameras {
		fields, err := replicaFields(rows[i], []string{"sensor_id", "red_fraction"}, nil)
		sample := samples[camera.SensorID]
		if err != nil || sample == nil || sample.Camera == nil || i > 0 && cameras[i-1].SensorID >= camera.SensorID || !replicaFinite(camera.RedFraction) || camera.RedFraction <= 0 || camera.RedFraction > 1 || spatial != (camera.Observation != nil) {
			return errReplica
		}
		if !spatial {
			if camera.RedFraction < .005 {
				return errReplica
			}
			continue
		}
		if _, err = replicaFields(fields["observation"], []string{"pixels", "centroid_px", "bounds_px", "ray_origin_m", "ray_direction", "reprojection_error_px"}, map[string]int{"centroid_px": 2, "bounds_px": 4, "ray_origin_m": 3, "ray_direction": 3}); err != nil {
			return err
		}
		o := camera.Observation
		c := sample.Camera
		if o.Pixels < 4 || o.Pixels > c.Width*c.Height || !replicaFinite(o.CentroidPX[:]...) || !replicaFinite(o.RayOriginM[:]...) || !replicaFinite(o.RayDirection[:]...) || !replicaFinite(o.ReprojectionErrorPX) || o.ReprojectionErrorPX < 0 || o.BoundsPX[0] < 0 || o.BoundsPX[1] < 0 || o.BoundsPX[2] > c.Width || o.BoundsPX[3] > c.Height || o.BoundsPX[0] >= o.BoundsPX[2] || o.BoundsPX[1] >= o.BoundsPX[3] || o.CentroidPX[0] < float64(o.BoundsPX[0]) || o.CentroidPX[0] >= float64(o.BoundsPX[2]) || o.CentroidPX[1] < float64(o.BoundsPX[1]) || o.CentroidPX[1] >= float64(o.BoundsPX[3]) {
			return errReplica
		}
		norm := o.RayDirection[0]*o.RayDirection[0] + o.RayDirection[1]*o.RayDirection[1] + o.RayDirection[2]*o.RayDirection[2]
		if math.Abs(norm-1) > 1e-6 {
			return errReplica
		}
	}
	return nil
}

func replicaFeatures(h ReplicaHazard, fields map[string]json.RawMessage, samples map[string]*sensing.Sample, lidars, totalPoints int) error {
	if replicaCameras(fields["cameras"], h.Cameras, samples, lidars > 0) != nil {
		return errReplica
	}
	if lidars == 0 {
		if h.Spatial != nil || h.LocationBasis != "gas sensor position; leak source is not localized" {
			return errReplica
		}
		for _, sample := range samples {
			if h.UncertaintyM < sample.UncertaintyM || sample.Gas != nil && (h.PositionM != [3]float64{sample.PositionM[0], sample.PositionM[1], sample.PositionM[2]}) {
				return errReplica
			}
		}
		return nil
	}
	spatial := h.Spatial
	if spatial == nil || !spatial.Associated || spatial.Reason != "two camera rays and observed LiDAR surfaces agree" || h.LocationBasis != "two calibrated camera observations associated with LiDAR surface returns" || h.PositionM != spatial.PositionM || h.UncertaintyM != spatial.UncertaintyM || !replicaFinite(spatial.PositionM[:]...) || !replicaFinite(spatial.TriangulatedM[:]...) || !replicaFinite(spatial.UncertaintyM, spatial.RaySeparationM, spatial.RayAngleDeg, spatial.ReprojectionErrorPX) || spatial.UncertaintyM < 0 || spatial.RaySeparationM < 0 || spatial.RayAngleDeg < 0 || spatial.RayAngleDeg > 90 || spatial.ReprojectionErrorPX < 0 || spatial.LidarPointsConsidered < 1 || spatial.LidarPointsConsidered > totalPoints || spatial.LidarSupportCount < 1 || spatial.LidarSupportCount > spatial.LidarPointsConsidered || len(spatial.LidarSupportPoints) != min(32, spatial.LidarSupportCount) {
		return errReplica
	}
	f, err := replicaFields(fields["spatial"], []string{"associated", "reason", "position_m", "triangulated_m", "uncertainty_m", "ray_separation_m", "ray_angle_deg", "reprojection_error_px", "cameras", "lidar_points_considered", "lidar_support_count", "lidar_support_points"}, map[string]int{"position_m": 3, "triangulated_m": 3})
	if err != nil || replicaCameras(f["cameras"], spatial.Cameras, samples, true) != nil {
		return errReplica
	}
	for i, camera := range spatial.Cameras {
		other := h.Cameras[i]
		if camera.SensorID != other.SensorID || camera.RedFraction != other.RedFraction {
			return errReplica
		}
		a, b := *camera.Observation, *other.Observation
		a.ReprojectionErrorPX, b.ReprojectionErrorPX = 0, 0
		if a != b {
			return errReplica
		}
	}
	var points []json.RawMessage
	if json.Unmarshal(f["lidar_support_points"], &points) != nil {
		return errReplica
	}
	for i, raw := range points {
		var coords []json.RawMessage
		if json.Unmarshal(raw, &coords) != nil || len(coords) != 3 || !replicaFinite(spatial.LidarSupportPoints[i][:]...) {
			return errReplica
		}
	}
	return nil
}
