#!/usr/bin/env python3
"""SO-101 read-only hardware reader; Python 3.10+, standard library only."""

import copy
import hashlib
import ipaddress
import json
import logging
import math
import os
import signal
import threading
import time
from dataclasses import dataclass, field, fields
from datetime import datetime, timezone
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from socketserver import TCPServer

from protocol import MODELS, ReaderError, position
from identity import adapter_metadata
from transport import MockTransport, SerialTransport

LOG = logging.getLogger("so101-reader")
JOINTS = ["shoulder_pan", "shoulder_lift", "elbow_flex", "wrist_flex", "wrist_roll", "gripper"]


def reject_constant(value):
    raise ValueError("Non-finite JSON number: " + value)


def finite_float(value):
    number = float(value)
    if not math.isfinite(number):
        raise ValueError("Non-finite JSON number: " + value)
    return number


def timestamp():
    return datetime.now(timezone.utc).isoformat(timespec="milliseconds").replace("+00:00", "Z")


@dataclass
class Config:
    mode: str = "physical"
    port: str | None = None
    baud: int = 1_000_000
    ids: list = field(default_factory=lambda: list(range(1, 7)))
    joints: list = field(default_factory=lambda: JOINTS.copy())
    host: str = "127.0.0.1"
    http_port: int = 8876
    interval: float = 0.5
    timeout: float = 0.1
    stale_after: float = 5.0
    reconnect_delay: float = 1.0
    binding: dict = field(default_factory=dict)
    calibration_file: str | None = None
    mock_failed_ids: list = field(default_factory=list)

    @classmethod
    def load(cls, env=None):
        env = os.environ if env is None else env
        values = {}
        if env.get("SO101_CONFIG"):
            with open(env["SO101_CONFIG"], encoding="utf-8") as source:
                values = json.load(source, parse_constant=reject_constant, parse_float=finite_float)
            if not isinstance(values, dict):
                raise ValueError("SO101_CONFIG must contain a JSON object")
        for item in fields(cls):
            key = "SO101_" + item.name.upper()
            if key in env:
                value = env[key]
                if item.name in {"baud", "http_port"}:
                    value = int(value)
                elif item.name in {"interval", "timeout", "stale_after", "reconnect_delay"}:
                    value = float(value)
                elif item.name in {"ids", "joints", "binding", "mock_failed_ids"}:
                    value = json.loads(value, parse_constant=reject_constant, parse_float=finite_float)
                elif item.name in {"port", "calibration_file"} and not value:
                    value = None
                values[item.name] = value
        result = cls(**values)
        result.validate()
        return result

    def validate(self):
        if self.mode not in {"physical", "mock"}:
            raise ValueError("mode must be physical or mock")
        if self.port is not None and (not isinstance(self.port, str) or not self.port.startswith("/dev/")
                                      or "\x00" in self.port):
            raise ValueError("port must be an explicit /dev/ path or null")
        if type(self.baud) is not int or self.baud not in {9600, 19200, 38400, 57600, 115200, 500000, 1000000}:
            raise ValueError("unsupported baud (default 1000000)")
        if (not isinstance(self.ids, list) or len(self.ids) != 6
                or any(type(i) is not int or not 1 <= i <= 253 for i in self.ids)
                or len(set(self.ids)) != 6):
            raise ValueError("ids must contain six unique unicast IDs in 1..253")
        if (not isinstance(self.joints, list) or len(self.joints) != 6
                or any(not isinstance(j, str) or not j.strip() for j in self.joints)
                or len(set(self.joints)) != 6):
            raise ValueError("joints must contain six unique nonempty names")
        # Numeric IPv4 makes the bind deterministic and avoids DNS/wildcard surprises.
        ipaddress.IPv4Address(self.host)
        if type(self.http_port) is not int or not 1 <= self.http_port <= 65535:
            raise ValueError("http_port must be 1..65535")
        for name, low, high in [("interval", 0.05, 60), ("timeout", 0.02, 1),
                                ("stale_after", 0.1, 300), ("reconnect_delay", 0.1, 60)]:
            value = getattr(self, name)
            if type(value) not in {int, float} or not math.isfinite(value) or not low <= value <= high:
                raise ValueError(f"{name} must be finite in {low}..{high} seconds")
        if not isinstance(self.binding, dict):
            raise ValueError("binding must be an object of operator-supplied metadata")
        if self.calibration_file is not None and not isinstance(self.calibration_file, str):
            raise ValueError("calibration_file must be a path or null")
        if (not isinstance(self.mock_failed_ids, list)
                or any(type(i) is not int or i not in self.ids for i in self.mock_failed_ids)):
            raise ValueError("mock_failed_ids must be a subset of ids")
        if self.mode != "mock" and self.mock_failed_ids:
            raise ValueError("mock_failed_ids is only allowed in mock mode")


