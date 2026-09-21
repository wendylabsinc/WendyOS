"""Finite pinned G1 SDK acceptance against a separate virtual robot.

No simulator Python module is imported. HTTP is limited to identity, observed
state, reset, explicit ROS ownership and disarm; every motor command uses DDS.
"""

import argparse
from collections import deque
import hashlib
import json
import math
from pathlib import Path
import threading
import time

from common import SimulatorAPI, Stream, grant_new_source, require, require_test_environment
from common import sdk_crc, sources, wait_until


class Harness:
    def __init__(self, api):
        self.api, self.results, self.streams, self.clients = api, [], [], []
        self.lock = threading.Lock()
        self.low, self.low_count = None, 0
        self.samples = deque(maxlen=20)
        self.crc = sdk_crc()
        from unitree_sdk2py.core.channel import ChannelFactoryInitialize, ChannelSubscriber
        from unitree_sdk2py.idl.unitree_hg.msg.dds_ import LowState_
        ChannelFactoryInitialize(0, "lo")
        self.subscriber = ChannelSubscriber("rt/lowstate", LowState_)
        self.subscriber.Init(self.observe, 10)
        self.fresh()

    def observe(self, message):
        with self.lock:
            self.low_count += 1
            self.low = (time.monotonic(), message)
            if not self.samples or time.monotonic() - self.samples[-1][0] >= .05:
                self.samples.append(self.low)

    def fresh(self):
        def latest():
            with self.lock:
                return self.low[1] if self.low and time.monotonic() - self.low[0] < .2 else None
        return wait_until(latest, "no fresh HG LowState reached the pinned SDK")

    def report(self, name, **details):
        value = {"check": name, "passed": True, **details}
        self.results.append(value)
        print(json.dumps(value, allow_nan=False), flush=True)

    def reset(self):
        for stream in self.streams:
            stream.stop()
        self.streams.clear()
        result = self.api.post("/api/reset")
        time.sleep(1.2)
        status = self.api.status()
        require(status["epoch"] == result["epoch"] and status["mode"] in {"standing", "moving"},
                "reset did not restore the standing virtual controller")
        return status

    def loco(self):
        from unitree_sdk2py.g1.loco.g1_loco_client import LocoClient
        before = sources(self.api)
        client = LocoClient()
        client.SetTimeout(2.)
        client.Init()
        self.clients.append(client)
        require(client.GetServerApiVersion() == (0, "1.0.0.0"), "G1 Loco server API version mismatch")
        require(client.GetFsmId() == (0, 500), "G1 GetFsmId did not report the standing controller")
        require(client.SetVelocity(0., 0., 0.) == 3205, "ungranted SDK Loco command was accepted")
        gid = grant_new_source(self.api, before, "sport")
        return client, gid

    def interfaces(self):
        low = self.fresh()
        status = self.api.status()
        require(len(low.motor_state) == 35 and (low.mode_pr, low.mode_machine) == (0, 0),
                "wrong G1 HG layout or PR/machine identity")
        require(list(low.version) == [int.from_bytes(b"SIM\0", "little"), 1], "native state lacks simulation version")
        require(low.crc == self.crc.Crc(low), "HG LowState CRC does not match the pinned SDK")
        require(len(status["joints"]["q"]) == 29, "runtime does not expose all29 physical joints")
        difference = max(abs(low.motor_state[i].q - status["joints"]["q"][i]) for i in range(29))
        require(difference < .12, "SDK native joint order differs from observed physical joint order")
        require(all(m.mode == 0 and m.q == 0 and m.dq == 0 and m.tau_est == 0 for m in low.motor_state[29:]),
                "unused HG slots are not zero")
        tick_age = status["time"] - low.tick / 1000
        require(-.1 < tick_age < .3, "HG tick does not track physics milliseconds")
        require(abs(sum(q*q for q in low.imu_state.quaternion) - 1) < .01, "HG quaternion is invalid")
        self.report("sdk_hg_observations", motor_slots=35, active_motors=29, crc=low.crc,
                    maximum_joint_difference_rad=difference, signed_tick_age_seconds=tick_age)

    def motion(self):
        self.reset()
        client, gid = self.loco()
        require(client.SetFsmId(500) == 0, "G1 Start did not accept the standing controller")
        require(client.SetFsmId(706) == 3203 and client.SetStandHeight(1.) == 3203,
                "unsupported factory posture/height succeeded")
        require(client.SetVelocity(0., 0., 0., -1.) == 3204, "invalid duration succeeded")
        def send(velocity):
            require(client.SetVelocity(*velocity) == 0, "SDK SetVelocity failed while granted")
        stream = Stream(send, .05).start((.3, 0., 0.))
        self.streams.append(stream)
        before = self.api.status()
        time.sleep(3.)
        after = self.api.status()
        require(stream.error is None and stream.count >= 40, f"SDK motion stream failed: {stream.error}")
        require(after["epoch"] == before["epoch"] and after["ros_commands"]["owner"] == gid,
                "motion reset the world or changed ownership")
        displacement = math.dist(after["position"][:2], before["position"][:2])
        require(displacement > .25 and after["position"][2] > .45,
                "G1 did not physically walk upright from native SDK commands")
        self.interfaces()
        stream.stop()
        self.streams.remove(stream)
        def stopped_motion():
            state = self.api.status()
            return state if max(abs(v) for v in state["applied_command"]) < .03 else None
        stopped = wait_until(stopped_motion, "Loco stream loss did not stop applied motion", timeout=.6)
        require(stopped["time"] > after["time"], "physics stopped with the SDK stream")
        self.report("sdk_loco_move_and_watchdog", displacement_m=displacement, commanded_vx_mps=.3, publisher_gid=gid,
                    commands=stream.count, maximum_publisher_gap_seconds=stream.max_gap)
        require(client.SetFsmId(1) == 0 and client.GetFsmId() == (0, 1), "Damp did not change queried FSM")
        require(self.api.status()["ros_commands"]["owner"] is None, "Damp retained ownership")
        self.report("sdk_loco_damp_and_unsupported_apis")
        self.reset()
        client, _ = self.loco()
        require(client.SetFsmId(0) == 0 and client.GetFsmId() == (0, 0), "ZeroTorque did not change queried FSM")
        require(self.api.status()["mode"] == "zero_torque", "ZeroTorque reported success without changing control")
        self.report("sdk_loco_zero_torque")

    def low_level(self):
        self.reset()
        from unitree_sdk2py.core.channel import ChannelPublisher
        from unitree_sdk2py.idl.default import unitree_hg_msg_dds__LowCmd_
        from unitree_sdk2py.idl.unitree_hg.msg.dds_ import LowCmd_
        measured = self.fresh()
        command = unitree_hg_msg_dds__LowCmd_()
        command.mode_pr, command.mode_machine = 0, measured.mode_machine
        for index in range(29):
            motor = command.motor_cmd[index]
            motor.mode, motor.q = 1, measured.motor_state[index].q
            motor.dq, motor.kp, motor.kd = 0., 100., 4.
            motor.tau = measured.motor_state[index].tau_est
        command.motor_cmd[19].q += .1
        command.crc = self.crc.Crc(command)
        before_sources = sources(self.api)
        publisher = ChannelPublisher("rt/lowcmd", LowCmd_)
        publisher.Init()
        def send(value):
            require(publisher.Write(value), "SDK HG LowCmd publish failed")
        stream = Stream(send, .01).start(command)
        self.streams.append(stream)
        gid = grant_new_source(self.api, before_sources, "lowcmd")
        before = self.api.status()
        time.sleep(.35)
        after = self.api.status()
        require(stream.error is None and after["ros_commands"]["owner"] == gid
                and after["mode"] == "lowlevel", "low-level grant failed or robot fell")
        require(after["metrics"]["policy_updates"] == before["metrics"]["policy_updates"],
                "walking policy continued under native low-level control")
        wrist_delta = after["joints"]["q"][19] - before["joints"]["q"][19]
        require(wrist_delta > .015, "SDK LowCmd did not physically move the selected wrist joint")
        stream.stop()
        self.streams.remove(stream)
        stopped = wait_until(lambda: state if (state := self.api.status())["mode"] == "damping"
                             and state["ros_commands"]["owner"] is None else None,
                             "low-level watchdog failed to revoke ownership", timeout=.3)
        require(stopped["time"] > after["time"], "physics stopped on low-level stream loss")
        self.report("sdk_lowcmd_pr_joint_motion_and_watchdog", wrist_delta_rad=wrist_delta,
                    publisher_gid=gid, packets=stream.count, maximum_publisher_gap_seconds=stream.max_gap)

    def close(self):
        try:
            for stream in self.streams:
                stream.stop()
        finally:
            try:
                self.api.post("/api/disarm_ros")
            finally:
                self.subscriber.Close()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--url", default="http://127.0.0.1:8890")
    parser.add_argument("--managed-vm")
    parser.add_argument("--source-digest")
    parser.add_argument("--output", type=Path)
    args = parser.parse_args()
    api = SimulatorAPI(args.url, vm_name=args.managed_vm, source_digest=args.source_digest)
    identity = require_test_environment(api)  # Validate before importing SDK transport or mutations.
    require(identity.get("healthy") is True and identity.get("ready") is True, "virtual robot is not ready")
    started, harness = time.monotonic(), None
    report = {"passed": False, "source_digest": api.source_digest, "vm_name": api.vm_name,
              "simulation": True, "robot_kind": "g1", "checks": [],
              "harness_sha256": {p.name: hashlib.sha256(p.read_bytes()).hexdigest()
                                  for p in (Path(__file__), Path(__file__).with_name("common.py"))}}
    try:
        harness = Harness(api)
        harness.interfaces()
        harness.motion()
        harness.low_level()
        report["passed"] = True
    except Exception as error:
        report["error"] = f"{type(error).__name__}: {error}"
    finally:
        if harness:
            report["checks"] = harness.results
            try:
                harness.close()
            except Exception as error:
                report["passed"] = False
                report["cleanup_error"] = str(error)
        report["wall_seconds"] = time.monotonic() - started
        if args.output:
            args.output.parent.mkdir(parents=True, exist_ok=True)
            args.output.write_text(json.dumps(report, indent=2, allow_nan=False) + "\n")
        print(json.dumps(report, allow_nan=False), flush=True)
    return 0 if report["passed"] else 1


if __name__ == "__main__":
    raise SystemExit(main())
