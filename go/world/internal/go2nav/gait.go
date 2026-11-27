package go2nav

import "math"

// validLateral preserves the fixed legacy cap. Selecting the gait model does
// not supply a default: its larger range must be explicitly commissioned.
func (g *Grid) validLateral(value float64, l MotionLimits) bool {
	if !finite(value) {
		return false
	}
	bound := .05
	if g != nil && g.motionEnvelope == IsotropicGaitV1 {
		bound = l.MaxMeasuredLateralMPS
		if !finite(bound) || bound <= 0 || bound > .2 {
			return false
		}
	}
	return math.Abs(value) <= bound
}

// commandGait proposes one commanded axis without interpreting measured gait
// oscillation as an axis change. The caller must enforce acknowledged-command
// transition settling; this stateless planner cannot prove a previous turn has
// ended. Measured, previous and proposed motion still need joint clearance.
func (g *Grid) commandGait(p Pose, target Point, motion BodyMotion, l MotionLimits) (Command, error) {
	zero := Command{}
	if _, err := g.commandInputs(p, target, motion.ForwardMPS, motion.YawRateRPS, l); err != nil {
		return zero, err
	}
	// Even a reached goal/zero proposal must cover arbitrary measured drift.
	if err := g.CheckContinuation(p, motion, nil, zero, l); err != nil {
		return zero, err
	}
	if !g.area.contains(target) {
		return zero, ErrBounds
	}
	if distance(point(p), target) <= l.WaypointToleranceM {
		return zero, nil
	}
	angle := normalize(heading(point(p), target) - p.Yaw)
	proposal := Command{ForwardMPS: l.MaxForwardMPS}
	if math.Abs(angle) > .08 {
		proposal = Command{YawRateRPS: math.Copysign(l.MaxYawRateRPS, angle)}
	}
	if err := g.CheckContinuation(p, motion, nil, proposal, l); err != nil {
		return zero, err
	}
	return proposal, nil
}
