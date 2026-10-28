import threading
import time
import unittest

from worker import StreamBytes


class StreamTests(unittest.TestCase):
    def test_byte_order_across_camera_chunks(self):
        stream = StreamBytes()
        stream.feed(b"abc")
        stream.feed(b"def")
        self.assertEqual(stream.read(2), b"ab")
        self.assertEqual(stream.read(100), b"c")
        self.assertEqual(stream.read(3), b"def")
        self.assertEqual(stream.pending, 0)
        self.assertEqual(stream.read(0), b"")

    def test_bounded_encoded_queue(self):
        stream = StreamBytes()
        self.assertTrue(stream.feed(b"x" * (8 << 20)))
        self.assertFalse(stream.feed(b"y"))
        stream.stop()
        self.assertEqual(stream.pending, 0)
        self.assertFalse(stream.feed(b"z"))

    def test_stop_unblocks_libav_read(self):
        stream = StreamBytes()
        result = []
        reader = threading.Thread(target=lambda: result.append(stream.read(4096)))
        reader.start()
        stream.stop()
        reader.join(timeout=1)
        self.assertFalse(reader.is_alive())
        self.assertEqual(result, [b""])


class YOLOTests(unittest.TestCase):
    def setUp(self):
        try:
            import numpy as np
        except ImportError:
            self.skipTest("requires numpy")
        self.np = np

    def test_letterbox_coordinates_class_aware_nms_and_filter(self):
        from worker import yolo_detections
        # A 640x320 frame was letterboxed to 640x640, with 160px top padding.
        rows = self.np.array([
            [100, 240, 40, 80, .9, .1],
            [102, 240, 40, 80, .8, .1],  # Same person, suppressed.
            [100, 240, 40, 80, .1, .95],  # Different class must survive NMS.
            [600, 300, 20, 20, .2, .1],  # Below threshold.
        ], dtype=self.np.float32)
        result = yolo_detections(rows.T[None], ["person", "dog"], {"person", "dog"},
                                 .5, 1, 0, 160, 640, 320)
        self.assertEqual([r["label"] for r in result], ["dog", "person"])
        self.assertEqual(result[1]["box"], [80, 40, 120, 120])
        result = yolo_detections(rows.T[None], ["person", "dog"], {"person"},
                                 .5, 1, 0, 160, 640, 320)
        self.assertEqual(len(result), 1)
        self.assertEqual(result[0]["label"], "person")

    def test_invalid_outputs_rejected(self):
        from worker import yolo_detections
        for output in (self.np.zeros((1, 300, 6)), self.np.full((1, 5, 10), self.np.nan)):
            with self.assertRaises(ValueError):
                yolo_detections(output, ["person"], {"person"}, .5, 1, 0, 0, 640, 640)

    def test_real_onnx_session_download_and_frame(self):
        import importlib.util
        import pathlib
        import tempfile
        from types import SimpleNamespace
        from unittest.mock import patch
        if any(importlib.util.find_spec(name) is None for name in ("onnx", "onnxruntime", "huggingface_hub", "PIL")):
            self.skipTest("requires ONNX fixture dependencies")
        import onnx
        from onnx import helper, numpy_helper, TensorProto
        from PIL import Image
        from worker import YOLODetector

        predictions = self.np.array([[[320], [320], [200], [100], [.9]]], dtype=self.np.float32)
        graph = helper.make_graph([
            helper.make_node("Constant", [], ["detections"], value=numpy_helper.from_array(predictions))
        ], "detector", [helper.make_tensor_value_info("images", TensorProto.FLOAT, [1, 3, 640, 640])],
            [helper.make_tensor_value_info("detections", TensorProto.FLOAT, [1, 5, 1])])
        model = helper.make_model(graph, opset_imports=[helper.make_opsetid("", 17)], ir_version=9)
        helper.set_model_props(model, {"names": "{0: 'person'}", "task": "detect"})
        config = {"model": "test/yolo", "model_file": "model.onnx", "revision": "a" * 40,
                  "labels": ["person"], "threshold": .5}
        with tempfile.TemporaryDirectory() as directory:
            path = pathlib.Path(directory) / "model.onnx"
            onnx.save(model, path)
            with patch("huggingface_hub.get_hf_file_metadata", return_value=SimpleNamespace(size=path.stat().st_size)), \
                    patch("huggingface_hub.hf_hub_download", return_value=str(path)) as download:
                detector = YOLODetector(config)
                download.assert_called_once_with(repo_id="test/yolo", filename="model.onnx", revision="a" * 40, token=False)
                result = detector(SimpleNamespace(to_image=lambda: Image.new("RGB", (320, 320))))
                self.assertEqual(result[0]["label"], "person")
                self.assertEqual(result[0]["box"], [110, 135, 210, 185])
                # A valid external tensor is present on disk, but the runtime
                # must not follow model-supplied filesystem references.
                onnx.save_model(model, path, save_as_external_data=True,
                                all_tensors_to_one_file=True, location="weights.bin",
                                size_threshold=0, convert_attribute=True)
                self.assertTrue((path.parent / "weights.bin").is_file())
                with self.assertRaisesRegex(ValueError, "external tensor files"):
                    YOLODetector(config)
                external = onnx.load(path, load_external_data=False)
                tensor = external.graph.node[0].attribute[0].t
                for entry in tensor.external_data:
                    if entry.key == "location":
                        entry.value = str(path.parent / "weights.bin")
                path.write_bytes(external.SerializeToString())
                with self.assertRaisesRegex(ValueError, "external tensor files"):
                    YOLODetector(config)


