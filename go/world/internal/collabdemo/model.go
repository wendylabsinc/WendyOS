// Package collabdemo coordinates a synthetic, in-memory inspection demonstration.
// It is an application planner, not production mission authority or a safety controller.
package collabdemo

import (
	"encoding/json"
	"errors"
)

type Vec [2]float64
type Cell [2]int

func (v *Vec) UnmarshalJSON(raw []byte) error {
	var values []float64
	if err := json.Unmarshal(raw, &values); err != nil {
		return err
	}
	if len(values) != 2 || !finite(values[0]) || !finite(values[1]) {
		return errors.New("coordinate must contain exactly two finite numbers")
	}
	*v = Vec{values[0], values[1]}
	return nil
}
func (c *Cell) UnmarshalJSON(raw []byte) error {
	var values []int
	if err := json.Unmarshal(raw, &values); err != nil {
		return err
	}
	if len(values) != 2 {
		return errors.New("grid cell must contain exactly two integers")
	}
	*c = Cell{values[0], values[1]}
	return nil
}

type Grid struct {
	Origin     Vec     `json:"origin"`
	Resolution float64 `json:"resolution"`
	Width      int     `json:"width"`
	Height     int     `json:"height"`
	Blocked    []Cell  `json:"blocked"`
}
type Robot struct {
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Color json.RawMessage `json:"color,omitempty"`
	Start Vec             `json:"start"`
}
type Job struct {
	ID       string `json:"id"`
	Label    string `json:"label"`
	Position Vec    `json:"position"`
}
type Scene struct {
	Robots []Robot `json:"robots"`
	Grid   Grid    `json:"grid"`
	Jobs   []Job   `json:"jobs"`
}
type Observation struct {
	ID        string `json:"id"`
	Position  Vec    `json:"position"`
	Velocity  Vec    `json:"velocity"`
	Online    bool   `json:"online"`
	Available bool   `json:"available"`
}
type Input struct {
	Exclusions []Exclusion     `json:"exclusions,omitempty"`
	Preview    *PreviewRequest `json:"preview,omitempty"`
	RunID      string          `json:"run_id"`
	Sequence   uint64          `json:"sequence"`
	Time       float64         `json:"time_s"`
	Authorized bool            `json:"authorized"`
	Stop       bool            `json:"stop"`
	Robots     []Observation   `json:"robots"`
}
type RobotOutput struct {
	ID       string `json:"id"`
	Velocity Vec    `json:"velocity"`
	State    string `json:"state"`
	JobID    string `json:"job_id,omitempty"`
	Path     []Vec  `json:"path"`
}
type Reading struct {
	GasPPM     float64 `json:"gas_ppm"`
	Position   Vec     `json:"position"`
	Time       float64 `json:"sample_time_s"`
	Provenance string  `json:"provenance"`
}
type JobOutput struct {
	ID         string   `json:"id"`
	Label      string   `json:"label"`
	AssignedTo string   `json:"assigned_to,omitempty"`
	State      string   `json:"state"`
	Reading    *Reading `json:"reading,omitempty"`
}
type Output struct {
	Exclusions       []Exclusion   `json:"exclusions,omitempty"`
	ExclusionsFrozen bool          `json:"exclusions_frozen"`
	Preview          *RoutePreview `json:"preview,omitempty"`
	RunID            string        `json:"run_id"`
	Sequence         uint64        `json:"sequence"`
	Time             float64       `json:"time_s"`
	Phase            string        `json:"phase"`
	Robots           []RobotOutput `json:"robots"`
	Jobs             []JobOutput   `json:"jobs"`
	Events           []string      `json:"events"`
	Completed        int           `json:"completed"`
	Total            int           `json:"total"`
}
