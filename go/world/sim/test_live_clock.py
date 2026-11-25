"""Clock transport tests need no renderer, MuJoCo process, or actuator command."""
import json
import threading
from types import SimpleNamespace
import unittest
from unittest.mock import patch
import urllib.error
import urllib.request

from sim.actuator import Rejected
from sim.live import LiveScene, make_server
from sim.run import PackageError


ANCHOR = 1_794_398_400_000_000_000
READ_TOKEN = "s" * 48
ACTUATOR_TOKEN = "a" * 48
NONCE = "f" * 64


def scene_fixture(**kwargs):
    actuator = SimpleNamespace(lock=threading.Lock(), done=threading.Event(),
        thread=SimpleNamespace(is_alive=lambda: True), boot_id="boot-a", data=SimpleNamespace(time=.5))
    package = {"_digest": "a" * 64, "frame_revision": "survey-1"}
    options = dict(run_id="clock-test", clock_domain="simulation:clock-test", clock_anchor_unix_ns=ANCHOR)
    options.update(kwargs)
    with patch("sim.live.Actuator", return_value=actuator):
        scene = LiveScene(package, **options)
    return scene


class LiveClockTests(unittest.TestCase):
    def test_fixed_identity_and_paused_instant(self):
        scene = scene_fixture()
        scene.latest = b"unchanged rendered sample"
        scene.capture_monotonic = 10.0
        scene.sequence = 7
        with patch("sim.live.time.time_ns", side_effect=[ANCHOR + 10, ANCHOR + 1_000_000_000]):
            first = scene.source_clock(NONCE)
            second = scene.source_clock("e" * 64)
        self.assertEqual(first["simulation_time_ns"], 500_000_000)
        self.assertEqual(second["simulation_time_ns"], first["simulation_time_ns"])
        self.assertEqual(second["anchor_unix_ns"], ANCHOR)
        self.assertEqual(second["observation_sequence"], first["observation_sequence"] + 1)
        self.assertGreater(second["observed_unix_ns"], first["observed_unix_ns"])
        self.assertEqual(scene.latest, b"unchanged rendered sample")
        self.assertEqual(scene.capture_monotonic, 10.0)
        self.assertEqual(scene.sequence, 7)
        first["frame"]["revision"] = "caller-mutated"
        self.assertEqual(scene.clock_identity["frame"]["revision"], "survey-1")

    def test_uncommissioned_clock_and_partial_config_refused(self):
        scene = scene_fixture(clock_domain=None, clock_anchor_unix_ns=None)
        with self.assertRaises(Rejected):
            scene.source_clock(NONCE)
        for kwargs in ({"clock_domain": None}, {"clock_anchor_unix_ns": None},
                       {"clock_domain": "physical"}, {"clock_anchor_unix_ns": 0},
                       {"clock_anchor_unix_ns": True}, {"clock_anchor_unix_ns": 2**63}):
            with self.subTest(kwargs=kwargs), self.assertRaises(PackageError):
                scene_fixture(**kwargs)

    def test_regression_and_identity_change_latch(self):
        for name in ("time", "boot", "package", "frame", "wall"):
            with self.subTest(name=name):
                scene = scene_fixture()
                with patch("sim.live.time.time_ns", return_value=ANCHOR):
                    scene.source_clock(NONCE)
                if name == "time": scene.actuator.data.time = .4
                if name == "boot": scene.actuator.boot_id = "other-boot"
                if name == "package": scene.package["_digest"] = "b" * 64
                if name == "frame": scene.package["frame_revision"] = "other-frame"
                with patch("sim.live.time.time_ns", return_value=ANCHOR - (1 if name == "wall" else 0)):
                    with self.assertRaises(Rejected): scene.source_clock(NONCE)
                self.assertTrue(scene.clock_fault)
                scene.actuator.data.time = .6
                with self.assertRaises(Rejected): scene.source_clock(NONCE)

    def test_dead_physics_and_invalid_nonce_refused(self):
        scene = scene_fixture()
        for nonce in (None, "", "f" * 63, "g" * 64, "x\r\n" * 32):
            with self.subTest(nonce=nonce), self.assertRaises(Rejected): scene.source_clock(nonce)
        scene.actuator.done.set()
        with self.assertRaises(Rejected): scene.source_clock(NONCE)

    def test_read_token_clock_access_cannot_actuate(self):
        scene = scene_fixture()
        with patch.object(scene, "request", side_effect=AssertionError("read token actuated")):
            server = make_server(("127.0.0.1", 0), scene, ACTUATOR_TOKEN, READ_TOKEN)
            worker = threading.Thread(target=server.serve_forever, daemon=True)
            worker.start()
            endpoint = f"http://127.0.0.1:{server.server_port}"
            headers = {"Authorization": "Bearer " + READ_TOKEN, "X-Wendy-Clock-Nonce": NONCE}
            try:
                request = urllib.request.Request(endpoint + "/v1/clock", headers=headers)
                with urllib.request.urlopen(request, timeout=1) as response:
                    clock = json.load(response)
                self.assertEqual(clock["run_id"], "clock-test")
                self.assertEqual(clock["nonce"], NONCE)
                for path in ("session", "command", "command/bounded", "stop", "fault", "render-pause"):
                    request = urllib.request.Request(endpoint + "/v1/" + path, data=b"{}", headers=headers)
                    with self.subTest(path=path), self.assertRaises(urllib.error.HTTPError) as error:
                        urllib.request.urlopen(request, timeout=1)
                    self.assertEqual(error.exception.code, 401)
                request = urllib.request.Request(endpoint + "/v1/state", headers=headers)
                with self.assertRaises(urllib.error.HTTPError) as error:
                    urllib.request.urlopen(request, timeout=1)
                self.assertEqual(error.exception.code, 401)
                request = urllib.request.Request(endpoint + "/v1/clock", headers={"Authorization": "Bearer " + READ_TOKEN})
                with self.assertRaises(urllib.error.HTTPError) as error:
                    urllib.request.urlopen(request, timeout=1)
                self.assertEqual(error.exception.code, 400)
            finally:
                server.shutdown()
                server.server_close()
                worker.join(timeout=2)


if __name__ == "__main__":
    unittest.main()
