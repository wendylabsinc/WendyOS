package fusion

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"sort"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/sensing"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

// VisualConfig commissions exactly two calibrated cameras and one LiDAR. This
// deliberately reuses the existing red-fixture geometry, not a gas detector.
type VisualConfig struct {
	Tenant      string                  `json:"tenant"`
	Site        string                  `json:"site"`
	Frame       world.FrameRef          `json:"frame"`
	Anchor      time.Time               `json:"anchor"`
	Simulation  world.SimulationContext `json:"simulation"`
	Package     string                  `json:"package_sha256"`
	Calibration string                  `json:"calibration_revision"`
	Sources     map[string]world.Source `json:"sources"`
	Models      map[string]string       `json:"sensor_models"`
}
type VisualResult struct {
	Spatial    SpatialFeatures   `json:"spatial"`
	Oldest     time.Time         `json:"oldest"`
	Latest     time.Time         `json:"latest"`
	ValidUntil time.Time         `json:"valid_until"`
	Inputs     []world.Component `json:"inputs"`
}

func (c VisualConfig) Validate() error {
	if c.Tenant == "" || c.Site == "" || c.Frame.Validate() != nil || c.Anchor.IsZero() || c.Simulation.RunID == "" || c.Simulation.ClockDomain != "simulation:"+c.Simulation.RunID || c.Simulation.ScenarioVersion == "" || !sensing.DigestValid(c.Package) || c.Calibration == "" || len(c.Sources) != 3 || len(c.Models) != 3 {
		return errors.New("explicit visual simulation contract required")
	}
	seen := map[world.Source]bool{}
	for _, id := range []string{"camera-a", "camera-b", "lidar-site"} {
		s, ok := c.Sources[id]
		if !ok || c.Models[id] == "" || s.Validate() != nil || s.Service != id || seen[s] {
			return errors.New("exact distinct visual source catalog required")
		}
		seen[s] = true
	}
	return nil
}

// EvaluateVisual retains the raw sensing schema, source/clock/calibration
// bindings, digest checks and post-load expiry check. Geometry uses the same
// observeRed/localize functions as the multimodal fixture. No gas/audio input or
// confidence score is invented to make that legacy rule run.
func EvaluateVisual(ctx context.Context, cfg VisualConfig, records []world.Component, load Loader, now func() time.Time) (VisualResult, error) {
	out := VisualResult{Inputs: []world.Component{}}
	if err := cfg.Validate(); err != nil {
		return out, err
	}
	if now == nil || load == nil || len(records) != 3 {
		return out, errors.New("exact visual inputs, loader and clock required")
	}
	start := now()
	chosen := append([]world.Component(nil), records...)
	sort.Slice(chosen, func(i, j int) bool { return chosen[i].Key.Source.Service < chosen[j].Key.Source.Service })
	seen := map[string]bool{}
	cameras := []spatialCamera{}
	lidars := []sensing.Sample{}
	for _, c := range chosen {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		s, err := sensing.FromComponent(c)
		if err != nil {
			return out, err
		}
		if seen[s.SensorID] || cfg.Sources[s.SensorID] != c.Key.Source || c.Key.Tenant != cfg.Tenant || s.SiteID != cfg.Site || s.Frame != cfg.Frame || s.RunID != cfg.Simulation.RunID || s.ScenarioVersion != cfg.Simulation.ScenarioVersion || s.Seed != cfg.Simulation.Seed || s.PackageSHA256 != cfg.Package || s.CalibrationRevision != cfg.Calibration || s.SensorModel != cfg.Models[s.SensorID] || s.SensorLink != nil || !c.Timing.SampleTime.Equal(cfg.Anchor.Add(time.Duration(s.SampleNS))) || !c.Timing.ErrorKnown || c.Timing.ErrorBound != 0 || c.Timing.AgeHint < 0 || c.TTL <= 0 || c.TTL > 10*time.Second || c.Timing.SampleTime.After(start) {
			return out, errors.New("visual input scope, model, calibration or capture clock differs")
		}
		if (s.SensorID == "lidar-site" && s.Kind != "lidar") || (s.SensorID != "lidar-site" && s.Kind != "camera") {
			return out, errors.New("visual kind differs from catalog")
		}
		seen[s.SensorID] = true
		until := c.Timing.SampleTime.Add(c.TTL)
		if hint := start.Add(c.TTL - c.Timing.AgeHint); hint.Before(until) {
			until = hint
		}
		if s.Camera != nil {
			if cal := c.Timing.SampleTime.Add(time.Duration(s.Camera.CalibrationValidForNS)); cal.Before(until) {
				until = cal
			}
		}
		if !start.Before(until) {
			return out, errors.New("visual input or calibration expired")
		}
		if out.Oldest.IsZero() || c.Timing.SampleTime.Before(out.Oldest) {
			out.Oldest = c.Timing.SampleTime
		}
		if c.Timing.SampleTime.After(out.Latest) {
			out.Latest = c.Timing.SampleTime
		}
		if out.ValidUntil.IsZero() || until.Before(out.ValidUntil) {
			out.ValidUntil = until
		}
		if s.Camera != nil {
			blob, e := load(ctx, *s.Evidence)
			if e != nil {
				return out, e
			}
			hash := sha256.Sum256(blob)
			if int64(len(blob)) != s.Evidence.Size || hex.EncodeToString(hash[:]) != s.Evidence.SHA256 {
				return out, errors.New("visual evidence digest differs")
			}
			observation, e := observeRed(blob, *s.Camera, s.Evidence.ContentType)
			if e != nil {
				return out, e
			}
			cameras = append(cameras, spatialCamera{sample: s, observation: observation})
		} else {
			lidars = append(lidars, s)
		}
		out.Inputs = append(out.Inputs, c)
	}
	if out.Latest.Sub(out.Oldest) > 350*time.Millisecond {
		return out, errors.New("visual capture intervals differ")
	}
	out.Spatial = localize(cameras, lidars)
	if done := now(); done.Before(start) || !done.Before(out.ValidUntil) {
		return VisualResult{}, errors.New("visual support expired during processing")
	}
	return out, nil
}
