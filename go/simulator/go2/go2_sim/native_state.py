"""Native Unitree observations for the pinned Go2 virtual profile.

Joints, quaternion (wxyz), gyro, specific force, and feet come from one copied
MuJoCo state. Policy order FL/FR/RL/RR is converted to SDK order FR/FL/RR/RL.
Raw joint values alias ideal simulated encoders; estimated and measured foot
loads alias the same rounded/saturated contact load in newtons. Sport position
uses the caller's odometry estimate; velocity is body-frame, as in the standard
Odometry.twist stream. These mounts/estimates are not factory calibration.

The battery is an ideal constant 28.8V, 90% SOC source: eight 3600mV cells and
seven unused zero cells. Current, temperature, cycle, and power draw are not
modeled. Serial/version words explicitly identify simulation, not a real unit.
Unknown hardware fields, obstacle ranges, and commanded foot height remain
zero. Sport progress is zero because no dance is implemented. Native modes use
the upstream table: default stand 0, balance stand 1, locomotion 3, lie down 5,
joint lock 6 (standing up), damping 7. Sport state is absent outside sport control.

The caller owns scheduling and lifecycle fencing. Each call emits /lowstate;
LF LowState and both Sport topics emit on a 50Hz source-time grid. No timers,
physics mutation, policy execution, or DDS participant is created here.
"""

import math

from rclpy.qos import DurabilityPolicy, QoSProfile, ReliabilityPolicy
from unitree_go.msg import LowState, SportModeState

from .unitree_crc import low_state_crc


POLICY_TO_SDK = (3, 4, 5, 0, 1, 2, 9, 10, 11, 6, 7, 8)
SDK_FEET_FROM_POLICY = (1, 0, 3, 2)
SIMULATION_SERIAL = (int.from_bytes(b"WEND", "little"), int.from_bytes(b"SIM1", "little"))
SIMULATION_VERSION = (int.from_bytes(b"SIM\0", "little"), 1)
SPORT_MODES = {"standing": 0, "balance_stand": 1, "moving": 3,
               "standing_down": 5, "lying": 5, "standing_up": 6,
               "damping": 7, "fallen": 7, "fault": 7}


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
        self.sport_pub = node.create_publisher(SportModeState, "/sportmodestate", qos)
        self.lf_sport_pub = node.create_publisher(SportModeState, "/lf/sportmodestate", qos)
        self.low = LowState()
        self.low.head = [0xFE, 0xEF]
        self.low.level_flag = 0xFF
        self.low.sn = list(SIMULATION_SERIAL)
        self.low.version = list(SIMULATION_VERSION)
        self.low.bms_state.soc = 90
        self.low.bms_state.cell_vol = [3600] * 8 + [0] * 7
        self.low.power_v = 28.8
        self.sport = SportModeState()
        self.epoch = None
        self.next_lf = 0.0
        self.samples = {"lowstate": 0, "lf_lowstate": 0,
                        "sportmodestate": 0, "lf_sportmodestate": 0}

    def publish(self, sampler, state, stamp, odom_position, mode, control_mode):
        if state["epoch"] != self.epoch:
            self.epoch = state["epoch"]
            self.next_lf = 0.0

        imu = self.low.imu_state
        imu.quaternion = list(map(float, state["quaternion_wxyz"]))
        imu.gyroscope = list(map(float, state["angular_velocity_body"]))
        imu.accelerometer = list(map(float, state["specific_force_body"]))
        imu.rpy = rpy(state["quaternion_wxyz"])
        for policy_index, sdk_index in enumerate(POLICY_TO_SDK):
            motor = self.low.motor_state[sdk_index]
            motor.mode = 1
            motor.q = motor.q_raw = float(state["joint_position"][policy_index])
            motor.dq = motor.dq_raw = float(state["joint_velocity"][policy_index])
            motor.ddq = motor.ddq_raw = float(state["joint_acceleration"][policy_index])
            motor.tau_est = float(state["joint_effort"][policy_index])
        lf_due = state["time"] + 1e-9 >= self.next_lf
        # LowState needs actual contact loads at 500 Hz. Relative positions and
        # velocities belong to SportModeState, emitted at 50 Hz only.
        feet = sampler.feet(include_kinematics=lf_due and control_mode == "sport")
        force = [max(0, min(32767, round(float(feet["force"][index]))))
                 for index in SDK_FEET_FROM_POLICY]
        self.low.foot_force = force
        self.low.foot_force_est = force
        self.low.tick = round(state["time"] * 1000) & 0xFFFFFFFF
        self.low.crc = low_state_crc(self.low)
        self.low_pub.publish(self.low)
        self.samples["lowstate"] += 1

        if not lf_due:
            return
        self.next_lf = (math.floor((state["time"] + 1e-9) / 0.02) + 1) * 0.02
        self.lf_low_pub.publish(self.low)
        self.samples["lf_lowstate"] += 1
        if control_mode != "sport":
            return
        sport = self.sport
        sport.stamp.sec, sport.stamp.nanosec = stamp.sec, stamp.nanosec
        sport.imu_state = imu
        sport.mode = SPORT_MODES.get(mode, 0)
        sport.gait_type = 1 if mode == "moving" else 0
        sport.position = list(map(float, odom_position))
        sport.body_height = float(state["position"][2])
        sport.velocity = list(map(float, state["linear_velocity_body"]))
        sport.yaw_speed = float(state["angular_velocity_body"][2])
        sport.foot_force = force
        sport.foot_position_body = [float(value) for index in SDK_FEET_FROM_POLICY
                                    for value in feet["position_body"][index]]
        sport.foot_speed_body = [float(value) for index in SDK_FEET_FROM_POLICY
                                 for value in feet["velocity_body"][index]]
        self.sport_pub.publish(sport)
        self.lf_sport_pub.publish(sport)
        self.samples["sportmodestate"] += 1
        self.samples["lf_sportmodestate"] += 1
