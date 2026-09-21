"""Go2's aligned native-record CRC, independent of ROS CDR serialization.

Layouts follow the pinned Unitree SDK contract recorded in UPSTREAM.md.
This is the non-reflected 0x04C11DB7 word algorithm. The accelerated path
reflects input/output bits around the standard library's compiled CRC; applying
crc32 directly to the native record would produce a different checksum.
"""

import array
import binascii
import struct


LOW_CMD_FORMAT = "<4B4IH2x" + "B3x5f3I" * 20 + "4B" + "55Bx2I"
LOW_STATE_FORMAT = "<4B4IH2x" + "13fb3x" + "B3x7fb3x3I" * 20 + "4BiH4b15H" + "8hI41B3xf2b2x2f4h2I"


def _table():
    values = []
    for byte in range(256):
        crc = byte << 24
        for _ in range(8):
            crc = ((crc << 1) ^ (0x04C11DB7 if crc & 0x80000000 else 0)) & 0xFFFFFFFF
        values.append(crc)
    return tuple(values)


TABLE = _table()
BIT_REVERSE = bytes(int(f"{value:08b}"[::-1], 2) for value in range(256))


def crc_words(data):
    # Unitree feeds each little-endian uint32 from its most significant bit.
    # Reverse the bytes within each word, then reflect each byte so the C CRC
    # implementation traverses precisely the same bit stream. Remove its final
    # XOR and reflect the result back to Unitree's non-reflected register.
    words = array.array("I", data)
    words.byteswap()
    reflected = binascii.crc32(words.tobytes().translate(BIT_REVERSE)) ^ 0xFFFFFFFF
    return int.from_bytes(reflected.to_bytes(4, "little").translate(BIT_REVERSE), "big")


def _header(msg):
    return [*msg.head, msg.level_flag, msg.frame_reserve, *msg.sn, *msg.version, msg.bandwidth]


def low_cmd_record(msg):
    values = _header(msg)
    for motor in msg.motor_cmd:
        values.extend([motor.mode, motor.q, motor.dq, motor.tau, motor.kp, motor.kd, *motor.reserve])
    values.extend([msg.bms_cmd.off, *msg.bms_cmd.reserve, *msg.wireless_remote,
                   *msg.led, *msg.fan, msg.gpio, msg.reserve, 0])
    return struct.pack(LOW_CMD_FORMAT, *values)


def low_state_record(msg):
    values = _header(msg)
    imu = msg.imu_state
    values.extend([*imu.quaternion, *imu.gyroscope, *imu.accelerometer, *imu.rpy, imu.temperature])
    for motor in msg.motor_state:
        values.extend([motor.mode, motor.q, motor.dq, motor.ddq, motor.tau_est,
                       motor.q_raw, motor.dq_raw, motor.ddq_raw, motor.temperature,
                       motor.lost, *motor.reserve])
    bms = msg.bms_state
    values.extend([bms.version_high, bms.version_low, bms.status, bms.soc, bms.current,
                   bms.cycle, *bms.bq_ntc, *bms.mcu_ntc, *bms.cell_vol,
                   *msg.foot_force, *msg.foot_force_est, msg.tick, *msg.wireless_remote,
                   msg.bit_flag, msg.adc_reel, msg.temperature_ntc1, msg.temperature_ntc2,
                   msg.power_v, msg.power_a, *msg.fan_frequency, msg.reserve, 0])
    return struct.pack(LOW_STATE_FORMAT, *values)


def low_cmd_crc(msg):
    return crc_words(low_cmd_record(msg)[:-4])


def low_state_crc(msg):
    return crc_words(low_state_record(msg)[:-4])
