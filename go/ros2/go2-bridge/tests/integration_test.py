"""End-to-end check inside the bridge image: a fake agent streams robot data on
a Unix socket, the bridge runs unchanged, and a ROS 2 client checks every
published topic and sends commands back. No robot or real agent involved.

    python3 tests/integration_test.py   # inside the image, from /opt/go2-bridge
"""

import json
import os
import struct
import subprocess
import sys
import tempfile
import threading
import time
from concurrent import futures

import av
import grpc
import lz4.block
import numpy as np
import rclpy
from foxglove_msgs.msg import CompressedVideo
from geometry_msgs.msg import Twist
from nav_msgs.msg import Odometry
from sensor_msgs.msg import Image, Imu, JointState, Joy, PointCloud2
from std_msgs.msg import String, UInt8MultiArray
from unitree_api.msg import Request, Response
from unitree_go.msg import LowState, SportModeState

from go2_bridge import unitree_robot_service_pb2 as pb
from go2_bridge import unitree_robot_service_pb2_grpc as pb_grpc
from go2_bridge import unitree_service_pb2 as pairing_pb
from go2_bridge import unitree_service_pb2_grpc as pairing_grpc

SERIAL = "B42D1000TEST0001"


def envelope(topic, data):
    return json.dumps({"type": "msg", "topic": topic, "data": data}).encode()


def lidar_frame():
    grid = bytearray(0x800)
    grid[0] = 0b11000000  # two voxels
    meta = {"src_size": len(grid), "origin": [0.5, 0.0, 0.0], "resolution": 0.05, "frame_id": "odom"}
    header = json.dumps({"type": "msg", "topic": "rt/utlidar/voxel_map_compressed", "data": meta}).encode()
    return struct.pack("<HHI4x", 2, 0, len(header)) + header + lz4.block.compress(bytes(grid), store_size=False)


def h264_frames(count=30):
    encoder = av.CodecContext.create("libx264", "w")
    encoder.width, encoder.height, encoder.pix_fmt = 64, 48, "yuv420p"
    encoder.options = {"tune": "zerolatency", "g": "10"}
    frames = []
    for i in range(count):
        image = np.full((48, 64, 3), i * 8 % 255, dtype=np.uint8)
        frame = av.VideoFrame.from_ndarray(image, format="rgb24").reformat(format="yuv420p")
        frames += [bytes(p) for p in encoder.encode(frame)]
    return frames + [bytes(p) for p in encoder.encode(None)]


# Over Wi-Fi lowstate carries only rpy; the sport state has the full IMU.
LOWSTATE = {
    "imu_state": {"rpy": [0, 0, 0]},
    "motor_state": [{"q": 0.1 * i, "dq": 0.0, "temperature": 30} for i in range(20)],
    "bms_state": {"soc": 77, "current": -1200},
    "foot_force": [10, 20, 30, 40],
    "power_v": 28.5,
}
SPORTSTATE = {
    "imu_state": {"quaternion": [1, 0, 0, 0], "gyroscope": [0, 0, 0.1], "accelerometer": [0, 0, 9.8], "rpy": [0, 0, 0]},
    "mode": 1, "position": [1.0, 2.0, 0.3], "body_height": 0.3,
    "velocity": [0.2, 0.0, 0.0], "yaw_speed": 0.1,
}


