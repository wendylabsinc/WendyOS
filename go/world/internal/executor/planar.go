package executor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/collabdemo"
	"github.com/wendylabsinc/wendy/go/world/internal/mission"
	"github.com/wendylabsinc/wendy/go/world/internal/tasking"
)

const PlanarStateSchema = "wendy.sim.actuator.planar.v1"

// PlanarConfig names one simulation actuator and its existing office grid.
// Grid and Reservations use MuJoCo XY, while tasks and HTTP use World XYZ.
type PlanarConfig struct {
	RobotID      string             `json:"robot_id"`
	FloorY       float64            `json:"floor_y"`
	Grid         collabdemo.Grid    `json:"grid"`
	Reservations [][]collabdemo.Vec `json:"reservations,omitempty"`
}

type PlanarNavigation struct {
	config       PlanarConfig
	configSHA256 string
}

func NewPlanarNavigation(config PlanarConfig) (*PlanarNavigation, error) {
	g := config.Grid
	if config.RobotID == "" || len(config.RobotID) > 128 || !finitePlanar(config.FloorY) || !finitePlanar(g.Origin[0]) || !finitePlanar(g.Origin[1]) || !finitePlanar(g.Resolution) || g.Resolution <= 0 || g.Resolution > 2 || g.Width < 1 || g.Height < 1 || g.Width > 512 || g.Height > 512 || g.Width*g.Height > 100000 || len(g.Blocked) > g.Width*g.Height || len(config.Reservations) > 16 {
		return nil, errors.New("invalid planar simulation configuration")
	}
	for _, cell := range g.Blocked {
		if cell[0] < 0 || cell[1] < 0 || cell[0] >= g.Width || cell[1] >= g.Height {
			return nil, errors.New("planar obstacle outside grid")
		}
	}
	config.Grid.Blocked = append([]collabdemo.Cell(nil), g.Blocked...)
	reservations := make([][]collabdemo.Vec, len(config.Reservations))
	for i, path := range config.Reservations {
		if len(path) > 4096 {
			return nil, errors.New("planar reservation exceeds bound")
		}
		for _, point := range path {
			if !finitePlanar(point[0]) || !finitePlanar(point[1]) {
				return nil, errors.New("invalid planar reservation")
			}
		}
		reservations[i] = append([]collabdemo.Vec(nil), path...)
	}
	config.Reservations = reservations
	raw, err := json.Marshal(config)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(raw)
	return &PlanarNavigation{config: config, configSHA256: hex.EncodeToString(sum[:])}, nil
}

// PlanarConfigSHA256 hashes the same validated, owned canonical configuration
// used by the executor, including the existing grid and route reservations.
func PlanarConfigSHA256(config PlanarConfig) (string, error) {
	navigation, err := NewPlanarNavigation(config)
	if err != nil {
		return "", err
	}
	return navigation.configSHA256, nil
}

func finitePlanar(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }

func validatePlanarState(s State, robot string, floor float64) error {
	if err := validateStateBase(s); err != nil {
		return err
	}
	if s.SchemaVersion != PlanarStateSchema || s.RobotID != robot || math.Abs(s.Position[1]-floor) > 1e-6 || math.Abs(s.Velocity[1]) > 1e-6 || math.Hypot(s.Velocity[0], s.Velocity[2]) > .501 || math.Abs(s.PlanarCommandedVelocity[1]) > 1e-6 {
		return errors.New("state does not match configured planar robot")
	}
	for _, v := range s.PlanarCommandedVelocity {
		if !finitePlanar(v) {
			return errors.New("invalid planar commanded velocity")
		}
	}
	if math.Hypot(s.PlanarCommandedVelocity[0], s.PlanarCommandedVelocity[2]) > .5 {
		return errors.New("planar command exceeds speed")
	}
	return nil
}

func (c *Controller) validateState(s State) error {
	if c.Planar != nil {
		return validatePlanarState(s, c.Planar.config.RobotID, c.Planar.config.FloorY)
	}
	return validateState(s)
}

