"""DDS integration check, skipped outside the ROS Humble runtime image."""
import time

import numpy as np
import pytest

rclpy = pytest.importorskip("rclpy")
from rclpy.context import Context
from rclpy.executors import SingleThreadedExecutor
from rclpy.node import Node
from sensor_msgs.msg import JointState, Image, Imu, CameraInfo
from nav_msgs.msg import Odometry
from rosgraph_msgs.msg import Clock
from std_srvs.srv import Trigger
from tf2_msgs.msg import TFMessage

from coke_demo.ros_bridge import ROSBridge


def test_real_dds_command_sensors_and_services():
    names = [f"joint_{i}" for i in range(43)]
    bridge = ROSBridge(names, namespace="/coke/test_g1")
    context = Context()
    rclpy.init(context=context)
    peer = Node("coke_ros_test_peer", context=context)
    executor = SingleThreadedExecutor(context=context)
    executor.add_node(peer)
    received = {}
    subscriptions = []
    topic_types = {"joint_states": JointState, "camera/color/image_raw": Image,
                   "camera/depth/image_rect_raw": Image, "perception/can_mask": Image,
                   "camera/color/camera_info": CameraInfo, "imu/data": Imu, "odom": Odometry}
    for topic, message_type in topic_types.items():
        subscriptions.append(peer.create_subscription(message_type, "/coke/test_g1/" + topic,
                             lambda msg, name=topic: received.__setitem__(name, msg), 1))
    subscriptions.append(peer.create_subscription(Clock, "/clock", lambda msg: received.__setitem__("clock", msg), 1))
    transforms = {}
    subscriptions.append(peer.create_subscription(TFMessage, "/tf",
                         lambda msg: transforms.update({tf.child_frame_id: tf for tf in msg.transforms}), 100))
    try:
        bridge.start()
        target = np.linspace(-.5, .5, 43)
        bridge.send_target(target)
        np.testing.assert_allclose(bridge.take_command(timeout=3), target)
        deadline = time.monotonic() + 5
        while any(pub.get_subscription_count() == 0 for name, pub in bridge._publishers.items() if name in topic_types):
            assert time.monotonic() < deadline, "DDS sensor discovery timed out"
            executor.spin_once(timeout_sec=.02)
        observation = dict(frame=1, sim_time=.025, epoch=1, camera_frame=1, camera_sim_time=.025,
                           q43=target, dq43=np.zeros(43), joint_effort43=np.ones(43),
                           rgb_u8=np.zeros((240, 320, 3), dtype=np.uint8), depth_m=np.full((240, 320), 1.25),
                           mask=np.ones((240, 320), dtype=bool), detection_valid=True,
                           base_position=[0, 0, .793], base_quaternion_wxyz=[1, 0, 0, 0],
                           base_linear_velocity=[0, 0, 0], base_angular_velocity=[0, 0, 0],
                           imu_specific_force=[0, 0, 9.81], camera_position=[.1, 0, 1.5], camera_rotation=np.eye(3))
        bridge.publish(observation, {"mode": "paused", "fixed_base": True})
        deadline = time.monotonic() + 5
        required_frames = {"coke/test_g1/base_link", "coke/test_g1/camera_optical_frame"}
        while len(received) < len(topic_types) + 1 or not required_frames.issubset(transforms):
            assert time.monotonic() < deadline, f"Missing ROS observations: {set(topic_types) - received.keys()}"
            bridge.publish(observation)
            executor.spin_once(timeout_sec=.02)
        np.testing.assert_allclose(received["joint_states"].position, target)
        assert received["joint_states"].name == names
        assert received["joint_states"].header.stamp.nanosec == 25_000_000
        assert received["clock"].clock.nanosec == 25_000_000
        depth = received["camera/depth/image_rect_raw"]
        assert depth.encoding == "32FC1" and depth.step == 320 * 4
        np.testing.assert_array_equal(np.frombuffer(depth.data, dtype="<f4"), np.full(240 * 320, 1.25))
        assert received["perception/can_mask"].data[0] == 255
        assert received["imu/data"].linear_acceleration.z == 9.81
        assert received["odom"].pose.pose.position.z == .793
        assert transforms["coke/test_g1/camera_optical_frame"].transform.rotation.x == 1.
        # These volatile subscribers join after the scene has stopped moving.
        # A heartbeat must deliver the cached exposure with its capture time.
        late = {}
        subscriptions.append(peer.create_subscription(JointState, "/coke/test_g1/joint_states",
                             lambda msg: late.__setitem__("joints", msg), 1))
        subscriptions.append(peer.create_subscription(Image, "/coke/test_g1/camera/color/image_raw",
                             lambda msg: late.__setitem__("rgb", msg), 1))
        deadline = time.monotonic() + 5
        while len(late) < 2:
            assert time.monotonic() < deadline, "Idle heartbeat did not reach late ROS subscribers"
            bridge.publish(observation)
            executor.spin_once(timeout_sec=.02)
        assert late["joints"].header.stamp.nanosec == 25_000_000
        assert late["rgb"].header.stamp.nanosec == 25_000_000
        assert bytes(late["rgb"].data) == bytes(240 * 320 * 3)
        assert bridge.status()["counts"]["observations"] == 1
        assert bridge.status()["counts"]["camera_frames"] == 1
        assert bridge.status()["counts"]["heartbeats"] >= 1
        client = peer.create_client(Trigger, "/coke/test_g1/simulation/reset")
        assert client.wait_for_service(timeout_sec=3)
        result = client.call_async(Trigger.Request())
        executor.spin_until_future_complete(result, timeout_sec=3)
        assert result.result().success
        assert bridge.take_event() == "reset"
    finally:
        bridge.close()
        executor.shutdown(timeout_sec=1)
        peer.destroy_node()
        context.shutdown()
