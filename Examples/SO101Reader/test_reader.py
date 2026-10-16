"""Run with python3 -m unittest -v; all serial traffic uses synthetic PTYs."""

import errno
import fcntl
import json
import os
from pathlib import Path
import pty
import plistlib
import signal
import socket
import struct
import subprocess
import sys
import tempfile
import termios
import threading
import time
import unittest
from unittest.mock import patch
import urllib.error
import urllib.request

from protocol import ReaderError, position, request, response
from reader import Config, Monitor, calibration_metadata, server_for
from transport import MockTransport, SerialTransport, check_port_owners, io_error
from identity import adapter_metadata


def frame(motor_id, data, bits=0):
    packet = bytes((255, 255, motor_id, len(data) + 2, bits)) + data
    return packet + bytes((~sum(packet[2:]) & 255,))


def wait_until(predicate, timeout=3):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        if predicate():
            return
        time.sleep(0.01)
    raise AssertionError("Condition timed out")


class ProtocolTests(unittest.TestCase):
    def test_exact_read_whitelist(self):
        self.assertEqual(request(1, 3, 2), bytes.fromhex("ffff0104020302f3"))
        self.assertEqual(request(6, 56, 8), bytes.fromhex("ffff0604023808b3"))
        for address in range(256):
            for count in (0, 1, 2, 8, 255):
                if (address, count) in {(3, 2), (56, 8)}:
                    packet = request(253, address, count)
                    self.assertEqual(packet[4], 2)
                    self.assertEqual(sum(packet[2:]) & 255, 255)
                else:
                    with self.assertRaises(ReaderError):
                        request(1, address, count)
        for motor_id in (0, 254, 255, -1, True, 1.5):
            with self.assertRaises(ReaderError):
                request(motor_id, 3, 2)

    def test_response_validation(self):
        good = frame(1, b"\x09\x03", 0x24)
        self.assertEqual(response(good, 1, 2), (b"\x09\x03", 0x24))
        bad = [b"", good[:-1], good + b"\x00", frame(2, b"\x09\x03"),
               b"\x00" + good[1:], good[:3] + b"\x05" + good[4:], good[:-1] + b"\x00"]
        for item in bad:
            with self.assertRaises(ReaderError):
                response(item, 1, 2)

    def test_raw_signed_boundaries(self):
        for word, ticks in [(0, 0), (2048, 2048), (4095, 4095), (4096, 4096),
                            (32767, 32767), (32768, 0), (32769, -1), (65535, -32767)]:
            self.assertEqual(position(word), ticks)
            self.assertEqual(int.from_bytes(struct.pack("<H", word), "little"), word)


class ConfigTests(unittest.TestCase):
    def test_defaults_and_env_precedence(self):
        self.assertEqual(Config.load({}).ids, list(range(1, 7)))
        self.assertIsNone(Config.load({}).port)
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "config.json"
            path.write_text('{"port":"/dev/ttyUSB9","binding":{"id":"arm-a"}}')
            config = Config.load({"SO101_CONFIG": str(path), "SO101_PORT": "/dev/cu.fixture",
                                  "SO101_HTTP_PORT": "18876", "SO101_IDS": "[7,8,9,10,11,12]"})
            self.assertEqual(config.port, "/dev/cu.fixture")
            self.assertEqual(config.binding["id"], "arm-a")
            self.assertEqual(config.ids, [7, 8, 9, 10, 11, 12])

    def test_invalid_config(self):
        for env in [{"SO101_MODE": "sim"}, {"SO101_PORT": "/tmp/port"},
                    {"SO101_IDS": "[1,1,2,3,4,5]"}, {"SO101_IDS": "[1,2,3,4,5,254]"},
                    {"SO101_JOINTS": "[]"}, {"SO101_TIMEOUT": "nan"},
                    {"SO101_TIMEOUT": "10"}, {"SO101_HTTP_PORT": "0"},
                    {"SO101_HOST": "localhost"}, {"SO101_BINDING": "[]"},
                    {"SO101_BINDING": "{\"value\":NaN}"},
                    {"SO101_BINDING": "{\"value\":1e999}"},
                    {"SO101_MOCK_FAILED_IDS": "[1]"}, {"SO101_BAUD": "123456"}]:
            with self.assertRaises((ValueError, TypeError)):
                Config.load(env)

    def test_optional_calibration_never_blocks_raw(self):
        self.assertEqual(calibration_metadata(None)["state"], "not_configured")
        self.assertEqual(calibration_metadata("/missing/fixture.json")["state"], "load_failed")
        with tempfile.NamedTemporaryFile(mode="w") as source:
            source.write('{"shoulder_pan":{"id":1,"homing_offset":123}}')
            source.flush()
            meta = calibration_metadata(source.name)
            self.assertEqual(meta["state"], "file_loaded_unverified")
            self.assertFalse(meta["physically_verified"])
            self.assertFalse(meta["applied_to_readings"])
            self.assertEqual(len(meta["sha256"]), 64)


