"""Real MuJoCo and HTTP adapter checks; no physical robot is accessed."""

import json
import math
from pathlib import Path
import tempfile
import subprocess
import sys
import threading
import time
import unittest
from unittest.mock import patch
import urllib.error
import urllib.request

from sim.actuator import Actuator, Rejected, make_server, read_token
from sim.run import PackageError, load_package


PACKAGE = Path(__file__).resolve().parents[1] / "examples/mission-office/world.json"
TOKEN = "a" * 48


class ActuatorTests(unittest.TestCase):
    def setUp(self):
        self.adapter = Actuator(load_package(PACKAGE), allow_test_faults=True)
        self.adapter.start()
        self.session = self.adapter.state()["session"]

    def tearDown(self):
        self.adapter.close()

    def command(self, sequence=1, velocity=.5, lease=500):
        return self.adapter.request("/v1/command", {"session": self.session, "sequence": sequence,
                                                     "velocity_mps": velocity, "lease_ms": lease})

    def test_actual_motion_and_watchdog_brakes_without_executor(self):
        initial = self.adapter.state()
        self.command(lease=300)
        time.sleep(.18)
        moving = self.adapter.state()
        self.assertGreater(moving["position_m"][0], initial["position_m"][0] + .02)
        self.assertGreater(moving["velocity_mps"][0], .1)
        # No executor commands occur after this point. The physics process must
        # still advance, and the independent lease must remove actual velocity.
        time.sleep(.3)
        stopped = self.adapter.state()
        self.assertTrue(stopped["watchdog_stopped"])
        self.assertEqual(stopped["velocity_mps"], [0., 0., 0.])
        time.sleep(.08)
        later = self.adapter.state()
        self.assertEqual(later["position_m"], stopped["position_m"])
        self.assertGreater(later["simulation_time_s"], stopped["simulation_time_s"])
        self.assertGreater(later["sample_unix_ms"], stopped["sample_unix_ms"])

    def test_stop_brakes_actual_velocity_and_replay_cannot_restart(self):
        self.command()
        time.sleep(.08)
        stopped = self.adapter.request("/v1/stop", {"session": self.session, "sequence": 2})
        self.assertEqual(stopped["velocity_mps"][0], 0)
        with self.assertRaises(Rejected):
            self.command(sequence=1)
        self.assertTrue(self.adapter.state()["watchdog_stopped"])

    def test_session_rotation_invalidates_old_executor_and_restart(self):
        self.command()
        claimed = self.adapter.request("/v1/session", {})
        self.assertNotEqual(self.session, claimed["session"])
        self.assertTrue(claimed["watchdog_stopped"])
        with self.assertRaises(Rejected):
            self.command(sequence=2)
        restarted = Actuator(load_package(PACKAGE))
        try:
            self.assertNotEqual(restarted.boot_id, self.adapter.boot_id)
            with self.assertRaises(Rejected):
                restarted.request("/v1/command", {"session": claimed["session"], "sequence": 1,
                                                  "velocity_mps": .5, "lease_ms": 500})
            self.assertTrue(restarted.state()["watchdog_stopped"])
        finally:
            restarted.close()

    def test_invalid_command_does_not_consume_sequence_or_renew(self):
        for velocity, lease in [(math.nan, 100), (math.inf, 100), (.6, 100), (True, 100),
                                (.4, 501), (.4, 0), (.4, True), (10**1000, 100)]:
            with self.subTest(velocity=velocity, lease=lease), self.assertRaises(Rejected):
                self.command(velocity=velocity, lease=lease)
        self.assertEqual(self.adapter.state()["sequence"], 0)
        self.assertTrue(self.adapter.state()["watchdog_stopped"])

    def test_sensor_loss_brakes_and_blocks_commands_until_repaired(self):
        self.command()
        state = self.adapter.request("/v1/fault", {"sensor": "gas", "available": False})
        self.assertFalse(state["sensors"]["gas"])
        self.assertTrue(state["watchdog_stopped"])
        with self.assertRaises(Rejected):
            self.command(sequence=2)
        self.adapter.request("/v1/fault", {"sensor": "gas", "available": True})
        self.assertTrue(self.adapter.state()["watchdog_stopped"])
        self.command(sequence=2)

    def test_fault_endpoint_disabled_by_default(self):
        normal = Actuator(load_package(PACKAGE))
        try:
            with self.assertRaises(Rejected):
                normal.request("/v1/fault", {"sensor": "gas", "available": False})
        finally:
            normal.close()

    def bounded_command(self, sequence=1, velocity=.5, lease=300, not_after=None):
        if not_after is None:
            not_after = int(time.time() * 1000) + lease
        return self.adapter.request("/v1/command/bounded", {
            "session": self.session, "sequence": sequence,
            "velocity_mps": velocity, "lease_ms": lease, "not_after_unix_ms": not_after})

    def test_delayed_command_cannot_start_or_renew_after_deadline(self):
        not_after = int(time.time() * 1000) + 20
        time.sleep(.05)
        with self.assertRaises(Rejected):
            self.bounded_command(not_after=not_after)
        state = self.adapter.state()
        self.assertEqual(state["sequence"], 0)
        self.assertTrue(state["watchdog_stopped"])
        self.assertEqual(state["velocity_mps"], [0., 0., 0.])

    def test_arrival_delay_consumes_watchdog_budget(self):
        not_after = int(time.time() * 1000) + 180
        started = time.monotonic()
        time.sleep(.10)
        result = self.bounded_command(lease=300, not_after=not_after)
        self.assertLessEqual(result["lease_remaining_ms"], 80)
        with self.adapter.lock:
            self.assertLessEqual(self.adapter.deadline, started + .182)
        time.sleep(.12)
        state = self.adapter.state()
        self.assertTrue(state["watchdog_stopped"])
        self.assertEqual(state["velocity_mps"], [0., 0., 0.])
        # A fresh legitimately renewed command may run with a greater sequence.
        self.bounded_command(sequence=2)
        time.sleep(.06)
        self.assertGreater(self.adapter.state()["velocity_mps"][0], .1)

    def test_bounded_command_requires_exact_deadline_and_current_session(self):
        for value in (0, True, 1.5, "later", int(time.time() * 1000) + 10000):
            with self.subTest(value=value), self.assertRaises(Rejected):
                self.bounded_command(not_after=value)
        with self.assertRaises(Rejected):
            self.adapter.request("/v1/command/bounded", {
                "session": self.session, "sequence": 1, "velocity_mps": .5, "lease_ms": 100})
        self.adapter.request("/v1/session", {})
        with self.assertRaises(Rejected):
            self.bounded_command()
        self.assertEqual(self.adapter.state()["sequence"], 0)
        self.assertTrue(self.adapter.state()["watchdog_stopped"])

    def test_clock_rollback_brakes_active_bounded_motion(self):
        self.bounded_command(lease=300)
        time.sleep(.06)
        self.assertGreater(self.adapter.state()["velocity_mps"][0], .1)
        before = time.time()
        with patch("sim.actuator.time.time", return_value=before - .1):
            time.sleep(.04)
        state = self.adapter.state()
        self.assertTrue(state["watchdog_stopped"])
        self.assertEqual(state["velocity_mps"], [0., 0., 0.])
        with self.assertRaises(Rejected):
            self.bounded_command(sequence=2)

    def test_clock_rollback_latches_bounded_commands_only(self):
        # No physics thread runs during this deterministic clock-fault check.
        adapter = Actuator(load_package(PACKAGE))
        try:
            now_ms = int(time.time() * 1000)
            body = {"session": adapter.session, "sequence": 1,
                    "velocity_mps": .5, "lease_ms": 100, "not_after_unix_ms": now_ms + 100}
            with patch("sim.actuator.time.time", return_value=(now_ms - 20) / 1000):
                with self.assertRaises(Rejected):
                    adapter.request("/v1/command/bounded", body)
            with self.assertRaises(Rejected):
                adapter.request("/v1/command/bounded", body)
            self.assertTrue(adapter.bounded_clock_fault)
            self.assertEqual(adapter.sequence, 0)
            ordinary = dict(body)
            del ordinary["not_after_unix_ms"]
            adapter.request("/v1/command", ordinary)
            self.assertEqual(adapter.sequence, 1)
        finally:
            adapter.close()

    def test_state_contract_is_finite_and_explicitly_synthetic(self):
        state = self.adapter.state()
        json.dumps(state, allow_nan=False)
        self.assertTrue(state["synthetic"])
        self.assertEqual(state["frame"], {"id": "office-floor", "revision": "office-frame-v1"})
        self.assertEqual(state["position_m"][1:], [.2, -0.])
        self.assertGreater(state["gas_ppm"], 0)


