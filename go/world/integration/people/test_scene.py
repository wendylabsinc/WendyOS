import base64
import hashlib
import json
import threading
import unittest

from integration.people.scene import Source


class Renderer:
    xml = '<mujoco/>'

    def __init__(self):
        self.threads = set()

    def pose(self, seconds):
        self.seconds = seconds
        self.threads.add(threading.get_ident())

    def render(self, camera):
        return (camera+str(self.seconds)).encode()

    def calibration(self, camera):
        return dict(width=640, height=480, fx=400, fy=400, cx=320, cy=240,
                    camera_to_scene=[1,0,0,2,0,1,0,3,0,0,1,4,0,0,0,1])

    def close(self):
        self.threads.add(threading.get_ident())


class Tests(unittest.TestCase):
    def setUp(self):
        self.renderer = Renderer()
        self.source = Source('test-run', 1_790_000_000_000_000_000, self.renderer)

    def tearDown(self):
        self.source.close()

    def test_original_capture_and_fixed_clock(self):
        self.assertIsNone(self.source.camera('camera-a'))
        self.source.step({'simulation_time_ns':250_000_000,'cameras':['camera-a','camera-b']})
        original = self.source.camera('camera-a')
        a = json.loads(original)
        sample = json.loads(base64.b64decode(a['capture']['sample_json']))
        media = base64.b64decode(a['media'])
        self.assertEqual(sample['sample_ns'],250_000_000)
        self.assertEqual(sample['position_m'],[2,4,-3])
        self.assertEqual(sample['evidence']['sha256'],hashlib.sha256(media).hexdigest())
        self.assertEqual(sample['frame'],self.source.identity['frame'])
        clock = self.source.clock('a'*64)
        later = self.source.clock('b'*64)
        self.assertEqual(clock['simulation_time_ns'],later['simulation_time_ns'])
        self.assertGreater(later['observation_sequence'],clock['observation_sequence'])
        self.assertEqual(self.source.camera('camera-a'),original)

    def test_disabled_camera_and_silence_do_not_reuse_capture(self):
        self.source.step({'simulation_time_ns':1,'cameras':['camera-a','camera-b']})
        self.source.step({'simulation_time_ns':2,'cameras':['camera-a']})
        self.assertIsNone(self.source.camera('camera-b'))
        self.source.step({'simulation_time_ns':3,'cameras':[]})
        self.assertIsNone(self.source.camera('camera-a'))
        self.assertEqual(self.source.clock('c'*64)['simulation_time_ns'],3)
        self.source.step({'simulation_time_ns':4,'cameras':['camera-b']})
        sample = json.loads(base64.b64decode(json.loads(self.source.camera('camera-b'))['capture']['sample_json']))
        self.assertEqual(sample['sequence'],4)
        self.assertEqual(len(self.renderer.threads),1)

    def test_invalid_controls_leave_source_unchanged(self):
        self.source.step({'simulation_time_ns':10,'cameras':[]})
        for value in ({'simulation_time_ns':10,'cameras':[]},
                      {'simulation_time_ns':False,'cameras':[]},
                      {'simulation_time_ns':20,'cameras':['camera-a','camera-a']},
                      {'simulation_time_ns':20,'cameras':['robot']},
                      {'simulation_time_ns':20,'cameras':[],'extra':1}):
            with self.assertRaises(ValueError):
                self.source.step(value)
        self.assertEqual(self.source.simulation_ns,10)
        self.assertEqual(self.source.sequence,1)


if __name__ == '__main__':
    unittest.main()