def calibration_metadata(path):
    result = {"state": "not_configured", "physically_verified": False, "applied_to_readings": False}
    if not path:
        return result
    try:
        with open(path, "rb") as source:
            raw = source.read(1_048_577)
        if len(raw) > 1_048_576:
            raise ValueError("Calibration JSON exceeds 1 MiB")
        entries = json.loads(raw, parse_constant=reject_constant, parse_float=finite_float)
        if not isinstance(entries, dict):
            raise ValueError("Calibration metadata must be an object")
        result.update(state="file_loaded_unverified", sha256=hashlib.sha256(raw).hexdigest(), entries=entries)
    except (OSError, ValueError) as exc:
        result.update(state="load_failed", error=str(exc))
    return result


def empty_joint(motor_id, name):
    return {"id": motor_id, "name": name, "model_number": None, "model": None,
            "position": {"raw_word": None, "ticks": None, "unit": "encoder_tick",
                         "encoding": None,
                         "source": "present_position_register_56", "calibrated": False},
            "voltage": None, "voltage_unit": "V", "temperature": None, "temperature_unit": "degC",
            "packet_error_bits": None, "packet_error_bits_unit": "bitmask",
            "observed_at": None, "age_seconds": None, "fresh": False, "errors": [], "_mono": None}


class Monitor:
    def __init__(self, config, factory=None):
        self.config = config
        self.stop = threading.Event()
        self.lock = threading.Lock()
        self.transport = None
        self.factory = factory or self.make_transport
        self.thread = threading.Thread(target=self.run, name="motor-reader", daemon=False)
        self.sequence = 0
        self.state = "connecting" if config.port or config.mode == "mock" else "missing_port"
        self.error = None
        self.joints = [empty_joint(i, j) for i, j in zip(config.ids, config.joints)]
        self.attempted_at = None
        self.completed_at = None
        self.calibration = calibration_metadata(config.calibration_file)
        self.adapter = {"source": "unavailable", "robot_identity_proven": False}

    def make_transport(self):
        if self.config.mode == "mock":
            return MockTransport(self.config.mock_failed_ids)
        transport = SerialTransport(self.config.port, self.config.baud, self.config.timeout, self.stop)
        self.adapter = {"source": "unavailable", "robot_identity_proven": False}
        try:
            self.adapter = adapter_metadata(self.config.port)
        except Exception:
            # Metadata is optional; never lose an already-owned descriptor if
            # host I/O Registry/sysfs inspection fails unexpectedly.
            LOG.warning("USB adapter metadata unavailable", exc_info=True)
        return transport

    def start(self):
        self.thread.start()

    def shutdown(self):
        self.stop.set()
        if self.thread.ident is not None:
            self.thread.join()  # serial waits observe stop at least every 20 ms

    def release(self):
        if self.transport is not None:
            owned = self.transport
            self.transport = None
            owned.close()
            self.adapter = {"source": "unavailable", "robot_identity_proven": False}

    def sample(self):
        """Publish each joint immediately so a slow scan never hides its age."""
        with self.lock:
            self.attempted_at = timestamp()
        if self.transport is None:
            self.transport = self.factory()
        for index, (motor_id, name) in enumerate(zip(self.config.ids, self.config.joints)):
            if self.stop.is_set():
                return
            motor = empty_joint(motor_id, name)
            status_bits = None
            for address, count in ((3, 2), (56, 8)):
                try:
                    data, bits = self.transport.read(motor_id, address, count)
                    status_bits = (status_bits or 0) | bits
                    if address == 3:
                        motor["model_number"] = int.from_bytes(data, "little")
                        motor["model"] = MODELS.get(motor["model_number"])
                        if motor["model"]:
                            motor["position"]["encoding"] = "sts_sms_sign_magnitude_bit15"
                    else:
                        word = int.from_bytes(data[:2], "little")
                        motor["position"]["raw_word"] = word
                        motor["position"]["ticks"] = position(word) if motor["model"] else None
                        motor["voltage"], motor["temperature"] = data[6] / 10, data[7]
                        motor["observed_at"], motor["_mono"] = timestamp(), time.monotonic()
                except ReaderError as exc:
                    motor["errors"].append(exc.json())
                    if exc.code not in {"timeout", "protocol", "checksum"}:
                        raise
            motor["packet_error_bits"] = status_bits
            if motor["model_number"] is not None and motor["model"] is None:
                motor["errors"].append({"code": "unsupported_model", "message": "Raw word retained; tick decoding requires a known STS/SMS model"})
            if status_bits:
                motor["errors"].append({"code": "motor_fault", "message": f"Motor response error bits: 0x{status_bits:02x}"})
            with self.lock:
                self.joints[index] = motor
        with self.lock:
            self.sequence += 1
            self.completed_at = timestamp()
            responding = sum(j["position"]["raw_word"] is not None for j in self.joints)
            self.state = "no_response" if responding == 0 else "ready" if all(
                not j["errors"] and j["position"]["ticks"] is not None for j in self.joints) else "degraded"
            self.error = None
        if not responding:
            self.release()

    def run(self):
        last_error = None
        try:
            while not self.stop.is_set():
                if self.config.mode == "physical" and not self.config.port:
                    self.stop.wait(self.config.reconnect_delay)
                    continue
                try:
                    self.sample()
                    last_error = None
                except ReaderError as exc:
                    self.release()
                    with self.lock:
                        self.state, self.error = exc.code, exc.json()
                        # An I/O failure invalidates ALL previous hardware values.
                        self.joints = [empty_joint(i, j) for i, j in zip(self.config.ids, self.config.joints)]
                    if (exc.code, str(exc)) != last_error:
                        LOG.warning("hardware state=%s: %s", exc.code, exc)
                        last_error = (exc.code, str(exc))
                with self.lock:
                    state = self.state
                delay = self.config.interval if state in {"ready", "degraded"} else self.config.reconnect_delay
                self.stop.wait(delay)
        except Exception:
            # Unexpected worker failures must fail /healthz instead of looking ready.
            LOG.exception("Reader worker failed")
            with self.lock:
                self.state = "worker_failed"
        finally:
            self.release()

    def current(self, now=None):
        now = time.monotonic() if now is None else now
        with self.lock:
            joints = copy.deepcopy(self.joints)
            state, error = self.state, copy.deepcopy(self.error)
            sequence, attempted, completed = self.sequence, self.attempted_at, self.completed_at
        for motor in joints:
            mono = motor.pop("_mono")
            age = max(0, now - mono) if mono is not None else None
            motor["age_seconds"] = age
            motor["fresh"] = age is not None and age <= self.config.stale_after and motor["position"]["raw_word"] is not None
        observation_ready = all(j["fresh"] and not j["errors"] and j["position"]["ticks"] is not None for j in joints)
        if state in {"ready", "degraded"} and any(j["observed_at"] and not j["fresh"] for j in joints):
            state = "stale"
        if self.stop.is_set():
            state = "stopping"
            observation_ready = False
        worker_alive = self.thread.is_alive()
        observation_ready = observation_ready and worker_alive and state in {"ready", "degraded"}
        return {"schema_version": 1, "generated_at": timestamp(), "sequence": sequence,
                "source": "mock" if self.config.mode == "mock" else "physical_serial",
                "synthetic": self.config.mode == "mock", "read_only": True,
                "process": {"healthy": worker_alive and not self.stop.is_set(), "worker_alive": worker_alive},
                "observation_ready": observation_ready,
                "hardware_ready": observation_ready and self.config.mode == "physical",
                "connection": {"state": state, "port": self.config.port,
                               "resolved_port": os.path.realpath(self.config.port) if self.config.port else None,
                               "baud": self.config.baud, "binding": self.config.binding,
                               "adapter": self.adapter,
                               "identity_verified": False, "error": error},
                "scan": {"attempted_at": attempted, "completed_at": completed,
                         "simultaneous": False, "stale_after_seconds": self.config.stale_after},
                "calibration": self.calibration, "joints": joints}


