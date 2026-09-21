"""Run the supplied weights against their own sealed RGB-D reference trace."""
from __future__ import annotations

import time
from pathlib import Path

import numpy as np
import torch

from runtime.async_vision import VisionSnapshot
from runtime.exact_policy import CachedReferenceResidualPolicy, EXPECTED_CHECKPOINT_SHA256


def verify_policy(bundle: Path, progress=None) -> dict:
    policy = CachedReferenceResidualPolicy(bundle, device="cpu")
    errors = {"action": 0.0, "target": 0.0}
    parity_passed = True
    latencies = []
    try:
        with np.load(bundle / "golden-trace-100.npz", allow_pickle=False) as trace:
            count = len(trace["reference_full"])
            policy.reset_episode()
            generation = policy.vision.status()["episode_generation"]
            for index in range(count):
                # The trace records sampled actions. Restore its actual pre-step
                # recurrent/controller state when comparing deterministic means.
                policy.hidden = torch.from_numpy(trace["hidden_before"][index:index + 1].copy())[None]
                policy.controller.offset = trace["previous_normalized_residual"][index].astype(np.float64) * policy.controller.cap
                policy.controller.velocity = trace["correction_velocity"][index].astype(np.float64).copy()
                policy.controller.steps = int(index != 0)
                policy.steps = index
                rgb = torch.from_numpy(trace["camera_rgb_u8"][index].copy()).float().div(255).permute(2, 0, 1)[None]
                depth = torch.from_numpy(trace["camera_depth_m"][index].copy()).float().mul(1000).round().mul(.001).clamp(0, 5)[None, None].div(5)
                mask = torch.from_numpy(trace["camera_mask"][index].copy()).bool()
                mask = mask & bool(trace["detection_valid"][index])
                packed = torch.cat((rgb, depth, mask.float()[None, None]), dim=1)
                started = time.perf_counter()
                with torch.inference_mode():
                    embedding = policy.model.encoder.vision(packed)
                    now = time.time_ns()
                    snapshot = VisionSnapshot(
                        episode_generation=generation, generation=index + 1,
                        frame_id=index, stream_id="coke-demo-golden",
                        captured_at_ns=now, completed_at_ns=now,
                        elapsed_ms=(time.perf_counter() - started) * 1000,
                        embedding=embedding,
                    )
                    proposal = policy.propose(
                        joint_features=torch.from_numpy(trace["joint_features_raw"][index:index + 1].copy()),
                        reference_full=trace["reference_full"][index],
                        reference_velocity=trace["reference_velocity"][index],
                        control_at_ns=now, vision_snapshot=snapshot,
                    )
                latencies.append((time.perf_counter() - started) * 1000)
                action_error = float(np.max(np.abs(np.asarray(proposal["raw_action_mean"]) - trace["policy_mean_raw_action"][index])))
                target_error = float(np.max(np.abs(np.asarray(proposal["target_q_43"]) - trace["mean_action_target_full"][index])))
                if not np.isfinite([action_error, target_error]).all():
                    raise RuntimeError(f"Non-finite policy output at frame {index}")
                errors["action"] = max(errors["action"], action_error)
                errors["target"] = max(errors["target"], target_error)
                parity_passed &= bool(
                    np.allclose(proposal["raw_action_mean"], trace["policy_mean_raw_action"][index], atol=1e-5, rtol=1e-5)
                    and np.allclose(proposal["target_q_43"], trace["mean_action_target_full"][index], atol=1e-5, rtol=1e-5)
                )
                if progress:
                    progress(index + 1, count, proposal)
    finally:
        policy.close()
    return {
        "candidate": "reference-residual-gru-u002525",
        "checkpoint_sha256": EXPECTED_CHECKPOINT_SHA256,
        "reference": "attempt-000159",
        "frames": count,
        "passed": parity_passed,
        "maximum_action_error": errors["action"],
        "maximum_target_error_rad": errors["target"],
        "absolute_tolerance": 1e-5,
        "relative_tolerance": 1e-5,
        "full_inference_ms_p50": float(np.percentile(latencies, 50)),
        "full_inference_ms_p99": float(np.percentile(latencies, 99)),
        "physical_commands_sent": 0,
        "validation": "100 recorded pre-action states; deterministic action means",
    }
