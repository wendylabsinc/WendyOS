"""Virtual camera calibration, retained exposures, and optional real GL rendering."""

import math
import os
import threading
import time
from types import SimpleNamespace

import mujoco
import numpy as np
import pytest

from g1_sim.camera import CameraFrame, intrinsics
from g1_sim.sensors import CAMERA_POSITION, instrumented_model
from g1_sim.simulation import DEFAULT_ASSETS, Simulation
from g1_sim.runtime import Runtime


def test_front_camera_optical_axes_and_intrinsics_match_mujoco_model():
    model = instrumented_model(DEFAULT_ASSETS)
    data = mujoco.MjData(model)
    mujoco.mj_forward(model, data)
    camera = model.camera("front").id
    np.testing.assert_allclose(model.cam_pos[camera], CAMERA_POSITION)
    base_rotation = data.xmat[model.body("pelvis").id].reshape(3, 3)
    local_rotation = base_rotation.T @ data.cam_xmat[camera].reshape(3, 3)
    # MuJoCo axes: right = -body Y, up = body Z, backward = -body X.
    np.testing.assert_allclose(local_rotation, [[0, 0, -1], [-1, 0, 0], [0, 1, 0]], atol=1e-12)
    fx, fy, cx, cy = intrinsics()
    assert fx == fy == pytest.approx(360 / (2 * math.tan(math.radians(model.cam_fovy[camera]) / 2)))
    assert (cx, cy) == (319.5, 179.5)


def test_exposure_owns_immutable_rgb_copy_and_retains_capture_identity():
    pixels = np.full((360, 640, 3), 123, dtype=np.uint8)
    frame = CameraFrame(7, 4, 1.25, 123456789, pixels)
    pixels[:] = 0
    assert frame.epoch == 7 and frame.generation == 4
    assert frame.time == 1.25 and frame.wall_timestamp_ns == 123456789
    assert frame.frame_id == "camera_optical_frame"
    assert np.all(frame.rgb == 123)
    with pytest.raises(ValueError):
        frame.rgb[0, 0, 0] = 0
    with pytest.raises(ValueError, match="640x360 RGB uint8"):
        CameraFrame(7, 4, 1.25, 123456789, np.zeros((360, 640, 4), dtype=np.uint8))


def test_runtime_renders_only_sensor_camera_and_fences_old_exposures(monkeypatch):
    entered, finish = threading.Event(), threading.Event()
    cameras = []

    class Renderer:
        scene = SimpleNamespace(flags={})

        def __init__(self, model, *, height, width):
            assert (height, width) == (360, 640)
            self.camera = model.camera("front").id

        def update_scene(self, data, *, camera, scene_option):
            assert camera == self.camera, "the server must never render an observer view"
            assert not scene_option.geomgroup[3]
            cameras.append(camera)

        def render(self):
            entered.set()
            assert finish.wait(timeout=3)
            return np.full((360, 640, 3), 123, dtype=np.uint8)

        def close(self):
            pass

    def until(predicate):
        deadline = time.monotonic() + 3
        while time.monotonic() < deadline:
            if predicate():
                return
            time.sleep(0.01)
        raise AssertionError("timed out waiting for camera renderer")

    monkeypatch.setattr(mujoco, "Renderer", Renderer)
    runtime = Runtime(render=True)
    runtime.start()
    try:
        assert entered.wait(timeout=3)
        runtime.configure_sensors({"camera_enabled": False})
        finish.set()
        until(lambda: bool(runtime.render_stage_ms))
        assert runtime.camera_frame is None and runtime.camera_jpeg is None
        assert runtime.camera_frames == 0
        until(lambda: runtime.status()["ready"])
        assert runtime.status()["healthy"], "camera-disabled mode remains usable in the browser"
        runtime.configure_sensors({"camera_enabled": True})
        until(lambda: runtime.camera_frame is not None)
        assert isinstance(runtime.camera_frame, CameraFrame)
        assert runtime.camera_frame.generation == runtime.observation_generation
        assert runtime.camera_frame.epoch == runtime.sim.epoch
        assert np.all(runtime.camera_frame.rgb == 123)
        assert runtime.camera_jpeg.startswith(b"\xff\xd8")
        assert cameras and runtime.error is None
        assert not any(stage.startswith("observer_") for record in runtime.render_stage_ms for stage in record)
    finally:
        finish.set()
        runtime.close()


def test_sensor_renderer_failure_is_reported_even_with_browser_rendering(monkeypatch):
    def fail(*args, **kwargs):
        raise RuntimeError("test camera failure")

    monkeypatch.setattr(mujoco, "Renderer", fail)
    runtime = Runtime(render=True)
    runtime.start()
    try:
        runtime.threads[1].join(timeout=2)
        status = runtime.status()
        assert "test camera failure" in status["error"]
        assert not status["healthy"] and not status["ready"]
        assert runtime.scene.state_json(runtime), "the 3D viewer can still inspect the world"
    finally:
        runtime.close()


@pytest.mark.skipif(os.environ.get("G1_TEST_RENDER") != "1", reason="requires an OpenGL renderer; use MUJOCO_GL=osmesa in the image")
@pytest.mark.parametrize("detail", ["full", "balanced"])
def test_front_camera_is_clear_and_movable_box_changes_actual_rgb(detail):
    sim = Simulation(model=instrumented_model(DEFAULT_ASSETS))
    for _ in range(500):
        sim.step()
    model = instrumented_model(DEFAULT_ASSETS)
    model.vis.quality.offsamples = 0
    data = mujoco.MjData(model)
    data.qpos[:] = sim.data.qpos
    data.qvel[:] = sim.data.qvel
    data.mocap_pos[:] = sim.data.mocap_pos
    data.mocap_quat[:] = sim.data.mocap_quat
    mujoco.mj_forward(model, data)
    camera = model.camera("front").id
    options = mujoco.MjvOption()
    options.geomgroup[2] = True  # Original robot visual geometry remains visible.
    options.geomgroup[3] = False  # Collision proxies are not camera surfaces.
    robot_geoms = np.flatnonzero(model.body_rootid[model.geom_bodyid] == model.body("pelvis").id)
    with mujoco.Renderer(model, height=360, width=640) as renderer:
        def scene():
            renderer.update_scene(data, camera=camera, scene_option=options)
            renderer.scene.flags[mujoco.mjtRndFlag.mjRND_SHADOW] = False
            renderer.scene.flags[mujoco.mjtRndFlag.mjRND_REFLECTION] = False
        scene()
        before = renderer.render().copy()
        renderer.enable_segmentation_rendering()
        scene()
        segments = renderer.render()
        object_ids = segments[:, :, 0][segments[:, :, 1] == mujoco.mjtObj.mjOBJ_GEOM]
        assert not np.isin(object_ids, robot_geoms).any(), "front camera is occluded by a robot mesh"
        renderer.disable_segmentation_rendering()
        obstacle = model.body("sandbox_obstacle").id
        data.mocap_pos[model.body_mocapid[obstacle]] = [2, 0, .4]
        mujoco.mj_forward(model, data)
        scene()
        after = renderer.render().copy()
        assert np.mean(np.abs(before.astype(float) - after)) > 3
        # The forward box must cover its analytically projected optical center.
        position = data.cam_xmat[camera].reshape(3, 3).T @ (np.array([2, 0, .4]) - data.cam_xpos[camera])
        fx, fy, cx, cy = intrinsics()
        u, v = round(cx + fx * position[0] / -position[2]), round(cy - fy * position[1] / -position[2])
        red, green, blue = after[v, u].astype(float)
        assert red > green * 1.3 and green > blue * 1.3
