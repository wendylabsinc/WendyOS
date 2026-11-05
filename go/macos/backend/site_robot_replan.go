package backend

import (
	"github.com/wendylabsinc/wendy/go/macos/backend/sitenav"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
)

// Keep the original order's destination, departure allowance and deadline.
// Replanning never creates a new movement order or refreshes a telemetry lease.
func (r *SiteRobot) replanLocked(grid sitenav.Grid, position sitenav.Point) {
	if err := r.command(pb.UnitreeCommand_UNITREE_COMMAND_STOP, 0, 0); err != nil {
		r.message = "Could not confirm Stop."
		r.stopLocked()
		return
	}
	path, err := grid.PlanDetour(position, r.observed.Pose.Yaw, r.goal)
	if err != nil {
		r.message = "Route blocked. No alternative route: " + err.Error()
		r.stopLocked()
		return
	}
	if !personalSpaceClear(r.personExclusion, path) {
		r.message = "The detour passes too close to the wearer. Movement stopped."
		r.stopLocked()
		return
	}
	arrivalYaw := r.observed.Pose.Yaw
	if len(path) > 1 {
		arrivalYaw = sitenav.Heading(path[len(path)-2], r.goal)
	}
	if err := r.navigationGridLocked(nil).PoseClearanceError(r.goal, arrivalYaw, "the destination"); err != nil {
		r.message = "Route blocked. No alternative route: " + err.Error()
		r.stopLocked()
		return
	}
	r.route = path[1:]
	r.routeOrigin = position
	r.forwardOrigin = nil
	r.message = "Route changed to avoid a blocked area."
	// Planning can outlast a sensor or UI lease. The next tick must recheck
	// freshness and all stopping checks before sending another MOVE.
}
