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
            with patch("huggingface_hub.get_hf_file_metadata", return_value=SimpleNamespace(size=path.stat().st_size)) as metadata, \
                    patch("huggingface_hub.hf_hub_download", return_value=str(path)) as download:
                detector = YOLODetector(config)
                # Cached: loaded from the cache alone, with no request to the Hub.
                download.assert_called_once_with(repo_id="test/yolo", filename="model.onnx", revision="a" * 40,
                                                 token=False, local_files_only=True)
                metadata.assert_not_called()
                result = detector(SimpleNamespace(to_image=lambda: Image.new("RGB", (320, 320))))
                self.assertEqual(result[0]["label"], "person")
                self.assertEqual(result[0]["box"], [110, 135, 210, 185])
                # With no accelerator plan the CPU scores, and says so.
                self.assertEqual(detector.provenance["engine"], "cpu")
                self.assertEqual(detector.provenance["precision"], "fp32")
                self.assertEqual(detector.accelerator_status["active"], "cpu")
                import hashlib
                self.assertEqual(detector.model_sha256, hashlib.sha256(path.read_bytes()).hexdigest())
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

        class Frame(int):
            width, height = 64, 48

        class FakeDecoder:
            def __init__(self, source_id, generation, encoding, initialization=b""):
                self.source_id, self.generation, self.encoding = source_id, generation, encoding
                self.stream = types.SimpleNamespace(stopped=False, feed=lambda payload: True)

            def take(self, now, interval):
                if frame_count is not None and len(taken) >= frame_count:
                    return None
                taken.append(1)
                return Frame(len(taken))

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
            {"type": "prediction", "source_id": "camera", "generation": 1, "detections": [],
             "width": 64, "height": 48},
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

    def test_frame_with_no_engine_yet_is_not_a_failure(self):
        # A tensorrt campaign scores nothing until its engine is ready.
        emitted, _ = self.run_with_detector(lambda frame: None, frame_count=30, wait=1)
        self.assertEqual(emitted, [])

    def test_prediction_names_the_engine(self):
        class Detector:
            provenance = {"engine": "tensorrt", "precision": "fp16", "infer_ms": 5.5}

            def __call__(self, frame):
                return []

        emitted, _ = self.run_with_detector(Detector(), frame_count=1, wait=1)
        self.assertEqual(emitted, [{"type": "prediction", "source_id": "camera", "generation": 1, "detections": [],
                                    "width": 64, "height": 48,
                                    "engine": "tensorrt", "precision": "fp16", "infer_ms": 5.5}])

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


