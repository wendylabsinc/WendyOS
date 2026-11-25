// Proprietary Wendy World integration. No simulator truth enters this helper.
#ifndef WENDY_EXACT_GOAL_TERMINAL_ORIENTATION_HPP
#define WENDY_EXACT_GOAL_TERMINAL_ORIENTATION_HPP

#include <algorithm>
#include <cmath>
#include <cstddef>
#include <stdexcept>
#include <string>

namespace wendy_nav
{
struct GridView
{
  unsigned int width, height;
  double resolution, origin_x, origin_y;
  const unsigned char * cells;
};

inline void require(bool condition, const char * message)
{
  if (!condition) {
    throw std::runtime_error(message);
  }
}

template<typename Pose>
void validatePlanarPose(const Pose & pose)
{
  const auto & p = pose.position;
  const auto & q = pose.orientation;
  require(std::isfinite(p.x) && std::isfinite(p.y) && std::isfinite(p.z),
    "nonfinite planner pose");
  require(std::isfinite(q.x) && std::isfinite(q.y) &&
    std::isfinite(q.z) && std::isfinite(q.w), "nonfinite planner quaternion");
  require(std::abs(q.x * q.x + q.y * q.y + q.z * q.z + q.w * q.w - 1.0) <= 1e-6,
    "planner quaternion must have unit norm");
  require(std::abs(q.x) <= 1e-6 && std::abs(q.y) <= 1e-6,
    "planner quaternion must be planar");
}

inline void requireClearRotationDisk(const GridView & grid, double x, double y, double radius)
{
  require(grid.cells && grid.width && grid.height &&
    std::isfinite(grid.resolution) && grid.resolution > 0.0 &&
    std::isfinite(grid.origin_x) && std::isfinite(grid.origin_y) &&
    std::isfinite(x) && std::isfinite(y) && std::isfinite(radius) && radius > 0.0,
    "invalid terminal rotation geometry");
  const double max_x = grid.origin_x + grid.width * grid.resolution;
  const double max_y = grid.origin_y + grid.height * grid.resolution;
  require(std::isfinite(max_x) && std::isfinite(max_y), "invalid costmap extent");
  require(x - radius >= grid.origin_x && y - radius >= grid.origin_y &&
    x + radius < max_x && y + radius < max_y,
    "terminal rotation leaves the costmap");
  const auto min_ix = static_cast<unsigned int>(std::floor((x - radius - grid.origin_x) / grid.resolution));
  const auto max_ix = static_cast<unsigned int>(std::floor((x + radius - grid.origin_x) / grid.resolution));
  const auto min_iy = static_cast<unsigned int>(std::floor((y - radius - grid.origin_y) / grid.resolution));
  const auto max_iy = static_cast<unsigned int>(std::floor((y + radius - grid.origin_y) / grid.resolution));
  require(max_ix < grid.width && max_iy < grid.height,
    "terminal rotation indexes leave the costmap");
  for (unsigned int iy = min_iy; iy <= max_iy; ++iy) {
    for (unsigned int ix = min_ix; ix <= max_ix; ++ix) {
      // Cell-area intersection includes obstacles enclosed by the footprint.
      const double left = grid.origin_x + ix * grid.resolution;
      const double bottom = grid.origin_y + iy * grid.resolution;
      const double dx = x - std::clamp(x, left, left + grid.resolution);
      const double dy = y - std::clamp(y, bottom, bottom + grid.resolution);
      if (dx * dx + dy * dy > radius * radius) {
        continue;
      }
      const auto cost = grid.cells[static_cast<std::size_t>(iy) * grid.width + ix];
      // Nav2 254 is lethal, 255 is unknown. Inflation (including 253) is
      // already a robot-clearance estimate and is not a physical obstacle.
      require(cost < 254, "terminal rotation intersects lethal or unknown costmap cells");
    }
  }
}

template<typename Path, typename PoseStamped>
void restoreTerminalOrientation(
  Path & path, const PoseStamped & goal, const std::string & global_frame,
  const GridView & grid, double footprint_radius)
{
  require(!global_frame.empty() && goal.header.frame_id == global_frame &&
    path.header.frame_id == global_frame, "planner goal or path frame mismatch");
  validatePlanarPose(goal.pose);
  require(!path.poses.empty(), "empty lattice path");
  for (const auto & pose : path.poses) {
    require(pose.header.frame_id == global_frame, "planner path pose frame mismatch");
    validatePlanarPose(pose.pose);
  }
  auto & terminal = path.poses.back().pose;
  // The circumscribed disk contains the padded footprint at every possible
  // heading. It conservatively covers the complete terminal rotation, not
  // just a set of sampled angles. Smac's planned positions remain untouched.
  requireClearRotationDisk(grid, terminal.position.x, terminal.position.y, footprint_radius);
  terminal.orientation = goal.pose.orientation;
}
}  // namespace wendy_nav

#endif
