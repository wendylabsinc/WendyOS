"""Process-isolated native-MuJoCo actors with one centralized CUDA learner."""
import ctypes
import math
import multiprocessing as mp
import os
import traceback
from pathlib import Path

os.environ.setdefault("MUJOCO_GL", "egl")
os.environ.setdefault("PYOPENGL_PLATFORM", "egl")

import mujoco
import numpy as np
import torch

from cpu_environment import CPUReferenceEnvironment
from policy import DT, OWNED
from sensor_contract import camera_packet, joint_features


HEIGHT, WIDTH = 240, 320


def _view(raw, dtype, shape):
    return np.frombuffer(raw, dtype=dtype).reshape(shape)


def _actor_capture(env, index, buffers, worlds):
    if env.frame % 2 == 0:
        d = env.data[0]
        env.renderer.update_scene(d, camera="robot_rgbd")
        rgb = env.renderer.render().copy()
        env.renderer.enable_depth_rendering()
        env.renderer.update_scene(d, camera="robot_rgbd")
        depth = env.renderer.render().copy()
        env.renderer.disable_depth_rendering()
        env.renderer.enable_segmentation_rendering()
        env.renderer.update_scene(d, camera="robot_rgbd")
        seg = env.renderer.render().copy()
        env.renderer.disable_segmentation_rendering()
        ids = np.flatnonzero(env.m.geom_bodyid == env.can)
        mask = (seg[..., 1] == int(mujoco.mjtObj.mjOBJ_GEOM)) & np.isin(seg[..., 0], ids)
        _view(buffers["rgb"], np.uint8, (worlds, HEIGHT, WIDTH, 3))[index] = rgb
        _view(buffers["depth"], np.float32, (worlds, HEIGHT, WIDTH))[index] = depth
        _view(buffers["mask"], np.uint8, (worlds, HEIGHT, WIDTH))[index] = mask
    d = env.data[0]
    q = d.qpos[env.qa].copy()
    dq = d.qvel[env.qv].copy()
    if env.frame:
        alpha = 1 - math.exp(-2 * math.pi * 5 * DT)
        env.acc[0] += alpha * ((dq - env.previous_dq[0]) / DT - env.acc[0])
    env.previous_dq[0] = dq
    _view(buffers["q"], np.float32, (worlds, 43))[index] = q
    _view(buffers["dq"], np.float32, (worlds, 43))[index] = dq
    _view(buffers["ddq"], np.float32, (worlds, 43))[index] = env.acc[0]
    _view(buffers["offset"], np.float32, (worlds, 15))[index] = env.offset[0] / env.cap
    _view(buffers["offset_velocity"], np.float32, (worlds, 15))[index] = env.offset_velocity[0]
    _view(buffers["can"], np.float32, (worlds, 3))[index] = d.xpos[env.can]


def _actor_main(index, worlds, source, seed, position_half_range_m, yaw_range_rad,
                table_edge_margin_m, buffers, connection):
    os.environ.setdefault("MUJOCO_GL", "egl")
    try:
        torch.set_num_threads(1)
        env = CPUReferenceEnvironment(source, worlds=1, render=True, physics_workers=1,
                                      can_position_half_range_m=position_half_range_m,
                                      can_yaw_range_rad=yaw_range_rad,
                                      can_table_edge_margin_m=table_edge_margin_m,
                                      randomization_seed=seed + index * 1000003, device="cpu")
        env.reset(randomize_can=True)
        _actor_capture(env, index, buffers, worlds)
        connection.send(("ready", dict(pid=os.getpid(), length=env.length,
                                        joint_names=env.joint_names,
                                        reference_release_frame=env.reference_release_frame)))
        while True:
            command, payload = connection.recv()
            if command == "step":
                reward, done = env.step(torch.from_numpy(np.asarray(payload, dtype=np.float32))[None])
                _actor_capture(env, index, buffers, worlds)
                _view(buffers["reward"], np.float32, (worlds,))[index] = float(reward[0])
                _view(buffers["done"], np.uint8, (worlds,))[index] = bool(done[0])
                _view(buffers["grip"], np.float32, (worlds,))[index] = env.step_grip_score[0]
                _view(buffers["three_finger"], np.float32, (worlds,))[index] = env.step_three_finger_score[0]
                _view(buffers["grip_force"], np.float32, (worlds,))[index] = env.step_grip_force[0] / 25
                _view(buffers["palm_reward"], np.float32, (worlds,))[index] = env.step_palm_reward[0]
                _view(buffers["lift_reward"], np.float32, (worlds,))[index] = env.step_lift_above_8cm_reward[0]
                _view(buffers["contact_location"], np.float32, (worlds, 3))[index] = env.step_contact_location_substeps[0]
                _view(buffers["carry_causes"], np.float32, (worlds, 3))[index] = env.step_carry_cause_substeps[0]
                _view(buffers["top_penalty"], np.float32, (worlds,))[index] = env.step_top_contact_penalty[0]
                connection.send(("stepped", env.frame))
            elif command == "diagnostics":
                connection.send(("diagnostics", env.diagnostics()))
            elif command == "close":
                env.close()
                connection.send(("closed", None))
                return
            else:
                raise ValueError("Unknown actor command: " + str(command))
    except BaseException:
        try:
            connection.send(("error", traceback.format_exc()))
        finally:
            connection.close()


