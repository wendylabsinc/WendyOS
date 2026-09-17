"""Exact Stage 2 policy split into asynchronous vision and 40 Hz control."""
from __future__ import annotations

import importlib
import json
import os
import sys
import time
from pathlib import Path
from typing import Any

import numpy as np
import torch

from .async_vision import AsyncVisionBuffer, VisionFrame, VisionSnapshot
from .policy_bundle import (
    EXPECTED_ARCHITECTURE,
    EXPECTED_OWNED_INDICES,
    PolicyBundleIdentity,
    verify_runtime_bundle,
)

EXPECTED_CHECKPOINT_SHA256 = "3aaf0c8279e9190a6d47740f6dfdd427ff4fb495ab7e1ae11334e1e1d3573738"


def verify_bundle(bundle: Path) -> PolicyBundleIdentity:
    """Compatibility wrapper retained for callers and focused tests."""
    return verify_runtime_bundle(bundle)


def load_exact_policy(bundle: Path, device: str):
    identity = verify_bundle(bundle)
    checkpoint = bundle / "checkpoint.pt"
    source = bundle / "source"
    reference_rl = source / "reference_rl"
    recurrent_bc = source / "recurrent_bc"
    for path in (str(reference_rl), str(recurrent_bc)):
        if path not in sys.path:
            sys.path.insert(0, path)
    recurrent = importlib.import_module("recurrent_residual")
    policy_module = importlib.import_module("policy")
    payload = torch.load(checkpoint, map_location="cpu", weights_only=False)
    if payload.get("update") != identity.checkpoint_update:
        raise ValueError("checkpoint update differs from POLICY.json")
    contract = payload.get("contract") or {}
    state = payload.get("model") or {}
    if contract.get("architecture") != EXPECTED_ARCHITECTURE:
        raise ValueError("unexpected policy architecture")
    if contract.get("control_hz") != 40 or contract.get("camera_hz") != 20:
        raise ValueError("unexpected policy rates")
    if contract.get("owned_indices") != EXPECTED_OWNED_INDICES:
        raise ValueError("unexpected owned-joint mapping")
    sensor = contract.get("sensor_input_contract") or {}
    if sensor.get("actor_input_dim") != 316:
        raise ValueError("historical embedded actor-input metadata changed")
    if tuple(state["trunk.0.weight"].shape) != (256, 316):
        raise ValueError("checkpoint tensor does not contain the exact 316-input trunk")
    if tuple(state["actor.weight"].shape) != (15, 128):
        raise ValueError("checkpoint action mapping changed")
    model = recurrent.initialize_recurrent(payload, device=device)
    model.eval()
    return model, policy_module.ResidualController, payload["contract"], identity


class ExactVisionEncoder:
    """Run the frozen CNN on its own CUDA stream and publish only completed data."""

    def __init__(self, model, device: str, *, output_device: str | torch.device | None = None):
        self.model = model
        self.device = torch.device(device)
        self.output_device = torch.device(output_device or device)
        self.stream = torch.cuda.Stream(device=self.device) if self.device.type == "cuda" else None

    @torch.inference_mode()
    def __call__(self, image: torch.Tensor) -> torch.Tensor:
        if image.shape != (1, 5, 240, 320) or image.dtype != torch.float32:
            raise ValueError("vision input must be float32 [1,5,240,320]")
        if self.stream is None:
            return self.model.encoder.vision(image.to(self.device)).to(self.output_device).detach()
        with torch.cuda.stream(self.stream):
            value = self.model.encoder.vision(image.to(self.device, non_blocking=True)).detach()
            if self.output_device != self.device:
                value = value.to(self.output_device)
        self.stream.synchronize()
        return value


