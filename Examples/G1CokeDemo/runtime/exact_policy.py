"""Exact update-2525 policy split into asynchronous vision and 40 Hz control."""
from __future__ import annotations

import hashlib
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


from .contracts import DEFAULT_MAXIMUM_CAMERA_AGE_S, EXPECTED_CHECKPOINT_SHA256
EXPECTED_SOURCE_SHA256 = {
    "recurrent_bc/baseline/visual_policy.py": "109b7f0e060ffd3ecdf214a4fc938238404ecdf89394ab76b823837642c1c432",
    "recurrent_bc/recurrent_policy.py": "da0832f5456a50554eaffcec456cb1081b2678325daf6c1dc77f4717c73285d8",
    "reference_rl/recurrent_residual.py": "cf905a1a4db80e1a93455d33cb90974c0fd77ad482674e739d9debba8aa45800",
    "reference_rl/policy.py": "9e617c72e84ebc1a767939ed924e9c0fa7e943049219a1bba3b9da36f5c50227",
    "reference_rl/sensor_contract.py": "9b4325116a7610a77ec0b416b456c316c098657f283077733cdfd99c7d77042b",
}
EXPECTED_BUNDLE_SHA256 = {
    "MANIFEST.json": "a23b0a44ae8c44d4c3f49d3fdd1c3f4e3d907e8bdbc69d5a73ef92c520e931f8",
    "checkpoint.pt": EXPECTED_CHECKPOINT_SHA256,
    "golden-trace-100.npz": "3b4e97574a38da1adc0b1fee57248adbc22b0e914066e65260afa3d9c6d4bdfc",
    "reference-contract.npz": "98f12d860335bbaf6e770d626c3fd0efc6bfeff7cb9aad6cae36e296020b41b4",
    "reference/attempt-000159/result.json": "48e0d17b6f4dd13d9a27ee3ce2ec679293f7929c21db22f87fe6cdd9750d6756",
    **{f"source/{path}": digest for path, digest in EXPECTED_SOURCE_SHA256.items()},
}


def _sha256(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as source:
        for chunk in iter(lambda: source.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def verify_bundle(bundle: Path) -> None:
    for relative, expected in EXPECTED_BUNDLE_SHA256.items():
        if _sha256(bundle / relative) != expected:
            raise ValueError(f"runtime bundle SHA-256 mismatch: {relative}")


def load_exact_policy(bundle: Path, device: str):
    verify_bundle(bundle)
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
    if payload.get("update") != 2525:
        raise ValueError("unexpected checkpoint update")
    model = recurrent.initialize_recurrent(payload, device=device)
    model.eval()
    return model, policy_module.ResidualController, payload["contract"]


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
        maximum_camera_age_s: float = DEFAULT_MAXIMUM_CAMERA_AGE_S,
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
        self.model, controller_type, self.contract = load_exact_policy(bundle, str(self.device))
        if self.vision_device == self.device:
            self._vision_model = self.model
        else:
            self._vision_model, _, vision_contract = load_exact_policy(
                bundle, str(self.vision_device)
            )
            if vision_contract != self.contract:
                raise ValueError("vision and control checkpoint contracts differ")
        reference_result = json.loads(
            (bundle / "reference" / "attempt-000159" / "result.json").read_text()
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
            "checkpoint_sha256": EXPECTED_CHECKPOINT_SHA256,
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