class HTTPTests(unittest.TestCase):
    def setUp(self):
        self.adapter = Actuator(load_package(PACKAGE))
        self.adapter.start()
        self.server = make_server(("127.0.0.1", 0), self.adapter, TOKEN)
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.thread.start()
        self.url = "http://127.0.0.1:" + str(self.server.server_port)

    def tearDown(self):
        self.server.shutdown()
        self.server.server_close()
        self.thread.join()
        self.adapter.close()

    def request(self, path, body=None, token=TOKEN):
        headers = {"Authorization": "Bearer " + token}
        if body is not None:
            headers["Content-Type"] = "application/json"
        request = urllib.request.Request(self.url + path, data=body, headers=headers)
        with urllib.request.urlopen(request, timeout=2) as response:
            return json.load(response)

    def test_auth_and_nonfinite_input(self):
        with self.assertRaises(urllib.error.HTTPError) as caught:
            self.request("/v1/state", token="wrong")
        self.assertEqual(caught.exception.code, 401)
        state = self.request("/v1/state")
        body = json.dumps({"session": state["session"], "sequence": 1,
                           "velocity_mps": math.nan, "lease_ms": 300}).encode()
        with self.assertRaises(urllib.error.HTTPError) as caught:
            self.request("/v1/command", body)
        self.assertEqual(caught.exception.code, 400)
        self.assertEqual(self.request("/v1/state")["sequence"], 0)

    def test_http_command_and_stop(self):
        state = self.request("/v1/session", b"{}")
        body = {"session": state["session"], "sequence": 1, "velocity_mps": .4, "lease_ms": 300}
        self.request("/v1/command", json.dumps(body).encode())
        time.sleep(.1)
        self.assertGreater(self.request("/v1/state")["velocity_mps"][0], .1)
        stopped = self.request("/v1/stop", json.dumps({"session": state["session"], "sequence": 2}).encode())
        self.assertEqual(stopped["velocity_mps"][0], 0)

    def test_http_delayed_bounded_command_is_refused(self):
        state = self.request("/v1/session", b"{}")
        body = {"session": state["session"], "sequence": 1, "velocity_mps": .4,
                "lease_ms": 300, "not_after_unix_ms": int(time.time() * 1000) + 20}
        time.sleep(.05)
        with self.assertRaises(urllib.error.HTTPError) as caught:
            self.request("/v1/command/bounded", json.dumps(body).encode())
        self.assertEqual(caught.exception.code, 409)
        self.assertEqual(self.request("/v1/state")["sequence"], 0)
        body["not_after_unix_ms"] = int(time.time() * 1000) + 120
        self.request("/v1/command/bounded", json.dumps(body).encode())
        time.sleep(.18)
        stopped = self.request("/v1/state")
        self.assertTrue(stopped["watchdog_stopped"])
        self.assertEqual(stopped["velocity_mps"], [0., 0., 0.])

    def test_private_token_file(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "token"
            path.write_text(TOKEN)
            path.chmod(0o644)
            with self.assertRaises(PackageError):
                read_token(path)
            path.chmod(0o600)
            self.assertEqual(read_token(path), TOKEN)

    def test_unknown_duplicate_and_oversized_fields(self):
        for body in (b'{"extra":true}', b'{"a":1,"a":2}', b' ' * 2049):
            with self.subTest(body=body[:40]), self.assertRaises(urllib.error.HTTPError):
                self.request("/v1/session", body)


class IndependentProcessTests(unittest.TestCase):
    def test_killed_producer_does_not_kill_physics_and_lease_stops_motion(self):
        root = PACKAGE.parents[2]
        with tempfile.TemporaryDirectory() as directory:
            token = Path(directory) / "token"
            token.write_text(TOKEN)
            token.chmod(0o600)
            service = subprocess.Popen(
                [sys.executable, "-m", "sim.actuator", str(PACKAGE), "--token-file", str(token), "--port", "0"],
                cwd=root, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
            producer = None
            try:
                ready = json.loads(service.stdout.readline())
                url = "http://127.0.0.1:" + str(ready["listen"][1])
                def state():
                    request = urllib.request.Request(url + "/v1/state", headers={"Authorization": "Bearer " + TOKEN})
                    with urllib.request.urlopen(request, timeout=2) as response:
                        return json.load(response)
                script = """
import json,sys,time,urllib.request
url,token=sys.argv[1:]
def send(path,body):
    req=urllib.request.Request(url+path,data=json.dumps(body).encode(),headers={'Authorization':'Bearer '+token})
    with urllib.request.urlopen(req,timeout=2) as response:return json.load(response)
session=send('/v1/session',{})['session']
sequence=0
while True:
    sequence+=1
    send('/v1/command',{'session':session,'sequence':sequence,'velocity_mps':.5,'lease_ms':200})
    time.sleep(.05)
"""
                producer = subprocess.Popen([sys.executable, "-c", script, url, TOKEN],
                                            stdout=subprocess.DEVNULL, stderr=subprocess.PIPE)
                deadline = time.monotonic() + 3
                while state()["velocity_mps"][0] < .1 and time.monotonic() < deadline:
                    time.sleep(.02)
                self.assertGreater(state()["velocity_mps"][0], .1)
                producer.kill()
                producer.wait(timeout=2)
                time.sleep(.35)
                stopped = state()
                self.assertIsNone(service.poll())
                self.assertTrue(stopped["watchdog_stopped"])
                self.assertEqual(stopped["velocity_mps"][0], 0)
                time.sleep(.06)
                later = state()
                self.assertEqual(later["position_m"], stopped["position_m"])
                self.assertGreater(later["simulation_time_s"], stopped["simulation_time_s"])
            finally:
                if producer is not None:
                    if producer.poll() is None:
                        producer.kill()
                        producer.wait(timeout=2)
                    producer.stderr.close()
                service.terminate()
                service.wait(timeout=3)
                service.stdout.close()
                service.stderr.close()


if __name__ == "__main__":
    unittest.main()