class CachedReferenceResidualPolicy:
    """Deterministic non-actuating policy proposal generator.

    No Unitree SDK, DDS publisher, or controller writer is imported here.
    """

    motion_capability = False

    def __init__(
        self,
        bundle: Path,
        *,
        device: str = "cuda",
        control_device: str | None = None,
        maximum_camera_age_s: float = 0.125,
    ):
        """Load the exact checkpoint with independently placed vision/control paths.

        ``device`` remains the vision device for compatibility.  By default the
        actor/controller stays on the same device.  A CPU ``control_device``
        keeps the asynchronous CNN on CUDA but avoids launching and synchronizing
        dozens of tiny CUDA kernels at 40 Hz on Jetson.
        """
        self.vision_device = torch.device(device)
        self.device = torch.device(control_device or device)
        if self.device.type == "cpu":
            # This is a small, batch-1 recurrent graph. Jetson's default
            # intra-op pool creates more scheduling work than useful compute
            # and contends with the concurrent RGB-D packing thread.
            torch.set_num_threads(max(1, int(os.environ.get("G1_POLICY_CPU_THREADS", "1"))))
            try:
                torch.set_num_interop_threads(1)
            except RuntimeError:
                # PyTorch permits setting this only before inter-op work has
                # begun. A previously configured process-wide value is safe.
                pass
        self.model, controller_type, self.contract, self.identity = load_exact_policy(
            bundle, str(self.device)
        )
        if self.vision_device == self.device:
            self._vision_model = self.model
        else:
            self._vision_model, _, vision_contract, vision_identity = load_exact_policy(
                bundle, str(self.vision_device)
            )
            if vision_contract != self.contract or vision_identity != self.identity:
                raise ValueError("vision and control checkpoint contracts differ")
        reference_result = json.loads(
            (bundle / self.identity.manifest["reference_result_path"]).read_text()
        )
        self._controller_type = controller_type
        self._bounds = np.asarray(reference_result["joint_bounds"], dtype=np.float64)
        self.controller = controller_type(self._bounds)
        self.hidden = self.model.initial_hidden(1)
        self.vision = AsyncVisionBuffer(
            ExactVisionEncoder(
                self._vision_model,
                str(self.vision_device),
                output_device=self.device,
            ),
            maximum_capture_age_s=maximum_camera_age_s,
        )
        self.vision.start()
        self.steps = 0
        self.control_ms: list[float] = []

    def reset_episode(self) -> None:
        self.hidden = self.model.initial_hidden(1)
        self.controller = self._controller_type(self._bounds)
        self.steps = 0
        self.vision.reset_episode()

    def submit_camera(self, *, image: torch.Tensor, frame_id: int, stream_id: str, captured_at_ns: int) -> bool:
        return self.vision.submit(VisionFrame(frame_id, stream_id, captured_at_ns, image))

    @torch.inference_mode()
    def propose(
        self,
        *,
        joint_features: torch.Tensor,
        reference_full: np.ndarray,
        reference_velocity: np.ndarray,
        control_at_ns: int | None = None,
        vision_snapshot: VisionSnapshot | None = None,
    ) -> dict[str, Any]:
        started = time.perf_counter_ns()
        control_at_ns = time.time_ns() if control_at_ns is None else control_at_ns
        snapshot = (
            self.vision.latest(control_at_ns=control_at_ns)
            if vision_snapshot is None
            else self.vision.admit(vision_snapshot, control_at_ns=control_at_ns)
        )
        if joint_features.shape != (1, 131):
            raise ValueError("joint_features must be [1,131]")
        reference_full = np.asarray(reference_full, dtype=np.float32)
        reference_velocity = np.asarray(reference_velocity, dtype=np.float32)
        if reference_full.shape != (43,) or reference_velocity.shape != (15,):
            raise ValueError("reference shapes must be [43] and [15]")

        joints = joint_features.to(self.device, non_blocking=True)
        normalized = ((joints - self.model.encoder.joint_mean) / self.model.encoder.joint_std).clamp(-10, 10)
        joint_embedding = self.model.encoder.joints(normalized)
        sensors = torch.cat((snapshot.embedding, joint_embedding), dim=-1)
        owned = reference_full[[12, *range(29, 43)]]
        actor_input = self.model.inputs(
            sensors,
            torch.as_tensor(owned, device=self.device)[None],
            torch.as_tensor(reference_velocity, device=self.device)[None],
            torch.as_tensor(self.controller.normalized, device=self.device)[None],
            torch.as_tensor(self.controller.velocity, dtype=torch.float32, device=self.device)[None],
        )
        if actor_input.shape != (1, 316):
            raise RuntimeError("Stage 2 actor input did not contain exactly 316 features")
        distribution, value, self.hidden = self.model.step(actor_input, self.hidden)
        self.hidden = self.hidden.detach()
        raw = distribution.loc[0].detach().cpu().numpy()
        target = self.controller.step(reference_full, raw)
        self.steps += 1
        elapsed_ms = (time.perf_counter_ns() - started) / 1e6
        self.control_ms.append(elapsed_ms)
        self.control_ms = self.control_ms[-4000:]
        return {
            "schema": "wendy.g1.reference-residual-proposal.v1",
            "motion_capability": False,
            "checkpoint_sha256": self.identity.checkpoint_sha256,
            "candidate_id": self.identity.candidate_id,
            "checkpoint_update": self.identity.checkpoint_update,
            "step": self.steps - 1,
            "frame_id": snapshot.frame_id,
            "stream_id": snapshot.stream_id,
            "vision_generation": snapshot.generation,
            "camera_age_ms": (control_at_ns - snapshot.captured_at_ns) / 1e6,
            "vision_encode_ms": snapshot.elapsed_ms,
            "control_compute_ms": elapsed_ms,
            "raw_action_mean": raw.tolist(),
            "target_q_43": target.tolist(),
            "value": float(value[0].detach().cpu()),
        }

    def close(self) -> None:
        self.vision.close()
