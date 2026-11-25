// Package fusion implements an explicit synthetic gas-warning rule. It consumes
// raw signed-replica components and verified media, never another fusion output.
package fusion

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/replay"
	"github.com/wendylabsinc/wendy/go/world/internal/sensing"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

const (
	HazardDetected       = "hazard"
	NotDetected          = "not_detected"
	InsufficientEvidence = "insufficient_evidence"
	ModelID              = "synthetic-gas-warning-rule"
	ModelVersion         = "2"
	AppVersion           = "world-fusion-v2"
	HazardType           = "wendy.hazard.v1"
	ReplayHazardType     = "wendy.hazard.replay.v1"
)

type Loader func(context.Context, sensing.EvidenceRef) ([]byte, error)
type Config struct {
	// CameraQuorum opts into two-of-N spatial fusion. RequiredSensors must
	// then name only gas/audio and required LiDAR, never individual cameras.
	CameraQuorum []string

	Tenant, Site, RunID, ClockDomain string
	Frame                            world.FrameRef
	Now                              func() time.Time
	RequiredSensors                  map[string]string
	// OptionalSensors is a fixed LiDAR catalog. Its presence requires spatial
	// support even when no optional record is currently available.
	OptionalSensors map[string]string
	Sources         map[string]world.Source
	MaxSkew         time.Duration
	GasThresholdPPM float64
	Replay          bool
	RecordingSHA256 string
	// CaptureType/DecodeCapture enable the agent proof adapter. This mode
	// requires exact source bindings and is separate from legacy and replay.
	CaptureType   string
	DecodeCapture CaptureDecoder
}
type InputRef struct {
	Sample       sensing.Sample       `json:"sample"`
	SampleID     string               `json:"sample_id"`
	SensorID     string               `json:"sensor_id"`
	Kind         string               `json:"kind"`
	Source       world.Source         `json:"source"`
	Version      world.Version        `json:"version"`
	SampleTime   time.Time            `json:"sample_time"`
	CaptureStart time.Time            `json:"capture_start"`
	ValidUntil   time.Time            `json:"valid_until"`
	Evidence     *sensing.EvidenceRef `json:"evidence,omitempty"`
	Replay       *replay.Envelope     `json:"replay,omitempty"`
	Capture      *CaptureOrigin       `json:"capture,omitempty"`
}
type CameraFeature struct {
	SensorID    string             `json:"sensor_id"`
	RedFraction float64            `json:"red_fraction"`
	Observation *CameraObservation `json:"observation,omitempty"`
}
type Hazard struct {
	Quorum *QuorumSelection `json:"quorum,omitempty"`

	Label             string                `json:"label"`
	Confidence        float64               `json:"confidence"`
	ConfidenceMeaning string                `json:"confidence_meaning"`
	PositionM         [3]float64            `json:"position_m"`
	UncertaintyM      float64               `json:"uncertainty_m"`
	LocationBasis     string                `json:"location_basis"`
	GasValuePPM       float64               `json:"gas_value_ppm"`
	GasUncertaintyPPM float64               `json:"gas_uncertainty_ppm"`
	GasSpecies        string                `json:"gas_species"`
	Cameras           []CameraFeature       `json:"cameras"`
	Audio             AudioFeatures         `json:"audio"`
	Inputs            []InputRef            `json:"inputs"`
	Evidence          []sensing.EvidenceRef `json:"evidence"`
	ValidUntil        time.Time             `json:"valid_until"`
	InputDigest       string                `json:"input_digest"`
	Spatial           *SpatialFeatures      `json:"spatial,omitempty"`
}
type Result struct {
	Quorum *QuorumSelection `json:"quorum,omitempty"`

	State       string           `json:"state"`
	Reason      string           `json:"reason"`
	InputDigest string           `json:"input_digest,omitempty"`
	Hazard      *Hazard          `json:"hazard,omitempty"`
	Spatial     *SpatialFeatures `json:"spatial,omitempty"`
	cfg         Config
	oldest      time.Time
	provenance  []world.Input
}
type selected struct {
	sample    sensing.Sample
	component world.Component
	expiry    time.Time
	replay    *replay.Envelope
	capture   *CapturedSample
}

