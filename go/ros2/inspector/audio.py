#!/usr/bin/env python3
"""Inspect or publish native Unitree AudioData packets without assuming a codec."""

import argparse
import base64
import hashlib
import json
import re
import sys
import time

MAX_PACKET_BYTES = 1024 * 1024
MAX_PUBLISH_BYTES = 64 * 1024  # Base64 must fit Linux's per-argument exec limit.


def topic(value):
    if len(value) > 255 or not re.fullmatch(r"/([A-Za-z_][A-Za-z0-9_]*)(/[A-Za-z_][A-Za-z0-9_]*)*", value):
        raise argparse.ArgumentTypeError("expected an absolute ROS topic name")
    return value


def bounded_int(lower, upper):
    def parse(value):
        number = int(value)
        if not lower <= number <= upper:
            raise argparse.ArgumentTypeError(f"expected an integer in {lower}..{upper}")
        return number
    return parse


def decode_packet(value):
    if len(value) > 4 * ((MAX_PUBLISH_BYTES + 2) // 3):
        raise ValueError("audio publication exceeds 64 KiB")
    data = base64.b64decode(value, validate=True)
    if not data or len(data) > MAX_PUBLISH_BYTES:
        raise ValueError("audio publication must contain 1..65536 bytes")
    return data


def summarize(message, topic_name, include_data=False):
    if len(message.data) > MAX_PACKET_BYTES:
        raise ValueError("received audio packet exceeds 1 MiB")
    data = bytes(message.data)
    result = {"schema_version": 1, "topic": topic_name,
              "message_type": "unitree_go/msg/AudioData",
              "time_frame": message.time_frame, "byte_count": len(data),
              "sha256": hashlib.sha256(data).hexdigest(),
              "preview_base64": base64.b64encode(data[:32]).decode(),
              "encoding": "unknown", "source_freshness": "unknown"}
    if include_data:
        result["data_base64"] = base64.b64encode(data).decode()
    return result


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    sub = parser.add_subparsers(dest="command", required=True)
    sample = sub.add_parser("sample", help="Read bounded packet metadata; optionally include lossless bytes")
    sample.add_argument("topic", type=topic)
    sample.add_argument("--count", type=bounded_int(1, 100), default=3)
    sample.add_argument("--include-data", action="store_true")
    publish = sub.add_parser("publish", help="Publish one already-encoded native packet")
    publish.add_argument("topic", type=topic)
    publish.add_argument("--time-frame", type=bounded_int(0, 2**64 - 1), required=True)
    publish.add_argument("--data-base64", required=True)
    for command in (sample, publish):
        command.add_argument("--duration", type=bounded_int(1, 60), default=10)
    args = parser.parse_args(argv)
    data = decode_packet(args.data_base64) if args.command == "publish" else None

    import rclpy
    from rclpy.qos import QoSProfile, ReliabilityPolicy
    from unitree_go.msg import AudioData

    rclpy.init(args=[])
    node = rclpy.create_node("wendy_unitree_audio", enable_rosout=False)
    try:
        deadline = time.monotonic() + args.duration
        if args.command == "sample":
            received = 0

            def observe(message):
                nonlocal received
                print(json.dumps(summarize(message, args.topic, args.include_data)), flush=True)
                received += 1

            subscription = node.create_subscription(AudioData, args.topic, observe,
                QoSProfile(depth=5, reliability=ReliabilityPolicy.BEST_EFFORT))
            while received < args.count and time.monotonic() < deadline:
                rclpy.spin_once(node, timeout_sec=min(0.1, max(0, deadline - time.monotonic())))
            node.destroy_subscription(subscription)
            if received < args.count:
                print(f"audio sample timed out after {received}/{args.count} packets", file=sys.stderr)
                return 2
        else:
            publisher = node.create_publisher(AudioData, args.topic,
                QoSProfile(depth=1, reliability=ReliabilityPolicy.RELIABLE))
            while publisher.get_subscription_count() == 0 and time.monotonic() < deadline:
                rclpy.spin_once(node, timeout_sec=0.1)
            if publisher.get_subscription_count() == 0:
                print("no matching AudioData subscriber; no packet published", file=sys.stderr)
                return 2
            message = AudioData(time_frame=args.time_frame, data=data)
            publisher.publish(message)
            # Keep the writer alive while DDS sends the packet. This does not
            # assert that the robot decoded it or played sound.
            from rclpy.duration import Duration
            if not publisher.wait_for_all_acked(Duration(seconds=max(0.01, deadline - time.monotonic()))):
                print("audio publication acknowledgement timed out", file=sys.stderr)
                return 2
            print(json.dumps({"status": "published", "topic": args.topic,
                              "time_frame": args.time_frame, "byte_count": len(data)}))
        return 0
    finally:
        node.destroy_node()
        rclpy.shutdown()


if __name__ == "__main__":
    try:
        sys.exit(main())
    except (ValueError, RuntimeError) as error:
        print(str(error), file=sys.stderr)
        sys.exit(1)
