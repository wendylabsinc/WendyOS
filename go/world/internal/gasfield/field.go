// Package gasfield estimates a source region under an explicitly synthetic
// concentration kernel. It does not model indoor fluid dynamics or certify safety.
package gasfield

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"sort"
	"sync"

	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

const Model = "synthetic-directional-gas-kernel-v1"

var identifier = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_./:-]{0,127}$`)

type Sensor struct {
	PositionUncertainty float64      `json:"position_uncertainty_m"`
	ID                  string       `json:"id"`
	Source              world.Source `json:"source"`
	Position            [2]float64   `json:"position_xz"`
	Calibration         string       `json:"calibration"`
	SensorType          string       `json:"sensor_type"`
}
type Config struct {
	Site       string                  `json:"site"`
	Frame      world.FrameRef          `json:"frame"`
	Simulation world.SimulationContext `json:"simulation"`
	Sensors    []Sensor                `json:"sensors"`
	Min        [2]float64              `json:"min_xz"`
	Max        [2]float64              `json:"max_xz"`
	Step       float64                 `json:"step_m"`
	Background float64                 `json:"background_ppm"`
}

// Assumptions are declared model inputs, never measured airflow. Changing them
// reevaluates original readings without altering their values or lifetimes.
type Assumptions struct {
	Revision    string  `json:"revision"`
	WindRadians float64 `json:"wind_radians"`
	Downwind    float64 `json:"downwind_m"`
	Upwind      float64 `json:"upwind_m"`
	Crosswind   float64 `json:"crosswind_m"`
}
type Observation struct {
	ID          string           `json:"id"`
	Sensor      string           `json:"sensor"`
	Source      world.Source     `json:"source"`
	Version     world.Version    `json:"version"`
	Site        string           `json:"site"`
	Frame       world.FrameRef   `json:"frame"`
	Provenance  world.Provenance `json:"provenance"`
	Calibration string           `json:"calibration"`
	SensorType  string           `json:"sensor_type"`
	Units       string           `json:"units"`
	Species     string           `json:"species"`
	Captured    float64          `json:"captured_s"`
	ValidUntil  float64          `json:"valid_until_s"`
	Value       float64          `json:"value_ppm"`
	Uncertainty float64          `json:"uncertainty_ppm"`
}
type Request struct {
	Config       *Config       `json:"config,omitempty"`
	Now          float64       `json:"now_s"`
	Assumptions  Assumptions   `json:"assumptions"`
	Observations []Observation `json:"observations"`
}
type Reading struct {
	Sensor      Sensor       `json:"sensor"`
	State       string       `json:"state"`
	Observation *Observation `json:"observation,omitempty"`
	Predicted   *float64     `json:"predicted_ppm,omitempty"`
	Residual    *float64     `json:"residual_ppm,omitempty"`
}
type Cell struct {
	Position [2]float64 `json:"position_xz"`
	Score    float64    `json:"normalized_rms"`
}
type Estimate struct {
	Position   [2]float64 `json:"position_xz"`
	Strength   float64    `json:"strength_ppm"`
	Score      float64    `json:"normalized_rms"`
	Radius     float64    `json:"region_radius_m"`
	CellSize   float64    `json:"cell_size_m"`
	Cells      []Cell     `json:"cells"`
	ValidUntil float64    `json:"valid_until_s"`
}
type Result struct {
	Schema      string           `json:"schema"`
	Model       string           `json:"model"`
	State       string           `json:"state"`
	Reason      string           `json:"reason"`
	Now         float64          `json:"now_s"`
	Assumptions Assumptions      `json:"assumptions"`
	Coverage    int              `json:"coverage"`
	Total       int              `json:"total"`
	Readings    []Reading        `json:"readings"`
	Estimate    *Estimate        `json:"estimate,omitempty"`
	FitResidual *float64         `json:"fit_normalized_rms,omitempty"`
	Provenance  world.Provenance `json:"provenance"`
}
type retained struct {
	observation Observation
	digest      [32]byte
}
type Session struct {
	mu     sync.Mutex
	cfg    *Config
	latest map[string]retained
	now    float64
}

func finite(x float64) bool             { return !math.IsNaN(x) && !math.IsInf(x, 0) }
func bounded(x, low, high float64) bool { return finite(x) && x >= low && x <= high }
func validSource(s world.Source) bool {
	return identifier.MatchString(s.Device) && identifier.MatchString(s.App) && identifier.MatchString(s.Service) && identifier.MatchString(s.Stream)
}
func copyJSON[T any](value T) T {
	raw, _ := json.Marshal(value)
	var out T
	_ = json.Unmarshal(raw, &out)
	return out
}
func validateConfig(c Config) error {
	if !identifier.MatchString(c.Site) || (c.Frame.Validate() != nil || len(c.Frame.ID) > 128 || len(c.Frame.Revision) > 128) || !identifier.MatchString(c.Simulation.RunID) || !identifier.MatchString(c.Simulation.ScenarioVersion) || c.Simulation.ClockDomain != "simulation:"+c.Simulation.RunID || len(c.Sensors) < 4 || len(c.Sensors) > 16 || !bounded(c.Step, .05, 1) || !bounded(c.Background, 0, 1000) {
		return errors.New("invalid simulation configuration")
	}
	cells := 1
	for i := 0; i < 2; i++ {
		if !bounded(c.Min[i], -100, 100) || !bounded(c.Max[i], -100, 100) || c.Max[i] <= c.Min[i] {
			return errors.New("invalid search bounds")
		}
		cells *= int(math.Floor((c.Max[i]-c.Min[i])/c.Step)) + 1
	}
	if cells > 4096 {
		return errors.New("search exceeds 4096 cells")
	}
	ids := map[string]bool{}
	sources := map[world.Source]bool{}
	for _, s := range c.Sensors {
		if !identifier.MatchString(s.ID) || ids[s.ID] || sources[s.Source] || !validSource(s.Source) || !identifier.MatchString(s.Calibration) || !identifier.MatchString(s.SensorType) || s.PositionUncertainty != 0 || !bounded(s.Position[0], -100, 100) || !bounded(s.Position[1], -100, 100) {
			return errors.New("invalid sensor catalog")
		}
		ids[s.ID] = true
		sources[s.Source] = true
	}
	return nil
}
func validateAssumptions(a Assumptions) error {
	if !identifier.MatchString(a.Revision) || !bounded(a.WindRadians, -math.Pi, math.Pi) || !bounded(a.Downwind, .2, 10) || !bounded(a.Upwind, .2, 10) || !bounded(a.Crosswind, .2, 10) {
		return errors.New("invalid declared airflow/kernel assumptions")
	}
	return nil
}

// Kernel is a designed asymmetric Gaussian fixture, not a physical dispersion
// equation. Concentration = background + strength*Kernel. Lengths are explicit.
func Kernel(sensor, source [2]float64, a Assumptions) float64 {
	dx, dz := sensor[0]-source[0], sensor[1]-source[1]
	c, s := math.Cos(a.WindRadians), math.Sin(a.WindRadians)
	along, cross := dx*c+dz*s, -dx*s+dz*c
	length := a.Downwind
	if along < 0 {
		length = a.Upwind
	}
	return math.Exp(-.5 * (along*along/(length*length) + cross*cross/(a.Crosswind*a.Crosswind)))
}
func validateObservation(o Observation, c Config, now float64) error {
	var sensor *Sensor
	for i := range c.Sensors {
		if c.Sensors[i].ID == o.Sensor {
			sensor = &c.Sensors[i]
			break
		}
	}
	if sensor == nil || !identifier.MatchString(o.ID) || o.Source != sensor.Source || o.Site != c.Site || o.Frame != c.Frame || o.Calibration != sensor.Calibration || o.SensorType != sensor.SensorType || o.Version.Epoch == 0 || o.Version.Sequence == 0 || o.Units != "ppm" || o.Species != "synthetic-tracer" {
		return errors.New("observation identity, frame or calibration mismatch")
	}
	p := o.Provenance
	if p.Origin != world.Simulated || p.Simulation == nil || *p.Simulation != c.Simulation || p.OriginalCaptureTime != nil || p.OriginalClockDomain != "" || len(p.Inputs) != 0 || p.ModelID != "" || p.ModelVersion != "" || !identifier.MatchString(p.AppVersion) {
		return errors.New("original simulated measurement required")
	}
	if !bounded(o.Captured, 0, 1e9) || o.Captured > now || !finite(o.ValidUntil) || o.ValidUntil <= o.Captured || o.ValidUntil-o.Captured > 5 || !bounded(o.Value, 0, 10000) || !bounded(o.Uncertainty, .1, 100) {
		return errors.New("invalid concentration or lifetime")
	}
	return nil
}
func newer(a, b world.Version) bool {
	return a.Epoch > b.Epoch || a.Epoch == b.Epoch && a.Sequence > b.Sequence
}

// Step validates the complete batch before committing state. At most one
// observation/high-water record is retained per configured sensor, even expired.
func (s *Session) Step(req Request) (Result, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !bounded(req.Now, 0, 1e9) || req.Now < s.now || len(req.Observations) > 32 {
		return Result{}, errors.New("invalid clock or batch bound")
	}
	if err := validateAssumptions(req.Assumptions); err != nil {
		return Result{}, err
	}
	cfg := s.cfg
	if cfg == nil {
		if req.Config == nil {
			return Result{}, errors.New("first request requires config")
		}
		if err := validateConfig(*req.Config); err != nil {
			return Result{}, err
		}
		value := copyJSON(*req.Config)
		sort.Slice(value.Sensors, func(i, j int) bool { return value.Sensors[i].ID < value.Sensors[j].ID })
		cfg = &value
	} else if req.Config != nil {
		incoming := copyJSON(*req.Config)
		sort.Slice(incoming.Sensors, func(i, j int) bool { return incoming.Sensors[i].ID < incoming.Sensors[j].ID })
		a, _ := json.Marshal(incoming)
		b, _ := json.Marshal(cfg)
		if string(a) != string(b) {
			return Result{}, errors.New("configuration is immutable; start a new session")
		}
	}
	next := map[string]retained{}
	for k, v := range s.latest {
		next[k] = v
	}
	for _, input := range req.Observations {
		if err := validateObservation(input, *cfg, req.Now); err != nil {
			return Result{}, err
		}
		o := copyJSON(input)
		raw, _ := json.Marshal(o)
		digest := sha256.Sum256(raw)
		previous, exists := next[o.Sensor]
		if exists {
			if o.Version == previous.observation.Version {
				if digest != previous.digest {
					return Result{}, errors.New("conflicting bytes under one source version")
				}
				continue
			}
			if !newer(o.Version, previous.observation.Version) {
				continue
			}
			if o.Captured <= previous.observation.Captured {
				return Result{}, errors.New("new source version must advance capture time")
			}
		}
		next[o.Sensor] = retained{o, digest}
	}
	s.cfg = cfg
	s.latest = next
	s.now = req.Now
	return evaluate(*cfg, next, req.Now, req.Assumptions), nil
}
func evaluate(c Config, latest map[string]retained, now float64, a Assumptions) Result {
	r := Result{Schema: "wendy.gas-source.v1", Model: Model, State: "unavailable", Now: now, Assumptions: a, Total: len(c.Sensors), Readings: []Reading{}, Provenance: world.Provenance{Origin: world.Inferred, AppVersion: "gas-source-v1", ModelID: Model, ModelVersion: "1", Inputs: []world.Input{}}}
	newest := -1.
	for _, v := range latest {
		if v.observation.ValidUntil > now && v.observation.Captured > newest {
			newest = v.observation.Captured
		}
	}
	indices := []int{}
	signal := 0.
	until := math.Inf(1)
	for _, sensor := range c.Sensors {
		reading := Reading{Sensor: sensor, State: "missing"}
		if v, ok := latest[sensor.ID]; ok {
			o := copyJSON(v.observation)
			reading.Observation = &o
			reading.State = "live"
			if o.ValidUntil <= now {
				reading.State = "expired"
			} else if newest-o.Captured > .35 {
				reading.State = "unaligned"
			} else {
				indices = append(indices, len(r.Readings))
				if o.ValidUntil < until {
					until = o.ValidUntil
				}
				signal += math.Pow((o.Value-c.Background)/o.Uncertainty, 2)
				// Source and source version are included so equal caller sample IDs cannot alias.
				raw, _ := json.Marshal(o.Source)
				sourceHash := sha256.Sum256(raw)
				r.Provenance.Inputs = append(r.Provenance.Inputs, world.Input{ID: fmt.Sprintf("%x/%d/%d/%s", sourceHash, o.Version.Epoch, o.Version.Sequence, o.ID), Origin: world.Simulated, Simulation: copyJSON(o.Provenance.Simulation)})
			}
		}
		r.Readings = append(r.Readings, reading)
	}
	r.Coverage = len(indices)
	if len(indices) < 4 {
		r.Reason = "At least four fresh, capture-aligned sensors are required"
		return r
	}
	if math.Sqrt(signal/float64(len(indices))) < 3 {
		r.Reason = "Concentrations do not distinguish a source from the declared background"
		return r
	}
	// Coverage geometry detects a line of sensors even if grid discretization
	// happens to pick one side. It is a policy gate, not an observability theorem.
	var mx, mz, xx, zz, xz float64
	for _, i := range indices {
		mx += r.Readings[i].Sensor.Position[0]
		mz += r.Readings[i].Sensor.Position[1]
	}
	mx /= float64(len(indices))
	mz /= float64(len(indices))
	for _, i := range indices {
		p := r.Readings[i].Sensor.Position
		dx, dz := p[0]-mx, p[1]-mz
		xx += dx * dx
		zz += dz * dz
		xz += dx * dz
	}
	poorGeometry := (xx*zz - xz*xz) <= .02*(xx+zz)*(xx+zz)
	type fit struct {
		cell     Cell
		strength float64
	}
	fits := []fit{}
	best := fit{cell: Cell{Score: math.Inf(1)}}
	nx := int(math.Floor((c.Max[0] - c.Min[0]) / c.Step))
	nz := int(math.Floor((c.Max[1] - c.Min[1]) / c.Step))
	for iz := 0; iz <= nz; iz++ {
		for ix := 0; ix <= nx; ix++ {
			p := [2]float64{c.Min[0] + float64(ix)*c.Step, c.Min[1] + float64(iz)*c.Step}
			num, den := 0., 0.
			for _, i := range indices {
				rd := r.Readings[i]
				o := rd.Observation
				k := Kernel(rd.Sensor.Position, p, a)
				w := 1 / (o.Uncertainty * o.Uncertainty)
				num += w * k * (o.Value - c.Background)
				den += w * k * k
			}
			if den < 1e-12 {
				continue
			}
			strength := math.Max(0, math.Min(10000, num/den))
			sse := 0.
			for _, i := range indices {
				rd := r.Readings[i]
				o := rd.Observation
				res := (c.Background + strength*Kernel(rd.Sensor.Position, p, a) - o.Value) / o.Uncertainty
				sse += res * res
			}
			f := fit{Cell{p, math.Sqrt(sse / float64(len(indices)))}, strength}
			fits = append(fits, f)
			if f.cell.Score < best.cell.Score {
				best = f
			}
		}
	}
	if len(fits) > 0 {
		score := best.cell.Score
		r.FitResidual = &score
		for _, i := range indices {
			rd := &r.Readings[i]
			pred := c.Background + best.strength*Kernel(rd.Sensor.Position, best.cell.Position, a)
			res := rd.Observation.Value - pred
			rd.Predicted = &pred
			rd.Residual = &res
		}

	}
	if len(fits) == 0 || best.cell.Score > 3 {
		r.Reason = "No source fits the declared airflow model within the residual limit"
		if len(fits) > 0 {
			r.Reason = fmt.Sprintf("Model mismatch: best normalized residual %.2f exceeds 3", best.cell.Score)
		}
		return r
	}
	e := &Estimate{Position: best.cell.Position, Strength: best.strength, Score: best.cell.Score, Cells: []Cell{}, CellSize: c.Step, ValidUntil: until}
	for _, f := range fits {
		if f.cell.Score <= math.Min(3, best.cell.Score+.75) {
			e.Cells = append(e.Cells, f.cell)
			distance := math.Hypot(f.cell.Position[0]-e.Position[0], f.cell.Position[1]-e.Position[1])
			e.Radius = math.Max(e.Radius, distance)
		}
	}
	// Region cells have area; even a single accepted grid cell is not an exact point.
	e.Radius += c.Step / math.Sqrt2
	r.Estimate = e
	r.State = "localized"
	r.Reason = "Compact source region under the declared synthetic kernel; not a confidence interval"
	boundary := false
	lastX, lastZ := c.Min[0]+float64(nx)*c.Step, c.Min[1]+float64(nz)*c.Step
	for _, cell := range e.Cells {
		if cell.Position[0] <= c.Min[0]+c.Step/2 || cell.Position[0] >= lastX-c.Step/2 || cell.Position[1] <= c.Min[1]+c.Step/2 || cell.Position[1] >= lastZ-c.Step/2 {
			boundary = true
		}
	}
	if poorGeometry || e.Radius > .75 || boundary {
		r.State = "ambiguous"
		r.Reason = "Source region is broad, touches the search boundary, or sensor geometry is inadequate"
	}
	return r
}
