import hashlib
import os
from pathlib import Path
import tempfile
import unittest
import xml.etree.ElementTree as ET

import numpy as np

from integration.xiaofire import scene
from integration.xiaofire.perception import validate_calibration

ROOT = Path(__file__).resolve().parents[2]
OFFICE = ROOT / "examples/collaborative-office/office.xml"


class SceneTests(unittest.TestCase):
    def test_projection_bounds_catch_partial_visibility_and_optical_plane_crossing(self):
        calibration = {"camera_to_world": [1, 0, 0, 0, 0, 1, 0, 0, 0, 0, 1, 0, 0, 0, 0, 1],
                       "fx": 100, "fy": 100, "cx": 50, "cy": 50, "width": 100, "height": 100}
        self.assertFalse(scene.projected_visible(calibration, [1.1, 0, 2], 0))
        self.assertTrue(scene.projected_visible(calibration, [1.1, 0, 2], .3))
        self.assertFalse(scene.projected_visible(calibration, [0, 0, -2], .3))
        self.assertTrue(scene.projected_visible(calibration, [0, 0, 0], .3))
        # Crossing the optical plane does not imply visibility when every
        # corner lies outside the same frustum side plane.
        for center in ([100, 0, 0], [-100, 0, 0], [0, 100, 0], [0, -100, 0]):
            self.assertFalse(scene.projected_visible(calibration, center, .3))
        for point, radius in (([0, 0], .3), ([float("nan"), 0, 2], .3), ([0, 0, 2], -1)):
            with self.assertRaises(ValueError):
                scene.projected_visible(calibration, point, radius)

    def test_office_contacts_and_body_placements_preserved(self):
        with tempfile.TemporaryDirectory() as directory:
            output = Path(directory) / "office.xml"
            scene.compose_office_file(OFFICE, output)
            before, after = ET.parse(OFFICE).getroot(), ET.parse(output).getroot()
            original = {g.get("name"): dict(g.attrib) for g in before.iter("geom") if g.get("name")}
            new = {g.get("name"): dict(g.attrib) for g in after.iter("geom") if g.get("name")}
            for name, values in original.items():
                self.assertEqual(values, new[name], name)
            for name in new.keys() - original.keys():
                self.assertEqual(new[name]["contype"], "0")
                self.assertEqual(new[name]["conaffinity"], "0")
            for body in before.iter("body"):
                if body.get("name"):
                    self.assertEqual(body.attrib, after.find(f".//body[@name='{body.get('name')}']").attrib)
            self.assertIsNotNone(after.find(f".//camera[@name='{scene.CAMERA_NAME}']"))
            with self.assertRaisesRegex(ValueError, "already contains"):
                scene.compose_office_file(output, Path(directory) / "duplicate.xml")

    @unittest.skipUnless(os.environ.get("XIAO_FIRE_ASSETS"), "pinned Go2 assets required")
    def test_actual_camera_basis_floor_projection_and_replay_boundary(self):
        import mujoco as mj
        from sim.patrol_physics import compose_office, INITIAL
        with tempfile.TemporaryDirectory() as directory:
            output = Path(directory) / "office.xml"
            scene.compose_office_file(OFFICE, output)
            xml = compose_office(output, os.environ["XIAO_FIRE_ASSETS"])
            model = mj.MjModel.from_xml_string(xml)
            data = mj.MjData(model)
            data.qpos[:7] = INITIAL
            mj.mj_forward(model, data)
            calibration = scene.camera_calibration(model, data, "scene-test", hashlib.sha256(xml.encode()).hexdigest())
            validate_calibration(calibration, "scene-test", (scene.WIDTH, scene.HEIGHT))
            t = np.array(calibration["camera_to_world"]).reshape(4, 4)
            np.testing.assert_allclose(t[:3, :3].T @ t[:3, :3], np.eye(3), atol=1e-9)
            np.testing.assert_allclose(t[:3, 3], [5.5, 2.3, -3.6])
            ground = np.array([6.6, 0, -2.4])
            optical = t[:3, :3].T @ (ground - t[:3, 3])
            pixel = [calibration["fx"] * optical[0]/optical[2] + calibration["cx"],
                     calibration["fy"] * optical[1]/optical[2] + calibration["cy"]]
            ray = t[:3, :3] @ np.array([(pixel[0]-calibration["cx"])/calibration["fx"],
                                      (pixel[1]-calibration["cy"])/calibration["fy"], 1])
            projected = t[:3, 3] - t[1, 3]/ray[1] * ray
            np.testing.assert_allclose(projected, ground, atol=1e-8)
            self.assertTrue(scene.world_point_in_view(calibration, ground))
            self.assertTrue(scene.projected_visible(calibration, ground, .65))
            # The complete cue, including the visible floor base, must fit in
            # the calibrated image. A clipped base biases floor projection.
            office = ET.parse(output).getroot()
            fixture = office.find(".//body[@name='xiao-fire-fixture']")
            origin = np.array([float(v) for v in fixture.get("pos").split()])
            for name in scene.FIRE_GEOMS:
                geom = fixture.find(f"geom[@name='{name}']")
                offset = np.array([float(v) for v in geom.get("pos").split()])
                vertices = [float(v) for v in office.find(f".//mesh[@name='{name}']").get("vertex").split()]
                for vertex in np.array(vertices).reshape(-1, 3):
                    nav = origin + offset + vertex
                    world = np.array([nav[0], nav[2], -nav[1]])
                    optical = t[:3, :3].T @ (world - t[:3, 3])
                    u = calibration["fx"] * optical[0]/optical[2] + calibration["cx"]
                    v = calibration["fy"] * optical[1]/optical[2] + calibration["cy"]
                    self.assertTrue(32 < u < scene.WIDTH-32 and 32 < v < scene.HEIGHT-32, (name, u, v))
            # Bound the complete robot, including corners, along the doorway
            # approach. Runtime must also check the actual route and poses.
            for point in ((-6, 0), (4.2, 2.4), (4.2, 1.5), (4.6, 1.3), (4.8, 1.3)):
                self.assertFalse(scene.projected_visible(calibration, [point[0], .35, -point[1]], .85), point)
            # Recorded approach deviation from the prior wider camera view.
            self.assertFalse(scene.projected_visible(calibration, [4.68366457007133, .34747930134716587, -1.9612816236800708], .85))
            self.assertFalse(scene.projected_visible(calibration, [5, .35, -1.3], .65))
            # The initial Go2 and proposed west-side stand-off footprint stay
            # outside the fixed camera; final navigation still checks its route.
            for center in ((-6, 0), (4.2, 2.4)):
                for dx in (-.45, .45):
                    for dy in (-.45, .45):
                        for height in (0, .65):
                            self.assertFalse(scene.world_point_in_view(calibration, [center[0]+dx, height, -center[1]-dy], 20))
            qpos = data.qpos.copy()
            scene.set_cue(model, "clean")
            self.assertEqual(model.geom_rgba[model.geom(scene.FIRE_GEOMS[0]).id, 3], 0)
            scene.set_cue(model, "fire", 2)
            self.assertEqual(model.geom_rgba[model.geom(scene.FIRE_GEOMS[1]).id, 3], 1)
            scene.set_cue(model, "orange-distractor")
            self.assertEqual(model.geom_rgba[model.geom("xiao-orange-distractor").id, 3], 1)
            np.testing.assert_array_equal(data.qpos, qpos)


if __name__ == "__main__":
    unittest.main()
