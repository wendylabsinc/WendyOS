"""Independent live ROS checks for the named, disposable Unitree VM."""

import json
import math
import shlex


PROBE = '''import json, time
import rclpy
from nav_msgs.msg import Odometry
from sensor_msgs.msg import JointState
from rclpy.qos import qos_profile_sensor_data
rclpy.init()
node = rclpy.create_node("wendy_eval_independent_probe")
samples = {"odom": [], "joints": []}
def receive(kind, msg):
    stamp = msg.header.stamp.sec + msg.header.stamp.nanosec / 1e9
    row = {"stamp": stamp, "age": time.time() - stamp, "frame": msg.header.frame_id}
    if kind == "odom":
        p, q = msg.pose.pose.position, msg.pose.pose.orientation
        row.update(position=[p.x,p.y,p.z], orientation=[q.x,q.y,q.z,q.w], child=msg.child_frame_id)
    else:
        row.update(names=list(msg.name), positions=list(msg.position))
    if len(samples[kind]) < 5:
        samples[kind].append(row)
node.create_subscription(Odometry, ODOM_TOPIC, lambda m: receive("odom",m), qos_profile_sensor_data)
node.create_subscription(JointState, "/joint_states", lambda m: receive("joints",m), qos_profile_sensor_data)
deadline = time.monotonic() + 30
while time.monotonic() < deadline and any(len(v) < 5 for v in samples.values()):
    rclpy.spin_once(node, timeout_sec=.2)
print(json.dumps(samples, allow_nan=False))
node.destroy_node()
rclpy.shutdown()
'''


def validate_samples(samples, kind):
    for channel in ("odom", "joints"):
        rows = samples.get(channel, [])
        if len(rows) < 5:
            raise RuntimeError(f"missing fresh {channel} samples")
        stamps = [row["stamp"] for row in rows]
        if not all(math.isfinite(s) for s in stamps) or any(b <= a for a, b in zip(stamps, stamps[1:])):
            raise RuntimeError(f"{channel} timestamps did not advance")
        if any(not math.isfinite(row["age"]) or not -.5 <= row["age"] <= 5 for row in rows):
            raise RuntimeError(f"{channel} data is stale")
        for row in rows:
            if channel == "odom":
                values = row["position"] + row["orientation"]
                if row["frame"] != "odom" or row["child"] != "base_link":
                    raise RuntimeError("wrong odometry frames")
                if abs(sum(v * v for v in row["orientation"]) - 1) > .02:
                    raise RuntimeError("odometry quaternion is not normalized")
            else:
                expected = 12 if kind == "go2" else 29
                values = row["positions"]
                if len(set(row["names"])) != expected or len(values) != expected:
                    raise RuntimeError(f"expected {expected} distinct {kind} joints")
            if not all(isinstance(v, (int, float)) and math.isfinite(v) for v in values):
                raise RuntimeError(f"non-finite {channel} data")


def verify(args, cli):
    kind = "go2" if args.task == "create-go2-simulator" else "g1"
    state = json.loads(cli(args, "--json", "vm", "robot", "status", args.vm_name))
    expected = {"vm_name": args.vm_name, "simulation": True, "robot_kind": kind,
                "healthy": True, "ready": True, "dds_isolation": "udp-rtps-loopback"}
    if any(state.get(k) != v for k, v in expected.items()):
        raise RuntimeError(f"wrong or unhealthy robot runtime: {state}")
    if not state.get("source_digest") or not state.get("policy_bundle"):
        raise RuntimeError("robot runtime lacks pinned source/policy identity")
    topic = "/utlidar/robot_odom" if kind == "go2" else "/odom"
    code = "ODOM_TOPIC = " + repr(topic) + "\n" + PROBE
    prefix = f"/opt/wendy-{kind}"
    command = (f"source /opt/ros/humble/setup.bash && source {prefix}/ros_ws/install/setup.bash && "
               f"export ROS_DOMAIN_ID=0 ROS_LOCALHOST_ONLY=0 RMW_IMPLEMENTATION=rmw_cyclonedds_cpp "
               f"CYCLONEDDS_URI=file://{prefix}/cyclonedds.xml && " + shlex.join(["python3", "-c", code]))
    raw = cli(args, "--device", "vm:" + args.vm_name, "device", "attach", f"sh.wendy.simulator.{kind}",
              "--", "bash", "-c", command, timeout=60)
    samples = json.loads(raw)
    validate_samples(samples, kind)
    print(json.dumps({"robot": state, "ros_samples": samples}))
