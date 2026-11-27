#!/usr/bin/env python3
"""Subscribe with a real ROS 2 node and print what arrives.

Prints one JSON object per line: {"ready": true} once the subscription
exists, one object per message, and a final {"graph": ...} object with what
the ROS 2 graph reports for the topic. Exits 0 after --count messages and 1
if they do not arrive within --timeout seconds. With --timing-topic it also
subscribes to that topic's diagnostic_msgs/msg/DiagnosticArray messages,
prints each as {"timing": ...}, and waits for --count of those too.
"""
import argparse
import fcntl
import json
import socket
import struct
import sys
import time

import rclpy
from diagnostic_msgs.msg import DiagnosticArray
from rclpy.qos import DurabilityPolicy, HistoryPolicy, QoSProfile, ReliabilityPolicy
from sensor_msgs.msg import Temperature
from std_msgs.msg import UInt8MultiArray


def bring_up_loopback():
    """Set IFF_UP on lo, as `ip link set lo up` does, without needing iproute2."""
    siocsifflags, iff_up = 0x8914, 0x1
    with socket.socket(socket.AF_INET, socket.SOCK_DGRAM) as s:
        fcntl.ioctl(s, siocsifflags, struct.pack('16sH', b'lo', iff_up))


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--topic', required=True)
    parser.add_argument('--type', choices=['temperature', 'bytes'], required=True)
    parser.add_argument('--reliability', choices=['reliable', 'best_effort'], required=True)
    parser.add_argument('--count', type=int, required=True)
    parser.add_argument('--timeout', type=float, required=True)
    parser.add_argument('--timing-topic',
                        help='also subscribe to how the time of each frame is known')
    parser.add_argument('--bring-up-loopback', action='store_true',
                        help='started in a new network namespace, whose loopback is down')
    args = parser.parse_args()
    if args.bring_up_loopback:
        bring_up_loopback()

    rclpy.init()
    node = rclpy.create_node('wendy_interop_subscriber')
    reliable = args.reliability == 'reliable'
    qos = QoSProfile(
        history=HistoryPolicy.KEEP_LAST, depth=100,
        reliability=ReliabilityPolicy.RELIABLE if reliable else ReliabilityPolicy.BEST_EFFORT,
        durability=DurabilityPolicy.VOLATILE)
    received = 0
    timings = 0

    def emit(record):
        print(json.dumps(record), flush=True)

    def on_temperature(msg):
        nonlocal received
        received += 1
        emit({'temperature': msg.temperature, 'variance': msg.variance,
              'frame_id': msg.header.frame_id,
              'sec': msg.header.stamp.sec, 'nanosec': msg.header.stamp.nanosec})

    def on_bytes(msg):
        nonlocal received
        received += 1
        emit({'data': list(msg.data), 'data_offset': msg.layout.data_offset,
              'dim': [[d.label, d.size, d.stride] for d in msg.layout.dim]})

    def on_timing(msg):
        nonlocal timings
        timings += 1
        status = msg.status[0] if msg.status else None
        emit({'timing': {
            'frame_id': msg.header.frame_id,
            'sec': msg.header.stamp.sec, 'nanosec': msg.header.stamp.nanosec,
            'statuses': len(msg.status),
            'level': int.from_bytes(status.level, 'little') if status else None,
            'name': status.name if status else None,
            'message': status.message if status else None,
            'hardware_id': status.hardware_id if status else None,
            'values': {v.key: v.value for v in status.values} if status else {}}})

    if args.timing_topic:
        node.create_subscription(DiagnosticArray, args.timing_topic, on_timing, qos)
    if args.type == 'temperature':
        node.create_subscription(Temperature, args.topic, on_temperature, qos)
    else:
        node.create_subscription(UInt8MultiArray, args.topic, on_bytes, qos)
    emit({'ready': True})

    deadline = time.monotonic() + args.timeout
    wanted_timings = args.count if args.timing_topic else 0

    def done():
        return received >= args.count and timings >= wanted_timings

    while rclpy.ok() and not done() and time.monotonic() < deadline:
        rclpy.spin_once(node, timeout_sec=0.1)
    names_and_types = dict(node.get_topic_names_and_types())
    graph = {'types': names_and_types.get(args.topic, []), 'publishers': node.count_publishers(args.topic)}
    if args.timing_topic:
        graph['timing_types'] = names_and_types.get(args.timing_topic, [])
        graph['timing_publishers'] = node.count_publishers(args.timing_topic)
    emit({'graph': graph})
    node.destroy_node()
    rclpy.shutdown()
    sys.exit(0 if done() else 1)


if __name__ == '__main__':
    main()
