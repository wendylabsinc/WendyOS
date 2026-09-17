"""Exact-frame YOLO segmentation input for the motion-zero inference service.

This module intentionally contains no Unitree SDK, DDS, or actuator imports.
"""
from __future__ import annotations

import io
import json
import os
import threading
import time
from typing import Any
from urllib.request import Request, urlopen

import numpy as np
import torch

from .observation_adapter import ExactEpisodeObservationAdapter, SynchronizedPolicyPacket


EXPECTED_MASK_SHA256 = "586135a02f5e6154169e1164636bb3c5b68aa7dea7228949bc453023ec999f49"
SEGMENTATION_URL = os.environ.get("G1_SEGMENTATION_URL", "http://127.0.0.1:8003").rstrip("/")
MAXIMUM_CAMERA_AGE_S = float(os.environ.get("G1_MAXIMUM_CAMERA_AGE_S", "1.0"))


def get(url: str, *, limit: int, timeout: float = 0.6) -> bytes:
    request = Request(url, headers={"User-Agent": "Wendy-G1-Stage02-Shadow/1"})
    with urlopen(request, timeout=timeout) as response:
        body = response.read(limit + 1)
    if len(body) > limit:
        raise ValueError("bounded HTTP read exceeded")
    return body


def decode_policy_frame(
    raw: bytes, expected: dict[str, Any]
) -> tuple[torch.Tensor, dict[str, Any], np.ndarray, np.ndarray]:
    with np.load(io.BytesIO(raw), allow_pickle=False) as values:
        if set(values.files) != {"color_bgr", "depth_m", "mask", "metadata_json"}:
            raise ValueError("segmentation policy-frame contract changed")
        meta = json.loads(values["metadata_json"].item())
        bgr = values["color_bgr"].copy()
        depth = values["depth_m"].copy()
        mask = values["mask"].copy()
    for key in ("frame_id", "stream_id", "captured_at_unix_ns"):
        if meta.get(key) != expected.get(key):
            raise ValueError("segmentation policy-frame identity mismatch")
    if (
        bgr.shape != (240, 320, 3)
        or bgr.dtype != np.uint8
        or depth.shape != (240, 320)
        or mask.shape != (240, 320)
    ):
        raise ValueError("segmentation policy-frame dimensions changed")
    rgb = bgr[:, :, ::-1]
    quantized_depth = np.clip(np.rint(depth * 1000) * 0.001, 0, 5)
    packed = np.empty((1, 5, 240, 320), dtype=np.float32)
    packed[0, :3] = np.transpose(rgb, (2, 0, 1)).astype(np.float32) / 255
    packed[0, 3] = quantized_depth.astype(np.float32) / 5
    packed[0, 4] = (mask > 0).astype(np.float32)
    return torch.from_numpy(packed), meta, depth, mask > 0


class SegmentationCameraPump:
    """Continuously submit synchronized RGB-D-mask packets after activation."""

    motion_capability = False

    def __init__(
        self,
        adapter: ExactEpisodeObservationAdapter,
        *,
        segmentation_url: str = SEGMENTATION_URL,
    ) -> None:
        self.adapter = adapter
        self.segmentation_url = segmentation_url.rstrip("/")
        self.stop = threading.Event()
        self.active = threading.Event()
        self.thread: threading.Thread | None = None
        self.lock = threading.Lock()
        self.latest: dict[str, Any] = {}
        self.last_error: str | None = None

    def preflight(self) -> dict[str, Any]:
        state = json.loads(get(self.segmentation_url + "/state", limit=1_000_000))
        latest = state.get("latest") or {}
        if not state.get("healthy") or not latest:
            raise RuntimeError(state.get("last_error") or "segmentation is unavailable")
        model = latest.get("model") or {}
        if model.get("checkpoint_sha256") != EXPECTED_MASK_SHA256:
            raise RuntimeError("segmentation checkpoint identity changed")
        captured_at_ns = int(latest.get("captured_at_unix_ns", 0))
        age_s = (time.time_ns() - captured_at_ns) / 1_000_000_000
        if captured_at_ns <= 0 or age_s < 0 or age_s > MAXIMUM_CAMERA_AGE_S:
            raise RuntimeError(f"segmentation capture is stale ({age_s * 1000:.1f} ms)")
        return state

    def start(self) -> None:
        if self.thread is not None:
            raise RuntimeError("camera pump already started")
        self.thread = threading.Thread(target=self._run, name="stage02-camera", daemon=True)
        self.thread.start()

    def activate(self) -> None:
        self.active.set()

    def deactivate(self) -> None:
        self.active.clear()

    def status(self) -> dict[str, Any]:
        with self.lock:
            return {"latest": dict(self.latest), "last_error": self.last_error}

    def close(self) -> None:
        self.stop.set()
        self.active.set()
        if self.thread is not None:
            self.thread.join(timeout=2.0)
            if self.thread.is_alive():
                raise RuntimeError("camera pump did not stop")

    def _run(self) -> None:
        seen: tuple[str, int] | None = None
        while not self.stop.is_set():
            try:
                state = self.preflight()
                latest = state["latest"]
                stream_id = str(latest["stream_id"])
                frame_id = int(latest["frame_id"])
                identity = (stream_id, frame_id)
                if identity == seen:
                    self.stop.wait(0.005)
                    continue
                raw = get(
                    self.segmentation_url
                    + f"/frame.policy-rgbd?frame_id={frame_id}&stream_id={stream_id}",
                    limit=1_000_000,
                )
                image, meta, depth_m, visible_mask = decode_policy_frame(raw, latest)
                target_mask_valid = state.get("target_mask_valid") is True
                if not target_mask_valid:
                    image[:, 4].zero_()
                self.adapter.retargeter.observe(
                    depth_m=depth_m,
                    mask=visible_mask,
                    metadata=latest,
                    detection_valid=target_mask_valid,
                )
                packet = SynchronizedPolicyPacket(
                    image=image,
                    frame_id=frame_id,
                    stream_id=stream_id,
                    captured_at_ns=int(meta["captured_at_unix_ns"]),
                    detection_valid=target_mask_valid,
                )
                admitted = self.adapter.submit_camera(packet) if self.active.is_set() else False
                seen = identity
                with self.lock:
                    self.latest = {
                        "frame_id": frame_id,
                        "stream_id": stream_id,
                        "captured_at_unix_ns": packet.captured_at_ns,
                        "admitted": admitted,
                        "overlay_only": not self.active.is_set(),
                    }
                    self.last_error = None
            except RuntimeError as exc:
                if "reset_episode must be called" not in str(exc):
                    with self.lock:
                        self.last_error = f"{type(exc).__name__}: {exc}"
                self.stop.wait(0.025)
            except Exception as exc:
                with self.lock:
                    self.last_error = f"{type(exc).__name__}: {exc}"
                self.stop.wait(0.025)
