"""Native state layout, physical observations, and the pinned SDK CRC oracle.

Tests use generated ROS types and their real CDR serializer with a recording
publisher sink. No DDS participant or SDK transport is initialized. MuJoCo and
the walking policy supply the measured joint, IMU, and contact state.
"""

import ast
from collections import deque
import struct
import threading
import time
from types import SimpleNamespace
import unittest

import mujoco
import numpy as np

from go2_sim.sensors import PhysicsSampler, instrumented_model
from go2_sim.simulation import DEFAULT_ASSETS, Simulation

try:
    from builtin_interfaces.msg import Time
    from rclpy.qos import DurabilityPolicy, ReliabilityPolicy
    from rclpy.serialization import deserialize_message, serialize_message
    from unitree_go.msg import LowState, MotorState, SportModeState
    from go2_sim.native_state import NativeState, POLICY_TO_SDK, SIMULATION_SERIAL, SIMULATION_VERSION
except ImportError:
    NativeState = None


def sdk_crc_oracle():
    """Compile only three pure SDK methods, excluding imports and __init__.

    The SDK constructor loads a shared object and its imports load DDS types.
    Neither is needed for its independent Python packer/bitwise CRC oracle.
    """
    source = DEFAULT_ASSETS / "sdk2_python/unitree_sdk2py/utils/crc.py"
    tree = ast.parse(source.read_text())
    sdk_class = next(item for item in tree.body if isinstance(item, ast.ClassDef) and item.name == "CRC")
    methods = [method for method in sdk_class.body if isinstance(method, ast.FunctionDef)
               and method.name in {"__PackLowState", "__Trans", "_crc_py"}]
    for method in methods:
        method.returns = None
        for argument in method.args.args:
            argument.annotation = None
    constructor = next(method for method in sdk_class.body
                       if isinstance(method, ast.FunctionDef) and method.name == "__init__")
    pack_format = next(item.value for item in constructor.body if isinstance(item, ast.Assign)
                       and any(isinstance(target, ast.Attribute) and target.attr == "__packFmtLowState"
                               for target in item.targets))
    pure_class = ast.ClassDef(name="CRC", bases=[], keywords=[], body=methods, decorator_list=[])
    namespace = {"struct": struct}
    exec(compile(ast.fix_missing_locations(ast.Module(body=[pure_class], type_ignores=[])),
                 str(source), "exec"), namespace)
    oracle = namespace["CRC"]()
    oracle._CRC__packFmtLowState = eval(compile(ast.Expression(body=pack_format), str(source), "eval"), {})
    assert struct.calcsize(oracle._CRC__packFmtLowState) == 1180
    return lambda message: oracle._crc_py(oracle._CRC__PackLowState(message))


class RecordingPublisher:
    def __init__(self, message_type, qos):
        self.message_type, self.qos = message_type, qos
        self.packets = deque(maxlen=1)
        self.count = 0

    def publish(self, message):
        self.packets.append(serialize_message(message))
        self.count += 1

    @property
    def last(self):
        return deserialize_message(self.packets[-1], self.message_type)


class RecordingNode:
    def __init__(self):
        self.publishers = {}

    def create_publisher(self, message_type, topic, qos):
        result = RecordingPublisher(message_type, qos)
        self.publishers[topic] = result
        return result


