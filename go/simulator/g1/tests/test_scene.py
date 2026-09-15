"""Browser geometry fidelity, shared kinematics, and fault-safe wire state."""

from concurrent.futures import ThreadPoolExecutor
import gzip
import json
import threading
from types import SimpleNamespace

import mujoco
import numpy as np
import pytest

from g1_sim.scene import BrowserScene, export_scene


@pytest.fixture
def world():
    model = mujoco.MjModel.from_xml_string('''
      <mujoco>
        <asset>
          <material name="red" rgba="0.8 0.1 0.2 1"/>
          <mesh name="tetra" vertex="0 0 0  1 0 0  0 2 0  0 0 3"
                face="0 2 1  0 1 3  0 3 2  1 2 3" scale="0.2 0.3 0.4"/>
        </asset>
        <worldbody>
          <geom name="floor" type="plane" size="0 0 0.1"/>
          <geom name="proxy" type="box" size="0.1 0.1 0.1" group="3"/>
          <body name="moving" pos="1 2 3">
            <freejoint/>
            <geom name="mesh" type="mesh" mesh="tetra" material="red" pos="0.3 0.2 0.1" euler="20 30 40"/>
            <geom name="mesh_copy" type="mesh" mesh="tetra" rgba="0.1 0.2 0.8 1" pos="1 0 0"/>
            <camera name="front" pos="0.4 0 0.04" xyaxes="0 -1 0 0 0 1" fovy="60"/>
          </body>
          <body name="obstacle" mocap="true" pos="2 3 0.4">
            <geom name="box" type="box" size="0.4 0.5 0.4" material="red"/>
          </body>
        </worldbody>
      </mujoco>''')
    data = mujoco.MjData(model)
    mujoco.mj_forward(model, data)
    runtime = SimpleNamespace(lock=threading.RLock(), observation_generation=0,
                              sim=SimpleNamespace(model=model, data=data, epoch=1, mode="standing"))
    return model, data, runtime


def test_scene_exports_compiled_mesh_frames_materials_and_deduplicates_assets(world):
    model, data, _ = world
    scene = export_scene(model)
    assert scene == export_scene(model), "model identity must survive reloads"
    assert len(scene["meshes"]) == 1
    assert "proxy" not in {geom["name"] for geom in scene["geoms"]}
    assert scene["camera"]["body"] == model.body("moving").id
    assert scene["camera"]["quaternion"] == pytest.approx([-0.5, 0.5, 0.5, -0.5])
    assert scene["camera"]["fovy"] == 60
    geoms = {geom["name"]: geom for geom in scene["geoms"]}
    assert geoms["box"]["size"] == [0.4, 0.5, 0.4], "box dimensions are MuJoCo half extents"
    assert geoms["mesh"]["rgba"] == pytest.approx([0.8, 0.1, 0.2, 1])
    assert geoms["mesh_copy"]["rgba"] == pytest.approx([0.1, 0.2, 0.8, 1])
    mesh = scene["meshes"][0]
    vertices = np.array(mesh["positions"]).reshape(-1, 3)
    assert min(mesh["indices"]) >= 0 and max(mesh["indices"]) < len(vertices)
    for name in ("mesh", "mesh_copy"):
        geom = geoms[name]
        compiled = model.geom(name).id
        q = np.array(geom["quaternion"])[[3, 0, 1, 2]]
        rotation = np.empty(9)
        mujoco.mju_quat2Mat(rotation, q)
        local = vertices @ rotation.reshape(3, 3).T + geom["position"]
        world_vertices = local @ data.xmat[geom["body"]].reshape(3, 3).T + data.xpos[geom["body"]]
        expected = model.mesh_vert @ data.geom_xmat[compiled].reshape(3, 3).T + data.geom_xpos[compiled]
        np.testing.assert_allclose(world_vertices, expected, atol=2e-7)


def test_snapshots_recompute_post_integration_pose_and_share_one_cache(world, monkeypatch):
    model, data, runtime = world
    scene = BrowserScene(model)
    monkeypatch.setattr("g1_sim.scene.time.monotonic", lambda: 1.0)
    # Mutating qpos without forward mimics mj_step's stale derived transforms.
    data.qpos[:3] = [4, 5, 6]
    data.qpos[3:7] = [2 ** -0.5, 0, 0, 2 ** -0.5]
    data.mocap_pos[0] = [-1, -2, 0.4]
    initial_derived = data.xpos.copy()
    with ThreadPoolExecutor(max_workers=8) as clients:
        payloads = list(clients.map(lambda _: scene.state_json(runtime), range(20)))
    assert len(set(payloads)) == 1 and scene.samples == 1
    state = json.loads(payloads[0])
    assert state["valid"] and state["scene_id"] == scene.description["id"]
    positions = np.array(state["positions"]).reshape(-1, 3)
    quaternions = np.array(state["quaternions"]).reshape(-1, 4)
    np.testing.assert_allclose(positions[model.body("moving").id], [4, 5, 6])
    np.testing.assert_allclose(positions[model.body("obstacle").id], [-1, -2, 0.4])
    np.testing.assert_allclose(quaternions[model.body("moving").id], [0, 0, 2 ** -0.5, 2 ** -0.5], atol=1e-7)
    np.testing.assert_array_equal(data.xpos, initial_derived), "HTTP never forwards live physics data"
    assert json.loads(gzip.decompress(scene.gzip)) == scene.description


def test_lifecycle_changes_invalidate_cache_and_nonfinite_state_retains_last_pose(world, monkeypatch):
    model, data, runtime = world
    scene = BrowserScene(model)
    monkeypatch.setattr("g1_sim.scene.time.monotonic", lambda: 1.0)
    initial = json.loads(scene.state_json(runtime))
    runtime.sim.mode = "paused"
    runtime.observation_generation += 1
    paused = json.loads(scene.state_json(runtime))
    assert paused["mode"] == "paused" and paused["generation"] == 1
    data.qpos[0] = float("nan")
    runtime.sim.mode = "fault"
    fault = json.loads(scene.state_json(runtime))
    assert fault["valid"] is False and fault["mode"] == "fault"
    assert fault["positions"] == initial["positions"]
    assert fault["quaternions"] == initial["quaternions"]
    assert "NaN" not in scene.cached_json.decode()
    mujoco.mj_resetData(model, data)
    runtime.sim.epoch += 1
    runtime.observation_generation += 1
    runtime.sim.mode = "standing"
    reset = json.loads(scene.state_json(runtime))
    assert reset["epoch"] == 2 and reset["generation"] == 2 and reset["valid"]
    assert scene.samples == 4