class IdentityTests(unittest.TestCase):
    def test_macos_usb_subtrees_match_selected_port_and_inherit_identity(self):
        port = "/dev/cu.fixture-target"
        tree = [
            {"USB Serial Number": "other", "IORegistryEntryChildren": [
                {"IOCalloutDevice": "/dev/cu.fixture-other"}]},
            {"USB Serial Number": "fixture-serial", "idVendor": 6790,
             "idProduct": 21971, "IORegistryEntryChildren": [
                 {"IORegistryEntryChildren": [{"IOCalloutDevice": port,
                                                "IODialinDevice": "/dev/tty.fixture-target"}]}]},
        ]
        reply = subprocess.CompletedProcess([], 0, stdout=plistlib.dumps(tree))
        with patch("identity.sys.platform", "darwin"), patch("identity.subprocess.run", return_value=reply) as run:
            found = adapter_metadata(port)
            self.assertEqual(found, {"source": "macos_ioreg", "robot_identity_proven": False,
                                     "serial_number": "fixture-serial", "vendor_id": 6790,
                                     "product_id": 21971})
            self.assertEqual(adapter_metadata("/dev/tty.fixture-target"), found)
            self.assertEqual(adapter_metadata("/dev/cu.missing")["source"], "unavailable")
            self.assertEqual(run.call_args.args[0],
                             ["/usr/sbin/ioreg", "-a", "-l", "-r", "-c", "IOUSBHostDevice"])
            self.assertEqual(run.call_args.kwargs["timeout"], 2)

    def test_macos_usb_lookup_timeout_stays_unverified(self):
        with patch("identity.sys.platform", "darwin"), patch("identity.subprocess.run",
                    side_effect=subprocess.TimeoutExpired("ioreg", 2)):
            self.assertEqual(adapter_metadata("/dev/cu.fixture"),
                             {"source": "unavailable", "robot_identity_proven": False})


