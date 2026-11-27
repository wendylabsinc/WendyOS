#include <cmath>
#include <iostream>
#include <limits>
#include <string>
#include <vector>

#include "exact_goal_planner/terminal_orientation.hpp"

struct Header {std::string frame_id = "map";};
struct Point {double x = 0, y = 0, z = 0;};
struct Quaternion {double x = 0, y = 0, z = 0, w = 1;};
struct Pose {Point position; Quaternion orientation;};
struct PoseStamped {Header header; Pose pose;};
struct Path {Header header; std::vector<PoseStamped> poses;};

int checks = 0;
void check(bool value, const char * message)
{
  ++checks;
  if (!value) {throw std::runtime_error(message);}
}

struct Fixture
{
  std::vector<unsigned char> cells = std::vector<unsigned char>(40 * 40, 0);
  wendy_nav::GridView grid{40, 40, 0.1, -2.0, -2.0, cells.data()};
  Path path;
  PoseStamped goal;
  Fixture()
  {
    PoseStamped first, last;
    first.pose.position = {-0.5, -0.5, 0.0};
    last.pose.position = {0.025, 0.025, 0.0};
    last.pose.orientation.z = std::sin(-0.46364766756166653 / 2.0);
    last.pose.orientation.w = std::cos(-0.46364766756166653 / 2.0);
    path.poses = {first, last};
    goal.pose.position = {0.05, 0.05, 0.0};
    goal.pose.orientation.z = std::sin(-0.24514650743845004 / 2.0);
    goal.pose.orientation.w = std::cos(-0.24514650743845004 / 2.0);
  }
  void run(double radius = 0.6)
  {
    wendy_nav::restoreTerminalOrientation(path, goal, "map", grid, radius);
  }
  void rejects(const char * message, double radius = 0.6)
  {
    const auto old = path.poses.empty() ? Quaternion{} : path.poses.back().pose.orientation;
    bool rejected = false;
    try {run(radius);} catch (const std::runtime_error &) {rejected = true;}
    check(rejected, message);
    if (!path.poses.empty()) {
      check(path.poses.back().pose.orientation.z == old.z &&
        path.poses.back().pose.orientation.w == old.w, "rejected path was changed");
    }
  }
};

int main()
{
  try {
    {
      Fixture f;
      const auto before = f.path;
      f.run();
      check(f.path.poses.size() == before.poses.size(), "path length changed");
      for (std::size_t i = 0; i < before.poses.size(); ++i) {
        const auto & actual = f.path.poses[i];
        const auto & old = before.poses[i];
        check(actual.pose.position.x == old.pose.position.x &&
          actual.pose.position.y == old.pose.position.y && actual.pose.position.z == old.pose.position.z,
          "planned position changed");
        check(actual.header.frame_id == old.header.frame_id, "path frame changed");
      }
      check(f.path.poses.front().pose.orientation.w == before.poses.front().pose.orientation.w,
        "intermediate heading changed");
      check(f.path.poses.back().pose.orientation.z == f.goal.pose.orientation.z &&
        f.path.poses.back().pose.orientation.w == f.goal.pose.orientation.w,
        "exact requested orientation not restored");
    }
    {Fixture f; f.goal.header.frame_id = "odom"; f.rejects("wrong goal frame accepted");}
    {Fixture f; f.path.header.frame_id = "odom"; f.rejects("wrong path frame accepted");}
    {Fixture f; f.path.poses[0].header.frame_id = "odom"; f.rejects("wrong pose frame accepted");}
    {Fixture f; f.path.poses.clear(); f.rejects("empty path accepted");}
    {Fixture f; f.goal.pose.position.x = std::numeric_limits<double>::quiet_NaN(); f.rejects("NaN goal accepted");}
    {Fixture f; f.goal.pose.orientation.w = std::numeric_limits<double>::infinity(); f.rejects("infinite quaternion accepted");}
    {Fixture f; f.goal.pose.orientation.w = 0; f.rejects("nonunit quaternion accepted");}
    {Fixture f; f.goal.pose.orientation = {0.1, 0, 0, std::sqrt(0.99)}; f.rejects("nonplanar goal accepted");}
    {Fixture f; f.path.poses.front().pose.position.z = std::numeric_limits<double>::infinity(); f.rejects("invalid path accepted");}
    {Fixture f; f.cells[20 * 40 + 20] = 254; f.rejects("enclosed obstacle accepted");}
    {Fixture f; f.cells[20 * 40 + 25] = 255; f.rejects("unknown rotation area accepted");}
    {Fixture f; f.cells[20 * 40 + 26] = 254; f.rejects("intersecting cell area accepted");}
    {Fixture f; f.cells[26 * 40 + 26] = 254; f.run(); check(true, "outside disk obstacle");}
    {Fixture f; std::fill(f.cells.begin(), f.cells.end(), 253); f.run(); check(true, "inflation double counted");}
    {Fixture f; f.path.poses.back().pose.position.x = 1.6; f.rejects("out of map rotation accepted");}
    {Fixture f; f.grid.cells = nullptr; f.rejects("missing grid accepted");}
    {Fixture f; f.grid.resolution = 0; f.rejects("invalid grid accepted");}
    {Fixture f; f.rejects("zero footprint radius accepted", 0.0);}
    {Fixture f; f.rejects("NaN footprint radius accepted", std::numeric_limits<double>::quiet_NaN());}
    std::cout << checks << " terminal orientation checks passed\n";
    return 0;
  } catch (const std::exception & error) {
    std::cerr << "FAILED: " << error.what() << '\n';
    return 1;
  }
}
