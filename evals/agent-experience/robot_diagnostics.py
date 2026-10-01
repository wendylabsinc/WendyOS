#!/usr/bin/env python3
"""Seed and grade ROS consumer and virtual-camera faults in owned robot VMs."""

import argparse
import hashlib
import json
from pathlib import Path
import re
import secrets
import shlex
import socket
import subprocess
import sys
import time
import urllib.request
from urllib.parse import urlsplit

import fixtures as f
import robot_fixtures as robot


CONSUMER = '''import collections,json,threading,time
from http.server import BaseHTTPRequestHandler,ThreadingHTTPServer
from pathlib import Path
import rclpy
from nav_msgs.msg import Odometry
from rclpy.qos import qos_profile_sensor_data
s=json.loads(Path("settings.json").read_text())
samples=collections.deque(maxlen=20)
count=0
def receive(m):
    global count
    p=m.pose.pose.position
    count+=1
    samples.append({"stamp":m.header.stamp.sec+m.header.stamp.nanosec/1e9,
        "frame":m.header.frame_id,"child":m.child_frame_id,"position":[p.x,p.y,p.z]})
class Handler(BaseHTTPRequestHandler):
    def do_GET(self):
        value={k:s[k] for k in ("app_id","version","nonce")}
        if self.path=="/observations": value.update(count=count,samples=list(samples),now=time.time())
        data=json.dumps(value).encode()
        self.send_response(200);self.send_header("Content-Length",str(len(data)));self.end_headers();self.wfile.write(data)
rclpy.init()
node=rclpy.create_node("eval_odom_consumer")
node.create_subscription(Odometry,s["topic"],receive,qos_profile_sensor_data)
threading.Thread(target=ThreadingHTTPServer(("0.0.0.0",s["port"]),Handler).serve_forever,daemon=True).start()
rclpy.spin(node)
'''

CAMERA_PROBE = '''import hashlib,json,time
import rclpy
from sensor_msgs.msg import Image
from rclpy.qos import qos_profile_sensor_data
rclpy.init()
n=rclpy.create_node("eval_camera_probe")
samples=[]
def receive(m):
    data=bytes(m.data)
    stamp=m.header.stamp.sec+m.header.stamp.nanosec/1e9
    samples.append({"stamp":stamp,"age":time.time()-stamp,"width":m.width,"height":m.height,
        "encoding":m.encoding,"frame":m.header.frame_id,"step":m.step,"bytes":len(data),
        "min":min(data) if data else 0,"max":max(data) if data else 0,"sha256":hashlib.sha256(data).hexdigest()})
n.create_subscription(Image,"/camera/color/image_raw",receive,qos_profile_sensor_data)
deadline=time.monotonic()+SECONDS
while len(samples)<5 and time.monotonic()<deadline:rclpy.spin_once(n,timeout_sec=.2)
print(json.dumps(samples));n.destroy_node();rclpy.shutdown()
'''


def status(args):
    value = json.loads(f.cli(args, "--json", "vm", "robot", "status", args.vm_name))
    if any(value.get(k) != v for k, v in {"vm_name": args.vm_name, "robot_kind": args.kind,
            "simulation": True, "healthy": True, "ready": True, "dds_isolation": "udp-rtps-loopback"}.items()):
        raise RuntimeError(f"wrong or unready robot: {value}")
    url = urlsplit(value["sandbox_url"])
    if url.scheme != "http" or url.hostname != "127.0.0.1" or not url.port:
        raise RuntimeError("robot sandbox must be the named VM's loopback endpoint")
    return value


def post(args, path, body):
    url = status(args)["sandbox_url"] + path
    request = urllib.request.Request(url, json.dumps(body).encode(), {"Content-Type": "application/json"})
    with urllib.request.urlopen(request, timeout=10) as response:
        return json.load(response)


def camera_samples(args, seconds=20):
    code = "SECONDS=" + str(seconds) + "\n" + CAMERA_PROBE
    base = f"/opt/wendy-{args.kind}"
    command = (f"source /opt/ros/humble/setup.bash && source {base}/ros_ws/install/setup.bash && "
               f"export ROS_DOMAIN_ID=0 ROS_LOCALHOST_ONLY=0 RMW_IMPLEMENTATION=rmw_cyclonedds_cpp "
               f"CYCLONEDDS_URI=file://{base}/cyclonedds.xml && " + shlex.join(["python3", "-c", code]))
    return json.loads(f.cli(args, "--device", args.device, "device", "attach", f"sh.wendy.simulator.{args.kind}",
                            "--", "bash", "-c", command, timeout=seconds + 30))


def validate_camera(samples):
    if len(samples) < 5:
        raise RuntimeError("camera did not deliver five fresh frames")
    previous = 0
    for row in samples:
        if not -.5 <= row["age"] <= 5 or row["stamp"] <= previous:
            raise RuntimeError("camera timestamps are stale or do not advance")
        previous = row["stamp"]
        if (row["width"], row["height"], row["encoding"], row["frame"]) != (640, 360, "rgb8", "camera_optical_frame"):
            raise RuntimeError("unexpected virtual camera format or frame")
        if row["step"] != 1920 or row["bytes"] != 640*360*3 or row["max"] - row["min"] < 10:
            raise RuntimeError("invalid or blank RGB camera data")


def observations(args):
    code = f"import json,urllib.request; print(urllib.request.urlopen('http://127.0.0.1:{args.port}/observations',timeout=5).read().decode())"
    return json.loads(f.cli(args, "--device", args.device, "device", "attach", args.app_id,
                            "--", "python3", "-c", code))


