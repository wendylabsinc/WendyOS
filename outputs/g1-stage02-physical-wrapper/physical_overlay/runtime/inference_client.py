"""Persistent client for the motion-zero inference service.

The physical wrapper deliberately learns policy identity and reference frame 0
from the active inference bundle.  Compatible simulator checkpoints therefore
do not require a physical-wrapper rebuild or new checkpoint/update latches.
"""
from __future__ import annotations

import http.client
import json
import secrets
import socket
import threading
from types import SimpleNamespace
from typing import Any
from urllib.parse import urlsplit

import numpy as np

from .async_vision import VisionUnavailable


class InferenceClient:
    def __init__(self, url: str) -> None:
        parsed = urlsplit(url)
        if parsed.scheme != "http" or not parsed.hostname or parsed.path not in {"", "/"}:
            raise ValueError("G1_POLICY_INFERENCE_URL must be an HTTP origin")
        self.host = parsed.hostname
        self.port = parsed.port or 80
        self.lock = threading.RLock()
        self.connection: http.client.HTTPConnection | None = None
        self.session_id: str | None = None

    def request(self, method: str, path: str, value: dict[str, Any] | None = None) -> dict[str, Any]:
        payload = None if value is None else json.dumps(value, separators=(",", ":"))
        with self.lock:
            for attempt in range(2):
                try:
                    if self.connection is None:
                        self.connection = http.client.HTTPConnection(self.host, self.port, timeout=3.0)
                        self.connection.connect()
                        assert self.connection.sock is not None
                        self.connection.sock.setsockopt(socket.IPPROTO_TCP, socket.TCP_NODELAY, 1)
                    self.connection.request(
                        method,
                        path,
                        body=payload,
                        headers={"Content-Type": "application/json"} if payload is not None else {},
                    )
                    response = self.connection.getresponse()
                    body = response.read()
                    result = json.loads(body)
                    if response.status != 200:
                        raise RuntimeError(result.get("error") or f"inference HTTP {response.status}")
                    return result
                except (OSError, http.client.HTTPException):
                    if self.connection is not None:
                        self.connection.close()
                    self.connection = None
                    if attempt:
                        raise
            raise AssertionError("unreachable")

    def close(self) -> None:
        with self.lock:
            if self.connection is not None:
                self.connection.close()
                self.connection = None


class RemoteEpisode:
    def __init__(self, client: InferenceClient) -> None:
        self.client = client
        self.policy = SimpleNamespace(vision_device="cuda", device="cpu", close=lambda: None)
        self.active_policy: dict[str, Any] = {}
        self.reference_frame0 = np.zeros(43, dtype=float)
        self.joint_names: tuple[str, ...] = ()
        self.reference = SimpleNamespace(references=range(0))
        self.refresh_contract()

    def refresh_contract(self) -> dict[str, Any]:
        value = self.client.request("GET", "/contract")
        if value.get("schema") != "wendy.g1.reference-residual-active-contract.v1":
            raise RuntimeError("inference contract schema changed")
        names = tuple(str(item) for item in value.get("joint_names", ()))
        frame0 = np.asarray(value.get("reference_frame0_q_43"), dtype=float)
        count = value.get("reference_frames")
        policy = value.get("active_policy")
        if (
            len(names) != 43
            or len(set(names)) != 43
            or frame0.shape != (43,)
            or not np.isfinite(frame0).all()
            or isinstance(count, bool)
            or not isinstance(count, int)
            or not 1 <= count <= 100_000
            or not isinstance(policy, dict)
        ):
            raise RuntimeError("inference returned an invalid active contract")
        self.joint_names = names
        self.reference_frame0 = frame0
        self.reference = SimpleNamespace(references=range(count))
        self.active_policy = dict(policy)
        return value

    def reset_episode(self, *, started_at_ns: int) -> dict[str, Any]:
        self.refresh_contract()
        self.client.session_id = secrets.token_hex(16)
        return self.client.request(
            "POST", "/reset", {"session_id": self.client.session_id, "started_at_ns": started_at_ns}
        )

    def propose(self, state: Any, *, control_at_ns: int) -> dict[str, Any]:
        if self.client.session_id is None:
            raise RuntimeError("remote policy session has not been reset")
        try:
            return self.client.request(
                "POST",
                "/propose",
                {
                    "session_id": self.client.session_id,
                    "q_43": list(state.q),
                    "dq_43": list(state.dq),
                    "sampled_at_ns": int(state.sampled_at_ns),
                    "control_at_ns": int(control_at_ns),
                },
            )
        except RuntimeError as exc:
            if "VisionUnavailable:" in str(exc):
                raise VisionUnavailable(str(exc).split("VisionUnavailable:", 1)[1].strip()) from exc
            raise


class RemoteCamera:
    def __init__(self, client: InferenceClient) -> None:
        self.client = client

    def preflight(self) -> dict[str, Any]:
        return self.client.request("POST", "/preflight", {"check": True})

    def activate(self) -> None:
        if self.client.session_id is None:
            raise RuntimeError("remote policy session has not been reset")
        self.client.request("POST", "/activate", {"session_id": self.client.session_id})

    def deactivate(self) -> None:
        if self.client.session_id is not None:
            session_id, self.client.session_id = self.client.session_id, None
            self.client.request("POST", "/deactivate", {"session_id": session_id})

    def status(self) -> dict[str, Any]:
        try:
            health = self.client.request("GET", "/health")
            return {
                **health.get("camera", {}),
                "active_policy": health.get("active_policy"),
                "inference_healthy": health.get("healthy") is True,
            }
        except Exception as exc:
            return {
                "latest": {},
                "last_error": f"{type(exc).__name__}: {exc}",
                "timing": {},
                "inference_healthy": False,
            }

    def close(self) -> None:
        try:
            self.deactivate()
        finally:
            self.client.close()


def remote_components(url: str, _bundle: Any = None) -> tuple[RemoteEpisode, RemoteCamera]:
    client = InferenceClient(url)
    status = client.request("GET", "/health")
    if status.get("healthy") is not True or status.get("motion_capability") is not False:
        raise RuntimeError("remote inference service is not motion-zero healthy")
    return RemoteEpisode(client), RemoteCamera(client)
