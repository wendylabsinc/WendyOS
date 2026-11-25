"""Independent wire fixtures; no ROS process, simulator or ground truth input."""

import dataclasses
import hashlib
import math
import random
import struct
import unittest

from sim.go2_cloud import (CloudDecodeError, decode_cloud, SOURCE_DIGEST,
                          DIRECTION_QUANTIZATION_RAD, RANGE_QUANTIZATION_M,
                          MAX_CDR_BYTES, RAY_COUNT)


def ray(index, distance=1.0):
    # Construct independently from the publisher's documented elevation/azimuth
    # formulas, then encode the same IEEE binary32 values as PointCloud2.
    elevation = (-30 + 2.5 * (index // 360)) * math.pi / 180
    angle = -math.pi + (index % 360) * math.pi / 180
    return (distance * math.cos(elevation) * math.cos(angle),
            distance * math.cos(elevation) * math.sin(angle),
            distance * math.sin(elevation))


def wire(points, **options):
    """Independent ROS PointCloud2 XCDR1 encoder, with adversarial overrides."""
    buf = bytearray(options.get("encoding", b"\x00\x01\x00\x00"))
    def put(fmt, *values):
        alignment = 1 if fmt in ("B",) else 4
        buf.extend(b"\x00" * ((-(len(buf)-4)) % alignment))
        buf.extend(struct.pack("<" + fmt, *values))
    def string(value):
        raw = value.encode("ascii") + b"\x00"
        put("I", len(raw)); buf.extend(raw)
    put("i", options.get("sec", 123))
    put("I", options.get("nanosec", 456))
    string(options.get("frame", "utlidar_lidar"))
    put("I", options.get("height", 1))
    width = options.get("width", len(points))
    put("I", width)
    fields = options.get("fields", [("x", 0, 7, 1), ("y", 4, 7, 1), ("z", 8, 7, 1)])
    put("I", options.get("field_count", len(fields)))
    for name, offset, kind, count in fields:
        string(name); put("I", offset); put("B", kind); put("I", count)
    put("B", options.get("bigendian", 0))
    put("I", options.get("point_step", 12))
    put("I", options.get("row_step", width * 12))
    put("I", options.get("data_size", len(points) * 12))
    for xyz in points:
        buf.extend(struct.pack("<fff", *xyz))
    put("B", options.get("dense", 1))
    buf.extend(options.get("trailing", b""))
    return bytes(buf)


def decode(raw, source=SOURCE_DIGEST):
    return decode_cloud(raw, source_digest=source)


class CloudTests(unittest.TestCase):
    def test_all_pattern_rays_at_both_range_endpoints(self):
        for distance in (.1, 12.0):
            with self.subTest(distance=distance):
                raw = wire([ray(i, distance) for i in range(RAY_COUNT)])
                self.assertLess(len(raw), MAX_CDR_BYTES)
                cloud = decode(raw)
                self.assertEqual(tuple(p.ray_index for p in cloud.points), tuple(range(RAY_COUNT)))
                self.assertEqual(cloud.capture_unix_ns, 123000000456)
                self.assertEqual(cloud.serialized_sha256, hashlib.sha256(raw).hexdigest())
                self.assertTrue(all(v is not None for v in cloud.ranges_m))
                for p in cloud.points:
                    self.assertLessEqual(abs(p.range_m-distance), RANGE_QUANTIZATION_M)
                    self.assertEqual(p.xyz_m, struct.unpack("<fff", struct.pack("<fff", *ray(p.ray_index, distance))))

    def test_sparse_dropout_missing_and_empty_are_not_clearance(self):
        indices = [0, 359, 360, 4320, 4499, 4500, 8999]
        cloud = decode(wire([ray(i, 2.7) for i in indices]))
        self.assertEqual([p.ray_index for p in cloud.points], indices)
        self.assertEqual(sum(x is None for x in cloud.ranges_m), RAY_COUNT-len(indices))
        empty = decode(wire([]))
        self.assertEqual(empty.points, ())
        self.assertEqual(empty.ranges_m, (None,) * RAY_COUNT)
        self.assertEqual(cloud.frame_id, "utlidar_lidar")

    def test_result_has_no_mutable_aliases(self):
        capture = decode(wire([ray(4320)]))
        with self.assertRaises(dataclasses.FrozenInstanceError):
            capture.frame_id = "base_link"
        with self.assertRaises(dataclasses.FrozenInstanceError):
            capture.points[0].ray_index = 0
        with self.assertRaises(TypeError):
            capture.ranges_m[0] = 1
        with self.assertRaises(CloudDecodeError):
            decode(bytearray(wire([])))

    def test_rejects_duplicate_reordered_and_nonunique_assignments(self):
        for points in ([ray(7), ray(7)], [ray(8), ray(7)],
                       [ray(7, 1), ray(7, 2)]):
            with self.subTest(points=points), self.assertRaisesRegex(CloudDecodeError, "duplicate_or_order"):
                decode(wire(points))
        # A midpoint between azimuths, or between elevations, cannot be guessed
        # into a closest slot. This includes an azimuth-wrap midpoint.
        for azimuth, elevation in ((.5, 0), (0, 1.25), (179.5, 0)):
            a, e = math.radians(azimuth), math.radians(elevation)
            p = (math.cos(e)*math.cos(a), math.cos(e)*math.sin(a), math.sin(e))
            with self.subTest(a=a, e=e), self.assertRaisesRegex(CloudDecodeError, "off_pattern"):
                decode(wire([p]))

    def test_quantization_bound_and_off_pattern(self):
        rng = random.Random(417)
        indices = sorted(rng.sample(range(RAY_COUNT), 1000))
        points = [ray(i, rng.uniform(.1, 12)) for i in indices]
        cloud = decode(wire(points))
        self.assertEqual([p.ray_index for p in cloud.points], indices)
        for p, exact in zip(cloud.points, points):
            original_range = math.hypot(*exact)
            self.assertLessEqual(abs(original_range-p.range_m), cloud.range_quantization_m)
            dot = sum(a/original_range*b/p.range_m for a, b in zip(exact, p.xyz_m))
            self.assertLessEqual(math.acos(min(1, max(-1, dot))), cloud.direction_quantization_rad)
        # Much smaller than angular ray spacing, but outside the derived binary32 cap.
        angle = DIRECTION_QUANTIZATION_RAD * 4
        with self.assertRaisesRegex(CloudDecodeError, "off_pattern"):
            decode(wire([(math.cos(angle), math.sin(angle), 0)]))

    def test_strict_metadata(self):
        bad = [dict(encoding=b"\x00\x00\x00\x00"), dict(encoding=b"\x00\x07\x00\x00"),
               dict(encoding=b"\x00\x01\x00\x01"), dict(frame="lidar_link"), dict(frame="base_link"),
               dict(height=2), dict(height=0), dict(width=9001), dict(width=2**32-1,row_step=0),
               dict(field_count=2), dict(field_count=2**32-1),
               dict(fields=[("y",0,7,1),("x",4,7,1),("z",8,7,1)]),
               dict(fields=[("x",0,7,1),("y",4,7,1),("z",4,7,1)]),
               dict(fields=[("x",0,8,1),("y",4,7,1),("z",8,7,1)]),
               dict(fields=[("x",0,7,2),("y",4,7,1),("z",8,7,1)]),
               dict(bigendian=1), dict(bigendian=2), dict(point_step=16),
               dict(row_step=24), dict(data_size=0), dict(data_size=2**32-1),
               dict(dense=0), dict(dense=2), dict(trailing=b"\x00"),
               dict(sec=-1), dict(sec=0,nanosec=0), dict(nanosec=1_000_000_000)]
        for change in bad:
            with self.subTest(change=change), self.assertRaises(CloudDecodeError):
                decode(wire([ray(1)], **change))

    def test_truncated_oversized_and_malicious_string_lengths(self):
        raw = wire([ray(0), ray(8999)])
        for n in range(len(raw)):
            with self.subTest(length=n), self.assertRaises(CloudDecodeError):
                decode(raw[:n])
        for replacement in (0, 33, 2**32-1):
            bad = bytearray(raw)
            struct.pack_into("<I", bad, 12, replacement)
            with self.assertRaises(CloudDecodeError):
                decode(bytes(bad))
        with self.assertRaises(CloudDecodeError):
            decode(b"\x00" * (MAX_CDR_BYTES+1))
        for frame in ("utlidar\x00lidar", "x"*32):
            with self.assertRaises(CloudDecodeError):
                decode(wire([], frame=frame))

    def test_invalid_point_values_and_ranges(self):
        for p in ((math.nan,0,0), (math.inf,0,0), (0,0,0),
                  (.09,0,0), (12.001,0,0), (1e30,0,0), (0,0,1)):
            with self.subTest(p=p), self.assertRaises(CloudDecodeError):
                decode(wire([p]))

    def test_source_pin_is_required_and_static_errors_do_not_echo_input(self):
        with self.assertRaises(TypeError):
            decode_cloud(wire([]))
        for pin in (None, "", SOURCE_DIGEST+"x", "secret-not-a-source"):
            with self.assertRaisesRegex(CloudDecodeError, "^cloud_source$"):
                decode(wire([]), pin)
        with self.assertRaisesRegex(CloudDecodeError, "^cloud_frame$"):
            decode(wire([],frame="private-value"))


if __name__ == "__main__":
    unittest.main()
