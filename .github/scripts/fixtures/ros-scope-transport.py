#!/usr/bin/env python3
"""Fresh DDS payloads prove visibility; graph names alone cannot pass."""
import argparse
import json
import time
import rclpy
from rclpy.executors import ExternalShutdownException
from std_msgs.msg import String

p = argparse.ArgumentParser()
p.add_argument('mode', choices=['publish', 'observe'])
p.add_argument('--role', default='')
p.add_argument('--nonce', required=True)
p.add_argument('--seconds', type=float, default=6)
a = p.parse_args()
rclpy.init()
node = rclpy.create_node('scope_'+a.mode+'_'+(a.role or 'candidate'))
try:
    if a.mode == 'publish':
        publisher = node.create_publisher(String, '/wendy_scope/'+a.role, 10)
        sequence = [0]
        def publish():
            sequence[0] += 1
            message = String()
            message.data = json.dumps({'role': a.role, 'nonce': a.nonce, 'sequence': sequence[0]})
            publisher.publish(message)
        node.create_timer(0.05, publish)
        try:
            rclpy.spin(node)
        except ExternalShutdownException:
            pass
    else:
        seen = {name: set() for name in ('device', 'peripheral', 'uplink')}
        def receive(role, message):
            value = json.loads(message.data)
            if value['nonce'] == a.nonce and value['role'] == role:
                seen[role].add(value['sequence'])
        for role in seen:
            node.create_subscription(String, '/wendy_scope/'+role,
                lambda message, role=role: receive(role, message), 10)
        end = time.monotonic()+a.seconds
        while time.monotonic() < end:
            rclpy.spin_once(node, timeout_sec=0.05)
        print(json.dumps({name: sorted(values) for name, values in seen.items()}), flush=True)
finally:
    node.destroy_node()
    rclpy.try_shutdown()
