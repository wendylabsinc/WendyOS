"""Finite read-only ROS observer for managed Go2 VM lifecycle acceptance.

Deploy with --no-restart. The application only GETs simulator status and
subscribes to /odom; it never grants control, publishes commands or resets.
"""

import argparse
import json
import math
import os
import time
from urllib.parse import urlsplit
from urllib.request import HTTPRedirectHandler, ProxyHandler, Request, build_opener

import rclpy
from nav_msgs.msg import Odometry
from rclpy.node import Node
from rclpy.qos import QoSProfile, ReliabilityPolicy


def require(condition, message):
    if not condition:
        raise RuntimeError(message)


def report(event, **values):
    print(json.dumps({"event": event, **values}, allow_nan=False), flush=True)


class NoRedirects(HTTPRedirectHandler):
    def redirect_request(self, *args, **kwargs):
        raise RuntimeError("simulator status redirected")


class StatusReader:
    def __init__(self, url, expected_vm, expected_source):
        parsed = urlsplit(url)
        require(parsed.scheme == "http" and parsed.hostname in {"127.0.0.1", "::1"}
                and parsed.path in {"", "/"} and not parsed.username and not parsed.password
                and not parsed.query and not parsed.fragment, "expected a loopback simulator URL")
        self.url = url.rstrip("/") + "/api/status"
        self.expected_vm = expected_vm
        self.expected_source = expected_source
        self.http = build_opener(ProxyHandler({}), NoRedirects())

    def read(self):
        with self.http.open(Request(self.url, method="GET"), timeout=3) as response:
            require(response.status == 200, "simulator status is unavailable")
            raw = response.read((1 << 20) + 1)
        require(len(raw) <= 1 << 20, "simulator status exceeded size limit")
        state = json.loads(raw)
        require(state.get("simulation") is True and state.get("robot") == "go2"
                and state.get("robot_kind") == "go2" and type(state.get("profile_version")) is int
                and state["profile_version"] == 1 and state.get("clock_mode") == "device"
                and state.get("dds_isolation") == "udp-rtps-loopback",
                "endpoint is not a managed Go2 virtual robot")
        require(state.get("healthy") is True and state.get("ready") is True,
                "virtual robot is not healthy and ready")
        if self.expected_vm:
            require(state.get("vm_name") == self.expected_vm, "unexpected VM identity")
        if self.expected_source:
            require(state.get("source_digest") == self.expected_source, "unexpected runtime source")
        return state


class Observer(Node):
    def __init__(self):
        super().__init__("wendy_go2_lifecycle_observer", enable_rosout=False, start_parameter_services=False)
        self.samples = 0
        self.fresh_samples = 0
        self.first_stamp_ns = None
        self.last_stamp_ns = None
        self.last_receipt = None
        self.subscription = self.create_subscription(
            Odometry, "/odom", self.observe,
            QoSProfile(depth=5, reliability=ReliabilityPolicy.BEST_EFFORT))

    def observe(self, message):
        stamp = message.header.stamp.sec * 1_000_000_000 + message.header.stamp.nanosec
        self.samples += 1
        self.last_receipt = time.monotonic()
        if self.first_stamp_ns is None:
            self.first_stamp_ns = stamp
        self.last_stamp_ns = stamp
        age = (time.time_ns() - stamp) / 1_000_000_000
        if -0.1 <= age <= 1.0:
            self.fresh_samples += 1

    def snapshot(self):
        return {
            "samples": self.samples,
            "fresh_samples": self.fresh_samples,
            "first_stamp_ns": self.first_stamp_ns,
            "last_stamp_ns": self.last_stamp_ns,
            "source_time_advanced_seconds": None if self.first_stamp_ns is None else
                (self.last_stamp_ns - self.first_stamp_ns) / 1_000_000_000,
            "last_source_age_seconds": None if self.last_stamp_ns is None else
                (time.time_ns() - self.last_stamp_ns) / 1_000_000_000,
            "last_receipt_age_seconds": None if self.last_receipt is None else
                time.monotonic() - self.last_receipt,
        }


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--seconds", type=float, default=45, help="observation duration, 1–300 seconds")
    parser.add_argument("--url", default=os.environ.get("GO2_SIMULATOR_URL", "http://127.0.0.1:8890"))
    parser.add_argument("--expected-vm")
    parser.add_argument("--expected-source")
    args = parser.parse_args()
    require(math.isfinite(args.seconds) and 1 <= args.seconds <= 300, "seconds must be between 1 and 300")
    status = StatusReader(args.url, args.expected_vm, args.expected_source)
    before = status.read()
    report("observer_started", vm_name=before["vm_name"], source_digest=before["source_digest"],
           epoch=before["epoch"], seconds=args.seconds)
    rclpy.init(args=[])
    node = Observer()
    started = time.monotonic()
    next_report = started + 1
    try:
        while time.monotonic() - started < args.seconds:
            rclpy.spin_once(node, timeout_sec=0.1)
            now = time.monotonic()
            if now >= next_report:
                report("odom_observation", elapsed_seconds=now - started, **node.snapshot())
                next_report = now + 1
        after = status.read()
        observation = node.snapshot()
        require(after["vm_name"] == before["vm_name"] and after["source_digest"] == before["source_digest"]
                and after["epoch"] == before["epoch"], "runtime identity or world epoch changed")
        require(after["time"] > before["time"], "physics time did not advance")
        require(node.fresh_samples >= 2 and observation["source_time_advanced_seconds"] > 0
                and -0.1 <= observation["last_source_age_seconds"] <= 1
                and observation["last_receipt_age_seconds"] <= 1, "fresh odometry did not keep advancing")
        report("observer_complete", passed=True, vm_name=after["vm_name"], epoch=after["epoch"],
               elapsed_seconds=time.monotonic() - started,
               physics_time_advanced_seconds=after["time"] - before["time"], **observation)
    finally:
        node.destroy_node()
        rclpy.shutdown()


if __name__ == "__main__":
    try:
        main()
    except Exception as error:
        report("observer_complete", passed=False, error=str(error))
        raise SystemExit(1)
