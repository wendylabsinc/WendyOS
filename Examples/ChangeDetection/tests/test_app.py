import io
import json
from pathlib import Path
import tempfile
import threading
import unittest
import urllib.error
import urllib.request
import zipfile

import cv2

from app import Runtime, make_server
from change_detection.sources import CaptureSource, DemoSource
from change_detection.audio_runtime import AudioRuntime
from change_detection.audio_sources import DemoAudioSource


class AppTests(unittest.TestCase):
    def setUp(self):
        self.runtime = Runtime(DemoSource())
        for _ in range(14):
            self.runtime.tick()
        self.server = make_server(self.runtime, port=0)
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.thread.start()
        self.url = f"http://127.0.0.1:{self.server.server_port}"

    def tearDown(self):
        self.server.shutdown()
        self.server.server_close()
        self.runtime.close()
        self.thread.join()

    def request(self, route, body=None, content_type="application/json"):
        request = urllib.request.Request(self.url + route,
                                         data=None if body is None else json.dumps(body).encode(),
                                         headers={"Content-Type": content_type})
        return urllib.request.urlopen(request)

    def test_dashboard_and_state(self):
        with self.request("/") as response:
            self.assertIn(b"What changed?", response.read())
        with self.request("/api/state") as response:
            state = json.load(response)
        self.assertEqual(state["score_kind"], "heuristic score")
        self.assertEqual(state["frame_size"], [640, 360])
        self.assertTrue(state["images"]["current"].startswith("data:image/jpeg;base64,"))
        self.assertTrue(state["regions"][0]["alert"])

    def test_export_contains_aligned_crops_and_manifest_features(self):
        with self.request("/api/export", {"id": 1, "label": "liquid"}) as response:
            archive = zipfile.ZipFile(io.BytesIO(response.read()))
        manifest = json.loads(archive.read(next(n for n in archive.namelist() if n.endswith("sample.json"))))
        self.assertTrue(manifest["synthetic"])
        self.assertEqual(manifest["label"], "liquid")
        self.assertGreater(manifest["features"]["age_seconds"], 8)
        self.assertIn(manifest["before"], archive.namelist())
        self.assertNotIn("split", manifest)

    def test_controls_reset_tracks_and_reject_invalid_requests(self):
        with self.request("/api/pause", {"paused": True}):
            pass
        self.assertTrue(self.runtime.paused)
        with self.request("/api/demo", {"scenario": "lighting"}):
            pass
        self.assertEqual(self.runtime.latest["events"], [])
        self.assertEqual(self.runtime.latest["regions"], [])
        for body in ({"scenario": "missing"}, []):
            with self.assertRaises(urllib.error.HTTPError) as error:
                self.request("/api/demo", body)
            self.assertEqual(error.exception.code, 400)
        with self.assertRaises(urllib.error.HTTPError) as error:
            self.request("/api/reset", {}, "text/plain")
        self.assertEqual(error.exception.code, 415)

    def test_capture_failure_is_visible_in_health_and_state(self):
        self.runtime.error = "Camera stopped returning frames"
        with self.assertRaises(urllib.error.HTTPError) as error:
            self.request("/health")
        self.assertEqual(error.exception.code, 503)
        with self.request("/api/state") as response:
            self.assertEqual(json.load(response)["error"], self.runtime.error)

    def test_audio_api_evidence_controls_and_health(self):
        self.runtime.audio = AudioRuntime(DemoAudioSource("belt_rip"))
        for _ in range(28):
            self.runtime.audio.tick()
        with self.request("/api/state") as response:
            audio = json.load(response)["audio"]
        self.assertTrue(audio["conveyor"]["stopped"])
        event = audio["events"][0]
        with self.request(f"/api/audio/clip?id={event['id']}") as response:
            self.assertEqual(response.headers["Content-Type"], "audio/wav")
            self.assertEqual(response.read()[:4], b"RIFF")
        with self.request("/api/audio/export", {"id": event["id"], "label": "belt_rip"}) as response:
            self.assertTrue(zipfile.is_zipfile(io.BytesIO(response.read())))
        with self.request("/api/audio/pause", {"paused": True}):
            pass
        self.assertTrue(self.runtime.audio.paused)
        self.assertFalse(self.runtime.paused)
        with self.request("/api/reset", {}):
            pass
        self.assertTrue(self.runtime.audio.conveyor.stopped)
        with self.request("/api/audio/demo", {"scenario": "normal"}):
            pass
        self.assertFalse(self.runtime.audio.conveyor.stopped)
        self.runtime.audio.error = "microphone disconnected"
        with self.assertRaises(urllib.error.HTTPError) as error:
            self.request("/health")
        self.assertEqual(error.exception.code, 503)
        self.assertEqual(json.load(error.exception)["audio_error"], "microphone disconnected")
        for path, body in (("/api/audio/export", {"id": 1, "label": "belt_rip"}),
                           ("/api/audio/demo", {"scenario": "invalid"}),
                           ("/api/audio/pause", {"paused": "yes"})):
            with self.assertRaises(urllib.error.HTTPError) as error:
                self.request(path, body)
            self.assertEqual(error.exception.code, 400)

    def test_audio_disabled_is_explicit(self):
        with self.request("/api/state") as response:
            self.assertFalse(json.load(response)["audio"]["enabled"])
        with self.assertRaises(urllib.error.HTTPError) as error:
            self.request("/api/audio/reset", {})
        self.assertEqual(error.exception.code, 400)


class VideoTests(unittest.TestCase):
    def test_sampled_video_uses_source_time_and_reports_eof(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "clip.avi"
            writer = cv2.VideoWriter(str(path), cv2.VideoWriter_fourcc(*"MJPG"), 10, (640, 360))
            self.assertTrue(writer.isOpened())
            demo = DemoSource()
            for _ in range(12):
                writer.write(demo.read()[0])
            writer.release()
            source = CaptureSource(str(path), fps=2)
            try:
                self.assertEqual([source.read()[1] for _ in range(3)], [0.0, 0.5, 1.0])
                with self.assertRaisesRegex(EOFError, "Video ended"):
                    source.read()
            finally:
                source.close()


if __name__ == "__main__":
    unittest.main()
