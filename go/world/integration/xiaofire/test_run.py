"""Receipt-tail checks use real append-only files without starting a runtime."""
import json
import hashlib
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import os
from pathlib import Path
import tempfile
import threading
import unittest
from unittest.mock import patch

from integration.xiaofire.run import FrameTail, load_stimulus_calibration, request


class CalibrationArtifactTests(unittest.TestCase):
    def test_original_bytes_are_verified_before_run_identity_changes(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            original = b'{ "run_id": "render-fixture", "fx": 400 }\n'
            (root / "calibration.json").write_bytes(original)
            manifest = {"calibration_sha256": hashlib.sha256(original).hexdigest()}
            calibration = load_stimulus_calibration(root, manifest)
            self.assertEqual(calibration["run_id"], "render-fixture")
            calibration["run_id"] = "new-runtime"
            self.assertEqual((root / "calibration.json").read_bytes(), original)
            # Even a semantically identical reserialization is a changed artifact.
            (root / "calibration.json").write_text(json.dumps(json.loads(original)))
            with self.assertRaisesRegex(ValueError, "optical calibration changed"):
                load_stimulus_calibration(root, manifest)

    def test_missing_or_wrong_digest_rejects_before_parsing(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / "calibration.json").write_bytes(b'not valid JSON')
            for manifest in ({}, {"calibration_sha256": "0"*64}, {"calibration_sha256": None}):
                with self.subTest(manifest=manifest), self.assertRaisesRegex(ValueError, "optical calibration changed"):
                    load_stimulus_calibration(root, manifest)


class LocalRequestTests(unittest.TestCase):
    def test_local_request_ignores_environment_proxy_without_losing_auth_or_body(self):
        received = []

        class Handler(BaseHTTPRequestHandler):
            def do_POST(self):
                received.append((self.path, self.headers.get("Authorization"),
                                 json.loads(self.rfile.read(int(self.headers["Content-Length"])))))
                body = b'{"accepted":true}'
                self.send_response(200)
                self.send_header("Content-Type", "application/json")
                self.send_header("Content-Length", str(len(body)))
                self.end_headers()
                self.wfile.write(body)

            def log_message(self, *args):
                pass

        with ThreadingHTTPServer(("127.0.0.1", 0), Handler) as server:
            worker = threading.Thread(target=server.serve_forever, kwargs={"poll_interval": .01})
            worker.start()
            try:
                # Port 0 cannot be a reachable proxy. Force bypass off as well
                # so a platform's implicit localhost exception cannot hide a bug.
                with patch.dict(os.environ, {"http_proxy": "http://127.0.0.1:0",
                                             "HTTP_PROXY": "http://127.0.0.1:0",
                                             "no_proxy": "", "NO_PROXY": ""}), \
                     patch("urllib.request.proxy_bypass", return_value=False):
                    result = request(f"http://127.0.0.1:{server.server_port}", "/capture",
                                     {"run_id": "private-test"}, token="test-token", timeout=2)
                self.assertEqual(result, {"accepted": True})
                self.assertEqual(received, [("/capture", "Bearer test-token", {"run_id": "private-test"})])
            finally:
                server.shutdown()
                worker.join(timeout=2)


class FrameTailTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name).resolve()
        self.capture = self.root / "capture-session-1"
        self.capture.mkdir()
        (self.capture / "frames").mkdir()
        self.log = self.capture / "frames.jsonl"
        self.log.touch()
        self.tail = FrameTail(self.root)
        self.wall_ns = 100_000_000_000
        self.monotonic_s = 500.
        wall = patch("integration.xiaofire.run.time.time_ns", lambda: self.wall_ns)
        monotonic = patch("integration.xiaofire.run.time.monotonic", lambda: self.monotonic_s)
        wall.start(); monotonic.start()
        self.addCleanup(wall.stop)
        self.addCleanup(monotonic.stop)

    def row(self, sequence=1, age_ns=0):
        return {"session_id": "session-1", "frame_seq": sequence,
                "timestamp_us": 900_000 + sequence * 200_000,
                "jpeg_path": "frames/frame-%08d.jpg" % sequence,
                "received_unix_ns": self.wall_ns-age_ns}

    def append(self, row):
        with self.log.open("ab") as stream:
            stream.write(json.dumps(row).encode() + b"\n")

    def test_stale_queued_frames_are_skipped_without_renewing_receipt(self):
        self.append(self.row(1, 9_000_000_000))
        self.append(self.row(2, 2_000_000_000))
        fresh = self.row(3, 1_500_000_000)
        self.append(fresh)
        rows = self.tail.read()
        self.assertEqual(len(rows), 1)
        original, image, receipt = rows[0]
        self.assertEqual(original, fresh)
        self.assertEqual(image, self.capture / fresh["jpeg_path"])
        self.assertEqual(receipt, 498.5)
        # Polling again must neither replay the valid row nor make old rows new.
        self.wall_ns += 500_000_000
        self.monotonic_s += .5
        self.assertEqual(self.tail.read(), [])

    def test_host_timestamp_is_required_and_firmware_time_is_not_a_substitute(self):
        row = self.row()
        del row["received_unix_ns"]
        self.append(row)
        with self.assertRaisesRegex(ValueError, "host receipt timestamp"):
            self.tail.read()

    def test_host_timestamp_rejects_non_integer_values(self):
        for stamp in (True, 100_000_000_000., "100000000000", None):
            with self.subTest(stamp=stamp):
                row = self.row()
                row["received_unix_ns"] = stamp
                self.log.write_text(json.dumps(row)+"\n")
                with self.assertRaisesRegex(ValueError, "host receipt timestamp"):
                    FrameTail(self.root).read()

    def test_future_host_receipt_is_not_admitted(self):
        self.append(self.row(1, -1))
        self.append(self.row(2, 0))
        rows = self.tail.read()
        self.assertEqual([row[0]["frame_seq"] for row in rows], [2])
        self.assertEqual(rows[0][2], 500.)

    def test_partial_json_line_resumes_once_after_writer_finishes(self):
        self.append(self.row(1))
        second = self.row(2, 250_000_000)
        encoded = json.dumps(second).encode()+b"\n"
        cut = len(encoded)//2
        with self.log.open("ab") as stream:
            stream.write(encoded[:cut])
        self.assertEqual([row[0]["frame_seq"] for row in self.tail.read()], [1])
        self.assertEqual(self.tail.read(), [])
        self.wall_ns += 500_000_000
        self.monotonic_s += .5
        with self.log.open("ab") as stream:
            stream.write(encoded[cut:])
        rows = self.tail.read()
        self.assertEqual([row[0]["frame_seq"] for row in rows], [2])
        self.assertEqual(rows[0][2], 499.75)
        self.assertEqual(self.tail.read(), [])

    def test_path_escape_is_rejected(self):
        for escaped in ("../outside.jpg", str(self.root/"outside.jpg")):
            with self.subTest(path=escaped):
                row = self.row()
                row["jpeg_path"] = escaped
                self.log.write_text(json.dumps(row)+"\n")
                with self.assertRaisesRegex(ValueError, "path escaped"):
                    FrameTail(self.root).read()

    def test_symlink_escape_is_rejected(self):
        outside = self.root/"outside.jpg"
        outside.write_bytes(b"not read by the tail")
        (self.capture/"frames/escaped.jpg").symlink_to(outside)
        row = self.row()
        row["jpeg_path"] = "frames/escaped.jpg"
        self.append(row)
        with self.assertRaisesRegex(ValueError, "path escaped"):
            self.tail.read()


if __name__ == "__main__":
    unittest.main()
