"""Native MuJoCo backend for the current recurrent reference-residual policy.

Physics runs in native CPU MuJoCo; rendering uses its OpenGL backend. Frozen visual encoders, the GRU,
actor, critic, and PPO tensors remain on CUDA. Privileged simulator state is
used only for rewards and audits, never policy observations.
"""
import collections
import json
import math
from concurrent.futures import ThreadPoolExecutor
from pathlib import Path

import mujoco
import numpy as np
import torch

from contact_location import (
    CONTACT_LOCATION_NAMES,
    TOP_CONTACT_MIN_NORMAL_FORCE_N,
    classify_can_contact,
    top_contact_penalty,
)
from policy import DT, OWNED
from sensor_contract import camera_packet, freeze_contract, joint_features, visible_can_mask
from grip_reward import (
    CAN_TRACKING_REWARD_RATE,
    GRIP_REWARD_RATE,
    JOINT_TRACKING_REWARD_RATE,
    LIFT_ABOVE_8CM_REWARD_RATE,
    THREE_FINGER_REWARD_RATE,
    SECURE_SIDE_FORCE_N,
    lift_above_threshold_score,
    reference_grip_schedule,
)
from palm_reward import (
    PALM_CONTACT_MIN_SUBSTEP_FRACTION,
    eligible_palm_contact,
    is_inside_palm_region,
    update_palm_progress,
)

import sys
sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "recurrent_bc" / "baseline"))
from closed_loop_contract import HAND, UPPER
from export_visual import sha

from rgbd_evaluation import ContactAudit


STATE = mujoco.mjtState.mjSTATE_INTEGRATION


def can_reference_shift_weights(recorded_can, origin, destination):
    recorded = np.asarray(recorded_can, dtype=np.float64)
    origin = np.asarray(origin, dtype=np.float64)
    destination = np.asarray(destination, dtype=np.float64)
    initial_distance = float(np.linalg.norm(origin[:2] - destination[:2]))
    if initial_distance < 1e-6:
        raise ValueError("Can origin and destination must differ")
    weights = np.clip(np.linalg.norm(recorded[:, :2] - destination[:2], axis=1) / initial_distance, 0, 1)
    weights = np.minimum.accumulate(weights)
    weights[0] = 1
    return weights.astype(np.float64)


def can_support_bounds(model, data, can_geom, origin, half_range_m, edge_margin_m):
    origin = np.asarray(origin, dtype=np.float64)
    can_radius = float(model.geom_size[can_geom, 0])
    can_bottom = origin[2] - float(model.geom_size[can_geom, 1])
    candidates = []
    for geom in range(model.ngeom):
        name = model.geom(geom).name or ""
        if model.geom_type[geom] != mujoco.mjtGeom.mjGEOM_BOX or "table" not in name:
            continue
        rotation = np.asarray(data.geom_xmat[geom]).reshape(3, 3)
        center = np.asarray(data.geom_xpos[geom])
        local = rotation.T @ (origin - center)
        half = np.asarray(model.geom_size[geom])
        top = center[2] + float((np.abs(rotation[2]) * half).sum())
        if abs(can_bottom - top) > .015 or np.any(np.abs(local[:2]) > half[:2] + can_radius):
            continue
        usable = half[:2] - can_radius - edge_margin_m
        low = np.maximum(-half_range_m, -usable - local[:2])
        high = np.minimum(half_range_m, usable - local[:2])
        if np.all(usable > 0) and np.all(high > low):
            candidates.append((abs(can_bottom - top), geom, rotation[:2, :2], low, high))
    if not candidates:
        raise RuntimeError("Could not bound randomized can pose on its support table")
    _, geom, rotation, low, high = min(candidates, key=lambda x: x[0])
    return geom, rotation, low, high


