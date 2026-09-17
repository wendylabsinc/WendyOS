"""Exact motion-zero physical observation seam for the Stage 2 policy.

This module binds ordered live joint samples to the same immutable RGB-D-mask
snapshot consumed by the policy.  It owns recurrent/reference episode state,
but has no Unitree SDK import, publisher, controller ownership, or command path.
"""
from __future__ import annotations

from dataclasses import dataclass
import hashlib
import math
from pathlib import Path
from typing import TYPE_CHECKING, Any, Sequence

import numpy as np
import torch

from .async_vision import VisionSnapshot
from .object_retarget import ObjectRelativeRetargeter

if TYPE_CHECKING:
    from .exact_policy import CachedReferenceResidualPolicy


CONTROL_HZ = 40
CONTROL_DT_S = 1 / CONTROL_HZ
JOINT_COUNT = 43
JOINT_FEATURE_COUNT = 131
IMAGE_SHAPE = (1, 5, 240, 320)
ACCELERATION_FILTER_HZ = 5.0
ACCELERATION_VALID_AFTER_S = 0.2


def _sha256(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as source:
        for chunk in iter(lambda: source.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def _finite_vector(value: Any, *, name: str) -> np.ndarray:
    if isinstance(value, torch.Tensor):
        value = value.detach().cpu().numpy()
    result = np.asarray(value, dtype=np.float32).copy()
    if result.shape != (JOINT_COUNT,) or not bool(np.isfinite(result).all()):
        raise ValueError(f"{name} must contain 43 finite values")
    return result


@dataclass(frozen=True)
class OrderedJointState:
    """One live sample already converted to the frozen 43-joint model order."""

    names: tuple[str, ...]
    q: Any
    dq: Any
    sampled_at_ns: int


@dataclass(frozen=True)
class SynchronizedPolicyPacket:
    """One source-frame RGB, aligned depth, and YOLO visible-mask packet."""

    image: torch.Tensor
    frame_id: int
    stream_id: str
    captured_at_ns: int
    detection_valid: bool


def pack_synchronized_policy_packet(
    *,
    rgb_u8: Any,
    depth_m: Any,
    visible_mask: Any,
    detection_valid: bool,
    frame_id: int,
    stream_id: str,
    captured_at_ns: int,
) -> SynchronizedPolicyPacket:
    """Pack the frozen 5x240x320 policy image without changing frame identity."""
    rgb = torch.as_tensor(rgb_u8)
    depth = torch.as_tensor(depth_m)
    mask = torch.as_tensor(visible_mask)
    if rgb.dtype != torch.uint8 or rgb.shape != (240, 320, 3):
        raise ValueError("RGB must be uint8 [240,320,3]")
    if depth.shape != (240, 320) or not bool(torch.isfinite(depth).all()):
        raise ValueError("depth must be finite [240,320] metres")
    if mask.dtype != torch.bool or mask.shape != (240, 320):
        raise ValueError("visible mask must be bool [240,320]")
    if frame_id < 0 or not stream_id or captured_at_ns <= 0:
        raise ValueError("invalid frame identity or capture timestamp")

    valid = bool(detection_valid) and bool(mask.any())
    packed_mask = mask if valid else torch.zeros_like(mask)
    quantized_depth = (depth.float().mul(1000).round().mul(0.001)).clamp(0, 5).div(5)
    image = torch.cat(
        (
            rgb.float().div(255).permute(2, 0, 1)[None],
            quantized_depth[None, None],
            packed_mask.float()[None, None],
        ),
        dim=1,
    ).contiguous()
    if image.shape != IMAGE_SHAPE or image.dtype != torch.float32:
        raise AssertionError("internal policy packet packing error")
    return SynchronizedPolicyPacket(
        image=image,
        frame_id=frame_id,
        stream_id=stream_id,
        captured_at_ns=captured_at_ns,
        detection_valid=valid,
    )


class LiveJointFeatureAdapter:
    """Pack q/dq into the frozen q,dq,filtered-ddq,valid,age feature order."""

    motion_capability = False

    def __init__(self, expected_joint_names: Sequence[str]) -> None:
        self.expected_joint_names = tuple(expected_joint_names)
        if len(self.expected_joint_names) != JOINT_COUNT or len(set(self.expected_joint_names)) != JOINT_COUNT:
            raise ValueError("frozen contract must contain 43 unique joint names")
        self.reset_episode(started_at_ns=None)

    def reset_episode(self, *, started_at_ns: int | None) -> None:
        if started_at_ns is not None and started_at_ns <= 0:
            raise ValueError("episode start timestamp must be positive")
        self.started_at_ns = started_at_ns
        self.previous_control_at_ns: int | None = None
        self.previous_sampled_at_ns: int | None = None
        self.previous_dq: np.ndarray | None = None
        self.filtered_ddq: np.ndarray | None = None

    def pack(
        self,
        state: OrderedJointState,
        *,
        camera_captured_at_ns: int,
        control_at_ns: int,
    ) -> torch.Tensor:
        if tuple(state.names) != self.expected_joint_names:
            raise ValueError("joint order differs from frozen policy contract")
        if state.sampled_at_ns <= 0 or camera_captured_at_ns <= 0 or control_at_ns <= 0:
            raise ValueError("joint, camera, and control timestamps must be positive")
        if state.sampled_at_ns > control_at_ns:
            raise ValueError("joint timestamp is in the future")
        if camera_captured_at_ns > control_at_ns:
            raise ValueError("camera timestamp is in the future")
        if self.previous_sampled_at_ns is not None and state.sampled_at_ns < self.previous_sampled_at_ns:
            raise ValueError("joint timestamp regressed")

        q = _finite_vector(state.q, name="q")
        dq = _finite_vector(state.dq, name="dq")
        if self.previous_control_at_ns is None:
            if self.started_at_ns is None:
                self.started_at_ns = control_at_ns
            self.filtered_ddq = np.zeros_like(dq)
        else:
            dt_s = (control_at_ns - self.previous_control_at_ns) / 1_000_000_000
            if dt_s <= 0:
                raise ValueError("control timestamps must increase")
            assert self.filtered_ddq is not None and self.previous_dq is not None
            alpha = 1 - math.exp(-2 * math.pi * ACCELERATION_FILTER_HZ * dt_s)
            raw_ddq = (dq - self.previous_dq) / dt_s
            self.filtered_ddq = self.filtered_ddq + alpha * (raw_ddq - self.filtered_ddq)

        assert self.started_at_ns is not None and self.filtered_ddq is not None
        acceleration_valid = (
            (control_at_ns - self.started_at_ns) / 1_000_000_000
            >= ACCELERATION_VALID_AFTER_S - 1e-9
        )
        camera_age_s = (control_at_ns - camera_captured_at_ns) / 1_000_000_000
        packed = np.empty(JOINT_FEATURE_COUNT, dtype=np.float32)
        packed[:43] = q
        packed[43:86] = dq
        packed[86:129] = self.filtered_ddq
        packed[129] = float(acceleration_valid)
        packed[130] = camera_age_s
        features = torch.from_numpy(packed[None])
        if features.shape != (1, JOINT_FEATURE_COUNT) or not bool(np.isfinite(packed).all()):
            raise AssertionError("internal joint feature packing error")

        self.previous_dq = dq
        self.previous_control_at_ns = control_at_ns
        self.previous_sampled_at_ns = state.sampled_at_ns
        return features


class FrozenReferenceTimeline:
    """The sealed 1,200-frame reference, advanced once per proposal."""

    def __init__(
        self,
        path: Path,
        *,
        expected_joint_names: Sequence[str],
        expected_sha256: str,
    ) -> None:
        if _sha256(path) != expected_sha256:
            raise ValueError("reference contract SHA-256 mismatch")
        with np.load(path, allow_pickle=False) as values:
            required = {
                "joint_names",
                "reference_joint_targets_43",
                "reference_owned_velocities_15",
            }
            if not required.issubset(values.files):
                raise ValueError("reference contract arrays are incomplete")
            names = tuple(str(value) for value in values["joint_names"].tolist())
            references = np.asarray(values["reference_joint_targets_43"], dtype=np.float32)
            velocities = np.asarray(values["reference_owned_velocities_15"], dtype=np.float32)
        if names != tuple(expected_joint_names):
            raise ValueError("reference and checkpoint joint order differ")
        if (
            references.ndim != 2
            or references.shape[1] != JOINT_COUNT
            or velocities.shape != (len(references), 15)
            or not np.isfinite(references).all()
            or not np.isfinite(velocities).all()
        ):
            raise ValueError("reference contract shapes or values changed")
        self.references = references.copy()
        self.velocities = velocities.copy()
        self.reset_episode()

    def reset_episode(self) -> None:
        self.frame = 0

    def current(self) -> tuple[int, np.ndarray, np.ndarray]:
        if self.frame >= len(self.references):
            raise RuntimeError("sealed reference is complete")
        return self.frame, self.references[self.frame], self.velocities[self.frame]

    def advance(self) -> None:
        if self.frame >= len(self.references):
            raise RuntimeError("sealed reference is complete")
        self.frame += 1


class ExactEpisodeObservationAdapter:
    """Bind live observations, frozen reference, and explicit recurrent reset."""

    motion_capability = False

    def __init__(self, policy: "CachedReferenceResidualPolicy", bundle: Path) -> None:
        sensor = policy.contract.get("sensor_input_contract")
        if not isinstance(sensor, dict) or sensor.get("actor_input_dim") != 316:
            raise ValueError("checkpoint metadata does not contain the historical 316-input contract")
        if policy.model.trunk[0].in_features != 316:
            raise ValueError("Stage 2 checkpoint trunk must consume 316 inputs")
        if policy.contract.get("control_hz") != CONTROL_HZ:
            raise ValueError("checkpoint control rate differs from 40 Hz")
        self.policy = policy
        self.joint_names = tuple(sensor.get("joint_names", ()))
        self.joints = LiveJointFeatureAdapter(self.joint_names)
        self.reference = FrozenReferenceTimeline(
            bundle / "reference-contract.npz",
            expected_joint_names=self.joint_names,
            expected_sha256=policy.identity.reference_contract_sha256,
        )
        self.retargeter = ObjectRelativeRetargeter(bundle)
        self.episode_generation = 0
        self.episode_started_at_ns: int | None = None

    def reset_episode(self, *, started_at_ns: int) -> dict[str, Any]:
        if started_at_ns <= 0:
            raise ValueError("episode start timestamp must be positive")
        self.policy.reset_episode()
        self.joints.reset_episode(started_at_ns=started_at_ns)
        self.reference.reset_episode()
        self.retargeter.reset_episode()
        self.episode_generation += 1
        self.episode_started_at_ns = started_at_ns

        hidden_zero = bool(torch.count_nonzero(self.policy.hidden).item() == 0)
        residual_zero = bool(np.count_nonzero(self.policy.controller.normalized) == 0)
        velocity_zero = bool(np.count_nonzero(self.policy.controller.velocity) == 0)
        if not (hidden_zero and residual_zero and velocity_zero and self.policy.steps == 0):
            raise RuntimeError("policy recurrent/controller history did not reset to episode zero")
        return {
            "episode_generation": self.episode_generation,
            "reference_frame": 0,
            "gru_hidden_zero": hidden_zero,
            "previous_normalized_residual_zero": residual_zero,
            "correction_velocity_zero": velocity_zero,
            "policy_steps": self.policy.steps,
            "motion_capability": False,
        }

    def submit_camera(self, packet: SynchronizedPolicyPacket) -> bool:
        if self.episode_started_at_ns is None:
            raise RuntimeError("reset_episode must be called before camera submission")
        if packet.captured_at_ns < self.episode_started_at_ns:
            raise ValueError("camera packet predates the current episode")
        return self.policy.submit_camera(
            image=packet.image,
            frame_id=packet.frame_id,
            stream_id=packet.stream_id,
            captured_at_ns=packet.captured_at_ns,
        )

    def propose(self, state: OrderedJointState, *, control_at_ns: int) -> dict[str, Any]:
        if self.episode_started_at_ns is None:
            raise RuntimeError("reset_episode must be called before proposal")
        snapshot: VisionSnapshot = self.policy.vision.latest(control_at_ns=control_at_ns)
        features = self.joints.pack(
            state,
            camera_captured_at_ns=snapshot.captured_at_ns,
            control_at_ns=control_at_ns,
        )
        reference_frame, reference_full, reference_velocity = self.reference.current()
        reference_full, reference_velocity, retarget = self.retargeter.apply(
            frame=reference_frame,
            reference_q_43=reference_full,
            reference_velocity_15=reference_velocity,
            live_q_43=state.q,
            control_at_ns=control_at_ns,
        )
        proposal = self.policy.propose(
            joint_features=features,
            reference_full=reference_full,
            reference_velocity=reference_velocity,
            control_at_ns=control_at_ns,
            vision_snapshot=snapshot,
        )
        if (
            proposal["frame_id"] != snapshot.frame_id
            or proposal["stream_id"] != snapshot.stream_id
            or proposal["step"] != reference_frame
        ):
            raise RuntimeError("policy proposal lost episode/frame synchronization")
        self.reference.advance()
        return {
            **proposal,
            "reference_frame": reference_frame,
            "reference_target_q_43": reference_full.tolist(),
            "joint_features_131": features[0].tolist(),
            "episode_generation": self.episode_generation,
            "object_retarget": retarget,
        }
