"""Pinned G1 Loco RPC and PR low-level command admission.

The transport kind stays ``sport`` because that is the upstream Loco service
name. Only explicitly implemented API IDs succeed; factory posture, arm-task,
height, speed-mode and control-switch orchestration are not emulated.
"""

import json
import math
import queue
import struct

from rclpy.serialization import deserialize_message
from unitree_api.msg import Request, Response
from unitree_hg.msg import LowCmd

from .simulation import COMMAND_TIMEOUT, LOW_LEVEL_TIMEOUT
from .unitree_crc import low_cmd_crc


MOTOR_COUNT = 29
MODE_MACHINE = 0
POSITION_STOP = struct.unpack("<f", struct.pack("<f", 2.146e9))[0]
VELOCITY_STOP = 16000.0
API_VERSION = "1.0.0.0"
MOTION_SWITCHER_VERSION = "1.0.0.1"
UNSUPPORTED, INVALID, DENIED = 3203, 3204, 3205


def fsm_id(sim):
    mode = sim._before_pause if sim.mode == "paused" else sim.mode
    if mode in {"standing", "moving"}:
        return 500
    if mode in {"zero_torque", "lowlevel"}:
        return 0
    return 1


class NativeCommands:
    def __init__(self, node, runtime):
        self.runtime = runtime
        self.responses = queue.Queue(maxsize=128)
        self.publishers = {kind: node.create_publisher(Response, f"/api/{kind}/response", 10)
                           for kind in ("sport", "motion_switcher")}
        self.timer = node.create_timer(0.005, self.flush)

    def flush(self):
        for _ in range(32):
            try:
                kind, response = self.responses.get_nowait()
            except queue.Empty:
                return
            self.publishers[kind].publish(response)

    def receive(self, envelope, *, owned):
        kind = envelope["kind"]
        payload_hex = envelope.get("payload_hex")
        if kind not in {"sport", "motion_switcher", "lowcmd"}:
            raise ValueError("invalid G1 native command kind")
        if not isinstance(payload_hex, str) or len(payload_hex) > 3840:
            raise ValueError("invalid native command payload")
        payload = bytes.fromhex(payload_hex)
        message = deserialize_message(payload, LowCmd if kind == "lowcmd" else Request)
        if kind == "lowcmd":
            if not owned:
                return False
            self.low_level(message, envelope["received_ns"], envelope["source_timestamp_ns"])
            return True
        code, data, applied = self.request(kind, message, owned, envelope["received_ns"])
        if not message.header.policy.noreply:
            response = Response()
            response.header.identity.id = message.header.identity.id
            response.header.identity.api_id = message.header.identity.api_id
            response.header.status.code = code
            response.data = data
            try:
                self.responses.put_nowait((kind, response))
            except queue.Full as exc:
                raise RuntimeError("native response queue full") from exc
        return applied

    def request(self, kind, request, owned, received_ns):
        api = request.header.identity.api_id
        sim = self.runtime.sim
        if api == 1:
            return 0, MOTION_SWITCHER_VERSION if kind == "motion_switcher" else API_VERSION, False
        if kind == "motion_switcher":
            if api == 1001:
                return 0, json.dumps({"name": "sport" if sim.control_mode == "sport" else ""}), False
            return UNSUPPORTED, "motion-switcher mutations are unsupported", False
        if api not in {7001, 7101, 7105}:
            return UNSUPPORTED, "unsupported G1 Loco API", False
        if request.header.lease.id != 0 or request.header.policy.priority != 0:
            return DENIED, "leases and command priorities are unsupported", False
        try:
            if request.binary:
                raise ValueError("binary Loco parameters are unsupported")
            values = json.loads(request.parameter)
            if not isinstance(values, dict):
                raise ValueError("Loco parameters must be an object")
            if api == 7001:
                if values:
                    raise ValueError("GetFsmId requires an empty object")
                return 0, json.dumps({"data": fsm_id(sim)}), False
            if not owned:
                return DENIED, "select this publisher and enable ROS control in the simulator", False
            token = self.runtime.ros_commands.token
            if api == 7105:
                if set(values) != {"velocity", "duration"}:
                    raise ValueError("SetVelocity requires velocity and duration")
                duration = values["duration"]
                if (isinstance(duration, bool) or not isinstance(duration, (int, float))
                        or not math.isfinite(duration) or not 0 < duration <= 864000):
                    raise ValueError("duration must be finite in (0,864000] seconds")
                self.runtime.command(values["velocity"], token)
                # Short requested durations reduce the lease. Long SDK Move
                # durations never bypass the profile's 200 ms stale stop.
                sim._last_received = received_ns / 1e9 - max(0.0, COMMAND_TIMEOUT - duration)
            else:
                if set(values) != {"data"} or type(values["data"]) is not int:
                    raise ValueError("SetFsmId requires one integer data field")
                requested = values["data"]
                if requested == 1:
                    sim.damp(token)
                elif requested == 0:
                    sim.zero_torque(token)
                elif requested == 500:
                    if sim.mode not in {"standing", "moving"} or sim.control_mode != "sport":
                        raise ValueError("Start requires a standing policy controller; reset to recover")
                    sim.stop(token)
                else:
                    return UNSUPPORTED, "unsupported G1 FSM transition", False
            return 0, "", True
        except PermissionError as exc:
            return DENIED, str(exc), False
        except (ValueError, TypeError, OverflowError) as exc:
            return INVALID, str(exc), False

    def low_level(self, command, received_ns, source_timestamp_ns):
        sim = self.runtime.sim
        commands = self.runtime.ros_commands
        if (commands.clock() - received_ns >= int(LOW_LEVEL_TIMEOUT * 1e9) or
                commands.wall_clock() - source_timestamp_ns >= int(LOW_LEVEL_TIMEOUT * 1e9)):
            raise ValueError("expired low-level ingress command")
        if command.mode_pr != 0:
            raise ValueError("G1 supports PR joint commands only; AB linkage commands are unsupported")
        if command.mode_machine != MODE_MACHINE:
            raise ValueError("G1 mode_machine does not match the published simulation value")
        if low_cmd_crc(command) != command.crc:
            raise ValueError("G1 LowCmd CRC mismatch")
        if any(command.reserve):
            raise ValueError("reserved LowCmd fields are unsupported")
        for index, motor in enumerate(command.motor_cmd):
            if motor.mode not in (0, 1) or motor.reserve != 0:
                raise ValueError("unsupported motor mode or reserved field")
            if not all(math.isfinite(value) for value in (motor.q, motor.dq, motor.kp, motor.kd, motor.tau)):
                raise ValueError("non-finite motor command")
            if not 0 <= motor.kp <= 300 or not 0 <= motor.kd <= 20:
                raise ValueError("motor gains exceed simulator limits")
            if index >= MOTOR_COUNT and (motor.mode != 0 or any(
                    value != 0 for value in (motor.q, motor.dq, motor.kp, motor.kd, motor.tau))):
                raise ValueError("inactive motor slots 29–34 must remain zero")
        q, dq, kp, kd, tau, active = [], [], [], [], [], []
        for index in range(MOTOR_COUNT):
            motor = command.motor_cmd[index]
            enabled = motor.mode == 1
            position_enabled = enabled and motor.q != POSITION_STOP and motor.kp != 0
            velocity_enabled = enabled and motor.dq != VELOCITY_STOP and motor.kd != 0
            q.append(float(motor.q) if position_enabled else float(sim.policy.default[index]))
            dq.append(float(motor.dq) if velocity_enabled else 0.0)
            kp.append(float(motor.kp) if position_enabled else 0.0)
            kd.append(float(motor.kd) if velocity_enabled else 0.0)
            tau.append(float(motor.tau) if enabled else 0.0)
            active.append(enabled)
        sim.command_low_level(q, dq, kp, kd, tau, active, token=commands.token)
        sim._low_level_received = received_ns / 1e9
