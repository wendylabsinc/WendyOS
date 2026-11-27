package twinrobot

import (
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/twinrobot/sitenav"
)

func validForwardDistance(distance float64) bool {
	return !math.IsNaN(distance) && !math.IsInf(distance, 0) && distance >= .1 && distance <= 2
}

// forwardLocked starts a finite straight movement in live odometry. It shares
// discovery's measured progress, drift, stopping horizon and command watchdog;
// it neither needs a room alignment nor permits unobserved floor along the path.
func (r *SiteRobot) forwardLocked(distance float64) error {
	if !validForwardDistance(distance) {
		return errors.New("Forward distance must be 0.1 to 2 meters.")
	}
	now := time.Now()
	if r.closed || r.ctx.Err() != nil || now.Sub(r.heartbeat) > 600*time.Millisecond {
		return errors.New("The robot controller is no longer active.")
	}
	if r.moving || r.stopUnconfirmed {
		return errors.New("Stop Go2 before starting a forward movement.")
	}
	if err := r.observed.Ready(now); err != nil {
		return fmt.Errorf("%w: %v", ErrObservationUnavailable, err)
	}
	p := r.observed.Pose
	target := sitenav.Point{X: p.Position.X + math.Cos(p.Yaw)*distance, Y: p.Position.Y + math.Sin(p.Yaw)*distance}
	var departure *sitenav.Departure
	if r.underBodyAllowance {
		departure = &sitenav.Departure{Position: p.Position, Yaw: p.Yaw}
	}
	if err := discoveryClearanceErrorAtSpeed(r.observed.Grid.WithDeparture(departure), p.Position, target, p.Yaw, sitenav.WalkingSpeed, p.Yaw); err != nil {
		return err
	}
	r.clearMotionFailureLocked()
	r.discoveryMove = &discoveryMove{origin: p.Position, target: target, yaw: p.Yaw, progressAt: now, epoch: r.poseEpoch, relative: true}
	r.discoveryState = 1
	r.departure = departure
	r.route, r.preview, r.localizations = nil, nil, nil
	r.started, r.moving = now, true
	r.message = fmt.Sprintf("Moving forward %.2f meters", distance)
	return nil
}
