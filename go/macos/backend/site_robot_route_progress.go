package backend

import "math"

// Telemetry can cross a short waypoint between updates. Never steer back to an
// already passed waypoint: advance only inside the route corridor, otherwise stop.
// The following tick still checks the actual pose's turn and swept footprint.
func (r *SiteRobot) advanceRouteLocked() bool {
	p := r.observed.Pose.Position
	for len(r.route) > 0 {
		target := r.route[0]
		distance := p.Distance(target)
		dx, dy := target.X-r.routeOrigin.X, target.Y-r.routeOrigin.Y
		length := math.Hypot(dx, dy)
		passed := length > 1e-6 && (p.X-target.X)*dx+(p.Y-target.Y)*dy >= 0
		if len(r.route) > 1 && distance < .04 {
			r.routeOrigin = target
			r.route = r.route[1:]
			continue
		}
		if !passed {
			return true
		}
		crossTrack := math.Abs((p.X-r.routeOrigin.X)*dy-(p.Y-r.routeOrigin.Y)*dx) / length
		if len(r.route) > 1 && distance <= .15 && crossTrack <= .08 {
			r.routeOrigin = target
			r.route = r.route[1:]
			continue
		}
		r.message = "Go2 passed the route target. Movement stopped; preview a new route."
		r.stopLocked()
		return false
	}
	return true
}

// A forward command must produce forward odometry. Do not compensate a frame
// or hardware direction disagreement by commanding a larger turn or more speed.
func (r *SiteRobot) checkForwardProgressLocked() bool {
	if r.forwardOrigin == nil {
		return true
	}
	p, start := r.observed.Pose, r.forwardOrigin
	dx, dy := p.Position.X-start.Position.X, p.Position.Y-start.Position.Y
	distance := math.Hypot(dx, dy)
	if distance < .08 {
		return true
	}
	forward := dx*math.Cos(start.Yaw) + dy*math.Sin(start.Yaw)
	if forward < -.04 {
		r.message = "Go2 moved opposite its commanded heading. Movement stopped; check navigation alignment."
		r.stopLocked()
		return false
	}
	r.forwardOrigin = &p
	return true
}
