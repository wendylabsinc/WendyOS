"""Tests the Linux half of the demo against a fake Arduino Bridge."""
import json
import threading
import unittest
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
            raise TimeoutError("no answer from the sketch")
        self.calls.append((name, *params))


class DemoTest(unittest.TestCase):
    def start(self, bridge):
        demo = app.Demo(bridge)
        server = app.make_server(demo, 0)
        threading.Thread(target=server.serve_forever, daemon=True).start()
        self.addCleanup(server.shutdown)
        self.addCleanup(server.server_close)
        return demo, f"http://127.0.0.1:{server.server_address[1]}"

    def get(self, url):
        with urllib.request.urlopen(url, timeout=5) as r:
            return r.status, r.read()

    def post(self, url):
        req = urllib.request.Request(url, data=b"", method="POST")
        try:
            with urllib.request.urlopen(req, timeout=5) as r:
                return r.status, json.loads(r.read())
        except urllib.error.HTTPError as e:
            return e.code, json.loads(e.read())

    def test_state_tracks_notifications(self):
        bridge = FakeBridge()
        _, base = self.start(bridge)
        bridge.provided["knob"](42, True)
        bridge.provided["climate"](23.6, 41.0)
        status, body = self.get(base + "/api/state")
        self.assertEqual(status, 200)
        state = json.loads(body)
        self.assertEqual((state["knob"], state["pressed"]), (42, True))
        self.assertEqual((state["celsius"], state["humidity"]), (23.6, 41.0))

    def test_buzz_calls_bridge(self):
        bridge = FakeBridge()
        _, base = self.start(bridge)
        status, body = self.post(base + "/api/buzz")
        self.assertEqual((status, body["ok"]), (200, True))
        self.assertEqual(bridge.calls, [("buzz", 880, 200)])

    def test_buzz_reports_a_silent_sketch(self):
        _, base = self.start(FakeBridge(fail_calls=True))
        status, body = self.post(base + "/api/buzz")
        self.assertEqual(status, 502)
        self.assertIn("no answer", body["error"])

    def test_missing_thermo_reads_as_null(self):
        bridge = FakeBridge()
        _, base = self.start(bridge)
        bridge.provided["climate"](float("nan"), float("nan"))

        def reject(token):
            raise ValueError("non-JSON constant " + token)

        state = json.loads(self.get(base + "/api/state")[1], parse_constant=reject)
        self.assertIsNone(state["celsius"])
        self.assertIsNone(state["humidity"])

    def test_health_ok_without_mcu(self):
        _, base = self.start(FakeBridge())
        status, body = self.get(base + "/health")
        self.assertEqual((status, body), (200, b"ok"))
        state = json.loads(self.get(base + "/api/state")[1])
        self.assertIsNone(state["celsius"])

    def test_page_served(self):
        _, base = self.start(FakeBridge())
        status, body = self.get(base + "/")
        self.assertEqual(status, 200)
        self.assertIn(b"/api/state", body)


if __name__ == "__main__":
    unittest.main()
