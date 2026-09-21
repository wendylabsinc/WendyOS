"""Real ROS/Nav2/MCP test with a planar simulated robot, in an isolated container.

Run only with Docker --network none. Motor output goes solely to /simulation/cmd_vel.
"""
import asyncio
import json
import math
import os
from pathlib import Path
import signal
import subprocess
import tempfile
import threading
import time

import rclpy
from rclpy.node import Node
from rclpy.qos import qos_profile_sensor_data
from geometry_msgs.msg import TransformStamped, Twist
from nav_msgs.msg import Odometry
from sensor_msgs.msg import LaserScan, Imu
from sensor_msgs.msg import Image, CameraInfo
from tf2_ros import TransformBroadcaster, StaticTransformBroadcaster
from vision_msgs.msg import Detection2DArray, Detection2D, ObjectHypothesisWithPose
from mcp import ClientSession
from mcp.client.streamable_http import streamablehttp_client


class Simulation(Node):
    def __init__(self):
        super().__init__("navigation_integration_simulator")
        self.x = self.y = self.yaw = 0.0
        self.v = self.w = 0.0
        self.last_command = 0.0
        self.last = time.monotonic()
        self.publish_scan = True
        self.moved = False
        self.camera_enabled = False
        self.person_visible = True
        self.last_camera = 0.0
        self.tf = TransformBroadcaster(self)
        # A calibrated stationary RGB-D observer in the simulator's odom frame.
        self.camera_tf = StaticTransformBroadcaster(self)
        mount = TransformStamped()
        mount.header.frame_id, mount.child_frame_id = "odom", "camera_optical"
        mount.transform.translation.z = 1.3
        q = mount.transform.rotation
        q.x, q.y, q.z, q.w = -.5, .5, -.5, .5
        self.camera_tf.sendTransform(mount)
        self.rgb = self.create_publisher(Image, "/camera/color/image_rect", qos_profile_sensor_data)
        self.depth = self.create_publisher(Image, "/camera/aligned_depth_to_color/image_raw", qos_profile_sensor_data)
        self.info = self.create_publisher(CameraInfo, "/camera/color/camera_info", qos_profile_sensor_data)
        self.people = self.create_publisher(Detection2DArray, "/robot_navigation/people", qos_profile_sensor_data)
        self.scan = self.create_publisher(LaserScan, "/scan", qos_profile_sensor_data)
        self.odom = self.create_publisher(Odometry, "/odom", qos_profile_sensor_data)
        self.imu = self.create_publisher(Imu, "/imu/data", qos_profile_sensor_data)
        self.create_subscription(Twist, "/simulation/cmd_vel", self.drive, 1)
        self.create_timer(.04, self.tick)

    def drive(self, msg):
        self.v, self.w = msg.linear.x, msg.angular.z
        self.last_command = time.monotonic()
        self.moved |= abs(self.v) > .001 or abs(self.w) > .001

    def tick(self):
        now = time.monotonic()
        dt, self.last = min(.1, now-self.last), now
        if now-self.last_command > .25:
            self.v = self.w = 0.0
        self.x += self.v * math.cos(self.yaw) * dt
        self.y += self.v * math.sin(self.yaw) * dt
        self.yaw += self.w * dt
        stamp = self.get_clock().now().to_msg()
        tf = TransformStamped()
        tf.header.stamp, tf.header.frame_id, tf.child_frame_id = stamp, "odom", "base_link"
        tf.transform.translation.x, tf.transform.translation.y = self.x, self.y
        tf.transform.rotation.z, tf.transform.rotation.w = math.sin(self.yaw/2), math.cos(self.yaw/2)
        self.tf.sendTransform(tf)
        odom = Odometry()
        odom.header = tf.header
        odom.child_frame_id = "base_link"
        odom.pose.pose.position.x, odom.pose.pose.position.y = self.x, self.y
        odom.pose.pose.orientation = tf.transform.rotation
        odom.pose.covariance[0] = odom.pose.covariance[7] = odom.pose.covariance[35] = .01
        odom.twist.twist.linear.x, odom.twist.twist.angular.z = self.v, self.w
        self.odom.publish(odom)
        imu = Imu()
        imu.header.stamp, imu.header.frame_id = stamp, "base_link"
        imu.orientation = tf.transform.rotation
        imu.orientation_covariance[0] = imu.orientation_covariance[4] = imu.orientation_covariance[8] = .01
        self.imu.publish(imu)
        if self.publish_scan:
            scan = LaserScan()
            scan.header.stamp, scan.header.frame_id = stamp, "base_link"
            scan.angle_min, scan.angle_max = -math.pi, math.pi
            scan.angle_increment = 2*math.pi/720
            scan.range_min, scan.range_max = .05, 10.0
            scan.ranges = [8.0]*720
            self.scan.publish(scan)
        if self.camera_enabled and now-self.last_camera >= .15:
            self.last_camera = now
            self.camera(stamp)

    def camera(self, stamp):
        import numpy as np
        rgb = Image()
        rgb.header.stamp, rgb.header.frame_id = stamp, "camera_optical"
        rgb.width, rgb.height, rgb.step, rgb.encoding = 200, 200, 600, "bgr8"
        rgb.data = np.zeros((200, 200, 3), dtype=np.uint8).tobytes()
        depth = Image()
        depth.header = rgb.header
        depth.width, depth.height, depth.step, depth.encoding = 200, 200, 400, "16UC1"
        depth.data = np.full((200, 200), 4500, dtype="<u2").tobytes()
        info = CameraInfo()
        info.header = rgb.header
        info.width, info.height = 200, 200
        info.k = [400., 0., 100., 0., 400., 100., 0., 0., 1.]
        info.p = [400., 0., 100., 0., 0., 400., 100., 0., 0., 0., 1., 0.]
        info.r = [1., 0., 0., 0., 1., 0., 0., 0., 1.]
        people = Detection2DArray()
        people.header = rgb.header
        if self.person_visible:
            person = Detection2D()
            person.header, person.id = rgb.header, "synthetic-person-1"
            person.bbox.center.position.x = person.bbox.center.position.y = 100.
            person.bbox.size_x, person.bbox.size_y = 80., 160.
            hypothesis = ObjectHypothesisWithPose()
            hypothesis.hypothesis.class_id, hypothesis.hypothesis.score = "person", .95
            person.results = [hypothesis]
            people.detections = [person]
        self.rgb.publish(rgb)
        self.depth.publish(depth)
        self.info.publish(info)
        self.people.publish(people)


