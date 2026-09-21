"""Real Unitree SDK and ROS acceptance against an explicitly identified simulator.

HTTP is used only for identity, observation, explicit source grants, and reset.
All robot motion commands travel through native DDS/ROS messages.
"""

import argparse
from collections import deque
import json
import math
from pathlib import Path
import queue
import subprocess
import sys
import threading
import time

from common import DEFAULT_Q, POLICY_TO_SDK, SDK_COMMIT, SimulatorAPI, Stream, grant_new_source
from common import require, require_test_environment, sdk_crc, sources, wait_until, yaw


class ROSWorker:
    def __init__(self, api):
        self.messages, self.logs = queue.Queue(), deque(maxlen=10)
        command = [sys.executable, str(Path(__file__).with_name("ros_requests.py")), "--url", api.base]
        if api.vm_name is not None:
            command += ["--managed-vm", api.vm_name, "--source-digest", api.source_digest]
        self.process = subprocess.Popen(command,
                                        stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True)
        self.thread = threading.Thread(target=self.read, daemon=True)
        self.thread.start()
        require(self.messages.get(timeout=10).get("ready") is True, "ROS request worker did not become ready")

    def read(self):
        for line in self.process.stdout:
            try:
                self.messages.put(json.loads(line))
            except json.JSONDecodeError:
                self.logs.append(line.rstrip())

    def call(self, **value):
        self.process.stdin.write(json.dumps(value) + "\n")
        self.process.stdin.flush()
        try:
            return self.messages.get(timeout=5)
        except queue.Empty as error:
            raise AssertionError(f"ROS request worker timed out: {list(self.logs)}") from error

    def close(self):
        if self.process.poll() is None:
            try:
                self.process.stdin.write('{"op":"close"}\n')
                self.process.stdin.flush()
            except BrokenPipeError:
                # Child already exited and closed stdin during shutdown.
                pass
            try:
                self.process.wait(timeout=3)
            except subprocess.TimeoutExpired:
                self.process.terminate()
                self.process.wait(timeout=3)