class MonitorTests(unittest.TestCase):
    def test_optional_identity_failure_preserves_ownership_lifecycle(self):
        monitor = Monitor(Config(port="/dev/fixture"))
        with patch("reader.SerialTransport") as serial, patch("reader.adapter_metadata", side_effect=OSError("fixture")):
            with self.assertLogs("so101-reader", level="WARNING"):
                monitor.transport = monitor.make_transport()
            self.assertIs(monitor.transport, serial.return_value)
            monitor.release()
            serial.return_value.close.assert_called_once()
            self.assertIsNone(monitor.transport)

    def start(self, config=None, factory=None):
        monitor = Monitor(config or Config(mode="mock", interval=0.05), factory)
        monitor.start()
        self.addCleanup(monitor.shutdown)
        return monitor

    def test_mock_readiness_and_shutdown(self):
        monitor = self.start()
        wait_until(lambda: monitor.current()["observation_ready"])
        body = monitor.current()
        self.assertTrue(body["synthetic"])
        self.assertFalse(body["hardware_ready"])
        self.assertEqual(body["joints"][0]["position"]["ticks"], 1100)
        self.assertEqual(body["joints"][0]["model_number"], 777)
        monitor.shutdown()
        self.assertFalse(monitor.current()["process"]["healthy"])
        self.assertFalse(monitor.current()["observation_ready"])

    def test_partial_and_stale(self):
        monitor = self.start(Config(mode="mock", interval=60, mock_failed_ids=[2]))
        wait_until(lambda: monitor.current()["sequence"] == 1)
        body = monitor.current()
        self.assertEqual(body["connection"]["state"], "degraded")
        self.assertTrue(body["joints"][0]["fresh"])
        self.assertFalse(body["joints"][1]["fresh"])
        self.assertIsNone(body["joints"][1]["position"]["raw_word"])
        self.assertIsNone(body["joints"][1]["packet_error_bits"])
        body = monitor.current(now=time.monotonic() + 6)
        self.assertEqual(body["connection"]["state"], "stale")
        self.assertFalse(body["observation_ready"])
        self.assertTrue(all(not j["fresh"] for j in body["joints"]))

    def test_unknown_model_keeps_raw_word(self):
        class Unknown(MockTransport):
            def read(self, motor_id, address, count):
                return (b"\x01\x00", 0) if address == 3 else super().read(motor_id, address, count)
        monitor = self.start(factory=Unknown)
        wait_until(lambda: monitor.current()["sequence"] > 0)
        joint = monitor.current()["joints"][0]
        self.assertEqual(joint["position"]["raw_word"], 1100)
        self.assertIsNone(joint["position"]["ticks"])
        self.assertEqual(joint["errors"][0]["code"], "unsupported_model")

    def test_model_failure_does_not_hide_position(self):
        class Partial(MockTransport):
            def read(self, motor_id, address, count):
                if address == 3:
                    raise ReaderError("checksum", "bad fixture")
                return super().read(motor_id, address, count)
        monitor = self.start(factory=Partial)
        wait_until(lambda: monitor.current()["sequence"] > 0)
        joint = monitor.current()["joints"][0]
        self.assertEqual(joint["position"]["raw_word"], 1100)
        self.assertIsNone(joint["position"]["ticks"])
        self.assertEqual(joint["errors"][0]["code"], "checksum")

    def test_faults_are_not_ready(self):
        class Fault(MockTransport):
            def read(self, *args):
                data, _ = super().read(*args)
                return data, 4
        monitor = self.start(factory=Fault)
        wait_until(lambda: monitor.current()["sequence"] > 0)
        self.assertFalse(monitor.current()["observation_ready"])
        self.assertEqual(monitor.current()["joints"][0]["packet_error_bits"], 4)

    def test_silent_bus_possible_unpowered(self):
        monitor = self.start(Config(mode="mock", mock_failed_ids=list(range(1, 7))))
        wait_until(lambda: monitor.current()["sequence"] > 0)
        body = monitor.current()
        self.assertTrue(body["process"]["healthy"])
        self.assertEqual(body["connection"]["state"], "no_response")
        self.assertFalse(body["hardware_ready"])

    def test_missing_port_process_remains_healthy(self):
        monitor = self.start(Config())
        body = monitor.current()
        self.assertTrue(body["process"]["healthy"])
        self.assertEqual(body["connection"]["state"], "missing_port")

    def test_disconnect_invalidates_and_reconnects(self):
        class Disconnect(MockTransport):
            def __init__(self):
                super().__init__()
                self.reads = 0
                self.closed = False
            def read(self, *args):
                self.reads += 1
                if self.reads > 12:
                    raise ReaderError("disconnected", "synthetic unplug")
                return super().read(*args)
            def close(self):
                self.closed = True
        first = Disconnect()
        calls = []
        def factory():
            calls.append(True)
            return first if len(calls) == 1 else MockTransport()
        monitor = self.start(Config(mode="physical", port="/dev/fixture", interval=0.05, reconnect_delay=0.3), factory)
        wait_until(lambda: monitor.current()["hardware_ready"])
        wait_until(lambda: monitor.current()["connection"]["state"] == "disconnected")
        self.assertTrue(first.closed)
        self.assertTrue(all(j["position"]["raw_word"] is None for j in monitor.current()["joints"]))
        wait_until(lambda: len(calls) == 2 and monitor.current()["hardware_ready"])

    def test_open_errors(self):
        for code in ("permission_denied", "busy", "disconnected", "ownership_unknown"):
            def factory():
                raise ReaderError(code, "fixture open failure")
            monitor = self.start(Config(port="/dev/fixture"), factory)
            wait_until(lambda: monitor.current()["connection"]["state"] == code)
            self.assertTrue(monitor.current()["process"]["healthy"])
            self.assertFalse(monitor.current()["hardware_ready"])
            monitor.shutdown()


