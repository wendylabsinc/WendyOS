"""Render an exact randomized native-MuJoCo rollout from a frozen checkpoint."""

from __future__ import annotations

import argparse
import json
import subprocess
import sys
import time
from pathlib import Path

import imageio_ffmpeg
import mujoco
import numpy as np
import torch
from PIL import Image, ImageDraw

from cpu_environment import CPUReferenceEnvironment
from export_visual import atomic, sha
from recurrent_residual import initialize_recurrent


def label(draw: ImageDraw.ImageDraw, xy: tuple[int, int], text: str) -> None:
    box = draw.textbbox(xy, text)
    draw.rectangle((box[0] - 4, box[1] - 4, box[2] + 4, box[3] + 4), fill="black")
    draw.text(xy, text, fill="white")


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--root", type=Path, required=True)
    parser.add_argument("--checkpoint", type=Path, required=True)
    parser.add_argument("--reference", type=Path, required=True)
    parser.add_argument("--randomization-seed", type=int, required=True)
    args = parser.parse_args()

    args.root.mkdir(parents=True, exist_ok=False)
    torch.set_num_threads(1)
    torch.manual_seed(20260914)
    started = time.monotonic()
    payload = torch.load(args.checkpoint, map_location="cpu", weights_only=False)
    contract = payload["contract"]
    model = initialize_recurrent(payload).eval()
    env = CPUReferenceEnvironment(
        args.reference,
        worlds=1,
        render=True,
        physics_workers=1,
        can_position_half_range_m=float(contract["can_randomization"]["position_half_range_m"]),
        can_yaw_range_rad=float(contract["can_randomization"]["yaw_range_rad"]),
        can_table_edge_margin_m=float(contract["can_randomization"]["table_edge_margin_m"]),
        randomization_seed=args.randomization_seed,
    )
    env.reset(randomize_can=True)
    hidden = model.initial_hidden(1)
    observation = env.observation(model)

    model_file = args.reference / "model.mjb"
    rollout_file = args.reference / "rollout.npz"
    video = args.root / "cpu-mesh-current-policy.mp4"
    ffmpeg = imageio_ffmpeg.get_ffmpeg_exe()
    encoder = subprocess.Popen(
        [
            ffmpeg,
            "-y",
            "-v",
            "error",
            "-f",
            "rawvideo",
            "-pix_fmt",
            "rgb24",
            "-s",
            "1280x720",
            "-r",
            "10",
            "-i",
            "pipe:0",
            "-an",
            "-c:v",
            "libx264",
            "-threads",
            "1",
            "-preset",
            "fast",
            "-crf",
            "22",
            "-pix_fmt",
            "yuv420p",
            "-movflags",
            "+faststart",
            str(video),
        ],
        stdin=subprocess.PIPE,
    )

    frame_indices: list[int] = []
    qpos: list[np.ndarray] = []
    qvel: list[np.ndarray] = []
    targets: list[np.ndarray] = []
    actions: list[np.ndarray] = []
    can_positions: list[np.ndarray] = []
    rewards: list[float] = []
    grip_scores: list[float] = []
    camera = mujoco.MjvCamera()
    camera.lookat[:] = [0, 0, 0.95]
    camera.distance = 2.8
    camera.azimuth = -55
    camera.elevation = -28
    env.m.vis.global_.offwidth = 960
    env.m.vis.global_.offheight = 720

    try:
        with mujoco.Renderer(env.m, 720, 960) as overview_renderer:
            for index in range(env.length):
                with torch.no_grad():
                    distribution, value, hidden = model.step(observation, hidden)
                    action = distribution.mean

                if index % 4 == 0:
                    data = env.data[0]
                    overview_renderer.update_scene(data, camera=camera)
                    overview = overview_renderer.render().copy()
                    rgb = (env.rgb[0].clamp(0, 1) * 255).byte().cpu().numpy()
                    depth = env.depth[0].cpu().numpy()
                    segmentation = env.segmentation[0].cpu().numpy()
                    visible_ids = env.visible_geoms.cpu().numpy()
                    mask = (segmentation[..., 1] == int(mujoco.mjtObj.mjOBJ_GEOM)) & np.isin(
                        segmentation[..., 0], visible_ids
                    )
                    depth_u8 = (np.clip(depth, 0, 5) / 5 * 255).astype(np.uint8)
                    depth_rgb = np.repeat(depth_u8[..., None], 3, axis=-1)
                    overlay = rgb.copy()
                    overlay[mask] = (0.4 * overlay[mask] + 0.6 * np.array([0, 255, 80])).astype(np.uint8)
                    side = np.concatenate([rgb, depth_rgb, overlay], axis=0)
                    composite = Image.fromarray(np.concatenate([overview, side], axis=1))
                    draw = ImageDraw.Draw(composite)
                    label(
                        draw,
                        (12, 12),
                        f"CPU MUJOCO + CURRENT POLICY | update {payload['update']} | randomized can | "
                        f"{index * 0.025:.1f}s | deterministic mean",
                    )
                    label(draw, (970, 12), "Policy face RGB")
                    label(draw, (970, 252), "Policy depth (0-5 m)")
                    label(draw, (970, 492), "Visible can mask - no YOLO")
                    assert encoder.stdin is not None
                    encoder.stdin.write(np.asarray(composite).tobytes())
                    frame_indices.append(index)
                    qpos.append(data.qpos.copy())
                    qvel.append(data.qvel.copy())
                    targets.append(env.targets[0].copy())
                    actions.append(action[0].cpu().numpy().copy())
                    can_positions.append(data.xpos[env.can].copy())
                    if len(frame_indices) == 1:
                        composite.save(args.root / "preview-start.jpg")

                reward, _ = env.step(action)
                rewards.append(float(reward[0]))
                grip_scores.append(float(env.step_grip_score[0] / 25))
                observation = env.observation(model)
                if index % 128 == 0 or index + 1 == env.length:
                    diagnostics = env.diagnostics()
                    if any(diagnostics["numerical_faults"]):
                        raise RuntimeError("Nonfinite native-MuJoCo video rollout")
                    atomic(
                        args.root / "status.json",
                        {
                            "stage": "rendering randomized CPU policy rollout",
                            "running": True,
                            "frame": env.frame,
                            "frames": env.length,
                            "checkpoint_update": payload["update"],
                            "elapsed_s": time.monotonic() - started,
                        },
                    )
    finally:
        assert encoder.stdin is not None
        encoder.stdin.close()
        encoder_code = encoder.wait()

    if encoder_code:
        raise RuntimeError("Video encoder failed")

    np.savez_compressed(
        args.root / "rollout.npz",
        frame=frame_indices,
        qpos=qpos,
        qvel=qvel,
        targets=targets,
        raw_action=actions,
        can=can_positions,
        rewards=rewards,
        grip_scores=grip_scores,
    )
    diagnostics = env.diagnostics()
    result = {
        "complete": True,
        "scope": "one current-checkpoint deterministic rollout on one randomized training reference; not best-of or held-out qualification",
        "checkpoint_update": payload["update"],
        "checkpoint_sha256": sha(args.checkpoint),
        "reference": str(args.reference),
        "reference_rollout_sha256": sha(rollout_file),
        "reference_model_sha256": sha(model_file),
        "randomization_seed": args.randomization_seed,
        "can_randomization": diagnostics["can_randomization"],
        "video": str(video),
        "video_sha256": sha(video),
        "duration_s": len(frame_indices) / 10,
        "control_steps": env.frame,
        "video_frames": len(frame_indices),
        "action_selection": "deterministic policy mean",
        "physics": "native CPU MuJoCo float64; video rendered directly from the same rollout",
        "diagnostics": diagnostics,
        "positive_opposed_grip_control_steps": sum(value > 0 for value in grip_scores),
        "maximum_opposed_grip_score": max(grip_scores, default=0),
        "return_total": sum(rewards),
        "simulator_yolo_loaded": any(name.startswith("ultralytics") for name in sys.modules),
        "elapsed_s": time.monotonic() - started,
    }
    atomic(args.root / "result.json", result)
    atomic(
        args.root / "status.json",
        {"stage": "complete", "running": False, "checkpoint_update": payload["update"]},
    )
    env.close()
    print(json.dumps(result), flush=True)


if __name__ == "__main__":
    try:
        main()
    except Exception as error:
        if "--root" in sys.argv:
            output_root = Path(sys.argv[sys.argv.index("--root") + 1])
            output_root.mkdir(parents=True, exist_ok=True)
            atomic(output_root / "failure.json", {"error": str(error)})
            atomic(output_root / "status.json", {"stage": "failed", "running": False, "error": str(error)})
        raise
