"""Derived sensor scene and physics observations, without modifying pinned assets.

Mounts and ray pattern describe Wendy's virtual attachment, not calibrated G1
factory extrinsics. ROS conversion lives separately from MuJoCo sampling.
"""

from pathlib import Path
import math
import time
import xml.etree.ElementTree as ET

import mujoco
import numpy as np


IMU_POSITION = (0.0, 0.0, 0.0)
LIDAR_POSITION = (0.16, 0.0, -0.10)
CAMERA_POSITION = (0.12, 0.0, 0.45)
SCAN_COUNT = 360
SCAN_MIN = 0.10
SCAN_MAX = 12.0
SCAN_ANGLES = np.linspace(-math.pi, math.pi, SCAN_COUNT, endpoint=False)


def instrumented_model(asset_dir, *, sandbox=True, visual_mesh_dir=None):
    """Add massless sensors and optional room geometry to the matching model."""
    robot_dir = Path(asset_dir).resolve() / "robot"
    robot = ET.parse(robot_dir / "g1_29dof.xml").getroot()
    scene = ET.parse(robot_dir / "scene_29dof.xml").getroot()
    scene.remove(scene.find("include"))
    robot.find("compiler").set("meshdir", str(Path(visual_mesh_dir).resolve()
                                              if visual_mesh_dir else robot_dir / "meshes"))
    base = robot.find("worldbody/body[@name='pelvis']")
    # Ray group selection excludes every robot link, including collision geoms.
    # This is a visualization/raycast group change, not a contact mask change.
    for geom in base.iter("geom"):
        geom.set("group", "2" if geom.get("contype") == "0" and geom.get("conaffinity") == "0" else "3")
    for name, position in (("lidar", LIDAR_POSITION),):
        ET.SubElement(base, "site", name=name, pos=" ".join(map(str, position)),
                      size="0.003", rgba="0 0 0 0", group="4")
    # MuJoCo camera looks along -Z with +Y up: optical right=-bodyY,
    # optical up=bodyZ, optical backward=-bodyX.
    ET.SubElement(base, "camera", name="front", pos=" ".join(map(str, CAMERA_POSITION)),
                  xyaxes="0 -1 0 0 0 1", fovy="60")
    sensors = robot.find("sensor")
    ET.SubElement(sensors, "accelerometer", name="imu_acceleration", site="imu")
    if sandbox:
        world = scene.find("worldbody")
        for name, position, size in (
            ("east_wall", "6 0 1", "0.1 6.1 1"),
            ("west_wall", "-6 0 1", "0.1 6.1 1"),
            ("north_wall", "0 6 1", "6 0.1 1"),
            ("south_wall", "0 -6 1", "6 0.1 1"),
        ):
            ET.SubElement(world, "geom", name=name, type="box", pos=position,
                          size=size, rgba="0.55 0.64 0.68 1", group="0")
        obstacle = ET.SubElement(world, "body", name="sandbox_obstacle", mocap="true", pos="2.5 1.5 0.4")
        ET.SubElement(obstacle, "geom", name="obstacle", type="box", size="0.4 0.5 0.4",
                      rgba="0.85 0.44 0.18 1", group="0")
    for child in robot:
        scene.append(child)
    return mujoco.MjModel.from_xml_string(ET.tostring(scene, encoding="unicode"))


class PhysicsSampler:
    """One worker owns its copied data; physics never waits for raycasting."""

    def __init__(self, simulation):
        self.sim = simulation
        self.model = simulation.model
        self.data = mujoco.MjData(self.model)
        self.signature = mujoco.mjtState.mjSTATE_INTEGRATION
        self.integration_state = np.empty(mujoco.mj_stateSize(self.model, self.signature))
        self.epoch = None
        self.lidar_id = self.model.site("lidar").id
        self.geom_group = np.array([1, 0, 0, 0, 0, 0], dtype=np.uint8)

    def capture(self, runtime):
        with runtime.lock:
            if self.sim.mode in {"paused", "fault"}:
                return False
            mujoco.mj_getState(self.model, self.sim.data, self.integration_state, self.signature)
            self.epoch = self.sim.epoch
            self.mode = self.sim.mode
            self.control_mode = getattr(self.sim, "control_mode", "sport")
            self.wall_timestamp_ns = time.time_ns()
        mujoco.mj_setState(self.model, self.data, self.integration_state, self.signature)
        # mj_step leaves derived data at the pre-integration state. Recompute
        # on the private copy so all reported fields describe this sample time.
        mujoco.mj_forward(self.model, self.data)
        return True

    def state(self):
        data, sim = self.data, self.sim
        rotation = data.xmat[sim.body_id].reshape(3, 3)
        return {
            "epoch": self.epoch, "time": float(data.time),
            "mode": self.mode, "control_mode": self.control_mode,
            "wall_timestamp_ns": self.wall_timestamp_ns,
            "position": data.qpos[:3].copy(),
            "quaternion_wxyz": data.qpos[3:7].copy(),
            "linear_velocity_body": rotation.T @ data.qvel[:3],
            "angular_velocity_body": data.sensor("imu_gyro").data.copy(),
            "specific_force_body": data.sensor("imu_acceleration").data.copy(),
            "joint_names": sim.joint_names,
            "joint_position": data.qpos[sim.qaddr].copy(),
            "joint_velocity": data.qvel[sim.vaddr].copy(),
            "joint_acceleration": data.qacc[sim.vaddr].copy(),
            "joint_effort": data.qfrc_actuator[sim.vaddr].copy(),
        }

    def scan(self):
        origin = self.data.site_xpos[self.lidar_id]
        rotation = self.data.site_xmat[self.lidar_id].reshape(3, 3)
        geom_id = np.empty(1, dtype=np.int32)
        ranges = np.full(SCAN_COUNT, np.inf, dtype=np.float32)
        for index, angle in enumerate(SCAN_ANGLES):
            direction = rotation @ np.array([math.cos(angle), math.sin(angle), 0.0])
            distance = mujoco.mj_ray(self.model, self.data, origin, direction,
                                     self.geom_group, True, -1, geom_id)
            if SCAN_MIN <= distance <= SCAN_MAX:
                ranges[index] = distance
        return ranges
