"""Mapless Nav2: unknown space stays blocked and every velocity traverses the guard."""
import copy
from pathlib import Path
import tempfile
import yaml
from ament_index_python.packages import get_package_share_directory
from launch import LaunchDescription
from launch.actions import RegisterEventHandler, EmitEvent
from launch.event_handlers import OnProcessExit
from launch.events import Shutdown
from launch_ros.actions import Node
from robot_navigation.settings import load_settings


def parameters(settings, defaults):
    data = copy.deepcopy(defaults)
    c, g, topics = settings["runtime"], settings["guard"], settings["topics"]
    radius = g["footprint_radius"] + g["clearance"]
    nav_frame, base = c["navigation_frame"], c["base_frame"]
    for value in data.values():
        if isinstance(value, dict) and "ros__parameters" in value:
            value["ros__parameters"]["use_sim_time"] = False
    control = data["controller_server"]["ros__parameters"]
    control.update(odom_topic=topics["odometry"], speed_limit_topic="/robot_navigation/speed_limit")
    control["progress_checker"].update(required_movement_radius=.1, movement_time_allowance=15.0)
    control["general_goal_checker"].update(xy_goal_tolerance=.15, yaw_goal_tolerance=.15)
    control["FollowPath"].update(max_vel_x=c["max_linear_speed"], max_speed_xy=c["max_linear_speed"],
        max_vel_theta=c["max_angular_speed"], acc_lim_x=g["braking_deceleration"],
        decel_lim_x=-g["braking_deceleration"], acc_lim_theta=.5, decel_lim_theta=-.5,
        trans_stopped_velocity=.02, xy_goal_tolerance=.15, debug_trajectory_details=False)
    for name, size in (("local_costmap", 6), ("global_costmap", 20)):
        data[name] = {name: {"ros__parameters": {
            "use_sim_time": False, "global_frame": nav_frame, "robot_base_frame": base,
            "rolling_window": True, "width": size, "height": size, "resolution": .05,
            "robot_radius": radius, "footprint_padding": .0, "track_unknown_space": True,
            "update_frequency": 10.0, "publish_frequency": 2.0, "transform_tolerance": .2,
            "always_send_full_costmap": True, "plugins": ["obstacle_layer", "inflation_layer"],
            "obstacle_layer": {"plugin": "nav2_costmap_2d::ObstacleLayer", "enabled": True,
                "observation_sources": "scan", "scan": {
                    "topic": topics["scan"], "data_type": "LaserScan", "clearing": True,
                    "marking": True, "inf_is_valid": g.get("allow_infinite_ranges", False),
                    "obstacle_max_range": 10.0, "raytrace_max_range": 10.0,
                    "obstacle_min_range": .0, "raytrace_min_range": .0,
                    "expected_update_rate": .3, "observation_persistence": .0}},
            "inflation_layer": {"plugin": "nav2_costmap_2d::InflationLayer",
                "inflation_radius": radius + .2, "cost_scaling_factor": 3.0},
        }}}
    data["planner_server"]["ros__parameters"]["GridBased"].update(allow_unknown=False, tolerance=.1)
    data["bt_navigator"]["ros__parameters"].update(global_frame=nav_frame, robot_base_frame=base,
        odom_topic=topics["odometry"], default_nav_to_pose_bt_xml="/app/navigate.xml",
        default_nav_through_poses_bt_xml="/app/reject_waypoints.xml")
    return data


def generate_launch_description():
    settings = load_settings()
    defaults = yaml.safe_load((Path(get_package_share_directory("nav2_bringup")) / "params/nav2_params.yaml").read_text())
    values = parameters(settings, defaults)
    path = Path(tempfile.mkdtemp(prefix="wendy-nav2-")) / "parameters.yaml"
    path.write_text(yaml.safe_dump({"robot_navigation": values}))
    nodes = []
    for package, executable in (("nav2_planner", "planner_server"),
                                ("nav2_controller", "controller_server"),
                                ("nav2_bt_navigator", "bt_navigator")):
        nodes.append(Node(package=package, executable=executable, name=executable,
            namespace="robot_navigation", output="screen", parameters=[str(path)],
            remappings=[("/tf", "/tf"), ("/tf_static", "/tf_static"),
                        ("cmd_vel", "/robot_navigation/unsafe_cmd_vel")]))
    nodes.append(Node(package="nav2_lifecycle_manager", executable="lifecycle_manager",
        name="lifecycle_manager_navigation", namespace="robot_navigation", output="screen",
        parameters=[{"use_sim_time": False, "autostart": True,
                     "bond_timeout": 2.0, "attempt_respawn_reconnection": False,
                     "node_names": ["planner_server", "controller_server", "bt_navigator"]}]))
    handlers = [RegisterEventHandler(OnProcessExit(target_action=node,
        on_exit=[EmitEvent(event=Shutdown(reason="Nav2 process exited"))])) for node in nodes]
    return LaunchDescription(nodes + handlers)
