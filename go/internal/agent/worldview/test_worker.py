import json
import math
import threading
import unittest

import cv2
import numpy as np

from worker import (DepthFrame, FrameSlot, RateMeter, StreamBytes, pair_period_seconds,
                    parse_z16, propose, score, validate)

GREY = (128, 128, 128)
RED = (0, 0, 255)  # OpenCV images are BGR.


def frame(width=320, height=240):
    return np.full((height, width, 3), GREY, np.uint8)


def red_rect(x=100, y=90, width=120, height=60):
    image = frame()
    image[y:y + height, x:x + width] = RED
    return image


def config(**overrides):
    base = {"proposer": "contours", "rate": 4.0, "every_frames": None, "max_proposals": 50,
            "min_area_fraction": 0.002,
            "depth": {"scale_m": 0.001, "intrinsics": {"fx": 320.0, "fy": 320.0, "cx": 160.0, "cy": 120.0}},
            "pairs": {"rgb": "depth"}}
    base.update(overrides)
    return validate(base)


def plane(units=1000, width=320, height=240):
    return np.full((height, width), units, np.uint16)


class ProposerTests(unittest.TestCase):
    def test_red_rectangle(self):
        proposals = propose(red_rect())
        self.assertEqual(len(proposals), 1)
        proposal = proposals[0]
        for got, want in zip(proposal.box, (100, 90, 120, 60)):
            self.assertLessEqual(abs(got - want), 3, proposal.box)
        dominant = proposal.palette[0]
        for got, want in zip(dominant["lab"], (53, 80, 67)):
            self.assertLessEqual(abs(got - want), 12, dominant)
        self.assertGreater(dominant["share"], 0.9)
        self.assertAlmostEqual(sum(entry["share"] for entry in proposal.palette), 1.0, places=3)
        self.assertEqual(proposal.silhouette["primitive"], "rect")
        self.assertTrue(0.45 <= proposal.silhouette["aspect"] <= 0.55, proposal.silhouette)

    def test_filled_circle_is_disc(self):
        image = frame()
        cv2.circle(image, (160, 120), 50, (255, 0, 0), thickness=-1)
        proposals = propose(image)
        self.assertEqual(len(proposals), 1)
        self.assertEqual(proposals[0].silhouette["primitive"], "disc")

    def test_trapezoid(self):
        image = frame()
        cv2.fillPoly(image, [np.array([[130, 60], [190, 60], [220, 180], [100, 180]])], (0, 200, 200))
        self.assertEqual(propose(image)[0].silhouette["primitive"], "trapezoid")

    def test_boxes_are_in_decoded_frame_pixels(self):
        image = np.full((1080, 1920, 3), GREY, np.uint8)
        image[300:600, 500:1100] = RED
        proposals = propose(image)
        self.assertEqual(len(proposals), 1)
        for got, want in zip(proposals[0].box, (500, 300, 600, 300)):
            self.assertLessEqual(abs(got - want), 6, proposals[0].box)

    def test_min_area_and_max_proposals(self):
        image = frame()
        image[10:20, 10:20] = RED  # 100 pixels, below 0.002 of 76800
        image[100:160, 100:160] = RED
        image[100:160, 200:260] = (255, 0, 0)
        self.assertEqual(len(propose(image)), 2)
        self.assertEqual(len(propose(image, max_proposals=1)), 1)
        self.assertEqual(propose(frame()), [])

    def test_output_is_json(self):
        _, proposals = score(red_rect(), config(), DepthFrame(1, 0, plane()), 0)
        json.dumps(proposals, allow_nan=False)


class DepthTests(unittest.TestCase):
    def test_metric_from_depth_plane(self):
        paired, proposals = score(red_rect(), config(), DepthFrame(1, 1_000_000, plane()), 1_000_000)
        self.assertTrue(paired)
        box, metric = proposals[0]["box"], proposals[0]["metric"]
        self.assertAlmostEqual(metric["distance_m"], 1.0, places=6)
        self.assertLess(abs(metric["width_m"] - box[2] / 320) / (box[2] / 320), 0.02)
        self.assertLess(abs(metric["height_m"] - box[3] / 320) / (box[3] / 320), 0.02)
        self.assertLess(abs(metric["bearing_deg"]), 0.5)

    def test_bearing_right_of_centre(self):
        _, proposals = score(red_rect(x=180), config(), DepthFrame(1, 0, plane()), 0)
        self.assertAlmostEqual(proposals[0]["metric"]["bearing_deg"], math.degrees(math.atan(80 / 320)), delta=0.5)

    def test_depth_is_scaled_to_frame(self):
        _, proposals = score(red_rect(), config(), DepthFrame(1, 0, plane(width=160, height=120)), 0)
        self.assertAlmostEqual(proposals[0]["metric"]["distance_m"], 1.0, places=6)

    def test_stale_depth_is_unpaired(self):
        period_ns = int(pair_period_seconds(config()) * 1e9)
        paired, proposals = score(red_rect(), config(), DepthFrame(1, 0, plane()), period_ns + 1)
        self.assertFalse(paired)
        self.assertIsNone(proposals[0]["metric"])
        paired, _ = score(red_rect(), config(), DepthFrame(1, 0, plane()), period_ns)
        self.assertTrue(paired)

    def test_no_valid_depth_under_mask(self):
        paired, proposals = score(red_rect(), config(), DepthFrame(1, 0, plane(units=0)), 0)
        self.assertTrue(paired)
        self.assertIsNone(proposals[0]["metric"])

    def test_pair_period(self):
        self.assertEqual(pair_period_seconds(config(rate=4.0)), 0.25)
        self.assertEqual(pair_period_seconds(config(rate=30.0)), 0.1)
        self.assertEqual(pair_period_seconds(config(rate=None, every_frames=2)), 0.1)

    def test_z16(self):
        pixels = parse_z16(np.array([[1, 2, 3], [4, 5, 65535]], "<u2").tobytes(), 3, 2)
        self.assertEqual(pixels.tolist(), [[1, 2, 3], [4, 5, 65535]])
        with self.assertRaises(ValueError):
            parse_z16(b"\x00" * 10, 3, 2)
        with self.assertRaises(ValueError):
            parse_z16(b"", 0, 0)


