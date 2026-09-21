"""Persistent localhost client/proxies for the DDS-free policy service."""
from __future__ import annotations

import http.client
import json
import secrets
import socket
import threading
from pathlib import Path
from types import SimpleNamespace
from typing import Any
from urllib.parse import urlsplit

import numpy as np

from .async_vision import VisionUnavailable
from .contracts import EXPECTED_CHECKPOINT_SHA256, INFERENCE_SCHEMA


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
                        self.connection.sock.setsockopt(
                            socket.IPPROTO_TCP, socket.TCP_NODELAY, 1
                        )
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
                        self.close()
                        raise RuntimeError(result.get("error") or f"inference HTTP {response.status}")
                    return result
                except (OSError, http.client.HTTPException):
                    if self.connection is not None:
                        self.connection.close()
                    self.connection = None
                    if attempt or method != "GET":
                        raise
            raise AssertionError("unreachable")

    def close(self) -> None:
        with self.lock:
            if self.connection is not None:
                self.connection.close()
                self.connection = None


class RemoteEpisode:
    def __init__(self, client: InferenceClient, bundle: Path) -> None:
        self.client = client
        with np.load(bundle / "reference-contract.npz", allow_pickle=False) as values:
            self.joint_names = tuple(str(value) for value in values["joint_names"].tolist())
            count = len(values["reference_joint_targets_43"])
        self.reference = SimpleNamespace(references=range(count))
        self.policy = SimpleNamespace(vision_device="cuda", device="cpu", close=lambda: None)

    def reset_episode(self, *, started_at_ns: int) -> dict[str, Any]:
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
            # Preserve the physical runner's existing retry semantics across
            # the process boundary. Every other remote error remains fatal.
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
            self.client.request("POST", "/deactivate", {"session_id": self.client.session_id})
            self.client.session_id = None

    def status(self) -> dict[str, Any]:
        try:
            return self.client.request("GET", "/health").get("camera", {})
        except Exception as exc:
            return {"latest": {}, "last_error": f"{type(exc).__name__}: {exc}", "timing": {}}

    def close(self) -> None:
        try:
            self.deactivate()
        finally:
            self.client.close()


def remote_components(url: str, bundle: Path) -> tuple[RemoteEpisode, RemoteCamera]:
    client = InferenceClient(url)
    try:
        status = client.request("GET", "/health")
        if status.get("healthy") is not True or status.get("motion_capability") is not False:
            raise RuntimeError("remote inference service is not motion-zero healthy")
        episode = RemoteEpisode(client, bundle)
        if (status.get("schema") != INFERENCE_SCHEMA
                or status.get("checkpoint_sha256") != EXPECTED_CHECKPOINT_SHA256
                or status.get("joint_names") != list(episode.joint_names)):
            raise RuntimeError("remote inference identity contract mismatch")
        return episode, RemoteCamera(client)
    except BaseException:
        client.close()
        raise
