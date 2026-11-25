"""Sensor-derived office localization checks with actual MuJoCo camera pixels."""
import copy
import json
import math
import os
import subprocess
import unittest
from types import SimpleNamespace
from unittest.mock import patch

from sim.office_fusion import OfficeFusion, OfficeSensors, display_result, to_scene, to_world
from sim.test_macos_demo_bridge import LocalPipe


class PresentationTests(unittest.TestCase):
    def test_axis_round_trip(self):
        self.assertEqual(to_world([2, 3, 4]), [2, 4, -3])
        self.assertEqual(to_scene(to_world([2, 3, 4])), [2, 3, 4])

    def test_incomplete_evidence_never_displays_a_known_target_or_detected_tone(self):
        request = {'now_ns': 500_000_000, 'samples': [
            {'sensor_id': 'audio-1', 'kind': 'audio', 'position_m': [1, 2, -3]}]}
        view = display_result(request, {'state': 'insufficient_evidence', 'reason': 'no LiDAR support'})
        self.assertNotIn('position', view)
        self.assertNotIn('value', view['observations'][0])
        self.assertEqual(view['observations'][0]['position'], [1, 3, 2])

    def test_each_original_sample_and_claim_expires_between_worker_replies(self):
        fusion = OfficeFusion.__new__(OfficeFusion)
        fusion.runtime = SimpleNamespace(data=SimpleNamespace(time=2.1))
        fusion.pipes = {'atlas': SimpleNamespace(error=None)}
        fusion.catalog = [{'sensor_id': 'camera-a'}, {'sensor_id': 'lidar-atlas'}]
        fusion.latest = {'atlas': {'state': 'hazard', 'label': 'Warning', 'reason': 'supported',
            'sample_time_s': 1.9, 'valid_until_s': 2., 'position': [1, 2, 3],
            'missing_observations': [], 'replica_node': 'atlas', 'observations': [
                {'id': 'camera-a', 'kind': 'camera', 'label': 'Camera A', 'source_node': 'hub',
                 'age_s': 1.9, 'active': True, 'evidence_path': 'fusion/atlas/camera-a.png'},
                {'id': 'lidar-atlas', 'kind': 'lidar', 'label': 'Atlas LiDAR', 'source_node': 'atlas',
                 'age_s': .4, 'active': True}]}}
        view = fusion.snapshot('atlas')
        self.assertEqual([o['id'] for o in view['observations']], ['lidar-atlas'])
        self.assertEqual(view['missing_observations'][0]['reason'], 'expired')
        self.assertEqual(view['state'], 'insufficient_evidence')
        self.assertNotIn('position', view)
        self.assertEqual(len(fusion.latest['atlas']['observations']), 2)


@unittest.skipUnless(os.environ.get('WORLD_FUSION_DEMO_BINARY'), 'set WORLD_FUSION_DEMO_BINARY for actual MuJoCo fusion')
class ActualOfficeFusionTests(unittest.TestCase):
    def setUp(self):
        from sim.collaborative_runtime import SceneRuntime
        self.patch = patch('sim.collaborative_runtime.CoordinatorPipe', LocalPipe)
        self.patch.start()
        self.addCleanup(self.patch.stop)
        self.runtime = SceneRuntime('examples/collaborative-office/scene.json', 'unused', 'fusion-office-test')
        self.addCleanup(self.runtime.close)
        for _ in range(28):
            self.runtime.tick(.02)
        self.sensors = OfficeSensors(self.runtime)
        self.addCleanup(self.sensors.close)

    def evaluate(self, request):
        proc = subprocess.run([os.environ['WORLD_FUSION_DEMO_BINARY']], input=json.dumps(request) + '\n',
                              capture_output=True, text=True, timeout=10, check=True)
        return json.loads(proc.stdout)

    def test_rendered_camera_and_ray_returns_localize_the_fixture(self):
        request = self.sensors.capture()
        result = self.evaluate(request)
        self.assertEqual(result['state'], 'hazard', result['reason'])
        self.assertEqual(len(request['samples']), 7)
        point = to_scene(result['hazard']['position_m'])
        # Independent expected front centre, used only by the test assertion.
        error = math.dist(point, [-7.3, .761, .65])
        self.assertLess(error, .25)
        self.assertLess(error, result['hazard']['uncertainty_m'])
        self.assertGreater(result['spatial']['lidar_points_considered'], 200)
        self.assertGreater(len(result['spatial']['lidar_support_points']), 0)
        self.assertEqual(set(request), {'run_id', 'now_ns', 'frame', 'samples', 'media'})
        for sample in request['samples']:
            self.assertNotIn('target', sample)
            self.assertNotIn('source_local_m', sample)
        self.assertEqual(len(request['media']), 3)
        view = display_result(request, result)
        self.assertEqual(view['position'], point)
        self.assertTrue(all(o['target'] == point for o in view['observations'] if o['kind'] == 'camera'))

    def test_removing_lidar_or_camera_evidence_removes_localization(self):
        request = self.sensors.capture()
        no_lidar = copy.deepcopy(request)
        for sample in no_lidar['samples']:
            if sample['kind'] == 'lidar':
                sample['lidar']['points_m'] = []
        result = self.evaluate(no_lidar)
        self.assertEqual(result['state'], 'insufficient_evidence')
        self.assertNotIn('position', display_result(no_lidar, result))
        no_camera = copy.deepcopy(request)
        camera = next(s for s in no_camera['samples'] if s['sensor_id'] == 'camera-a')
        del no_camera['media'][camera['evidence']['sha256']]
        self.assertEqual(self.evaluate(no_camera)['state'], 'insufficient_evidence')

    def test_gas_and_audio_off_do_not_raise_a_warning(self):
        request = self.sensors.capture(alarm=False)
        result = self.evaluate(request)
        self.assertEqual(result['state'], 'not_detected', result['reason'])
        self.assertNotIn('position', display_result(request, result))

    def test_moving_the_fixture_changes_inferred_location(self):
        initial = self.evaluate(self.sensors.capture())
        rt = self.runtime
        body = rt.mj.mj_name2id(rt.model, rt.mj.mjtObj.mjOBJ_BODY, 'indicator')
        rt.model.body_pos[body][0] += .4
        rt.mj.mj_forward(rt.model, rt.data)
        rt._lidar()
        moved = self.evaluate(self.sensors.capture())
        self.assertEqual(moved['state'], 'hazard', moved['reason'])
        initial_point = to_scene(initial['hazard']['position_m'])
        moved_point = to_scene(moved['hazard']['position_m'])
        self.assertGreater(moved_point[0] - initial_point[0], .25)
        self.assertLess(math.dist(moved_point, [-6.9, .761, .65]), .25)


if __name__ == '__main__':
    unittest.main()