def prepare(args):
    f.prepare(args)
    f.cli(args, "vm", "--yes", "create", args.vm_name, "--profile", args.kind, timeout=600)
    with socket.socket() as sock:
        sock.bind(("127.0.0.1", 0))
        port = sock.getsockname()[1]
    f.cli(args, "vm", "--yes", "start", args.vm_name, "--detach", "--port", str(port), timeout=180)
    f.cli(args, "--device", args.device, "device", "update", "--binary", args.agent_binary, timeout=180)
    f.cli(args, "vm", "--yes", "robot", "start", args.vm_name, timeout=1800)
    robot.verify(args, f.cli)
    if args.case == "camera":
        validate_camera(camera_samples(args))
        post(args, "/api/sensors", {"camera_enabled": False})
        if camera_samples(args, seconds=5):
            raise RuntimeError("camera fault was not reproduced")
        return
    f.fixture(args)
    Path("app.py").write_text(CONSUMER)
    settings = json.loads(Path("settings.json").read_text())
    settings["topic"] = "/eval/missing_odometry"
    Path("settings.json").write_text(json.dumps(settings, indent=2))
    manifest = json.loads(Path("wendy.json").read_text())
    manifest["frameworks"] = {"ros2": {"distro": "humble", "rmw": "rmw_cyclonedds_cpp",
                                       "domainId": 0, "discoveryScope": "host"}}
    Path("wendy.json").write_text(json.dumps(manifest, indent=2))
    Path("Dockerfile").write_text('FROM ros:humble-ros-base\nRUN apt-get update && apt-get install -y --no-install-recommends ros-humble-rclpy ros-humble-nav-msgs ros-humble-rmw-cyclonedds-cpp && rm -rf /var/lib/apt/lists/*\nWORKDIR /app\nCOPY app.py settings.json ./\nCMD ["python3","-u","app.py"]\n')
    f.cli(args, "run", "--yes", "--detach", "--no-restart", "--device", args.device, timeout=900)
    f.wait_health(args, "v1")
    time.sleep(3)
    before = observations(args)
    if before["count"] != 0:
        raise RuntimeError("ROS consumer fault was not reproduced")
    print(json.dumps({"broken_consumer": before}))


def verify(args):
    # This checks passive readiness before any device connection can reconcile.
    f.verify(args)
    if args.case == "camera":
        frames = []
        positions = [[2 + secrets.randbelow(100) / 1000, sign * (.6 + secrets.randbelow(200) / 1000)]
                     for sign in (-1, 1)]
        for position in positions:
            post(args, "/api/obstacle", {"position": position})
            samples = camera_samples(args)
            validate_camera(samples)
            frames.append(samples)
        if {s["sha256"] for s in frames[0]} & {s["sha256"] for s in frames[1]}:
            raise RuntimeError("camera replayed pixels across a changed visual stimulus")
        print(json.dumps({"camera_frames": frames, "stimulus": positions}))
        return
    first = observations(args)
    code = "import hashlib; print(hashlib.sha256(open('/app/app.py','rb').read()).hexdigest())"
    actual = f.cli(args, "--device", args.device, "device", "attach", args.app_id, "--", "python3", "-c", code).strip()
    if actual != hashlib.sha256(CONSUMER.encode()).hexdigest():
        raise RuntimeError("the reference ROS consumer was replaced instead of repairing its configuration")
    time.sleep(2)
    second = observations(args)
    if (first["count"] < 5 or second["count"] <= first["count"] or second.get("nonce") != args.run_id
            or second.get("app_id") != args.app_id):
        raise RuntimeError("consumer did not process fresh robot observations")
    rows = second["samples"]
    if len(rows) < 5:
        raise RuntimeError("too few consumer observations")
    previous = 0
    for row in rows:
        if row["frame"] != "odom" or row["child"] != "base_link" or row["stamp"] <= previous:
            raise RuntimeError("consumer has wrong frames or stale timestamps")
        if not -.5 <= second["now"] - row["stamp"] <= 5:
            raise RuntimeError("consumer received stale data")
        if not all(isinstance(v, (int, float)) and abs(v) < 100 for v in row["position"]):
            raise RuntimeError("invalid odometry payload")
        previous = row["stamp"]
    print(json.dumps({"consumer_first": first, "consumer_second": second}))


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("phase", choices=["prepare", "verify", "cleanup", "smoke"])
    parser.add_argument("kind", choices=["go2", "g1"])
    parser.add_argument("case", choices=["ros2", "camera"])
    parser.add_argument("--wendy", required=True)
    parser.add_argument("--agent-binary", required=True)
    parser.add_argument("--run-id", required=True)
    parser.add_argument("--port", type=int, default=18765)
    args = parser.parse_args()
    if not re.fullmatch("[a-f0-9]{12}", args.run_id):
        parser.error("invalid run ID")
    args.vm_name = "wendy-eval-" + args.run_id
    args.device = "vm:" + args.vm_name
    args.app_id = "dev.wendy.eval." + args.run_id
    args.task = "create-" + args.kind + "-simulator"
    args.state = Path.cwd().parent / ("wendy-eval-state-" + args.run_id + ".json")
    try:
        if args.phase == "smoke":
            try:
                prepare(args)
                if args.case == "camera":
                    post(args, "/api/sensors", {"camera_enabled": True})
                else:
                    settings = json.loads(Path("settings.json").read_text())
                    settings["topic"] = "/utlidar/robot_odom" if args.kind == "go2" else "/odom"
                    Path("settings.json").write_text(json.dumps(settings))
                    f.cli(args, "run", "--yes", "--detach", "--no-restart", "--device", args.device, timeout=900)
                verify(args)
            finally:
                f.cleanup(args)
        elif args.phase == "cleanup":
            f.cleanup(args)
        else:
            globals()[args.phase](args)
    except (RuntimeError, ValueError, subprocess.TimeoutExpired) as exc:
        print(str(exc), file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