def result_dict(result):
    assert not result.isError, result
    if result.structuredContent:
        return result.structuredContent
    return json.loads(next(item.text for item in result.content if item.type == "text"))


async def exercise(sim):
    # Wait for the HTTP listener without creating extra MCP sessions on failures.
    import httpx
    for _ in range(200):
        try:
            async with httpx.AsyncClient() as client:
                await client.get("http://127.0.0.1:8128/mcp", timeout=.2)
            break
        except httpx.HTTPError:
            await asyncio.sleep(.1)
    async with streamablehttp_client("http://127.0.0.1:8128/mcp") as (read, write, _):
        async with ClientSession(read, write) as client:
            await client.initialize()
            names = {tool.name for tool in (await client.list_tools()).tools}
            assert {"robot_status", "navigation_goal", "navigation_cancel", "approach_person"} <= names
            status = None
            for _ in range(150):
                status = result_dict(await client.call_tool("robot_status", {}))
                if status["readiness"]["ready"]:
                    break
                await asyncio.sleep(.2)
            assert status["readiness"]["ready"], status
            assert not sim.moved, "startup must not emit motion"
            print("Readiness and no startup motion: OK", flush=True)
            args = {"request_id": "sim-route-1", "x": 1.0, "y": .0, "yaw": .0,
                    "frame_id": "odom", "max_speed": .15, "lease_seconds": 10.0}
            goal = result_dict(await client.call_tool("navigation_goal", args))
            repeat = result_dict(await client.call_tool("navigation_goal", args))
            assert repeat["goal_id"] == goal["goal_id"]
            for _ in range(100):
                state = result_dict(await client.call_tool("navigation_status", {"goal_id": goal["goal_id"]}))
                if sim.moved:
                    break
                await asyncio.sleep(.1)
            assert sim.moved, state
            print("Nav2 planned and simulated motor moved; duplicate request kept same goal: OK", flush=True)
            sim.publish_scan = False
            for _ in range(70):
                state = result_dict(await client.call_tool("navigation_status", {"goal_id": goal["goal_id"]}))
                if state["stopped_confirmed"]:
                    break
                await asyncio.sleep(.1)
            assert state["stopped_confirmed"] and state["state"] == "failed", state
            assert sim.v == 0 and sim.w == 0
            print("Laser loss cancelled action and confirmed stopping: OK", flush=True)
            sim.publish_scan = True
            for _ in range(50):
                status = result_dict(await client.call_tool("robot_status", {}))
                if status["readiness"]["ready"]:
                    break
                await asyncio.sleep(.1)
            assert status["readiness"]["ready"], status
            args["request_id"] = "sim-route-2"
            args["x"] = sim.x + 1.0
            goal = result_dict(await client.call_tool("navigation_goal", args))
            await asyncio.sleep(.5)
            result_dict(await client.call_tool("navigation_cancel", {"goal_id": goal["goal_id"]}))
            for _ in range(70):
                state = result_dict(await client.call_tool("navigation_status", {"goal_id": goal["goal_id"]}))
                if state["stopped_confirmed"]:
                    break
                await asyncio.sleep(.1)
            assert state["state"] == "cancelled" and state["stopped_confirmed"], state
            print("Explicit cancellation waited for action settlement and measured rest: OK", flush=True)
            args.update(request_id="sim-lease-expiry", x=sim.x + 1.0, lease_seconds=.8)
            goal = result_dict(await client.call_tool("navigation_goal", args))
            for _ in range(70):
                state = result_dict(await client.call_tool("navigation_status", {"goal_id": goal["goal_id"]}))
                if state["stopped_confirmed"]:
                    break
                await asyncio.sleep(.1)
            assert state["state"] == "failed" and state["reason"] == "lease_expired", state
            assert state["stopped_confirmed"]
            print("Read-only status polling did not renew the expired supervision lease: OK", flush=True)
            sim.moved = False
            args.update(request_id="sim-unknown-space", x=sim.x + 9.0, lease_seconds=10.0)
            goal = result_dict(await client.call_tool("navigation_goal", args))
            for _ in range(70):
                state = result_dict(await client.call_tool("navigation_status", {"goal_id": goal["goal_id"]}))
                if state["stopped_confirmed"]:
                    break
                await asyncio.sleep(.1)
            assert state["state"] == "failed" and state["stopped_confirmed"], state
            assert not sim.moved, "unknown-space plan must not emit movement"
            print("Nav2 rejected an endpoint beyond observed free space without movement: OK", flush=True)
            sim.camera_enabled = True
            for _ in range(40):
                targets = result_dict(await client.call_tool("robot_targets", {}))
                if targets["targets"]:
                    break
                await asyncio.sleep(.1)
            target = targets["targets"][0]
            observation = await client.call_tool("robot_observe", {})
            assert not observation.isError and any(c.type == "image" for c in observation.content)
            person_args = {"request_id": "sim-person", "target_id": target["target_id"],
                           "standoff": 1.5, "max_speed": .15, "lease_seconds": 10.0}
            approach = result_dict(await client.call_tool("approach_person", person_args))
            repeat = result_dict(await client.call_tool("approach_person", person_args))
            goal = approach["goal"]
            assert repeat["goal"]["goal_id"] == goal["goal_id"]
            assert approach["grounding"]["protected_standoff_m"] > 2.5
            for _ in range(40):
                if sim.moved:
                    break
                await asyncio.sleep(.1)
            assert sim.moved, result_dict(await client.call_tool("navigation_status", {"goal_id": goal["goal_id"]}))
            sim.person_visible = False
            for _ in range(70):
                state = result_dict(await client.call_tool("navigation_status", {"goal_id": goal["goal_id"]}))
                if state["stopped_confirmed"]:
                    break
                await asyncio.sleep(.1)
            assert state["state"] == "failed" and state["reason"] == "target_lost" and state["stopped_confirmed"], state
            print("Calibrated person approach ran through MCP/Nav2; target loss stopped it without switching: OK", flush=True)


