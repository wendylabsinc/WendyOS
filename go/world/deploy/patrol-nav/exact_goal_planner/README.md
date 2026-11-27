# Exact requested orientation for the lattice planner

This private Nav2 1.1.20 plugin subclasses `SmacPlannerLattice`. It retains the
base planner's configuration, lifecycle, search, smoothing and every planned
position. It replaces only the final pose's orientation with the original
requested orientation after validating the path and terminal rotation area.

Nav2 1.1.20 lattice search converts the requested yaw to a heading bin. Path
reconstruction returns that bin's yaw, and smoothing does not restore the
requested orientation. ControllerServer uses `path.poses.back()` as its goal.
Consequently a successful FollowPath action can miss the signed observation yaw
even though localization is accurate. This adapter preserves the requested yaw
without changing mission checks, goal positions or controller tolerances.

Before returning a path, the adapter checks matching global frames, finite poses
and planar unit quaternions. Under the costmap mutex it takes the maximum vertex
radius of the current padded robot footprint. Every costmap cell whose area
intersects the resulting disk must be known and nonlethal. The disk contains
the whole footprint through every heading, so it covers the complete terminal
rotation and catches obstacles enclosed within the footprint. It is conservative
and can reject a rotation whose exact polygon sweep would fit. It checks at
the existing planned endpoint, not at the requested position.

Costs 254 and 255 mean lethal and unknown and cause rejection. Inflated costs up
to 253 are allowed here because the footprint disk already accounts for robot
size. Treating inflated cells as physical obstacles would count clearance twice.
The original Smac collision checks still apply to the original route. The local
controller remains responsible for live obstacle checks during execution.

Configure the planner plugin as `wendy_nav::ExactGoalSmacPlannerLattice`. Build with
the installed pinned Nav2 dependencies using CMake and `BUILD_TESTING=ON`, then
run CTest before installing. `terminal_orientation_contract` exercises the pure
helper without ROS or simulation, including endpoint preservation, invalid
poses and frames, empty paths, enclosed lethal cells, unknown cells, cell-area
intersection, map boundaries and inflation semantics. Runtime qualification
must also exercise actual plugin loading and mission failure/recovery behavior.
