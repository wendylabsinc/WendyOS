"""Actual generated ROS Request/Response client, isolated from SDK DDS state."""

import argparse
import json
import sys
import threading
import time

from common import SimulatorAPI, require_test_environment


def main(url, vm_name=None, source_digest=None):
    require_test_environment(SimulatorAPI(url, vm_name=vm_name, source_digest=source_digest))
    # Identity precedes rclpy initialization.
    import rclpy
    from rclpy.executors import SingleThreadedExecutor
    from rclpy.node import Node
    from unitree_api.msg import Request, Response

    rclpy.init()
    node = Node("wendy_go2_native_ros_requests")
    responses, condition = {}, threading.Condition()
    def receive(service, message):
        with condition:
            responses[message.header.identity.id] = {
                "service": service, "id": message.header.identity.id,
                "api": message.header.identity.api_id,
                "code": message.header.status.code, "data": message.data,
            }
            condition.notify_all()
    publishers = {service: node.create_publisher(Request, f"/api/{service}/request", 10)
                  for service in ("sport", "motion_switcher")}
    subscriptions = [node.create_subscription(Response, f"/api/{service}/response",
                     lambda message, name=service: receive(name, message), 10) for service in publishers]
    streaming = threading.Event()
    def request(api, parameter, noreply=False, priority=0, lease=0):
        message = Request()
        message.header.identity.id = time.monotonic_ns()
        message.header.identity.api_id = api
        message.header.policy.noreply = noreply
        message.header.policy.priority = priority
        message.header.lease.id = lease
        message.parameter = parameter
        return message
    def send_zero():
        if streaming.is_set():
            publishers["sport"].publish(request(1008, '{"x":0,"y":0,"z":0}', True))
    node.create_timer(0.05, send_zero)
    executor = SingleThreadedExecutor()
    executor.add_node(node)
    thread = threading.Thread(target=executor.spin, daemon=True)
    thread.start()
    print(json.dumps({"ready": True}), flush=True)
    try:
        for line in sys.stdin:
            value = json.loads(line)
            if value["op"] == "stream":
                streaming.set() if value["enabled"] else streaming.clear()
                result = {"streaming": streaming.is_set()}
            elif value["op"] == "request":
                message = request(value["api"], value.get("parameter", "{}"), value.get("noreply", False),
                                  value.get("priority", 0), value.get("lease", 0))
                service = value.get("service", "sport")
                publishers[service].publish(message)
                deadline = time.monotonic() + (0.3 if message.header.policy.noreply else 2.0)
                with condition:
                    while message.header.identity.id not in responses and time.monotonic() < deadline:
                        condition.wait(max(0, deadline - time.monotonic()))
                    result = {"request_id": message.header.identity.id, "request_api": value["api"],
                              "response": responses.pop(message.header.identity.id, None)}
            elif value["op"] == "close":
                break
            else:
                raise ValueError("unknown ROS worker operation")
            print(json.dumps(result), flush=True)
    finally:
        streaming.clear()
        executor.shutdown()
        thread.join(timeout=2)
        node.destroy_node()
        rclpy.shutdown()


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--url", required=True)
    parser.add_argument("--managed-vm")
    parser.add_argument("--source-digest")
    args = parser.parse_args()
    main(args.url, args.managed_vm, args.source_digest)
