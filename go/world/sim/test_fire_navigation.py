from copy import deepcopy
import hashlib
import io
from pathlib import Path
import tempfile
import unittest

from PIL import Image
from sim.fire_navigation import NoObservationRoute, prepare_response, segment_distance


class FireNavigationTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        buffer = io.BytesIO()
        Image.new('RGB', (32, 24), (220, 90, 15)).save(buffer, format='JPEG')
        self.jpeg = Path(self.temp.name)/'original.jpg'
        self.jpeg.write_bytes(buffer.getvalue())
        self.scope = {'run_id': 'sim-fire-1', 'simulation': True,
                      'frame': {'id': 'office-floor', 'revision': 'office-test'}}
        self.calibration = 'a'*64
        inputs = [{'session_id': 'firmware-boot-1', 'channel_id': 0,
                   'frame_seq': i, 'timestamp_us': 100000+i*200000,
                   'sha256': hashlib.sha256(buffer.getvalue()).hexdigest(),
                   'width': 32, 'height': 24, 'origin': 'simulated',
                   'simulation_run': self.scope['run_id'],
                   'timestamp_clock': 'firmware-boot-microseconds',
                   'received_monotonic_s': 100+i*.2, 'expires_monotonic_s': 105+i*.2}
                  for i in range(3)]
        self.detection = {'schema': 'wendy.xiao-fire-detection.v1',
                          'origin': 'inferred', 'simulation_run': self.scope['run_id'],
                          'model': 'synthetic-flame-cue-v1',
                          'score_kind': 'fixture_rule_not_probability',
                          'detected': True, 'candidate': True, 'confirmation_frames': 3,
                          'position_m': [6.6, 0, -2.4],
                          'frame': deepcopy(self.scope['frame']),
                          'calibration_sha256': self.calibration,
                          'source': deepcopy(inputs[-1]), 'confirming_inputs': inputs,
                          'expires_monotonic_s': 105}
        self.nav = {'ready': True, 'provenance': 'simulated',
                    'inspection_mode': 'fixed-position-yaw-v1',
                    'pose': {'x': -6, 'y': 0, 'yaw': 0},
                    'policy_frozen': False, 'policy_digest': ''}
        self.inventory = {'rooms': [{'id': 'private-office',
            'polygon': [[2.8, 1.65], [7.8, 1.65], [7.8, 5.8], [2.8, 5.8]],
            'candidates': [{'x': 4.2, 'y': 2.4, 'yaw': 2},
                           {'x': 4.6, 'y': 2.4, 'yaw': 2}]}]}
        self.calls = []

    def route(self, body):
        self.calls.append(body)
        return {'reachable': True, 'route': [[-6, 0, 0], [body['x'], body['y'], body['yaw']]]}

    def prepare(self, route=None, **kwargs):
        return prepare_response(self.detection, self.jpeg, self.scope, self.nav,
            self.inventory, route or self.route, navigation_frame=self.scope['frame'],
            calibration_sha256=self.calibration, now_monotonic=101, **kwargs)

    def test_prepares_existing_tour_without_dispatch_and_preserves_evidence(self):
        result = self.prepare()
        self.assertFalse(result['motion_dispatched'])
        self.assertFalse(result['authority_supplied'])
        self.assertGreater(result['route_length_m'], 10)
        stop = result['schedule']['tour']['stops'][0]
        self.assertEqual(stop, {'room_id': 'private-office', 'position': [4.2, .3, -2.4], 'yaw': 0})
        self.assertEqual(result['schedule']['return']['position'], [-6, .3, 0])
        self.assertEqual(result['detection'], self.detection)
        self.assertEqual(result['expires_monotonic_s'], 105)
        result['detection']['source']['sha256'] = 'b'*64
        self.assertNotEqual(result['detection'], self.detection)
        self.assertEqual(len(self.calls), 1)
        self.assertEqual(set(self.calls[0]), {'x', 'y', 'yaw', 'policy_digest'})

    def test_rejects_stale_changed_run_physical_and_unconfirmed_before_route(self):
        variants = [('detected', False), ('simulation_run', 'other'),
                    ('frame', {'id': 'other', 'revision': 'other'}),
                    ('origin', 'measured'), ('expires_monotonic_s', 99)]
        for key, value in variants:
            before = deepcopy(self.detection)
            self.detection[key] = value
            with self.assertRaises(ValueError): self.prepare()
            self.detection = before
        self.assertEqual(self.calls, [])
        self.scope['simulation'] = False
        with self.assertRaises(ValueError): self.prepare()

    def test_rejects_renewed_deadline_or_retired_camera_session(self):
        self.detection['expires_monotonic_s'] = 110
        with self.assertRaisesRegex(ValueError, 'extended'): self.prepare()
        self.detection['expires_monotonic_s'] = 105
        self.detection['confirming_inputs'][0]['session_id'] = 'previous-boot'
        with self.assertRaisesRegex(ValueError, 'one firmware session'): self.prepare()
        self.assertEqual(self.calls, [])

    def test_rejects_changed_image_and_calibration(self):
        self.jpeg.write_bytes(self.jpeg.read_bytes()+b'changed')
        with self.assertRaisesRegex(ValueError, 'differs'): self.prepare()
        self.detection['calibration_sha256'] = 'b'*64
        with self.assertRaisesRegex(ValueError, 'calibration'): self.prepare()
        self.assertEqual(self.calls, [])

    def test_unreachable_route_does_not_create_alternate_motion(self):
        def blocked(body):
            self.calls.append(body)
            return {'reachable': False, 'reason': 'no_route', 'route': []}
        with self.assertRaisesRegex(ValueError, 'no reachable'): self.prepare(blocked)
        self.assertEqual(len(self.calls), 2)

    def test_candidate_endpoint_and_route_must_stay_away_from_incident(self):
        self.inventory['rooms'][0]['candidates'] = [{'x': 6.5, 'y': 2.4}]
        with self.assertRaisesRegex(ValueError, 'no reachable'): self.prepare()
        self.assertEqual(self.calls, [])
        self.inventory['rooms'][0]['candidates'] = [{'x': 4.2, 'y': 2.4}]
        def through_incident(body):
            return {'reachable': True, 'route': [[-6, 0, 0], [6.6, 2.4, 0], [4.2, 2.4, 0]]}
        with self.assertRaisesRegex(ValueError, 'no reachable'): self.prepare(through_incident)

    def test_wrong_planner_endpoint_is_rejected(self):
        def wrong_target(body):
            return {'reachable': True, 'route': [[-6, 0, 0], [0, 0, 0]]}
        with self.assertRaisesRegex(ValueError, 'endpoints'): self.prepare(wrong_target)

    def test_segment_distance_checks_between_sparse_waypoints(self):
        self.assertEqual(segment_distance([1, 0], [0, 0], [2, 0]), 0)
        self.assertEqual(segment_distance([1, 1], [0, 0], [2, 0]), 1)

    def test_camera_filter_rejects_route_and_records_reason_before_next_candidate(self):
        checked = []
        def accept(vertices):
            checked.append(deepcopy(vertices))
            return vertices[-1][0] > 4.3
        result = self.prepare(route_acceptor=accept)
        self.assertEqual(len(checked), 2)
        self.assertEqual(result['attempts'][0]['reason'], 'cameraFOV')
        self.assertEqual(result['schedule']['tour']['stops'][0]['position'][0], 4.6)
        self.assertEqual(checked[-1], result['route'])

    def test_camera_filter_fails_closed_without_motion(self):
        for rejected in (False, None, 1, 'yes'):
            with self.assertRaisesRegex(NoObservationRoute, 'no reachable') as failure:
                self.prepare(route_acceptor=lambda vertices: rejected)
            self.assertEqual([row['reason'] for row in failure.exception.attempts],
                             ['cameraFOV', 'cameraFOV'])

    def test_camera_filter_cannot_change_retained_route(self):
        def accept(vertices):
            vertices[-1][0] = 99
            return True
        result = self.prepare(route_acceptor=accept)
        self.assertEqual(result['route'][-1][0], 4.2)

    def test_camera_filter_error_does_not_prepare_motion(self):
        def failed(vertices):
            raise RuntimeError('projection failed')
        with self.assertRaisesRegex(RuntimeError, 'projection failed'):
            self.prepare(route_acceptor=failed)


if __name__ == '__main__':
    unittest.main()
