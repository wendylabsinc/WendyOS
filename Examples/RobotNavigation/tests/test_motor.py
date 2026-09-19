import math
import types
import unittest
from robot_navigation.motor import MotorConfig, MotorGate, UnitreeBackend, build_backend


class Backend:
    def __init__(self):
        self.calls = []
        self.fail_move = self.fail_stop = False
        self.on_move = None

    def move(self, linear, angular):
        self.calls.append(("move", linear, angular))
        if self.on_move:
            self.on_move()
        if self.fail_move:
            raise RuntimeError("transport failed")

    def stop(self):
        self.calls.append(("stop",))
        if self.fail_stop:
            raise RuntimeError("stop transport failed")


class MotorTests(unittest.TestCase):
    def setUp(self):
        self.now = 100.0
        self.backend = Backend()
        self.config = MotorConfig(mode="twist", motion_enabled=True, watchdog_commissioned=True)
        self.gate = MotorGate(self.config, self.backend, monotonic=lambda: self.now)

    def receive(self, linear=0.2, angular=0.1, stamp=None, source_now=None, frame="base_link"):
        return self.gate.receive(linear, angular, self.now if stamp is None else stamp,
                                 self.now if source_now is None else source_now, frame)

    def test_default_and_uncommissioned_modes_never_create_backend_or_emit_commands(self):
        for mode in ("disabled", "twist", "unitree"):
            for motion, commissioned in ((False, False), (True, False), (False, True)):
                config = MotorConfig(mode=mode, motion_enabled=motion, watchdog_commissioned=commissioned,
                                     network_interface="enP8p1s0")
                def forbidden(*args):
                    self.fail("disabled mode initialized a physical backend")
                backend, _ = build_backend(config, twist_factory=forbidden, unitree_factory=forbidden)
                self.assertIsNone(backend)
                gate = MotorGate(config, self.backend, monotonic=lambda: self.now)
                self.assertFalse(gate.receive(0.2, 0.0, 100, 100, "base_link"))
                gate.tick()
                gate.stop()
                self.assertFalse(gate.status()["ready"])
        self.assertEqual(self.backend.calls, [])
        config = MotorConfig(mode="disabled", motion_enabled=True, watchdog_commissioned=True)
        self.assertFalse(config.enabled)

    def test_valid_command_and_guarded_zero_preserve_axes(self):
        self.assertTrue(self.receive())
        self.assertEqual(self.backend.calls, [("move", 0.2, 0.1)])
        self.now += 0.05
        self.assertTrue(self.receive(0, 0))
        self.assertEqual(self.backend.calls[-1], ("stop",))
        self.now += 0.05
        self.assertTrue(self.receive(0, 0))
        self.assertEqual(len(self.backend.calls), 2)
        self.assertTrue(self.gate.status()["ready"])
        self.assertTrue(self.gate.status()["stop_command_sent"])
        self.assertFalse(self.gate.status()["stopped_confirmed"])

    def test_watchdog_uses_monotonic_time_when_ros_clock_freezes(self):
        self.receive(stamp=10, source_now=10)
        self.now += 0.21
        self.gate.tick()
        self.assertEqual(self.backend.calls[-1], ("stop",))
        self.assertEqual(self.gate.status()["reason"], "command_watchdog_expired")
        self.assertFalse(self.receive(stamp=10, source_now=10))
        self.assertEqual(sum(c[0] == "move" for c in self.backend.calls), 1)

    def test_stale_future_replayed_and_regressing_stamps_stop_without_replay(self):
        self.receive()
        for stamp, source in ((99, 100), (100.2, 100.1), (100, 100.1), (99.99, 100.1)):
            self.assertFalse(self.receive(stamp=stamp, source_now=source))
            self.assertEqual(self.backend.calls[-1], ("stop",))
        self.now = 100.15
        self.assertTrue(self.receive())  # A rejected future stamp did not poison the watermark.
        self.assertEqual(sum(c[0] == "move" for c in self.backend.calls), 2)

    def test_invalid_frame_nonfinite_values_and_limits_stop(self):
        self.receive()
        self.now += 0.01
        self.assertFalse(self.receive(frame="odom"))
        self.assertEqual(self.gate.status()["reason"], "command_frame_invalid")
        for value in (math.nan, math.inf, -math.inf, True, "0.2"):
            self.assertFalse(self.receive(linear=value))
            self.assertFalse(self.receive(angular=value))
            self.assertFalse(self.receive(stamp=value))
            self.assertFalse(self.receive(source_now=value))
        self.assertFalse(self.receive(linear=0.26))
        self.assertFalse(self.receive(angular=-0.41))
        self.assertEqual(sum(c[0] == "move" for c in self.backend.calls), 1)

    def test_move_failure_attempts_stop_and_latches_backend_unavailable(self):
        self.backend.fail_move = True
        self.assertFalse(self.receive())
        self.assertEqual(self.backend.calls, [("move", 0.2, 0.1), ("stop",)])
        self.assertFalse(self.gate.status()["ready"])
        self.backend.fail_move = False
        self.now += 0.01
        self.assertFalse(self.receive())
        self.assertEqual(len(self.backend.calls), 2)

    def test_stop_failure_is_not_confirmed_and_is_retried(self):
        self.receive()
        self.backend.fail_stop = True
        self.now += 0.21
        self.gate.tick()
        self.assertFalse(self.gate.status()["ready"])
        self.assertFalse(self.gate.status()["stop_command_sent"])
        self.backend.fail_stop = False
        self.gate.tick()
        self.assertTrue(self.gate.status()["stop_command_sent"])
        self.assertFalse(self.gate.status()["stopped_confirmed"])
        self.assertFalse(self.gate.status()["ready"])

    def test_slow_backend_result_cannot_revalidate_an_expired_command(self):
        def slow():
            self.now += 0.3
        self.backend.on_move = slow
        self.assertFalse(self.receive())
        self.assertEqual(self.backend.calls[-1], ("stop",))
        self.assertEqual(self.gate.status()["reason"], "backend_command_expired")
        self.assertFalse(self.gate.status()["ready"])

    def test_regressing_monotonic_clock_stops_and_faults(self):
        self.receive()
        self.now -= 1
        self.gate.tick()
        self.assertEqual(self.backend.calls[-1], ("stop",))
        self.assertFalse(self.gate.status()["ready"])

    def test_shutdown_stops_once_and_blocks_further_commands(self):
        self.receive()
        self.gate.stop()
        self.gate.stop()
        self.now += 0.01
        self.assertFalse(self.receive())
        self.gate.tick()
        self.assertEqual(self.backend.calls, [("move", 0.2, 0.1), ("stop",)])

    def test_missing_optional_sdk_reports_unavailable_without_commands(self):
        config = MotorConfig(mode="unitree", motion_enabled=True, watchdog_commissioned=True,
                             network_interface="enP8p1s0")
        def absent(*args):
            raise ImportError("unitree_sdk2py is not installed")
        backend, reason = build_backend(config, unitree_factory=absent)
        gate = MotorGate(config, backend, unavailable_reason=reason)
        self.assertIsNone(backend)
        self.assertFalse(gate.status()["available"])
        self.assertIn("unitree_sdk2py", gate.status()["reason"])

    def test_unitree_adapter_uses_only_move_stop_and_bounded_rpc_timeout(self):
        calls = []
        class Sport:
            def __init__(self, enableLease):
                calls.append(("create", enableLease))
            def SetTimeout(self, timeout):
                calls.append(("timeout", timeout))
            def Init(self):
                calls.append(("init",))
            def Move(self, vx, vy, wz):
                calls.append(("move", vx, vy, wz))
                return 0
            def StopMove(self):
                calls.append(("stop",))
                return 0
        def importer(name):
            if name.endswith("channel"):
                return types.SimpleNamespace(ChannelFactoryInitialize=lambda domain, iface: calls.append(("channel", domain, iface)))
            return types.SimpleNamespace(SportClient=Sport)
        config = MotorConfig(mode="unitree", motion_enabled=True, watchdog_commissioned=True,
                             network_interface="enP8p1s0")
        backend, error = build_backend(config, unitree_factory=lambda iface, timeout: UnitreeBackend(iface, timeout, import_module=importer))
        self.assertEqual(error, "")
        self.assertEqual(calls, [("channel", 0, "enP8p1s0"), ("create", False), ("timeout", 0.1), ("init",)])
        backend.move(0.2, 0.1)
        backend.stop()
        self.assertEqual(calls[-2:], [("move", 0.2, 0.0, 0.1), ("stop",)])
        for code in (None, False, 1, -1):
            with self.assertRaises(RuntimeError):
                UnitreeBackend._check(code)

    def test_invalid_config_is_rejected_before_backend_creation(self):
        for kwargs in ({"mode": "other"}, {"motion_enabled": 1}, {"watchdog_commissioned": "true"},
                       {"command_timeout": math.nan}, {"command_timeout": 0}, {"command_timeout": 2},
                       {"max_linear": -1}, {"max_angular": True},
                       {"mode": "unitree", "network_interface": "eth0<xml>"},
                       {"output_topic": "/robot_navigation/safe_cmd_vel"}):
            with self.assertRaises(ValueError):
                MotorConfig(**kwargs)


if __name__ == "__main__":
    unittest.main()