class SamplingTests(unittest.TestCase):
    def test_every_frames(self):
        slot = FrameSlot(every_frames=3)
        scored = []
        for index in range(1, 10):
            slot.put(100.0, index)
            taken = slot.take(100.0)
            if taken is not None:
                scored.append(taken[0])
        self.assertEqual(scored, [3, 6, 9])

    def test_rate_takes_latest_once_per_interval(self):
        slot = FrameSlot(rate=4.0)
        slot.put(0.0, "a", (1, 10))
        slot.put(0.01, "b", (2, 20))
        self.assertEqual(slot.take(0.02), ("b", (2, 20)))
        slot.put(0.1, "c")
        self.assertIsNone(slot.take(0.2))
        self.assertEqual(slot.take(0.27)[0], "c")
        slot.put(0.3, "d")
        self.assertIsNone(slot.take(10.0), "frames older than 5 seconds are stale")

    def test_rate_meter_window(self):
        meter = RateMeter()
        self.assertEqual(meter.tick(0.0), 0.0)
        for i in range(1, 41):
            fps = meter.tick(i * 0.25)
        self.assertAlmostEqual(fps, 4.0)
        self.assertEqual(len(meter.times), 21)
        self.assertEqual(meter.tick(100.0), 0.0)


class ConfigTests(unittest.TestCase):
    def test_rejects_both_or_neither_rate(self):
        with self.assertRaises(ValueError):
            config(rate=4.0, every_frames=4)
        with self.assertRaises(ValueError):
            config(rate=None, every_frames=None)
        with self.assertRaises(ValueError):
            config(proposer="yolo")
        with self.assertRaises(ValueError):
            config(rate=31.0)
        with self.assertRaises(ValueError):
            config(rate=None, every_frames=301)
        config(depth=None)


class StreamTests(unittest.TestCase):
    def test_byte_order_and_sample_metadata(self):
        stream = StreamBytes()
        stream.feed(b"abc", (1, 100))
        stream.feed(b"def", (2, 200))
        self.assertEqual(stream.read(2), b"ab")
        self.assertEqual(stream.read_meta, (1, 100))
        self.assertEqual(stream.read(100), b"c")
        self.assertEqual(stream.read(3), b"def")
        self.assertEqual(stream.read_meta, (2, 200))
        self.assertEqual(stream.pending, 0)

    def test_bounded_encoded_queue(self):
        stream = StreamBytes()
        self.assertTrue(stream.feed(b"x" * (8 << 20)))
        self.assertFalse(stream.feed(b"y"))
        stream.stop()
        self.assertEqual(stream.pending, 0)
        self.assertFalse(stream.feed(b"z"))

    def test_stop_unblocks_libav_read(self):
        stream = StreamBytes()
        result = []
        reader = threading.Thread(target=lambda: result.append(stream.read(4096)))
        reader.start()
        stream.stop()
        reader.join(timeout=1)
        self.assertFalse(reader.is_alive())
        self.assertEqual(result, [b""])


class DecoderTests(unittest.TestCase):
    def test_h264_frames_carry_sample_metadata(self):
        import importlib.util
        import shutil
        import subprocess
        import time
        from worker import Decoder

        if importlib.util.find_spec("av") is None or shutil.which("ffmpeg") is None:
            self.skipTest("requires PyAV and ffmpeg for real H.264 decoding")
        # libav probes several seconds of a non-seekable stream before it
        # yields the first frame, so the fixture must be longer than that.
        data = subprocess.run([
            "ffmpeg", "-v", "error", "-f", "lavfi", "-i", "testsrc2=size=96x64:rate=10", "-t", "12",
            "-c:v", "libx264", "-g", "10", "-bsf:v", "h264_mp4toannexb", "-f", "h264", "-",
        ], check=True, capture_output=True).stdout
        decoder = Decoder("camera", 1, "h264", slot=FrameSlot(every_frames=1))
        try:
            for index, pos in enumerate(range(0, len(data), 2048)):
                self.assertTrue(decoder.stream.feed(data[pos:pos + 2048], (index, index * 1000)))
            deadline = time.monotonic() + 5
            taken = None
            while taken is None and time.monotonic() < deadline:
                taken = decoder.take(time.monotonic())
                time.sleep(0.01)
            self.assertIsNotNone(taken, "H.264 stream never decoded")
            image, meta = taken
            self.assertEqual((image.width, image.height), (96, 64))
            self.assertEqual(meta[1], meta[0] * 1000)
        finally:
            decoder.stop()


if __name__ == "__main__":
    unittest.main()
