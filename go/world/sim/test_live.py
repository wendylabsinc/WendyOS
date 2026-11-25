import base64
import hashlib
import json
from pathlib import Path
import sys
import threading
import time
import unittest
from unittest.mock import patch
import urllib.error
import urllib.request

from sim.actuator import Rejected
from sim.live import LiveScene, MAX_SNAPSHOT_BYTES, live_package, make_server
from sim.run import PackageError

ROOT = Path(__file__).resolve().parents[1]
PACKAGE = ROOT / "examples/live-office/world.json"
ACTUATOR_TOKEN = "a" * 48
SENSOR_TOKEN = "s" * 48


def wait_for(fn, predicate, timeout=5):
    deadline = time.monotonic() + timeout
    value = None
    while time.monotonic() < deadline:
        try:
            value = fn()
            if predicate(value):
                return value
        except Rejected:
            pass
        time.sleep(.02)
    raise AssertionError(f"condition not reached: {value}")


@unittest.skipUnless(sys.platform.startswith("linux"), "live render worker requires Linux OSMesa; tested in sim/live.Dockerfile")
class LiveSceneTests(unittest.TestCase):
    def setUp(self):
        self.scene = LiveScene(live_package(PACKAGE), run_id="live-test", interval_ms=100, allow_test_faults=True)
        self.scene.start()
        self.initial = wait_for(lambda: json.loads(self.scene.samples()), lambda s: len(s["observations"]) == 4)

    def tearDown(self):
        self.scene.close()

    def test_coherent_snapshot_and_content_addressed_media(self):
        snapshot = self.initial
        self.assertEqual(snapshot["state"]["sample_unix_ms"], snapshot["sample_unix_ms"])
        self.assertEqual(snapshot["state"]["boot_id"], snapshot["boot_id"])
        self.assertEqual(round(snapshot["state"]["simulation_time_s"] * 1e9), snapshot["simulation_time_ns"])
        self.assertLess(len(self.scene.samples()), MAX_SNAPSHOT_BYTES)
        blobs = {m["sha256"]: base64.b64decode(m["data"]) for m in snapshot["media"]}
        self.assertEqual(len(blobs), 3)
        for sample in snapshot["observations"]:
            self.assertEqual(sample["sequence"], snapshot["sequence"])
            self.assertEqual(sample["sample_ns"], snapshot["simulation_time_ns"])
            self.assertEqual(sample["package_sha256"], self.scene.package["_digest"])
            if "evidence" in sample:
                blob = blobs[sample["evidence"]["sha256"]]
                self.assertEqual(hashlib.sha256(blob).hexdigest(), sample["evidence"]["sha256"])
                self.assertEqual(len(blob), sample["evidence"]["size"])
            else:
                self.assertEqual(sample["position_m"], snapshot["state"]["position_m"])
                self.assertGreater(sample["gas"]["value_ppm"], 50)

    def test_control_identity_does_not_depend_on_rendered_media(self):
        self.scene.request("/v1/render-pause", {"paused": True})
        state = self.scene.control_state()
        self.assertEqual(state["run_id"], "live-test")
        self.assertEqual(state["provenance"]["package_sha256"], self.scene.package["_digest"])
        self.assertEqual(state["site_id"], "synthetic-office")
        # The existing capture contract stays byte-compatible in its fields.
        self.assertNotIn("run_id", self.initial["state"])

    def test_commands_change_rendered_robot_camera_and_gas(self):
        before = self.initial
        state = self.scene.request("/v1/session", {})
        for seq in range(1, 11):
            self.scene.request("/v1/command", {"session": state["session"], "sequence": seq, "velocity_mps": .5, "lease_ms": 300})
            time.sleep(.1)
        after = wait_for(lambda: json.loads(self.scene.samples()), lambda s:s["state"]["position_m"][0] > before["state"]["position_m"][0] + .3)
        b = {s["sensor_id"]:s for s in before["observations"]}
        a = {s["sensor_id"]:s for s in after["observations"]}
        self.assertNotEqual(a["camera-a"]["evidence"]["sha256"], b["camera-a"]["evidence"]["sha256"])
        self.assertNotEqual(a["camera-b"]["evidence"]["sha256"], b["camera-b"]["evidence"]["sha256"])
        self.assertGreater(a["camera-b"]["position_m"][0], b["camera-b"]["position_m"][0] + .3)
        self.assertEqual(a["gas-1"]["position_m"], after["state"]["position_m"])
        self.assertGreater(a["gas-1"]["gas"]["value_ppm"], b["gas-1"]["gas"]["value_ppm"] + 1)

    def test_paused_render_cannot_renew_stale_snapshot_or_block_watchdog(self):
        state = self.scene.request("/v1/session", {})
        self.scene.request("/v1/command", {"session": state["session"], "sequence": 1, "velocity_mps": .5, "lease_ms": 250})
        wait_for(self.scene.actuator.state, lambda s:s["velocity_mps"][0] > .1)
        self.scene.request("/v1/render-pause", {"paused": True})
        stopped = wait_for(self.scene.actuator.state, lambda s:s["watchdog_stopped"] and s["velocity_mps"][0] == 0, timeout=.6)
        time.sleep(1.05)
        with self.assertRaises(Rejected) as caught:
            self.scene.samples()
        self.assertEqual(caught.exception.status, 503)
        self.assertGreater(self.scene.actuator.state()["simulation_time_s"], stopped["simulation_time_s"] + .5)

    def test_fault_removes_gas_and_brakes(self):
        self.scene.request("/v1/fault", {"sensor":"gas", "available":False})
        latest = wait_for(lambda:json.loads(self.scene.samples()), lambda s:not s["state"]["sensors"]["gas"])
        self.assertNotIn("gas-1", [s["sensor_id"] for s in latest["observations"]])
        self.assertEqual(latest["state"]["velocity_mps"], [0.,0.,0.])

    def test_cached_snapshot_is_not_restamped_on_reads(self):
        self.scene.request("/v1/render-pause", {"paused":True})
        # Let an already-started render finish before selecting the cache.
        time.sleep(.15)
        before = self.scene.samples()
        time.sleep(.15)
        self.assertEqual(self.scene.samples(), before)
        snapshot = json.loads(before)
        self.assertGreater(int(time.time()*1000), snapshot["sample_unix_ms"]+100)

    def test_renderer_skips_unchanged_physics_then_recovers(self):
        self.scene.request("/v1/render-pause", {"paused": True})
        time.sleep(.15)  # Let an in-flight render finish before freezing its step.
        before = self.scene.samples()
        snapshot = json.loads(before)
        original = self.scene._copy_state

        def same_physics_step(target):
            state, captured = original(target)
            state["simulation_time_s"] = snapshot["simulation_time_ns"] / 1e9
            return state, captured

        with patch.object(self.scene, "_copy_state", same_physics_step):
            self.scene.request("/v1/render-pause", {"paused": False})
            time.sleep(.3)
            self.assertEqual(self.scene.samples(), before)
            time.sleep(.8)
            with self.assertRaises(Rejected) as caught:
                self.scene.samples()
            self.assertEqual(caught.exception.status, 503)
            self.assertEqual(self.scene.sequence, snapshot["sequence"])
            self.assertIsNone(self.scene.render_error)
        newer = wait_for(lambda: json.loads(self.scene.samples()), lambda s: s["sequence"] > snapshot["sequence"])
        self.assertGreater(newer["simulation_time_ns"], snapshot["simulation_time_ns"])

    def test_sensor_token_cannot_control_or_inject_faults(self):
        server = make_server(("127.0.0.1",0), self.scene, ACTUATOR_TOKEN, SENSOR_TOKEN)
        worker = threading.Thread(target=server.serve_forever,daemon=True); worker.start()
        try:
            endpoint = "http://127.0.0.1:" + str(server.server_port)
            req = urllib.request.Request(endpoint+"/v1/samples", headers={"Authorization":"Bearer "+SENSOR_TOKEN})
            with urllib.request.urlopen(req) as response:self.assertEqual(json.load(response)["run_id"],"live-test")
            for path,body in (("session",{}),("render-pause",{"paused":True}),("fault",{"sensor":"gas","available":False})):
                req = urllib.request.Request(endpoint+"/v1/"+path,data=json.dumps(body).encode(),headers={"Authorization":"Bearer "+SENSOR_TOKEN})
                with self.assertRaises(urllib.error.HTTPError) as error:urllib.request.urlopen(req)
                self.assertEqual(error.exception.code,401)
        finally:
            server.shutdown();server.server_close();worker.join(timeout=2)


