"""Pinned Unitree HG observations from one immutable G1 physics sample.

The first 29 motors follow the upstream G1 PR joint order. Slots 29–34 and
unmodeled hardware temperature/voltage/remote/reserved fields remain zero.
mode_machine=0 is this simulation's declared value, not a hardware revision.
No Go2 SportModeState or factory BMS is synthesized. Standard odometry and
GetFsmId provide the separately documented locomotion feedback.
"""

import math

from rclpy.qos import DurabilityPolicy, QoSProfile, ReliabilityPolicy
from unitree_hg.msg import LowState

from .unitree_crc import low_state_crc


MOTOR_COUNT = 29
MODE_MACHINE = 0
SIMULATION_VERSION = (int.from_bytes(b"SIM\0", "little"), 1)


def rpy(wxyz):
    w, x, y, z = map(float, wxyz)
    return [math.atan2(2 * (w*x + y*z), 1 - 2 * (x*x + y*y)),
            math.asin(max(-1.0, min(1.0, 2 * (w*y - z*x)))),
            math.atan2(2 * (w*z + x*y), 1 - 2 * (y*y + z*z))]


class NativeState:
    def __init__(self, node, runtime):
        self.runtime = runtime
        qos = QoSProfile(depth=1, reliability=ReliabilityPolicy.RELIABLE,
                         durability=DurabilityPolicy.VOLATILE)
        self.low_pub = node.create_publisher(LowState, "/lowstate", qos)
        self.lf_low_pub = node.create_publisher(LowState, "/lf/lowstate", qos)
        self.low = LowState()
        self.low.version = list(SIMULATION_VERSION)
        self.low.mode_pr = 0
        self.low.mode_machine = MODE_MACHINE
        self.epoch = None
        self.next_lf = 0.0
        self.samples = {"lowstate": 0, "lf_lowstate": 0}

    def publish(self, sampler, state, stamp, odom_position, mode, control_mode):
        if state["epoch"] != self.epoch:
            self.epoch = state["epoch"]
            self.next_lf = 0.0
        imu = self.low.imu_state
        imu.quaternion = list(map(float, state["quaternion_wxyz"]))
        imu.gyroscope = list(map(float, state["angular_velocity_body"]))
        imu.accelerometer = list(map(float, state["specific_force_body"]))
        imu.rpy = rpy(state["quaternion_wxyz"])
        for index in range(MOTOR_COUNT):
            motor = self.low.motor_state[index]
            motor.mode = 0 if mode == "zero_torque" else 1
            motor.q = float(state["joint_position"][index])
            motor.dq = float(state["joint_velocity"][index])
            motor.ddq = float(state["joint_acceleration"][index])
            motor.tau_est = float(state["joint_effort"][index])
        self.low.tick = round(state["time"] * 1000) & 0xFFFFFFFF
        self.low.crc = low_state_crc(self.low)
        self.low_pub.publish(self.low)
        self.samples["lowstate"] += 1
        if state["time"] + 1e-9 < self.next_lf:
            return
        self.next_lf = (math.floor((state["time"] + 1e-9) / 0.02) + 1) * 0.02
        self.lf_low_pub.publish(self.low)
        self.samples["lf_lowstate"] += 1
