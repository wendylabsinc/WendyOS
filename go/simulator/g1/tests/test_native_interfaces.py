"""Actual HG ROS CDR/SDK CRC and adapter semantics, without DDS or physics.

These are interface unit tests. The independent integration/native exercise
checks real SDK transport and MuJoCo motion against a separate runtime.
"""

from collections import deque
import importlib
import json
import math
import struct
import sys
from types import ModuleType, SimpleNamespace
import unittest
from unittest.mock import patch

from rclpy.serialization import deserialize_message, serialize_message
from unitree_api.msg import Request
from unitree_hg.msg import LowCmd, LowState, MotorState
from unitree_sdk2py.idl.unitree_hg.msg.dds_ import LowCmd_ as SDKLowCmd, LowState_ as SDKLowState
from unitree_sdk2py.utils.crc import CRC

from g1_sim.native_state import NativeState, SIMULATION_VERSION
from g1_sim.unitree_crc import low_cmd_crc, low_cmd_record, low_state_crc, low_state_record

# The adapters depend only on these two timing constants from simulation.
# Loading MuJoCo/policy is deliberately outside this interface test's scope.
constants = ModuleType("g1_sim.simulation")
constants.COMMAND_TIMEOUT, constants.LOW_LEVEL_TIMEOUT = 0.2, 0.04
with patch.dict(sys.modules, {"g1_sim.simulation": constants}):
    adapter = importlib.import_module("g1_sim.native_commands")


def crc_oracle():
    with patch("unitree_sdk2py.utils.crc.platform.system", return_value="PythonFallback"):
        return CRC()


class Publisher:
    def __init__(self, kind):
        self.kind, self.packets, self.count = kind, deque(maxlen=1), 0

    def publish(self, message):
        self.packets.append(serialize_message(message))
        self.count += 1

    @property
    def last(self):
        return deserialize_message(self.packets[-1], self.kind)


class Node:
    def __init__(self):
        self.publishers = {}

    def create_publisher(self, kind, topic, qos):
        publisher = Publisher(kind)
        self.publishers[topic] = publisher
        return publisher

    def create_timer(self, period, callback):
        return (period, callback)