class LiveConfigurationTests(unittest.TestCase):
    def test_repeated_physics_step_cannot_publish_or_refresh_capture(self):
        # No renderer or MuJoCo is needed: this reproduces scheduling where
        # rendering observes the last published physics step again even though
        # the wall clock has advanced. The cache must keep its original age.
        scene = LiveScene.__new__(LiveScene)
        scene.last_published_sim_ns = 500_000_000
        scene.sequence = 12
        scene.latest = b"previous immutable snapshot"
        scene.capture_monotonic = 10.0
        scene._copy_state = lambda data: ({"simulation_time_s": .5, "sample_unix_ms": 1800000001000}, 11.0)
        snapshot, captured = scene._snapshot(None, None)
        self.assertIsNone(snapshot)
        self.assertEqual(captured, 11.0)
        self.assertEqual(scene.sequence, 12)
        self.assertEqual(scene.latest, b"previous immutable snapshot")
        self.assertEqual(scene.capture_monotonic, 10.0)

    def test_physics_rollback_is_not_treated_as_duplicate(self):
        scene = LiveScene.__new__(LiveScene)
        scene.last_published_sim_ns = 500_000_000
        scene._copy_state = lambda data: ({"simulation_time_s": .49}, 11.0)
        with self.assertRaisesRegex(PackageError, "clock regressed"):
            scene._snapshot(None, None)

    def test_same_tokens_rejected(self):
        with self.assertRaises(ValueError):make_server(("127.0.0.1",0),None,ACTUATOR_TOKEN,ACTUATOR_TOKEN)

    def test_fixture_is_one_actuated_scene_with_explicit_bindings(self):
        p = live_package(PACKAGE)
        self.assertEqual(p["mujoco"]["control_mode"], "velocity")
        self.assertEqual(p["live_sensors"]["gas_binding"], "robot")
        self.assertEqual(p["provenance"], "synthetic")


if __name__ == "__main__":unittest.main()
