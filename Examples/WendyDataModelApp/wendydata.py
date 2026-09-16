"""Minimal Wendy Data socket client and record helpers.

This module is standard-library only so the record framing and the
uncertainty formula are unit-testable without a camera, a model, or a
running agent. The protocol matches the agent's application-record socket
(go/internal/agent/services/app_data_socket.go):

  - Unix stream socket, path in the WENDY_DATA_SOCKET environment variable
    (the agent injects /run/wendy/data/data.sock for apps with the
    "episode-write" entitlement).
  - Each record is a 4-byte big-endian length prefix followed by a JSON
    document of at most 64 KiB.
  - Records carry {"version": 1, "type": "event"|"prediction", ...} plus
    CLOCK_BOOTTIME nanoseconds and the kernel boot id so the agent can
    place them on the device timeline.
  - A prediction may carry an optional "inputs" list naming the harness
    samples it was computed from, as [{"source_id", "sample_id"}]. The
    identifiers come from the agent-fed node's buffer timestamps;
    the agent records the same identifiers in the episode, so an outcome
    can be paired with the exact input bytes offline. The field is
    optional and a prediction without it is still accepted.
  - Every record is acknowledged with {"version": 1, "state": ...} where
    state is "buffered", "recorded", or "rejected".
"""

from __future__ import annotations

import json
import logging
import os
import socket
import struct
import time

log = logging.getLogger("wendydata")

MAX_RECORD_BYTES = 64 * 1024
DEFAULT_SOCKET_PATH = "/run/wendy/data/data.sock"

ACK_OK_STATES = ("buffered", "recorded")


def socket_path() -> str:
    """The agent-injected data socket path, with the documented default."""
    return os.environ.get("WENDY_DATA_SOCKET", DEFAULT_SOCKET_PATH)


def boot_clock_nanos() -> int:
    """Nanoseconds on the device boot clock (CLOCK_BOOTTIME).

    CLOCK_BOOTTIME exists on Linux, which is where this app runs; on other
    development platforms fall back to CLOCK_MONOTONIC so the pure helpers
    stay testable.
    """
    clock = getattr(time, "CLOCK_BOOTTIME", time.CLOCK_MONOTONIC)
    return time.clock_gettime_ns(clock)


def read_boot_id() -> str:
    """The kernel boot id, or an empty string off-Linux."""
    try:
        with open("/proc/sys/kernel/random/boot_id", "r", encoding="ascii") as f:
            return f.read().strip()
    except OSError:
        return ""


def uncertainty_score(confidences, threshold: float) -> float:
    """Per-frame model uncertainty in 0..1.

    Formula: 1 - max(confidence) over detections at or above the
    confidence threshold; 1.0 when nothing was detected above the
    threshold. A frame the model is sure about scores near 0, an empty or
    ambiguous frame scores near 1. Being 1 minus a confidence, it does not
    separate "unsure about what it saw" from "recognised nothing": both sit
    at 1.0. Record it, query it, but see README "The uncertainty formula"
    before arming a model.uncertainty trigger on it.
    """
    best = 0.0
    for c in confidences:
        c = float(c)
        if c >= threshold and c > best:
            best = c
    if best <= 0.0:
        return 1.0
    return max(0.0, min(1.0, 1.0 - best))


def frame_record(payload: dict) -> bytes:
    """Encode one record as a length-prefixed JSON frame."""
    body = json.dumps(payload, separators=(",", ":")).encode("utf-8")
    if len(body) > MAX_RECORD_BYTES:
        raise ValueError(f"record is {len(body)} bytes; the protocol caps records at 64 KiB")
    return struct.pack(">I", len(body)) + body


def _base_record(record_type: str) -> dict:
    return {
        "version": 1,
        "type": record_type,
        "client_boottime_nanos": boot_clock_nanos(),
        "boot_id": read_boot_id(),
    }


MAX_INPUT_REFS = 32


def build_prediction(
    model: str,
    model_version: str,
    uncertainty: float,
    detections,
    attributes: dict | None = None,
    inputs=None,
) -> dict:
    """A "prediction" record. The agent requires the model name; the
    uncertainty rides in attributes where campaign model.uncertainty
    triggers read it.

    `inputs` binds the outcome to the harness samples the model consumed,
    as an iterable of {"source_id", "sample_id"} mappings. Pass the value
    of a frame's input_refs(). It is optional — a
    prediction with no inputs is accepted and recorded, and counted in the
    episode manifest as an outcome whose input is unknown."""
    record = _base_record("prediction")
    record["model"] = model
    attrs = {
        "model_version": model_version,
        "uncertainty": float(uncertainty),
        "detections": list(detections),
    }
    if attributes:
        attrs.update(attributes)
    record["attributes"] = attrs
    if inputs:
        refs = [{"source_id": str(r["source_id"]), "sample_id": int(r["sample_id"])} for r in inputs]
        if len(refs) > MAX_INPUT_REFS:
            # The agent rejects more than this; keep the newest references
            # rather than losing the whole record.
            refs = refs[-MAX_INPUT_REFS:]
        record["inputs"] = refs
    return record


def build_event(name: str, attributes: dict | None = None) -> dict:
    """An "event" record. The agent requires the event name."""
    record = _base_record("event")
    record["name"] = name
    if attributes:
        record["attributes"] = dict(attributes)
    return record


class DataSocketClient:
    """A small reconnect-and-retry client for the data socket.

    send() delivers one record and returns the acknowledged state
    ("buffered", "recorded", or "rejected"), or None when the socket is
    unavailable and the record was dropped after one reconnect attempt.
    The agent enforces its own per-connection rate limit (200 records per
    second); callers should stay far below it.
    """

    def __init__(self, path: str | None = None, timeout: float = 2.0):
        self.path = path or socket_path()
        self.timeout = timeout
        self._sock: socket.socket | None = None

    def _connect(self) -> None:
        sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        sock.settimeout(self.timeout)
        sock.connect(self.path)
        self._sock = sock

    def close(self) -> None:
        if self._sock is not None:
            try:
                self._sock.close()
            finally:
                self._sock = None

    def _read_ack(self) -> dict:
        header = self._recv_exact(4)
        (length,) = struct.unpack(">I", header)
        if length == 0 or length > MAX_RECORD_BYTES:
            raise ConnectionError(f"ack length {length} out of range")
        return json.loads(self._recv_exact(length))

    def _recv_exact(self, n: int) -> bytes:
        assert self._sock is not None
        buf = b""
        while len(buf) < n:
            chunk = self._sock.recv(n - len(buf))
            if not chunk:
                raise ConnectionError("data socket closed by agent")
            buf += chunk
        return buf

    def _send_once(self, frame: bytes) -> tuple[str, str]:
        if self._sock is None:
            self._connect()
        assert self._sock is not None
        self._sock.sendall(frame)
        ack = self._read_ack()
        return str(ack.get("state", "")), str(ack.get("error", ""))

    def send(self, record: dict) -> str | None:
        frame = frame_record(record)
        for attempt in (1, 2):
            try:
                state, error = self._send_once(frame)
            except (OSError, ConnectionError, json.JSONDecodeError) as exc:
                self.close()
                if attempt == 1:
                    continue
                log.warning("data socket unavailable, dropped %s record: %s", record.get("type"), exc)
                return None
            if state in ACK_OK_STATES:
                return state
            log.warning("agent rejected %s record: %s", record.get("type"), error or "unspecified")
            return state
        return None
