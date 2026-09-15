"""Point/scan coherence and actual MuJoCo world intersections."""

import math
from types import SimpleNamespace
import threading
import time

import mujoco
import numpy as np
import pytest

from go2_sim.lidar import DIRECTIONS, HORIZONTAL_RING, Lidar, RAY_COUNT
from go2_sim.sensors import LIDAR_POSITION, SCAN_COUNT, SCAN_MAX, SCAN_MIN, PhysicsSampler, instrumented_model
from go2_sim.simulation import DEFAULT_ASSETS, Simulation


@pytest.fixture
def world():
    sim = Simulation(model=instrumented_model(DEFAULT_ASSETS))
    sampler = PhysicsSampler(sim)
    runtime = SimpleNamespace(lock=threading.RLock())
    assert sampler.capture(runtime)
    return sim, sampler, runtime


def point_for(result, ray_index):
    matching = np.flatnonzero(result["ray_indices"] == ray_index)
    assert len(matching) == 1, f"ray {ray_index} did not have exactly one finite cloud return"
    return result["xyz"][matching[0]]


def test_horizontal_ring_matches_existing_scan_and_cloud_ranges(world):
    _, sampler, _ = world
    result = Lidar().sample(sampler)
    np.testing.assert_array_equal(result["ranges"], sampler.scan())
    assert result["ranges"].dtype == np.float32
    assert result["xyz"].dtype == np.float32
    assert result["xyz"].shape[1] == 3
    assert np.isfinite(result["xyz"]).all()
    assert len(result["xyz"]) > SCAN_COUNT
    for azimuth, distance in enumerate(result["ranges"]):
        point = point_for(result, HORIZONTAL_RING * SCAN_COUNT + azimuth)
        assert np.linalg.norm(point) == pytest.approx(distance, rel=1e-6)
        assert point[2] == 0.0
    assert result["ranges"][180] == pytest.approx(5.9 - LIDAR_POSITION[0], abs=1e-6)


def test_elevations_hit_actual_floor_walls_and_leave_open_sky_empty(world):
    _, sampler, _ = world
    result = Lidar().sample(sampler)
    np.testing.assert_allclose(np.linalg.norm(DIRECTIONS, axis=1), 1.0, atol=1e-15)
    origin = sampler.data.site_xpos[sampler.lidar_id]
    # Every -30deg ray reaches the floor before room walls or the obstacle.
    for azimuth in (0, 90, 180, 270):
        point = point_for(result, azimuth)
        assert np.linalg.norm(point) == pytest.approx(origin[2] / 0.5, rel=1e-6)
        assert (origin + point)[2] == pytest.approx(0.0, abs=1e-7)
    # +15deg forward intersects the east wall; +30deg passes above its top.
    upper_wall = point_for(result, 3 * SCAN_COUNT + 180)
    assert (origin + upper_wall)[0] == pytest.approx(5.9, abs=1e-6)
    assert 0.0 < (origin + upper_wall)[2] < 2.0
    assert 4 * SCAN_COUNT + 180 not in result["ray_indices"]
    assert np.all(np.linalg.norm(result["xyz"], axis=1) >= SCAN_MIN - 1e-6)
    assert np.all(np.linalg.norm(result["xyz"], axis=1) <= SCAN_MAX + 1e-6)


def test_translation_changes_wall_and_obstacle_ranges_causally(world):
    sim, sampler, runtime = world
    lidar = Lidar()
    before = lidar.sample(sampler)
    # At 45deg the nearest actual hit is the off-axis box's west face x=2.1.
    obstacle_index = 225
    expected = (2.1 - LIDAR_POSITION[0]) / math.cos(math.pi / 4)
    assert before["ranges"][obstacle_index] == pytest.approx(expected, rel=1e-6)
    sim.data.qpos[0] += 0.5  # Physical fixture perturbation, not a movement backend.
    assert sampler.capture(runtime)
    after = lidar.sample(sampler)
    assert after["ranges"][180] == pytest.approx(before["ranges"][180] - 0.5, abs=1e-6)
    assert after["ranges"][obstacle_index] == pytest.approx(expected - 0.5 / math.cos(math.pi / 4), rel=1e-6)
    np.testing.assert_array_equal(after["ranges"], sampler.scan())
    # Previously returned arrays must remain owned by that sample.
    assert before["ranges"][180] == pytest.approx(5.62, abs=1e-6)


def test_lidar_site_extrinsics_rotate_points_and_rays_together(world):
    sim, sampler, runtime = world
    sim.data.qpos[3:7] = [math.sqrt(0.5), 0.0, 0.0, math.sqrt(0.5)]
    assert sampler.capture(runtime)
    result = Lidar().sample(sampler)
    origin = sampler.data.site_xpos[sampler.lidar_id]
    rotation = sampler.data.site_xmat[sampler.lidar_id].reshape(3, 3)
    np.testing.assert_allclose(origin, [0.0, 0.28, 0.42], atol=1e-12)
    forward = point_for(result, HORIZONTAL_RING * SCAN_COUNT + 180)
    np.testing.assert_allclose(origin + rotation @ forward, [0.0, 5.9, 0.42], atol=1e-6)
    np.testing.assert_array_equal(result["ranges"], sampler.scan())
    assert result["frame_id"] == "lidar_link"
    assert result["world_frame_id"] == "simulation_world"


