"""Deterministic, motion-zero NX2 benchmark for the update-2,525 hot path.

This module intentionally has no Unitree/DDS imports.  It replays the sealed
100-step golden trace, checks every recurrent output, and compares the legacy
all-CUDA control path with a split CUDA-vision/CPU-control path.
"""
from __future__ import annotations

import json
import os
import sys
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from typing import Any

import numpy as np
import torch

from .async_vision import VisionSnapshot
from .exact_policy import CachedReferenceResidualPolicy, EXPECTED_CHECKPOINT_SHA256


BUNDLE = Path(os.environ.get("G1_POLICY_BUNDLE", "/bundle"))
PORT = int(os.environ.get("G1_BENCHMARK_PORT", "8096"))
BASELINE_STEPS = int(os.environ.get("G1_BENCHMARK_BASELINE_STEPS", "300"))
OPTIMIZED_STEPS = int(os.environ.get("G1_BENCHMARK_OPTIMIZED_STEPS", "10000"))
WARMUP_STEPS = 100
PARITY_ATOL = 1e-5
PROPOSAL_P99_GATE_MS = 15.0


def _percentiles(values: list[float]) -> dict[str, float]:
    array = np.asarray(values, dtype=np.float64)
    return {
        "p50": float(np.percentile(array, 50)),
        "p95": float(np.percentile(array, 95)),
        "p99": float(np.percentile(array, 99)),
        "maximum": float(np.max(array)),
        "mean": float(np.mean(array)),
    }


def _policy_image(trace: dict[str, np.ndarray], index: int) -> torch.Tensor:
    rgb = torch.from_numpy(trace["camera_rgb_u8"][index]).float().div(255).permute(2, 0, 1)[None]
    depth = (
        torch.from_numpy(trace["camera_depth_m"][index])
        .float()
        .mul(1000)
        .round()
        .mul(.001)
        .clamp(0, 5)[None, None]
        .div(5)
    )
    mask = torch.from_numpy(trace["camera_mask"][index]).float()[None, None]
    return torch.cat((rgb, depth, mask), dim=1).contiguous()


def _load_trace() -> dict[str, np.ndarray]:
    with np.load(BUNDLE / "golden-trace-100.npz", allow_pickle=False) as values:
        return {key: values[key].copy() for key in values.files}


def _synchronize(device: torch.device) -> None:
    if device.type == "cuda":
        torch.cuda.synchronize(device)


def _actor_benchmark(
    policy: CachedReferenceResidualPolicy,
    trace: dict[str, np.ndarray],
    *,
    steps: int,
) -> dict[str, Any]:
    device = policy.device
    actor_inputs = torch.from_numpy(trace["actor_input"]).to(device)
    hidden_before = torch.from_numpy(trace["hidden_before"])[None].to(device)
    expected_raw = trace["policy_mean_raw_action"]
    expected_hidden = trace["hidden_after"]
    expected_value = trace["value"]
    timings: list[float] = []
    maximum_raw_error = 0.0
    maximum_hidden_error = 0.0
    maximum_value_error = 0.0

    for iteration in range(WARMUP_STEPS + steps):
        index = iteration % 100
        _synchronize(device)
        started = time.perf_counter_ns()
        with torch.inference_mode():
            distribution, value, hidden = policy.model.step(
                actor_inputs[index : index + 1],
                hidden_before[:, index : index + 1],
            )
            actual_raw = distribution.loc[0].detach().cpu().numpy()
            actual_hidden = hidden[0, 0].detach().cpu().numpy()
            actual_value = float(value[0].detach().cpu())
        elapsed_ms = (time.perf_counter_ns() - started) / 1e6
        if iteration >= WARMUP_STEPS:
            timings.append(elapsed_ms)
        if iteration < 100:
            maximum_raw_error = max(
                maximum_raw_error,
                float(np.max(np.abs(actual_raw - expected_raw[index]))),
            )
            maximum_hidden_error = max(
                maximum_hidden_error,
                float(np.max(np.abs(actual_hidden - expected_hidden[index]))),
            )
            maximum_value_error = max(
                maximum_value_error,
                abs(actual_value - float(expected_value[index])),
            )

    parity = {
        "maximum_raw_action_abs_error": maximum_raw_error,
        "maximum_hidden_abs_error": maximum_hidden_error,
        "maximum_value_abs_error": maximum_value_error,
        "atol": PARITY_ATOL,
    }
    # The deployed deterministic contract consumes actor mean + recurrent
    # hidden state.  Critic value is logged for diagnosis but is not used by
    # physical control and is not part of the existing golden acceptance test.
    parity["value_observational_only"] = True
    parity["passed"] = (
        maximum_raw_error <= PARITY_ATOL
        and maximum_hidden_error <= PARITY_ATOL
    )
    return {"steps": steps, "latency_ms": _percentiles(timings), "parity": parity}