class TransportTests(unittest.TestCase):
    def setUp(self):
        self.master, self.slave = pty.openpty()
        self.port = os.ttyname(self.slave)
        self.addCleanup(self.close_pty)
        # The test owns the PTY; do not require lsof or pretend it is a USB device.
        self.owners = patch("transport.check_port_owners")
        self.owners.start()
        self.addCleanup(self.owners.stop)
        if sys.platform == "darwin":
            real_ioctl = fcntl.ioctl
            def ioctl(fd, code, *args):
                if code & 0xffff == 0x5402:  # IOSSIOSPEED has no PTY implementation.
                    return 0
                return real_ioctl(fd, code, *args)
            self.ioctl = patch("transport.fcntl.ioctl", side_effect=ioctl)
            self.ioctl.start()
            self.addCleanup(self.ioctl.stop)
        self.stop = threading.Event()
        self.transport = SerialTransport(self.port, 1_000_000, 0.2, self.stop)
        self.addCleanup(self.transport.close)

    def close_pty(self):
        for name in ("master", "slave"):
            fd = getattr(self, name)
            if fd is not None:
                os.close(fd)

    def test_fragmented_response_and_request_capture(self):
        captured = []
        def peer():
            captured.append(os.read(self.master, 8))
            answer = frame(1, b"\xff\xff", 8)
            for byte in answer:
                os.write(self.master, bytes([byte]))
                time.sleep(0.001)
        thread = threading.Thread(target=peer)
        thread.start()
        data, bits = self.transport.read(1, 3, 2)
        thread.join()
        self.assertEqual(captured, [request(1, 3, 2)])
        self.assertEqual(data, b"\xff\xff")
        self.assertEqual(bits, 8)

    def test_timeout_and_partial_response(self):
        def peer():
            os.read(self.master, 8)
            os.write(self.master, frame(1, b"\x09\x03")[:5])
        thread = threading.Thread(target=peer)
        thread.start()
        start = time.monotonic()
        with self.assertRaises(ReaderError) as caught:
            self.transport.read(1, 3, 2)
        thread.join()
        self.assertEqual(caught.exception.code, "timeout")
        self.assertLess(time.monotonic() - start, 0.5)

    def test_disconnect(self):
        os.close(self.master)
        self.master = None
        with self.assertRaises(ReaderError) as caught:
            self.transport.read(1, 3, 2)
        self.assertEqual(caught.exception.code, "disconnected")

    def test_busy_and_release(self):
        with self.assertRaises(ReaderError) as caught:
            SerialTransport(self.port, 1_000_000, 0.1, self.stop)
        self.assertEqual(caught.exception.code, "busy")
        self.transport.close()
        reopened = SerialTransport(self.port, 1_000_000, 0.1, self.stop)
        reopened.close()

    def test_cancel_is_bounded(self):
        self.stop.set()
        start = time.monotonic()
        with self.assertRaises(ReaderError) as caught:
            self.transport.read(1, 3, 2)
        self.assertEqual(caught.exception.code, "stopped")
        self.assertLess(time.monotonic() - start, 0.1)

    def test_arbitrary_writes_rejected_before_io(self):
        for address, count in [(40, 1), (42, 2), (31, 2)]:
            with self.assertRaises(ReaderError):
                self.transport.read(1, address, count)