def test_lidar_uses_only_the_copied_capture_and_pause_does_not_forge_a_stamp(world):
    sim, sampler, runtime = world
    lidar = Lidar()
    first = lidar.sample(sampler)
    sim.data.qpos[0] = 1.0
    sim.data.time += 1.0
    sim.epoch += 1
    # Live physics changes cannot alter an already captured exposure.
    second = lidar.sample(sampler)
    for key in ("time", "epoch", "wall_timestamp_ns"):
        assert second[key] == first[key]
    np.testing.assert_array_equal(second["xyz"], first["xyz"])
    sim.pause()
    assert not sampler.capture(runtime)
    assert lidar.sample(sampler)["wall_timestamp_ns"] == first["wall_timestamp_ns"]
    sim.resume()
    assert sampler.capture(runtime)
    fresh = lidar.sample(sampler)
    assert fresh["time"] == sim.data.time and fresh["epoch"] == sim.epoch
    assert fresh["wall_timestamp_ns"] > first["wall_timestamp_ns"]


def test_open_and_out_of_range_rays_have_no_cloud_points_or_invented_clear_returns():
    sim = Simulation(model=instrumented_model(DEFAULT_ASSETS, sandbox=False))
    sampler = PhysicsSampler(sim)
    runtime = SimpleNamespace(lock=threading.RLock())
    sampler.capture(runtime)
    result = Lidar().sample(sampler)
    assert np.isinf(result["ranges"]).all()
    assert len(result["xyz"]) == 2 * SCAN_COUNT  # Only the two downward rings hit ground.
    assert np.all(result["ray_indices"] < HORIZONTAL_RING * SCAN_COUNT)
    sim.data.qpos[2] = 10.0  # The floor now lies beyond 12m for even the lowest ring.
    sampler.capture(runtime)
    far = Lidar().sample(sampler)
    assert np.isinf(far["ranges"]).all()
    assert far["xyz"].shape == (0, 3)


def test_returns_inside_the_minimum_range_are_unknown(world):
    sim, sampler, runtime = world
    sim.data.qpos[0] = 5.60  # Lidar origin x=5.88, only 2cm from the east wall.
    sampler.capture(runtime)
    result = Lidar().sample(sampler)
    assert math.isinf(result["ranges"][180])
    assert HORIZONTAL_RING * SCAN_COUNT + 180 not in result["ray_indices"]


def test_dropout_applies_one_shared_mask_and_repeats_from_the_seed(world):
    _, sampler, _ = world
    complete = Lidar().sample(sampler)
    empty = Lidar(dropout=1.0).sample(sampler)
    assert np.isinf(empty["ranges"]).all()
    assert empty["xyz"].shape == (0, 3)
    assert empty["ray_indices"].shape == (0,)
    first, second = Lidar(dropout=0.5, seed=123), Lidar(dropout=0.5, seed=123)
    prior = None
    for _ in range(2):
        a, b = first.sample(sampler), second.sample(sampler)
        np.testing.assert_array_equal(a["ranges"], b["ranges"])
        np.testing.assert_array_equal(a["ray_indices"], b["ray_indices"])
        np.testing.assert_array_equal(a["xyz"], b["xyz"])
        assert 0 < len(a["xyz"]) < len(complete["xyz"])
        cloud_horizontal = a["ray_indices"][(a["ray_indices"] >= 720) & (a["ray_indices"] < 1080)] - 720
        np.testing.assert_array_equal(cloud_horizontal, np.flatnonzero(np.isfinite(a["ranges"])))
        if prior is not None:
            assert not np.array_equal(prior, a["ray_indices"])
        prior = a["ray_indices"]


@pytest.mark.parametrize("dropout", [-0.1, 1.1, math.nan, math.inf, True, "0.5", None])
def test_invalid_dropout_is_rejected(dropout):
    with pytest.raises(ValueError, match="dropout"):
        Lidar(dropout=dropout)


def test_capture_is_required_and_ray_cost_is_measured(world):
    sim, sampler, _ = world
    with pytest.raises(ValueError, match="capture"):
        Lidar().sample(PhysicsSampler(sim))
    lidar = Lidar()
    costs = []
    for _ in range(20):
        started = time.perf_counter()
        result = lidar.sample(sampler)
        costs.append((time.perf_counter() - started) * 1000)
    print(f"MuJoCo {RAY_COUNT}-ray batch: mean={np.mean(costs):.3f}ms "
          f"p95={np.percentile(costs, 95):.3f}ms, points={len(result['xyz'])}")


def test_private_batch_matches_every_scalar_ray_with_rotations_and_moving_obstacle(world):
    sim, sampler, runtime = world
    lidar = Lidar()
    original_bvh = sim.model.body_bvhadr.copy()
    rng = np.random.default_rng(43)
    obstacle_body = sim.model.body("sandbox_obstacle").id
    mocap = sim.model.body_mocapid[obstacle_body]
    geom_id = np.empty(1, dtype=np.int32)
    for _ in range(12):
        sim.data.qpos[:3] = [*rng.uniform(-5.7, 5.7, 2), rng.uniform(0.15, 1.5)]
        rotation = rng.normal(size=4)
        sim.data.qpos[3:7] = rotation / np.linalg.norm(rotation)
        sim.data.mocap_pos[mocap] = [*rng.uniform(-4.0, 4.0, 2), 0.4]
        sampler.capture(runtime)
        lidar.sample(sampler)
        origin = sampler.data.site_xpos[sampler.lidar_id]
        matrix = sampler.data.site_xmat[sampler.lidar_id].reshape(3, 3)
        expected = np.array([
            mujoco.mj_ray(sim.model, sampler.data, origin, direction,
                          sampler.geom_group, True, -1, geom_id)
            for direction in DIRECTIONS @ matrix.T])
        np.testing.assert_array_equal(lidar.distances, expected)
        np.testing.assert_array_equal(sim.model.body_bvhadr, original_bvh)
    assert lidar.ray_model is not sim.model
    assert np.all(lidar.ray_model.body_bvhadr == -1)