class CPUReferenceEnvironment:
    def __init__(self, source, worlds=4, render=True, can_position_half_range_m=.06,
                 can_yaw_range_rad=math.pi, can_table_edge_margin_m=.005,
                 randomization_seed=20260914, physics_workers=1, device="cuda"):
        if str(device).startswith("cuda") and not torch.cuda.is_available():
            raise RuntimeError("CUDA learner required")
        if worlds < 1:
            raise ValueError("World count must be positive")
        self.source = Path(source)
        self.worlds = int(worlds)
        self.physics_workers = min(int(physics_workers), self.worlds)
        if self.physics_workers < 1:
            raise ValueError("Physics worker count must be positive")
        self.device = torch.device(device)
        self.host_model = mujoco.MjModel.from_binary_path(str(self.source / "model.mjb"))
        self.m = self.host_model
        self.data = [mujoco.MjData(self.m) for _ in range(self.worlds)]
        report = json.loads((self.source / "result.json").read_text())
        verification = json.loads((self.source / "verification.json").read_text())
        assert verification["passed"]
        assert sha(self.source / "rollout.npz") == verification["rollout_sha256"]
        assert sha(self.source / "model.mjb") == verification["model_sha256"]
        with np.load(self.source / "rollout.npz") as z:
            self.initial_state = z["initial_state"].copy()
            trace = z["trace"].copy()
            names = z["joint_names"].tolist()
            recorded_can = z["can"][:, :3].copy()
        if self.m.opt.timestep != .001 or names[12] != "waist_yaw_joint":
            raise ValueError("Unexpected reference timing or joint order")
        self.length = len(trace)
        self.reference_hash = verification["rollout_sha256"]
        self.references = trace[:, 87:].astype(np.float64)
        self.reference_q = trace[:, 1:44].astype(np.float64)
        self.reference_can = recorded_can.astype(np.float64)
        self.joint_names = names
        ids = np.array([self.m.joint(n).id for n in names])
        aj = self.m.actuator_trnid[:, 0]
        order = np.array([names.index(self.m.joint(int(j)).name) for j in aj])
        self.qa = self.m.jnt_qposadr[ids]
        self.qv = self.m.jnt_dofadr[ids]
        self.aq = self.m.jnt_qposadr[aj]
        self.av = self.m.jnt_dofadr[aj]
        self.order = order
        self.pd_owned = np.isin(order, UPPER) | np.isin(order, HAND)
        self.locked = np.isin(order, [13, 14])
        controller = report["controller"]
        kp = np.full(43, 60.0)
        kd = np.full(43, 1.5)
        kp[[19, 20, 21, 33, 34, 35]] = 40.0
        kp[HAND] = controller.get("hand_kp_by_joint", [controller["hand_kp"]] * 7)
        kd[HAND] = controller["hand_kd"]
        self.kp = kp[order]
        self.kd = kd[order]
        self.torque_bounds = self.m.jnt_actfrcrange[aj].copy()
        self.bounds = np.asarray(report["joint_bounds"], dtype=np.float64)
        self.cap = .15 * (self.bounds[OWNED, 1] - self.bounds[OWNED, 0]) / 2
        mujoco.mj_setState(self.m, self.data[0], self.initial_state, STATE)
        mujoco.mj_forward(self.m, self.data[0])
        self.start = self.data[0].qpos[self.qa].copy()
        self.origin = np.asarray(report["can_initial"], dtype=np.float64)
        self.destination = np.r_[report["marker"][:2], self.origin[2]].astype(np.float64)
        grip = reference_grip_schedule(self.references[:, HAND], recorded_can, self.origin, self.destination)
        self.reference_grip_enabled = grip.astype(bool)
        self.reference_release_frame = int(np.flatnonzero(~grip)[0])
        self.open_hand = self.references[-1, HAND].copy()
        self.can = self.m.body("bottle_body").id
        self.right_palm = self.m.body("right_wrist_yaw_link").id
        self.can_geom = self.m.geom("bottle").id
        self.can_half_height_m = float(self.m.geom_size[self.can_geom, 1])
        can_joint = int(self.m.body(self.can).jntadr[0])
        self.can_qpos = int(self.m.jnt_qposadr[can_joint])
        self.can_dof = int(self.m.jnt_dofadr[can_joint])
        self.can_position_half_range_m = float(can_position_half_range_m)
        self.can_yaw_range_rad = float(can_yaw_range_rad)
        self.can_table_edge_margin_m = float(can_table_edge_margin_m)
        if self.can_position_half_range_m < 0 or not 0 <= self.can_yaw_range_rad <= math.pi:
            raise ValueError("Invalid can randomization")
        support, rotation, low, high = can_support_bounds(
            self.m, self.data[0], self.can_geom, self.origin,
            self.can_position_half_range_m, self.can_table_edge_margin_m)
        self.can_support_geom_name = self.m.geom(support).name
        self.can_support_rotation_xy = rotation
        self.can_delta_low_local = low
        self.can_delta_high_local = high
        self.reference_shift_weight = can_reference_shift_weights(recorded_can, self.origin, self.destination)
        self.rng = np.random.default_rng(int(randomization_seed) ^ int(self.reference_hash[:8], 16))
        self.randomization_seed = int(randomization_seed)
        self.can_reset_count = 0
        audit = ContactAudit(self.m)
        self.allowed_pairs = audit.allowed_pairs
        self.body_names = audit.body_names
        self.geom_body = self.m.geom_bodyid.copy()
        self.front_table = self.m.body("table_body").id
        self.visible_geoms = torch.as_tensor(np.flatnonzero(self.m.geom_bodyid == self.can), device=self.device)
        self.renderer = mujoco.Renderer(self.m, 240, 320) if render else None
        self.render_enabled = render
        self.rgb = torch.zeros(self.worlds, 240, 320, 3, device=self.device)
        self.depth = torch.zeros(self.worlds, 240, 320, device=self.device)
        self.segmentation = torch.zeros(self.worlds, 240, 320, 2, device=self.device, dtype=torch.int32)
        self.vision_features = torch.zeros(self.worlds, 128, device=self.device)
        self.mask_pixels = torch.zeros(self.worlds, device=self.device)
        self.camera_timestamp = torch.zeros(self.worlds, device=self.device, dtype=torch.float64)
        # MuJoCo permits one immutable MjModel to be stepped concurrently with
        # independent MjData instances. The main thread remains the sole owner
        # of EGL rendering and CUDA policy state.
        self.executor = ThreadPoolExecutor(max_workers=self.physics_workers) if self.physics_workers > 1 else None
        self.reset()

    def reset(self, randomize_can=False):
        self.can_xy_offset = np.zeros((self.worlds, 2), dtype=np.float64)
        self.can_yaw_offset = np.zeros(self.worlds, dtype=np.float64)
        if randomize_can:
            self.can_reset_count += 1
            unit = self.rng.random((self.worlds, 2))
            local = self.can_delta_low_local + unit * (self.can_delta_high_local - self.can_delta_low_local)
            self.can_xy_offset = local @ self.can_support_rotation_xy.T
            self.can_yaw_offset = self.rng.uniform(-self.can_yaw_range_rad, self.can_yaw_range_rad, self.worlds)
        for i, d in enumerate(self.data):
            mujoco.mj_setState(self.m, d, self.initial_state, STATE)
            if randomize_can:
                d.qpos[self.can_qpos:self.can_qpos + 2] += self.can_xy_offset[i]
                old = d.qpos[self.can_qpos + 3:self.can_qpos + 7].copy()
                c, s = math.cos(self.can_yaw_offset[i] / 2), math.sin(self.can_yaw_offset[i] / 2)
                d.qpos[self.can_qpos + 3:self.can_qpos + 7] = [
                    c * old[0] - s * old[3], c * old[1] - s * old[2],
                    c * old[2] + s * old[1], c * old[3] + s * old[0]]
                d.qacc_warmstart[:] = 0
            mujoco.mj_forward(self.m, d)
        self.initial_can_position = np.stack([d.qpos[self.can_qpos:self.can_qpos + 3].copy() for d in self.data])
        z = lambda *shape: np.zeros(shape, dtype=np.float64)
        self.offset = z(self.worlds, 15)
        self.offset_velocity = z(self.worlds, 15)
        self.targets = np.tile(self.references[0], (self.worlds, 1))
        self.previous_target = self.targets.copy()
        self.previous_velocity = z(self.worlds, 43)
        self.previous_dq = z(self.worlds, 43)
        self.acc = z(self.worlds, 43)
        self.violations = z(self.worlds)
        self.step_violations = z(self.worlds)
        self.carry_bad = z(self.worlds)
        self.contact_location_substeps = z(self.worlds, len(CONTACT_LOCATION_NAMES))
        self.step_contact_location_substeps = z(self.worlds, len(CONTACT_LOCATION_NAMES))
        self.top_contact_penalized_substeps = z(self.worlds)
        self.step_top_contact_penalized_substeps = z(self.worlds)
        self.top_contact_penalty_total = z(self.worlds)
        self.step_top_contact_penalty = z(self.worlds)
        self.carry_cause_substeps = z(self.worlds, 3)
        self.step_carry_cause_substeps = z(self.worlds, 3)
        self.supported = np.zeros(self.worlds, dtype=np.int64)
        self.grasp = z(self.worlds, 7)
        self.have_grasp = np.zeros(self.worlds, dtype=bool)
        self.released = np.zeros(self.worlds, dtype=bool)
        self.lifted = np.zeros(self.worlds, dtype=bool)
        self.landed = np.zeros(self.worlds, dtype=bool)
        self.max_lift = z(self.worlds)
        self.max_grip_force = z(self.worlds)
        self.step_grip_score = z(self.worlds)
        self.step_three_finger_score = z(self.worlds)
        self.step_grip_force = z(self.worlds)
        self.step_palm_inside_substeps = z(self.worlds)
        self.step_palm_reward = z(self.worlds)
        self.step_lift_above_8cm_reward = z(self.worlds)
        self.palm_contact_streak_s = z(self.worlds)
        self.palm_contact_best_progress = z(self.worlds)
        self.palm_contact_reward_total = z(self.worlds)
        self.palm_contact_complete = np.zeros(self.worlds, dtype=bool)
        self.numerical_faults = np.zeros(self.worlds, dtype=bool)
        self.detection_valid = torch.zeros(self.worlds, device=self.device, dtype=torch.bool)
        self.frame = 0

    def _contact(self, world, d, grip_enabled):
        flags = np.zeros(7, dtype=bool)
        contact_locations = np.zeros(len(CONTACT_LOCATION_NAMES), dtype=bool)
        # Independent digit forces are required for the all-three bonus.
        # Index 3 remains total right-hand/can force for diagnostics only.
        forces = np.zeros(4, dtype=np.float64)
        palm_normal_force = 0.0
        force = np.zeros(6, dtype=np.float64)
        for j in range(d.ncon):
            con = d.contact[j]
            g1, g2 = int(con.geom1), int(con.geom2)
            b1, b2 = int(self.geom_body[g1]), int(self.geom_body[g2])
            mujoco.mj_contactForce(self.m, d, j, force)
            magnitude = float(np.linalg.norm(force[:3]))
            if not self.allowed_pairs[g1, g2] and (-float(con.dist) > .001 or magnitude > 2):
                flags[5] = True
            if b1 != self.can and b2 != self.can:
                continue
            other = b2 if b1 == self.can else b1
            name = self.body_names[other]
            thumb = "right_hand_thumb" in name
            index = "right_hand_index" in name
            middle = "right_hand_middle" in name
            opposing = index or middle
            hand = "right_hand" in name or "right_wrist" in name
            table = "table" in name
            flags |= [thumb, opposing, hand, table, other == self.front_table, False, table and other != self.front_table]
            normal = max(0.0, float(force[0]))
            if thumb:
                forces[0] += normal
            if index:
                forces[1] += normal
            if middle:
                forces[2] += normal
            if hand:
                forces[3] += normal
                if normal >= TOP_CONTACT_MIN_NORMAL_FORCE_N:
                    can_local = d.geom_xmat[self.can_geom].reshape(3, 3).T @ (
                        con.pos - d.geom_xpos[self.can_geom]
                    )
                    contact_locations[classify_can_contact(
                        can_local, self.can_half_height_m,
                    )] = True
            if other == self.right_palm:
                palm_local = d.xmat[self.right_palm].reshape(3, 3).T @ (
                    con.pos - d.xpos[self.right_palm]
                )
                if is_inside_palm_region(palm_local):
                    palm_normal_force += normal
        self.contact_location_substeps[world] += contact_locations
        self.step_contact_location_substeps[world] += contact_locations
        penalized_top_contact = bool(
            grip_enabled and not self.released[world] and contact_locations[0]
        )
        self.top_contact_penalized_substeps[world] += penalized_top_contact
        self.step_top_contact_penalized_substeps[world] += penalized_top_contact
        self.violations[world] += flags[5]
        self.step_violations[world] += flags[5]
        self.step_grip_force[world] += forces[3]
        self.max_grip_force[world] = max(self.max_grip_force[world], forces[3])
        if grip_enabled and not self.released[world]:
            opposing_force = forces[1] + forces[2]
            self.step_grip_score[world] += np.clip(
                min(forces[0], opposing_force) / SECURE_SIDE_FORCE_N, 0, 1,
            )
            self.step_three_finger_score[world] += np.clip(
                min(forces[0], forces[1], forces[2]) / SECURE_SIDE_FORCE_N, 0, 1,
            )
        pos = d.xpos[self.can]
        upright = float(d.xmat[self.can].reshape(3, 3)[2, 2])
        speed = float(np.linalg.norm(d.qvel[self.can_dof:self.can_dof + 3]))
        lift = float(pos[2] - self.initial_can_position[world, 2])
        if eligible_palm_contact(palm_normal_force, lift):
            self.step_palm_inside_substeps[world] += 1
        opposed = flags[0] and flags[1]
        if not self.have_grasp[world] and opposed and lift > .03:
            self.have_grasp[world] = True
            self.grasp[world] = self.targets[world, HAND]
        corridor = np.linalg.norm(pos[:2] - self.destination[:2]) < .045 and abs(pos[2] - self.destination[2]) < .020 and upright > .95
        direction = self.open_hand - self.grasp[world]
        length = float(np.linalg.norm(direction))
        opening = float((self.targets[world, HAND] - self.grasp[world]) @ direction / max(length, 1e-12))
        self.released[world] |= self.have_grasp[world] and corridor and length > .02 and opening >= .02
        allowed = self.released[world] and corridor
        self.lifted[world] |= lift > .03
        active = self.lifted[world] and not self.landed[world]
        landing = active and flags[4] and not flags[6] and corridor and speed < (.64641839 if allowed else .1)
        hand_contact_cause = active and not landing and flags[3]
        lost_opposed_cause = (
            active and not landing and not opposed
            and not (allowed and speed < .64641839)
        )
        both_causes = hand_contact_cause and lost_opposed_cause
        causes = np.asarray(
            [hand_contact_cause, lost_opposed_cause, both_causes], dtype=np.float64,
        )
        self.carry_cause_substeps[world] += causes
        self.step_carry_cause_substeps[world] += causes
        bad = hand_contact_cause or lost_opposed_cause
        self.carry_bad[world] += bad
        self.landed[world] |= landing
        self.supported[world] = self.supported[world] + 1 if (not flags[2] and flags[4]) else 0
        self.max_lift[world] = max(self.max_lift[world], lift)

    def step(self, raw):
        raw = raw.detach().cpu().numpy()
        if raw.shape != (self.worlds, 15):
            raise ValueError("Expected one 15-D action per world")
        ref = self.references[min(self.frame, self.length - 1)]
        desired = np.clip(self.cap * np.tanh(raw), self.bounds[OWNED, 0] - ref[OWNED], self.bounds[OWNED, 1] - ref[OWNED])
        if self.frame:
            wanted = np.clip((desired - self.offset) / DT, -.025, .025)
            self.offset_velocity += np.clip(wanted - self.offset_velocity, -.05 * DT, .05 * DT)
            self.offset = np.clip(self.offset + self.offset_velocity * DT, -self.cap, self.cap)
        self.targets[:] = ref
        self.targets[:, OWNED] = np.clip(ref[OWNED] + self.offset, self.bounds[OWNED, 0], self.bounds[OWNED, 1])
        self.offset = self.targets[:, OWNED] - ref[OWNED]
        self.step_grip_score.fill(0)
        self.step_three_finger_score.fill(0)
        self.step_grip_force.fill(0)
        self.step_palm_inside_substeps.fill(0)
        self.step_palm_reward.fill(0)
        self.step_lift_above_8cm_reward.fill(0)
        self.step_contact_location_substeps.fill(0)
        self.step_top_contact_penalized_substeps.fill(0)
        self.step_top_contact_penalty.fill(0)
        self.step_carry_cause_substeps.fill(0)
        self.step_violations.fill(0)
        grip_enabled = bool(self.reference_grip_enabled[min(self.frame, self.length - 1)])
        if self.executor is None:
            for i in range(self.worlds):
                self._advance_world(i, grip_enabled)
        else:
            list(self.executor.map(self._advance_world, range(self.worlds), [grip_enabled] * self.worlds))
        self.frame += 1
        velocity = (self.targets - self.previous_target) / DT
        acceleration = (velocity - self.previous_velocity) / DT
        excess = np.mean(np.maximum(np.abs(velocity) - .25, 0) ** 2, axis=-1)
        excess += .05 * np.mean(np.maximum(np.abs(acceleration) - .5, 0) ** 2, axis=-1)
        idx = min(self.frame, self.length - 1)
        q = np.stack([d.qpos[self.qa] for d in self.data])
        qerror = np.mean((q[:, OWNED] - self.reference_q[idx, OWNED]) ** 2, axis=-1)
        position = np.stack([d.xpos[self.can].copy() for d in self.data])
        target_can = self.reference_can[idx] + np.pad(self.can_xy_offset, ((0, 0), (0, 1))) * self.reference_shift_weight[idx]
        can_error = np.sum((position - target_can) ** 2, axis=-1)
        reward = DT * (JOINT_TRACKING_REWARD_RATE * np.exp(-30 * qerror)
                       + CAN_TRACKING_REWARD_RATE * np.exp(-80 * can_error)
                       - .1 * np.mean((self.offset / self.cap) ** 2, axis=-1) - .05 * excess)
        reward -= DT * 5 * self.step_violations / 25
        reward += DT * GRIP_REWARD_RATE * self.step_grip_score / 25
        reward += DT * THREE_FINGER_REWARD_RATE * self.step_three_finger_score / 25
        lift = position[:, 2] - self.initial_can_position[:, 2]
        self.step_lift_above_8cm_reward[:] = (
            DT * LIFT_ABOVE_8CM_REWARD_RATE * lift_above_threshold_score(lift)
        )
        reward += self.step_lift_above_8cm_reward
        self.step_top_contact_penalty[:] = top_contact_penalty(
            self.step_top_contact_penalized_substeps, 25, DT,
        )
        self.top_contact_penalty_total += self.step_top_contact_penalty
        reward += self.step_top_contact_penalty
        palm_fraction = self.step_palm_inside_substeps / 25
        for world in range(self.worlds):
            eligible = (
                palm_fraction[world] >= PALM_CONTACT_MIN_SUBSTEP_FRACTION
                and not self.lifted[world]
            )
            streak, best, complete, palm_reward = update_palm_progress(
                self.palm_contact_streak_s[world],
                self.palm_contact_best_progress[world],
                bool(self.palm_contact_complete[world]),
                bool(eligible),
                DT,
            )
            self.palm_contact_streak_s[world] = streak
            self.palm_contact_best_progress[world] = best
            self.palm_contact_complete[world] = complete
            self.step_palm_reward[world] = palm_reward
            self.palm_contact_reward_total[world] += palm_reward
        reward += self.step_palm_reward
        done = np.full(self.worlds, self.frame >= self.length, dtype=bool)
        reward += 5 * (self.task_success() & done)
        for i, d in enumerate(self.data):
            self.numerical_faults[i] |= not (np.isfinite(d.qpos).all() and np.isfinite(d.qvel).all())
        reward = np.where(self.numerical_faults, -100, reward)
        done |= self.numerical_faults
        self.previous_target[:] = self.targets
        self.previous_velocity[:] = velocity
        return torch.as_tensor(reward, device=self.device, dtype=torch.float32), torch.as_tensor(done, device=self.device)

    def _advance_world(self, i, grip_enabled):
        """Run one actor's 25 native-MuJoCo substeps on its worker thread."""
        d = self.data[i]
        for _ in range(25):
            base = 100 * (self.start[self.order] - d.qpos[self.aq]) - 4 * d.qvel[self.av] + d.qfrc_bias[self.av]
            pd = self.kp * (self.targets[i, self.order] - d.qpos[self.aq]) - self.kd * d.qvel[self.av]
            torque = np.where(self.pd_owned, pd, base)
            torque[self.locked] = 0
            d.ctrl[:] = np.clip(torque, self.torque_bounds[:, 0], self.torque_bounds[:, 1])
            mujoco.mj_step(self.m, d)
            self._contact(i, d, grip_enabled)

    def task_success(self):
        pos = np.stack([d.xpos[self.can].copy() for d in self.data])
        upright = np.array([d.xmat[self.can].reshape(3, 3)[2, 2] for d in self.data])
        speed = np.array([np.linalg.norm(d.qvel[self.can_dof:self.can_dof + 3]) for d in self.data])
        return ((self.max_lift > .08) & (np.linalg.norm(pos[:, :2] - self.destination[:2], axis=-1) < .045) &
                (upright > .95) & (speed < .03) & self.released & self.lifted & self.landed &
                (self.carry_bad == 0) & (self.violations == 0) & (self.supported >= 1000) & ~self.numerical_faults)

    @torch.no_grad()
    def observation(self, encoder):
        if not self.render_enabled:
            raise RuntimeError("Rendering is disabled")
        if self.frame % 2 == 0:
            rgbs, depths, segs = [], [], []
            for d in self.data:
                self.renderer.update_scene(d, camera="robot_rgbd")
                rgbs.append(self.renderer.render().copy())
                self.renderer.enable_depth_rendering()
                self.renderer.update_scene(d, camera="robot_rgbd")
                depths.append(self.renderer.render().copy())
                self.renderer.disable_depth_rendering()
                self.renderer.enable_segmentation_rendering()
                self.renderer.update_scene(d, camera="robot_rgbd")
                segs.append(self.renderer.render().copy())
                self.renderer.disable_segmentation_rendering()
            self.rgb = torch.as_tensor(np.stack(rgbs), device=self.device, dtype=torch.float32) / 255
            self.depth = torch.as_tensor(np.stack(depths), device=self.device, dtype=torch.float32)
            self.segmentation = torch.as_tensor(np.stack(segs), device=self.device, dtype=torch.int32)
            mask = visible_can_mask(self.segmentation, self.visible_geoms, int(mujoco.mjtObj.mjOBJ_GEOM))
            packet = camera_packet(self.rgb, self.depth, mask, self.frame * DT)
            self.detection_valid = packet.detection_valid
            self.camera_timestamp[:] = packet.captured_at_s
            self.vision_features[:] = encoder.encoder.vision(packet.image)
            self.mask_pixels[:] = mask.sum((-1, -2))
        q = torch.as_tensor(np.stack([d.qpos[self.qa] for d in self.data]), device=self.device, dtype=torch.float32)
        dq_np = np.stack([d.qvel[self.qv] for d in self.data])
        if self.frame:
            alpha = 1 - math.exp(-2 * math.pi * 5 * DT)
            self.acc += alpha * ((dq_np - self.previous_dq) / DT - self.acc)
        dq = torch.as_tensor(dq_np, device=self.device, dtype=torch.float32)
        ddq = torch.as_tensor(self.acc, device=self.device, dtype=torch.float32)
        joints = joint_features(q, dq, ddq, self.frame >= 8, self.camera_timestamp, self.frame * DT)
        self.previous_dq[:] = dq_np
        i = min(self.frame, self.length - 1)
        ref = torch.as_tensor(self.references[i, OWNED], device=self.device, dtype=torch.float32)
        nxt = torch.as_tensor(self.references[min(i + 1, self.length - 1), OWNED], device=self.device, dtype=torch.float32)
        normalized = ((joints - encoder.encoder.joint_mean) / encoder.encoder.joint_std).clamp(-10, 10)
        features = torch.cat((self.vision_features, encoder.encoder.joints(normalized)), dim=-1)
        return encoder.inputs(features, ref.expand(self.worlds, -1), ((nxt - ref) / DT).expand(self.worlds, -1),
                              torch.as_tensor(self.offset / self.cap, device=self.device, dtype=torch.float32),
                              torch.as_tensor(self.offset_velocity, device=self.device, dtype=torch.float32))

    def diagnostics(self):
        pos = np.stack([d.xpos[self.can].copy() for d in self.data])
        return dict(
            frame=self.frame, worlds=self.worlds,
            physics_backend="CPU MuJoCo actor mesh", physics_workers=self.physics_workers,
            mujoco_version=mujoco.__version__,
            solver=dict(tolerance=float(self.m.opt.tolerance), iterations=int(self.m.opt.iterations),
                        solver=int(self.m.opt.solver), integrator=int(self.m.opt.integrator)),
            can_randomization=dict(enabled=self.can_reset_count > 0, reset_count=self.can_reset_count,
                                   seed=self.randomization_seed, position_half_range_m=self.can_position_half_range_m,
                                   yaw_range_rad=self.can_yaw_range_rad, table_edge_margin_m=self.can_table_edge_margin_m,
                                   support_geom=self.can_support_geom_name, xy_offset_min_m=self.can_xy_offset.min(0).tolist(),
                                   xy_offset_max_m=self.can_xy_offset.max(0).tolist(), xy_offset_std_m=self.can_xy_offset.std(0).tolist(),
                                   yaw_offset_min_rad=float(self.can_yaw_offset.min()), yaw_offset_max_rad=float(self.can_yaw_offset.max()),
                                   yaw_offset_std_rad=float(self.can_yaw_offset.std())),
            task_success=self.task_success().tolist(), max_lift_m=self.max_lift.tolist(),
            final_distance_m=np.linalg.norm(pos[:, :2] - self.destination[:2], axis=-1).tolist(),
            contact_violations=self.violations.tolist(), carry_violations=self.carry_bad.tolist(),
            hand_can_top_contact_substeps=self.contact_location_substeps[:, 0].tolist(),
            hand_can_side_contact_substeps=self.contact_location_substeps[:, 1].tolist(),
            hand_can_bottom_contact_substeps=self.contact_location_substeps[:, 2].tolist(),
            top_contact_penalized_substeps=self.top_contact_penalized_substeps.tolist(),
            top_contact_penalty_total=self.top_contact_penalty_total.tolist(),
            carry_cause_hand_contact_substeps=self.carry_cause_substeps[:, 0].tolist(),
            carry_cause_lost_opposed_substeps=self.carry_cause_substeps[:, 1].tolist(),
            carry_cause_both_substeps=self.carry_cause_substeps[:, 2].tolist(),
            numerical_faults=self.numerical_faults.tolist(), maximum_hand_can_normal_force_n=float(self.max_grip_force.max()),
            grip_reward_mean=float((DT * GRIP_REWARD_RATE * self.step_grip_score / 25).mean()),
            three_finger_reward_mean=float(
                (DT * THREE_FINGER_REWARD_RATE * self.step_three_finger_score / 25).mean()
            ),
            palm_contact_reward_mean=float(self.step_palm_reward.mean()),
            lift_above_8cm_reward_mean=float(self.step_lift_above_8cm_reward.mean()),
            lift_above_8cm_worlds=int((self.step_lift_above_8cm_reward > 0).sum()),
            top_contact_penalty_mean=float(self.step_top_contact_penalty.mean()),
            palm_contact_completed=self.palm_contact_complete.tolist(),
            palm_contact_best_progress=self.palm_contact_best_progress.tolist(),
            palm_contact_streak_s=self.palm_contact_streak_s.tolist(),
            palm_contact_reward_total=self.palm_contact_reward_total.tolist(),
            reference_release_frame=self.reference_release_frame)

    def close(self):
        if self.executor is not None:
            self.executor.shutdown(wait=True, cancel_futures=True)
        if self.renderer is not None:
            self.renderer.close()
