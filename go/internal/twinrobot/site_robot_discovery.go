package twinrobot

import (
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/twinrobot/sitenav"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
)

const discoveryDistance = .4 // preferred viewpoint distance, not a required motion
const discoverySpeed = sitenav.WalkingSpeed
const discoveryMargin = .10 // 6 cm lateral drift plus the footprint swept by 0.1 rad yaw drift

// ErrDiscoveryBlocked identifies preflight clearance loss for a viewpoint or
// look turn. A search can wait for new measurements and recheck the full path.
var ErrDiscoveryBlocked = errors.New("no observed clear floor for a new viewpoint")

type discoveryMove struct {
	origin, target                        sitenav.Point
	yaw, progress                         float64
	progressAt                            time.Time
	epoch                                 uint64
	relative                              bool
	turning                               bool
	startYaw, turnDirection, turnProgress float64
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
	return r.discoveryStepLocked(root)
}

func (r *SiteRobot) discoveryStepLocked(root uint64) error {
	if r.moving || r.stopUnconfirmed {
		return errors.New("Stop Go2 before gathering another viewpoint.")
	}
	now := time.Now()
	if r.closed || r.ctx.Err() != nil || now.Sub(r.heartbeat) > 600*time.Millisecond {
		return errors.New("The map is no longer active.")
	}
	if err := r.observed.Ready(now); err != nil {
		return fmt.Errorf("%w: %v", ErrObservationUnavailable, err)
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
	var departure *sitenav.Departure
	if r.underBodyAllowance {
		departure = &sitenav.Departure{Position: p.Position, Yaw: p.Yaw}
	}
	move, err := planDiscoveryView(r.observed.Grid.WithDeparture(departure), p, *history)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrDiscoveryBlocked, err)
	}
	r.clearMotionFailureLocked()
	history.starts++
	move.progressAt = now
	move.epoch = r.poseEpoch
	r.discoveryMove = move
	r.discoveryState = 1
	r.departure = departure
	r.route, r.preview, r.localizations = nil, nil, nil
	r.started = now
	r.moving = true
	r.message = "Discovery: moving to a nearby viewpoint"
	return nil
}

// Sample bounded turn-and-advance trajectories in the live odometry grid. A
// viewpoint needs separation from earlier samples, not an exact forward target.
// Use the same swept-body geometry as waypoint navigation, without requiring a
// site alignment or permitting the general unobserved-floor navigation policy.
func planDiscoveryView(grid sitenav.Grid, pose sitenav.Pose, history discoveryHistory) (*discoveryMove, error) {
	var lastErr error
	for _, angle := range []float64{0, math.Pi / 6, -math.Pi / 6, math.Pi / 3, -math.Pi / 3, math.Pi / 2, -math.Pi / 2} {
		yaw := pose.Yaw + angle
		if angle != 0 && !grid.TurnWithMargin(pose.Position, pose.Yaw, yaw+math.Copysign(lookStopMargin, angle), discoveryMargin) {
			continue
		}
		for _, distance := range []float64{discoveryDistance, .6, .3} {
			target := sitenav.Point{X: pose.Position.X + math.Cos(yaw)*distance, Y: pose.Position.Y + math.Sin(yaw)*distance}
			if target.Distance(history.visited[0]) > 1.3 {
				lastErr = errors.New("the next viewpoint would exceed the 1.3 m discovery radius")
				continue
			}
			visited := false
			for _, previous := range history.visited {
				visited = visited || target.Distance(previous) < .25
			}
			if visited {
				lastErr = errors.New("the next viewpoint is too close to a previously visited position")
				continue
			}
			if err := discoveryClearanceError(grid, pose.Position, target, yaw); err != nil {
				lastErr = err
				continue
			}
			return &discoveryMove{origin: pose.Position, target: target, yaw: yaw, turning: angle != 0,
				startYaw: pose.Yaw, turnDirection: math.Copysign(1, angle), turnProgress: pose.Yaw}, nil
		}
	}
	if lastErr == nil {
		lastErr = errors.New("no nearby viewpoint has room for Go2's footprint and turn")
	}
	return nil, lastErr
}