class WebMLateJoinTests(unittest.TestCase):
    def test_cached_initialization_decodes_late_chunks_and_resets(self):
        import importlib.util
        import pathlib
        import shutil
        import subprocess
        import tempfile
        import time
        from worker import Decoder

        if importlib.util.find_spec("av") is None or shutil.which("ffmpeg") is None:
            self.skipTest("requires PyAV and ffmpeg for real WebM decoding")
        with tempfile.TemporaryDirectory() as directory:
            path = pathlib.Path(directory) / "stream.webm"
            subprocess.run([
                "ffmpeg", "-v", "error", "-f", "lavfi", "-i",
                "testsrc2=size=96x64:rate=10", "-t", "12", "-c:v", "libvpx",
                "-g", "10", "-f", "webm", "-live", "1", str(path),
            ], check=True)
            data = path.read_bytes()
        header = data[:data.index(bytes.fromhex("1f43b675"))]
        # Neither join is aligned to a container header. A new decoder models
        # both a late subscriber and a reset after an overflowing input queue.
        for generation, offset in enumerate((len(data) // 3 + 19, len(data) // 2 + 17), 1):
            decoder = Decoder("camera", generation, "vp8", header)
            try:
                self.assertTrue(decoder.stream.feed(data[offset:]))
                deadline = time.monotonic() + 5
                while time.monotonic() < deadline:
                    with decoder.lock:
                        frame = decoder.latest
                    if frame is not None:
                        break
                    time.sleep(0.01)
                self.assertIsNotNone(frame, "late WebM subscriber never decoded")
                self.assertEqual((frame[1].width, frame[1].height), (96, 64))
            finally:
                decoder.stop()


class DetectorFailureTests(unittest.TestCase):
    def run_with_detector(self, detector, frame_count=None, wait=30):
        """Runs worker.run over one camera that always has a frame ready and
        returns what it emitted and how long run took."""
        import json
        import types
        from unittest import mock
        from worker import run

        emitted = []
        finished = threading.Event()
        taken = []

        class FakeDecoder:
            def __init__(self, source_id, generation, encoding, initialization=b""):
                self.source_id, self.generation, self.encoding = source_id, generation, encoding
                self.stream = types.SimpleNamespace(stopped=False, feed=lambda payload: True)

            def take(self, now, interval):
                if frame_count is not None and len(taken) >= frame_count:
                    return None
                taken.append(1)
                return len(taken)

            def stop(self):
                self.stream.stopped = True

        class Input:
            lines = [json.dumps({"source_id": "camera", "generation": 1, "encoding": "h264",
                                 "payload": "eA=="}).encode() + b"\n"]

            def readline(self, limit):
                if self.lines:
                    return self.lines.pop(0)
                finished.wait(wait)
                return b""

        def emit(result):
            emitted.append(result)

        started = time.monotonic()
        with mock.patch("worker.Decoder", FakeDecoder), \
                mock.patch("worker.emit", emit), \
                mock.patch("worker.sys.stdin", types.SimpleNamespace(buffer=Input())):
            run({"rate": 1000}, detector)
        finished.set()
        return emitted, time.monotonic() - started

    def test_failing_frame_reports_source_error_and_next_frame_is_scored(self):
        def detector(frame):
            if frame == 1:
                raise ValueError("bad frame")
            return []

        emitted, _ = self.run_with_detector(detector, frame_count=2, wait=1)
        self.assertEqual(emitted, [
            {"type": "source_error", "source_id": "camera", "generation": 1,
             "error": "inference: ValueError: bad frame"},
            {"type": "prediction", "source_id": "camera", "generation": 1, "detections": []},
        ])

    def test_detector_that_always_fails_ends_the_worker_with_an_error(self):
        def detector(frame):
            raise ValueError("broken")

        emitted, elapsed = self.run_with_detector(detector)
        self.assertLess(elapsed, 10)
        self.assertEqual([item["type"] for item in emitted], ["source_error"] * 9 + ["error"])
        self.assertEqual(emitted[-1], {"type": "error", "error": "inference: ValueError: broken"})

    def test_mostly_failing_detector_ends_the_worker(self):
        # Nine failures, one success, then failures: a straight count of
        # consecutive failures would reset at the success and never trip.
        def detector(frame):
            if frame == 10:
                return []
            raise ValueError("broken")

        emitted, elapsed = self.run_with_detector(detector)
        self.assertLess(elapsed, 10)
        self.assertEqual([item["type"] for item in emitted],
                         ["source_error"] * 9 + ["prediction", "error"])

    def test_occasional_failures_keep_the_worker_running(self):
        # One frame in four fails, over 40 frames: 10 failures in all, but
        # never 10 within any 20 frames.
        def detector(frame):
            if frame % 4 == 0:
                raise ValueError("flaky")
            return []

        # The loop sleeps 20 ms per pass, so 40 frames need ~0.8 s before
        # stdin closes.
        emitted, _ = self.run_with_detector(detector, frame_count=40, wait=5)
        types_seen = [item["type"] for item in emitted]
        self.assertNotIn("error", types_seen)
        self.assertEqual(types_seen.count("source_error"), 10)
        self.assertEqual(types_seen.count("prediction"), 30)

    def test_detection_error_never_raises(self):
        from worker import detection_error

        class Unprintable(Exception):
            def __str__(self):
                raise RuntimeError("no")

        self.assertEqual(detection_error(Unprintable()), "inference: Unprintable")
        message = detection_error(ValueError("\udc80" * 400))
        self.assertLessEqual(len(message.encode()), 512)
        self.assertTrue(message.startswith("inference: ValueError: "))

    def test_detection_error_is_cut_to_512_utf8_bytes(self):
        from worker import detection_error
        message = detection_error(RuntimeError("é" * 600))
        self.assertLessEqual(len(message.encode()), 512)
        self.assertTrue(message.startswith("inference: RuntimeError: é"))
        self.assertEqual(detection_error(KeyError()), "inference: KeyError")


if __name__ == "__main__":
    unittest.main()

class ClassificationTests(unittest.TestCase):
    def test_whole_image_labels_threshold_and_no_boxes(self):
        from worker import classification_predictions
        result = classification_predictions([.7, .95, .2, float('nan')],
                                            {0: 'cat, tabby', 1: 'dog', 2: 'bird', 3: 'bad'},
                                            {'cat, tabby', 'dog', 'bird', 'bad'}, .5)
        self.assertEqual(result, [{'label': 'dog', 'score': .95}, {'label': 'cat, tabby', 'score': .7}])
        self.assertTrue(all('box' not in item for item in result))

    def test_classifier_uses_safe_loader_and_probability_output(self):
        import contextlib
        import types
        from unittest.mock import MagicMock, patch
        from worker import Detector
        model = MagicMock()
        model.eval.return_value = model
        model.config = types.SimpleNamespace(id2label={0: 'cat', 1: 'dog'}, problem_type=None)
        model.return_value.logits.__getitem__.return_value.softmax.return_value.tolist.return_value = [.9, .1]
        processor = MagicMock(return_value={'pixels': object()})
        transformers = types.SimpleNamespace(
            AutoConfig=MagicMock(), AutoImageProcessor=MagicMock(),
            AutoModelForObjectDetection=MagicMock(), AutoModelForImageClassification=MagicMock())
        transformers.AutoConfig.from_pretrained.return_value = model.config
        transformers.AutoImageProcessor.from_pretrained.return_value = processor
        transformers.AutoModelForImageClassification.from_pretrained.return_value = model
        torch = types.SimpleNamespace(inference_mode=contextlib.nullcontext)
        with patch.dict('sys.modules', {'torch': torch, 'transformers': transformers}):
            detector = Detector({'task': 'image-classification', 'model': 'google/test',
                                 'revision': 'a' * 40, 'labels': ['cat'], 'threshold': .5})
            self.assertEqual(detector(MagicMock()), [{'label': 'cat', 'score': .9}])
        transformers.AutoModelForObjectDetection.from_pretrained.assert_not_called()
        args = transformers.AutoModelForImageClassification.from_pretrained.call_args.kwargs
        self.assertEqual(args['revision'], 'a' * 40)
        self.assertTrue(args['use_safetensors'])
        self.assertFalse(args['trust_remote_code'])
