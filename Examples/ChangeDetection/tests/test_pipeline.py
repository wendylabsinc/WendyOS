import unittest

import cv2
import numpy as np

from change_detection.pipeline import Pipeline
from change_detection.sources import DemoSource


class PipelineTests(unittest.TestCase):
    def run_scene(self, scenario, count=28):
        source, pipeline = DemoSource(scenario), Pipeline()
        return [pipeline.process(*source.read()) for _ in range(count)]

    def test_growing_liquid_requires_time_and_repeated_evidence(self):
        frames = self.run_scene("liquid")
        alerts = [f for f in frames if any(r["alert"] for r in f["regions"])]
        self.assertTrue(alerts)
        self.assertGreaterEqual(alerts[0]["regions"][0]["features"]["age_seconds"], 8)
        self.assertEqual(len(frames[-1]["events"]), 1)
        self.assertEqual({r["id"] for f in frames for r in f["regions"]}, {1})
        np.testing.assert_array_equal(frames[0]["before"], frames[-1]["before"])

    def test_negative_scenes_never_alert(self):
        for scenario in ("empty", "shadow", "lighting", "object", "camera_shift"):
            with self.subTest(scenario=scenario):
                frames = self.run_scene(scenario)
                self.assertFalse(any(r["alert"] for f in frames for r in f["regions"]))
                self.assertEqual(frames[-1]["events"], [])

    def test_moving_shadow_and_new_object_have_distinct_labels(self):
        shadow = self.run_scene("shadow")
        self.assertIn("shadow", {r["label"] for f in shadow for r in f["regions"]})
        self.assertEqual(self.run_scene("object")[-1]["regions"][0]["label"], "new_object")

    def test_global_lighting_does_not_become_a_foreground_region(self):
        frames = self.run_scene("lighting")
        self.assertTrue(any(f["status"] == "global lighting change" for f in frames))
        self.assertTrue(all(not f["regions"] for f in frames))

    def test_camera_motion_suppresses_regions(self):
        result = self.run_scene("camera_shift")[-1]
        self.assertGreater(result["camera_shift_pixels"], 3)
        self.assertIn("reset reference", result["status"])
        self.assertEqual(result["regions"], [])

    def test_crops_share_coordinates_and_mask_is_binary(self):
        result = self.run_scene("liquid", 12)[-1]
        region = result["regions"][0]
        x, y, w, h = region["crop_box"]
        np.testing.assert_array_equal(region["before"], result["before"][y:y+h, x:x+w])
        np.testing.assert_array_equal(region["after"], result["current"][y:y+h, x:x+w])
        self.assertEqual(region["mask"].shape, region["before"].shape[:2])
        self.assertEqual(set(np.unique(region["mask"])), {0, 255})

    def test_static_dark_patch_abstains(self):
        source, pipeline = DemoSource("liquid"), Pipeline()
        pipeline.process(*source.read())
        for _ in range(6):
            frame, _ = source.read()
        for timestamp in range(1, 40):
            result = pipeline.process(frame, float(timestamp))
        self.assertFalse(result["regions"][0]["alert"])
        self.assertEqual(result["regions"][0]["label"], "unknown")

    def test_detected_liquid_remains_a_candidate_when_growth_stops(self):
        source, pipeline = DemoSource("liquid"), Pipeline()
        for _ in range(14):
            result = pipeline.process(*source.read())
        frame = result["current"]
        for timestamp in range(28, 70, 2):
            result = pipeline.process(frame, timestamp)
        self.assertTrue(result["regions"][0]["alert"])
        self.assertEqual(len(result["events"]), 1)
        result = pipeline.process(source.base, 72)
        self.assertFalse(result["regions"])

    def test_track_expiry_clears_age_and_confirmation(self):
        source, pipeline = DemoSource("liquid"), Pipeline()
        for _ in range(14):
            result = pipeline.process(*source.read())
        old_id = result["regions"][0]["id"]
        changed = result["current"]
        pipeline.process(source.base, 28)
        result = pipeline.process(changed, 36)
        self.assertNotEqual(result["regions"][0]["id"], old_id)
        self.assertEqual(result["regions"][0]["features"]["age_seconds"], 0)
        self.assertFalse(result["regions"][0]["alert"])

    def test_multiple_regions_are_tracked_independently(self):
        source, pipeline = DemoSource("empty"), Pipeline()
        pipeline.process(source.base, 0)
        frame = source.base.copy()
        cv2.rectangle(frame, (230, 210), (265, 240), (0, 0, 0), -1)
        cv2.rectangle(frame, (500, 210), (535, 240), (0, 0, 0), -1)
        result = pipeline.process(frame, 1)
        self.assertEqual(len(result["regions"]), 2)
        self.assertEqual(len({r["id"] for r in result["regions"]}), 2)

    def test_invalid_timestamps_and_frame_changes_do_not_replace_reference(self):
        source, pipeline = DemoSource(), Pipeline()
        frame, _ = source.read()
        pipeline.process(frame, 1)
        for timestamp in (1, 0, float("nan"), float("inf")):
            with self.assertRaises(ValueError):
                pipeline.process(frame, timestamp)
        with self.assertRaises(ValueError):
            pipeline.process(frame[:100], 2)
        np.testing.assert_array_equal(pipeline.reference, frame)
        pipeline.reset()
        self.assertEqual(pipeline.process(frame[:100], 0)["status"], "reference captured")


if __name__ == "__main__":
    unittest.main()
