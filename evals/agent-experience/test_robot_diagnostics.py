import copy
import unittest

from robot_diagnostics import validate_camera


class CameraEvidenceTests(unittest.TestCase):
    def test_rejects_stale_blank_or_wrongly_encoded_frames(self):
        frames = [{"stamp": 100+i, "age": .01, "width": 640, "height": 360, "encoding": "rgb8",
                   "frame": "camera_optical_frame", "step": 1920, "bytes": 640*360*3,
                   "min": 0, "max": 200} for i in range(5)]
        validate_camera(frames)
        for update in ({"age": 9}, {"max": 0}, {"encoding": "bgr8"}, {"bytes": 0}, {"stamp": 0}):
            changed = copy.deepcopy(frames)
            changed[0].update(update)
            with self.assertRaises(RuntimeError):
                validate_camera(changed)
        with self.assertRaises(RuntimeError):
            validate_camera(frames[:1])


if __name__ == "__main__":
    unittest.main()
