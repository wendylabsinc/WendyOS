"""Probe installed ROS/SDK type pairs in a loopback-only Docker namespace.

This test publishes synthetic interface fixtures on /native_pair/*, never robot
command topics. ROS and SDK run in separate processes with their real types.
"""

import argparse
import json
from pathlib import Path
import subprocess
import sys
import threading
import time

from common import require, require_isolated_network


def ros_worker():
    require_isolated_network()
    import rclpy
    from rclpy.node import Node
    from unitree_api.msg import Request, Response
    from unitree_go.msg import LowCmd, LowState, SportModeState

    rclpy.init()
    node = Node("wendy_native_type_pairs_ros")
    counts = {"request": 0, "lowcmd": 0}
    def observed(kind, message):
        counts[kind] += 1
    subscriptions = [node.create_subscription(Request, "/native_pair/request", lambda msg: observed("request", msg), 10),
                     node.create_subscription(LowCmd, "/native_pair/lowcmd", lambda msg: observed("lowcmd", msg), 10)]
    publishers = [node.create_publisher(Response, "/native_pair/response", 10),
                  node.create_publisher(LowState, "/native_pair/lowstate", 10),
                  node.create_publisher(SportModeState, "/native_pair/sport", 10)]
    response, low, sport = Response(), LowState(), SportModeState()
    response.header.identity.id, response.header.identity.api_id = 17, 1
    response.data = "native-pair"
    response.binary = [0, 127, 128, 255] if Response.get_fields_and_field_types()["binary"] == "sequence<uint8>" else [0, 127, -128, -1]
    low.head = [0xFE, 0xEF]
    low.motor_state[3].q = 0.125
    sport.mode, sport.body_height = 3, 0.321
    node.create_timer(0.05, lambda: [publisher.publish(message) for publisher, message in zip(publishers, (response, low, sport))])
    print(json.dumps({"ready": True, "derived_ros_types": hasattr(sport, "path_point")}), flush=True)
    deadline = time.monotonic() + 10.0
    while time.monotonic() < deadline:
        rclpy.spin_once(node, timeout_sec=0.02)
    print(json.dumps({"ros_received": counts}), flush=True)
    node.destroy_node()
    rclpy.shutdown()


def probe():
    require_isolated_network()
    worker = subprocess.Popen([sys.executable, str(Path(__file__)), "--ros-worker"],
                              stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
    stock = None
    try:
        line = worker.stdout.readline()
        ready = json.loads(line)
        require(ready.get("ready") is True, f"ROS type-pair worker did not start: {line}")
        stock = subprocess.Popen(["/bin/bash", "-c", "unset AMENT_PREFIX_PATH CMAKE_PREFIX_PATH COLCON_PREFIX_PATH PYTHONPATH LD_LIBRARY_PATH; source /opt/ros/humble/setup.bash; source /app/stock_ros/install/setup.bash; exec python3 /app/native/stock_reader.py"],
                                 stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
        require(json.loads(stock.stdout.readline()).get("ready") is True, "Stock ROS reader did not start")
        from unitree_sdk2py.core.channel import ChannelFactoryInitialize, ChannelPublisher, ChannelSubscriber
        from unitree_sdk2py.idl.unitree_api.msg.dds_ import Request_, RequestHeader_, RequestIdentity_, RequestLease_, RequestPolicy_, Response_
        from unitree_sdk2py.idl.unitree_go.msg.dds_ import LowCmd_, LowState_, SportModeState_
        from unitree_sdk2py.idl.default import unitree_go_msg_dds__LowCmd_
        ChannelFactoryInitialize(0, "lo")
        counts, latest = {"response": 0, "lowstate": 0, "sport": 0}, {}
        lock = threading.Lock()
        def observe(kind, message):
            with lock:
                counts[kind] += 1
                latest[kind] = message
        subscribers = []
        for kind, datatype in (("response", Response_), ("lowstate", LowState_), ("sport", SportModeState_)):
            subscriber = ChannelSubscriber(f"rt/native_pair/{kind}", datatype)
            subscriber.Init(lambda message, key=kind: observe(key, message), 1)
            subscribers.append(subscriber)
        request_pub = ChannelPublisher("rt/native_pair/request", Request_)
        low_pub = ChannelPublisher("rt/native_pair/lowcmd", LowCmd_)
        request_pub.Init()
        low_pub.Init()
        request = Request_(RequestHeader_(RequestIdentity_(17, 1), RequestLease_(0), RequestPolicy_(0, False)), "{}", [])
        low = unitree_go_msg_dds__LowCmd_()
        deadline = time.monotonic() + 5.0
        while time.monotonic() < deadline:
            request_pub.Write(request)
            low_pub.Write(low)
            time.sleep(0.05)
        stdout, stderr = worker.communicate(timeout=8.0)
        ros_counts = next(json.loads(item)["ros_received"] for item in stdout.splitlines() if '"ros_received"' in item)
        stock_out, stock_err = stock.communicate(timeout=3.0)
        stock_result = next(json.loads(item) for item in stock_out.splitlines() if '"stock_received"' in item)
        if stderr:
            print(stderr, file=sys.stderr)
        if stock_err:
            print(stock_err, file=sys.stderr)
        result = {
            "isolation": "Docker network namespace contains only lo",
            "sdk_source_types_modified": False,
            "ros_compatibility_overlay": ready["derived_ros_types"],
            "request_sdk_to_ros": {"received": ros_counts["request"], "passed": ros_counts["request"] > 0},
            "lowcmd_sdk_to_ros": {"received": ros_counts["lowcmd"], "passed": ros_counts["lowcmd"] > 0},
            "response_ros_to_sdk": {"received": counts["response"], "passed": counts["response"] > 0},
            "lowstate_ros_to_sdk": {"received": counts["lowstate"], "passed": counts["lowstate"] > 0},
            "sport_ros_to_sdk": {"received": counts["sport"], "passed": counts["sport"] > 0},
            "derived_sport_to_stock_ros": {"received": stock_result["stock_received"]["sport"],
                "passed": stock_result["stock_received"]["sport"] > 0 and abs(stock_result["stock_values"].get("sport", 0) - 0.321) < 1e-5},
            "derived_response_to_stock_ros": {"received": stock_result["stock_received"]["response"],
                "passed": stock_result["stock_values"].get("response") == [0, 127, -128, -1],
                "binary_signed_interpretation": stock_result["stock_values"].get("response")},
        }
        if counts["response"]:
            result["response_ros_to_sdk"]["passed"] &= latest["response"].data == "native-pair" and list(latest["response"].binary) == [0, 127, 128, 255]
        if counts["lowstate"]:
            result["lowstate_ros_to_sdk"]["passed"] &= abs(latest["lowstate"].motor_state[3].q - 0.125) < 1e-6
        if counts["sport"]:
            result["sport_ros_to_sdk"]["passed"] &= abs(latest["sport"].body_height - 0.321) < 1e-5
            result["sport_ros_to_sdk"]["path_points_received"] = len(latest["sport"].path_point)
        result["passed"] = all(value["passed"] for value in result.values() if isinstance(value, dict))
        print(json.dumps(result), flush=True)
        return 0 if result["passed"] else 1
    finally:
        if worker.poll() is None:
            worker.terminate()
            worker.wait(timeout=3)
        if stock is not None and stock.poll() is None:
            stock.terminate()
            stock.wait(timeout=3)


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--ros-worker", action="store_true")
    args = parser.parse_args()
    if args.ros_worker:
        ros_worker()
    else:
        sys.exit(probe())
