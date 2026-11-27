"""Actual model/renderer checks; ground truth is only an independent test oracle."""
import hashlib
import json
import math
from pathlib import Path
import tempfile
import unittest

from sim.people_scene import CAMERAS, HEIGHT, OFFICE, WIDTH, PeopleScene, capture, scene_xml


class PeopleSceneTests(unittest.TestCase):
    def setUp(self):
        self.scene=PeopleScene();self.addCleanup(self.scene.close)

    def test_deterministic_scene_and_pose_without_mutating_office(self):
        before=OFFICE.read_bytes()
        self.assertEqual(scene_xml(),self.scene.xml)
        self.scene.pose(2);first=self.scene.truth()
        self.scene.pose(5);self.assertNotEqual(self.scene.truth(),first)
        self.scene.pose(2);self.assertEqual(self.scene.truth(),first)
        self.assertEqual(OFFICE.read_bytes(),before)

    def test_clear_full_bodies_project_into_both_actual_calibrated_cameras(self):
        np=self.scene.np
        for camera in CAMERAS:
            c=self.scene.calibration(camera);m=np.array(c['camera_to_scene']).reshape(4,4)
            self.assertAlmostEqual(float(np.linalg.det(m[:3,:3])),1,places=6)
            for actor in self.scene.truth()['actors']:
                for point in actor['joints_scene_m'].values():
                    local=m[:3,:3].T@(np.array(point)-m[:3,3])
                    self.assertGreater(local[2],0)
                    x=c['fx']*local[0]/local[2]+c['cx'];y=c['fy']*local[1]/local[2]+c['cy']
                    self.assertTrue(8<x<WIDTH-8 and 8<y<HEIGHT-8,(camera,x,y))

    def test_actual_pixels_change_with_articulation_and_crossing(self):
        for camera in CAMERAS:
            a=self.scene.render(camera)
            self.assertEqual(a[:8],b'\x89PNG\r\n\x1a\n')
            self.scene.pose(4.8);b=self.scene.render(camera)
            self.assertNotEqual(hashlib.sha256(a).digest(),hashlib.sha256(b).digest())
            self.scene.pose(0)

    def test_inputs_keep_truth_separate_and_refuse_output_reuse(self):
        with tempfile.TemporaryDirectory(prefix='people-scene-') as directory:
            out=Path(directory)/'capture'
            result=capture(out,times=(0.,4.8))
            self.assertTrue(result['ground_truth_separate'])
            manifest=json.loads((out/'detector-input/frames.json').read_text())
            self.assertEqual(len(manifest['frames']),4)
            for path in (out/'detector-input').glob('*.json'):
                text=path.read_text()
                self.assertNotIn('actor_id',text);self.assertNotIn('joints_scene_m',text)
            for item in manifest['frames']:
                raw=(out/'detector-input'/item['image']).read_bytes()
                self.assertEqual(hashlib.sha256(raw).hexdigest(),item['sha256'])
            with self.assertRaises(ValueError):capture(out)

    def test_invalid_pose_or_camera_refused(self):
        for value in (-1,math.nan,math.inf,121,True):
            with self.assertRaises(ValueError):self.scene.pose(value)
        with self.assertRaises(ValueError):self.scene.render('unknown')


if __name__=='__main__':unittest.main()