class OwnershipTests(unittest.TestCase):
    def test_visible_preexisting_nonexclusive_owner(self):
        master, slave = pty.openpty()
        self.addCleanup(os.close, master)
        self.addCleanup(os.close, slave)
        port = os.ttyname(slave)
        child = subprocess.Popen([sys.executable, "-u", "-c",
                                  "import os,sys,time; fd=os.open(sys.argv[1],os.O_RDWR|os.O_NOCTTY); print('open'); time.sleep(15)", port],
                                 stdout=subprocess.PIPE, stderr=subprocess.PIPE)
        try:
            self.assertEqual(child.stdout.readline().strip(), b"open")
            with self.assertRaises(ReaderError) as caught:
                check_port_owners(port)
            self.assertEqual(caught.exception.code, "busy")
        finally:
            child.terminate()
            child.communicate(timeout=3)

    def test_termios_error_mapping(self):
        self.assertEqual(io_error(termios.error(errno.EIO, "fixture")).code, "disconnected")

    def test_preexisting_owner_and_missing_tool(self):
        for output, expected in [(f"p{os.getpid()}\np9999999\n".encode(), "busy")]:
            with patch("transport.subprocess.run", return_value=subprocess.CompletedProcess([], 0, output, b"")):
                with self.assertRaises(ReaderError) as caught:
                    check_port_owners("/dev/cu.fixture")
                self.assertEqual(caught.exception.code, expected)
        with patch("transport.subprocess.run", side_effect=FileNotFoundError):
            with self.assertRaises(ReaderError) as caught:
                check_port_owners("/dev/cu.fixture")
            self.assertEqual(caught.exception.code, "ownership_unknown")

    def test_os_error_mapping(self):
        for value, code in [(errno.EACCES, "permission_denied"), (errno.EBUSY, "busy"),
                            (errno.ENOENT, "disconnected"), (errno.EIO, "disconnected")]:
            self.assertEqual(io_error(OSError(value, "fixture")).code, code)

    def test_owner_timeout_blocks_open(self):
        master, slave = pty.openpty()
        self.addCleanup(os.close, master)
        self.addCleanup(os.close, slave)
        with patch("transport.subprocess.run", side_effect=subprocess.TimeoutExpired("lsof", 2)), \
                patch("transport.os.open") as opened:
            with self.assertRaises(ReaderError) as caught:
                SerialTransport(os.ttyname(slave), 1000000, .1, threading.Event())
            self.assertEqual(caught.exception.code, "ownership_unknown")
            self.assertIn("timed out", str(caught.exception))
            opened.assert_not_called()

    def test_cancellation_during_owner_check_blocks_open(self):
        master, slave = pty.openpty()
        self.addCleanup(os.close, master)
        self.addCleanup(os.close, slave)
        stop = threading.Event()
        with patch("transport.check_port_owners", side_effect=lambda _: stop.set()), \
                patch("transport.os.open") as opened:
            with self.assertRaises(ReaderError) as caught:
                SerialTransport(os.ttyname(slave), 1000000, .1, stop)
            self.assertEqual(caught.exception.code, "stopping")
            opened.assert_not_called()


