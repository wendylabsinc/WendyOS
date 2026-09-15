"""Small authenticated HTTP collective for Runpod Pods without peer networking."""

from __future__ import annotations

import hashlib
import os
import threading
import time
import urllib.error
import urllib.request
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

import numpy as np
import torch


class _State:
    def __init__(self, world_size: int, token: str):
        self.world_size = world_size
        self.token = token
        self.condition = threading.Condition()
        self.values: dict[str, dict[int, bytes]] = {}
        self.results: dict[str, bytes] = {}
        self.delivered: dict[str, set[int]] = {}

    def collect(self, key: str, rank: int, payload: bytes) -> bytes:
        deadline = time.monotonic() + 600
        with self.condition:
            values = self.values.setdefault(key, {})
            previous = values.get(rank)
            if previous is not None and previous != payload:
                raise ValueError(f"Rank {rank} sent conflicting payload for {key}")
            values[rank] = payload
            if len(values) == self.world_size and key not in self.results:
                lengths = {len(value) for value in values.values()}
                if len(lengths) != 1:
                    raise ValueError(f"Collective payload sizes differ for {key}: {sorted(lengths)}")
                ordered = [values[index] for index in range(self.world_size)]
                if key.startswith("barrier/"):
                    result = b""
                elif key.startswith("moments/"):
                    result = np.stack([np.frombuffer(value, dtype=np.float64) for value in ordered]).sum(0).tobytes()
                elif key.startswith("sum/"):
                    result = np.stack([np.frombuffer(value, dtype=np.float32) for value in ordered]).sum(0).tobytes()
                elif key.startswith("max/"):
                    result = np.stack([np.frombuffer(value, dtype=np.float32) for value in ordered]).max(0).tobytes()
                else:
                    result = np.stack([np.frombuffer(value, dtype=np.float32) for value in ordered]).mean(0).tobytes()
                self.results[key] = result
                self.condition.notify_all()
            while key not in self.results:
                remaining = deadline - time.monotonic()
                if remaining <= 0:
                    raise TimeoutError(f"Collective timed out for {key}: {sorted(values)}/{self.world_size}")
                self.condition.wait(min(remaining, 5))
            result = self.results[key]
            delivered = self.delivered.setdefault(key, set())
            delivered.add(rank)
            if len(delivered) == self.world_size:
                self.values.pop(key, None)
                self.delivered.pop(key, None)
                # Keep only a digest-sized tombstone impossible to confuse with a live result.
                self.results.pop(key, None)
            return result


def _handler(state: _State):
    class Handler(BaseHTTPRequestHandler):
        protocol_version = "HTTP/1.1"

        def log_message(self, *_args):
            return

        def _authorized(self) -> bool:
            return self.headers.get("Authorization") == f"Bearer {state.token}"

        def do_GET(self):
            if self.path != "/health" or not self._authorized():
                self.send_error(403)
                return
            body = b"ok"
            self.send_response(200)
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)

        def do_POST(self):
            if not self._authorized() or not self.path.startswith("/collect/"):
                self.send_error(403)
                return
            try:
                length = int(self.headers["Content-Length"])
                if length < 0 or length > 64 * 1024 * 1024:
                    raise ValueError("Invalid collective payload length")
                rank = int(self.headers["X-Mesh-Rank"])
                if not 0 <= rank < state.world_size:
                    raise ValueError("Invalid mesh rank")
                payload = self.rfile.read(length)
                key = self.path[len("/collect/"):]
                result = state.collect(key, rank, payload)
                self.send_response(200)
                self.send_header("Content-Type", "application/octet-stream")
                self.send_header("Content-Length", str(len(result)))
                self.end_headers()
                self.wfile.write(result)
            except Exception as exc:
                body = str(exc).encode()
                self.send_response(500)
                self.send_header("Content-Length", str(len(body)))
                self.end_headers()
                self.wfile.write(body)

    return Handler


class HTTPCollective:
    def __init__(self, rank: int, world_size: int, url: str, token: str, port: int = 8888):
        if not token or len(token) < 24:
            raise ValueError("MESH_TOKEN must contain at least 24 characters")
        self.rank = rank
        self.world_size = world_size
        self.url = url.rstrip("/")
        self.token = token
        self.server = None
        self.thread = None
        if rank == 0:
            state = _State(world_size, token)
            self.server = ThreadingHTTPServer(("0.0.0.0", port), _handler(state))
            self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)
            self.thread.start()
        self._wait_until_ready()

    def _request(self, path: str, payload: bytes | None = None) -> bytes:
        headers = {"Authorization": f"Bearer {self.token}", "X-Mesh-Rank": str(self.rank),
                   "User-Agent": "g1-coke-mesh/1.0"}
        request = urllib.request.Request(self.url + path, data=payload, headers=headers,
                                         method="POST" if payload is not None else "GET")
        with urllib.request.urlopen(request, timeout=620) as response:
            return response.read()

    def _wait_until_ready(self):
        deadline = time.monotonic() + 180
        last_error = None
        while time.monotonic() < deadline:
            try:
                if self._request("/health") == b"ok":
                    return
            except (OSError, urllib.error.URLError) as exc:
                last_error = exc
            time.sleep(1)
        raise TimeoutError(f"HTTP collective did not become ready: {last_error}")

    def reduce_tensor(self, kind: str, key: str, tensor: torch.Tensor) -> torch.Tensor:
        dtype = np.float64 if kind == "moments" else np.float32
        host = tensor.detach().to("cpu", dtype=torch.float64 if dtype == np.float64 else torch.float32).numpy()
        body = self._request(f"/collect/{kind}/{key}", host.tobytes())
        result = np.frombuffer(body, dtype=dtype).copy().reshape(host.shape)
        return torch.as_tensor(result, device=tensor.device, dtype=tensor.dtype)

    def barrier(self, key: str):
        self._request(f"/collect/barrier/{key}", b"")

    def reduce_gradients(self, key: str, parameters):
        trainable = [parameter for parameter in parameters if parameter.grad is not None]
        flat = torch.cat([parameter.grad.detach().reshape(-1) for parameter in trainable])
        reduced = self.reduce_tensor("grad", key, flat)
        offset = 0
        for parameter in trainable:
            count = parameter.grad.numel()
            parameter.grad.copy_(reduced[offset:offset + count].view_as(parameter.grad))
            offset += count

    def close(self):
        if self.server is not None:
            self.server.shutdown()
            self.server.server_close()


def from_environment() -> HTTPCollective | None:
    world_size = int(os.environ.get("MESH_WORLD_SIZE", "1"))
    if world_size <= 1:
        return None
    rank = int(os.environ["MESH_RANK"])
    return HTTPCollective(rank, world_size, os.environ["MESH_COORDINATOR_URL"],
                          os.environ["MESH_TOKEN"], int(os.environ.get("MESH_COORDINATOR_PORT", "8888")))