class ReaderHTTPServer(ThreadingHTTPServer):
    def server_bind(self):
        # Config accepts numeric IPv4 only. HTTPServer's reverse DNS lookup is
        # unnecessary and can delay offline/macOS startup before health is served.
        TCPServer.server_bind(self)
        self.server_name, self.server_port = self.server_address


def server_for(config, monitor):
    class Handler(BaseHTTPRequestHandler):
        def do_GET(self):
            if self.path not in {"/healthz", "/readyz", "/v1/observations"}:
                self.send_error(404)
                return
            body = monitor.current()
            status = 200
            if self.path == "/healthz":
                body = {"schema_version": 1, **body["process"]}
                status = 200 if body["healthy"] else 503
            elif self.path == "/readyz":
                body = {"schema_version": 1, "source": body["source"],
                        "observation_ready": body["observation_ready"], "hardware_ready": body["hardware_ready"],
                        "state": body["connection"]["state"]}
                status = 200 if body["hardware_ready"] else 503
            data = json.dumps(body, allow_nan=False, separators=(",", ":")).encode()
            self.send_response(status)
            self.send_header("Content-Type", "application/json")
            self.send_header("Cache-Control", "no-store")
            self.send_header("Content-Length", str(len(data)))
            self.end_headers()
            self.wfile.write(data)

        def log_message(self, fmt, *args):
            LOG.debug(fmt, *args)

    server = ReaderHTTPServer((config.host, config.http_port), Handler)
    server.timeout = 0.2
    server.daemon_threads = True
    return server


def main():
    logging.basicConfig(level=logging.INFO, format="%(asctime)s %(levelname)s %(message)s")
    config = Config.load()
    monitor = Monitor(config)
    server = server_for(config, monitor)  # fail before opening hardware if port is occupied
    for sig in (signal.SIGINT, signal.SIGTERM):
        signal.signal(sig, lambda *_: monitor.stop.set())
    monitor.start()
    LOG.info("API http://%s:%s source=%s read_only=true", config.host, config.http_port, config.mode)
    try:
        while not monitor.stop.is_set():
            server.handle_request()
    finally:
        server.server_close()
        monitor.shutdown()
        LOG.info("Stopped; serial ownership released")


if __name__ == "__main__":
    main()
