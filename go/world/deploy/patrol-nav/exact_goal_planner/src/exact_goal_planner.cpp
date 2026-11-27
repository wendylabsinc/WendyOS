#include <algorithm>
#include <cmath>
#include <mutex>
#include <string>

#include "exact_goal_planner/terminal_orientation.hpp"
#include "nav2_core/exceptions.hpp"
#include "nav2_smac_planner/smac_planner_lattice.hpp"
#include "pluginlib/class_list_macros.hpp"

namespace wendy_nav
{
// Nav2 1.1.20 quantizes the requested heading into a lattice bin and returns
// that bin's heading. FollowPath then treats the path endpoint as its goal.
class ExactGoalSmacPlannerLattice : public nav2_smac_planner::SmacPlannerLattice
{
public:
  nav_msgs::msg::Path createPlan(
    const geometry_msgs::msg::PoseStamped & start,
    const geometry_msgs::msg::PoseStamped & goal) override
  {
    try {
      require(_costmap && _costmap_ros, "exact-goal planner is not configured");
      require(! _global_frame.empty() && start.header.frame_id == _global_frame &&
        goal.header.frame_id == _global_frame, "planner start or goal frame mismatch");
      validatePlanarPose(start.pose);
      validatePlanarPose(goal.pose);
      auto path = SmacPlannerLattice::createPlan(start, goal);
      std::unique_lock<nav2_costmap_2d::Costmap2D::mutex_t> lock(*_costmap->getMutex());
      double radius = 0.0;
      const auto footprint = _costmap_ros->getRobotFootprint();
      require(footprint.size() >= 3, "missing polygon robot footprint");
      for (const auto & point : footprint) {
        require(std::isfinite(point.x) && std::isfinite(point.y), "invalid robot footprint");
        radius = std::max(radius, std::hypot(point.x, point.y));
      }
      const GridView grid{_costmap->getSizeInCellsX(), _costmap->getSizeInCellsY(),
        _costmap->getResolution(), _costmap->getOriginX(), _costmap->getOriginY(),
        _costmap->getCharMap()};
      restoreTerminalOrientation(path, goal, _global_frame, grid, radius);
      return path;
    } catch (const std::exception & error) {
      throw nav2_core::PlannerException(std::string("Exact goal orientation rejected: ") + error.what());
    }
  }
};
}  // namespace wendy_nav

PLUGINLIB_EXPORT_CLASS(wendy_nav::ExactGoalSmacPlannerLattice, nav2_core::GlobalPlanner)
