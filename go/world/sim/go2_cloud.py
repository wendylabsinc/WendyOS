"""Bounded decoder for the pinned synthetic Go2 multi-ring ROS observation.

This accepts only the source-pinned publisher's PointCloud2 CDR1 layout. It
does not authenticate a runtime or publisher: the caller must verify the
runtime source identity, actual DDS GID, epoch and capture freshness separately.
No missing ray is a clear-space observation. Returned distances are reconstructed
from original binary32 XYZ, not renewed measurements or corrected scene truth.
"""

from __future__ import annotations

from dataclasses import dataclass
import hashlib
import math
import struct


SOURCE_DIGEST = "sha256:2869d018ae55e4c83318329bb20cc670e16f6f62838989bb5192829002af8d2b"
FRAME_ID = "utlidar_lidar"
RINGS = 25
AZIMUTHS = 360
RAY_COUNT = RINGS * AZIMUTHS
MAX_CDR_BYTES = 110_000
MIN_RANGE_M = 0.10
MAX_RANGE_M = 12.0
# Binary32 component rounding is <= u*abs(component), apart from negligible
# subnormal absolute error. Vector norm error is thus <= u*12 m; normalization
# adds at most about 2u direction error. These outward bounds also cover double
# precision reconstruction error, while staying far below the 1 degree spacing.
_U32 = 2.0 ** -24
RANGE_QUANTIZATION_M = 2.0 ** -20
DIRECTION_QUANTIZATION_RAD = 4 * _U32


class CloudDecodeError(ValueError):
    """Static, payload-free refusal code suitable for bounded diagnostics."""


@dataclass(frozen=True)
class CloudPoint:
    ray_index: int
    xyz_m: tuple[float, float, float]
    range_m: float


@dataclass(frozen=True)
class CloudCapture:
    frame_id: str
    capture_unix_ns: int
    source_digest: str
    serialized_sha256: str
    ranges_m: tuple[float | None, ...]
    points: tuple[CloudPoint, ...]
    range_quantization_m: float = RANGE_QUANTIZATION_M
    direction_quantization_rad: float = DIRECTION_QUANTIZATION_RAD


def _direction(index):
    ring, azimuth = divmod(index, AZIMUTHS)
    elevation = math.radians(-30.0 + 2.5 * ring)
    angle = -math.pi + azimuth * (2 * math.pi / AZIMUTHS)
    c = math.cos(elevation)
    return c * math.cos(angle), c * math.sin(angle), math.sin(elevation)


_DIRECTIONS = tuple(_direction(i) for i in range(RAY_COUNT))


class _CDR:
    def __init__(self, raw):
        if type(raw) is not bytes or not 4 <= len(raw) <= MAX_CDR_BYTES:
            raise CloudDecodeError("cloud_size_or_type")
        # Standard ROS CDR1 little-endian, no parameter-list/extended encoding.
        if raw[:4] != b"\x00\x01\x00\x00":
            raise CloudDecodeError("cloud_encoding")
        self.raw, self.pos = raw, 4

    def take(self, count, alignment=1):
        # CDR alignment starts after the four-byte encapsulation header.
        self.pos += (-(self.pos - 4)) % alignment
        end = self.pos + count
        if count < 0 or end > len(self.raw):
            raise CloudDecodeError("cloud_truncated")
        start, self.pos = self.pos, end
        return memoryview(self.raw)[start:end]

    def uint(self):
        return struct.unpack("<I", self.take(4, 4))[0]

    def sint(self):
        return struct.unpack("<i", self.take(4, 4))[0]

    def byte(self):
        return self.take(1)[0]

    def string(self):
        count = self.uint()
        if not 1 <= count <= 32:
            raise CloudDecodeError("cloud_string_size")
        data = bytes(self.take(count))
        if data[-1] != 0 or b"\x00" in data[:-1]:
            raise CloudDecodeError("cloud_string")
        try:
            return data[:-1].decode("ascii")
        except UnicodeDecodeError:
            raise CloudDecodeError("cloud_string") from None