class FakeAgent(pb_grpc.WendyUnitreeRobotServiceServicer, pairing_grpc.WendyUnitreeServiceServicer):
    def __init__(self):
        self.commands = []
        self.video = h264_frames()
        self.block_commands = threading.Event()
        self.command_started = threading.Event()

    def ListUnitreePairings(self, request, context):
        return pairing_pb.ListUnitreePairingsResponse(robots=[pairing_pb.UnitreeRobot(serial=SERIAL, paired=True, connected=True)])

    def Command(self, request, context):
        self.command_started.set()
        while self.block_commands.is_set() and context.is_active():
            time.sleep(0.01)
        self.commands.append((pb.UnitreeCommand.Name(request.command).removeprefix("UNITREE_COMMAND_"),
                              round(request.forward, 3), round(request.lateral, 3), round(request.yaw, 3)))
        return pb.UnitreeCommandResponse()

    def Stream(self, request, context):
        assert request.serial == SERIAL and request.video, request
        i = 0
        while context.is_active():
            now = time.time_ns()
            for topic, data in (("rt/lf/lowstate", LOWSTATE), ("rt/lf/sportmodestate", SPORTSTATE),
                                ("rt/wirelesscontroller", {"lx": 0.5, "ly": 0, "rx": 0, "ry": 0, "keys": 1})):
                yield pb.UnitreeStreamMessage(topic=pb.UnitreeTopicMessage(topic=topic, data=envelope(topic, data), received_unix_nanos=now))
            yield pb.UnitreeStreamMessage(topic=pb.UnitreeTopicMessage(
                topic="rt/utlidar/voxel_map_compressed", data=lidar_frame(), binary=True, received_unix_nanos=now))
            yield pb.UnitreeStreamMessage(video=pb.UnitreeVideoFrame(annexb=self.video[i % len(self.video)], received_unix_nanos=now))
            i += 1
            time.sleep(0.05)