func (p *PlanarNavigation) validateTask(s tasking.Spec) error {
	if s.ControlContext == nil || s.Type != tasking.InspectType || s.Domain != mission.Simulation || s.Mode != mission.Autonomous || math.Abs(s.Target[1]-p.config.FloorY) > 1e-6 || !finitePlanar(s.SpeedMPS) || s.SpeedMPS <= 0 || s.SpeedMPS > .5 {
		return errors.New("planar driver requires commissioned autonomous inspection")
	}
	if d := s.InspectionDestination; d != nil {
		if d.Validate() != nil || d.FloorY != p.config.FloorY || d.PlanarConfigSHA256 != p.configSHA256 {
			return errors.New("inspection destination differs from planar configuration")
		}
	}
	for i, v := range s.Target {
		if !finitePlanar(v) || v < s.Area.Min[i] || v > s.Area.Max[i] {
			return errors.New("planar target outside task area")
		}
	}
	return nil
}

type PlanarAdapter interface {
	CommandPlanar(context.Context, string, uint64, [3]float64, time.Duration) error
}

func (c *Controller) commandPlanar(ctx context.Context, state State, spec tasking.Spec) error {
	adapter, ok := c.Adapter.(PlanarAdapter)
	if !ok {
		return errors.New("planar command adapter unavailable")
	}
	start := collabdemo.Vec{state.Position[0], -state.Position[2]}
	goal := collabdemo.Vec{spec.Target[0], -spec.Target[2]}
	path, err := collabdemo.PlanRoute(c.Planar.config.Grid, start, goal, c.Planar.config.Reservations)
	if err != nil {
		return err
	}
	for _, point := range path {
		if point[0] < spec.Area.Min[0] || point[0] > spec.Area.Max[0] || -point[1] < spec.Area.Min[2] || -point[1] > spec.Area.Max[2] {
			return errors.New("office route leaves signed task area")
		}
	}
	// Replan from the actual current pose. The first segment is checked by the
	// same supercover A* used by the collaborative office, including corners.
	velocity := [3]float64{}
	if len(path) > 1 {
		dx, dz := path[1][0]-start[0], -(path[1][1] - start[1])
		distance := math.Hypot(dx, dz)
		if distance > 0 && math.Hypot(goal[0]-start[0], goal[1]-start[1]) > spec.ToleranceM {
			speed := math.Min(spec.SpeedMPS, 3*distance)
			velocity = [3]float64{dx / distance * speed, 0, dz / distance * speed}
		}
	}
	velocity, brake, err := boundedPlanarVelocity(state, spec, velocity, c.clock())
	if err != nil {
		return err
	}
	if brake {
		return c.Brake(ctx)
	}
	if c.sequence >= maxSequence {
		return errors.New("command sequence exhausted")
	}
	c.sequence++
	return adapter.CommandPlanar(ctx, c.session, c.sequence, velocity, commandLease)
}

// Reuse the scalar driver's signed-area/lease budget independently on both
// horizontal axes. It only reduces the route-following speed.
func boundedPlanarVelocity(s State, spec tasking.Spec, v [3]float64, now time.Time) ([3]float64, bool, error) {
	age := now.UnixMilli() - s.SampleUnixMS
	if age < 0 || age > 250 {
		return v, true, errors.New("stale planar state")
	}
	horizon := (commandLease + time.Second + 20*time.Millisecond).Seconds() + float64(age)/1000
	scale := 1.0
	for _, axis := range []int{0, 2} {
		lo, hi, position := spec.Area.Min[axis], spec.Area.Max[axis], s.Position[axis]
		if !finitePlanar(lo) || !finitePlanar(hi) || lo >= hi || position < lo || position > hi {
			return v, true, errors.New("outside planar task bounds")
		}
		left, right := math.Max(0, position-lo-.001), math.Max(0, hi-position-.001)
		if math.Max(s.Velocity[axis], s.PlanarCommandedVelocity[axis]) > right/horizon || math.Max(-s.Velocity[axis], -s.PlanarCommandedVelocity[axis]) > left/horizon {
			return v, true, nil
		}
		if v[axis] > 0 {
			scale = math.Min(scale, right/(horizon*v[axis]))
		}
		if v[axis] < 0 {
			scale = math.Min(scale, left/(-horizon*v[axis]))
		}
	}
	for i := range v {
		v[i] *= scale
	}
	return v, false, nil
}
