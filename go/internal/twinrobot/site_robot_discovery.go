package twinrobot

import (
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/twinrobot/sitenav"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
)

const discoveryDistance = .4
const discoverySpeed = sitenav.WalkingSpeed
const discoveryMargin = .12 // lateral drift plus the footprint swept by 0.1 rad yaw drift

// A blocked viewpoint is optional; a sensor, command or movement fault ends the search.
var ErrDiscoveryBlocked = errors.New("no observed clear floor for a new viewpoint")

type discoveryMove struct {
	origin, target sitenav.Point
	yaw, progress  float64
	progressAt     time.Time
}

type discoveryHistory struct {
	root, epoch uint64
	starts      int
	visited     []sitenav.Point
}

// DiscoveryStep gathers a translated view before room alignment is known. It
// only uses live odometry and measured floor, never a candidate room transform
// or the supervised policy that permits walking on unobserved floor.
func (r *SiteRobot) DiscoveryStep(sequence, root uint64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if sequence <= r.commandSequence {
		return errors.New("This robot order was superseded.")
	}
	r.commandSequence = sequence
	if r.moving || r.stopUnconfirmed {
		return errors.New("Stop Go2 before gathering another viewpoint.")
	}
	now := time.Now()
	if r.closed || r.ctx.Err() != nil || now.Sub(r.heartbeat) > 600*time.Millisecond {
		return errors.New("The map is no longer active.")
	}
	if err := r.observed.Ready(now); err != nil {
		return err
	}
	p := r.observed.Pose
	if r.discoveryHistory.root != root || len(r.discoveryHistory.visited) == 0 {
		r.discoveryHistory = discoveryHistory{root: root, epoch: r.poseEpoch, visited: []sitenav.Point{p.Position}}
	}
	history := &r.discoveryHistory
	if history.epoch != r.poseEpoch {
		return errors.New("Go2 odometry changed during discovery. Start a new search.")
	}
	if history.starts >= 3 {
		return fmt.Errorf("%w: the search has reached its three-viewpoint movement limit", ErrDiscoveryBlocked)
	}
	target := sitenav.Point{X: p.Position.X + math.Cos(p.Yaw)*discoveryDistance, Y: p.Position.Y + math.Sin(p.Yaw)*discoveryDistance}
	if target.Distance(history.visited[0]) > 1.3 {
		return fmt.Errorf("%w: the next viewpoint would exceed the 1.3 m discovery radius", ErrDiscoveryBlocked)
	}
	for _, visited := range history.visited {
		if target.Distance(visited) < .25 {
			return fmt.Errorf("%w: the next viewpoint is too close to a previously visited position", ErrDiscoveryBlocked)
		}
	}
	var departure *sitenav.Departure
	if r.underBodyAllowance {
		departure = &sitenav.Departure{Position: p.Position, Yaw: p.Yaw}
	}
	if err := discoveryClearanceError(r.observed.Grid.WithDeparture(departure), p.Position, target, p.Yaw); err != nil {
		return fmt.Errorf("%w: %v", ErrDiscoveryBlocked, err)
	}
	history.starts++
	r.discoveryMove = &discoveryMove{origin: p.Position, target: target, yaw: p.Yaw, progressAt: now}
	r.discoveryState = 1
	r.departure = departure
	r.route, r.preview, r.localizations = nil, nil, nil
	r.started = now
	r.moving = true
	r.message = "Discovery: moving to a nearby viewpoint"
	return nil
}

func discoveryClearanceError(grid sitenav.Grid, start, target sitenav.Point, yaw float64) error {
	// Reserve the whole path plus the 500ms MOVE lease, braking at 0.5m/s²
	// and tracking tolerance, even though STOP normally arrives at the target.
	horizon := discoverySpeed*.5 + discoverySpeed*discoverySpeed + .05
	end := sitenav.Point{X: target.X + math.Cos(yaw)*horizon, Y: target.Y + math.Sin(yaw)*horizon}
	if grid.SweepWithMargin(start, end, yaw, discoveryMargin) {
		return nil
	}
	if !start.Valid() || !end.Valid() || math.IsNaN(yaw) || math.IsInf(yaw, 0) {
		return errors.New("The movement or stopping area has an invalid map pose.")
	}
	// Diagnose only after the real safety check rejected the move. This copy
	// cannot authorize movement, and leaves the observed grid untouched. When
	// floor and obstacle evidence both block the sweep, report missing floor;
	// clearing an obstacle alone would still not make that route traversable.
	floorOnly := grid
	floorOnly.Obstacles = nil
	if !floorOnly.SweepWithMargin(start, end, yaw, discoveryMargin) {
		return errors.New("LiDAR has not observed all the floor in the movement and stopping area, including the safety margin.")
	}
	return errors.New("An obstacle or drop overlaps the movement or stopping area, including the safety margin.")
}

func (r *SiteRobot) stepDiscoveryLocked(now time.Time) {
	move := r.discoveryMove
	p := r.observed.Pose
	dx, dy := p.Position.X-move.origin.X, p.Position.Y-move.origin.Y
	progress := dx*math.Cos(move.yaw) + dy*math.Sin(move.yaw)
	lateral := math.Abs(-dx*math.Sin(move.yaw) + dy*math.Cos(move.yaw))
	if lateral > .06 || math.Abs(math.Remainder(p.Yaw-move.yaw, 2*math.Pi)) > .1 ||
		progress < -.05 || progress > discoveryDistance+.1 || r.discoveryHistory.epoch != r.poseEpoch {
		r.message = "Discovery stopped: unexpected drift or odometry change."
		r.stopLocked()
		return
	}
	if progress >= discoveryDistance-.025 {
		r.discoveryState = 2
		r.discoveryHistory.visited = append(r.discoveryHistory.visited, p.Position)
		r.message = "New viewpoint reached. Gathering fresh observations."
		if r.stopLocked() != nil {
			r.discoveryState = 3
		}
		return
	}
	if now.Sub(r.started) > 4*time.Second || now.Sub(move.progressAt) > 2*time.Second {
		r.message = "Discovery stopped: no timely forward progress."
		r.stopLocked()
		return
	}
	if progress-move.progress >= .025 {
		move.progress, move.progressAt = progress, now
	}
	if err := discoveryClearanceError(r.observed.Grid.WithDeparture(r.departure), p.Position, move.target, p.Yaw); err != nil {
		r.message = "Discovery stopped: " + err.Error()
		r.stopLocked()
		return
	}
	if err := r.command(pb.UnitreeCommand_UNITREE_COMMAND_MOVE, discoverySpeed, 0); err != nil {
		r.message = siteCommandFailure("Discovery movement", err)
		r.stopLocked()
	}
}