class Harness:
    def __init__(self, api):
        self.api = api
        self.results = []
        self.streams = []
        self.subscribers = []
        self.clients = []
        self.low_samples = deque(maxlen=20)
        self.low_count = self.sport_count = 0
        self.latest_low = self.latest_sport = None
        self.lock = threading.Lock()

    def report(self, name, **details):
        result = {"check": name, "passed": True, **details}
        self.results.append(result)
        print(json.dumps(result, allow_nan=False), flush=True)

    def initialize_sdk(self):
        self.api.status()
        from unitree_sdk2py.core.channel import ChannelFactoryInitialize, ChannelSubscriber
        from unitree_sdk2py.idl.unitree_go.msg.dds_ import LowState_, SportModeState_
        ChannelFactoryInitialize(0, "lo")
        self.crc = sdk_crc()
        def low(message):
            with self.lock:
                self.low_count += 1
                self.latest_low = (time.monotonic(), message)
                if not self.low_samples or time.monotonic() - self.low_samples[-1][0] > 0.05:
                    self.low_samples.append((time.monotonic(), message))
        def sport(message):
            with self.lock:
                self.sport_count += 1
                self.latest_sport = (time.monotonic(), message)
        for topic, datatype, callback in (("rt/lowstate", LowState_, low), ("rt/sportmodestate", SportModeState_, sport)):
            subscriber = ChannelSubscriber(topic, datatype)
            subscriber.Init(callback, 1)
            self.subscribers.append(subscriber)
        self.report("sdk_initialized", sdk_commit=SDK_COMMIT, domain=0, interface="lo",
                    crc_backend="pinned SDK Python branch; Linux shared libraries absent from source archive")

    def reset(self):
        self.api.post("/api/reset")
        wait_until(lambda: self.api.status()["ready"], "simulator did not become ready after reset", timeout=20)

    def new_sport(self):
        from unitree_sdk2py.go2.sport.sport_client import SportClient
        before = set(sources(self.api))
        client = SportClient()
        client.SetTimeout(2.0)
        client.Init()
        self.clients.append(client)
        code, version = client.GetServerApiVersion()
        require(code == 0 and version == "1.0.0.1", f"real SportClient version call failed: {(code, version)}")
        def move(value):
            code = client.Move(*value)
            require(code == 0, f"SDK Move writer failed: {code}")
        stream = Stream(move, 0.05).start((0.0, 0.0, 0.0))
        self.streams.append(stream)
        gid = grant_new_source(self.api, before, "sport")
        self.report("real_sport_client_and_explicit_grant", version=version, publisher_gid=gid)
        return client, stream, gid

    def check_low_state(self):
        wait_until(lambda: len(self.low_samples) >= 5, "SDK LowState received fewer than five samples")
        with self.lock:
            samples = list(self.low_samples)[-5:]
            receipt, latest = self.latest_low
        require(time.monotonic() - receipt < 0.5, "SDK LowState is stale")
        for _, message in samples:
            require(len(message.motor_state) == 20, "SDK LowState does not contain twenty motor slots")
            require(list(message.head) == [0xFE, 0xEF] and message.level_flag == 0xFF, "LowState header differs")
            require(self.crc.Crc(message) == message.crc, "LowState CRC differs from the pinned SDK implementation")
            require(message.bms_state.soc == 90 and list(message.bms_state.cell_vol) == [3600] * 8 + [0] * 7,
                    "ideal simulated battery fields differ")
            require(abs(message.power_v - 28.8) < 1e-4, "simulated bus voltage differs")
            require(all(motor.mode == 1 for motor in message.motor_state[:12]) and
                    all(motor.mode == 0 for motor in message.motor_state[12:]), "motor-slot active modes differ")
        status = self.api.status()
        with self.lock:
            _, latest = self.latest_low
        dt = abs(status["time"] - latest.tick / 1000.0)
        require(dt < 0.2, "LowState and HTTP physics sample too far apart to check joint order")
        errors = []
        for policy_index, sdk_index in enumerate(POLICY_TO_SDK):
            motor = latest.motor_state[sdk_index]
            error = abs(status["joints"]["q"][policy_index] - motor.q)
            tolerance = 0.03 + abs(motor.dq) * dt
            require(error < tolerance, f"joint mapping differs at policy{policy_index}/SDK{sdk_index}: {error}")
            errors.append(error)
        self.report("sdk_lowstate_crc_battery_and_joint_order", crc_samples=5, motor_slots=20,
                    maximum_joint_error_rad=max(errors), sdk_order="FR,FL,RR,RL")

    def check_sport_state(self):
        try:
            wait_until(lambda: self.latest_sport, "pinned SDK SportModeState did not match/deserialize the ROS publisher", timeout=3)
            with self.lock:
                receipt, message = self.latest_sport
            require(time.monotonic() - receipt < 0.5, "SDK SportModeState is stale")
            require(len(message.path_point) == 10, "SDK SportModeState path_point shape differs")
            require(all(getattr(point, field) == 0 for point in message.path_point
                        for field in ("t_from_start", "x", "y", "yaw", "vx", "vy", "vyaw")),
                    "unsupported trajectory path points must remain zero")
            require(math.isfinite(message.body_height) and message.body_height > 0, "SportModeState body height invalid")
            self.report("sdk_sportmodestate_interoperability", path_points=len(message.path_point), samples=self.sport_count)
        except AssertionError as error:
            result = {"check": "sdk_sportmodestate_interoperability", "passed": False, "error": str(error),
                      "expected_overlay": "SDK path_point[10] with zero values"}
            self.results.append(result)
            print(json.dumps(result), flush=True)

    def exercise_sport(self):
        client, stream, gid = self.new_sport()
        self.check_low_state()
        self.check_sport_state()
        for vx in (0.3, -0.3):
            start = self.api.status()
            stream.set((vx, 0.0, 0.0))
            time.sleep(2.5)
            require(stream.error is None, f"SDK command stream failed: {stream.error}")
            end = self.api.status()
            dx, dy = [end["position"][i] - start["position"][i] for i in (0, 1)]
            displacement = dx * math.cos(yaw(start)) + dy * math.sin(yaw(start))
            require(math.copysign(1, vx) * displacement > 0.25, f"SDK Move produced insufficient signed displacement: {displacement}")
            require(end["mode"] not in {"fallen", "damping", "fault"}, f"SDK Move ended in {end['mode']}")
            stream.set((0.0, 0.0, 0.0))
            require(client.StopMove() == 0, "real SDK StopMove failed")
            wait_until(lambda: abs(self.api.status()["linear_velocity_world"][0]) < 0.15, "StopMove did not settle the robot")
            self.report("sdk_signed_move_and_stop", vx=vx, body_forward_displacement_m=displacement)
        stream.stop()
        require(client.StandDown() == 0, "real SDK StandDown failed")
        down = wait_until(lambda: (value if value["mode"] == "lying" else None) if (value := self.api.status()) else None,
                          "SDK StandDown did not finish in lying", timeout=8)
        require(down["position"][2] < 0.14 and down["ncontact"] > 0, "StandDown did not produce a supported low pose")
        require(client.StandUp() == 0, "real SDK StandUp failed")
        up = wait_until(lambda: (value if value["mode"] == "standing" and value["position"][2] > 0.25 else None)
                       if (value := self.api.status()) else None, "SDK StandUp did not physically raise the robot", timeout=8)
        require(up["epoch"] == down["epoch"] and up["time"] > down["time"], "posture operation reset the world")
        self.report("sdk_physical_postures", lying_height=down["position"][2], standing_height=up["position"][2])
        require(client.Damp() == 0, "real SDK Damp failed")
        wait_until(lambda: self.api.status()["mode"] == "damping", "SDK Damp did not enter damping")
        require(self.api.status()["ros_commands"]["owner"] is None, "Damp retained ownership")
        self.reset()
        # An old DDS writer remains blocked after reset even if it sends again.
        client.Move(0.0, 0.0, 0.0)
        time.sleep(0.1)
        require(sources(self.api)[gid]["requires_restart"], "reset did not block the old SDK writer identity")
        replacement, replacement_stream, replacement_gid = self.new_sport()
        require(replacement_gid != gid, "reset regrant reused an old SDK publisher")
        replacement_stream.stop()
        require(replacement.StopMove() == 0, "new SDK source could not stop after regrant")
        self.api.post("/api/disarm_ros")
        self.report("sdk_damp_reset_and_fresh_regrant", old_gid=gid, new_gid=replacement_gid)

    def exercise_ros_requests(self):
        before = set(sources(self.api))
        worker = ROSWorker(self.api)
        try:
            worker.call(op="stream", enabled=True)
            def response(api, expected_code, **kwargs):
                result = worker.call(op="request", api=api, **kwargs)
                received = result["response"]
                require(received is not None, f"ROS Request API{api} timed out")
                require(received["id"] == result["request_id"] and received["api"] == result["request_api"],
                        "native response correlation fields differ")
                require(received["code"] == expected_code, f"ROS Request API{api}: {received}")
                return received
            require(response(1, 0)["data"] == "1.0.0.1", "ROS API version response differs")
            response(1003, 3205)
            response(1006, 3203)
            grant_new_source(self.api, before, "sport")
            response(1008, 3204, parameter='{"x":"invalid","y":0,"z":0}')
            response(1003, 3205, priority=1)
            response(1003, 3205, lease=1)
            result = worker.call(op="request", api=1, noreply=True)
            require(result["response"] is None, "native server replied despite noreply=true")
            mode = response(1001, 0, service="motion_switcher")
            require("name" in json.loads(mode["data"]), "motion-switcher CheckMode response shape differs")
            response(1003, 3203, service="motion_switcher")
            self.report("generated_ros_requests", unsupported=3203, invalid=3204, denied=3205,
                        exact_correlation=True, noreply_respected=True,
                        motion_switcher="CheckMode only; mutations intentionally unsupported")
        finally:
            try:
                worker.call(op="stream", enabled=False)
            finally:
                worker.close()
                self.api.post("/api/disarm_ros")

    def make_low(self, target=None):
        from unitree_sdk2py.idl.default import unitree_go_msg_dds__LowCmd_
        message = unitree_go_msg_dds__LowCmd_()
        message.head, message.level_flag = [0xFE, 0xEF], 0xFF
        for index, motor in enumerate(message.motor_cmd):
            motor.mode = 1 if index < 12 else 0
            motor.q, motor.dq = 2146000000.0, 16000.0
            motor.kp = motor.kd = motor.tau = 0.0
        if target is not None:
            for policy_index, sdk_index in enumerate(POLICY_TO_SDK):
                motor = message.motor_cmd[sdk_index]
                motor.q, motor.dq = float(target[policy_index]), 0.0
                motor.kp, motor.kd = 50.0, 3.0
        message.crc = self.crc.Crc(message)
        return message

    def exercise_low_level(self):
        # Reach a supported lying pose through the real SDK before transferring
        # authority. The custom low-level stand does not use motion_switcher.
        client, sport_stream, _ = self.new_sport()
        sport_stream.stop()
        require(client.StandDown() == 0, "low-level setup StandDown failed")
        wait_until(lambda: self.api.status()["mode"] == "lying", "low-level setup did not lie down", timeout=8)
        self.api.post("/api/disarm_ros")
        from unitree_sdk2py.core.channel import ChannelPublisher
        from unitree_sdk2py.idl.unitree_go.msg.dds_ import LowCmd_
        before = set(sources(self.api))
        publisher = ChannelPublisher("rt/lowcmd", LowCmd_)
        publisher.Init()
        stream = Stream(lambda target: require(publisher.Write(self.make_low(target)), "SDK LowCmd writer failed"), 0.01).start(None)
        self.streams.append(stream)
        gid = grant_new_source(self.api, before, "lowcmd")
        initial = self.api.status()
        accepted = initial["ros_commands"]["accepted"]
        wait_until(lambda: self.api.status()["ros_commands"]["accepted"] > accepted, "sentinel LowCmd was not accepted")
        updates = self.api.status()["metrics"]["policy_updates"]
        start_q = initial["joints"]["q"]
        started = time.monotonic()
        while time.monotonic() - started < 3.0:
            alpha = min(1.0, (time.monotonic() - started) / 2.0)
            alpha = alpha * alpha * (3 - 2 * alpha)
            stream.set(tuple(a + alpha * (b - a) for a, b in zip(start_q, DEFAULT_Q)))
            time.sleep(0.01)
        stood = self.api.status()
        require(stream.error is None, f"low-level stream failed: {stream.error}")
        require(stood["mode"] == "lowlevel" and stood["position"][2] - initial["position"][2] > 0.17,
                f"external SDK LowCmd did not physically stand the robot: {stood['mode']}, {stood['position']}")
        require(stood["metrics"]["policy_updates"] == updates, "walking policy ran during external low-level control")
        require(stood["ncontact"] > 0, "external stand lacks contact support")
        for kind in ("crc", "motor_mode"):
            rejected = self.api.status()["ros_commands"]["rejected"]
            bad = self.make_low(DEFAULT_Q)
            if kind == "crc":
                bad.crc ^= 1
            else:
                bad.motor_cmd[0].mode = 2
                bad.crc = self.crc.Crc(bad)
            publisher.Write(bad)
            wait_until(lambda: self.api.status()["ros_commands"]["rejected"] > rejected, f"invalid {kind} LowCmd was not rejected")
        self.report("sdk_lowcmd_physical_stand", motor_slots=20, sentinel_packets_accepted=True,
                    lying_height=initial["position"][2], standing_height=stood["position"][2],
                    invalid_crc_rejected=True, invalid_mode_rejected=True, policy_updates_during_stand=0,
                    max_command_gap_ms=stream.max_gap * 1000)
        stream.stop()
        stopped_at = time.monotonic()
        damped = wait_until(lambda: (value if value["mode"] == "damping" else None) if (value := self.api.status()) else None,
                           "40ms LowCmd watchdog did not enter damping", timeout=2)
        detected_at = time.monotonic()
        require(damped["ros_commands"]["owner"] is None, "low-level watchdog retained source authority")
        time.sleep(0.3)
        require(self.api.status()["metrics"]["policy_updates"] == updates, "low-level timeout resumed walking policy")
        publisher.Write(self.make_low(DEFAULT_Q))
        time.sleep(0.1)
        require(self.api.status()["mode"] == "damping", "old LowCmd writer revived an expired grant")
        self.report("sdk_lowcmd_watchdog", configured_timeout_ms=40,
                    observed_http_detection_ms=(detected_at - stopped_at) * 1000,
                    owner_revoked=True, policy_did_not_resume=True, publisher_gid=gid)
        publisher.Close()

    def close(self):
        for stream in self.streams:
            stream.stop()
        self.api.post("/api/disarm_ros")
        for subscriber in self.subscribers:
            subscriber.Close()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--url", default="http://127.0.0.1:8890")
    parser.add_argument("--output", type=Path)
    parser.add_argument("--managed-vm", metavar="NAME",
                        help="explicit managed simulator VM, whose runtime must report enforced loopback DDS isolation")
    parser.add_argument("--source-digest", help="optional expected managed runtime SHA-256 source pin")
    args = parser.parse_args()
    api = SimulatorAPI(args.url, vm_name=args.managed_vm, source_digest=args.source_digest)
    require_test_environment(api)  # No SDK import, participant, or mutating call precedes this.
    harness = Harness(api)
    error = None
    try:
        harness.initialize_sdk()
        harness.reset()
        harness.exercise_sport()
        harness.exercise_ros_requests()
        harness.exercise_low_level()
    except Exception as failure:
        error = f"{type(failure).__name__}: {failure}"
    finally:
        try:
            harness.close()
        except Exception as cleanup_error:
            error = (error + "; " if error else "") + f"cleanup failed: {cleanup_error}"
    result = {"passed": error is None and all(item["passed"] for item in harness.results),
              "sdk_commit": SDK_COMMIT, "checks": harness.results, "error": error,
              "environment": {"mode": "managed-vm" if api.vm_name else "isolated-container",
                              "vm_name": api.vm_name, "source_digest": api.source_digest},
              "unmodified_stock_stand_example_supported": False}
    if args.output:
        args.output.parent.mkdir(parents=True, exist_ok=True)
        args.output.write_text(json.dumps(result, indent=2, allow_nan=False) + "\n")
    print(json.dumps({"native_acceptance": result}, allow_nan=False), flush=True)
    return 0 if result["passed"] else 1


if __name__ == "__main__":
    sys.exit(main())
