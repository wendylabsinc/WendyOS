"""Pinned Unitree sport request/response and normalized low-level commands."""

import json
import math
import queue

import numpy as np
from rclpy.serialization import deserialize_message
from unitree_api.msg import Request, Response
from unitree_go.msg import LowCmd

from .simulation import LOW_LEVEL_TIMEOUT
from .unitree_crc import low_cmd_crc


SDK_SLOTS = [3, 4, 5, 0, 1, 2, 9, 10, 11, 6, 7, 8]
POSITION_STOP = float(np.float32(2.146e9))
VELOCITY_STOP = 16000.0
API_VERSION = "1.0.0.1"
UNSUPPORTED, INVALID, DENIED = 3203, 3204, 3205


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
            return 0, API_VERSION, False
        if kind == "motion_switcher":
            if api == 1001:
                return 0, json.dumps({"name": "sport" if sim.control_mode == "sport" else ""}), False
            # The profile selects ownership explicitly. Stock motion-switcher
            # release/select orchestration is not advertised by this subset.
            return UNSUPPORTED, "motion-switcher mutations are unsupported", False
        if api not in {1001, 1002, 1003, 1004, 1005, 1008}:
            return UNSUPPORTED, "unsupported sport API", False
        if request.header.lease.id != 0 or request.header.policy.priority != 0:
            return DENIED, "leases and command priorities are unsupported", False
        if not owned:
            return DENIED, "select this publisher and enable ROS control in the simulator", False
        token = self.runtime.ros_commands.token
        try:
            if request.binary:
                raise ValueError("binary sport parameters are unsupported")
            if api == 1008:
                values = json.loads(request.parameter)
                if not isinstance(values, dict) or set(values) != {"x", "y", "z"}:
                    raise ValueError("Move requires x, y, z velocity fields")
                self.runtime.command([values[axis] for axis in ("x", "y", "z")], token)
                sim._last_received = received_ns / 1e9
            elif api == 1001:
                sim.damp(token)
            elif api == 1005:
                sim.stand_down(token)
            elif api == 1004:
                sim.stand_up(token)
            elif api == 1002:
                if sim.mode not in {"standing", "moving"}:
                    raise ValueError("BalanceStand requires a standing robot")
                sim.stop(token)
            else:
                # BalanceStand uses the validated standing walking controller,
                # without the factory Euler/body-height/stance adjustment APIs.
                sim.stop(token)
            return 0, "", True
        except PermissionError as exc:
            return DENIED, str(exc), False
        except (ValueError, TypeError) as exc:
            return INVALID, str(exc), False

    def low_level(self, command, received_ns, source_timestamp_ns):
        sim = self.runtime.sim
        commands = self.runtime.ros_commands
        if (commands.clock() - received_ns >= int(LOW_LEVEL_TIMEOUT * 1e9) or
                commands.wall_clock() - source_timestamp_ns >= int(LOW_LEVEL_TIMEOUT * 1e9)):
            raise ValueError("expired low-level ingress command")
        if list(command.head) != [0xFE, 0xEF] or command.level_flag != 0xFF:
            raise ValueError("invalid Go2 low-level header")
        if low_cmd_crc(command) != command.crc:
            raise ValueError("Go2 LowCmd CRC mismatch")
        if command.bms_cmd.off != 0 or command.gpio != 0:
            raise ValueError("BMS and GPIO commands are unsupported")
        for index, motor in enumerate(command.motor_cmd):
            if motor.mode not in (0, 1):
                raise ValueError("unsupported motor mode")
            if not all(math.isfinite(value) for value in (motor.q, motor.dq, motor.kp, motor.kd, motor.tau)):
                raise ValueError("non-finite motor command")
            if not 0 <= motor.kp <= 100 or not 0 <= motor.kd <= 10:
                raise ValueError("motor gains exceed simulator limits")
            if index >= 12 and (motor.kp != 0 or motor.kd != 0 or motor.tau != 0):
                raise ValueError("inactive motor slots must have zero gains and torque")
        q, dq, kp, kd, tau, active = [], [], [], [], [], []
        for policy_index, slot in enumerate(SDK_SLOTS):
            motor = command.motor_cmd[slot]
            enabled = motor.mode == 1
            position_enabled = enabled and motor.q != POSITION_STOP and motor.kp != 0
            velocity_enabled = enabled and motor.dq != VELOCITY_STOP and motor.kd != 0
            q.append(float(motor.q) if position_enabled else float(sim.policy.default[policy_index]))
            dq.append(float(motor.dq) if velocity_enabled else 0.0)
            kp.append(float(motor.kp) if position_enabled else 0.0)
            kd.append(float(motor.kd) if velocity_enabled else 0.0)
            tau.append(float(motor.tau) if enabled else 0.0)
            active.append(enabled)
        sim.command_low_level(q, dq, kp, kd, tau, active, token=commands.token)
        sim._low_level_received = received_ns / 1e9
