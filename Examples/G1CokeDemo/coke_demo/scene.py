"""Target-controlled Coke training world for a Wendy app in a VM.

The model, initial integration state, controller gains and 43-joint ordering
come from the supplied attempt-000001 expert bundle. The pelvis is fixed and
the original waist constraints remain active. This is not the missing
attempt-000159 model used to collect the frozen policy's reference.

Create, step, observe and close on one worker thread. Callers serialize access
to scene_description/scene_state with that worker. Select MUJOCO_GL before
importing this module. Rendering starts lazily on the first observation.
"""
from __future__ import annotations

import hashlib
import json
from pathlib import Path
import time

import mujoco
import numpy as np

from .simulation import Simulation


CONTROL_DT = 0.025
CAMERA_PERIOD_FRAMES = 2
WIDTH, HEIGHT = 320, 240
_GEOM_TYPES = {
    int(getattr(mujoco.mjtGeom, "mjGEOM_" + name.upper())): name
    for name in ("plane", "sphere", "capsule", "ellipsoid", "cylinder", "box", "mesh")
}


def _numbers(values):
    return np.round(np.asarray(values, dtype=np.float64), 7).reshape(-1).tolist()


def _xyzw(values):
    return _numbers(np.asarray(values)[..., [1, 2, 3, 0]])


