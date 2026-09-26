import unittest
from unittest.mock import patch

import app


class Clock:
    def __init__(self):
        self.now = 0.0
        self.stopped = False

    def __call__(self):
        return self.now

    def is_set(self):
        return self.stopped

    def wait(self, seconds):
        self.now += max(seconds, 0.001)


class Client:
    def __init__(self, codes=()):
        self.codes = iter(codes)
        self.calls = []

    def SetVelocity(self, *args):
        self.calls.append(args)
        return next(self.codes, 0)

    def GetFsmId(self):
        return 0, 500


class DemoTests(unittest.TestCase):
    @patch("builtins.print")
    def test_waits_for_grant_then_streams_and_stops(self, _):
        clock, client = Clock(), Client([3205, 3205, 0])
        app.run(client, clock, clock=clock)
        self.assertTrue(all(call[:3] == (0, 0, 0) for call in client.calls[:3]))
        self.assertGreater(len(client.calls), 300)
        self.assertTrue(any(call[2] > 0 for call in client.calls))
        self.assertTrue(any(call[2] < 0 for call in client.calls))
        self.assertEqual(client.calls[-1], (0, 0, 0, app.LEASE))

    @patch("builtins.print")
    def test_revoked_control_aborts_and_attempts_stop(self, _):
        clock, client = Clock(), Client([0, 3205, 0])
        with self.assertRaisesRegex(RuntimeError, "revoked"):
            app.run(client, clock, clock=clock)
        self.assertEqual(len(client.calls), 3)
        self.assertEqual(client.calls[-1][:3], (0, 0, 0))

    @patch("builtins.print")
    def test_timeout_never_sends_motion(self, _):
        clock, client = Clock(), Client([3205] * 20)
        with self.assertRaisesRegex(RuntimeError, "Timed out"):
            app.run(client, clock, grant_timeout=0.1, clock=clock)
        self.assertTrue(all(call[:3] == (0, 0, 0) for call in client.calls))

    @patch("builtins.print")
    def test_interrupt_attempts_stop(self, _):
        clock, client = Clock(), Client()
        def interrupt(seconds):
            clock.stopped = True
        clock.wait = interrupt
        app.run(client, clock, clock=clock)
        self.assertEqual(client.calls[-1][:3], (0, 0, 0))
        self.assertEqual(len(client.calls), 3)

    @patch("builtins.print")
    def test_transport_error_attempts_stop(self, _):
        clock, client = Clock(), Client([0, 3102, 0])
        with self.assertRaisesRegex(RuntimeError, "3102"):
            app.run(client, clock, clock=clock)
        self.assertEqual(client.calls[-1][:3], (0, 0, 0))


if __name__ == "__main__":
    unittest.main()
