"""Actual shared-model/render checks; actor sites are evaluation oracles only."""
import hashlib
import json
import math
import struct
import unittest
import xml.etree.ElementTree as ET

from sim import people_scene
from sim.shared_perception_scene import (CAMERA_DIMENSIONS, PEOPLE_CAMERAS, SCENE,
                                         SharedPerceptionScene, assemble_office_xml)


class SharedPerceptionSceneTests(unittest.TestCase):
    def setUp(self):
        self.scene = SharedPerceptionScene('shared-scene-test')
        self.addCleanup(self.scene.close)

    def test_original_office_elements_and_collision_geometry_are_retained(self):
        original_bytes = people_scene.OFFICE.read_bytes()
        original = ET.fromstring(original_bytes)
        combined = ET.fromstring(self.scene.xml)
        # Existing world children include furnished geometry, all robot bodies,
        # indicator and original camera definitions. None is replaced/rebased.
        children = list(combined.find('worldbody'))
        for index, node in enumerate(original.find('worldbody')):
            self.assertEqual(ET.tostring(node), ET.tostring(children[index]))
        for name in ('option', 'actuator', 'default', 'contact'):
            a, b = original.find(name), combined.find(name)
            self.assertEqual(None if a is None else ET.tostring(a), None if b is None else ET.tostring(b))
        for geom in combined.findall('.//geom'):
            if geom.get('name', '').startswith('person-'):
                self.assertEqual((geom.get('contype'), geom.get('conaffinity')), ('0', '0'))
        self.assertEqual(self.scene.xml, assemble_office_xml())
        self.assertEqual(people_scene.OFFICE.read_bytes(), original_bytes)
        self.assertEqual(self.scene.package_sha256, hashlib.sha256(self.scene.package_json).hexdigest())
        self.assertEqual(self.scene.config['model']['sha256'], hashlib.sha256(self.scene.xml.encode()).hexdigest())
        self.assertNotEqual(self.scene.model_sha256, json.loads(SCENE.read_bytes())['model']['sha256'])

    def test_shared_y_up_calibration_covers_actual_people_and_hazard_pixels(self):
        s, np = self.scene, self.scene.np
        for camera, dimensions in CAMERA_DIMENSIONS.items():
            position, c = s.calibration(camera)
            self.assertEqual((c['width'], c['height']), dimensions)
            transform = np.array(c['camera_to_site']).reshape(4, 4)
            np.testing.assert_allclose(transform[:3, 3], position)
            self.assertAlmostEqual(float(np.linalg.det(transform[:3, :3])), 1, places=6)
            raw = s.render(camera)
            self.assertEqual(raw[:8], b'\x89PNG\r\n\x1a\n')
            self.assertEqual(struct.unpack('>II', raw[16:24]), dimensions)
            if camera not in PEOPLE_CAMERAS:
                continue
            # Only this test sees joint truth; the camera DTO contains none.
            for number in (1, 2):
                for joint in people_scene.SITES:
                    sid = s.mj.mj_name2id(s.model, s.mj.mjtObj.mjOBJ_SITE, f'person-{number}/{joint}')
                    x, y, z = s.data.site_xpos[sid]
                    point = np.array([x, z, -y])
                    local = transform[:3, :3].T @ (point - transform[:3, 3])
                    self.assertGreater(local[2], 0)
                    px, py = c['fx']*local[0]/local[2]+c['cx'], c['fy']*local[1]/local[2]+c['cy']
                    self.assertTrue(8 < px < dimensions[0]-8 and 8 < py < dimensions[1]-8, (camera, joint, px, py))
            self.assertNotIn('actor', json.dumps(c))
            self.assertNotIn('joints', json.dumps(c))

    def test_one_model_advances_people_and_office_and_exports_mocap_torsos(self):
        s = self.scene
        model, data = s.model, s.data
        initial = s.presentation()
        initial_transforms = {v['id']: v for v in initial['dynamic_geoms']}
        people = [g for g in initial['geometry']['geoms'] if g['name'].startswith('person-')]
        self.assertGreater(len(people), 30)
        self.assertTrue(all(g['dynamic'] and g['id'] in initial_transforms for g in people))
        before = {name: s.render(name) for name in PEOPLE_CAMERAS}
        s.advance(4_800_000_000)
        self.assertIs(s.model, model)
        self.assertIs(s.data, data)
        self.assertAlmostEqual(s.data.time, 4.8)
        self.assertAlmostEqual(s.model.opt.timestep, .01)
        later = s.presentation()
        current = {v['id']: v for v in later['dynamic_geoms']}
        for geom in people:
            if geom['name'].endswith('-torso'):
                self.assertNotEqual(initial_transforms[geom['id']], current[geom['id']])
        for camera in PEOPLE_CAMERAS:
            self.assertNotEqual(before[camera], s.render(camera))
        for contact in s.data.contact:
            for geom_id in contact.geom:
                name = s.mj.mj_id2name(s.model, s.mj.mjtObj.mjOBJ_GEOM, int(geom_id)) or ''
                self.assertFalse(name.startswith('person-'))

    def test_time_and_camera_contract(self):
        for value in (-1, 120_000_000_001, True, math.nan, 2.5):
            with self.assertRaises(ValueError):
                self.scene.advance(value)
        self.scene.advance(250_000_000)
        with self.assertRaises(ValueError):
            self.scene.advance(100_000_000)
        for action in (self.scene.render, self.scene.calibration):
            with self.assertRaises(ValueError):
                action('camera-b')  # source alias remains the adapter's concern
        self.scene.close()
        self.scene.close()


if __name__ == '__main__':
    unittest.main()