@unittest.skipIf(NativeState is None, "generated Unitree ROS types are available in the ROS image")
class NativeStateTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.sim = Simulation(model=instrumented_model(DEFAULT_ASSETS))
        for _ in range(500):
            cls.sim.step()
        # Small, distinct physical encoder/velocity/control values expose a
        # wrong slot mapping instead of letting symmetric standing legs pass.
        cls.sim.data.qpos[cls.sim.qaddr] += np.arange(12) * 0.001
        cls.sim.data.qvel[cls.sim.vaddr] = np.arange(1, 13) * 0.01
        cls.sim.data.ctrl[cls.sim.actuators] = np.arange(1, 13) * 0.1
        mujoco.mj_forward(cls.sim.model, cls.sim.data)
        cls.runtime = SimpleNamespace(sim=cls.sim, lock=threading.RLock())
        cls.sampler = PhysicsSampler(cls.sim)
        cls.sampler.capture(cls.runtime)
        cls.state = cls.sampler.state()
        cls.feet = cls.sampler.feet()
        cls.oracle = staticmethod(sdk_crc_oracle())

    def setUp(self):
        self.node = RecordingNode()
        self.publisher = NativeState(self.node, self.runtime)
        self.stamp = Time(sec=123, nanosec=456789)

    def publish(self, *, at=None, epoch=None, mode="standing", control_mode="sport"):
        state = dict(self.state)
        if at is not None:
            state["time"] = at
        if epoch is not None:
            state["epoch"] = epoch
        self.publisher.publish(self.sampler, state, self.stamp, [0.25, -0.5, 0.35], mode, control_mode)
        return self.node.publishers["/lowstate"].last

    def test_native_cdr_uses_all_twenty_slots_and_sdk_crc_of_measured_state(self):
        low = self.publish()
        self.assertIsInstance(low, LowState)
        self.assertEqual(len(low.motor_state), 20)
        for policy, sdk in enumerate(POLICY_TO_SDK):
            motor = low.motor_state[sdk]
            self.assertEqual(motor.mode, 1)
            for field, key in (("q", "joint_position"), ("dq", "joint_velocity"),
                               ("ddq", "joint_acceleration"), ("tau_est", "joint_effort")):
                self.assertAlmostEqual(getattr(motor, field), float(np.float32(self.state[key][policy])), places=5)
            self.assertEqual((motor.q_raw, motor.dq_raw, motor.ddq_raw), (motor.q, motor.dq, motor.ddq))
        for motor in low.motor_state[12:]:
            self.assertEqual(motor, MotorState())
        self.assertEqual(low.crc, self.oracle(low))
        changed = self.publish(at=self.state["time"] + 0.002)
        self.assertEqual(changed.crc, self.oracle(changed))
        self.assertNotEqual(changed.crc, low.crc)
        self.assertEqual(changed.tick - low.tick, 2)

    def test_measured_imu_feet_and_odometry_are_serialized_in_native_order(self):
        low = self.publish(mode="moving")
        sport = self.node.publishers["/sportmodestate"].last
        self.assertIsInstance(sport, SportModeState)
        np.testing.assert_allclose(low.imu_state.quaternion, self.state["quaternion_wxyz"], atol=1e-7)
        np.testing.assert_allclose(low.imu_state.gyroscope, self.state["angular_velocity_body"], atol=1e-7)
        np.testing.assert_allclose(low.imu_state.accelerometer, self.state["specific_force_body"], rtol=1e-6)
        self.assertEqual(sport.imu_state, low.imu_state)
        expected_force = [max(0, min(32767, round(float(self.feet["force"][index])))) for index in (1, 0, 3, 2)]
        self.assertGreater(sum(expected_force), 0, "fixture must have real foot contacts")
        self.assertEqual(list(low.foot_force), expected_force)
        self.assertEqual(list(low.foot_force_est), expected_force)
        self.assertEqual(list(sport.foot_force), expected_force)
        np.testing.assert_allclose(np.array(sport.foot_position_body).reshape(4, 3),
                                   self.feet["position_body"][[1, 0, 3, 2]], atol=1e-7)
        np.testing.assert_allclose(np.array(sport.foot_speed_body).reshape(4, 3),
                                   self.feet["velocity_body"][[1, 0, 3, 2]], atol=1e-7)
        np.testing.assert_allclose(sport.position, [0.25, -0.5, 0.35], atol=1e-7)
        np.testing.assert_allclose(sport.velocity, self.state["linear_velocity_body"], atol=1e-7)
        self.assertEqual((sport.stamp.sec, sport.stamp.nanosec), (123, 456789))
        self.assertAlmostEqual(sport.body_height, self.state["position"][2], places=6)
        self.assertEqual(list(sport.range_obstacle), [0.0] * 4)

    def test_battery_and_identification_are_explicitly_synthetic(self):
        low = self.publish()
        self.assertEqual(list(low.head), [0xFE, 0xEF])
        self.assertEqual(low.level_flag, 0xFF)
        self.assertEqual(tuple(low.sn), SIMULATION_SERIAL)
        self.assertEqual(tuple(low.version), SIMULATION_VERSION)
        self.assertEqual(low.bms_state.soc, 90)
        self.assertEqual(list(low.bms_state.cell_vol), [3600] * 8 + [0] * 7)
        self.assertAlmostEqual(sum(low.bms_state.cell_vol) / 1000, low.power_v, places=5)
        self.assertEqual(low.bms_state.current, 0)

    def test_lowstate_each_step_and_lf_sport_follow_fifty_hz_source_grid(self):
        costs = []
        for tick in range(500):
            state = {**self.state, "time": tick * 0.002}
            started = time.perf_counter()
            self.publisher.publish(self.sampler, state, self.stamp, [0.25, -0.5, 0.35], "standing", "sport")
            costs.append((time.perf_counter() - started) * 1000)
        print(f"NativeState measured conversion/CRC/CDR: mean={np.mean(costs):.3f}ms "
              f"p95={np.percentile(costs, 95):.3f}ms over 500 calls (no DDS transport)", flush=True)
        self.assertEqual(self.publisher.samples,
                         {"lowstate": 500, "lf_lowstate": 50,
                          "sportmodestate": 50, "lf_sportmodestate": 50})
        for topic, target in (("/lowstate", 500), ("/lf/lowstate", 50),
                               ("/sportmodestate", 50), ("/lf/sportmodestate", 50)):
            recorded = self.node.publishers[topic]
            self.assertEqual(recorded.count, target)
            self.assertEqual(recorded.qos.depth, 1)
            self.assertEqual(recorded.qos.reliability, ReliabilityPolicy.RELIABLE)
            self.assertEqual(recorded.qos.durability, DurabilityPolicy.VOLATILE)
        # A late caller emits one current low-frequency sample, not stale
        # catch-up frames. A new epoch restarts the grid immediately.
        self.publish(at=4.007)
        self.assertEqual(self.publisher.samples["lf_lowstate"], 51)
        self.publish(at=0.002, epoch=self.state["epoch"] + 1)
        self.assertEqual(self.publisher.samples["lf_lowstate"], 52)

    def test_sport_state_is_suppressed_when_sport_control_is_released(self):
        self.publish(at=0.0)
        for tick, control in enumerate(("lowlevel", None, "lowlevel"), 1):
            self.publish(at=tick * 0.02, mode="damping", control_mode=control)
        self.assertEqual(self.publisher.samples["lowstate"], 4)
        self.assertEqual(self.publisher.samples["lf_lowstate"], 4)
        self.assertEqual(self.publisher.samples["sportmodestate"], 1)
        self.publish(at=0.08)
        self.assertEqual(self.publisher.samples["sportmodestate"], 2)

    def test_posture_and_gait_modes_follow_upstream_table(self):
        for index, (mode, expected) in enumerate((("standing", 0), ("balance_stand", 1),
                                                ("moving", 3), ("standing_down", 5),
                                                ("lying", 5), ("standing_up", 6),
                                                ("damping", 7), ("fallen", 7), ("fault", 7))):
            self.publish(at=index * 0.02, mode=mode)
            sport = self.node.publishers["/sportmodestate"].last
            self.assertEqual(sport.mode, expected)
            self.assertEqual(sport.gait_type, 1 if mode == "moving" else 0)
            self.assertEqual(sport.progress, 0.0)


if __name__ == "__main__":
    unittest.main()