def main():
    socket = os.path.join(tempfile.mkdtemp(), "agent.sock")
    agent = FakeAgent()
    server = grpc.server(futures.ThreadPoolExecutor(max_workers=4))
    pb_grpc.add_WendyUnitreeRobotServiceServicer_to_server(agent, server)
    pairing_grpc.add_WendyUnitreeServiceServicer_to_server(agent, server)
    server.add_insecure_port("unix:" + socket)
    server.start()

    bridge = subprocess.Popen([sys.executable, "-m", "go2_bridge.node"], env={**os.environ, "WENDY_AGENT_SOCKET": socket})
    rclpy.init()
    node = rclpy.create_node("go2_bridge_check")
    received = {}
    responses = {}

    def keep(name):
        return lambda msg: received.setdefault(name, msg)

    def keep_response(msg):
        responses[msg.header.identity.id] = msg

    for name, kind, topic in (("lowstate", LowState, "/lowstate"), ("sport", SportModeState, "/lf/sportmodestate"),
                              ("imu", Imu, "/imu/data"), ("joints", JointState, "/joint_states"),
                              ("odom", Odometry, "/odom"), ("cloud", PointCloud2, "/utlidar/cloud"),
                              ("robot_odom", Odometry, "/utlidar/robot_odom"), ("lidar_imu", Imu, "/utlidar/imu"),
                              ("cloud_base", PointCloud2, "/utlidar/cloud_base"), ("voxel_map", PointCloud2, "/go2/voxel_map"),
                              ("joy", Joy, "/joy"), ("image", Image, "/camera/color/image_raw"),
                              ("h264", CompressedVideo, "/camera/color/h264"),
                              ("raw", String, "/go2/raw/lf/lowstate"),
                              ("raw_lidar", UInt8MultiArray, "/go2/raw/utlidar/voxel_map_compressed")):
        node.create_subscription(kind, topic, keep(name), 10)
    node.create_subscription(Response, "/api/sport/response", keep_response, 10)
    cmd_vel = node.create_publisher(Twist, "/cmd_vel", 10)
    sport = node.create_publisher(Request, "/api/sport/request", 10)

    deadline = time.time() + 60
    wanted = {"lowstate", "sport", "imu", "joints", "odom", "cloud", "joy", "image", "h264", "raw", "raw_lidar",
              "robot_odom", "lidar_imu", "cloud_base", "voxel_map"}
    while time.time() < deadline and not wanted <= received.keys():
        rclpy.spin_once(node, timeout_sec=0.1)
    missing = wanted - received.keys()

    twist = Twist()
    twist.linear.x, twist.linear.y, twist.angular.z = 5.0, 0.1, -0.2  # forward is clamped to 0.8
    request = Request()
    request.header.identity.id, request.header.identity.api_id = 7, 1016
    sit = Request()
    sit.header.identity.id, sit.header.identity.api_id = 8, 1009  # not allowed
    for _ in range(20):
        cmd_vel.publish(twist)
        sport.publish(request)
        sport.publish(sit)
        rclpy.spin_once(node, timeout_sec=0.1)
        if ("MOVE" in str(agent.commands)) and ("HELLO" in str(agent.commands)) and 7 in responses and 8 in responses:
            break

    # A command can consume its full deadline while the agent restarts. It must
    # not occupy the single-threaded ROS executor and hold up unrelated topics.
    agent.command_started.clear()
    agent.block_commands.set()
    cmd_vel.publish(twist)
    blocked_deadline = time.monotonic() + 2
    while time.monotonic() < blocked_deadline and not agent.command_started.is_set():
        rclpy.spin_once(node, timeout_sec=0.05)
    prompt = Request()
    prompt.header.identity.id, prompt.header.identity.api_id = 9, 1009
    started = time.monotonic()
    sport.publish(prompt)
    while time.monotonic() - started < 0.5 and 9 not in responses:
        rclpy.spin_once(node, timeout_sec=0.05)
    executor_responsive = 9 in responses
    agent.block_commands.clear()

    bridge.terminate()
    bridge.wait(10)

    failures = []
    if missing:
        failures.append(f"no messages on {sorted(missing)}")
    else:
        low, odom, cloud, image, joints = (received[k] for k in ("lowstate", "odom", "cloud", "image", "joints"))
        if low.bms_state.soc != 77 or low.foot_force[3] != 40 or abs(low.power_v - 28.5) > 1e-3:
            failures.append("LowState fields not copied")
        if (odom.pose.pose.position.x, odom.pose.pose.position.y) != (1.0, 2.0) or odom.child_frame_id != "base_link":
            failures.append("odometry wrong")
        frames = {k: received[k].header.frame_id for k in ("cloud", "cloud_base", "voxel_map")}
        if cloud.width != 2 or frames != {"cloud": "utlidar_lidar", "cloud_base": "base_link", "voxel_map": "odom"}:
            failures.append(f"cloud has {cloud.width} points; frames {frames}")
        base = np.frombuffer(bytes(received["cloud_base"].data), dtype=np.float32).reshape(-1, 3)
        # Voxels at odom (0.5, 0, 0) and (0.55, 0, 0); robot at (1, 2, 0.3) with identity orientation.
        if not np.allclose(base[0], [-0.5, -2.0, -0.3], atol=1e-5):
            failures.append(f"cloud_base point {base[0]}")
        if (image.width, image.height, image.encoding) != (64, 48, "rgb8"):
            failures.append(f"image {image.width}x{image.height} {image.encoding}")
        if joints.name[0] != "FR_hip_joint" or abs(joints.position[1] - 0.1) > 1e-6:
            failures.append("joint states wrong")
        if received["joy"].axes[0] != 0.5 or received["joy"].buttons[0] != 1:
            failures.append("joy wrong")
    if ("MOVE", 0.8, 0.1, -0.2) not in agent.commands or ("HELLO", 0.0, 0.0, 0.0) not in agent.commands:
        failures.append(f"commands {agent.commands[:5]}")
    if any(c[0] not in ("MOVE", "HELLO", "STOP") for c in agent.commands):
        failures.append(f"unexpected command {agent.commands}")
    if not executor_responsive:
        failures.append("a blocked agent command stalled the ROS executor")
    if failures:
        print("FAIL:", *failures, sep="\n  ")
        sys.exit(1)
    print("PASS: all topics published; /cmd_vel and /api/sport/request reached the agent")


if __name__ == "__main__":
    main()
