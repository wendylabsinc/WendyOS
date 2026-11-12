"""Tests the Linux half of the thermostat against a fake Arduino Bridge."""
import json
import threading
import unittest
import urllib.error
import urllib.request

import app


class FakeBridge:
    def __init__(self, fail_calls=False):
        self.provided = {}
        self.calls = []
        self.fail_calls = fail_calls

    def provide(self, name, handler):
        self.provided[name] = handler

    def call(self, name, *params, timeout=None):
        if self.fail_calls:
            raise TimeoutError("no answer")
        self.calls.append((name, *params))
        return params[0]


class Clock:
    def __init__(self):
        self.now = 1_000_000.0

    def __call__(self):
        return self.now


class ThermostatTest(unittest.TestCase):
    def start(self, bridge, clock=None):
        thermostat = app.Thermostat(bridge, clock or Clock())
        server = app.make_server(thermostat, 0)
        threading.Thread(target=server.serve_forever, daemon=True).start()
        self.addCleanup(server.shutdown)
        self.addCleanup(server.server_close)
        return thermostat, f"http://127.0.0.1:{server.server_address[1]}"

    def get(self, url):
        with urllib.request.urlopen(url, timeout=5) as r:
            return r.status, json.loads(r.read())

    def post(self, url, body):
        req = urllib.request.Request(url, data=json.dumps(body).encode(), method="POST",
                                     headers={"Content-Type": "application/json"})
        try:
            with urllib.request.urlopen(req, timeout=5) as r:
                return r.status, json.loads(r.read())
        except urllib.error.HTTPError as e:
            return e.code, json.loads(e.read())

    def test_state_mirrors_the_sketch(self):
        bridge = FakeBridge()
        _, base = self.start(bridge)
        bridge.provided["state"](20.4, 41.0, 22, True, True)
        status, state = self.get(base + "/api/state")
        self.assertEqual(status, 200)
        self.assertEqual((state["celsius"], state["humidity"], state["target"], state["enabled"], state["heating"]),
                         (20.4, 41.0, 22, True, True))
        self.assertEqual((state["target_min"], state["target_max"]), (10, 30))

    def test_missing_thermo_reads_as_null(self):
        bridge = FakeBridge()
        _, base = self.start(bridge)
        bridge.provided["state"](float("nan"), float("nan"), 21, True, False)

        def reject(token):
            raise ValueError("non-JSON constant " + token)

        with urllib.request.urlopen(base + "/api/state", timeout=5) as r:
            state = json.loads(r.read(), parse_constant=reject)
        self.assertIsNone(state["celsius"])
        self.assertIsNone(state["humidity"])

    def test_history_keeps_a_point_a_minute_and_every_switch(self):
        bridge, clock = FakeBridge(), Clock()
        thermostat, _ = self.start(bridge, clock)
        notify = bridge.provided["state"]
        notify(20.0, 40.0, 22, True, True)
        clock.now += 2
        notify(20.1, 40.0, 22, True, True)    # same minute, nothing changed: skipped
        clock.now += 2
        notify(20.2, 40.0, 23, True, True)    # target changed: kept
        clock.now += 2
        notify(23.6, 40.0, 23, True, False)   # heating stopped: kept
        clock.now += 60
        notify(23.4, 40.0, 23, True, False)   # a minute later: kept
        self.assertEqual([p["celsius"] for p in thermostat.history()], [20.0, 20.2, 23.6, 23.4])

    def test_history_drops_points_older_than_a_day(self):
        bridge, clock = FakeBridge(), Clock()
        thermostat, _ = self.start(bridge, clock)
        bridge.provided["state"](20.0, 40.0, 22, True, True)
        clock.now += app.HISTORY_SECONDS + 1
        bridge.provided["state"](21.0, 40.0, 22, True, True)
        self.assertEqual([p["celsius"] for p in thermostat.history()], [21.0])

    def test_set_target_calls_the_sketch(self):
        bridge = FakeBridge()
        _, base = self.start(bridge)
        status, body = self.post(base + "/api/target", {"target": 23})
        self.assertEqual((status, body), (200, {"ok": True, "target": 23}))
        self.assertEqual(bridge.calls, [("set_target", 23)])

    def test_bad_targets_never_reach_the_sketch(self):
        bridge = FakeBridge()
        _, base = self.start(bridge)
        for body in ({"target": 31}, {"target": 9}, {"target": 22.5}, {"target": "22"}, {"target": True}, {}, [1]):
            status, reply = self.post(base + "/api/target", body)
            self.assertEqual(status, 400, body)
            self.assertFalse(reply["ok"])
        self.assertEqual(bridge.calls, [])

    def test_set_enabled_calls_the_sketch(self):
        bridge = FakeBridge()
        _, base = self.start(bridge)
        status, body = self.post(base + "/api/enabled", {"enabled": False})
        self.assertEqual((status, body), (200, {"ok": True, "enabled": False}))
        status, _ = self.post(base + "/api/enabled", {"enabled": "no"})
        self.assertEqual(status, 400)
        self.assertEqual(bridge.calls, [("set_enabled", False)])

    def test_a_silent_sketch_reports_502(self):
        _, base = self.start(FakeBridge(fail_calls=True))
        status, body = self.post(base + "/api/target", {"target": 22})
        self.assertEqual(status, 502)
        self.assertIn("did not answer", body["error"])

    def test_page_and_health(self):
        _, base = self.start(FakeBridge())
        with urllib.request.urlopen(base + "/", timeout=5) as r:
            self.assertIn(b"/api/history", r.read())
        with urllib.request.urlopen(base + "/health", timeout=5) as r:
            self.assertEqual(r.read(), b"ok")
        status, state = self.get(base + "/api/state")
        self.assertIsNone(state["celsius"])


if __name__ == "__main__":
    unittest.main()