def _point(xyz):
    if not all(math.isfinite(v) for v in xyz):
        raise CloudDecodeError("cloud_nonfinite")
    distance = math.hypot(*xyz)
    # Exact endpoint rays may round just outside the source range in binary32.
    # Retain that measured norm with its error, never clamp it into clearance.
    if not MIN_RANGE_M - RANGE_QUANTIZATION_M <= distance <= MAX_RANGE_M + RANGE_QUANTIZATION_M:
        raise CloudDecodeError("cloud_range")
    x, y, z = xyz
    elevation = math.degrees(math.atan2(z, math.hypot(x, y)))
    ring = round((elevation + 30.0) / 2.5)
    azimuth = round((math.atan2(y, x) + math.pi) * AZIMUTHS / (2 * math.pi)) % AZIMUTHS
    if not 0 <= ring < RINGS:
        raise CloudDecodeError("cloud_off_pattern")
    index = ring * AZIMUTHS + azimuth
    direction = _DIRECTIONS[index]
    chord = math.hypot(*(v / distance - d for v, d in zip(xyz, direction)))
    if chord > 2 * math.sin(DIRECTION_QUANTIZATION_RAD / 2):
        raise CloudDecodeError("cloud_off_pattern")
    # The accepted cap is < 0.000014 degrees, whereas distinct source rays are
    # separated by >= ~0.866 degrees. The nearest match is therefore unique;
    # no nearest-neighbour guess is accepted outside that cap.
    return CloudPoint(index, xyz, distance)


def decode_cloud(raw: bytes, *, source_digest: str) -> CloudCapture:
    """Decode a pinned /utlidar/cloud CDR sample, or raise CloudDecodeError.

    Only immutable bytes are accepted. Original point order must match the
    publisher's ascending valid-ray order. Padding bytes have no semantic value
    in CDR; extra bytes after is_dense are refused. This function performs no
    I/O and does not use runtime truth, wall time or mutable global state.
    """
    if source_digest != SOURCE_DIGEST:
        raise CloudDecodeError("cloud_source")
    cdr = _CDR(raw)
    sec, nanosec = cdr.sint(), cdr.uint()
    if sec < 0 or nanosec >= 1_000_000_000 or sec == nanosec == 0:
        raise CloudDecodeError("cloud_timestamp")
    if cdr.string() != FRAME_ID:
        raise CloudDecodeError("cloud_frame")
    height, width = cdr.uint(), cdr.uint()
    if height != 1 or width > RAY_COUNT:
        raise CloudDecodeError("cloud_dimensions")
    if cdr.uint() != 3:
        raise CloudDecodeError("cloud_fields")
    for offset, name in enumerate(("x", "y", "z")):
        if (cdr.string() != name or cdr.uint() != offset * 4
                or cdr.byte() != 7 or cdr.uint() != 1):
            raise CloudDecodeError("cloud_fields")
    if cdr.byte() != 0:
        raise CloudDecodeError("cloud_endianness")
    if cdr.uint() != 12 or cdr.uint() != width * 12:
        raise CloudDecodeError("cloud_stride")
    if cdr.uint() != width * 12:
        raise CloudDecodeError("cloud_data_size")
    data = cdr.take(width * 12)
    if cdr.byte() != 1:
        raise CloudDecodeError("cloud_density")
    if cdr.pos != len(raw):
        raise CloudDecodeError("cloud_trailing")
    slots = [None] * RAY_COUNT
    points = []
    previous = -1
    for xyz in struct.iter_unpack("<fff", data):
        point = _point(xyz)
        if point.ray_index <= previous:
            raise CloudDecodeError("cloud_duplicate_or_order")
        previous = point.ray_index
        slots[previous] = point.range_m
        points.append(point)
    return CloudCapture(FRAME_ID, sec * 1_000_000_000 + nanosec,
                        SOURCE_DIGEST, hashlib.sha256(raw).hexdigest(),
                        tuple(slots), tuple(points))
