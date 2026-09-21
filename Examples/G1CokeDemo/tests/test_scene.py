"""Physical command application, sensor timing and exact scene regressions."""
from pathlib import Path
import unittest
from unittest.mock import patch

import mujoco
import numpy as np

from coke_demo.scene import CokeScene
from coke_demo.simulation import Simulation


EXPERT = Path(__file__).resolve().parents[1] / "expert"


class FakeRenderer:
    """Deterministic raw exposures keep camera packing tests OpenGL-free."""
    def __init__(self, model, height, width):
        self.mode = "rgb"
        self.can = model.geom("bottle").id

    def update_scene(self, data, camera):
        pass

    def render(self):
        if self.mode == "depth":
            return np.full((240, 320), 1.2344, dtype=np.float32)
        if self.mode == "segmentation":
            frame = np.full((240, 320, 2), -1, dtype=np.int32)
            frame[10:20, 20:30] = [self.can, int(mujoco.mjtObj.mjOBJ_GEOM)]
            return frame
        frame = np.zeros((240, 320, 3), dtype=np.uint8)
        frame[..., 0] = 255
        return frame

    def enable_depth_rendering(self):
        self.mode = "depth"

    def disable_depth_rendering(self):
        self.mode = "rgb"

    def enable_segmentation_rendering(self):
        self.mode = "segmentation"

    def disable_segmentation_rendering(self):
        self.mode = "rgb"

    def close(self):
        pass


@unittest.skipUnless((EXPERT / "model.mjb").exists(), "Run prepare-assets.py first")
class SceneTests(unittest.TestCase):
    def test_targets_reproduce_expert_physics_and_drive_a_different_target(self):
        scene, replay = CokeScene(EXPERT), Simulation(EXPERT)
        for target in replay.references[:100]:
            scene.step(target)
            replay.step()
        np.testing.assert_array_equal(scene.data.qpos, replay.data.qpos)
        np.testing.assert_array_equal(scene.data.qvel, replay.data.qvel)
        changed = replay.references[100].copy()
        changed[29] += .1
        scene.step(changed)
        replay.step()
        self.assertGreater(abs(scene.data.qpos[scene.qa[29]] - replay.data.qpos[replay.qa[29]]), 1e-5)
        self.assertEqual(scene.frame, 101)

    def test_invalid_commands_do_not_advance_physics(self):
        scene = CokeScene(EXPERT)
        for target in (np.zeros(42), np.full(43, np.nan), np.full(43, 100)):
            before = scene.data.qpos.copy()
            with self.assertRaises(ValueError):
                scene.step(target)
            np.testing.assert_array_equal(scene.data.qpos, before)
            self.assertEqual(scene.frame, 0)

    def test_camera_age_packing_reset_and_physics_ownership(self):
        scene = CokeScene(EXPERT)
        with patch("coke_demo.scene.mujoco.Renderer", FakeRenderer):
            initial_qpos = scene.data.qpos.copy()
            first = scene.observation()
            np.testing.assert_array_equal(scene.data.qpos, initial_qpos)
            self.assertEqual(first["image"].shape, (1, 5, 240, 320))
            self.assertEqual(first["image"].dtype, np.float32)
            np.testing.assert_allclose(first["image"][0, 3], 1.234 / 5)
            np.testing.assert_array_equal(first["image"][0, 0], 1)
            self.assertTrue(first["detection_valid"])
            self.assertEqual(int(first["mask"].sum()), 100)
            self.assertEqual(first["camera_frame"], 0)
            scene.step(scene.references[0])
            second = scene.observation()
            self.assertEqual(second["camera_frame"], 0)
            self.assertEqual(second["sim_time"] - second["camera_sim_time"], .025)
            first["image"][:] = 0
            self.assertGreater(float(second["image"].sum()), 0)
            scene.step(scene.references[1])
            third = scene.observation()
            self.assertEqual(third["camera_frame"], 2)
            self.assertEqual(third["camera_sim_time"], .05)
            scene.reset()
            reset = scene.observation()
            self.assertEqual(reset["camera_frame"], 0)
            self.assertEqual(reset["epoch"], first["epoch"] + 1)
            np.testing.assert_allclose(reset["base_position"], [0, 0, .793])
            np.testing.assert_allclose(reset["imu_specific_force"], [0, 0, 9.81])
            scene.close()

    def test_export_keeps_desks_can_and_fixed_robot(self):
        scene = CokeScene(EXPERT)
        description = scene.scene_description()
        bodies = {body["name"] for body in description["bodies"]}
        self.assertTrue({"pelvis", "table_body", "left_table_body", "right_table_body", "bottle_body"} <= bodies)
        self.assertEqual(description["provenance"]["joint_count"], 43)
        geoms = {geom["name"]: geom for geom in description["geoms"]}
        self.assertIn("bottle", geoms)
        self.assertEqual(geoms["placement_decal"]["position"], np.round(scene.result["marker"], 7).tolist())
        state = scene.scene_state()
        self.assertEqual(state["scene_id"], description["id"])
        self.assertEqual(len(state["positions"]), 3 * scene.model.nbody)
        self.assertEqual(len(state["quaternions"]), 4 * scene.model.nbody)
        scene.reset()
        self.assertEqual(scene.scene_description()["id"], description["id"])


if __name__ == "__main__":
    unittest.main()
