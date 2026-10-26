package backend

import (
	"errors"
	"math"
	"time"

	"github.com/wendylabsinc/wendy/go/macos/backend/sitenav"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
)

// A look turn is explicit, bounded and independent of the unconfirmed site
// transform. Clearance is evaluated in the live odometry frame throughout.
type siteLookTurn struct {
	origin                         sitenav.Point
	startYaw, targetYaw, direction float64
	progressYaw                    float64
	progressAt                     time.Time
}

const lookAngle = math.Pi / 6

// Match the existing manual turn control. Do not taper into low rates that
// the physical robot may ignore; stop instead when the bounded angle is reached.
const lookSpeed = .5

// Cover the 500ms MOVE lease, braking at 1 rad/s² and 0.05rad tolerance.
const lookStopMargin = lookSpeed*.5 + lookSpeed*lookSpeed/2 + .05

func (r *SiteRobot) Look(sequence uint64, direction int) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if sequence <= r.commandSequence {
		return errors.New("This robot order was superseded.")
	}
	r.commandSequence = sequence
	if r.moving || r.stopUnconfirmed {
		if err := r.stopLocked(); err != nil {
			return err
		}
	}
	if direction != -1 && direction != 0 && direction != 1 {
		return errors.New("Choose left or right for a 30-degree look turn.")
	}
	now := time.Now()
	if r.closed || r.ctx.Err() != nil || now.Sub(r.heartbeat) > 600*time.Millisecond {
		return errors.New("The map is no longer active.")
	}
	if err := r.observed.Ready(now); err != nil {
		return err
	}
	p := r.observed.Pose
	var departure *sitenav.Departure
	if r.underBodyAllowance {
		departure = &sitenav.Departure{Position: p.Position, Yaw: p.Yaw}
	}
	if direction == 0 {
		direction = 1
		if !r.navigationGridLocked(departure).Turn(p.Position, p.Yaw, p.Yaw+lookAngle+lookStopMargin) {
			direction = -1
		}
	}
	target := p.Yaw + float64(direction)*lookAngle
	if !r.navigationGridLocked(departure).Turn(p.Position, p.Yaw, target+float64(direction)*lookStopMargin) {
		return errors.New("Go2 needs observed clear floor around its turning footprint, including 10 cm clearance. An obstacle, drop or unseen floor blocks this turn.")
	}
	r.lookState = 1
	r.lookDirection = direction
	r.lookTurn = &siteLookTurn{origin: p.Position, startYaw: p.Yaw, targetYaw: target, direction: float64(direction), progressYaw: p.Yaw, progressAt: now}
	r.departure = departure
	r.route = nil
	r.preview = nil
	r.localizations = nil
	r.started = now
	r.moving = true
	r.message = "Looking around · turning 30°"
	return nil
}

func (r *SiteRobot) stepLookLocked(now time.Time) {
	turn := r.lookTurn
	p := r.observed.Pose
	progress := math.Remainder(p.Yaw-turn.startYaw, 2*math.Pi) * turn.direction
	if p.Position.Distance(turn.origin) > .08 || progress < -.08 || progress > lookAngle+lookStopMargin {
		r.message = "Look turn stopped: unexpected drift or rotation."
		r.stopLocked()
		return
	}
	remaining := (turn.targetYaw - p.Yaw) * turn.direction
	// Use wrapped angles at the odometry +/-pi boundary.
	remaining = math.Remainder(remaining, 2*math.Pi)
	if remaining <= .025 {
		r.lookState = 2
		r.message = "Look turn finished. Matching the new view."
		r.stopLocked()
		return
	}
	if now.Sub(r.started) > 8*time.Second || now.Sub(turn.progressAt) > 2*time.Second {
		r.message = "Look turn stopped: no timely rotation progress."
		r.stopLocked()
		return
	}
	if math.Abs(math.Remainder(p.Yaw-turn.progressYaw, 2*math.Pi)) >= .025 {
		turn.progressYaw = p.Yaw
		turn.progressAt = now
	}
	if !r.navigationGridLocked(r.departure).Turn(p.Position, p.Yaw, p.Yaw+turn.direction*(remaining+lookStopMargin)) {
		r.message = "Look turn stopped: obstacle, drop or unseen floor in the turning footprint."
		r.stopLocked()
		return
	}
	if err := r.command(pb.UnitreeCommand_UNITREE_COMMAND_MOVE, 0, turn.direction*lookSpeed); err != nil {
		r.message = "Look turn command failed."
		r.stopLocked()
	}
}
