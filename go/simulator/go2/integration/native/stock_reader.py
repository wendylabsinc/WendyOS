"""Read derived DDS fixtures using separately built, unmodified ROS messages."""

import json
import time

from common import require, require_isolated_network

require_isolated_network()
import rclpy
from rclpy.node import Node
from unitree_api.msg import Response
from unitree_go.msg import SportModeState

require(not hasattr(SportModeState(), "path_point"), "Stock reader accidentally loaded derived ROS types")
rclpy.init()
node = Node("wendy_native_stock_reader")
counts, values = {"sport": 0, "response": 0}, {}


def observed(kind, message):
    counts[kind] += 1
    values[kind] = message.body_height if kind == "sport" else list(message.binary)


subscriptions = [node.create_subscription(SportModeState, "/native_pair/sport", lambda msg: observed("sport", msg), 10),
                 node.create_subscription(Response, "/native_pair/response", lambda msg: observed("response", msg), 10)]
print(json.dumps({"ready": True}), flush=True)
deadline = time.monotonic() + 7
while time.monotonic() < deadline:
    rclpy.spin_once(node, timeout_sec=0.02)
print(json.dumps({"stock_received": counts, "stock_values": values}), flush=True)
node.destroy_node()
rclpy.shutdown()
