package fusion

import (
	"context"
	"errors"
	"math"
	"sort"
	"strings"

	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

// PairAssessment explains an attempted pair. Alternatives are diagnostics, not
// additional inputs to the selected inference or evidence of free space.
type PairAssessment struct {
	Cameras      [2]string `json:"cameras"`
	State        string    `json:"state"`
	Reason       string    `json:"reason"`
	Associated   bool      `json:"associated"`
	UncertaintyM float64   `json:"uncertainty_m,omitempty"`
	Selected     bool      `json:"selected"`
}
type QuorumSelection struct {
	Catalog  []string         `json:"catalog"`
	Required int              `json:"required"`
	Selected []string         `json:"selected"`
	Pairs    []PairAssessment `json:"pairs"`
	Policy   string           `json:"policy"`
}

func evaluateQuorum(ctx context.Context, cfg Config, records []world.Component, load Loader) (Result, error) {
	if len(cfg.CameraQuorum) < 2 || len(cfg.CameraQuorum) > 6 {
		return Result{}, errors.New("camera quorum requires two to six catalog cameras")
	}
	cameras := append([]string(nil), cfg.CameraQuorum...)
	sort.Strings(cameras)
	spatial := len(cfg.OptionalSensors) > 0
	for _, kind := range cfg.RequiredSensors {
		if kind == "camera" {
			return Result{}, errors.New("quorum cameras must not also be required sensors")
		}
		spatial = spatial || kind == "lidar"
	}
	if !spatial {
		return Result{}, errors.New("camera quorum requires configured LiDAR support")
	}
	if len(cfg.RequiredSensors)+len(cfg.OptionalSensors)+len(cameras) > 16 {
		return Result{}, errors.New("quorum catalog exceeds bound")
	}
	for i, id := range cameras {
		if id == "" || len(id) > 80 || strings.TrimSpace(id) != id || i > 0 && cameras[i-1] == id || cfg.RequiredSensors[id] != "" || cfg.OptionalSensors[id] != "" {
			return Result{}, errors.New("invalid or overlapping quorum camera catalog")
		}
	}
	if cfg.Sources != nil && len(cfg.Sources) != len(cfg.RequiredSensors)+len(cfg.OptionalSensors)+len(cameras) {
		return Result{}, errors.New("source bindings must cover complete quorum catalog")
	}
	selection := &QuorumSelection{Catalog: cameras, Required: 2, Selected: []string{}, Pairs: []PairAssessment{}, Policy: "lowest reported geometric uncertainty; lexical pair breaks ties; no hazard-score preference"}
	best := invalid("no camera pair has fresh aligned camera/LiDAR support")
	bestIndex := -1
	for i, a := range cameras {
		for _, b := range cameras[i+1:] {
			if err := ctx.Err(); err != nil {
				return Result{}, err
			}
			pair := cfg
			pair.CameraQuorum = nil
			pair.RequiredSensors = make(map[string]string, len(cfg.RequiredSensors)+2)
			for id, kind := range cfg.RequiredSensors {
				pair.RequiredSensors[id] = kind
			}
			pair.RequiredSensors[a], pair.RequiredSensors[b] = "camera", "camera"
			if cfg.Sources != nil {
				pair.Sources = map[string]world.Source{}
				for id := range pair.RequiredSensors {
					source, ok := cfg.Sources[id]
					if !ok {
						return Result{}, errors.New("quorum source binding missing")
					}
					pair.Sources[id] = source
				}
				for id := range cfg.OptionalSensors {
					source, ok := cfg.Sources[id]
					if !ok {
						return Result{}, errors.New("quorum source binding missing")
					}
					pair.Sources[id] = source
				}
			}
			// Evaluate owns all existing source/version, capture, replay, media, geometry
			// and lifetime checks. Invalid alternatives cannot weaken the winning pair.
			result, err := Evaluate(ctx, pair, records, load)
			if err != nil {
				return Result{}, err
			}
			assessment := PairAssessment{Cameras: [2]string{a, b}, State: result.State, Reason: result.Reason}
			if result.Spatial != nil && result.Spatial.Associated {
				assessment.Associated = true
				assessment.UncertaintyM = result.Spatial.UncertaintyM
				if !math.IsNaN(assessment.UncertaintyM) && !math.IsInf(assessment.UncertaintyM, 0) && (bestIndex < 0 || assessment.UncertaintyM < selection.Pairs[bestIndex].UncertaintyM) {
					best, bestIndex = result, len(selection.Pairs)
				}
			}
			selection.Pairs = append(selection.Pairs, assessment)
		}
	}
	if bestIndex >= 0 {
		selection.Pairs[bestIndex].Selected = true
		selected := selection.Pairs[bestIndex].Cameras
		selection.Selected = []string{selected[0], selected[1]}
		// Evaluating alternatives takes time; never return an already-expired winner.
		if best.Hazard != nil && !best.cfg.Now().Before(best.Hazard.ValidUntil) {
			best = invalid("selected camera pair expired while evaluating alternatives")
			selection.Selected = []string{}
			selection.Pairs[bestIndex].Selected = false
		}
	}
	best.Quorum = selection
	if best.Hazard != nil {
		best.Hazard.Quorum = selection
	}
	return best, nil
}
