"""Run the Go2 simulator's example apps, unchanged, against this bridge.

A fake agent plays a Go2 in an 8 x 8 m walled room: it streams odometry,
low state, a camera and a LiDAR voxel map like the robot, and walks according
to the Move commands it receives (with the agent's 500 ms move expiry). Each
example must become ready, start, and drive the robot at a speed the real Go2
accepts. Run inside the bridge image with the examples mounted at /examples:

    python3 tests/simulator_examples_test.py [sensors teleop patrol roam hallway]
"""

import json
import math
import os
import subprocess
import sys
import tempfile
import threading
import time
import urllib.request
from concurrent import futures

import grpc
import lz4.block
import numpy as np
import rclpy
from std_srvs.srv import Trigger

sys.path.insert(0, os.path.dirname(__file__))
from integration_test import h264_frames  # noqa: E402

from go2_bridge import unitree_robot_service_pb2 as pb  # noqa: E402
from go2_bridge import unitree_robot_service_pb2_grpc as pb_grpc  # noqa: E402
from go2_bridge import unitree_service_pb2 as pairing_pb  # noqa: E402
from go2_bridge import unitree_service_pb2_grpc as pairing_grpc  # noqa: E402

SERIAL = "B42D1000TEST0001"
EXAMPLES = os.environ.get("GO2_EXAMPLES", "/examples")
ROOM = 4.0          # walls at +-4 m
BODY_HEIGHT = 0.3
RES = 0.1           # voxel size
GRID_XY, GRID_Z = 128, 12


def envelope(topic, data):
    return json.dumps({"type": "msg", "topic": topic, "data": data}).encode()


