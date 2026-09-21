"""Browser lidar shares physical rays and keeps lifecycle/fault boundaries."""

from concurrent.futures import ThreadPoolExecutor
import json
import threading
from types import SimpleNamespace

import mujoco
import numpy as np
import pytest

from go2_sim.browser_lidar import BrowserLidar, MAX_AGE
from go2_sim.lidar import Lidar
from go2_sim.sensors import PhysicsSampler, instrumented_model
from go2_sim.simulation import DEFAULT_ASSETS, Simulation


@pytest.fixture
def world():
    sim = Simulation(model=instrumented_model(DEFAULT_ASSETS))
    runtime = SimpleNamespace(
        sim=sim, lock=threading.RLock(), stop_event=threading.Event(),
        scene=SimpleNamespace(description={"id": "test-scene"}), seed=123,
        observation_generation=0, sensor_generation=0,
        sensor_settings={"lidar_enabled": True, "lidar_dropout": 0.0, "camera_enabled": True},
    )
    return runtime, BrowserLidar(runtime)


def state(browser):
    return json.loads(browser.state_json())


def test_world_points_use_actual_rotated_mount_and_private_physics_outside_lock(world, monkeypatch):
    runtime, browser = world
    sim = runtime.sim
    sim.data.qpos[:3] = [0.4, -0.3, 0.34]
    sim.data.qpos[3:7] = [2 ** -0.5, 0, 0, 2 ** -0.5]
    sim.data.mocap_pos[0] = [2, 0, 0.4]
    original_qpos = sim.data.qpos.copy()
    original_derived = sim.data.geom_xpos.copy()
    original_forward, original_sample = mujoco.mj_forward, Lidar.sample

    def forward(*args, **kwargs):
        assert not runtime.lock._is_owned(), "sensor dynamics must not block the physics lock"
        return original_forward(*args, **kwargs)

    def sample(*args, **kwargs):
        assert not runtime.lock._is_owned(), "rays must not block the physics lock"
        return original_sample(*args, **kwargs)

    monkeypatch.setattr(mujoco, "mj_forward", forward)
    monkeypatch.setattr(Lidar, "sample", sample)
    cloud = state(browser)
    sampler = PhysicsSampler(sim)
    sampler.capture(runtime)
    native = Lidar(seed=123).sample(sampler)
    origin = sampler.data.site_xpos[sampler.lidar_id]
    rotation = sampler.data.site_xmat[sampler.lidar_id].reshape(3, 3)
    np.testing.assert_allclose(cloud["origin"], [0.4, -0.02, 0.42], atol=1e-7)
    np.testing.assert_allclose(np.array(cloud["points"]).reshape(-1, 3), native["xyz"] @ rotation.T + origin, atol=5.1e-6)
    np.testing.assert_allclose(cloud["quaternion"], [0, 0, 2 ** -0.5, 2 ** -0.5], atol=1e-7)
    assert cloud["fresh"] and cloud["enabled"] and cloud["available"]
    assert len(cloud["points"]) > 360 * 3, "all five real elevation rings contribute"
    assert cloud["scene_id"] == "test-scene" and cloud["epoch"] == sim.epoch
    np.testing.assert_array_equal(sim.data.qpos, original_qpos)
    np.testing.assert_array_equal(sim.data.geom_xpos, original_derived)


def test_multiple_clients_share_one_bounded_ray_capture(world):
    runtime, browser = world
    first = state(browser)
    with ThreadPoolExecutor(max_workers=8) as clients:
        clouds = list(clients.map(lambda _: state(browser), range(16)))
    assert browser.samples == 1
    assert all(cloud["points"] == first["points"] for cloud in clouds)
    browser.record["captured_at"] -= 0.11
    runtime.sim.data.time += 0.1
    assert state(browser)["fresh"] and browser.samples == 2
    browser.record["captured_at"] -= MAX_AGE + 0.1
    assert not state(browser)["fresh"] and browser.samples == 2, "frozen physics cannot refresh lidar"