class CPUProcessMeshEnvironment:
    """Same learner-facing API as CPUReferenceEnvironment, one process/world."""
    def __init__(self, source, worlds=8, render=True, can_position_half_range_m=.06,
                 can_yaw_range_rad=math.pi, can_table_edge_margin_m=.005,
                 randomization_seed=20260914, physics_workers=None, device="cuda"):
        if not render:
            raise ValueError("Process mesh is the live-sensor training backend")
        if physics_workers is not None and int(physics_workers) != int(worlds):
            raise ValueError("Process mesh currently requires one actor process per world")
        self.source = Path(source)
        self.worlds = int(worlds)
        self.physics_workers = self.worlds
        self.device = torch.device(device)
        metadata = CPUReferenceEnvironment(source, worlds=1, render=False, physics_workers=1,
                                           can_position_half_range_m=can_position_half_range_m,
                                           can_yaw_range_rad=can_yaw_range_rad,
                                           can_table_edge_margin_m=can_table_edge_margin_m,
                                           randomization_seed=randomization_seed, device="cpu")
        self.length = metadata.length
        self.joint_names = metadata.joint_names
        self.references = metadata.references.copy()
        self.reference_release_frame = metadata.reference_release_frame
        self.cap = metadata.cap.copy()
        metadata.close()
        self.frame = 0
        self.randomization_seed = int(randomization_seed)
        self.can_position_half_range_m = float(can_position_half_range_m)
        self.can_yaw_range_rad = float(can_yaw_range_rad)
        self.can_table_edge_margin_m = float(can_table_edge_margin_m)
        ctx = mp.get_context("spawn")
        specs = {
            "rgb": (np.uint8, (self.worlds, HEIGHT, WIDTH, 3)),
            "depth": (np.float32, (self.worlds, HEIGHT, WIDTH)),
            "mask": (np.uint8, (self.worlds, HEIGHT, WIDTH)),
            "q": (np.float32, (self.worlds, 43)), "dq": (np.float32, (self.worlds, 43)),
            "ddq": (np.float32, (self.worlds, 43)), "offset": (np.float32, (self.worlds, 15)),
            "offset_velocity": (np.float32, (self.worlds, 15)),
            "reward": (np.float32, (self.worlds,)), "done": (np.uint8, (self.worlds,)),
            "grip": (np.float32, (self.worlds,)), "grip_force": (np.float32, (self.worlds,)),
            "three_finger": (np.float32, (self.worlds,)),
            "palm_reward": (np.float32, (self.worlds,)),
            "lift_reward": (np.float32, (self.worlds,)),
            "contact_location": (np.float32, (self.worlds, 3)),
            "carry_causes": (np.float32, (self.worlds, 3)),
            "top_penalty": (np.float32, (self.worlds,)),
            "can": (np.float32, (self.worlds, 3)),
        }
        ctype = {np.dtype(np.uint8): ctypes.c_uint8, np.dtype(np.float32): ctypes.c_float}
        self.buffers = {name: ctx.RawArray(ctype[np.dtype(dtype)], int(np.prod(shape)))
                        for name, (dtype, shape) in specs.items()}
        self.arrays = {name: _view(self.buffers[name], dtype, shape)
                       for name, (dtype, shape) in specs.items()}
        self.connections = []
        self.processes = []
        for index in range(self.worlds):
            parent, child = ctx.Pipe()
            process = ctx.Process(target=_actor_main,
                                  args=(index, self.worlds, str(source), self.randomization_seed,
                                        self.can_position_half_range_m, self.can_yaw_range_rad,
                                        self.can_table_edge_margin_m,
                                        self.buffers, child), daemon=False)
            process.start()
            child.close()
            self.connections.append(parent)
            self.processes.append(process)
        ready = [self._recv(i, "ready") for i in range(self.worlds)]
        if any(x["length"] != self.length or x["joint_names"] != self.joint_names for x in ready):
            raise RuntimeError("Actor metadata differs across process mesh")
        self.worker_pids = [x["pid"] for x in ready]
        self.vision_features = torch.zeros(self.worlds, 128, device=self.device)
        self.mask_pixels = torch.zeros(self.worlds, device=self.device)
        self.camera_timestamp = torch.zeros(self.worlds, device=self.device, dtype=torch.float64)
        self.detection_valid = torch.zeros(self.worlds, device=self.device, dtype=torch.bool)
        self.rgb = torch.zeros(self.worlds, HEIGHT, WIDTH, 3, device=self.device)
        self.depth = torch.zeros(self.worlds, HEIGHT, WIDTH, device=self.device)
        self.step_grip_score = self.arrays["grip"]
        self.step_three_finger_score = self.arrays["three_finger"]
        self.step_palm_reward = self.arrays["palm_reward"]
        self.step_lift_above_8cm_reward = self.arrays["lift_reward"]
        self.step_contact_location_substeps = self.arrays["contact_location"]
        self.step_carry_cause_substeps = self.arrays["carry_causes"]
        self.step_top_contact_penalty = self.arrays["top_penalty"]

    def _recv(self, index, expected):
        tag, payload = self.connections[index].recv()
        if tag == "error":
            raise RuntimeError(f"CPU actor {index} failed:\n{payload}")
        if tag != expected:
            raise RuntimeError(f"CPU actor {index}: expected {expected}, received {tag}")
        return payload

    def reset(self, randomize_can=False):
        # Actors are created already randomized and replaced at each episode.
        if not randomize_can or self.frame != 0:
            raise RuntimeError("Process actors reset by cohort recreation only")

    def step(self, raw):
        actions = raw.detach().cpu().numpy().astype(np.float32, copy=False)
        if actions.shape != (self.worlds, 15):
            raise ValueError("Expected one 15-D action per actor")
        for i, connection in enumerate(self.connections):
            connection.send(("step", actions[i]))
        frames = [self._recv(i, "stepped") for i in range(self.worlds)]
        if len(set(frames)) != 1:
            raise RuntimeError("CPU actor mesh lost its synchronous frame barrier")
        self.frame = frames[0]
        return (torch.as_tensor(self.arrays["reward"].copy(), device=self.device),
                torch.as_tensor(self.arrays["done"].copy(), device=self.device, dtype=torch.bool))

    @torch.no_grad()
    def observation(self, encoder):
        if self.frame % 2 == 0:
            self.rgb = torch.as_tensor(self.arrays["rgb"], device=self.device, dtype=torch.float32) / 255
            self.depth = torch.as_tensor(self.arrays["depth"], device=self.device, dtype=torch.float32)
            mask = torch.as_tensor(self.arrays["mask"], device=self.device, dtype=torch.bool)
            packet = camera_packet(self.rgb, self.depth, mask, self.frame * DT)
            self.detection_valid = packet.detection_valid
            self.camera_timestamp[:] = packet.captured_at_s
            self.vision_features[:] = encoder.encoder.vision(packet.image)
            self.mask_pixels[:] = mask.sum((-1, -2))
        q = torch.as_tensor(self.arrays["q"], device=self.device)
        dq = torch.as_tensor(self.arrays["dq"], device=self.device)
        ddq = torch.as_tensor(self.arrays["ddq"], device=self.device)
        joints = joint_features(q, dq, ddq, self.frame >= 8, self.camera_timestamp, self.frame * DT)
        i = min(self.frame, self.length - 1)
        ref = torch.as_tensor(self.references[i, OWNED], device=self.device, dtype=torch.float32)
        nxt = torch.as_tensor(self.references[min(i + 1, self.length - 1), OWNED], device=self.device, dtype=torch.float32)
        normalized = ((joints - encoder.encoder.joint_mean) / encoder.encoder.joint_std).clamp(-10, 10)
        features = torch.cat((self.vision_features, encoder.encoder.joints(normalized)), dim=-1)
        return encoder.inputs(features, ref.expand(self.worlds, -1), ((nxt - ref) / DT).expand(self.worlds, -1),
                              torch.as_tensor(self.arrays["offset"], device=self.device),
                              torch.as_tensor(self.arrays["offset_velocity"], device=self.device))

    def diagnostics(self):
        for connection in self.connections:
            connection.send(("diagnostics", None))
        reports = [self._recv(i, "diagnostics") for i in range(self.worlds)]
        list_keys = ["task_success", "max_lift_m", "final_distance_m", "contact_violations",
                     "carry_violations", "numerical_faults", "palm_contact_completed",
                     "palm_contact_best_progress", "palm_contact_streak_s",
                     "palm_contact_reward_total", "hand_can_top_contact_substeps",
                     "hand_can_side_contact_substeps", "hand_can_bottom_contact_substeps",
                     "top_contact_penalized_substeps", "top_contact_penalty_total",
                     "carry_cause_hand_contact_substeps", "carry_cause_lost_opposed_substeps",
                     "carry_cause_both_substeps"]
        result = {key: [report[key][0] for report in reports] for key in list_keys}
        offsets = np.array([report["can_randomization"]["xy_offset_min_m"] for report in reports])
        yaws = np.array([report["can_randomization"]["yaw_offset_min_rad"] for report in reports])
        result.update(frame=self.frame, worlds=self.worlds, physics_backend="CPU MuJoCo process actor mesh",
                      physics_workers=self.physics_workers, worker_pids=self.worker_pids,
                      mujoco_version=reports[0]["mujoco_version"], solver=reports[0]["solver"],
                      maximum_hand_can_normal_force_n=max(r["maximum_hand_can_normal_force_n"] for r in reports),
                      grip_reward_mean=float(np.mean([r["grip_reward_mean"] for r in reports])),
                      three_finger_reward_mean=float(
                          np.mean([r["three_finger_reward_mean"] for r in reports])
                      ),
                      palm_contact_reward_mean=float(np.mean([r["palm_contact_reward_mean"] for r in reports])),
                      lift_above_8cm_reward_mean=float(
                          np.mean([r["lift_above_8cm_reward_mean"] for r in reports])
                      ),
                      lift_above_8cm_worlds=sum(r["lift_above_8cm_worlds"] for r in reports),
                      top_contact_penalty_mean=float(
                          np.mean([r["top_contact_penalty_mean"] for r in reports])
                      ),
                      reference_release_frame=self.reference_release_frame,
                      can_randomization=dict(enabled=True, reset_count=1, seed=self.randomization_seed,
                                             position_half_range_m=self.can_position_half_range_m,
                                             yaw_range_rad=self.can_yaw_range_rad,
                                             table_edge_margin_m=self.can_table_edge_margin_m,
                                             support_geom=reports[0]["can_randomization"]["support_geom"],
                                             xy_offset_min_m=offsets.min(0).tolist(), xy_offset_max_m=offsets.max(0).tolist(),
                                             xy_offset_std_m=offsets.std(0).tolist(), yaw_offset_min_rad=float(yaws.min()),
                                             yaw_offset_max_rad=float(yaws.max()), yaw_offset_std_rad=float(yaws.std())))
        return result

    def close(self):
        for connection in self.connections:
            try:
                connection.send(("close", None))
            except (BrokenPipeError, EOFError):
                pass
        for i, process in enumerate(self.processes):
            try:
                if process.is_alive():
                    self._recv(i, "closed")
            except (BrokenPipeError, EOFError):
                pass
            process.join(timeout=5)
            if process.is_alive():
                process.terminate()
                process.join(timeout=5)
        for connection in self.connections:
            connection.close()