class FakeGo2(pb_grpc.WendyUnitreeRobotServiceServicer, pairing_grpc.WendyUnitreeServiceServicer):
    def __init__(self):
        self.lock = threading.Lock()
        self.x = self.y = self.yaw = 0.0
        self.command = (0.0, 0.0, 0.0)
        self.until = 0.0
        self.commands = []
        self.video = h264_frames(15)

    def ListUnitreePairings(self, request, context):
        return pairing_pb.ListUnitreePairingsResponse(robots=[pairing_pb.UnitreeRobot(serial=SERIAL, paired=True, connected=True)])

    def Command(self, request, context):
        name = pb.UnitreeCommand.Name(request.command).removeprefix("UNITREE_COMMAND_")
        with self.lock:
            self.commands.append((time.monotonic(), name, request.forward, request.lateral, request.yaw))
            if name == "MOVE":
                self.command, self.until = (request.forward, request.lateral, request.yaw), time.monotonic() + 0.5
            else:
                self.command = (0.0, 0.0, 0.0)
        return pb.UnitreeCommandResponse()

    def step(self, dt):
        with self.lock:
            vx, vy, wz = self.command if time.monotonic() < self.until else (0.0, 0.0, 0.0)
            c, s = math.cos(self.yaw), math.sin(self.yaw)
            self.x += (c * vx - s * vy) * dt
            self.y += (s * vx + c * vy) * dt
            self.yaw += wz * dt
            return self.x, self.y, self.yaw, (c * vx - s * vy, s * vx + c * vy), wz

    def voxel_frame(self, x, y):
        origin = np.array([round((x - GRID_XY * RES / 2) / RES) * RES, round((y - GRID_XY * RES / 2) / RES) * RES, -0.15])
        gx = origin[0] + RES * (np.arange(GRID_XY) + 0.5)
        gy = origin[1] + RES * (np.arange(GRID_XY) + 0.5)
        gz = origin[2] + RES * (np.arange(GRID_Z) + 0.5)
        X, Y = np.meshgrid(gx, gy)  # [y, x]
        wall = (np.abs(X) <= ROOM + RES) & (np.abs(Y) <= ROOM + RES) & ((np.abs(X) >= ROOM) | (np.abs(Y) >= ROOM))
        occupied = np.zeros((GRID_Z, GRID_XY, GRID_XY), dtype=bool)
        occupied[gz < 0.05] = (np.abs(X) < ROOM) & (np.abs(Y) < ROOM)  # floor
        occupied[(gz > 0.05) & (gz < 1.0)] |= wall
        bits = np.packbits(occupied.reshape(GRID_Z, GRID_XY, GRID_XY // 8, 8), axis=-1).reshape(-1)
        raw = bits.tobytes()
        meta = {"src_size": len(raw), "origin": origin.tolist(), "resolution": RES, "frame_id": "odom"}
        header = json.dumps({"type": "msg", "topic": "rt/utlidar/voxel_map_compressed", "data": meta}).encode()
        import struct
        return struct.pack("<HHI4x", 2, 0, len(header)) + header + lz4.block.compress(raw, store_size=False)

    def Stream(self, request, context):
        last, frame, tick = time.monotonic(), 0, 0
        while context.is_active():
            now = time.monotonic()
            x, y, yaw, (vx, vy), wz = self.step(now - last)
            last = now
            q = [math.cos(yaw / 2), 0.0, 0.0, math.sin(yaw / 2)]
            ns = time.time_ns()
            low = {"imu_state": {"rpy": [0, 0, yaw]},
                   "motor_state": [{"q": 0.0, "dq": 0.0} for _ in range(20)], "bms_state": {"soc": 80}}
            sport = {"imu_state": {"quaternion": q, "gyroscope": [0, 0, wz], "accelerometer": [0, 0, 9.81], "rpy": [0, 0, yaw]}, "mode": 1, "position": [x, y, BODY_HEIGHT],
                     "velocity": [vx, vy, 0.0], "yaw_speed": wz, "body_height": BODY_HEIGHT}
            for topic, data in (("rt/lf/lowstate", low), ("rt/lf/sportmodestate", sport)):
                yield pb.UnitreeStreamMessage(topic=pb.UnitreeTopicMessage(topic=topic, data=envelope(topic, data), received_unix_nanos=ns))
            if tick % 5 == 0:  # 10 Hz LiDAR
                yield pb.UnitreeStreamMessage(topic=pb.UnitreeTopicMessage(
                    topic="rt/utlidar/voxel_map_compressed", data=self.voxel_frame(x, y), binary=True, received_unix_nanos=ns))
            if tick % 3 == 0:
                yield pb.UnitreeStreamMessage(video=pb.UnitreeVideoFrame(annexb=self.video[frame % len(self.video)], received_unix_nanos=ns))
                frame += 1
            tick += 1
            time.sleep(0.02)

    def moves_since(self, start):
        with self.lock:
            return [c for c in self.commands if c[0] >= start and c[1] == "MOVE" and any(c[2:])]


def http(method, url, body=None):
    data = json.dumps(body).encode() if body is not None else None
    request = urllib.request.Request(url, data=data, method=method, headers={"Content-Type": "application/json"})
    with urllib.request.urlopen(request, timeout=5) as response:
        return json.loads(response.read())


def wait(predicate, timeout, interval=0.2):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        value = predicate()
        if value:
            return value
        time.sleep(interval)
    return None


def walking(robot, start):
    """A Move the physical Go2 would act on: forward >= 0.5 m/s, or a pure turn/strafe."""
    return [m for m in robot.moves_since(start) if abs(m[2]) >= 0.5 or (m[2] == 0 and (m[3] or m[4]))]


def run_app(name, args):
    log_file = open(f"/tmp/{name}.log", "w")
    try:
        process = subprocess.Popen([sys.executable, f"{EXAMPLES}/{name}/{args[0]}", *args[1:]],
                                   cwd=f"{EXAMPLES}/{name}", stdout=log_file, stderr=subprocess.STDOUT)
    except Exception:
        log_file.close()
        raise
    process.log_file = log_file
    return process


def stop_app(process):
    process.terminate()
    try:
        process.wait(timeout=5)
    except subprocess.TimeoutExpired:
        process.kill()
        process.wait()
    process.log_file.close()


def check_sensors(robot, node):
    app = run_app("sensors", ["dashboard.py", "--ignore-capture-age", "--port", "8904"])
    try:
        def live():
            try:
                topics = http("GET", "http://127.0.0.1:8904/api/status")["topics"]
            except OSError:
                return None
            states = {k: v["state"] for k, v in topics.items()}
            return states if all(s == "live" for s in states.values()) else None
        states = wait(live, 40)
        if not states:
            topics = http("GET", "http://127.0.0.1:8904/api/status")["topics"]
            return False, {k: (v["state"], v["error"]) for k, v in topics.items()}
        scan = http("GET", "http://127.0.0.1:8904/api/status")["topics"]["scan"]["data"]
        nearest = min(r for r in scan["ranges"] if r is not None) if scan else None
        return True, f"all topics live {sorted(states)}; nearest wall {nearest} m"
    finally:
        stop_app(app)


def check_teleop(robot, node):
    app = run_app("teleop", ["teleop.py", "--port", "8903"])
    try:
        if not wait(lambda: _try(lambda: http("GET", "http://127.0.0.1:8903/api/status")), 20):
            return False, "server did not start"
        session = http("POST", "http://127.0.0.1:8903/api/enable", {})["session"]
        start = time.monotonic()
        for sequence in range(15):
            http("POST", "http://127.0.0.1:8903/api/drive",
                 {"session": session, "sequence": sequence, "keys": ["w", "a"], "speed": 0.55, "turn_speed": 0.5})
            time.sleep(0.1)
        http("POST", "http://127.0.0.1:8903/api/release", {"session": session, "sequence": 99})
        moves = walking(robot, start)
        if not moves:
            return False, f"no walking Move: {robot.moves_since(start)[:3]}"
        return True, f"{len(moves)} Moves, e.g. forward {moves[-1][2]:.2f} lateral {moves[-1][3]:.2f}"
    finally:
        stop_app(app)


def _try(fn):
    try:
        return fn()
    except OSError:
        return None


def check_service_app(name, args, robot, node):
    app = run_app(name, args)
    client = node.create_client(Trigger, f"/{name}/start")
    stop = node.create_client(Trigger, f"/{name}/stop")
    try:
        if not client.wait_for_service(timeout_sec=30):
            return False, "no start service"
        start, message = time.monotonic(), ""
        for _ in range(40):
            future = client.call_async(Trigger.Request())
            rclpy.spin_until_future_complete(node, future, timeout_sec=5)
            if future.result() and future.result().success:
                break
            message = future.result().message if future.result() else "no reply"
            time.sleep(0.5)
        else:
            return False, f"start refused: {message[:300]}"
        moves = wait(lambda: walking(robot, start), 15)
        # Optionally let the app keep driving, to check closed-loop behaviour.
        time.sleep(float(os.environ.get("GO2_EXAMPLE_RUN_SECONDS", "0")))
        moves = walking(robot, start) or moves
        stop_future = stop.call_async(Trigger.Request())
        rclpy.spin_until_future_complete(node, stop_future, timeout_sec=5)
        if not moves:
            return False, f"started but no walking Move: {robot.moves_since(start)[:3]}"
        with robot.lock:
            position = (round(robot.x, 2), round(robot.y, 2))
        return True, f"{len(moves)} walking Moves, max forward {max(m[2] for m in moves):.2f}; robot moved to {position}"
    finally:
        stop_app(app)


CHECKS = {
    "sensors": check_sensors,
    "teleop": check_teleop,
    # Same arguments as each example's Dockerfile CMD.
    "patrol": lambda r, n: check_service_app("patrol", ["ros_app.py"], r, n),
    "roam": lambda r, n: check_service_app("roam", ["ros_app.py"], r, n),
    "hallway": lambda r, n: check_service_app("hallway", ["ros_app.py", "--ignore-capture-age"], r, n),
}


def main():
    names = sys.argv[1:] or list(CHECKS)
    socket = os.path.join(tempfile.mkdtemp(), "agent.sock")
    robot = FakeGo2()
    server = grpc.server(futures.ThreadPoolExecutor(max_workers=8))
    pb_grpc.add_WendyUnitreeRobotServiceServicer_to_server(robot, server)
    pairing_grpc.add_WendyUnitreeServiceServicer_to_server(robot, server)
    server.add_insecure_port("unix:" + socket)
    server.start()
    bridge_log = open("/tmp/bridge.log", "w")
    try:
        bridge = subprocess.Popen([sys.executable, "-m", "go2_bridge.node"],
                                  env={**os.environ, "WENDY_AGENT_SOCKET": socket},
                                  stdout=bridge_log, stderr=subprocess.STDOUT)
    except Exception:
        bridge_log.close()
        raise
    rclpy.init()
    node = rclpy.create_node("examples_check")
    results = {}
    try:
        time.sleep(3)
        for name in names:
            with robot.lock:  # each example starts from the room centre
                robot.x = robot.y = robot.yaw = 0.0
            results[name] = CHECKS[name](robot, node)
            print(f"{'PASS' if results[name][0] else 'FAIL'} {name}: {results[name][1]}", flush=True)
            time.sleep(1)
    finally:
        bridge.terminate()
        try:
            bridge.wait(timeout=5)
        except subprocess.TimeoutExpired:
            bridge.kill()
            bridge.wait()
        bridge_log.close()
        server.stop(0)
    sys.exit(0 if all(ok for ok, _ in results.values()) else 1)


if __name__ == "__main__":
    main()