def test_disabled_pause_fault_reset_and_moved_obstacle_fence_returns(world):
    runtime, browser = world
    initial = state(browser)
    runtime.sim.data.mocap_pos[0] = [2, 0, 0.4]
    runtime.observation_generation += 1
    moved = state(browser)
    assert moved["generation"] == 1 and moved["points"] != initial["points"]
    runtime.sensor_settings["lidar_enabled"] = False
    runtime.sensor_generation += 1
    runtime.observation_generation += 1
    disabled = state(browser)
    assert not disabled["enabled"] and not disabled["fresh"] and disabled["points"] == []
    assert disabled["origin"] is None
    runtime.sensor_settings["lidar_enabled"] = True
    runtime.sim.pause()
    runtime.observation_generation += 1
    paused = state(browser)
    assert paused["mode"] == "paused" and not paused["fresh"] and paused["points"] == []
    runtime.sim.mode = "fault"
    runtime.sim.data.qpos[0] = float("nan")
    runtime.sim.data.time = float("nan")
    fault = state(browser)
    assert fault["mode"] == "fault" and not fault["fresh"] and fault["points"] == []
    assert fault["time"] is None and b"NaN" not in browser.state_json()
    runtime.sim.reset()
    runtime.observation_generation += 1
    reset = state(browser)
    assert reset["epoch"] != initial["epoch"] and reset["fresh"]
    assert reset["points"] == initial["points"]


def test_dropout_controls_use_reproducible_seed_and_never_invent_returns(world):
    runtime, browser = world
    runtime.sensor_settings["lidar_dropout"] = 0.5
    first = state(browser)
    browser.record["captured_at"] -= 0.11
    runtime.sim.data.time += 0.1
    second = state(browser)
    assert first["points"] and second["points"] != first["points"]
    runtime.sim.reset()
    runtime.observation_generation += 1
    assert state(browser)["points"] == first["points"]
    runtime.sensor_settings["lidar_dropout"] = 1.0
    runtime.sensor_generation += 1
    runtime.observation_generation += 1
    dropped = state(browser)
    assert dropped["enabled"] and dropped["fresh"] and dropped["points"] == []
    assert dropped["origin"] is not None


def test_lifecycle_change_during_raycast_rejects_old_generation(world, monkeypatch):
    runtime, browser = world
    original = Lidar.sample

    def sample(lidar, sampler):
        result = original(lidar, sampler)
        with runtime.lock:
            runtime.observation_generation += 1
        return result

    monkeypatch.setattr(Lidar, "sample", sample)
    rejected = state(browser)
    assert rejected["generation"] == 1 and not rejected["fresh"] and rejected["points"] == []
    assert browser.samples == 0
    monkeypatch.setattr(Lidar, "sample", original)
    assert state(browser)["fresh"] and browser.samples == 1


def test_ros_reuses_exact_published_cloud_and_expires_stale_captures(world, monkeypatch):
    runtime, _ = world
    browser = BrowserLidar(runtime, ros=True)
    sampler = PhysicsSampler(runtime.sim)
    sampler.capture(runtime)
    result = Lidar(seed=123, dropout=0.5).sample(sampler)
    prepared = browser.prepare(result, sampler, runtime.observation_generation)
    browser.publish(prepared)
    monkeypatch.setattr(browser, "_sample", lambda: pytest.fail("ROS browser must not cast extra rays"))
    expected = result["xyz"] @ sampler.data.site_xmat[sampler.lidar_id].reshape(3, 3).T + sampler.data.site_xpos[sampler.lidar_id]
    np.testing.assert_allclose(np.array(state(browser)["points"]).reshape(-1, 3), expected, atol=5.1e-6)
    result["xyz"][:] = 100
    np.testing.assert_allclose(np.array(state(browser)["points"]).reshape(-1, 3), expected, atol=5.1e-6)
    browser.record["captured_at"] -= MAX_AGE + 0.1
    stale = state(browser)
    assert not stale["fresh"] and stale["points"] == [] and stale["origin"] is None
    runtime.observation_generation += 1
    browser.publish(prepared)
    assert not state(browser)["fresh"], "old generations cannot revive an expired cloud"
