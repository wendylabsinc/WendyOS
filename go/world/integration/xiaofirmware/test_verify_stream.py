"""Generated fixtures test the verifier only, never emulator readiness."""
import hashlib
import json
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest

from PIL import Image

import verify_stream as check


class StreamVerificationTests(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        self.root = Path(self.directory.name)
        for name in ["firmware.bin", "emulator", "stimulus.rgb"]:
            (self.root / name).write_bytes(b"verifier-only fixture: " + name.encode())
        Image.new("RGB", (32, 24), (15, 180, 90)).save(self.root / "frame.jpg", "JPEG")
        self.manifest = {
            "schema": check.SCHEMA, "origin": "simulated", "run_id": "verifier-unit-test",
            "firmware": self.reference("firmware.bin"),
            "emulator": dict(self.reference("emulator"), version="verifier-fixture"),
            "stimuli": [self.reference("stimulus.rgb")],
            "channels": [{"channel_id": 0, "width": 32, "height": 24}],
            "frames_path": "frames.jsonl", "capture_complete": True,
        }

    def reference(self, name):
        return {"path": name, "sha256": hashlib.sha256((self.root / name).read_bytes()).hexdigest()}

    def frame(self, sequence=1, timestamp=100, session="session-a"):
        return {"session_id": session, "channel_id": 0, "frame_seq": sequence,
                "timestamp_us": timestamp, "jpeg_path": "frame.jpg",
                "sha256": self.reference("frame.jpg")["sha256"], "width": 32, "height": 24}

    def capture(self, frames):
        path = self.root / "capture.json"
        path.write_text(json.dumps(self.manifest))
        (self.root / "frames.jsonl").write_text("".join(json.dumps(x) + "\n" for x in frames))
        return path

    def test_complete_decode_channel_zero_and_no_execution_claim(self):
        report = check.verify(self.capture([self.frame(), self.frame(2, 200)]))
        self.assertTrue(report["stream_integrity_verified"])
        self.assertFalse(report["firmware_execution_verified"])
        self.assertEqual(report["decoded_frames"], 2)
        self.assertEqual(report["distinct_jpegs"], 1)  # Static scenes are legitimate.

    def test_sequence_wrap_gap_and_explicit_reboot_session(self):
        report = check.verify(self.capture([
            self.frame(2**32 - 1, 100), self.frame(0, 200), self.frame(3, 300),
            self.frame(0, 0, "reboot-b"), self.frame(1, 10, "reboot-b"),
        ]))
        self.assertTrue(report["stream_integrity_verified"])
        self.assertEqual(report["missing_sequence_frames"], 2)
        self.assertEqual(report["sessions"], ["reboot-b", "session-a"])

    def test_duplicate_out_of_order_and_clock_regression_fail(self):
        report = check.verify(self.capture([
            self.frame(4, 400), self.frame(4, 401), self.frame(3, 402),
            self.frame(5, 399), self.frame(5, 500),
        ]))
        self.assertFalse(report["stream_integrity_verified"])
        self.assertEqual(report["decoded_frames"], 2)
        self.assertEqual(report["duplicate_sequence_frames"], 1)
        self.assertEqual(report["out_of_order_frames"], 1)
        self.assertEqual(report["nonadvancing_timestamps"], 1)
        self.assertEqual(report["missing_sequence_frames"], 0)

    def test_truncated_and_corrupt_jpegs_fail_with_recomputed_hashes(self):
        original = (self.root / "frame.jpg").read_bytes()
        for data in [original[:-2], original[:50] + b"\xff\xd9"]:
            with self.subTest(length=len(data)):
                (self.root / "frame.jpg").write_bytes(data)
                report = check.verify(self.capture([self.frame()]))
                self.assertFalse(report["stream_integrity_verified"])
                self.assertEqual(report["decoded_frames"], 0)

    def test_declared_hash_and_dimensions_do_not_replace_decoding(self):
        frame = self.frame()
        frame["sha256"] = "0" * 64
        report = check.verify(self.capture([frame]))
        self.assertIn("digest mismatch", report["rejected_frames"][0]["reason"])
        self.manifest["channels"][0]["width"] = 64
        frame = self.frame()
        frame["width"] = 64
        report = check.verify(self.capture([frame]))
        self.assertIn("decoded JPEG dimensions", report["rejected_frames"][0]["reason"])

    def test_artifact_hash_mismatch_and_measured_origin_refused(self):
        self.manifest["firmware"]["sha256"] = "0" * 64
        with self.assertRaisesRegex(check.InvalidCapture, "artifact digest mismatch"):
            check.verify(self.capture([self.frame()]))
        self.manifest["firmware"] = self.reference("firmware.bin")
        self.manifest["origin"] = "measured"
        with self.assertRaisesRegex(check.InvalidCapture, "simulated"):
            check.verify(self.capture([self.frame()]))

    def test_parent_absolute_and_symlink_paths_cannot_escape(self):
        with tempfile.TemporaryDirectory() as elsewhere:
            outside = Path(elsewhere) / "secret.jpg"
            outside.write_bytes((self.root / "frame.jpg").read_bytes())
            (self.root / "escape.jpg").symlink_to(outside)
            for path in [str(outside), "../" + Path(elsewhere).name + "/secret.jpg", "escape.jpg"]:
                with self.subTest(path=path):
                    frame = self.frame()
                    frame["jpeg_path"] = path
                    report = check.verify(self.capture([frame]))
                    self.assertFalse(report["stream_integrity_verified"])
                    self.assertEqual(report["decoded_frames"], 0)

    def test_empty_and_incomplete_capture_are_distinct(self):
        report = check.verify(self.capture([]))
        self.assertFalse(report["stream_integrity_verified"])
        self.manifest["capture_complete"] = False
        report = check.verify(self.capture([self.frame()]))
        self.assertTrue(report["stream_integrity_verified"])
        self.assertFalse(report["capture_complete"])

    def test_boolean_sequence_and_unknown_channel_rejected(self):
        rows = [self.frame(), self.frame()]
        rows[0]["frame_seq"] = True
        rows[1]["channel_id"] = 7
        report = check.verify(self.capture(rows))
        self.assertEqual(report["decoded_frames"], 0)
        self.assertEqual(len(report["rejected_frames"]), 2)

    def test_cli_failure_writes_honest_report_and_never_overwrites(self):
        capture = self.capture([])
        out = self.root / "report.json"
        args = [sys.executable, str(Path(check.__file__)), "--capture", str(capture), "--out", str(out)]
        result = subprocess.run(args, capture_output=True, text=True)
        self.assertEqual(result.returncode, 1)
        report = json.loads(out.read_text())
        self.assertFalse(report["stream_integrity_verified"])
        self.assertFalse(report["firmware_execution_verified"])
        before = out.read_bytes()
        result = subprocess.run(args, capture_output=True, text=True)
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(out.read_bytes(), before)


if __name__ == "__main__":
    unittest.main()
