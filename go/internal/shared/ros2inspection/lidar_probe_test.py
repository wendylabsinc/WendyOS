"""Host-side tests for the trusted probe without requiring a ROS installation."""

import contextlib
import importlib.util
import io
import json
import math
import pathlib
import struct
import sys
import time
import unittest
from unittest.mock import patch
from types import SimpleNamespace as NS


spec = importlib.util.spec_from_file_location("lidar_probe", pathlib.Path(__file__).with_name("lidar_probe.py"))
probe = importlib.util.module_from_spec(spec)
spec.loader.exec_module(probe)


def options(**kwargs):
    return probe.options_from_json({"topic": "/cloud", **kwargs})


def cloud(points, *, width=None, padding=0, bigendian=False, datatype=7):
    width = len(points) if width is None else width
    height = len(points) // width if width else 1
    fmt = "f" if datatype == 7 else "d"
    size = struct.calcsize(fmt)
    # Deliberately reorder coordinates and put padding before/between fields.
    step = 4 + 3 * size
    row_step = width * step + padding
    data = bytearray(row_step * height)
    for index, (x, y, z) in enumerate(points):
        offset = (index // width) * row_step + (index % width) * step + 4
        struct.pack_into((">" if bigendian else "<") + fmt * 3, data, offset, z, x, y)
    return NS(width=width, height=height, point_step=step, row_step=row_step,
              is_bigendian=bigendian, data=data,
              fields=[NS(name=name, offset=4+offset*size, datatype=datatype, count=1)
                      for name, offset in (("z", 0), ("x", 1), ("y", 2))],
              header=NS(frame_id="lidar_link", stamp=NS(sec=100, nanosec=500000000)))


def summarize(msg, **kwargs):
    return probe.summarize(msg, options(**kwargs), 102000000000, 102000000000)


def transform(translation=(0, 0, 0), quaternion=(0, 0, 0, 1)):
    return NS(translation=NS(**dict(zip(("x", "y", "z"), translation))),
              rotation=NS(**dict(zip(("x", "y", "z", "w"), quaternion))))


class CloudTests(unittest.TestCase):
    def test_organized_padding_reordered_fields_and_byte_order(self):
        points = [(1.25, 0.5, -0.25), (3, 4, 0.5), (-2, 0, 1), (0, -6, 0)]
        for bigendian in (False, True):
            for datatype in (7, 8):
                with self.subTest(bigendian=bigendian, datatype=datatype):
                    msg = cloud(points, width=2, padding=12, bigendian=bigendian, datatype=datatype)
                    total, decoded = probe.cloud_points(msg, 100)
                    self.assertEqual(total, 4)
                    self.assertEqual(list(decoded), points)

    def test_minima_visit_every_point_despite_output_sampling(self):
        msg = cloud([(8, 0, 0)] * 100 + [(0.1, 0, 0)])
        result = summarize(msg, sample_points=1)
        self.assertEqual(result["total_points"], 101)
        self.assertEqual(result["sectors"][0]["point_count"], 101)
        self.assertAlmostEqual(result["sectors"][0]["min_distance_m"], 0.1)
        self.assertEqual(len(result["sample_points"]), 1)

    def test_all_eight_sectors(self):
        points = [(2*math.cos(i*math.pi/4), 2*math.sin(i*math.pi/4), 0) for i in range(8)]
        result = summarize(cloud(points))
        self.assertEqual([sector["axis"] for sector in result["sectors"]], list(probe.AXES))
        self.assertEqual([sector["point_count"] for sector in result["sectors"]], [1]*8)
        for sector in result["sectors"]:
            self.assertAlmostEqual(sector["min_distance_m"], 2)

    def test_filter_counts_and_missing_sectors_remain_unknown(self):
        msg = cloud([(1, 0, 0), (math.nan, 1, 0), (math.inf, 0, 0), (1, 0, 3), (30, 0, 0), (0, 0, 0)])
        result = summarize(msg)
        self.assertEqual(result["finite_points"], 4)
        self.assertEqual(result["included_points"], 1)
        self.assertEqual(result["rejected_points"], {"nonfinite": 2, "height": 1, "range": 2, "sensor_range": 0})
        for sector in result["sectors"][1:]:
            self.assertEqual(sector["point_count"], 0)
            self.assertIsNone(sector["min_distance_m"])
            self.assertIsNone(sector["nearest_xyz_m"])
        self.assertEqual(result["coverage"], "observed_returns_only")
        self.assertNotIn("clear", json.dumps(result))

    def test_empty_and_all_invalid_are_valid_unknown_geometry(self):
        for msg in (cloud([]), cloud([(math.nan, 0, 0)])):
            result = summarize(msg)
            self.assertEqual(result["included_points"], 0)
            self.assertIsNone(result["bounds"])
            self.assertEqual(result["sample_points"], [])

    def test_malformed_clouds_rejected_before_iteration(self):
        mutations = [lambda m: setattr(m, "data", m.data[:-1]),
                     lambda m: setattr(m, "row_step", 1),
                     lambda m: setattr(m, "point_step", 0),
                     lambda m: setattr(m.fields[0], "offset", m.point_step),
                     lambda m: setattr(m.fields[0], "count", 2),
                     lambda m: setattr(m.fields[0], "datatype", 2),
                     lambda m: m.fields.pop(),
                     lambda m: m.fields.append(m.fields[0])]
        for mutate in mutations:
            msg = cloud([(1, 0, 0)])
            mutate(msg)
            with self.subTest(mutate=mutate), self.assertRaises(probe.ProbeError):
                probe.cloud_points(msg, 100)

    def test_oversized_cloud_is_not_downsampled(self):
        with self.assertRaisesRegex(probe.ProbeError, "point limit"):
            probe.cloud_points(cloud([(1, 0, 0)] * 101), 100)

    def test_decode_checks_deadline(self):
        with self.assertRaisesRegex(probe.ProbeError, "Deadline"):
            probe.summarize(cloud([(1, 0, 0)]), options(), 0, 0, deadline=time.monotonic()-1)

    def test_freshness_never_inferred_from_wall_delta(self):
        result = summarize(cloud([(1, 0, 0)]))
        self.assertEqual(result["source_age_seconds"], 1.5)
        self.assertEqual(result["source_freshness"], "unknown")
        self.assertEqual(result["clock_synchronization"], "unverified")
        self.assertEqual(result["clock_basis"], "device_wall")
        msg = cloud([(1, 0, 0)])
        msg.header.stamp.sec = 104
        self.assertEqual(summarize(msg)["source_age_seconds"], -2.5)
        msg.header.stamp = NS(sec=0, nanosec=0)
        self.assertIsNone(summarize(msg)["source_age_seconds"])
        self.assertIsNone(probe.summarize(msg, options(use_sim_time=True), 1000, 0)["source_age_seconds"])

    def test_transform_applies_before_sector_and_height_filter(self):
        msg = cloud([(1, 0, 0), (2, 0, 1.5)])
        tf = transform(translation=(0, 0, 1), quaternion=(0, 0, math.sqrt(0.5), math.sqrt(0.5)))
        result = probe.summarize(msg, options(target_frame="base_link"), 102000000000, 102000000000, tf)
        self.assertEqual(result["frame_id"], "base_link")
        self.assertEqual(result["source_frame"], "lidar_link")
        self.assertTrue(result["transform_applied"])
        self.assertEqual(result["included_points"], 1)
        self.assertEqual(result["sectors"][2]["point_count"], 1)
        self.assertEqual(result["rejected_points"]["height"], 1)

    def test_requested_transform_cannot_silently_fall_back(self):
        with self.assertRaisesRegex(probe.ProbeError, "requires a transform"):
            summarize(cloud([(1, 0, 0)]), target_frame="base_link")
        with self.assertRaisesRegex(probe.ProbeError, "normalized"):
            probe.transform_function(transform(quaternion=(0, 0, 0, 0)))

    def test_output_is_bounded_finite_json(self):
        result = summarize(cloud([(1, 0, 0)] * 1000), sample_points=128)
        output = io.StringIO()
        with contextlib.redirect_stdout(output):
            probe.emit(result)
        self.assertLess(len(output.getvalue().encode()), probe.MAX_RESULT_BYTES)
        self.assertEqual(json.loads(output.getvalue())["total_points"], 1000)


class ScanTests(unittest.TestCase):
    def scan(self, ranges):
        return NS(header=NS(frame_id="laser", stamp=NS(sec=100, nanosec=0)),
                  ranges=ranges, angle_min=-math.pi, angle_max=-math.pi+(len(ranges)-1)*math.pi/2,
                  angle_increment=math.pi/2, range_min=0.1, range_max=10.0, time_increment=0.001)

    def test_ranges_project_into_frame_with_explicit_timing(self):
        result = summarize(self.scan([1, 2, 3, 4]), message_type=probe.SCAN)
        self.assertEqual(result["included_points"], 4)
        self.assertAlmostEqual(result["sectors"][4]["min_distance_m"], 1)
        self.assertAlmostEqual(result["sectors"][6]["min_distance_m"], 2)
        self.assertAlmostEqual(result["sectors"][0]["min_distance_m"], 3)
        self.assertAlmostEqual(result["sectors"][2]["min_distance_m"], 4)
        self.assertFalse(result["motion_compensated"])
        self.assertEqual(result["scan_timing"]["stamp_reference"], "first_ray")

    def test_no_returns_and_out_of_sensor_range_do_not_become_clear(self):
        result = summarize(self.scan([math.inf, math.nan, 0.01, 20]), message_type=probe.SCAN)
        self.assertEqual(result["included_points"], 0)
        self.assertEqual(result["rejected_points"]["nonfinite"], 2)
        self.assertEqual(result["rejected_points"]["sensor_range"], 2)
        self.assertTrue(all(item["min_distance_m"] is None for item in result["sectors"]))

    def test_invalid_scan_geometry(self):
        msg = self.scan([1, 2])
        msg.angle_max += 1
        with self.assertRaisesRegex(probe.ProbeError, "angle_max"):
            summarize(msg, message_type=probe.SCAN)


class OptionsTests(unittest.TestCase):
    def test_untrusted_options(self):
        for values in ({"count": True}, {"sample_points": 129}, {"count": 1.5},
                       {"min_z": math.nan}, {"min_z": 3, "max_z": 2},
                       {"max_range": 0}, {"use_sim_time": "false"},
                       {"target_frame": "base; exec bad"}, {"message_type": "custom/msg/Foo"},
                       {"topic": "/bad topic"}):
            with self.subTest(values=values), self.assertRaises(probe.ProbeError):
                options(**values)


class SubscriberTests(unittest.TestCase):
    def fake_ros(self, message=None, *, tf_available=False):
        state = NS(seconds=0.0, pending=message, destroyed=False, shutdown=False,
                   subscription=None, parameters=None, transform_times=[], buffer_node=None)

        class Node:
            def create_subscription(self, cls, topic, callback, qos):
                state.subscription = NS(cls=cls, topic=topic, callback=callback, qos=qos)
                return state.subscription

            def get_clock(self):
                return NS(now=lambda: NS(nanoseconds=0))

            def destroy_node(self):
                state.destroyed = True

        def create_node(name, **kwargs):
            state.parameters = kwargs
            return Node()

        def spin_once(node, timeout_sec):
            state.seconds += max(timeout_sec, 0.001)
            if state.pending is not None:
                msg, state.pending = state.pending, None
                state.subscription.callback(msg)

        class Buffer:
            def __init__(self, **kwargs):
                state.buffer_node = kwargs.get("node")

            def can_transform(self, target, source, stamp):
                state.transform_times.append(stamp)
                return tf_available

            def lookup_transform(self, target, source, stamp):
                state.transform_times.append(stamp)
                return NS(transform=transform())

        modules = {
            "rclpy": NS(init=lambda **kwargs: None, create_node=create_node, spin_once=spin_once,
                        shutdown=lambda: setattr(state, "shutdown", True)),
            "rclpy.parameter": NS(Parameter=lambda name, value: NS(name=name, value=value)),
            "rclpy.qos": NS(QoSProfile=lambda **kwargs: NS(**kwargs),
                            ReliabilityPolicy=NS(BEST_EFFORT="best_effort"),
                            DurabilityPolicy=NS(VOLATILE="volatile"),
                            HistoryPolicy=NS(KEEP_LAST="keep_last")),
            "rclpy.time": NS(Time=NS(from_msg=lambda stamp: (stamp.sec, stamp.nanosec))),
            "sensor_msgs": NS(), "sensor_msgs.msg": NS(PointCloud2="cloud_type", LaserScan="scan_type"),
            "tf2_ros": NS(Buffer=Buffer, TransformListener=lambda *args, **kwargs: NS()),
        }
        stack = contextlib.ExitStack()
        self.addCleanup(stack.close)
        stack.enter_context(patch.dict(sys.modules, modules))
        stack.enter_context(patch.object(probe.time, "monotonic", lambda: state.seconds))
        stack.enter_context(patch.object(probe.time, "time_ns", lambda: 102000000000))
        return state

    def test_fixed_sensor_qos_and_cleanup_after_count(self):
        state = self.fake_ros(cloud([(1, 0, 0)]))
        output = io.StringIO()
        with contextlib.redirect_stdout(output):
            probe.run(options())
        result = json.loads(output.getvalue())
        self.assertEqual(result["included_points"], 1)
        self.assertEqual(vars(state.subscription.qos), {"depth": 1, "reliability": "best_effort", "durability": "volatile", "history": "keep_last"})
        self.assertFalse(state.parameters["enable_rosout"])
        self.assertFalse(state.parameters["start_parameter_services"])
        self.assertTrue(state.destroyed)
        self.assertTrue(state.shutdown)

    def test_no_messages_terminates_with_unknown(self):
        state = self.fake_ros()
        output = io.StringIO()
        with contextlib.redirect_stdout(output):
            probe.run(options(duration_seconds=1))
        result = json.loads(output.getvalue())
        self.assertEqual(result["status"], "unknown")
        self.assertEqual(result["error"]["code"], "no_messages")
        self.assertLess(state.seconds, 1.2)
        self.assertTrue(state.destroyed)

    def test_tf_uses_acquisition_stamp_and_does_not_expose_service(self):
        state = self.fake_ros(cloud([(1, 0, 0)]), tf_available=True)
        output = io.StringIO()
        with contextlib.redirect_stdout(output):
            probe.run(options(target_frame="base_link"))
        result = json.loads(output.getvalue())
        self.assertTrue(result["transform_applied"])
        self.assertEqual(state.transform_times, [(100, 500000000), (100, 500000000)])
        self.assertIsNone(state.buffer_node)

    def test_missing_tf_and_zero_stamp_do_not_use_latest(self):
        for zero_stamp in (False, True):
            msg = cloud([(1, 0, 0)])
            if zero_stamp:
                msg.header.stamp = NS(sec=0, nanosec=0)
            with self.subTest(zero_stamp=zero_stamp):
                state = self.fake_ros(msg)
                with self.assertRaises(probe.ProbeError) as error:
                    probe.run(options(target_frame="base_link", duration_seconds=1))
                self.assertEqual(error.exception.code, "transform_unavailable")
                self.assertTrue(state.destroyed)
                self.assertTrue(state.shutdown)
                self.assertNotIn((0, 0), state.transform_times)


if __name__ == "__main__":
    unittest.main()