class OfflineStartTests(unittest.TestCase):
    """A pinned model already on the device starts with no network call, and
    one that is missing while the network is down fails with a plain reason."""

    REVISION = "9c5acfdd74aaff2d0f47c51b878506361039a51f"

    def setUp(self):
        import importlib.util
        if any(importlib.util.find_spec(name) is None
               for name in ("numpy", "onnx", "onnxruntime", "huggingface_hub", "httpx")):
            self.skipTest("requires the YOLO ONNX runtime dependencies")
        import socket
        import tempfile
        from unittest import mock
        import huggingface_hub.constants

        directory = tempfile.TemporaryDirectory()
        self.addCleanup(directory.cleanup)
        self.cache = directory.name
        patches = [mock.patch.object(huggingface_hub.constants, "HF_HUB_CACHE", self.cache)]
        # The network is down: every lookup fails the way it does on a device
        # without a route, and is recorded.
        self.lookups = []

        def no_network(host, *args, **kwargs):
            self.lookups.append(host)
            raise socket.gaierror(-3, "Temporary failure in name resolution")

        patches.append(mock.patch("socket.getaddrinfo", no_network))
        for p in patches:
            p.start()
            self.addCleanup(p.stop)
        self.config = {"backend": "yolo_onnx", "model": "webnn/yolo11n", "model_file": "onnx/yolo11n.onnx",
                       "revision": self.REVISION, "labels": ["person"], "threshold": .5}

    def cache_model(self):
        """Lays the model out the way huggingface_hub caches a download."""
        import pathlib
        from onnx import helper, numpy_helper, TensorProto
        import numpy as np
        import onnx

        predictions = np.zeros((1, 5, 1), dtype=np.float32)
        graph = helper.make_graph([
            helper.make_node("Constant", [], ["detections"], value=numpy_helper.from_array(predictions))
        ], "detector", [helper.make_tensor_value_info("images", TensorProto.FLOAT, [1, 3, 640, 640])],
            [helper.make_tensor_value_info("detections", TensorProto.FLOAT, [1, 5, 1])])
        model = helper.make_model(graph, opset_imports=[helper.make_opsetid("", 17)], ir_version=9)
        helper.set_model_props(model, {"names": "{0: 'person'}", "task": "detect"})
        repo = pathlib.Path(self.cache) / "models--webnn--yolo11n"
        blob = repo / "blobs" / ("b" * 64)
        blob.parent.mkdir(parents=True)
        onnx.save(model, blob)
        snapshot = repo / "snapshots" / self.REVISION / "onnx" / "yolo11n.onnx"
        snapshot.parent.mkdir(parents=True)
        snapshot.symlink_to(blob)

    def test_cached_yolo_model_starts_without_the_network(self):
        from worker import YOLODetector
        self.cache_model()
        detector = YOLODetector(self.config)
        self.assertEqual(detector.names, ["person"])
        self.assertEqual(self.lookups, [])

    def test_missing_yolo_model_offline_names_the_reason(self):
        from worker import ModelUnavailable, YOLODetector
        with self.assertRaises(ModelUnavailable) as raised:
            YOLODetector(self.config)
        self.assertTrue(str(raised.exception).startswith(
            "model webnn/yolo11n@9c5acfd is not on this device and the network is unreachable ("),
            str(raised.exception))
        self.assertIn("huggingface.co", self.lookups)

    def test_missing_model_online_is_fetched(self):
        from worker import load_pinned
        calls = []

        def load(local_files_only):
            calls.append(local_files_only)
            if local_files_only:
                raise FileNotFoundError("not cached")
            return "downloaded"

        self.assertEqual(load_pinned(self.config, load), "downloaded")
        self.assertEqual(calls, [True, False])

    def test_closed_client_error_offline_names_the_reason(self):
        # What huggingface_hub 1.5 raises for an offline Transformers load: no
        # transport error in the chain, so the Hub's reachability decides.
        from worker import ModelUnavailable, load_pinned

        def load(local_files_only):
            if local_files_only:
                raise OSError("We couldn't connect to 'https://huggingface.co' to load the files")
            try:
                raise RuntimeError("Cannot send a request, as the client has been closed.")
            except RuntimeError:
                raise OSError("Can't load image processor for 'hustvl/yolos-tiny'")

        with self.assertRaisesRegex(ModelUnavailable, r"^model webnn/yolo11n@9c5acfd is not on this device and "
                                    r"the network is unreachable \(huggingface.co: .*name resolution\)$"):
            load_pinned(self.config, load)

    def assert_not_reworded(self, error):
        """With the Hub reachable, load_pinned raises error unchanged."""
        import socket
        from unittest import mock
        from worker import load_pinned

        def load(local_files_only):
            if local_files_only:
                raise FileNotFoundError("not cached")
            raise error

        with socket.create_server(("127.0.0.1", 0)) as hub, \
                mock.patch("socket.getaddrinfo", return_value=[
                    (socket.AF_INET, socket.SOCK_STREAM, 6, "", hub.getsockname())]) as lookup, \
                self.assertRaises(type(error)) as raised:
            load_pinned(self.config, load)
        self.assertIs(raised.exception, error)
        lookup.assert_called_once_with("huggingface.co", 443, type=socket.SOCK_STREAM)

    def test_errors_while_the_hub_is_reachable_are_not_reworded(self):
        self.assert_not_reworded(RuntimeError("Cannot send a request, as the client has been closed."))

    def test_read_timeout_while_the_hub_is_reachable_is_not_reworded(self):
        # A read timeout comes after a connection was made, so the Hub itself
        # decides whether the network is down.
        import httpx
        self.assert_not_reworded(httpx.ReadTimeout("The read operation timed out"))

    def probe_hub(self, reachable):
        """Runs hub_unreachable against huggingface.co's dozen addresses, IPv6
        first as DNS orders them. A connection to an address whose family is
        not in reachable hangs until its timeout. Returns the reason and the
        (address, timeout) of each connection attempt."""
        import httpx  # imported here, before socket.socket is replaced
        import socket
        from unittest import mock
        from worker import hub_unreachable

        attempts = []

        class Connection:
            def __init__(self, family, kind, protocol):
                self.family = family

            def __enter__(self):
                return self

            def __exit__(self, *exc):
                return False

            def settimeout(self, timeout):
                self.timeout = timeout

            def connect(self, address):
                attempts.append((address[0], self.timeout))
                if self.family not in reachable:
                    raise TimeoutError("timed out")

        addresses = [(socket.AF_INET6, socket.SOCK_STREAM, 6, "", (f"2001:db8::{i}", 443, 0, 0)) for i in range(8)]
        addresses += [(socket.AF_INET, socket.SOCK_STREAM, 6, "", (f"192.0.2.{i}", 443)) for i in range(4)]
        with mock.patch("socket.getaddrinfo", return_value=addresses), mock.patch("socket.socket", Connection):
            reason = hub_unreachable(RuntimeError("Cannot send a request, as the client has been closed."))
        return reason, attempts

    def test_hub_probe_tries_one_address_per_family(self):
        import socket
        reason, attempts = self.probe_hub(reachable=())
        self.assertEqual(reason, "huggingface.co: timed out")
        self.assertEqual(attempts, [("192.0.2.0", 5), ("2001:db8::0", 5)])
        # A broken IPv6 route does not hide a working IPv4 one.
        reason, attempts = self.probe_hub(reachable=(socket.AF_INET,))
        self.assertIsNone(reason)
        self.assertEqual(attempts, [("192.0.2.0", 5)])
        # Nor does a missing IPv4 route hide a working IPv6 one.
        reason, attempts = self.probe_hub(reachable=(socket.AF_INET6,))
        self.assertIsNone(reason)
        self.assertEqual(attempts, [("192.0.2.0", 5), ("2001:db8::0", 5)])

    def test_model_that_does_not_load_from_the_device_names_the_local_error(self):
        import pathlib
        from worker import ModelUnavailable, load_pinned
        snapshot = pathlib.Path(self.cache) / "models--webnn--yolo11n" / "snapshots" / self.REVISION
        snapshot.mkdir(parents=True)

        def load(local_files_only):
            raise PermissionError(13, "Permission denied", str(snapshot / "onnx" / "yolo11n.onnx"))

        with self.assertRaises(ModelUnavailable) as raised:
            load_pinned(self.config, load)
        self.assertRegex(str(raised.exception), r"^model webnn/yolo11n@9c5acfd did not load from this device "
                         r"\(PermissionError: \[Errno 13\] Permission denied: '.*yolo11n.onnx'\) and the "
                         r"network is unreachable \(huggingface.co: .*name resolution\)$")

    def test_cached_transformers_model_starts_without_the_network(self):
        import importlib.util
        if any(importlib.util.find_spec(name) is None for name in ("torch", "transformers", "PIL")):
            self.skipTest("requires the Transformers runtime dependencies")
        import pathlib
        from transformers import YolosConfig, YolosForObjectDetection, YolosImageProcessor
        from worker import Detector

        # A tiny model cached at the pinned commit. Like hustvl/yolos-tiny, it
        # has no processor_config.json, which an online load asks the Hub about.
        snapshot = pathlib.Path(self.cache) / "models--test--yolos" / "snapshots" / self.REVISION
        YolosForObjectDetection(YolosConfig(
            hidden_size=32, num_hidden_layers=1, num_attention_heads=2, intermediate_size=37, image_size=[64, 64],
            num_detection_tokens=2, id2label={0: "person"}, label2id={"person": 0})).save_pretrained(snapshot)
        YolosImageProcessor().save_pretrained(snapshot)
        detector = Detector(dict(self.config, backend="transformers", model="test/yolos"))
        self.assertEqual(detector.model.config.id2label, {0: "person"})
        self.assertEqual(self.lookups, [])

    def test_unpinned_revision_keeps_asking_the_hub(self):
        from worker import load_pinned
        calls = []
        load_pinned(dict(self.config, revision="main"), lambda local: calls.append(local))
        self.assertEqual(calls, [False])

    def test_ready_reports_the_planned_engine(self):
        import json
        import types
        from unittest import mock
        import worker

        self.cache_model()
        plan = {"requested": "auto", "candidates": ["cpu"],
                "reason": "no supported accelerator: TensorRT needs an NVIDIA GPU with CUDA"}
        lines = [json.dumps(dict(self.config, accelerator_plan=plan)).encode()]
        stdin = types.SimpleNamespace(buffer=types.SimpleNamespace(
            readline=lambda *limit: lines.pop(0) if lines else b""))
        emitted = []
        with mock.patch("worker.emit", emitted.append), mock.patch("worker.sys.stdin", stdin), \
                mock.patch("accelerator.claim_stdout", lambda: None):
            worker.main()
        ready = emitted[0]
        self.assertEqual(ready["type"], "ready")
        self.assertEqual(ready["accelerator"]["active"], "cpu")
        self.assertEqual(ready["accelerator"]["requested"], "auto")
        self.assertFalse(ready["accelerator"]["degraded"])
        self.assertEqual(ready["accelerator"]["reason"], plan["reason"])
        self.assertEqual(len(ready["accelerator"]["model_sha256"]), 64)

    def test_worker_reports_the_reason_to_the_agent(self):
        import json
        import types
        from unittest import mock
        import worker

        emitted = []
        stdin = types.SimpleNamespace(buffer=types.SimpleNamespace(
            readline=lambda: json.dumps(self.config).encode()))
        with mock.patch("worker.emit", emitted.append), mock.patch("worker.sys.stdin", stdin), \
                mock.patch("accelerator.claim_stdout", lambda: None), self.assertRaises(SystemExit) as exited:
            worker.main()
        self.assertEqual(exited.exception.code, 1)
        self.assertEqual(len(emitted), 1)
        self.assertEqual(emitted[0]["type"], "error")
        self.assertIn("is not on this device and the network is unreachable", emitted[0]["error"])


