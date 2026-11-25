"""Only localization and navigation, with no automatic recovery motion."""
import os
from launch import LaunchDescription
from launch_ros.actions import Node


def generate_launch_description():
    params = os.environ['PATROL_NAV_PARAMS']
    nodes = [('nav2_map_server', 'map_server'), ('nav2_amcl', 'amcl'),
             ('nav2_planner', 'planner_server'), ('nav2_controller', 'controller_server'),
             ('nav2_bt_navigator', 'bt_navigator')]
    actions = [Node(package=p, executable=n, name=n, output='screen',
                    parameters=[params], remappings=[('cmd_vel', '/nav_cmd_vel')]) for p, n in nodes]
    actions.append(Node(package='nav2_lifecycle_manager', executable='lifecycle_manager',
        name='lifecycle_manager_patrol', output='screen', parameters=[{
            'use_sim_time': True, 'autostart': True, 'bond_timeout': 4.0,
            'node_names': [n for _, n in nodes]}]))
    return LaunchDescription(actions)