class CokeScene(Simulation):
    """Real contact physics driven by caller-supplied 43-joint PD targets.

    ``step`` never advances the reference internally. As in the supplied
    controller, legs and the left hand hold their initial joint positions;
    upper-body joints and the right hand track the supplied targets, with
    waist roll/pitch constrained by the model. The caller can run the
    frozen policy or supply expert targets for a separate replay comparison.
    ``observation`` returns arrays copied from the current physical state.
    Camera arrays describe a genuine 20 Hz exposure and keep that exposure's
    frame/time when reused by the 40 Hz controller.
    """

    def __init__(self, expert: Path):
        self._renderer = None
        self._description = None
        self.epoch = 1
        self._camera = None
        super().__init__(Path(expert))
        if (self.model.nq, self.model.nv, self.model.nu) != (50, 49, 43):
            raise ValueError("Expected the fixed-base 43-joint G1 with a free can")
        self.qv = self.model.jnt_dofadr[[self.model.joint(name).id for name in self.names]]
        self.target_bounds = np.asarray(self.result["joint_bounds"], dtype=np.float64)
        self._can_geoms = np.flatnonzero(self.model.geom_bodyid == self.can)
        self._camera_id = self.model.camera("robot_rgbd").id
        self._pose_data = mujoco.MjData(self.model)
        pelvis = self.model.body("pelvis").id
        self._robot_bodies = {pelvis}
        for body in range(pelvis + 1, self.model.nbody):
            if int(self.model.body_parentid[body]) in self._robot_bodies:
                self._robot_bodies.add(body)

    def reset(self):
        super().reset()
        self.epoch += 1
        self._camera = None

    def step(self, target_q43=None):
        if target_q43 is None:
            if self.frame >= len(self.references):
                return False
            target_q43 = self.references[self.frame]
        target = np.asarray(target_q43, dtype=np.float64)
        if target.shape != (43,) or not np.isfinite(target).all():
            raise ValueError("target_q43 must contain 43 finite joint positions")
        if np.any(target < self.target_bounds[:, 0] - 1e-6) or np.any(target > self.target_bounds[:, 1] + 1e-6):
            raise ValueError("target_q43 exceeds the recorded joint bounds")
        target = np.clip(target, self.target_bounds[:, 0], self.target_bounds[:, 1])
        for _ in range(25):
            base = (100 * (self.start[self.order] - self.data.qpos[self.aq])
                    - 4 * self.data.qvel[self.av] + self.data.qfrc_bias[self.av])
            pd = (self.kp * (target[self.order] - self.data.qpos[self.aq])
                  - self.kd * self.data.qvel[self.av])
            torque = np.where(self.owned, pd, base)
            torque[self.locked] = 0
            self.data.ctrl[:] = np.clip(torque, self.bounds[:, 0], self.bounds[:, 1])
            mujoco.mj_step(self.model, self.data)
            self.max_lift = max(self.max_lift, float(self.data.xpos[self.can, 2] - self.initial_can[2]))
        if not np.isfinite(self.data.qpos).all() or not np.isfinite(self.data.qvel).all():
            raise RuntimeError("Simulation produced non-finite state")
        self.frame += 1
        return True

    def observation(self):
        # Forward a private data buffer: refreshing camera/pose transforms must
        # not alter the live solver's warm-start or controller bias state.
        signature = mujoco.mjtState.mjSTATE_INTEGRATION
        state = np.empty(mujoco.mj_stateSize(self.model, signature))
        mujoco.mj_getState(self.model, self.data, state, signature)
        mujoco.mj_setState(self.model, self._pose_data, state, signature)
        mujoco.mj_forward(self.model, self._pose_data)
        if self._camera is None or self.frame - self._camera["camera_frame"] >= CAMERA_PERIOD_FRAMES:
            if self._renderer is None:
                self._renderer = mujoco.Renderer(self.model, HEIGHT, WIDTH)
            renderer = self._renderer
            captured_at_ns = time.time_ns()
            renderer.update_scene(self._pose_data, camera=self._camera_id)
            rgb = renderer.render().copy()
            renderer.enable_depth_rendering()
            try:
                renderer.update_scene(self._pose_data, camera=self._camera_id)
                depth = renderer.render().copy()
            finally:
                renderer.disable_depth_rendering()
            renderer.enable_segmentation_rendering()
            try:
                renderer.update_scene(self._pose_data, camera=self._camera_id)
                segmentation = renderer.render().copy()
            finally:
                renderer.disable_segmentation_rendering()
            mask = ((segmentation[..., 1] == int(mujoco.mjtObj.mjOBJ_GEOM))
                    & np.isin(segmentation[..., 0], self._can_geoms))
            if not np.isfinite(depth).all():
                raise RuntimeError("Camera produced non-finite depth")
            normalized_depth = np.clip(np.rint(depth * 1000) * .001, 0, 5) / 5
            image = np.concatenate((rgb.astype(np.float32).transpose(2, 0, 1) / 255,
                                    normalized_depth[None], mask.astype(np.float32)[None]), axis=0)[None]
            self._camera = {
                "image": image.astype(np.float32), "rgb_u8": rgb, "depth_m": depth,
                "mask": mask, "detection_valid": bool(mask.any()),
                "camera_frame": self.frame, "camera_sim_time": self.frame * CONTROL_DT,
                "camera_captured_at_ns": captured_at_ns,
                "camera_position": self._pose_data.cam_xpos[self._camera_id].copy(),
                "camera_rotation": self._pose_data.cam_xmat[self._camera_id].reshape(3, 3).copy(),
                "camera_fovy": float(self.model.cam_fovy[self._camera_id]),
            }
        pelvis = self.model.body("pelvis").id
        base_rotation = self._pose_data.xmat[pelvis].reshape(3, 3)
        return {
            **{key: value.copy() if isinstance(value, np.ndarray) else value
               for key, value in self._camera.items()},
            "q43": self.data.qpos[self.qa].copy(),
            "dq43": self.data.qvel[self.qv].copy(),
            "joint_names": self.names.copy(), "frame": self.frame,
            "sim_time": self.frame * CONTROL_DT, "epoch": self.epoch,
            "joint_effort43": self._pose_data.qfrc_actuator[self.qv].copy(),
            "base_position": self._pose_data.xpos[pelvis].copy(),
            "base_quaternion_wxyz": self._pose_data.xquat[pelvis].copy(),
            "base_linear_velocity": np.zeros(3), "base_angular_velocity": np.zeros(3),
            "imu_specific_force": base_rotation.T @ -self.model.opt.gravity,
            "fixed_base": True,
            "body_transforms": [
                {"name": self.model.body(body).name, "parent": "world",
                 "position": self._pose_data.xpos[body].copy(),
                 "quaternion_wxyz": self._pose_data.xquat[body].copy()}
                for body in sorted(self._robot_bodies) if body != pelvis
            ],
        }

    def scene_description(self):
        """Return Wendy browser geometry in metres, Z-up, XYZW quaternions.

        Compiled mesh vertices already contain MuJoCo scaling and centering.
        Robot group 1 contains visual surfaces; group 0 duplicates are collision
        proxies. Environment group 0 contains the desks, decal and can surfaces.
        """
        if self._description is not None:
            return self._description
        model = self.model
        robot = model.body("pelvis").id
        result = {
            "version": 1, "robot_body": robot,
            "bodies": [{"id": body, "name": model.body(body).name} for body in range(model.nbody)],
            "geoms": [], "meshes": [],
            "camera": {
                "body": int(model.cam_bodyid[self._camera_id]),
                "position": _numbers(model.cam_pos[self._camera_id]),
                "quaternion": _xyzw(model.cam_quat[self._camera_id]),
                "fovy": float(model.cam_fovy[self._camera_id]),
            },
            "provenance": {"scene": "attempt-000001", "robot": "fixed-base G1 with Dex3 hands",
                           "joint_count": 43, "physics_hz": 1000, "control_hz": 40,
                           "camera_hz": 20, "equality_constraints": int(model.neq)},
        }
        mesh_ids = set()
        for geom in range(model.ngeom):
            body = int(model.geom_bodyid[geom])
            if body in self._robot_bodies and int(model.geom_group[geom]) != 1:
                continue
            kind = _GEOM_TYPES.get(int(model.geom_type[geom]))
            if kind is None:
                raise ValueError(f"Unsupported browser geom type {model.geom_type[geom]}")
            material = int(model.geom_matid[geom])
            rgba = model.geom_rgba[geom]
            if material >= 0 and np.array_equal(rgba, [.5, .5, .5, 1]):
                rgba = model.mat_rgba[material]
            if rgba[3] <= 0:
                continue
            surface = {
                "name": model.geom(geom).name, "body": body, "type": kind,
                "size": _numbers(model.geom_size[geom]), "position": _numbers(model.geom_pos[geom]),
                "quaternion": _xyzw(model.geom_quat[geom]), "rgba": _numbers(rgba),
            }
            if kind == "mesh":
                mesh = int(model.geom_dataid[geom])
                surface["mesh"] = mesh
                mesh_ids.add(mesh)
            result["geoms"].append(surface)
        for mesh in sorted(mesh_ids):
            vertex, face = int(model.mesh_vertadr[mesh]), int(model.mesh_faceadr[mesh])
            result["meshes"].append({
                "id": mesh,
                "positions": _numbers(model.mesh_vert[vertex:vertex + model.mesh_vertnum[mesh]]),
                "indices": model.mesh_face[face:face + model.mesh_facenum[mesh]].reshape(-1).tolist(),
            })
        result["id"] = hashlib.sha256(json.dumps(result, separators=(",", ":"), allow_nan=False).encode()).hexdigest()
        self._description = result
        return result

    def scene_state(self):
        self._pose_data.qpos[:] = self.data.qpos
        self._pose_data.mocap_pos[:] = self.data.mocap_pos
        self._pose_data.mocap_quat[:] = self.data.mocap_quat
        mujoco.mj_kinematics(self.model, self._pose_data)
        return {
            "scene_id": self.scene_description()["id"], "epoch": self.epoch,
            "frame": self.frame, "time": self.frame * CONTROL_DT,
            "positions": _numbers(self._pose_data.xpos), "quaternions": _xyzw(self._pose_data.xquat),
        }

    def metrics(self):
        result = super().metrics()
        result.update(controller="caller-supplied PD targets", fixed_base=True,
                      camera_hz=20, camera_frame=None if self._camera is None else self._camera["camera_frame"])
        return result

    def close(self):
        if self._renderer is not None:
            self._renderer.close()
            self._renderer = None
