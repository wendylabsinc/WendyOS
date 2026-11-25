#!/usr/bin/env python3
"""Actual ROS publisher -> passive native helper -> strict decoder check.

Run only in an isolated, network-disabled test container. A read-only container
must set ROS_LOG_DIR to a writable temporary directory. These are synthetic
ROS publishers, not runtime attestation or a clearance/command qualification.
The process has a nine-second watchdog, including cleanup reserve, and never
uses HTTP, arm services, command topics, or a simulator.
"""

from __future__ import annotations

import argparse
from array import array
import hashlib
import json
import math
import os
from pathlib import Path
import re
import selectors
import signal
import struct
import subprocess
import sys
import time

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))

ROW_LIMIT = 256 * 1024
STDOUT_LIMIT = 3 * 1024 * 1024
STDERR_LIMIT = 64 * 1024
SPARSE = (0, 359, 360, 4320, 4499, 4500, 8999)


class Refused(Exception):
    pass


def require(condition, reason):
    if not condition:
        raise Refused(reason)


def xyz_bytes(indices):
    """Independent documented pattern, explicitly rounded to binary32."""
    out = bytearray()
    for index in indices:
        elevation = math.radians(-30 + 2.5 * (index // 360))
        azimuth = -math.pi + (index % 360) * (2 * math.pi / 360)
        distance = 1.0 + (index % 11) / 10
        out.extend(struct.pack("<fff", distance * math.cos(elevation) * math.cos(azimuth),
                               distance * math.cos(elevation) * math.sin(azimuth),
                               distance * math.sin(elevation)))
    return bytes(out)


class Lines:
    def __init__(self):
        self.pending = bytearray()
        self.total = 0

    def feed(self, chunk):
        self.total += len(chunk)
        require(self.total <= STDOUT_LIMIT, "stdout_bound")
        self.pending.extend(chunk)
        result = []
        while b"\n" in self.pending:
            end = self.pending.index(10)
            require(end + 1 <= ROW_LIMIT, "row_bound")
            line = bytes(self.pending[:end])
            del self.pending[:end + 1]
            try:
                value = json.loads(line)
            except (ValueError, UnicodeError):
                raise Refused("helper_json") from None
            require(type(value) is dict, "helper_json_object")
            result.append(value)
        require(len(self.pending) < ROW_LIMIT, "row_bound")
        return result


def check_cloud(row, variants, expected_callback_gid, publish_bounds):
    from sim.go2_cloud import decode_cloud, SOURCE_DIGEST, RANGE_QUANTIZATION_M
    require(row.get("kind") == "sample" and row.get("topic") == "/utlidar/cloud", "unexpected_sample")
    gid = row.get("publisher_gid")
    require(type(gid) is str and re.fullmatch(r"[0-9a-f]{48}", gid) is not None
            and gid != "0" * 48
            and (expected_callback_gid is None or gid == expected_callback_gid), "publisher_gid")
    for key in ("source_timestamp_ns", "helper_elapsed_ns"):
        require(type(row.get(key)) is int and row[key] > 0, "source_metadata")
    require(type(row.get("received_timestamp_ns")) is int
            and row["received_timestamp_ns"] >= 0, "received_metadata")
    require(publish_bounds[0] <= row["source_timestamp_ns"] <= publish_bounds[1], "source_timestamp_bounds")
    encoded = row.get("payload_hex")
    require(type(encoded) is str and 0 < len(encoded) <= 220000
            and len(encoded) % 2 == 0 and re.fullmatch(r"[0-9a-f]+", encoded) is not None, "payload_hex")
    raw = bytes.fromhex(encoded)
    cloud = decode_cloud(raw, source_digest=SOURCE_DIGEST)
    require(cloud.capture_unix_ns in variants, "original_header_stamp")
    name, indices, original = variants[cloud.capture_unix_ns]
    require(tuple(p.ray_index for p in cloud.points) == indices, "recovered_indices")
    require(len(cloud.ranges_m) == 9000, "slot_count")
    expected_xyz = tuple(struct.iter_unpack("<fff", original))
    require(tuple(p.xyz_m for p in cloud.points) == expected_xyz, "original_float32_xyz")
    present = set(indices)
    for index, distance in enumerate(cloud.ranges_m):
        if index in present:
            require(distance is not None and abs(distance - (1.0 + (index % 11) / 10))
                    <= RANGE_QUANTIZATION_M, "reconstructed_range")
        else:
            require(distance is None, "missing_slot_not_unknown")
    return {"variant": name, "capture_unix_ns": cloud.capture_unix_ns,
            "publisher_gid": gid, "source_timestamp_ns": row["source_timestamp_ns"],
            "received_timestamp_ns": row["received_timestamp_ns"],
            "received_timestamp_available": row["received_timestamp_ns"] != 0,
            "helper_elapsed_ns": row["helper_elapsed_ns"], "points": len(indices),
            "unknown_slots": 9000 - len(indices), "serialized_bytes": len(raw),
            "serialized_sha256": hashlib.sha256(raw).hexdigest(),
            "xyz_sha256": hashlib.sha256(original).hexdigest()}


def run(helper, case="clouds"):
    started = time.monotonic()
    process = None
    node = None
    ros = None
    selector = selectors.DefaultSelector()
    result = {"schema": "wendy.world.go2-cloud-ros-interop.v1", "success": False,
              "sensor_source": "synthetic_ros_test_publishers", "runtime_attestation": False,
              "operational_authority": False, "runtime_calls": 0, "positive_commands": 0,
              "samples": [], "gid_observations": [],
              "identity_association": "isolated_sole_graph_publisher_and_exact_original_payload",
              "identity_limit": "Graph endpoint ID and callback RMW GID are distinct opaque identifiers. No conversion, equality or authentication claim.",
              "case": case, "scans": [],
              "companion_topics": "not_published" if case == "clouds" else "/scan only",
              "helper_complete": None}

    def emergency(_signum, _frame):
        # This last-resort wall bound also covers a blocked ROS shutdown. Reap
        # the only child before exiting; never wait for buffered pipe output.
        if process is not None:
            try:
                process.kill()
                process.wait(timeout=.2)
            except (ProcessLookupError, subprocess.TimeoutExpired):
                pass
        os.write(2, b"cloud_interop_absolute_deadline\n")
        os._exit(124)

    previous = signal.signal(signal.SIGALRM, emergency)
    signal.setitimer(signal.ITIMER_REAL, 9.0)
    try:
        import rclpy
        from rclpy.node import Node
        from rclpy.qos import QoSProfile, ReliabilityPolicy, DurabilityPolicy
        from sensor_msgs.msg import PointCloud2, PointField, LaserScan
        from rclpy.serialization import deserialize_message
        ros = rclpy
        rclpy.init(args=[])
        node = Node("world_go2_synthetic_cloud_interop")
        qos = QoSProfile(depth=1, reliability=ReliabilityPolicy.BEST_EFFORT,
                         durability=DurabilityPolicy.VOLATILE)
        publisher = node.create_publisher(PointCloud2, "/utlidar/cloud", qos)
        scan_publisher = None if case == "clouds" else node.create_publisher(LaserScan, "/scan", qos)
        # Deliberately distinct fixed original stamps, unrelated to DDS send
        # timestamps. This verifies preservation, not observation freshness.
        variants = {123000000456: ("full", tuple(range(9000)), xyz_bytes(range(9000))),
                    124000000789: ("sparse", SPARSE, xyz_bytes(SPARSE))}
        if case != "clouds":
            variants = {123000000456 + i * 1000000000:
                        ("full" if i == 0 else "sparse", tuple(range(9000)) if i == 0 else SPARSE,
                         xyz_bytes(range(9000) if i == 0 else SPARSE)) for i in range(5)}
        messages = {}
        scan_messages = {}
        for stamp, (name, indices, raw) in variants.items():
            message = PointCloud2()
            message.header.frame_id = "utlidar_lidar"
            message.header.stamp.sec, message.header.stamp.nanosec = divmod(stamp, 1_000_000_000)
            message.height, message.width = 1, len(indices)
            message.point_step, message.row_step = 12, len(raw)
            message.is_bigendian, message.is_dense = False, True
            message.fields = [PointField(name=n, offset=i * 4, datatype=PointField.FLOAT32, count=1)
                              for i, n in enumerate(("x", "y", "z"))]
            message.data = array("B", raw)
            messages[name if case == "clouds" else stamp] = message
            if case != "clouds":
                scan = LaserScan()
                scan.header.frame_id = "lidar_link"
                scan.header.stamp.sec, scan.header.stamp.nanosec = divmod(stamp, 1000000000)
                scan.angle_min, scan.angle_increment = -math.pi, 2 * math.pi / 360
                scan.angle_max = math.pi - 2 * math.pi / 360
                scan.range_min, scan.range_max = .1, 12.
                scan.ranges = [1.0 + (i % 11) / 10 if i in indices else math.inf
                               for i in range(4320, 4680)]
                scan_messages[stamp] = scan
        command = [str(helper), "--duration-ms", "7000", "--max-clouds", "5"]
        result["helper_command"] = command
        result["helper_sha256"] = hashlib.sha256(helper.read_bytes()).hexdigest()
        process = subprocess.Popen(command, stdin=subprocess.DEVNULL, stdout=subprocess.PIPE,
                                   stderr=subprocess.PIPE, bufsize=0, close_fds=True)
        for stream, label in ((process.stdout, "stdout"), (process.stderr, "stderr")):
            os.set_blocking(stream.fileno(), False)
            selector.register(stream, selectors.EVENT_READ, label)
        lines = Lines()
        stderr = bytearray()
        full_seen = False
        expected_gid = None
        callback_gid = None
        first_publish = None
        last_publish = 0.0
        companion_stamps = list(variants)
        cloud_send_times = {}
        pending_scan = None
        scan_gid = None
        scan_graph_gid = None
        next_cloud = 0
        final_cloud_observed = None
        last_extra_cloud = 0.0
        result["extra_clouds_before_completion_observed"] = 0
        result["extra_cloud_publish_after_fifth_ms"] = []
        while time.monotonic() - started < 8.0:
            now = time.monotonic()
            if expected_gid is None or now - last_publish >= .1:
                endpoints = node.get_publishers_info_by_topic("/utlidar/cloud")
                ours = [x for x in endpoints if x.node_name == node.get_name()
                        and x.node_namespace == node.get_namespace()]
                require(len(endpoints) <= 1 and len(ours) <= 1, "unexpected_test_publisher")
                if ours:
                    graph_gid = bytes(ours[0].endpoint_gid).hex()
                    require(re.fullmatch(r"[0-9a-f]{48}", graph_gid) is not None
                            and graph_gid != "0" * 48
                            and (expected_gid is None or graph_gid == expected_gid), "graph_publisher_changed")
                    expected_gid = graph_gid
                    result["graph_endpoint_gid"] = graph_gid
                else:
                    require(expected_gid is None, "graph_publisher_lost")
            if expected_gid and now - last_publish >= .1 and process.poll() is None:
                if case == "clouds":
                    before = time.time_ns()
                    if first_publish is None:
                        first_publish = before
                    publisher.publish(messages["sparse" if full_seen else "full"])
                    last_publish = now
                else:
                    endpoints = node.get_publishers_info_by_topic("/scan")
                    require(len(endpoints) <= 1, "unexpected_scan_publisher")
                    if endpoints:
                        endpoint = endpoints[0]
                        require(endpoint.node_name == node.get_name()
                                and endpoint.node_namespace == node.get_namespace(), "unexpected_scan_publisher")
                        current = bytes(endpoint.endpoint_gid).hex()
                        require(re.fullmatch(r"[0-9a-f]{48}", current) is not None
                                and current != "0" * 48 and (scan_graph_gid is None or scan_graph_gid == current),
                                "scan_graph_publisher_changed")
                        scan_graph_gid = current
                        result["scan_graph_endpoint_gid"] = current
                    if (scan_graph_gid and publisher.get_subscription_count() > 0
                            and scan_publisher.get_subscription_count() > 0 and next_cloud < 5
                            and len(result["scans"]) == next_cloud):
                        stamp = companion_stamps[next_cloud]
                        before = time.time_ns()
                        if first_publish is None:
                            first_publish = before
                        publisher.publish(messages[stamp])
                        cloud_send_times[stamp] = time.monotonic_ns()
                        next_cloud += 1
                        last_publish = now
            if pending_scan is not None and now >= pending_scan[1] and process.poll() is None:
                stamp, _, _ = pending_scan
                scan_publisher.publish(scan_messages[stamp])
                if stamp == companion_stamps[-1]:
                    result["final_scan_publish_after_cloud_observed_ms"] = (now - pending_scan[2]) * 1000
                pending_scan = None
            if (case != "clouds" and final_cloud_observed is not None and process.poll() is None
                    and result["helper_complete"] is None
                    and result["extra_clouds_before_completion_observed"] < 64
                    and now - last_extra_cloud >= .01):
                publisher.publish(messages[companion_stamps[-1]])
                result["extra_clouds_before_completion_observed"] += 1
                result["extra_cloud_publish_after_fifth_ms"].append((now - final_cloud_observed) * 1000)
                last_extra_cloud = now
            for key, _ in selector.select(timeout=.01):
                chunk = os.read(key.fileobj.fileno(), 32768)
                if not chunk:
                    selector.unregister(key.fileobj)
                    continue
                if key.data == "stderr":
                    stderr.extend(chunk)
                    require(len(stderr) <= STDERR_LIMIT, "stderr_bound")
                    continue
                for row in lines.feed(chunk):
                    if row.get("kind") == "complete":
                        require(result["helper_complete"] is None, "duplicate_complete")
                        result["helper_complete"] = row
                        if final_cloud_observed is not None:
                            result["completion_after_final_cloud_observed_ms"] = (time.monotonic() - final_cloud_observed) * 1000
                    else:
                        require(result["helper_complete"] is None, "sample_after_complete")
                        if case != "clouds" and row.get("topic") == "/scan":
                            require(len(result["scans"]) < 5, "scan_count")
                            gid = row.get("publisher_gid")
                            require(type(gid) is str and re.fullmatch(r"[0-9a-f]{48}", gid) is not None
                                    and gid != "0" * 48 and (scan_gid is None or scan_gid == gid), "scan_gid")
                            scan_gid = gid
                            payload = row.get("payload_hex")
                            require(type(payload) is str and len(payload) <= 98304 and len(payload) % 2 == 0
                                    and re.fullmatch(r"[0-9a-f]+", payload) is not None, "scan_payload")
                            raw = bytes.fromhex(payload)
                            scan = deserialize_message(raw, LaserScan)
                            stamp = scan.header.stamp.sec * 1000000000 + scan.header.stamp.nanosec
                            require(stamp in scan_messages and stamp not in {x["capture_unix_ns"] for x in result["scans"]},
                                    "scan_stamp")
                            expected = scan_messages[stamp]
                            require(scan.header.frame_id == "lidar_link" and scan.ranges == expected.ranges
                                    and scan.intensities == expected.intensities
                                    and all(getattr(scan, field) == struct.unpack('<f', struct.pack('<f', getattr(expected, field)))[0]
                                            for field in ('angle_min', 'angle_max', 'angle_increment', 'time_increment',
                                                          'scan_time', 'range_min', 'range_max')),
                                    "scan_original_fields")
                            for key in ("source_timestamp_ns", "helper_elapsed_ns"):
                                require(type(row.get(key)) is int and row[key] > 0, "scan_metadata")
                            require(type(row.get("received_timestamp_ns")) is int and row["received_timestamp_ns"] >= 0,
                                    "scan_received_metadata")
                            result["scans"].append({"capture_unix_ns": stamp, "publisher_gid": gid,
                                "serialized_bytes": len(raw), "serialized_sha256": hashlib.sha256(raw).hexdigest(),
                                "helper_elapsed_ns": row["helper_elapsed_ns"], "source_timestamp_ns": row["source_timestamp_ns"],
                                "received_timestamp_ns": row["received_timestamp_ns"], "original_ranges_equal": True})
                            continue
                        require(len(result["samples"]) < 5, "sample_count")
                        require(expected_gid is not None and first_publish is not None, "sample_before_publish")
                        observed_gid = row.get("publisher_gid")
                        result["gid_observations"].append({
                            "rclpy_graph_endpoint_gid": expected_gid,
                            "graph_bytes": len(expected_gid) // 2,
                            "observed_rclcpp_message_gid": observed_gid,
                            "observed_bytes": len(observed_gid) // 2 if type(observed_gid) is str else None})
                        receipt = check_cloud(row, variants, callback_gid, (first_publish, time.time_ns()))
                        callback_gid = receipt["publisher_gid"]
                        result["samples"].append(receipt)
                        full_seen = full_seen or receipt["variant"] == "full"
                        if case != "clouds":
                            stamp = receipt["capture_unix_ns"]
                            require(stamp == companion_stamps[len(result["samples"]) - 1]
                                    and stamp in cloud_send_times, "cloud_order_or_duplicate")
                            observed = time.monotonic()
                            if len(result["samples"]) == 5:
                                final_cloud_observed = observed
                            if len(result["samples"]) < 5 or case == "delayed-scan":
                                delay = .025 if len(result["samples"]) == 5 else 0
                                pending_scan = (stamp, observed + delay, observed)
            if process.poll() is not None and not selector.get_map():
                break
        require(process.poll() is not None, "helper_deadline")
        result["helper_exit"] = process.returncode
        result["stdout_bytes"] = lines.total
        result["stderr_bytes"] = len(stderr)
        result["stderr_sha256"] = hashlib.sha256(stderr).hexdigest()
        require(not lines.pending, "partial_output_line")
        require(process.returncode == 0, "helper_exit")
        complete = result["helper_complete"]
        expected_scans = 0 if case == "clouds" else (5 if case == "delayed-scan" else 4)
        require(type(complete) is dict and complete.get("reason") == "max_clouds"
                and complete.get("clouds") == 5 and complete.get("messages") == 5 + expected_scans
                and complete.get("serialized_bytes") == sum(x["serialized_bytes"] for x in result["samples"] + result["scans"]),
                "completion_counts")
        require({x["variant"] for x in result["samples"]} == {"full", "sparse"}, "missing_variant")
        if case != "clouds":
            require(len(result["scans"]) == expected_scans, "scan_count")
            require(result["extra_clouds_before_completion_observed"] > 0, "extra_cloud_case_not_exercised")
            received = {x["capture_unix_ns"] for x in result["scans"]}
            result["missing_same_capture_scan"] = [s for s in companion_stamps if s not in received]
            require(result["missing_same_capture_scan"] == ([] if case == "delayed-scan" else [companion_stamps[-1]]),
                    "companion_missing_classification")
            result["all_clouds_have_same_capture_scan"] = not result["missing_same_capture_scan"]
        result["success"] = True
    except Exception as error:
        result["failure"] = str(error)[:160] if isinstance(error, Refused) else type(error).__name__
    finally:
        if process is not None:
            if process.poll() is None:
                process.kill()
            process.wait(timeout=.3)
            result["helper_reaped"] = True
            for stream in (process.stdout, process.stderr):
                stream.close()
        selector.close()
        if node is not None:
            node.destroy_node()
        if ros is not None and ros.ok():
            ros.shutdown()
        result["elapsed_s"] = time.monotonic() - started
        signal.setitimer(signal.ITIMER_REAL, 0)
        signal.signal(signal.SIGALRM, previous)
    print(json.dumps(result, sort_keys=True), flush=True)
    return 0 if result["success"] else 1


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--helper", type=Path, required=True)
    parser.add_argument("--case", choices=("clouds", "delayed-scan", "missing-scan"), default="clouds")
    args = parser.parse_args()
    require(args.helper.is_absolute() and args.helper.is_file() and not args.helper.is_symlink()
            and os.access(args.helper, os.X_OK), "helper_path")
    return run(args.helper, args.case)


if __name__ == "__main__":
    raise SystemExit(main())
