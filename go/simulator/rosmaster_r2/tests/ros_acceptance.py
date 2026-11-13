"""Run inside the disposable simulator container, with ROS sourced.

This resets and drives the local robot. No physical robot is used.
"""
import json
import math
import time
import urllib.request

import rclpy
from geometry_msgs.msg import Twist
from nav_msgs.msg import Odometry
from sensor_msgs.msg import Imu, JointState, LaserScan, Image, CameraInfo
from tf2_msgs.msg import TFMessage
from rclpy.qos import qos_profile_sensor_data


def http(path, body=None):
    request = urllib.request.Request('http://127.0.0.1:8890/api/' + path,
                                     data=None if body is None else json.dumps(body).encode(),
                                     headers={'Content-Type': 'application/json'})
    with urllib.request.urlopen(request, timeout=2) as response:
        return json.load(response)


def main():
    assert http('status')['simulation'] is True
    http('reset', {})
    http('arm', {'mode': 'ros'})
    rclpy.init()
    node = rclpy.create_node('r2_acceptance')
    pub = node.create_publisher(Twist, '/cmd_vel', 1)
    samples = {topic: [] for topic in ['/odom', '/scan', '/imu/data', '/joint_states', '/tf', '/camera/color/image_raw', '/camera/depth/image_raw', '/camera/color/camera_info', '/camera/depth/camera_info']}
    subscriptions = []
    for topic, kind in [('/odom', Odometry), ('/scan', LaserScan), ('/imu/data', Imu), ('/joint_states', JointState), ('/tf', TFMessage), ('/camera/color/image_raw', Image), ('/camera/depth/image_raw', Image), ('/camera/color/camera_info', CameraInfo), ('/camera/depth/camera_info', CameraInfo)]:
        callback = (lambda name: lambda msg: samples[name].append(msg))(topic)
        subscriptions.append(node.create_subscription(kind, topic, callback, qos_profile_sensor_data))

    def spin(seconds, command=None):
        until, next_command = time.monotonic() + seconds, 0
        while time.monotonic() < until:
            rclpy.spin_once(node, timeout_sec=.01)
            if command is not None and time.monotonic() >= next_command:
                pub.publish(command)
                next_command = time.monotonic() + .05

    try:
        spin(2)
        message = Twist()
        message.linear.x, message.angular.z = .45, .3
        spin(2, message)
        moved = http('status')
        assert moved['healthy'], moved
        assert moved['state']['x'] > .3 and moved['state']['yaw'] > .2, moved
        spin(.5)
        assert http('status')['state']['speed'] == 0, 'command expiry did not stop motion'
        for topic, values in samples.items():
            assert len(values) >= (15 if topic == '/scan' or topic.startswith('/camera/') else 60), (topic, len(values))
        rgb = samples['/camera/color/image_raw'][-1]
        depth = samples['/camera/depth/image_raw'][-1]
        assert (rgb.width, rgb.height, rgb.encoding, len(rgb.data)) == (320, 240, 'rgb8', 320*240*3)
        assert (depth.width, depth.height, depth.encoding, len(depth.data)) == (320, 240, '16UC1', 320*240*2)
        assert rgb.header.frame_id == depth.header.frame_id == 'camera_optical_frame'
        assert any(depth.data), 'depth image is empty'
        for topic in ['/camera/color/camera_info', '/camera/depth/camera_info']:
            info = samples[topic][-1]
            assert info.k[0] > 0 and info.k[4] > 0 and info.width == 320
        scan = samples['/scan'][-1]
        assert len(scan.ranges) == 360 and scan.header.frame_id == 'laser_frame'
        assert any(math.isfinite(value) for value in scan.ranges)
        assert samples['/odom'][-1].child_frame_id == 'base_link'
        assert len(samples['/joint_states'][-1].position) == 6
        assert samples['/imu/data'][-1].linear_acceleration.z > 9.8
        http('reset', {})
        spin(.5, message)
        assert http('status')['state']['x'] == 0, 'reset rearmed old publisher'
        http('arm', {'mode': 'ros'})
        message.linear.x, message.angular.z = -.4, 0.
        spin(1., message)
        assert http('status')['state']['x'] < -.1, 'reverse ROS command failed'
        http('stop', {})
        spin(.4, message)
        assert http('status')['state']['speed'] == 0, 'stop did not revoke ROS control'
        print(json.dumps({'passed': True, 'received': {topic: len(values) for topic, values in samples.items()},
                          'forward_pose': moved['state']}, indent=2))
    finally:
        http('reset', {})
        node.destroy_node()
        rclpy.shutdown()


if __name__ == '__main__':
    main()