func validateConfig(cfg *Config) error {
	if cfg.Tenant == "" || cfg.Site == "" || cfg.RunID == "" || cfg.ClockDomain != "simulation:"+cfg.RunID || cfg.Now == nil {
		return errors.New("explicit simulation scope and clock required")
	}
	if cfg.Replay {
		if !sensing.DigestValid(cfg.RecordingSHA256) || cfg.Sources == nil {
			return errors.New("replay fusion requires an exact recording digest and source bindings")
		}
	} else if cfg.RecordingSHA256 != "" {
		return errors.New("recording digest requires explicit replay fusion")
	}
	if (cfg.CaptureType == "") != (cfg.DecodeCapture == nil) || cfg.CaptureType != "" && (cfg.Replay || cfg.Sources == nil || len(cfg.CaptureType) > 256 || cfg.CaptureType == sensing.ComponentType || cfg.CaptureType == replay.Type) {
		return errors.New("capture fusion requires a distinct decoder, exact sources and live mode")
	}
	if e := cfg.Frame.Validate(); e != nil {
		return e
	}
	if cfg.MaxSkew == 0 {
		cfg.MaxSkew = 350 * time.Millisecond
	}
	if cfg.MaxSkew < 0 || cfg.MaxSkew > time.Second {
		return errors.New("fusion skew outside supported range")
	}
	if cfg.GasThresholdPPM == 0 {
		cfg.GasThresholdPPM = 50
	}
	if math.IsNaN(cfg.GasThresholdPPM) || math.IsInf(cfg.GasThresholdPPM, 0) || cfg.GasThresholdPPM <= 0 {
		return errors.New("invalid gas threshold")
	}
	if len(cfg.RequiredSensors) == 0 {
		cfg.RequiredSensors = map[string]string{"camera-a": "camera", "camera-b": "camera", "gas-1": "gas", "audio-1": "audio"}
	}
	if cfg.Sources != nil && len(cfg.Sources) != len(cfg.RequiredSensors)+len(cfg.OptionalSensors) {
		return errors.New("source bindings must cover every required sensor")
	}
	catalog := make(map[string]string, len(cfg.RequiredSensors)+len(cfg.OptionalSensors))
	for id, kind := range cfg.RequiredSensors {
		catalog[id] = kind
	}
	for id, kind := range cfg.OptionalSensors {
		if kind != "lidar" || catalog[id] != "" {
			return errors.New("only distinct optional LiDAR sources allowed")
		}
		catalog[id] = kind
	}
	counts := map[string]int{}
	for id, kind := range catalog {
		if id == "" || (kind != "camera" && kind != "gas" && kind != "audio" && kind != "lidar") {
			return errors.New("invalid required sensor")
		}
		counts[kind]++
		source, bound := cfg.Sources[id]
		if cfg.Sources != nil && !bound {
			return errors.New("missing required source binding")
		}
		if bound {
			if e := source.Validate(); e != nil {
				return e
			}
			if cfg.CaptureType == "" && source.Service != id || cfg.CaptureType != "" && source.Stream != id {
				return errors.New("source binding disagrees with sensor service")
			}
		}
	}
	if counts["camera"] != 2 || counts["gas"] != 1 || counts["audio"] != 1 || len(catalog) > 16 {
		return errors.New("fixture requires exactly two cameras, one gas sensor and one audio sensor")
	}
	return nil
}
func invalid(reason string) Result { return Result{State: InsufficientEvidence, Reason: reason} }
func Evaluate(ctx context.Context, cfg Config, records []world.Component, load Loader) (Result, error) {
	if len(cfg.CameraQuorum) > 0 {
		return evaluateQuorum(ctx, cfg, records, load)
	}
	if e := validateConfig(&cfg); e != nil {
		return Result{}, e
	}
	if load == nil {
		return Result{}, errors.New("evidence loader required")
	}
	now := cfg.Now()
	if now.IsZero() {
		return Result{}, errors.New("invalid current time")
	}
	// Select the latest source version before validating freshness. A stale newer
	// version must never make an older observation eligible again.
	chosen := map[string]selected{}
	identity := map[string]string{}
	for _, c := range records {
		if c.Key.Type != sensing.ComponentType && c.Key.Type != replay.Type && (cfg.CaptureType == "" || c.Key.Type != cfg.CaptureType) {
			continue
		}
		sensorID := c.Key.Source.Service
		if cfg.CaptureType != "" {
			sensorID = c.Key.Source.Stream
		}
		kind, needed := cfg.RequiredSensors[sensorID]
		if !needed {
			kind, needed = cfg.OptionalSensors[sensorID]
		}
		if !needed {
			continue
		}
		if c.Key.Tenant != cfg.Tenant || c.Key.Site != cfg.Site {
			continue
		}
		if cfg.Replay != (c.Key.Type == replay.Type) {
			return invalid("live and replay sensor modes cannot be mixed"), nil
		}
		if cfg.CaptureType != "" && c.Key.Type != cfg.CaptureType {
			return invalid("agent captures and legacy sensors cannot be mixed"), nil
		}
		if expected, ok := cfg.Sources[sensorID]; ok && c.Key.Source != expected {
			return invalid("sensor source binding mismatch"), nil
		}
		old, exists := chosen[sensorID]
		if exists && old.component.Key.Source != c.Key.Source {
			return invalid("ambiguous source for sensor"), nil
		}
		compare := 1
		if exists {
			compare = c.Version.Compare(old.component.Version)
		}
		if compare < 0 {
			continue
		}
		copyC := c
		copyC.Timing.AgeHint = 0
		wire, e := json.Marshal(copyC)
		if e != nil {
			return invalid("invalid component encoding"), nil
		}
		var captured *CapturedSample
		if cfg.DecodeCapture != nil {
			value, err := cfg.DecodeCapture(c)
			if err == nil {
				err = validateCaptured(c, value)
			}
			if err != nil {
				return invalid("invalid authenticated capture: " + err.Error()), nil
			}
			captured = &value
			originBytes, err := json.Marshal(value.origin())
			if err != nil {
				return invalid("invalid capture origin encoding"), nil
			}
			wire = append(append(wire, '\n'), originBytes...)
		}
		if compare == 0 {
			if identity[sensorID] != string(wire) {
				return invalid("conflicting duplicate sensor version"), nil
			}
			if c.Timing.AgeHint > old.component.Timing.AgeHint {
				old.component.Timing.AgeHint = c.Timing.AgeHint
				chosen[sensorID] = old
			}
			continue
		}
		var s sensing.Sample
		var envelope *replay.Envelope
		runID := ""
		if cfg.Replay {
			var value replay.Envelope
			s, value, e = replay.FromComponent(c)
			if e == nil && value.RecordingSHA256 != cfg.RecordingSHA256 {
				return invalid("replay recording digest mismatch"), nil
			}
			envelope = &value
			runID = value.RunID
		} else if captured != nil {
			s, runID = captured.Sample, captured.Sample.RunID
		} else {
			s, e = sensing.FromComponent(c)
			runID = s.RunID
		}
		if e != nil {
			return invalid("invalid raw sensor component: " + e.Error()), nil
		}
		if s.Kind != kind || runID != cfg.RunID || c.Frame != cfg.Frame || c.Timing.ClockDomain != cfg.ClockDomain {
			return invalid("sensor kind, run, frame or clock mismatch"), nil
		}
		chosen[sensorID] = selected{sample: s, component: c, replay: envelope, capture: captured}
		identity[sensorID] = string(wire)
	}
	for id := range cfg.RequiredSensors {
		if _, ok := chosen[id]; !ok {
			return invalid("required sensor missing"), nil
		}
	}
	// Optional scans must be current and fit the required observations' full
	// interval. Missing or expired peers cannot switch off spatial mode.
	if len(cfg.OptionalSensors) > 0 {
		var lo, hi time.Time
		maxBound := time.Duration(0)
		for id := range cfg.RequiredSensors {
			c, s := chosen[id].component, chosen[id].sample
			start := c.Timing.SampleTime
			if s.Audio != nil {
				start = start.Add(-time.Duration(s.Audio.DurationNS))
			}
			if lo.IsZero() || start.Before(lo) {
				lo = start
			}
			if hi.IsZero() || c.Timing.SampleTime.After(hi) {
				hi = c.Timing.SampleTime
			}
			maxBound = max(maxBound, c.Timing.ErrorBound)
		}
		optional := []string{}
		for id := range cfg.OptionalSensors {
			if _, ok := chosen[id]; ok {
				optional = append(optional, id)
			}
		}
		sort.Slice(optional, func(i, j int) bool {
			a, b := chosen[optional[i]].component.Timing.SampleTime, chosen[optional[j]].component.Timing.SampleTime
			if a.Equal(b) {
				return optional[i] < optional[j]
			}
			return a.After(b)
		})
		for _, id := range optional {
			c := chosen[id].component
			if !c.Timing.ErrorKnown || c.Timing.ErrorBound < 0 || c.Timing.AgeHint < 0 || c.Timing.SampleTime.After(now) {
				return invalid("unknown clock accuracy or future sample"), nil
			}
			nextLo, nextHi := lo, hi
			if c.Timing.SampleTime.Before(nextLo) {
				nextLo = c.Timing.SampleTime
			}
			if c.Timing.SampleTime.After(nextHi) {
				nextHi = c.Timing.SampleTime
			}
			bound := max(maxBound, c.Timing.ErrorBound)
			if c.TTL <= 0 || c.TTL > 24*time.Hour || c.Timing.ErrorBound >= c.TTL || c.Timing.AgeHint >= c.TTL || !now.Before(c.Timing.SampleTime.Add(c.TTL-c.Timing.ErrorBound)) || bound > cfg.MaxSkew/2 || nextHi.Sub(nextLo) > cfg.MaxSkew-2*bound {
				delete(chosen, id)
				continue
			}
			lo, hi, maxBound = nextLo, nextHi, bound
		}
	}
	ids := make([]string, 0, len(chosen))
	for id := range chosen {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	earliest, latest, validUntil := time.Time{}, time.Time{}, time.Time{}
	maxError := time.Duration(0)
	packageDigest, scenario, originalRun := "", "", ""
	var replayTimeline *replay.Envelope
	var captureTimeline *CapturedSample
	var seed uint64
	refs := []InputRef{}
	provenance := []world.Input{}
	hash := sha256.New()
	for _, id := range ids {
		item := chosen[id]
		c, s := item.component, item.sample
		if item.capture != nil {
			if captureTimeline == nil {
				captureTimeline = item.capture
			} else if item.capture.SceneBootID != captureTimeline.SceneBootID || !item.capture.SimulationAnchor.Equal(captureTimeline.SimulationAnchor) {
				return invalid("capture inputs use different scene boots or simulation anchors"), nil
			}
		}
		if item.replay != nil {
			if replayTimeline == nil {
				replayTimeline = item.replay
			} else if !item.replay.CaptureAnchor.Equal(replayTimeline.CaptureAnchor) || !item.replay.ReplayAnchor.Equal(replayTimeline.ReplayAnchor) {
				return invalid("replay inputs use different timeline mappings"), nil
			}
		}
		if !c.Timing.ErrorKnown || c.Timing.ErrorBound < 0 || c.Timing.AgeHint < 0 || c.Timing.SampleTime.After(now) {
			return invalid("unknown clock accuracy or future sample"), nil
		}
		if c.TTL > 24*time.Hour || c.Timing.ErrorBound >= c.TTL || c.Timing.AgeHint >= c.TTL {
			return invalid("sensor expired or invalid lifetime"), nil
		}
		captureStart := c.Timing.SampleTime
		span := time.Duration(0)
		if s.Audio != nil {
			span = time.Duration(s.Audio.DurationNS)
			captureStart = captureStart.Add(-span)
		}
		until := captureStart.Add(c.TTL - c.Timing.ErrorBound)
		hintUntil := now.Add(c.TTL - c.Timing.AgeHint).Add(-span)
		if hintUntil.Before(until) {
			until = hintUntil
		}
		if s.Camera != nil {
			calUntil := c.Timing.SampleTime.Add(time.Duration(s.Camera.CalibrationValidForNS) - c.Timing.ErrorBound)
			if calUntil.Before(until) {
				until = calUntil
			}
		}
		if !now.Before(until) {
			return invalid("sensor or calibration expired"), nil
		}
		item.expiry = until
		chosen[id] = item
		if earliest.IsZero() || captureStart.Before(earliest) {
			earliest = captureStart
		}
		if latest.IsZero() || c.Timing.SampleTime.After(latest) {
			latest = c.Timing.SampleTime
		}
		if validUntil.IsZero() || until.Before(validUntil) {
			validUntil = until
		}
		if c.Timing.ErrorBound > maxError {
			maxError = c.Timing.ErrorBound
		}
		if packageDigest == "" {
			packageDigest, scenario, seed, originalRun = s.PackageSHA256, s.ScenarioVersion, s.Seed, s.RunID
		} else if packageDigest != s.PackageSHA256 || scenario != s.ScenarioVersion || seed != s.Seed || originalRun != s.RunID {
			return invalid("mixed simulation package or seed"), nil
		}
		hash.Write([]byte(identity[id]))
		hash.Write([]byte{0})
		ref := InputRef{Sample: s, SampleID: s.ID, SensorID: id, Kind: s.Kind, Source: c.Key.Source, Version: c.Version, SampleTime: c.Timing.SampleTime, CaptureStart: captureStart, ValidUntil: until, Evidence: s.Evidence, Replay: item.replay}
		if item.capture != nil {
			origin := item.capture.origin()
			ref.Capture = &origin
		}
		refs = append(refs, ref)
		sourceBytes, _ := json.Marshal(c.Key.Source)
		sourceHash := sha256.Sum256(sourceBytes)
		sim := *c.Provenance.Simulation
		input := world.Input{ID: hex.EncodeToString(sourceHash[:]) + "/" + fmt.Sprint(c.Version.Epoch) + "/" + c.Key.Entity + "/" + fmt.Sprint(c.Version.Sequence), Origin: c.Provenance.Origin, Simulation: &sim}
		if item.capture != nil {
			input.ID = "sha256:" + item.capture.ProofSHA256
		}
		if item.replay != nil {
			captureTime := *c.Provenance.OriginalCaptureTime
			input.OriginalCaptureTime = &captureTime
			input.OriginalClockDomain = c.Provenance.OriginalClockDomain
		}
		provenance = append(provenance, input)
	}
	if latest.Sub(earliest) > cfg.MaxSkew || maxError > cfg.MaxSkew/2 || latest.Sub(earliest) > cfg.MaxSkew-2*maxError {
		return invalid("observations are not temporally aligned"), nil
	}
	result := Result{State: NotDetected, Reason: "fixture rule not triggered; this is not clearance", InputDigest: hex.EncodeToString(hash.Sum(nil)), cfg: cfg, oldest: earliest, provenance: provenance}
	h := &Hazard{Label: "possible-gas-hazard", Confidence: .8, ConfidenceMeaning: "fixed synthetic rule score, not a calibrated probability", LocationBasis: "gas sensor position; leak source is not localized", Inputs: refs, ValidUntil: validUntil, InputDigest: result.InputDigest}
	camerasPositive, audioPositive, gasPositive := true, false, false
	var cameras []spatialCamera
	var lidars []sensing.Sample
	spatialMode := len(cfg.OptionalSensors) > 0
	for _, kind := range cfg.RequiredSensors {
		spatialMode = spatialMode || kind == "lidar"
	}
	for _, id := range ids {
		item := chosen[id]
		s := item.sample
		if s.UncertaintyM > h.UncertaintyM {
			h.UncertaintyM = s.UncertaintyM
		}
		if s.Gas != nil {
			copy(h.PositionM[:], s.PositionM)
			h.GasValuePPM = s.Gas.ValuePPM
			h.GasUncertaintyPPM = s.Gas.UncertaintyPPM
			h.GasSpecies = s.Gas.Species
			gasPositive = s.Gas.ValuePPM-s.Gas.UncertaintyPPM > cfg.GasThresholdPPM
			continue
		}
		if s.Lidar != nil {
			lidars = append(lidars, s)
			continue
		}
		if e := ctx.Err(); e != nil {
			return Result{}, e
		}
		blob, e := load(ctx, *s.Evidence)
		if e != nil {
			return invalid("evidence unavailable for " + id), nil
		}
		digest := sha256.Sum256(blob)
		if int64(len(blob)) != s.Evidence.Size || hex.EncodeToString(digest[:]) != s.Evidence.SHA256 {
			return invalid("evidence integrity failure for " + id), nil
		}
		h.Evidence = append(h.Evidence, *s.Evidence)
		if s.Camera != nil {
			feature := CameraFeature{SensorID: id}
			if spatialMode {
				observation, err := observeRed(blob, *s.Camera, s.Evidence.ContentType)
				if err != nil {
					return invalid("invalid camera evidence for " + id), nil
				}
				feature.Observation = &observation.feature
				feature.RedFraction = observation.fraction
				cameras = append(cameras, spatialCamera{sample: s, observation: observation})
				camerasPositive = camerasPositive && observation.feature.Pixels >= 4
			} else {
				fraction, err := RedFractionWithType(blob, *s.Camera, s.Evidence.ContentType)
				if err != nil {
					return invalid("invalid camera evidence for " + id), nil
				}
				feature.RedFraction = fraction
				camerasPositive = camerasPositive && fraction >= .005
			}
			h.Cameras = append(h.Cameras, feature)
		}
		if s.Audio != nil {
			features, e := WAVFeatures(blob, *s.Audio)
			if e != nil {
				return invalid("invalid audio evidence for " + id), nil
			}
			h.Audio = features
			audioPositive = features.RMS >= .05 && features.ToneFraction >= .6
		}
	}
	// Retrieval may consume time. A slow evidence server cannot revive inputs
	// that expired after the initial snapshot was validated.
	if after := cfg.Now(); after.Before(now) || !after.Before(validUntil) {
		return invalid("inputs expired during evidence retrieval or clock regressed"), nil
	}
	if spatialMode {
		spatial := localize(cameras, lidars)
		result.Spatial, h.Spatial = &spatial, &spatial
		if !spatial.Associated {
			result.State, result.Reason = InsufficientEvidence, spatial.Reason
			return result, nil
		}
		h.PositionM, h.UncertaintyM = spatial.PositionM, spatial.UncertaintyM
		h.LocationBasis = "two calibrated camera observations associated with LiDAR surface returns"
	}
	if gasPositive && camerasPositive && audioPositive {
		result.State = HazardDetected
		result.Reason = "gas value minus reported uncertainty above threshold, red fixture seen by both cameras and 880 Hz alarm evidence"
		result.Hazard = h
		if spatialMode {
			result.Reason = "camera/LiDAR localized red fixture corroborated by gas and 880 Hz audio"
		}
	}
	return result, nil
}
func (r Result) Component(source world.Source, epoch, sequence uint64) (world.Component, error) {
	if r.State != HazardDetected || r.Hazard == nil || epoch == 0 || sequence == 0 {
		return world.Component{}, errors.New("only a detected hazard has a component")
	}
	if e := source.Validate(); e != nil {
		return world.Component{}, e
	}
	if !r.cfg.Now().Before(r.Hazard.ValidUntil) || !r.Hazard.ValidUntil.After(r.oldest) {
		return world.Component{}, errors.New("hazard inputs expired")
	}
	data, e := json.Marshal(r.Hazard)
	if e != nil {
		return world.Component{}, e
	}
	typ := HazardType
	if r.cfg.Replay {
		typ = ReplayHazardType
	}
	return world.Component{Key: world.Key{Tenant: r.cfg.Tenant, Site: r.cfg.Site, Entity: r.cfg.RunID + "/possible-gas-hazard", Type: typ, Source: source}, Version: world.Version{Epoch: epoch, Sequence: sequence}, Frame: r.cfg.Frame, Timing: world.Timing{SampleTime: r.oldest, ClockDomain: r.cfg.ClockDomain, ErrorKnown: true}, TTL: r.Hazard.ValidUntil.Sub(r.oldest), Provenance: world.Provenance{Origin: world.Inferred, AppVersion: AppVersion, ModelID: ModelID, ModelVersion: ModelVersion, Inputs: r.provenance}, Payload: data}, nil
}