class NativeTests(unittest.TestCase):
    def setUp(self):
        self.node = Node()
        self.oracle = crc_oracle()

    def test_hg_crc_and_real_sdk_ros_cdr_roundtrips(self):
        command = LowCmd()
        for index, motor in enumerate(command.motor_cmd):
            motor.mode, motor.q, motor.dq = index % 2, index * 0.125, -index * 0.25
            motor.kp, motor.kd, motor.tau, motor.reserve = float(index), 0.5, -0.1, index
        command.reserve = [10, 20, 30, 40]
        command.crc = low_cmd_crc(command)
        sdk_command = SDKLowCmd.deserialize(serialize_message(command))
        self.assertEqual(command.crc, self.oracle.Crc(sdk_command))
        self.assertEqual(serialize_message(deserialize_message(sdk_command.serialize(), LowCmd)), serialize_message(command))
        self.assertEqual(len(low_cmd_record(command)), 1004)
        self.assertLess(len(serialize_message(command)) * 2 + 256, 4096)
        state = LowState()
        state.version, state.mode_pr, state.mode_machine, state.tick = [1, 2], 0, 0, 123
        state.imu_state.quaternion = [1., 0., 0., 0.]
        state.imu_state.temperature = -25
        for index, motor in enumerate(state.motor_state):
            motor.q, motor.dq, motor.ddq, motor.tau_est = index * .125, -.25, .5, -.75
            motor.temperature, motor.vol = [-index, index], 48.
            motor.sensor, motor.motorstate, motor.reserve = [index, index + 1], index, [1, 2, 3, 4]
        state.crc = low_state_crc(state)
        sdk_state = SDKLowState.deserialize(serialize_message(state))
        self.assertEqual(state.crc, self.oracle.Crc(sdk_state))
        self.assertEqual(serialize_message(deserialize_message(sdk_state.serialize(), LowState)), serialize_message(state))
        self.assertEqual(len(low_state_record(state)), 2092)

    def test_native_state_preserves_all29_measured_motors_and_imu(self):
        native = NativeState(self.node, SimpleNamespace())
        state = {"epoch": 1, "time": 1.234, "quaternion_wxyz": [1., 0., 0., 0.],
                 "angular_velocity_body": [.1, -.2, .3], "specific_force_body": [0., 0., 9.81]}
        for offset, key in enumerate(("joint_position", "joint_velocity", "joint_acceleration", "joint_effort")):
            state[key] = [index * .03125 + offset for index in range(29)]
        native.publish(None, state, None, None, "standing", "sport")
        low = self.node.publishers["/lowstate"].last
        self.assertEqual(set(self.node.publishers), {"/lowstate", "/lf/lowstate"})
        self.assertEqual(tuple(low.version), SIMULATION_VERSION)
        self.assertEqual((low.mode_pr, low.mode_machine, low.tick), (0, 0, 1234))
        self.assertEqual(low.crc, self.oracle.Crc(SDKLowState.deserialize(serialize_message(low))))
        for index, motor in enumerate(low.motor_state[:29]):
            for field, key in (("q", "joint_position"), ("dq", "joint_velocity"),
                               ("ddq", "joint_acceleration"), ("tau_est", "joint_effort")):
                self.assertAlmostEqual(getattr(motor, field), state[key][index], places=6)
        self.assertTrue(all(motor == MotorState() for motor in low.motor_state[29:]))
        self.assertAlmostEqual(low.imu_state.accelerometer[2], 9.81, places=5)
        for tick in range(500):
            native.publish(None, {**state, "epoch": 2, "time": tick * .002}, None, None, "lowlevel", "lowlevel")
        self.assertEqual(native.samples, {"lowstate": 501, "lf_lowstate": 51})

    def commands(self):
        sim = SimpleNamespace(mode="standing", control_mode="sport", _before_pause="standing",
                              policy=SimpleNamespace(default=[.1] * 29), calls=[])
        def record(name):
            def call(*args, **kwargs):
                sim.calls.append((name, args, kwargs))
                if name in {"damp", "zero_torque"}:
                    sim.mode, sim.control_mode = ("damping" if name == "damp" else "zero_torque"), None
            return call
        sim.damp, sim.zero_torque, sim.stop = record("damp"), record("zero_torque"), record("stop")
        sim.command_low_level = record("lowlevel")
        ingress = SimpleNamespace(token="grant", clock=lambda: 100_000_000_000,
                                  wall_clock=lambda: 200_000_000_000)
        runtime = SimpleNamespace(sim=sim, ros_commands=ingress, command=record("velocity"))
        return adapter.NativeCommands(self.node, runtime), sim

    def test_loco_fsm_velocity_duration_and_explicit_unsupported_operations(self):
        native, sim = self.commands()
        def call(api, value, owned=True):
            request = Request(parameter=json.dumps(value))
            request.header.identity.api_id = api
            return native.request("sport", request, owned, 100_000_000_000)
        self.assertEqual(call(7001, {}, False), (0, '{"data": 500}', False))
        self.assertEqual(call(7105, {"velocity": [.2, 0., 0.], "duration": 864000})[0], 0)
        self.assertEqual(sim._last_received, 100.)
        self.assertEqual(call(7105, {"velocity": [0., 0., 0.], "duration": .05})[0], 0)
        self.assertAlmostEqual(sim._last_received, 99.85)
        for duration in (True, 0, -1, math.nan, math.inf, 864001):
            self.assertEqual(call(7105, {"velocity": [0., 0., 0.], "duration": duration})[0], 3204)
        for api in (1001, 1008, 7002, 7102, 7104, 7106, 7110, 7111):
            self.assertEqual(call(api, {})[0], 3203)
        for fsm in (3, 702, 706):
            self.assertEqual(call(7101, {"data": fsm})[0], 3203)
        self.assertEqual(call(7101, {"data": True})[0], 3204)
        self.assertEqual(call(7101, {"data": 500}, False)[0], 3205)
        self.assertEqual(call(7101, {"data": 500})[0], 0)
        self.assertEqual(call(7101, {"data": 1})[0], 0)
        self.assertEqual(call(7001, {}, False), (0, '{"data": 1}', False))
        self.assertEqual(call(7101, {"data": 500})[0], 3204)
        self.assertEqual(call(7101, {"data": 0})[0], 0)
        self.assertEqual(call(7001, {}, False), (0, '{"data": 0}', False))

    def test_lowcmd_crc_pr_reserved_slots_and_normalization(self):
        native, sim = self.commands()
        command = LowCmd()
        for index, motor in enumerate(command.motor_cmd[:29]):
            motor.mode, motor.q, motor.dq = 1, index * .01, .5
            motor.kp, motor.kd = 300., 20.
        command.motor_cmd[0].q = adapter.POSITION_STOP
        command.motor_cmd[1].dq = adapter.VELOCITY_STOP
        command.crc = low_cmd_crc(command)
        native.low_level(command, 100_000_000_000, 200_000_000_000)
        name, args, kwargs = sim.calls[-1]
        self.assertEqual(name, "lowlevel")
        self.assertTrue(all(len(value) == 29 for value in args))
        self.assertEqual((args[0][0], args[2][0], args[1][1], args[3][1]), (.1, 0., 0., 0.))
        self.assertEqual(kwargs, {"token": "grant"})
        for field, value in (("mode_pr", 1), ("mode_machine", 29), ("crc", 0)):
            bad = deserialize_message(serialize_message(command), LowCmd)
            setattr(bad, field, value)
            if field != "crc": bad.crc = low_cmd_crc(bad)
            with self.assertRaises(ValueError): native.low_level(bad, 100_000_000_000, 200_000_000_000)
        for index, field, value in ((29, "mode", 1), (34, "tau", .1), (0, "kp", 300.1), (1, "reserve", 1)):
            bad = deserialize_message(serialize_message(command), LowCmd)
            setattr(bad.motor_cmd[index], field, value)
            bad.crc = low_cmd_crc(bad)
            with self.assertRaises(ValueError): native.low_level(bad, 100_000_000_000, 200_000_000_000)
        with self.assertRaises(ValueError): native.low_level(command, 99_950_000_000, 200_000_000_000)
        self.assertEqual(len(sim.calls), 1)

    def test_serialized_rpc_response_preserves_identity_and_noreply(self):
        native, sim = self.commands()
        request = Request(parameter="{}")
        request.header.identity.id, request.header.identity.api_id = 12345, 7001
        envelope = {"kind": "sport", "payload_hex": serialize_message(request).hex(), "received_ns": 100_000_000_000}
        self.assertFalse(native.receive(envelope, owned=False))
        native.flush()
        response = self.node.publishers["/api/sport/response"].last
        self.assertEqual((response.header.identity.id, response.header.identity.api_id), (12345, 7001))
        self.assertEqual((response.header.status.code, response.data), (0, '{"data": 500}'))
        request.header.policy.noreply = True
        envelope["payload_hex"] = serialize_message(request).hex()
        self.assertFalse(native.receive(envelope, owned=False))
        native.flush()
        self.assertEqual(self.node.publishers["/api/sport/response"].count, 1)
        self.assertEqual(sim.calls, [])


if __name__ == "__main__":
    unittest.main()