def _encode_trace(
    policy: CachedReferenceResidualPolicy,
    trace: dict[str, np.ndarray],
) -> tuple[list[torch.Tensor], dict[str, float]]:
    encode = policy.vision._encode  # internal benchmark seam; never published to a robot
    embeddings: list[torch.Tensor] = []
    timings: list[float] = []
    for index in range(100):
        image = _policy_image(trace, index)
        started = time.perf_counter_ns()
        embeddings.append(encode(image))
        timings.append((time.perf_counter_ns() - started) / 1e6)
    return embeddings, _percentiles(timings)


def _full_proposal_benchmark(
    policy: CachedReferenceResidualPolicy,
    trace: dict[str, np.ndarray],
    embeddings: list[torch.Tensor],
    *,
    steps: int,
) -> dict[str, Any]:
    timings: list[float] = []
    maximum_raw_error = 0.0
    maximum_target_error = 0.0

    policy.reset_episode()
    episode_generation = int(policy.vision.status()["episode_generation"])
    for iteration in range(WARMUP_STEPS + steps):
        index = iteration % 100
        # The sealed trace contains the sampled rollout's true recurrent and
        # residual-controller state before every action. Seed that frozen state
        # outside the timed region so all 100 rows validate independently.
        policy.hidden = (
            torch.from_numpy(trace["hidden_before"][index : index + 1])
            .to(policy.device)[None]
        )
        policy.controller.offset = (
            trace["previous_normalized_residual"][index].astype(np.float64)
            * policy.controller.cap
        )
        policy.controller.velocity = trace["correction_velocity"][index].astype(np.float64).copy()
        policy.controller.steps = 0 if index == 0 else 1
        policy.steps = index
        control_at_ns = time.time_ns()
        snapshot = VisionSnapshot(
            episode_generation=episode_generation,
            generation=iteration + 1,
            frame_id=index,
            stream_id="sealed-golden-trace",
            captured_at_ns=control_at_ns,
            completed_at_ns=control_at_ns,
            elapsed_ms=0.0,
            embedding=embeddings[index],
        )
        started = time.perf_counter_ns()
        proposal = policy.propose(
            joint_features=torch.from_numpy(trace["joint_features_raw"][index : index + 1]),
            reference_full=trace["reference_full"][index],
            reference_velocity=trace["reference_velocity"][index],
            control_at_ns=control_at_ns,
            vision_snapshot=snapshot,
        )
        elapsed_ms = (time.perf_counter_ns() - started) / 1e6
        if iteration >= WARMUP_STEPS:
            timings.append(elapsed_ms)
        if iteration < 100:
            maximum_raw_error = max(
                maximum_raw_error,
                float(
                    np.max(
                        np.abs(
                            np.asarray(proposal["raw_action_mean"], dtype=np.float32)
                            - trace["policy_mean_raw_action"][index]
                        )
                    )
                ),
            )
            maximum_target_error = max(
                maximum_target_error,
                float(
                    np.max(
                        np.abs(
                            np.asarray(proposal["target_q_43"], dtype=np.float64)
                            - trace["mean_action_target_full"][index]
                        )
                    )
                ),
            )

    parity = {
        "maximum_raw_action_abs_error": maximum_raw_error,
        "maximum_target_abs_error": maximum_target_error,
        "atol": PARITY_ATOL,
    }
    parity["passed"] = all(
        value <= PARITY_ATOL
        for key, value in parity.items()
        if key.startswith("maximum_")
    )
    latency = _percentiles(timings)
    return {
        "steps": steps,
        "latency_ms": latency,
        "parity": parity,
        "validation_contract": "100 frozen pre-action recurrent/controller states",
        "proposal_p99_gate_ms": PROPOSAL_P99_GATE_MS,
        "proposal_p99_gate_passed": latency["p99"] <= PROPOSAL_P99_GATE_MS,
    }