def main():
    rclpy.init()
    sim = Simulation()
    thread = threading.Thread(target=lambda: rclpy.spin(sim), daemon=True)
    thread.start()
    with tempfile.TemporaryDirectory() as tmp:
        settings = json.loads(Path("/app/config.json").read_text())
        settings["runtime"].update(motion_enabled=True, watchdog_commissioned=True)
        settings["motor"].update(mode="twist", output_topic="/simulation/cmd_vel")
        settings["database"] = tmp + "/navigation.sqlite3"
        settings["start_detector"] = False
        path = Path(tmp) / "config.json"
        path.write_text(json.dumps(settings))
        env = {**os.environ, "ROBOT_NAVIGATION_CONFIG": str(path)}
        with open("/tmp/robot-navigation-smoke.log", "w") as log:
            app = subprocess.Popen(["python3", "-m", "robot_navigation.launch"], env=env,
                                   stdout=log, stderr=subprocess.STDOUT)
            try:
                asyncio.run(exercise(sim))
            except BaseException:
                print(Path("/tmp/robot-navigation-smoke.log").read_text()[-22000:], flush=True)
                raise
            finally:
                app.send_signal(signal.SIGTERM)
                try:
                    app.wait(timeout=8)
                except subprocess.TimeoutExpired:
                    app.kill()
                    app.wait()
                rclpy.shutdown()
                thread.join(timeout=2)
                sim.destroy_node()
    print("ROS/Nav2/MCP integration smoke passed", flush=True)


if __name__ == "__main__":
    main()
