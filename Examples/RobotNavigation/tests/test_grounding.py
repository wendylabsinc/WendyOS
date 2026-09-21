from dataclasses import replace
import math
import unittest

from robot_navigation.grounding import (
    DepthImage, Detection, GroundingConfig, GroundingError, Intrinsics,
    RigidTransform, RobotPose, TargetRegistry, ground_detection,
)


class GroundingTests(unittest.TestCase):
    def setUp(self):
        self.detection = Detection("track-a", "person", 0.95, (60, 20, 140, 180))
        self.depth = DepthImage(200, 200, [3.0] * 40000, "camera_optical", 10.0)
        self.intrinsics = Intrinsics(200, 200, 400, 400, 100, 100, "camera_optical")
        self.transform = RigidTransform("camera_optical", "map", 10.0, (0, 0, 1.3),
                                        (0, 0, 1, -1, 0, 0, 0, -1, 0))

    def ground(self, **kwargs):
        arguments = dict(detection=self.detection, depth=self.depth,
                         intrinsics=self.intrinsics, transform=self.transform,
                         stamp=10.0, frame_id="camera_optical", now=10.0)
        arguments.update(kwargs)
        return ground_detection(**arguments)

    def assertRejected(self, code, **kwargs):
        with self.assertRaises(GroundingError) as caught:
            self.ground(**kwargs)
        self.assertEqual(code, caught.exception.code)

    def test_calibrated_depth_has_capture_provenance_and_expiry(self):
        observation = self.ground()
        self.assertAlmostEqual(3.0, observation.position[0])
        self.assertAlmostEqual(0.00375, observation.position[1])
        self.assertAlmostEqual(1.33375, observation.position[2])
        self.assertEqual(10.75, observation.expires_at)
        self.assertEqual("camera_optical", observation.source_frame)
        self.assertEqual(10.0, observation.transform_stamp)
        self.assertGreater(observation.uncertainty, 0.15)
        self.assertLessEqual(observation.depth_samples, 400)
        self.assertEqual("unverified", observation.as_dict()["mirror_exclusion"])

    def test_missing_depth_never_falls_back_to_monocular_bearing(self):
        self.assertRejected("missing_calibration", depth=None)
        self.assertRejected("missing_calibration", intrinsics=None)
        self.assertRejected("missing_calibration", transform=None)

    def test_frames_dimensions_and_rectification_must_match(self):
        cases = [
            ("frame_mismatch", {"frame_id": "wrong"}),
            ("frame_mismatch", {"depth": replace(self.depth, frame_id="depth_optical")}),
            ("frame_mismatch", {"transform": replace(self.transform, target_frame="odom")}),
            ("dimension_mismatch", {"depth": replace(self.depth, values=[1.0])}),
            ("dimension_mismatch", {"intrinsics": replace(self.intrinsics, width=201)}),
            ("uncalibrated", {"depth": replace(self.depth, aligned=False)}),
            ("uncalibrated", {"intrinsics": replace(self.intrinsics, rectified=False)}),
            ("invalid_intrinsics", {"intrinsics": replace(self.intrinsics, fx=0)}),
            ("invalid_intrinsics", {"intrinsics": replace(self.intrinsics, cy=math.nan)}),
        ]
        for code, kwargs in cases:
            with self.subTest(code=code, kwargs=kwargs):
                self.assertRejected(code, **kwargs)

    def test_stale_future_and_unsynchronized_data_are_rejected(self):
        self.assertRejected("stale", now=10.76)
        self.assertRejected("future_observation", now=9.9)
        self.assertRejected("invalid_time", stamp=math.nan)
        self.assertRejected("time_mismatch", depth=replace(self.depth, stamp=9.9))
        self.assertRejected("time_mismatch", transform=replace(self.transform, stamp=9.9))

    def test_invalid_or_missing_identity_and_bounds_are_rejected(self):
        for detection, code in [
            (replace(self.detection, id=""), "missing_track_id"),
            (replace(self.detection, label="chair"), "low_confidence"),
            (replace(self.detection, score=0.2), "low_confidence"),
            (replace(self.detection, score=math.nan), "low_confidence"),
            (replace(self.detection, bbox=(-1, 0, 100, 100)), "invalid_bbox"),
            (replace(self.detection, bbox=(1, 1, 1, 100)), "invalid_bbox"),
            (replace(self.detection, bbox=(1, 1, math.nan, 100)), "invalid_bbox"),
            (replace(self.detection, bbox=(1, 1, 4, 4)), "insufficient_depth"),
        ]:
            with self.subTest(detection=detection):
                self.assertRejected(code, detection=detection)

    def test_invalid_and_sparse_depth_is_unknown(self):
        for value in (0, -1, math.inf, math.nan, 100):
            with self.subTest(value=value):
                self.assertRejected("insufficient_depth", depth=replace(self.depth, values=[value]*40000))
        values = [3.0 if u % 4 else 0 for v in range(200) for u in range(200)]
        self.assertRejected("insufficient_depth", depth=replace(self.depth, values=values))

    def test_two_depth_clusters_and_broad_depth_distribution_are_rejected(self):
        values = [3.0 if u < 100 else 4.0 for v in range(200) for u in range(200)]
        self.assertRejected("ambiguous_depth", depth=replace(self.depth, values=values))
        values = [2.5 + (u-88)*0.05 for v in range(200) for u in range(200)]
        self.assertRejected("ambiguous_depth", depth=replace(self.depth, values=values))

    def test_small_fraction_of_outliers_does_not_move_median(self):
        values = [6.0 if (u+v) % 23 == 0 else 3.0 for v in range(200) for u in range(200)]
        observation = self.ground(depth=replace(self.depth, values=values))
        self.assertAlmostEqual(3.0, observation.position[0])

    def test_rigid_transform_height_and_uncertainty_validation(self):
        self.assertRejected("invalid_transform", transform=replace(self.transform, rotation=(1,)*9))
        self.assertRejected("invalid_transform", transform=replace(self.transform, rotation=(-1,0,0,0,1,0,0,0,1)))
        self.assertRejected("invalid_person_height", transform=replace(self.transform, translation=(0,0,0)))
        self.assertRejected("uncertain_position", config=GroundingConfig(max_uncertainty=0.2))
        identity = RigidTransform.from_quaternion("camera_optical", "map", 10, (0,0,0), (0,0,0,1))
        self.assertEqual((1,2,3), identity.apply((1,2,3)))
        with self.assertRaises(GroundingError):
            RigidTransform.from_quaternion("camera_optical", "map", 10, (0,0,0), (0,0,0,0))

    def update(self, registry, stamp=10.0, detections=None, **kwargs):
        arguments = dict(detections=[self.detection] if detections is None else detections,
                         depth=replace(self.depth, stamp=stamp), intrinsics=self.intrinsics,
                         transform=replace(self.transform, stamp=stamp), stamp=stamp,
                         frame_id="camera_optical", now=stamp)
        arguments.update(kwargs)
        return registry.update(**arguments)

    def test_registry_keeps_ids_stable_while_valid_and_never_revives_lost_ids(self):
        registry = TargetRegistry()
        target_id = self.update(registry)[0]["target_id"]
        self.assertEqual(target_id, self.update(registry, stamp=10.1)[0]["target_id"])
        self.assertEqual([], self.update(registry, stamp=10.2, detections=[]))
        self.assertEqual("lost", registry.status(target_id, 10.2)["status"])
        new_id = self.update(registry, stamp=10.3)[0]["target_id"]
        self.assertNotEqual(target_id, new_id)
        with self.assertRaises(GroundingError):
            registry.choose_goal(target_id, RobotPose(0,0,0,"map",10.3), 10.3)

    def test_registry_rejects_duplicate_ids_and_track_jumps(self):
        registry = TargetRegistry()
        target_id = self.update(registry)[0]["target_id"]
        self.assertEqual([], self.update(registry, stamp=10.1, detections=[self.detection, self.detection]))
        self.assertEqual("invalid", registry.status(target_id, 10.1)["status"])
        target_id = self.update(registry, stamp=10.2)[0]["target_id"]
        self.assertEqual([], self.update(registry, stamp=10.3,
                                        transform=replace(self.transform, stamp=10.3, translation=(1,0,1.3))))
        self.assertEqual("jumped", registry.status(target_id, 10.3)["status"])

    def test_staleness_and_duplicate_delivery_do_not_extend_expiry(self):
        registry = TargetRegistry()
        target_id = self.update(registry)[0]["target_id"]
        duplicate = self.update(registry, now=10.5)
        self.assertEqual(10.75, duplicate[0]["expires_at"])
        self.assertEqual([], registry.observed_targets(10.75))
        self.assertEqual("stale", registry.status(target_id, 10.75)["status"])
        self.assertEqual([], self.update(registry, now=10.8))
        self.assertNotEqual(target_id, self.update(registry, stamp=10.9)[0]["target_id"])

    def test_bad_future_frame_does_not_poison_subsequent_valid_frame(self):
        registry = TargetRegistry()
        self.assertEqual([], self.update(registry, stamp=99999, now=10.0))
        self.assertEqual(1, len(self.update(registry)))

    def test_approach_goal_stays_outside_standoff_plus_uncertainty(self):
        registry = TargetRegistry()
        target = self.update(registry)[0]
        choice = registry.choose_goal(target["target_id"], RobotPose(-1,0,0,"map",10.0,0.1), 10.0)
        self.assertEqual("requires_plan_validation", choice["status"])
        goal = choice["goal"]
        self.assertEqual(0, goal["z"])
        self.assertEqual("map", goal["frame_id"])
        distance = math.hypot(target["pose"]["x"]-goal["x"], target["pose"]["y"]-goal["y"])
        self.assertAlmostEqual(1.5+target["uncertainty_m"]+0.1+0.55+0.15+0.3, distance)
        self.assertEqual(0.55, choice["margins_m"]["robot_radius"])
        self.assertGreater(goal["x"], 0)
        with self.assertRaises(GroundingError):
            registry.choose_goal(target["target_id"], RobotPose(0,0,0,"map",10.0), 10.0, standoff=0.99)

    def test_already_close_never_requests_backing_or_approach(self):
        registry = TargetRegistry()
        target = self.update(registry)[0]
        for x in (2.0, 3.0):
            choice = registry.choose_goal(target["target_id"], RobotPose(x,0,0,"map",10.0), 10.0)
            self.assertEqual("already_within_standoff", choice["status"])
            self.assertIsNone(choice["goal"])

    def test_goal_rejects_stale_wrong_frame_or_nonfinite_robot_pose(self):
        registry = TargetRegistry()
        target_id = self.update(registry)[0]["target_id"]
        for pose in (RobotPose(0,0,0,"odom",10), RobotPose(0,0,0,"map",9),
                     RobotPose(math.nan,0,0,"map",10), RobotPose(0,0,0,"map",10,-1)):
            with self.assertRaises(GroundingError):
                registry.choose_goal(target_id, pose, 10.0)

    def test_config_cannot_disable_depth_or_minimum_standoff(self):
        for kwargs in ({"min_standoff": 0.5}, {"default_standoff": 0.5},
                       {"min_valid_fraction": 0}, {"max_depth_samples": 0},
                       {"target_ttl": math.inf}, {"max_targets": 1.5}):
            with self.subTest(kwargs=kwargs), self.assertRaises(ValueError):
                GroundingConfig(**kwargs)


if __name__ == "__main__":
    unittest.main()
