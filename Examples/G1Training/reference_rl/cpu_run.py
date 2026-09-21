"""Recurrent PPO using native CPU MuJoCo physics and CUDA policy learning."""
from pathlib import Path
import argparse
import gc
import json
import math
import os
import sys
import time

import numpy as np
import torch
import torch.distributed as tdist

from contact_location import CONTACT_INSTRUMENTATION_CONTRACT
from cpu_environment import CPUReferenceEnvironment
from cpu_process_mesh_environment import CPUProcessMeshEnvironment
from export_visual import atomic, sha
from grip_reward import GRIP_REWARD_RATE, THREE_FINGER_REWARD_RATE, REWARD_CONTRACT
from http_collective import from_environment
from palm_reward import PALM_REWARD_CONTRACT
from policy import DT, advantages
from recurrent_residual import ARCHITECTURE, initialize_recurrent, ordered_chunks
from reference_bank import load_training_sources
from sensor_contract import freeze_contract


def main():
    p = argparse.ArgumentParser()
    p.add_argument("--root", type=Path, required=True)
    p.add_argument("--parent", type=Path, required=True)
    p.add_argument("--data-root", type=Path, required=True,
                   help="Visual-BC corpus root containing dataset/*/episode.json")
    p.add_argument("--cache-root", type=Path, required=True,
                   help="Feature cache containing the parent-bound manifest.json")
    p.add_argument("--memory-checkpoint", type=Path,
                   help="Trained BC GRU checkpoint used only to initialize a non-recurrent parent")
    p.add_argument("--worlds", type=int, default=4)
    p.add_argument("--physics-workers", type=int, default=1)
    p.add_argument("--mesh-mode", choices=["sequential", "thread", "process"], default="sequential")
    p.add_argument("--updates", type=int, default=1000000)
    p.add_argument("--steps", type=int, default=128)
    p.add_argument("--hours", type=float, default=0)
    p.add_argument("--end-time-epoch", type=float, default=0,
                   help="Absolute Unix deadline; survives watchdog restarts")
    p.add_argument("--sequence-length", type=int, default=32)
    p.add_argument("--can-position-half-range-m", type=float, default=.06)
    p.add_argument("--can-yaw-range-rad", type=float, default=math.pi)
    p.add_argument("--can-table-edge-margin-m", type=float, default=.005)
    p.add_argument("--randomization-seed", type=int, default=20260914)
    p.add_argument("--reset-value-optimizer-for-reward-change", action="store_true")
    p.add_argument("--resume-in-place", action="store_true",
                   help="Resume this root from its newest atomic checkpoint after an elastic restart")
    p.add_argument("--preserve-parent-update", action="store_true",
                   help="Continue update numbering when starting a new root from a recurrent PPO checkpoint")
    a = p.parse_args()
    local_rank = int(os.environ.get("LOCAL_RANK", "0"))
    device = torch.device(f"cuda:{local_rank}")
    torch.cuda.set_device(device)
    os.environ["MUJOCO_EGL_DEVICE_ID"] = str(local_rank)
    distributed = int(os.environ.get("WORLD_SIZE", "1")) > 1
    http_collective = from_environment()
    if distributed and http_collective is not None:
        raise ValueError("Use either NCCL WORLD_SIZE or the HTTP mesh, not both")
    if distributed:
        tdist.init_process_group(backend="nccl", device_id=device)
    rank = (tdist.get_rank() if distributed else
            http_collective.rank if http_collective is not None else 0)
    learner_world_size = (tdist.get_world_size() if distributed else
                          http_collective.world_size if http_collective is not None else 1)

    def collective_barrier(key):
        if distributed:
            tdist.barrier(device_ids=[local_rank])
        elif http_collective is not None:
            http_collective.barrier(key)
    if (min(a.worlds, a.physics_workers, a.updates, a.steps, a.sequence_length) <= 0
            or a.hours < 0 or a.end_time_epoch < 0):
        raise ValueError("Invalid run size")
    if a.physics_workers > a.worlds:
        raise ValueError("Physics workers cannot exceed worlds")
    if a.mesh_mode == "process" and a.physics_workers != a.worlds:
        raise ValueError("Process mesh requires one physics worker per world")
    root = a.root
    root_preexisted = root.exists()
    if http_collective is not None:
        if root_preexisted and not a.resume_in_place:
            raise FileExistsError(root)
        root.mkdir(parents=True, exist_ok=a.resume_in_place)
    elif rank == 0:
        if root_preexisted and not a.resume_in_place:
            raise FileExistsError(root)
        root.mkdir(parents=True, exist_ok=a.resume_in_place)
    if learner_world_size > 1:
        collective_barrier("root-ready")
    rank_root = root / "ranks" / f"rank-{rank:03d}"
    rank_root.mkdir(parents=True, exist_ok=True)
    torch.set_num_threads(1)
    # Every rank must construct identical parameters before gradient averaging.
    torch.manual_seed(20260918)
    np.random.seed(20260918 + rank)
    resume_checkpoint = root / "latest.pt"
    if not resume_checkpoint.exists():
        resume_checkpoint = root / "initial.pt"
    resuming = bool(a.resume_in_place and root_preexisted and resume_checkpoint.exists())
    parent_path = resume_checkpoint if resuming else a.parent
    payload = torch.load(parent_path, map_location="cpu", weights_only=False)
    migrating_recurrent = payload["contract"].get("architecture") != ARCHITECTURE
    if migrating_recurrent and a.memory_checkpoint is None:
        raise ValueError("A non-recurrent parent requires --memory-checkpoint")
    bc_payload = (torch.load(a.memory_checkpoint, map_location="cpu", weights_only=False)
                  if migrating_recurrent else None)
    sources = load_training_sources(a.data_root, a.cache_root, payload["contract"])
    model = initialize_recurrent(payload, bc_payload, device=device)
    model.encoder.eval()
    # Actor and GRU are always retained. Critic/Adam restart only on the
    # MJWarp-to-native-MuJoCo boundary; mesh parallelism does not change the MDP.
    backend_changed = payload["contract"].get("physics_device") != "CPU MuJoCo float64"
    reward_contract = {
        **dict(REWARD_CONTRACT),
        "palm_pregrasp": dict(PALM_REWARD_CONTRACT),
        "top_contact": dict(CONTACT_INSTRUMENTATION_CONTRACT),
    }
    reward_changed = payload["contract"].get("reward_contract") != reward_contract
    if reward_changed and not a.reset_value_optimizer_for_reward_change:
        raise ValueError(
            "Reward contract changed; pass --reset-value-optimizer-for-reward-change "
            "to retain actor/GRU but reset the critic and optimizer"
        )
    reset_value_optimizer = backend_changed or reward_changed or migrating_recurrent
    if reset_value_optimizer:
        model.value.reset_parameters()
    optimizer = torch.optim.Adam([x for x in model.parameters() if x.requires_grad], lr=3e-5)
    if not reset_value_optimizer:
        optimizer.load_state_dict(payload["optimizer"])
    # Diversify policy samples only after synchronized parameter construction.
    torch.manual_seed(20261918 + rank)
    if rank == 0:
        (root / "failure.json").unlink(missing_ok=True)
        atomic(root / "status.json", dict(stage="CPU MuJoCo nominal qualification", running=True,
                                           learner_world_size=learner_world_size, resuming=resuming))
    (rank_root / "failure.json").unlink(missing_ok=True)
    qualification_path = rank_root / "cpu-zero-reference.json"
    if resuming and qualification_path.exists():
        qualification = json.loads(qualification_path.read_text())
    else:
        baseline = CPUReferenceEnvironment(sources[0], worlds=1, render=False,
                                           can_position_half_range_m=a.can_position_half_range_m,
                                           can_yaw_range_rad=a.can_yaw_range_rad,
                                           can_table_edge_margin_m=a.can_table_edge_margin_m,
                                           randomization_seed=a.randomization_seed + rank, physics_workers=1,
                                           device=device)
        zero = torch.zeros(1, 15, device=device)
        qualification_start = time.monotonic()
        for i in range(baseline.length):
            baseline.step(zero)
            if (i + 1) % 256 == 0:
                atomic(rank_root / "status.json", dict(stage="CPU MuJoCo nominal qualification", running=True,
                                                        rank=rank, frame=i + 1, frames=baseline.length,
                                                        elapsed_s=time.monotonic() - qualification_start))
                if rank == 0:
                    atomic(root / "status.json", dict(stage="CPU MuJoCo nominal qualification", running=True,
                                                       frame=i + 1, frames=baseline.length,
                                                       learner_world_size=learner_world_size,
                                                       elapsed_s=time.monotonic() - qualification_start))
        qualification = baseline.diagnostics()
        qualification["full_reference_replayed"] = True
        qualification["elapsed_s"] = time.monotonic() - qualification_start
        atomic(qualification_path, qualification)
        if rank == 0:
            atomic(root / "cpu-zero-reference.json", qualification)
        baseline.close()
    if not qualification["task_success"][0] or qualification["contact_violations"][0] != 0:
        raise RuntimeError("Native MuJoCo nominal reference did not pass full-task/contact qualification")
    if learner_world_size > 1:
        collective_barrier("qualification-ready")
    Environment = CPUProcessMeshEnvironment if a.mesh_mode == "process" else CPUReferenceEnvironment
    worker_count = a.physics_workers if a.mesh_mode != "sequential" else 1
    env = Environment(sources[0], worlds=a.worlds, render=True,
                                  can_position_half_range_m=a.can_position_half_range_m,
                                  can_yaw_range_rad=a.can_yaw_range_rad,
                                  can_table_edge_margin_m=a.can_table_edge_margin_m,
                                  randomization_seed=a.randomization_seed + rank * 1000003,
                                  physics_workers=worker_count, device=device)
    env.reset(randomize_can=True)
    obs = env.observation(model)
    if obs.shape != (a.worlds, 316) or not obs.is_cuda or not bool(torch.isfinite(obs).all()):
        raise RuntimeError("CPU backend sensor contract failed")
    sensor_contract = freeze_contract(payload["contract"]["normalization"], env.joint_names)
    prior = payload["contract"].get("sensor_input_contract")
    if prior is not None and prior["sha256"] != sensor_contract["sha256"]:
        raise RuntimeError("Frozen sensor input contract changed")
    smoke = dict(observation_shape=list(obs.shape), finite=True, cuda=obs.is_cuda,
                 rgb_range=[float(env.rgb.min()), float(env.rgb.max())],
                 depth_range=[float(env.depth.min()), float(env.depth.max())],
                 mask_pixels=env.mask_pixels.cpu().tolist())
    atomic(rank_root / "sensor-smoke.json", smoke)
    if rank == 0:
        atomic(root / "sensor-smoke.json", smoke)
    contract = {**payload["contract"], "architecture": ARCHITECTURE,
                "physics_device": "CPU MuJoCo float64", "physics_backend": "native mujoco.MjModel/MjData",
                "renderer": "CPU MuJoCo OpenGL/EGL", "segmentation": "native MuJoCo visible geom-id mask; no YOLO inference",
                "simulator_yolo_loaded": False, "reference_controller_device": "CPU",
                "reward_device": "CPU with CUDA rollout tensor transfer", "rollout_buffer_device": "CUDA",
                "learner_device": torch.cuda.get_device_name(device), "worlds_per_rank": a.worlds,
                "worlds": a.worlds * learner_world_size,
                "distributed_learner": {"backend": "NCCL" if distributed else "none",
                                        "ranks": learner_world_size,
                                        "worlds_per_rank": a.worlds,
                                        "gradient_reduction": "mean across ranks after every PPO minibatch",
                                        "advantage_normalization": "global across every rank and world"},
                "actor_mesh": {"kind": "synchronous native-MuJoCo " + a.mesh_mode + " actors",
                               "physics_workers": worker_count, "worlds": a.worlds,
                               "model_sharing": "independent MjData per world; process mode isolates models and Python audits",
                               "policy_versioning": "one frozen policy version per PPO rollout; synchronized GPU update",
                               "render_and_inference": ("process-local EGL rendering into shared memory; centralized batched CUDA encoder and GRU"
                                                        if a.mesh_mode == "process" else
                                                        "single-owner EGL rendering followed by batched CUDA encoder and GRU")},
                "updates": a.updates, "steps_per_update": a.steps, "duration_hours": a.hours,
                "end_time_epoch": a.end_time_epoch,
                "parent_checkpoint_sha256": sha(a.parent), "parent_update": payload.get("update"),
                "fresh_residual_ppo": bool(payload["contract"].get("fresh_residual_ppo",
                                                                      migrating_recurrent)),
                "memory_checkpoint_sha256": (sha(a.memory_checkpoint) if migrating_recurrent else
                                             payload["contract"].get("memory_checkpoint_sha256")),
                "memory_training": ("trained BC GRU imported at PPO update 0; residual actor inherited from "
                                    "the zero-actor non-recurrent parent" if migrating_recurrent else
                                    payload["contract"].get("memory_training")),
                "backend_migration": ("actor and GRU retained; critic and optimizer reset for native MuJoCo returns"
                                      if backend_changed and not migrating_recurrent else
                                      "zero residual actor retained; trained BC GRU imported; critic and optimizer fresh"
                                      if migrating_recurrent else
                                      "native MuJoCo policy, critic, GRU, and optimizer resumed; actor execution parallelized only"),
                "optimizer_migration": ("fresh for recurrent PPO initialization" if migrating_recurrent else
                                        "fresh for physics backend change" if backend_changed else
                                        "fresh for reward change" if reward_changed else "resumed"),
                "sensor_input_contract": sensor_contract,
                "mujoco_version": __import__("mujoco").__version__, "native_solver": qualification["solver"],
                "zero_reference_successes": 1, "baseline_full_reference": True,
                "can_randomization": {"scope": "training resets only; nominal qualification unrandomized",
                                      "position_half_range_m": a.can_position_half_range_m,
                                      "yaw_range_rad": a.can_yaw_range_rad,
                                      "table_edge_margin_m": a.can_table_edge_margin_m,
                                      "seed": a.randomization_seed,
                                      "rank_seed_stride": 1000003,
                                      "reference_reward_alignment": "initial XY offset decays along recorded path to fixed destination"},
                "reward_contract": reward_contract,
                "elastic_resume_in_place": a.resume_in_place,
                "source_sha256": {
                    name: sha(Path(__file__).parent / name)
                    for name in (
                        "cpu_run.py", "cpu_environment.py", "cpu_process_mesh_environment.py",
                        "contact_location.py", "http_collective.py", "grip_reward.py",
                        "palm_reward.py", "policy.py",
                        "recurrent_residual.py", "reference_bank.py", "sensor_contract.py",
                        "rgbd_evaluation.py",
                    )
                }}
    if resuming:
        prior_distributed = payload["contract"].get("distributed_learner", {})
        if (int(prior_distributed.get("ranks", learner_world_size)) != learner_world_size
                or int(prior_distributed.get("worlds_per_rank", a.worlds)) != a.worlds):
            raise ValueError("Elastic restart changed learner rank or per-rank world count")
        contract = payload["contract"]
    elif http_collective is not None:
        contract["distributed_learner"]["backend"] = "authenticated HTTP gradient coordinator"
    if rank == 0 and not resuming:
        atomic(root / "contract.json", contract)
        atomic(root / "sensor-input-contract.json", sensor_contract)
        atomic(root / "backend-transition.json", dict(from_backend=payload["contract"].get("physics_device"),
                                                        to_backend="CPU MuJoCo float64", parent=str(parent_path),
                                                        parent_sha256=sha(parent_path), actor_gru_retained=True,
                                                        value_optimizer_reset=reset_value_optimizer,
                                                        reward_changed=reward_changed,
                                                        recurrent_migration=migrating_recurrent,
                                                        fresh_residual_ppo=bool(migrating_recurrent),
                                                        actor_mesh_mode=a.mesh_mode,
                                                        learner_world_size=learner_world_size,
                                                        qualification=str(root / "cpu-zero-reference.json")))
    hidden = model.initial_hidden(a.worlds)
    initial_actor = model.actor.weight.detach().clone()
    initial_memory = {k: v.detach().clone() for k, v in model.named_parameters() if k.startswith("memory")}
    start = train_start = time.monotonic()
    source_index = 0
    visited = [str(sources[0])]
    completed_path = rank_root / "completed-episodes.json"
    completed = json.loads(completed_path.read_text()) if resuming and completed_path.exists() else []

    def save(name, update):
        # Each physical node keeps a resumable copy. Ranks on the same node share /workspace.
        if local_rank != 0:
            return
        tmp = root / (name + ".tmp")
        torch.save(dict(model=model.state_dict(), optimizer=optimizer.state_dict(), contract=contract, update=update), tmp)
        tmp.replace(root / name)

    base_update = int(payload.get("update", 0)) if (resuming or a.preserve_parent_update) else 0
    writer = None
    if rank == 0:
        from torch.utils.tensorboard import SummaryWriter
        writer = SummaryWriter(log_dir=str(root / "tensorboard"),
                               purge_step=base_update + 1 if resuming else None)
    if not resuming:
        save("initial.pt", 0)
    if learner_world_size > 1:
        collective_barrier("initial-checkpoint-ready")
    update = base_update
    for update in range(base_update + 1, a.updates + 1):
        features, actions, logprobs, values, rewards, dones, hidden_states = [], [], [], [], [], [], []
        episode_starts = []
        grip_rewards = []
        three_finger_rewards = []
        palm_rewards = []
        sustained_lift_rewards = []
        top_contact_penalties = []
        contact_location_fractions = []
        carry_cause_fractions = []
        for step in range(a.steps):
            hidden_states.append(hidden[0])
            with torch.no_grad():
                dist, value, hidden = model.step(obs, hidden)
                raw = dist.sample()
                logp = dist.log_prob(raw).sum(-1)
            reward, done = env.step(raw)
            grip_rewards.append(torch.as_tensor(DT * GRIP_REWARD_RATE * env.step_grip_score / 25, device=device))
            three_finger_rewards.append(torch.as_tensor(
                DT * THREE_FINGER_REWARD_RATE * env.step_three_finger_score / 25,
                device=device,
            ))
            palm_rewards.append(torch.as_tensor(env.step_palm_reward, device=device))
            sustained_lift_rewards.append(torch.as_tensor(env.step_lift_above_8cm_reward, device=device))
            top_contact_penalties.append(torch.as_tensor(
                env.step_top_contact_penalty, device=device, dtype=torch.float32,
            ))
            contact_location_fractions.append(torch.as_tensor(
                env.step_contact_location_substeps / 25, device=device, dtype=torch.float32,
            ))
            carry_cause_fractions.append(torch.as_tensor(
                env.step_carry_cause_substeps / 25, device=device, dtype=torch.float32,
            ))
            features.append(obs); actions.append(raw); logprobs.append(logp); values.append(value); rewards.append(reward); dones.append(done.float())
            if env.frame >= env.length:
                report = env.diagnostics()
                if any(report["numerical_faults"]):
                    raise RuntimeError("Nonfinite CPU MuJoCo episode")
                completed.append(dict(reference=str(env.source), **report))
                atomic(rank_root / "completed-episodes.json", completed)
                if rank == 0:
                    atomic(root / "completed-episodes.json", completed)
                hidden = model.initial_hidden(a.worlds)
                episode_starts.append(step + 1)
                source_index = (source_index + 1) % len(sources)
                env.close(); gc.collect()
                env = Environment(sources[source_index], worlds=a.worlds, render=True,
                                              can_position_half_range_m=a.can_position_half_range_m,
                                              can_yaw_range_rad=a.can_yaw_range_rad,
                                              can_table_edge_margin_m=a.can_table_edge_margin_m,
                                              randomization_seed=(a.randomization_seed + rank * 1000003
                                                                  + source_index),
                                              physics_workers=worker_count, device=device)
                env.reset(randomize_can=True)
                if str(env.source) not in visited:
                    visited.append(str(env.source))
            obs = env.observation(model)
            if (step + 1) % 32 == 0:
                rollout_status = dict(stage="CPU MuJoCo recurrent PPO rollout", running=True,
                                      rank=rank, update=update, updates=a.updates, step=step + 1,
                                      worlds=a.worlds,
                                      total_steps=((update - 1) * a.steps + step + 1) * a.worlds,
                                      current_reference=str(env.source), elapsed_s=time.monotonic() - start)
                atomic(rank_root / "status.json", rollout_status)
                if rank == 0:
                    atomic(root / "status.json", {**rollout_status,
                                                   "worlds": a.worlds * learner_world_size,
                                                   "learner_world_size": learner_world_size,
                                                   "total_steps": (((update - 1) * a.steps + step + 1)
                                                                   * a.worlds * learner_world_size)})
        features = torch.stack(features); actions = torch.stack(actions); logprobs = torch.stack(logprobs)
        values = torch.stack(values); rewards = torch.stack(rewards); dones = torch.stack(dones)
        hidden_states = torch.stack(hidden_states)
        with torch.no_grad():
            _, last, _ = model.step(obs, hidden)
        adv, returns = advantages(rewards, dones, values, last)
        if learner_world_size > 1:
            moments = torch.stack((adv.double().sum(), adv.double().square().sum(),
                                   torch.tensor(float(adv.numel()), device=device, dtype=torch.double)))
            if distributed:
                tdist.all_reduce(moments, op=tdist.ReduceOp.SUM)
            else:
                moments = http_collective.reduce_tensor("moments", f"update-{update}", moments)
            mean = moments[0] / moments[2]
            variance = (moments[1] - moments[0].square() / moments[2]) / (moments[2] - 1).clamp_min(1)
            adv = (adv - mean.to(adv.dtype)) / (variance.clamp_min(0).sqrt().to(adv.dtype) + 1e-8)
        else:
            adv = (adv - adv.mean()) / (adv.std() + 1e-8)
        chunks = list(ordered_chunks(a.steps, episode_starts, a.sequence_length))
        with torch.no_grad():
            lo, hi = chunks[0]
            check, _, _ = model.sequence(features[lo:hi, :1].transpose(0, 1), hidden_states[lo, :1][None])
            logp_error = float((check.log_prob(actions[lo:hi, :1].transpose(0, 1)).sum(-1) - logprobs[lo:hi, :1].T).abs().max())
        if logp_error > 1e-3:
            raise RuntimeError(f"Recurrent rollout/sequence mismatch: {logp_error}")
        gradient_step = 0
        for epoch in range(4):
            for ci in np.random.permutation(len(chunks)):
                lo, hi = chunks[ci]
                for ids in torch.randperm(a.worlds, device=device).split(max(1, 512 // (hi - lo))):
                    dist, value, _ = model.sequence(features[lo:hi, ids].transpose(0, 1), hidden_states[lo, ids][None])
                    ratio = (dist.log_prob(actions[lo:hi, ids].transpose(0, 1)).sum(-1) - logprobs[lo:hi, ids].T).exp()
                    batch_adv = adv[lo:hi, ids].T
                    loss = (-torch.minimum(ratio * batch_adv, ratio.clamp(.8, 1.2) * batch_adv).mean()
                            + .25 * (value - returns[lo:hi, ids].T).square().mean()
                            - .001 * dist.entropy().sum(-1).mean())
                    optimizer.zero_grad(); loss.backward()
                    if distributed:
                        for parameter in model.parameters():
                            if parameter.grad is not None:
                                torch.distributed.all_reduce(parameter.grad, op=torch.distributed.ReduceOp.SUM)
                                parameter.grad.div_(learner_world_size)
                    elif http_collective is not None:
                        http_collective.reduce_gradients(f"update-{update}-step-{gradient_step}",
                                                         model.parameters())
                    gradient_step += 1
                    norm = torch.nn.utils.clip_grad_norm_(model.parameters(), 1, error_if_nonfinite=True)
                    optimizer.step()
        hidden = hidden.detach()
        change = float((model.actor.weight - initial_actor).detach().norm())
        memory_change = sum(float((v - initial_memory[k]).detach().square().sum())
                            for k, v in model.named_parameters() if k in initial_memory) ** .5
        diagnostics = env.diagnostics()
        contact_location_metrics = torch.stack(contact_location_fractions).mean((0, 1))
        carry_cause_metrics = torch.stack(carry_cause_fractions).mean((0, 1))
        scalar_metrics = torch.stack((loss.detach(), rewards.mean(), torch.stack(grip_rewards).mean(),
                                      torch.stack(three_finger_rewards).mean(),
                                      torch.stack(palm_rewards).mean(),
                                      torch.stack(sustained_lift_rewards).mean(),
                                      torch.stack(top_contact_penalties).mean(),
                                      *contact_location_metrics,
                                      *carry_cause_metrics)).to(device)
        completed_worlds = len(completed) * a.worlds
        completed_lifts = [lift for report in completed for lift in report["max_lift_m"]]
        completed_violations = [violation for report in completed for violation in report["contact_violations"]]
        completed_success = [success for report in completed for success in report["task_success"]]
        episode_counts = torch.tensor((
            completed_worlds,
            sum(lift > .01 for lift in completed_lifts),
            sum(lift > .03 for lift in completed_lifts),
            sum(lift > .08 for lift in completed_lifts),
            sum(lift > .08 and violation == 0
                for lift, violation in zip(completed_lifts, completed_violations)),
            sum(bool(success) for success in completed_success),
        ), device=device, dtype=torch.float32)
        peak_lift = torch.tensor(max(diagnostics["max_lift_m"], default=0), device=device)
        if distributed:
            torch.distributed.all_reduce(scalar_metrics, op=torch.distributed.ReduceOp.SUM)
            scalar_metrics.div_(learner_world_size)
            torch.distributed.all_reduce(episode_counts, op=torch.distributed.ReduceOp.SUM)
            torch.distributed.all_reduce(peak_lift, op=torch.distributed.ReduceOp.MAX)
        elif http_collective is not None:
            scalar_metrics = http_collective.reduce_tensor("metrics", f"update-{update}", scalar_metrics)
            episode_counts = http_collective.reduce_tensor("sum", f"episodes-{update}", episode_counts)
            peak_lift = http_collective.reduce_tensor("max", f"peak-lift-{update}", peak_lift)
        info = dict(stage="CPU MuJoCo recurrent PPO", running=True, update=update, updates=a.updates,
                    total_steps=update * a.steps * a.worlds * learner_world_size,
                    loss=float(scalar_metrics[0]),
                    mean_reward=float(scalar_metrics[1]), mean_grip_reward=float(scalar_metrics[2]),
                    mean_three_finger_reward=float(scalar_metrics[3]),
                    mean_palm_contact_reward=float(scalar_metrics[4]),
                    mean_lift_above_8cm_reward=float(scalar_metrics[5]),
                    mean_top_contact_penalty=float(scalar_metrics[6]),
                    mean_hand_can_top_contact_fraction=float(scalar_metrics[7]),
                    mean_hand_can_side_contact_fraction=float(scalar_metrics[8]),
                    mean_hand_can_bottom_contact_fraction=float(scalar_metrics[9]),
                    mean_carry_cause_hand_contact_fraction=float(scalar_metrics[10]),
                    mean_carry_cause_lost_opposed_fraction=float(scalar_metrics[11]),
                    mean_carry_cause_both_fraction=float(scalar_metrics[12]),
                    actor_weight_change_norm=change, memory_weight_change_norm=memory_change,
                    memory_gate=float(model.memory_gate.detach()), sequence_logprob_error=logp_error,
                    elapsed_s=time.monotonic() - start, training_elapsed_s=time.monotonic() - train_start,
                    training_steps_per_second=((update - base_update) * a.steps * a.worlds
                                               * learner_world_size
                                               / (time.monotonic() - train_start)),
                    learner_world_size=learner_world_size, worlds_per_rank=a.worlds,
                    current_reference=str(env.source), references_visited=visited,
                    diagnostics_scope="rank 0" if learner_world_size > 1 else "all worlds",
                    completed_worlds=int(episode_counts[0]),
                    completed_lift_gt_1cm=int(episode_counts[1]),
                    completed_lift_gt_3cm=int(episode_counts[2]),
                    completed_lift_gt_8cm=int(episode_counts[3]),
                    completed_clean_lift_gt_8cm=int(episode_counts[4]),
                    completed_full_task_success=int(episode_counts[5]),
                    peak_lift_m=float(peak_lift), diagnostics=diagnostics)
        atomic(rank_root / "status.json", {**info, "rank": rank,
                                            "diagnostics_scope": f"rank {rank}"})
        if rank == 0:
            atomic(root / "status.json", info)
            with (root / "metrics.jsonl").open("a") as f:
                f.write(json.dumps(info) + "\n")
            tb = {
                "train/loss": info["loss"],
                "reward/mean_total": info["mean_reward"],
                "reward/opposed_grip": info["mean_grip_reward"],
                "reward/all_three_finger": info["mean_three_finger_reward"],
                "reward/inside_palm": info["mean_palm_contact_reward"],
                "reward/sustained_lift_above_8cm": info["mean_lift_above_8cm_reward"],
                "penalty/top_contact": info["mean_top_contact_penalty"],
                "contact_location/top_fraction": info["mean_hand_can_top_contact_fraction"],
                "contact_location/side_fraction": info["mean_hand_can_side_contact_fraction"],
                "contact_location/bottom_fraction": info["mean_hand_can_bottom_contact_fraction"],
                "carry_cause/hand_contact_fraction": info["mean_carry_cause_hand_contact_fraction"],
                "carry_cause/lost_opposed_fraction": info["mean_carry_cause_lost_opposed_fraction"],
                "carry_cause/both_fraction": info["mean_carry_cause_both_fraction"],
                "performance/steps_per_second": info["training_steps_per_second"],
                "episodes/completed": info["completed_worlds"],
                "episodes/lift_gt_1cm": info["completed_lift_gt_1cm"],
                "episodes/lift_gt_3cm": info["completed_lift_gt_3cm"],
                "episodes/lift_gt_8cm": info["completed_lift_gt_8cm"],
                "episodes/clean_lift_gt_8cm": info["completed_clean_lift_gt_8cm"],
                "episodes/full_task_success": info["completed_full_task_success"],
                "episodes/peak_lift_m": info["peak_lift_m"],
                "health/rank0_numerical_faults": sum(diagnostics["numerical_faults"]),
                "health/rank0_contact_violations": sum(diagnostics["contact_violations"]),
            }
            for tag, value in tb.items():
                writer.add_scalar(tag, value, update, walltime=time.time())
            writer.flush()
        save("latest.pt", update)
        if update % 64 == 0:
            save(f"update-{update:04d}.pt", update)
        if learner_world_size > 1:
            collective_barrier(f"checkpoint-{update}")
        if ((a.hours and time.monotonic() - train_start >= a.hours * 3600)
                or (a.end_time_epoch and time.time() >= a.end_time_epoch)):
            break
    save("final.pt", update)
    if learner_world_size > 1:
        collective_barrier("final-checkpoint-ready")
    if rank == 0:
        completion = dict(complete=True, updates=update,
                          total_steps=update * a.steps * a.worlds * learner_world_size,
                          memory_weight_change_norm=memory_change, actor_weight_change_norm=change,
                          references_visited=visited, completed_cohorts=len(completed),
                          checkpoint=str(root / "final.pt"),
                          checkpoint_sha256=sha(root / "final.pt"), full_task_qualified=False,
                          stop_reason=("absolute deadline" if a.end_time_epoch and time.time() >= a.end_time_epoch
                                       else "duration" if a.hours else "update limit"))
        atomic(root / "completion.json", completion)
        atomic(root / "status.json", dict(stage="complete", running=False, updates=update))
        writer.close()
    env.close()
    if distributed:
        tdist.destroy_process_group()
    if http_collective is not None:
        http_collective.close()


if __name__ == "__main__":
    try:
        main()
    except Exception as exc:
        if "--root" in sys.argv:
            root = Path(sys.argv[sys.argv.index("--root") + 1])
            root.mkdir(parents=True, exist_ok=True)
            failure_rank = int(os.environ.get("RANK", os.environ.get("MESH_RANK", "0")))
            failure_root = root / "ranks" / f"rank-{failure_rank:03d}"
            failure_root.mkdir(parents=True, exist_ok=True)
            atomic(failure_root / "failure.json", dict(rank=failure_rank, error=str(exc)))
            atomic(failure_root / "status.json", dict(stage="failed", running=False,
                                                        rank=failure_rank, error=str(exc)))
            if failure_rank == 0:
                atomic(root / "failure.json", dict(rank=failure_rank, error=str(exc)))
                atomic(root / "status.json", dict(stage="failed", running=False, error=str(exc)))
        raise