func discoveryClearanceError(grid sitenav.Grid, start, target sitenav.Point, yaw float64) error {
	// Check the target and the stopping horizon from the current pose. Repeat
	// before every command, as waypoint navigation does; do not add another
	// full stopping distance beyond a viewpoint we may never need to reach.
	horizon := discoverySpeed*.5 + discoverySpeed*discoverySpeed + .05
	distance := math.Max(start.Distance(target), horizon)
	end := sitenav.Point{X: start.X + math.Cos(yaw)*distance, Y: start.Y + math.Sin(yaw)*distance}
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
	operation := "Discovery"
	if move.relative {
		operation = "Forward movement"
	}
	if now.Sub(r.started) > 12*time.Second || now.Sub(move.progressAt) > 2*time.Second || move.epoch != r.poseEpoch {
		var faults []string
		if now.Sub(r.started) > 12*time.Second {
			faults = append(faults, fmt.Sprintf("movement elapsed %.2f s exceeds 12.00 s", now.Sub(r.started).Seconds()))
		}
		if now.Sub(move.progressAt) > 2*time.Second {
			faults = append(faults, fmt.Sprintf("no progress for %.2f s exceeds 2.00 s", now.Sub(move.progressAt).Seconds()))
		}
		if move.epoch != r.poseEpoch {
			faults = append(faults, fmt.Sprintf("odometry epoch changed from %d to %d", move.epoch, r.poseEpoch))
		}
		r.message = operation + " stopped: " + strings.Join(faults, "; ") + "."
		r.stopLocked()
		return
	}
	if move.turning {
		r.stepDiscoveryTurnLocked(now)
		return
	}
	distance := move.origin.Distance(move.target)
	dx, dy := p.Position.X-move.origin.X, p.Position.Y-move.origin.Y
	progress := dx*math.Cos(move.yaw) + dy*math.Sin(move.yaw)
	lateral := math.Abs(-dx*math.Sin(move.yaw) + dy*math.Cos(move.yaw))
	headingError := math.Abs(math.Remainder(p.Yaw-move.yaw, 2*math.Pi))
	if lateral > .06 || headingError > .1 || progress < -.05 || progress > distance+.1 {
		// Retain the measured cause before stopLocked discards the move. A
		// later fresh pose cannot reconstruct which guard ended this attempt.
		var faults []string
		if lateral > .06 {
			faults = append(faults, fmt.Sprintf("lateral drift %.3f m exceeds 0.060 m", lateral))
		}
		if headingError > .1 {
			faults = append(faults, fmt.Sprintf("heading drift %.3f rad exceeds 0.100 rad", headingError))
		}
		if progress < -.05 {
			faults = append(faults, fmt.Sprintf("reverse progress %.3f m exceeds 0.050 m", -progress))
		}
		if progress > distance+.1 {
			faults = append(faults, fmt.Sprintf("overshoot %.3f m exceeds 0.100 m", progress-distance))
		}
		r.message = fmt.Sprintf("%s stopped: %s (progress %.3f/%.3f m; odometry epoch %d).", operation, strings.Join(faults, "; "), progress, distance, r.poseEpoch)
		r.stopLocked()
		return
	}
	if progress >= distance-.025 {
		r.discoveryState = 2
		if move.relative {
			r.message = "Forward distance reached."
		} else {
			r.discoveryHistory.visited = append(r.discoveryHistory.visited, p.Position)
			r.message = "New viewpoint reached. Gathering fresh observations."
		}
		if r.stopLocked() != nil {
			r.discoveryState = 3
		}
		return
	}
	if progress-move.progress >= .025 {
		move.progress, move.progressAt = progress, now
	}
	if err := discoveryClearanceError(r.observed.Grid.WithDeparture(r.departure), p.Position, move.target, p.Yaw); err != nil {
		r.message = operation + " stopped: " + err.Error()
		r.stopForObservationLossLocked()
		return
	}
	if err := r.command(pb.UnitreeCommand_UNITREE_COMMAND_MOVE, discoverySpeed, 0); err != nil {
		r.message = siteCommandFailure(operation, err)
		r.stopLocked()
	}
}

// Finish the selected heading before translating. The initial departure
// footprint stays fixed across both phases and never follows the robot.
func (r *SiteRobot) stepDiscoveryTurnLocked(now time.Time) {
	move, pose := r.discoveryMove, r.observed.Pose
	progress := math.Remainder(pose.Yaw-move.startYaw, 2*math.Pi) * move.turnDirection
	angle := math.Abs(math.Remainder(move.yaw-move.startYaw, 2*math.Pi))
	if pose.Position.Distance(move.origin) > .06 || progress < -.08 || progress > angle+lookStopMargin {
		r.message = "Discovery stopped: unexpected drift or rotation."
		r.stopLocked()
		return
	}
	remaining := math.Remainder(move.yaw-pose.Yaw, 2*math.Pi)
	if math.Abs(remaining) <= .04 {
		// Acknowledge STOP before changing from rotation to forward motion.
		if err := r.command(pb.UnitreeCommand_UNITREE_COMMAND_STOP, 0, 0); err != nil {
			r.message = siteCommandFailure("Discovery turn stop", err)
			r.stopLocked()
			return
		}
		move.turning = false
		move.progressAt = now
		return
	}
	if remaining*move.turnDirection < 0 {
		r.message = "Discovery stopped: the viewpoint turn overshot."
		r.stopLocked()
		return
	}
	if math.Abs(math.Remainder(pose.Yaw-move.turnProgress, 2*math.Pi)) >= .025 {
		move.turnProgress, move.progressAt = pose.Yaw, now
	}
	grid := r.observed.Grid.WithDeparture(r.departure)
	if !grid.TurnWithMargin(pose.Position, pose.Yaw, move.yaw+move.turnDirection*lookStopMargin, discoveryMargin) {
		r.message = "Discovery stopped: blocked or unobserved floor in the viewpoint turn."
		r.stopForObservationLossLocked()
		return
	}
	if err := r.command(pb.UnitreeCommand_UNITREE_COMMAND_MOVE, 0, move.turnDirection*lookSpeed); err != nil {
		r.message = siteCommandFailure("Discovery turn", err)
		r.stopLocked()
	}
}