class APITests(unittest.TestCase):
    def test_numeric_listener_does_not_require_reverse_dns(self):
        config = Config(mode="mock", http_port=0)
        monitor = Monitor(config)
        with patch("socket.getfqdn", side_effect=AssertionError("Unexpected reverse DNS")):
            server = server_for(config, monitor)
        try:
            self.assertEqual(server.server_name, "127.0.0.1")
            self.assertGreater(server.server_port, 0)
            self.assertFalse(monitor.thread.is_alive())
        finally:
            server.server_close()

    def test_worker_failure_fails_health(self):
        def factory():
            raise RuntimeError("unexpected worker error fixture")
        monitor = Monitor(Config(mode="mock"), factory)
        with self.assertLogs("so101-reader", level="ERROR"):
            monitor.start()
            monitor.thread.join(timeout=2)
        self.assertFalse(monitor.current()["process"]["healthy"])
        self.assertFalse(monitor.current()["observation_ready"])
        self.assertEqual(monitor.current()["connection"]["state"], "worker_failed")

    def test_occupied_http_port_does_not_start_reader(self):
        with socket.socket() as sock:
            sock.bind(("127.0.0.1", 0))
            sock.listen()
            monitor = Monitor(Config(http_port=sock.getsockname()[1]))
            with self.assertRaises(OSError):
                server_for(monitor.config, monitor)
            self.assertFalse(monitor.thread.is_alive())
            self.assertIsNone(monitor.transport)

    def test_health_readiness_and_methods(self):
        config = Config(mode="mock", http_port=0)
        monitor = Monitor(config)
        server = server_for(config, monitor)
        worker = threading.Thread(target=server.serve_forever, kwargs={"poll_interval": 0.02})
        monitor.start()
        worker.start()
        base = f"http://127.0.0.1:{server.server_port}"
        try:
            wait_until(lambda: monitor.current()["observation_ready"])
            with urllib.request.urlopen(base + "/healthz") as reply:
                self.assertTrue(json.load(reply)["healthy"])
            with urllib.request.urlopen(base + "/v1/observations") as reply:
                self.assertEqual(reply.headers["Cache-Control"], "no-store")
                body = json.load(reply)
                self.assertEqual(body["schema_version"], 1)
                self.assertTrue(body["synthetic"])
            for path, method, status in [("/readyz", "GET", 503), ("/command", "GET", 404),
                                         ("/v1/observations", "POST", 501), ("/v1/observations", "PUT", 501)]:
                with self.assertRaises(urllib.error.HTTPError) as caught:
                    urllib.request.urlopen(urllib.request.Request(base + path, method=method))
                self.assertEqual(caught.exception.code, status)
                caught.exception.close()
        finally:
            server.shutdown()
            worker.join()
            server.server_close()
            monitor.shutdown()

    def test_real_process_sigterm(self):
        with socket.socket() as sock:
            sock.bind(("127.0.0.1", 0))
            port = sock.getsockname()[1]
        env = {k: v for k, v in os.environ.items() if not k.startswith("SO101_")}
        env.update(SO101_MODE="mock", SO101_HTTP_PORT=str(port))
        proc = subprocess.Popen([sys.executable, str(Path(__file__).with_name("reader.py"))],
                                env=env, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
        try:
            def ready():
                if proc.poll() is not None:
                    _, stderr = proc.communicate()
                    self.fail("Child exited before readiness: " + stderr.decode())
                try:
                    with urllib.request.urlopen(f"http://127.0.0.1:{port}/healthz", timeout=0.2) as reply:
                        return json.load(reply)["healthy"]
                except (OSError, urllib.error.URLError):
                    return False
            try:
                wait_until(ready, timeout=10)
            except AssertionError:
                proc.kill()
                _, stderr = proc.communicate(timeout=3)
                self.fail("Child did not serve health within 10s: " + stderr.decode())
            proc.send_signal(signal.SIGTERM)
            _, stderr = proc.communicate(timeout=3)
            self.assertEqual(proc.returncode, 0, stderr.decode())
            self.assertIn(b"serial ownership released", stderr)
        finally:
            if proc.poll() is None:
                proc.kill()
                proc.communicate()


if __name__ == "__main__":
    unittest.main()
