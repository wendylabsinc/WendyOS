"""Real Humble publisher → C++ subscriber → attributed Unix datagram tests.

Run in a container with --network none. No robot, policy, motor publisher,
simulator runtime, or host network access is used.
"""

import json
import math
import os
from pathlib import Path
import signal
import socket
import subprocess
import sys
import tempfile
import time

import rclpy
from rclpy.utilities import get_rmw_implementation_identifier
from rclpy.serialization import deserialize_message
from geometry_msgs.msg import Twist
from unitree_api.msg import Request
from unitree_hg.msg import LowCmd


def receive(receiver, publisher, message, process, *, timeout=5.0):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        assert process.poll() is None, "the ingress exited unexpectedly"
        publisher.publish(message)
        try:
            payload = receiver.recv(4097)
            assert len(payload) <= 4096
            return json.loads(payload)
        except socket.timeout:
            continue
    raise AssertionError("no attributed command datagram arrived")


def drain(receiver):
    while True:
        try:
            receiver.recv(4096)
        except socket.timeout:
            return


def main():
    executable = sys.argv[1]
    os.environ["ROS_LOCALHOST_ONLY"] = "1"
    os.environ["ROS_DOMAIN_ID"] = "73"
    with tempfile.TemporaryDirectory(prefix="g1-ingress-") as directory:
        path = str(Path(directory) / "commands.sock")
        receiver = socket.socket(socket.AF_UNIX, socket.SOCK_DGRAM)
        receiver.bind(path)
        receiver.settimeout(0.05)
        logs = tempfile.TemporaryFile(mode="w+")
        process = subprocess.Popen([executable], env={**os.environ, "G1_COMMAND_SOCKET": path},
                                   stdout=logs, stderr=logs)
        rclpy.init()
        node = rclpy.create_node("g1_ingress_test_publisher")
        first = node.create_publisher(Twist, "/cmd_vel", 1)
        second = node.create_publisher(Twist, "/cmd_vel", 1)
        sport = node.create_publisher(Request, "/api/sport/request", 1)
        motion = node.create_publisher(Request, "/api/motion_switcher/request", 1)
        lowcmd = node.create_publisher(LowCmd, "/lowcmd", 1)
        try:
            message = Twist()
            message.linear.x, message.linear.y, message.angular.z = 0.35, -0.2, 0.4
            packet = receive(receiver, first, message, process)
            assert packet["kind"] == "twist"
            assert packet["velocity"] == [0.35, -0.2, 0.4]
            assert 0 <= time.monotonic_ns() - packet["received_ns"] < 5_000_000_000
            assert 0 <= time.time_ns() - packet["source_timestamp_ns"] < 5_000_000_000
            assert packet["publisher_gid"] and set(packet["publisher_gid"]) != {"0"}
            discovered = node.get_publishers_info_by_topic("/cmd_vel")
            gids = {bytes(endpoint.endpoint_gid).hex() for endpoint in discovered}
            middleware = get_rmw_implementation_identifier()
            if middleware == "rmw_fastrtps_cpp":
                assert packet["publisher_gid"] in gids, (packet, gids)
            first_gid = packet["publisher_gid"]
            drain(receiver)
            repeated_packet = receive(receiver, first, message, process)
            assert repeated_packet["publisher_gid"] == first_gid
            drain(receiver)
            second_packet = receive(receiver, second, message, process)
            assert second_packet["publisher_gid"] != first_gid
            if middleware == "rmw_fastrtps_cpp":
                assert second_packet["publisher_gid"] in gids
            elif middleware == "rmw_cyclonedds_cpp":
                print("Cyclone per-message IDs are kept opaque; graph GUID parity is not assumed (rmw_cyclonedds#377)", flush=True)
            print("actual publisher GIDs, clocks, and planar velocity envelope: PASS", flush=True)

            # Invalid commands cannot leak non-JSON numbers or discarded axes.
            drain(receiver)
            for axis, value in (("linear.x", math.nan), ("linear.y", math.inf),
                                ("angular.z", -math.inf), ("linear.z", 0.1),
                                ("angular.x", 0.1), ("angular.y", 0.1)):
                invalid = Twist()
                vector, component = axis.split(".")
                setattr(getattr(invalid, vector), component, value)
                for _ in range(3):
                    first.publish(invalid)
                    try:
                        unexpected = receiver.recv(4096)
                    except socket.timeout:
                        continue
                    raise AssertionError(f"invalid Twist produced {unexpected!r}")
            print("nonfinite and nonplanar commands are rejected: PASS", flush=True)

            # Native messages retain their complete generated ROS CDR layout,
            # including binary sequences and all thirty-five LowCmd motor slots.
            # This process only receives the datagrams; it executes no motors.
            request = Request()
            request.header.identity.id = 314159
            request.header.identity.api_id = 7105
            request.header.lease.id = 2718
            request.header.policy.priority = 3
            request.header.policy.noreply = True
            request.parameter = '{"velocity":[0.3,0.0,-0.2],"duration":1.0}'
            request.binary = [0, 1, 127, 128, 255]
            native_gids = set()
            for kind, publisher in (("sport", sport), ("motion_switcher", motion)):
                drain(receiver)
                native = receive(receiver, publisher, request, process)
                assert native["kind"] == kind
                assert "velocity" not in native
                assert len(native["publisher_gid"]) == 48
                assert 0 <= time.monotonic_ns() - native["received_ns"] < 5_000_000_000
                assert 0 <= time.time_ns() - native["source_timestamp_ns"] < 5_000_000_000
                decoded = deserialize_message(bytes.fromhex(native["payload_hex"]), Request)
                assert decoded == request
                native_gids.add(native["publisher_gid"])
                drain(receiver)
                repeat = receive(receiver, publisher, request, process)
                assert repeat["publisher_gid"] == native["publisher_gid"]
            assert len(native_gids) == 2
            assert first_gid not in native_gids

            motors = LowCmd()
            motors.mode_pr = 0
            motors.mode_machine = 0
            for index, motor in enumerate(motors.motor_cmd):
                motor.mode = index
                motor.q = index * 0.125
                motor.dq = -index * 0.25
                motor.tau = index * 0.5
                motor.kp = 20.0 + index
                motor.kd = 0.25 + index
                motor.reserve = index
            motors.reserve = [123, 456, 789, 987]
            motors.crc = 0x12345678
            drain(receiver)
            native = receive(receiver, lowcmd, motors, process)
            assert native["kind"] == "lowcmd"
            decoded = deserialize_message(bytes.fromhex(native["payload_hex"]), LowCmd)
            assert len(decoded.motor_cmd) == 35
            assert decoded == motors
            assert native["publisher_gid"] not in native_gids | {first_gid}
            drain(receiver)
            repeat = receive(receiver, lowcmd, motors, process)
            assert repeat["publisher_gid"] == native["publisher_gid"]
            print("native request and 35-slot LowCmd CDR decode, identities, clocks: PASS", flush=True)

            # Unbounded Request fields are rejected before copying or encoding
            # a large payload. Smaller fields whose combined CDR exceeds the
            # budget are rejected after serialization. A later valid request
            # must still arrive, proving oversize input does not kill ingress.
            drain(receiver)
            for parameter, binary in (("x" * 4096, []), ("", [255] * 4096),
                                      ("x" * 1900, [255] * 20)):
                oversized = Request(parameter=parameter, binary=binary)
                for _ in range(3):
                    sport.publish(oversized)
                    try:
                        unexpected = receiver.recv(4097)
                    except socket.timeout:
                        continue
                    raise AssertionError(f"oversized native request produced {len(unexpected)} bytes")
            recovered_native = receive(receiver, sport, request, process)
            assert deserialize_message(bytes.fromhex(recovered_native["payload_hex"]), Request) == request
            print("oversized native payloads are rejected and ingress recovers: PASS", flush=True)
            drain(receiver)

            # Stop consuming long enough to fill the local datagram queue.
            # Nonblocking send must drop with EAGAIN and keep DDS responsive.
            for _ in range(60):
                first.publish(message)
                time.sleep(0.01)
            assert process.poll() is None
            drain(receiver)
            message.linear.x = 0.2
            after_backpressure = receive(receiver, first, message, process)
            assert after_backpressure["velocity"][0] == 0.2
            print("full receiver queue drops samples and recovers: PASS", flush=True)

            # An absent/full receiver must not accumulate a sender retry queue
            # or terminate the ROS participant. Rebinding then accepts new data.
            receiver.close()
            os.unlink(path)
            for _ in range(10):
                first.publish(message)
                time.sleep(0.01)
            assert process.poll() is None
            receiver = socket.socket(socket.AF_UNIX, socket.SOCK_DGRAM)
            receiver.bind(path)
            receiver.settimeout(0.05)
            message.linear.x = -0.25
            recovered = receive(receiver, first, message, process)
            assert recovered["velocity"][0] == -0.25
            assert recovered["publisher_gid"] == first_gid
            print("receiver disappearance and recreation: PASS", flush=True)
        finally:
            node.destroy_node()
            rclpy.shutdown()
            process.send_signal(signal.SIGINT)
            try:
                process.wait(timeout=5.0)
            except subprocess.TimeoutExpired:
                process.kill()
                process.wait(timeout=5.0)
            receiver.close()
            logs.seek(0)
            output = logs.read()
            print(output, end="", flush=True)
            logs.close()
        assert process.returncode == 0
        assert "Dropping command datagram" in output
        assert "Resource temporarily unavailable" in output
        assert "Dropping nonfinite Twist" in output
        assert "Dropping oversized native command payload" in output

    for bad_path in ("relative.sock", "", "/" + "x" * 200):
        invalid = subprocess.run([executable], env={**os.environ, "G1_COMMAND_SOCKET": bad_path},
                                 capture_output=True, text=True, timeout=5.0)
        assert invalid.returncode != 0
        assert "G1_COMMAND_SOCKET" in invalid.stderr
    print("invalid socket paths fail at startup: PASS", flush=True)


if __name__ == "__main__":
    main()