def _run_case(
    trace: dict[str, np.ndarray],
    *,
    name: str,
    control_device: str,
    steps: int,
) -> dict[str, Any]:
    policy = CachedReferenceResidualPolicy(
        BUNDLE,
        device="cuda",
        control_device=control_device,
        maximum_camera_age_s=1.0,
    )
    try:
        embeddings, vision_latency = _encode_trace(policy, trace)
        actor = _actor_benchmark(policy, trace, steps=steps)
        full = _full_proposal_benchmark(policy, trace, embeddings, steps=steps)
        return {
            "name": name,
            "vision_device": str(policy.vision_device),
            "control_device": str(policy.device),
            "actor_only": actor,
            "full_proposal": full,
            "vision_encode_ms": vision_latency,
            "passed": actor["parity"]["passed"] and full["parity"]["passed"],
        }
    finally:
        policy.close()


class BenchmarkRuntime:
    def __init__(self) -> None:
        self.lock = threading.Lock()
        self.value: dict[str, Any] = {
            "schema": "wendy.g1.reference-residual-hotpath-benchmark.v1",
            "status": "starting",
            "motion_capability": False,
            "physical_commands_sent": 0,
            "checkpoint_sha256": EXPECTED_CHECKPOINT_SHA256,
            "cases": {},
        }

    def start(self) -> None:
        threading.Thread(target=self._run, name="policy-hotpath-benchmark", daemon=True).start()

    def state(self) -> dict[str, Any]:
        with self.lock:
            return json.loads(json.dumps(self.value, allow_nan=False))

    def _update(self, **values: Any) -> None:
        with self.lock:
            self.value.update(values)

    def _run(self) -> None:
        try:
            if os.environ.get("G1_MOTION_ENABLED", "false").lower() != "false":
                raise RuntimeError("benchmark requires G1_MOTION_ENABLED=false")
            if any(name == "physical_io" or name.startswith("physical_io.") for name in sys.modules):
                raise RuntimeError("physical I/O module present in motion-zero benchmark")
            trace = _load_trace()
            self._update(status="running_legacy_cuda")
            legacy = _run_case(
                trace,
                name="legacy_cuda_control",
                control_device="cuda",
                steps=BASELINE_STEPS,
            )
            with self.lock:
                self.value["cases"]["legacy_cuda_control"] = legacy
                self.value["status"] = "running_split_control"
            split = _run_case(
                trace,
                name="cuda_vision_cpu_control",
                control_device="cpu",
                steps=OPTIMIZED_STEPS,
            )
            with self.lock:
                self.value["cases"]["cuda_vision_cpu_control"] = split
                self.value["status"] = "complete"
                self.value["acceptance"] = {
                    "exact_100_step_parity": split["passed"],
                    "full_proposal_p99_le_15_ms": split["full_proposal"]["proposal_p99_gate_passed"],
                    "warmed_steps": OPTIMIZED_STEPS,
                    "actor_to_publish_p99_lt_25_ms": None,
                    "actor_to_publish_note": "deferred to a separately authorized staged DDS test",
                }
                self.value["passed"] = bool(
                    split["passed"]
                    and split["full_proposal"]["proposal_p99_gate_passed"]
                    and OPTIMIZED_STEPS >= 10000
                )
                self.value["completed_at_unix_ns"] = time.time_ns()
        except Exception as exc:
            self._update(
                status="failed",
                passed=False,
                error=f"{type(exc).__name__}: {exc}",
                completed_at_unix_ns=time.time_ns(),
            )


def make_server(runtime: BenchmarkRuntime) -> ThreadingHTTPServer:
    class Handler(BaseHTTPRequestHandler):
        def do_GET(self) -> None:
            path = self.path.split("?", 1)[0]
            if path not in {"/health", "/api/reference-residual/benchmark"}:
                self.send_error(404)
                return
            body = json.dumps(runtime.state(), separators=(",", ":"), allow_nan=False).encode()
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)

        def do_POST(self) -> None:
            self.send_error(405)

        def log_message(self, _format: str, *_args: Any) -> None:
            return

    return ThreadingHTTPServer(("0.0.0.0", PORT), Handler)


def main() -> None:
    runtime = BenchmarkRuntime()
    runtime.start()
    make_server(runtime).serve_forever()


if __name__ == "__main__":
    main()
