from __future__ import annotations

from pathlib import Path
import sys
import unittest

import numpy as np


ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT))

from runtime.object_retarget import (  # noqa: E402
    MAXIMUM_ADDED_JOINT_STEP_RAD,
    ObjectRelativeRetargeter,
    camera_pose_from_live_joints,
)


class ObjectRetargetTests(unittest.TestCase):
    def setUp(self) -> None:
        self.runtime = ObjectRelativeRetargeter(ROOT / "bundle")

    def test_frozen_contract_and_zero_detection_are_exact_noop(self) -> None:
        base = self.runtime.reference_q[200].copy()
        velocity = np.zeros(15, dtype=np.float32)
        target, _, status = self.runtime.apply(
            frame=200,
            reference_q_43=base,
            reference_velocity_15=velocity,
            live_q_43=base,
            control_at_ns=1_000_000_000,
        )
        np.testing.assert_array_equal(target, base.astype(np.float32))
        self.assertFalse(status["available"])

    def test_live_waist_fk_reproduces_frozen_reference_camera_pose(self) -> None:
        with np.load(
            ROOT / "bundle" / "retarget" / "nominal-can-camera-reference.npz",
            allow_pickle=False,
        ) as values:
            reference_q = values["reference_q"]
            expected_position = values["camera_position_world"]
            expected_world_from_mujoco = values["camera_rotation_world_from_camera"]
        for frame in (0, 200, 400, 800, 1199):
            position, world_from_optical = camera_pose_from_live_joints(reference_q[frame])
            np.testing.assert_allclose(position, expected_position[frame], atol=1e-9)
            np.testing.assert_allclose(
                world_from_optical @ np.diag([1.0, -1.0, -1.0]),
                expected_world_from_mujoco[frame],
                atol=1e-9,
            )

    def test_late_detection_correction_is_slew_limited(self) -> None:
        frame = 299
        q = self.runtime.reference_q[frame]
        position, world_from_optical = camera_pose_from_live_joints(q)
        nominal_optical = world_from_optical.T @ (
            self.runtime.nominal_initial_world - position
        )
        intrinsics = {
            "frame_id": 1,
            "stream_id": "test-stream",
            "captured_at_unix_ns": 1_000_000_000,
            "source_width": 640,
            "source_height": 480,
            "source_intrinsics": {
                "fx": 600.0,
                "fy": 600.0,
                "ppx": 320.0,
                "ppy": 240.0,
            },
        }
        shifted = nominal_optical + world_from_optical.T @ np.asarray([0.04, -0.04, 0.0])
        u = 300.0 * shifted[0] / shifted[2] + 160.0
        v = 300.0 * shifted[1] / shifted[2] + 120.0
        mask = np.zeros((240, 320), dtype=bool)
        x, y = int(round(u)), int(round(v))
        mask[max(0, y - 3):min(240, y + 4), max(0, x - 3):min(320, x + 4)] = True
        depth = np.zeros((240, 320), dtype=np.float32)
        depth[mask] = max(0.11, shifted[2] - 0.0315)
        self.runtime.observe(
            depth_m=depth,
            mask=mask,
            metadata=intrinsics,
            detection_valid=True,
        )
        base = self.runtime.reference_q[frame]
        target, _, status = self.runtime.apply(
            frame=frame,
            reference_q_43=base,
            reference_velocity_15=np.zeros(15),
            live_q_43=q,
            control_at_ns=1_010_000_000,
        )
        self.assertTrue(status["available"])
        self.assertLessEqual(
            float(np.max(np.abs(target - base))),
            MAXIMUM_ADDED_JOINT_STEP_RAD + 1e-6,
        )
        self.assertTrue(np.all(np.abs(status["offset_world_xy_m"]) <= 0.040001))
        self.assertIsNotNone(status["distance_to_optimal_m"])
        self.assertAlmostEqual(
            status["distance_to_optimal_cm"],
            status["distance_to_optimal_m"] * 100.0,
            places=9,
        )
        self.assertEqual(status["measurement_frame_id"], 1)
        self.assertEqual(status["measurement_stream_id"], "test-stream")
        self.assertIsNotNone(status["pixel_displacement_px"])


if __name__ == "__main__":
    unittest.main()