class ProtocolChannelTests(unittest.TestCase):
    """The agent ends a worker on any stdout line that is not JSON. TensorRT
    printed a warning to stdout while loading a cached engine after a reboot,
    and the worker was killed every few seconds. Native code writing to fd 1,
    before or after ready, must reach stderr and leave the protocol intact."""

    # The real worker.main(), with a detector and an accelerator that write
    # to fd 1 the ways native code and stray Python do.
    SCRIPT = r'''
import ctypes, os, sys
sys.path.insert(0, sys.argv[1])
import worker

def noise(when):
    os.write(1, b"[TRT] [W] write(2) to fd 1 " + when + b"\n")
    libc = ctypes.CDLL(None)
    libc.puts(b"[TRT] [W] C stdio to stdout " + when)
    libc.fflush(None)
    print("python print " + when.decode())

class NoisyAccelerator:
    def start(self):
        # Where TensorRT loads a cached engine: after ready, in the background.
        noise(b"after ready")
        worker.emit({"type": "accelerator", "accelerator": {"active": "tensorrt"}})

    def stop(self):
        pass

class NoisyDetector:
    def __init__(self, config):
        noise(b"before ready")
        self.accelerator_status = {"active": "cpu"}
        self.accelerator = NoisyAccelerator()

worker.YOLODetector = NoisyDetector
worker.main()
'''

    def test_native_output_on_stdout_goes_to_stderr(self):
        import json
        import os
        import subprocess
        import sys
        here = os.path.dirname(os.path.abspath(__file__))
        config = {"backend": "yolo_onnx", "rate": 2}
        # With stdin closed after the config line, run() returns and the worker exits.
        worker = subprocess.run([sys.executable, "-u", "-c", self.SCRIPT, here],
                                input=json.dumps(config).encode() + b"\n",
                                capture_output=True, timeout=60)
        self.assertEqual(worker.returncode, 0, worker.stderr.decode(errors="replace"))
        lines = worker.stdout.decode().splitlines()
        # Every line is protocol JSON, in order, and nothing else.
        self.assertEqual([json.loads(line)["type"] for line in lines], ["ready", "accelerator"], lines)
        stderr = worker.stderr.decode()
        for when in ("before ready", "after ready"):
            self.assertIn("[TRT] [W] write(2) to fd 1 " + when, stderr)
            self.assertIn("[TRT] [W] C stdio to stdout " + when, stderr)
            self.assertIn("python print " + when, stderr)

    # The real worker.main() and the real accelerator, which imports TensorRT
    # only when an engine is about to load, after ready. The TensorRT it
    # imports is a package that writes to fd 1 while it loads, as native code
    # does; the GPU steps around it are scripted.
    LAZY_SCRIPT = r'''
import sys
sys.path.insert(0, sys.argv[1])
import numpy as np
import accelerator, worker

class Engine:
    def __init__(self, name, precision):
        self.name, self.precision, self.runtime = name, precision, name

    def infer(self, tensor):
        return np.zeros((1, 6, 8), np.float32)

    def device_bytes(self):
        return 0

    def close(self):
        pass

class Cuda:
    def reset(self):
        pass

class Accelerator(accelerator.Accelerator):
    def probe(self):
        return {"tensorrt": "10.16.2.10", "cuda": "13.2", "gpu_arch": "sm_87"}

    def build_delay(self):
        return 0

    def available_mib(self):
        return None

    def idle_cpus(self):
        return None

    def run_builder(self, job, timeout):
        accelerator.EngineCache(job["engine_dir"]).store(job["key"], job["fields"], b"plan", 1)
        return "done", ""

    def open_cuda(self):
        return Cuda()

    def load_engine(self, trt, cuda, plan):
        return Engine("tensorrt", "fp16")

    def start(self):
        # In the foreground, so that the worker cannot exit before it loads.
        assert "tensorrt" not in sys.modules
        self.accelerate()
        assert "tensorrt" in sys.modules

class Detector:
    def __init__(self, config):
        plan = {"requested": "auto", "candidates": ["tensorrt", "cpu"],
                "tensorrt": {"library": "10.16.2", "bindings": sys.argv[2], "engine_dir": sys.argv[3],
                             "cuda_runtime": "libcudart.so.13", "workspace_mib": 1024,
                             "build_min_available_mib": 3072, "gpu_budget_mib": 1536}}
        model = {"path": "model.onnx", "sha256": "a" * 64, "input_shape": [1, 3, 4, 4],
                 "output_shape": [1, 6, 8], "threshold": 0.5, "decode": lambda output: []}
        self.accelerator = Accelerator(plan, Engine("cpu", "fp32"), model, worker.emit)
        # A camera frame to compare the engines on.
        self.accelerator.infer(np.zeros((1, 3, 4, 4), np.float32))
        self.accelerator_status = self.accelerator.status()

worker.YOLODetector = Detector
worker.main()
'''

    NOISY_TENSORRT = r'''
import ctypes, os
__version__ = "10.16.2.10"
os.write(1, b"[TRT] [W] write(2) to fd 1 while importing\n")
libc = ctypes.CDLL(None)
libc.puts(b"[TRT] [W] C stdio to stdout while importing")
libc.fflush(None)
print("python print while importing")
'''

    def test_tensorrt_loaded_after_ready_writes_to_stderr(self):
        import json
        import os
        import pathlib
        import subprocess
        import sys
        import tempfile
        here = os.path.dirname(os.path.abspath(__file__))
        with tempfile.TemporaryDirectory() as directory:
            package = pathlib.Path(directory) / "bindings" / "tensorrt"
            package.mkdir(parents=True)
            (package / "__init__.py").write_text(self.NOISY_TENSORRT)
            config = {"backend": "yolo_onnx", "rate": 2}
            worker = subprocess.run([sys.executable, "-u", "-c", self.LAZY_SCRIPT, here,
                                     str(package.parent), os.path.join(directory, "engines")],
                                    input=json.dumps(config).encode() + b"\n", capture_output=True, timeout=60)
        self.assertEqual(worker.returncode, 0, worker.stderr.decode(errors="replace"))
        messages = [json.loads(line) for line in worker.stdout.decode().splitlines()]
        self.assertEqual(messages[0]["type"], "ready")
        self.assertEqual({message["type"] for message in messages[1:]}, {"accelerator"})
        self.assertEqual(messages[-1]["accelerator"]["active"], "tensorrt")
        stderr = worker.stderr.decode()
        self.assertIn("[TRT] [W] write(2) to fd 1 while importing", stderr)
        self.assertIn("[TRT] [W] C stdio to stdout while importing", stderr)
        self.assertIn("python print while importing", stderr)


if __name__ == "__main__":
    unittest.main()
