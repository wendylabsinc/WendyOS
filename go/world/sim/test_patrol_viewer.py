"""Native patrol bridge projection checks; no running device or mission needed."""
import copy
import hashlib
import importlib.util
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

from sim.patrol_viewer import display, fusion_view


class PatrolViewerTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        root = Path(self.temp.name)
        self.session, self.evidence = root / 'session', root / 'originals'
        (self.session / 'evidence').mkdir(parents=True)
        self.evidence.mkdir()
        self.physics = dict(stamp=20., run_id='patrol-test', position=[4., 5., .3],
                            linear_velocity_world=[.5, 0, 0], lidar_points=[[4., 6., .4]],
                            dynamic_geoms=[dict(id=31, position=[4., 5., .2], rotation=[1, 0, 0, 0, 1, 0, 0, 0, 1])])
        covariance = [0.] * 36
        covariance[0], covariance[1], covariance[6], covariance[7] = .04, .005, .005, .02
        self.mission = dict(patrol=dict(mission_id='one-patrol', state='navigating', current_room='studio',
            expires_at_unix_ms=1_100_000, nominal_speed_mps=.55, recent_findings=[],
            rooms=[dict(id='studio', name='Studio', polygon=[[0, 0], [3, 0], [3, 2]],
                        visits=2, last_observed_s=12., blocked_until_unix_ms=1_025_000,
                        blocked_reason='door blocked', evidence=['retained-ref'], candidates=[dict(x=2, y=1)])]),
            navigation=dict(ready=True, pose=dict(x=3.8, y=5.2, yaw=.4), covariance=covariance,
                            route=[[3.8, 5.2], [2., 1.]]))

    def project(self):
        with patch('sim.patrol_viewer.time.time', return_value=1000.):
            return display(self.physics, self.mission, 7, 2, self.session, self.evidence)

    def test_return_route_and_deadline(self):
        self.mission['patrol']['state']='returning'
        self.mission['patrol']['return']={'destination':{'x':1.,'y':2.,'yaw':0.},'before_expiry_seconds':30,'begin_at_unix_ms':1_070_000,'requested':True}
        state=self.project()['snapshot']
        self.assertEqual(state['patrol']['return']['begin_at_s'],90.)
        self.assertTrue(state['robots'][0]['path'])
        self.mission['patrol']['state']='completed'
        state=self.project()['snapshot']
        self.assertFalse(state['authorized'])
        self.assertTrue(state['stopped'])
        self.assertEqual(state['robots'][0]['path'],[])

    def test_truth_and_estimate_remain_distinct_and_inputs_unchanged(self):
        before = copy.deepcopy((self.physics, self.mission))
        state = self.project()
        self.assertEqual((self.physics, self.mission), before)
        self.assertEqual(state['mode'], 'go2-patrol')
        self.assertEqual(state['dynamic_geoms'], self.physics['dynamic_geoms'])
        snapshot = state['snapshot']
        self.assertEqual(snapshot['robots'][0]['position'], [4., 5., .3])
        self.assertEqual(snapshot['patrol']['localization']['position'], [3.8, 5.2, .2])
        self.assertEqual(snapshot['patrol']['localization']['covariance'], [.04, .005, .005, .02])
        self.assertEqual(snapshot['robots'][0]['path'], [[3.8, 5.2, .06], [2., 1., .06]])
        self.assertEqual(state['last_command_sequence'], 2)
        self.assertEqual(state['sequence'], 7)

    def test_wall_expiry_projects_to_sim_time_without_renewing_coverage(self):
        patrol = self.project()['snapshot']['patrol']
        self.assertEqual(patrol['expires_at_s'], 120.)
        room = patrol['rooms'][0]
        self.assertEqual(room['blocked_until_s'], 45.)
        self.assertEqual(room['last_observed_s'], 12.)
        self.assertEqual(room['visits'], 2)
        self.assertEqual(room['evidence'], ['retained-ref'])
        self.assertNotIn('candidates', room)
        self.assertNotIn('blocked_until_unix_ms', room)
        self.mission['patrol']['expires_at_unix_ms'] = 999_999
        self.assertEqual(self.project()['snapshot']['patrol']['expires_at_s'], 20.)

    def test_schedule_deadlines_keep_wall_clock_and_do_not_rewrite_observations(self):
        room = self.mission['patrol']['rooms'][0]
        room.update(revisit_seconds=30, next_due_unix_ms=995_000)
        before = copy.deepcopy(self.mission)
        projected = self.project()['snapshot']['patrol']['rooms'][0]
        self.assertEqual(projected['next_due_s'], 15.)
        self.assertEqual(projected['revisit_seconds'], 30)
        self.assertEqual(projected['last_observed_s'], 12.)
        self.assertNotIn('next_due_unix_ms', projected)
        self.assertEqual(self.mission, before)
        self.mission['patrol']['state'] = 'paused'
        self.assertEqual(self.project()['snapshot']['patrol']['rooms'][0]['next_due_s'], 15.)

    def test_localization_unavailable_never_reuses_truth_as_estimate(self):
        self.mission['navigation']['ready'] = False
        state = self.project()
        self.assertFalse(state['snapshot']['patrol']['localization']['valid'])
        self.mission['navigation'] = {}
        state = self.project()
        self.assertNotIn('localization', state['snapshot']['patrol'])
        self.assertEqual(state['snapshot']['robots'][0]['path'], [])

    def test_mission_stop_pause_and_coverage_values_are_observed(self):
        for status, stopped, authorized in [('navigating', False, True), ('paused', True, True),
                                             ('stopped', True, False), ('expired', True, False),
                                             ('failed', True, False)]:
            self.mission['patrol']['state'] = status
            snapshot = self.project()['snapshot']
            self.assertEqual(snapshot['stopped'], stopped)
            self.assertEqual(snapshot['authorized'], authorized)
            self.assertEqual(snapshot['completed'], 2)
            self.assertEqual(snapshot['total'], 1)
            self.assertEqual(snapshot['patrol']['rooms'][0]['visits'], 2)

    def test_fusion_axes_and_original_evidence_bytes_preserved(self):
        raw = b'original simulated PNG fixture bytes'
        digest = hashlib.sha256(raw).hexdigest()
        (self.evidence / (digest + '.png')).write_bytes(raw)
        output = dict(now_ns=18_000_000_000, result=dict(state='hazard', reason='test',
            hazard=dict(position_m=[1., .4, -2.], uncertainty_m=.5, confidence=.8)),
            observations=[dict(sensor_id='camera-a', kind='camera', position_m=[3., .6, -4.],
                evidence=dict(sha256=digest, size=len(raw), content_type='image/png'))])
        value = fusion_view(output, self.session, self.evidence)
        self.assertEqual(value['position'], [1., 2., .4])
        self.assertEqual(value['observations'][0]['position'], [3., 4., .6])
        self.assertEqual(value['sample_time_s'], 18.)
        self.assertEqual((self.session / value['observations'][0]['evidence_path']).read_bytes(), raw)
        self.assertNotIn('original_envelope_sha256', value)
        self.assertNotIn('native_evidence', value)

    def test_corrupt_original_does_not_become_a_camera_thumbnail(self):
        digest = hashlib.sha256(b'original').hexdigest()
        (self.evidence / (digest + '.png')).write_bytes(b'changed')
        output = dict(now_ns=1, result=dict(state='insufficient', reason='no image'), observations=[
            dict(sensor_id='camera-a', kind='camera', position_m=[0., .3, 0.],
                 evidence=dict(sha256=digest, size=8, content_type='image/png'))])
        value = fusion_view(output, self.session, self.evidence)
        self.assertNotIn('evidence_path', value['observations'][0])
        self.assertFalse((self.session / 'evidence' / (digest + '.png')).exists())

    def test_interaction_recorder_refuses_waiting_without_a_control(self):
        spec = importlib.util.spec_from_file_location('record_patrol_twin',
            Path(__file__).resolve().parents[1] / 'scripts/record-patrol-twin.py')
        recorder = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(recorder)
        (self.session / 'geometry.json').write_text(json.dumps({'run_id': 'patrol-test'}))
        (self.session / 'state.json').write_text(json.dumps({'run_id': 'patrol-test',
            'mode': 'go2-patrol', 'snapshot': {'patrol': {'state': 'waiting'}}}))
        output = Path(self.temp.name) / 'recording'
        output.mkdir()
        with self.assertRaisesRegex(ValueError, 'requires a navigating patrol'):
            recorder.capture(self.session, output, 'interactions', 12, 12)
        self.assertFalse((self.session / 'commands').exists())

    def test_recorded_pause_requires_motion_and_resume_waits_for_acknowledgement(self):
        spec = importlib.util.spec_from_file_location('record_patrol_motion',
            Path(__file__).resolve().parents[1] / 'scripts/record-patrol-twin.py')
        recorder = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(recorder)
        snapshot = {'patrol': {'state': 'navigating', 'localization': {'valid': True}},
                    'robots': [{'velocity': [0., 0., 0.]}]}
        self.assertIsNone(recorder.interaction_action(5, 30, set(), None, snapshot))
        snapshot['robots'][0]['velocity'] = [.4, 0., 0.]
        self.assertEqual(recorder.interaction_action(5, 30, set(), None, snapshot), 'patrol_pause')
        self.assertIsNone(recorder.interaction_action(25, 30, set(), None, snapshot))
        self.assertIsNone(recorder.interaction_action(10, 30, {'patrol_pause'}, None, snapshot))
        self.assertIsNone(recorder.interaction_action(11, 30, {'patrol_pause'}, 9, snapshot))
        self.assertEqual(recorder.interaction_action(12, 30, {'patrol_pause'}, 9, snapshot), 'patrol_resume')
        snapshot['patrol']['localization']['valid'] = False
        self.assertFalse(recorder.is_navigating_motion(snapshot))

    def test_unobserved_and_no_fusion_need_no_placeholder_evidence(self):
        self.mission['patrol']['rooms'][0]['evidence'] = None
        self.assertEqual(self.project()['snapshot']['patrol']['rooms'][0]['evidence'], [])
        self.assertIsNone(fusion_view(None, self.session, self.evidence))
        self.assertNotIn('fusion', self.project()['snapshot'])
        json.dumps(self.project(), allow_nan=False)

    def test_candidate_queries_are_not_displayed_as_active_routes(self):
        self.mission['patrol']['state'] = 'planning'
        self.assertEqual(self.project()['snapshot']['robots'][0]['path'], [])

    def test_old_capture_is_retained_but_not_displayed_as_live(self):
        output = dict(now_ns=1_000_000_000, result=dict(state='insufficient', reason='saved'),
                      observations=[dict(sensor_id='lidar-go2', kind='lidar', position_m=[0,0,0])])
        value = fusion_view(output, self.session, self.evidence, now=10)
        self.assertEqual(value['label'], 'Recorded observation')
        self.assertFalse(value['observations'][0]['active'])
        self.assertEqual(value['sample_time_s'], 1)


if __name__ == '__main__':
    unittest.main()
