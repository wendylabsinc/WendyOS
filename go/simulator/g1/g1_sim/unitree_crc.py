"""G1's aligned native-record CRC, independent of ROS CDR serialization.

Layouts follow the pinned Unitree SDK contract recorded in UPSTREAM.md.
This is the non-reflected 0x04C11DB7 word algorithm. The accelerated path
reflects input/output bits around the standard library's compiled CRC; applying
crc32 directly to the native record would produce a different checksum.
"""

import array
import binascii
import struct


LOW_CMD_FORMAT = "<2B2x" + "B3x5fI" * 35 + "5I"
LOW_STATE_FORMAT = "<2I2B2xI" + "13fh2x" + "B3x4f2hf7I" * 35 + "40B5I"


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


def low_cmd_record(msg):
    values = [msg.mode_pr, msg.mode_machine]
    for motor in msg.motor_cmd:
        values.extend([motor.mode, motor.q, motor.dq, motor.tau, motor.kp, motor.kd, motor.reserve])
    values.extend([*msg.reserve, 0])
    return struct.pack(LOW_CMD_FORMAT, *values)


def low_state_record(msg):
    values = [*msg.version, msg.mode_pr, msg.mode_machine, msg.tick]
    imu = msg.imu_state
    values.extend([*imu.quaternion, *imu.gyroscope, *imu.accelerometer, *imu.rpy, imu.temperature])
    for motor in msg.motor_state:
        values.extend([motor.mode, motor.q, motor.dq, motor.ddq, motor.tau_est,
                       *motor.temperature, motor.vol, *motor.sensor, motor.motorstate, *motor.reserve])
    values.extend([*msg.wireless_remote, *msg.reserve, 0])
    return struct.pack(LOW_STATE_FORMAT, *values)


def low_cmd_crc(msg):
    return crc_words(low_cmd_record(msg)[:-4])


def low_state_crc(msg):
    return crc_words(low_state_record(msg)[:-4])
