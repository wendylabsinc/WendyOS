import hashlib
import io
import json
import unittest
from unittest import mock

import numpy as np
from PIL import Image

from perception import pose
from perception.fetch_model import verify


def png(width=640, height=480, color=(255, 0, 0)):
    stream = io.BytesIO()
    Image.new("RGB", (width, height), color).save(stream, format="PNG")
    return stream.getvalue()


def output():
    data = np.zeros((1, 6, 56), dtype=np.float32)
    data[0, 0, 55] = .8
    return data


class PoseTests(unittest.TestCase):
    def test_rgb_letterbox_retains_original_pixel_coordinates(self):
        tensor, transform = pose.prepare_image(png())
        self.assertEqual(tensor.shape, (1, 384, 384, 3))
        self.assertEqual(tensor.dtype, np.int32)
        self.assertEqual((transform["left"], transform["top"]), (0, 48))
        np.testing.assert_array_equal(tensor[0, 0, 0], [0, 0, 0])
        np.testing.assert_array_equal(tensor[0, 48, 0], [255, 0, 0])
        raw = output()
        # Original (160,120) and (480,360), mapped into the padded input.
        raw[0, 0, 45:48] = [(120 * .6 + 48) / 384, 160 * .6 / 384, .7]
        raw[0, 0, 48:51] = [(360 * .6 + 48) / 384, 480 * .6 / 384, .3]
        actual = pose.decode_output(raw, transform)[0]
        for name, expected in (("left_ankle", (160, 120)), ("right_ankle", (480, 360))):
            self.assertAlmostEqual(actual[name]["x"], expected[0], places=4)
            self.assertAlmostEqual(actual[name]["y"], expected[1], places=4)
        self.assertAlmostEqual(actual["right_ankle"]["confidence"], .3)

    def test_portrait_rounding_has_independent_scale_axes(self):
        _, transform = pose.prepare_image(png(333, 700))
        raw = output()
        raw[0, 0, 45:48] = [.5, .5, .9]
        raw[0, 0, 48:51] = [.5, .5, .9]
        actual = pose.decode_output(raw, transform)[0]["left_ankle"]
        self.assertAlmostEqual(actual["x"], (192 - transform["left"]) / transform["scale_x"])
        self.assertAlmostEqual(actual["y"], 350)

    def test_padding_or_outside_image_cannot_be_confident_foot(self):
        _, transform = pose.prepare_image(png())
        raw = output()
        raw[0, 0, 45:48] = [0, .5, .9]
        raw[0, 0, 48:51] = [.5, 1.2, .8]
        actual = pose.decode_output(raw, transform)[0]
        self.assertEqual(actual["left_ankle"]["confidence"], 0)
        self.assertEqual(actual["left_ankle"]["y"], 0)
        self.assertEqual(actual["right_ankle"]["confidence"], 0)
        self.assertEqual(actual["right_ankle"]["x"], 639)

    def test_threshold_does_not_invent_people(self):
        _, transform = pose.prepare_image(png())
        self.assertEqual(pose.decode_output(np.zeros((1, 6, 56)), transform), [])
        raw = output()
        raw[0, 1, 55] = .19
        self.assertEqual(len(pose.decode_output(raw, transform)), 1)

    def test_invalid_tensor_and_confidence_refuse(self):
        _, transform = pose.prepare_image(png())
        bad = [np.zeros((1, 17, 3)), np.full((1, 6, 56), np.nan)]
        high = output()
        high[0, 0, 47] = 1.1
        bad.append(high)
        for data in bad:
            with self.subTest(shape=data.shape), self.assertRaises(ValueError):
                pose.decode_output(data, transform)

    def test_result_binds_exact_image_and_model_without_scene_fields(self):
        detector = pose.PoseDetector.__new__(pose.PoseDetector)
        detector.input_size = 384
        detector.session = mock.Mock()
        detector.session.run.return_value = [np.zeros((1, 6, 56))]
        data = png()
        result = detector.infer(data)
        self.assertEqual(result["image_sha256"], hashlib.sha256(data).hexdigest())
        self.assertEqual(result["model_sha256"], pose.METADATA["sha256"])
        self.assertEqual(set(result), {"model_id", "model_version", "model_sha256", "image_sha256",
                                      "image_width", "image_height", "detections"})
        self.assertEqual(result["detections"], [])
        args = detector.session.run.call_args
        self.assertEqual(set(args.args[1]), {"input"})

    def test_stream_reuses_detector_and_rejects_truth_input(self):
        detector = mock.Mock()
        detector.infer_path.return_value = {"detections": []}
        target = io.StringIO()
        pose.serve(detector, io.StringIO('{"image_path":"a.png"}\n{"image_path":"b.png"}\n'), target)
        self.assertEqual(detector.infer_path.call_args_list, [mock.call("a.png"), mock.call("b.png")])
        self.assertEqual(len(target.getvalue().splitlines()), 2)
        detector.reset_mock()
        with self.assertRaises(ValueError):
            pose.serve(detector, io.StringIO('{"image_path":"a.png","actor_position":[1,2,3]}\n'), target)
        detector.infer_path.assert_not_called()

    def test_input_bounds_and_mismatched_model_refuse(self):
        for raw in (b"", b"bad image"):
            with self.assertRaises((ValueError, OSError)):
                pose.prepare_image(raw)
        with self.assertRaises(ValueError):
            pose.prepare_image(png(4097, 1))
        with self.assertRaises(ValueError):
            pose.prepare_image(png(), 257)
        with self.assertRaises(ValueError):
            verify(b"unrelated ONNX bytes")


if __name__ == "__main__":
    unittest.main()
