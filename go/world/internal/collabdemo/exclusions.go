package collabdemo

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
)

// Exclusion is operator configuration for this synthetic office run, not a
// measured obstacle, world-layer edit, or physical motion authorization.
type Exclusion struct {
	ID  string `json:"id"`
	Min Vec    `json:"min"`
	Max Vec    `json:"max"`
}

// UnmarshalJSON requires both corners. Missing coordinates must not become
// zero-valued points, and a custom decoder must retain unknown-field rejection.
// Decode into temporary values so a malformed edit cannot partially replace e.
func (e *Exclusion) UnmarshalJSON(raw []byte) error {
	var wire struct {
		ID  string          `json:"id"`
		Min json.RawMessage `json:"min"`
		Max json.RawMessage `json:"max"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&wire); err != nil {
		return err
	}
	var trailing any
	if decoder.Decode(&trailing) != io.EOF {
		return errors.New("one exclusion object is required")
	}
	corner := func(raw json.RawMessage) (Vec, error) {
		var numbers []*float64
		if err := json.Unmarshal(raw, &numbers); err != nil {
			return Vec{}, err
		}
		if len(numbers) != 2 || numbers[0] == nil || numbers[1] == nil || !finite(*numbers[0]) || !finite(*numbers[1]) {
			return Vec{}, errors.New("exclusion corner requires exactly two finite non-null numbers")
		}
		return Vec{*numbers[0], *numbers[1]}, nil
	}
	if len(wire.Min) == 0 || len(wire.Max) == 0 {
		return errors.New("exclusion requires both min and max corners")
	}
	min, err := corner(wire.Min)
	if err != nil {
		return err
	}
	max, err := corner(wire.Max)
	if err != nil {
		return err
	}
	*e = Exclusion{ID: wire.ID, Min: min, Max: max}
	return nil
}

const ExclusionClearance = 0.32 // Matches this office's static robot-clearance inflation.

type PreviewRequest struct {
	RobotID string `json:"robot_id"`
	JobID   string `json:"job_id"`
}
type RoutePreview struct {
	RobotID        string  `json:"robot_id"`
	JobID          string  `json:"job_id"`
	Reachable      bool    `json:"reachable"`
	Path           []Vec   `json:"path"`
	Length         float64 `json:"length_m"`
	BaselinePath   []Vec   `json:"baseline_path"`
	BaselineLength float64 `json:"baseline_length_m"`
	Reason         string  `json:"reason,omitempty"`
}

func sameExclusions(a, b []Exclusion) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
func (c *Coordinator) exclusionGrid(regions []Exclusion, obs map[string]Observation) (map[Cell]bool, error) {
	if len(regions) > 16 {
		return nil, errors.New("at most 16 route exclusions are supported")
	}
	blocked := make(map[Cell]bool, len(c.scene.Grid.Blocked))
	for _, cell := range c.scene.Grid.Blocked {
		blocked[cell] = true
	}
	g := c.scene.Grid
	ids := map[string]bool{}
	for _, region := range regions {
		if region.ID == "" || len(region.ID) > 64 || ids[region.ID] || !validVec(region.Min) || !validVec(region.Max) ||
			region.Min[0] >= region.Max[0] || region.Min[1] >= region.Max[1] ||
			region.Min[0] < g.Origin[0] || region.Min[1] < g.Origin[1] ||
			region.Max[0] > g.Origin[0]+float64(g.Width)*g.Resolution || region.Max[1] > g.Origin[1]+float64(g.Height)*g.Resolution {
			return nil, errors.New("exclusions require unique IDs and finite nonempty rectangles inside the known grid")
		}
		ids[region.ID] = true
		// Mark every closed cell touched by the padded rectangle, including
		// exact boundary contacts. The planner's existing full-segment check
		// then protects connectors, diagonals and smoothed route segments.
		for y := 0; y < g.Height; y++ {
			lowY := g.Origin[1] + float64(y)*g.Resolution
			if lowY > region.Max[1]+ExclusionClearance || lowY+g.Resolution < region.Min[1]-ExclusionClearance {
				continue
			}
			for x := 0; x < g.Width; x++ {
				lowX := g.Origin[0] + float64(x)*g.Resolution
				if lowX <= region.Max[0]+ExclusionClearance && lowX+g.Resolution >= region.Min[0]-ExclusionClearance {
					blocked[Cell{x, y}] = true
				}
			}
		}
	}
	// An operator can make a destination unreachable, but cannot place a new
	// exclusion over a robot and invent an authorized escape through it.
	for _, robot := range obs {
		if blocked[g.cell(robot.Position)] && !c.baseBlocked(g.cell(robot.Position)) {
			return nil, errors.New("route exclusion overlaps a robot's current clearance; move the rectangle before starting")
		}
	}
	return blocked, nil
}
func (c *Coordinator) baseBlocked(cell Cell) bool {
	for _, b := range c.scene.Grid.Blocked {
		if b == cell {
			return true
		}
	}
	return false
}
func previewRouteLength(path []Vec) float64 {
	sum := 0.0
	for i := 1; i < len(path); i++ {
		sum += distance(path[i-1], path[i])
	}
	return sum
}
func (c *Coordinator) preview(request *PreviewRequest) *RoutePreview {
	if request == nil {
		return nil
	}
	start := c.workers[request.RobotID].position
	var goal Vec
	for _, job := range c.scene.Jobs {
		if job.ID == request.JobID {
			goal = job.Position
			break
		}
	}
	reservations := c.reservations(request.RobotID)
	path := c.plan(start, goal, reservations)
	baseline, _ := PlanRoute(c.scene.Grid, start, goal, reservations)
	p := &RoutePreview{RobotID: request.RobotID, JobID: request.JobID, Reachable: len(path) > 0, Path: path, Length: previewRouteLength(path), BaselinePath: baseline, BaselineLength: previewRouteLength(baseline)}
	if !p.Reachable {
		p.Reason = "No route with these exclusions, known obstacles and current peer reservations"
	}
	return p
}
