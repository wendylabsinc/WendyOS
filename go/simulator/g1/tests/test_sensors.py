"""Physical sensor conventions and ray/world causality, independent of ROS."""

from types import SimpleNamespace
import threading

import mujoco
import numpy as np
import pytest

from g1_sim.sensors import PhysicsSampler, instrumented_model
from g1_sim.simulation import DEFAULT_ASSETS, Simulation


@pytest.fixture
def sampled():
    sim = Simulation(model=instrumented_model(DEFAULT_ASSETS))
    runtime = SimpleNamespace(lock=threading.RLock())
    return sim, PhysicsSampler(sim), runtime


def test_instrumentation_preserves_robot_dynamics():
    original = mujoco.MjModel.from_xml_path(str(DEFAULT_ASSETS / "robot/scene_29dof.xml"))
    model = instrumented_model(DEFAULT_ASSETS, sandbox=False)
    for field in ("body_mass", "body_inertia", "jnt_range", "jnt_actfrcrange",
                  "dof_damping", "dof_armature", "geom_contype", "geom_conaffinity"):
        np.testing.assert_array_equal(getattr(model, field), getattr(original, field))
    assert model.nsensor == original.nsensor + 1
    assert model.ncam == original.ncam + 1


def test_resting_imu_reports_specific_force_and_local_velocity(sampled):
    sim, sampler, runtime = sampled
    for _ in range(1500):
        sim.step()
    assert sampler.capture(runtime)
    state = sampler.state()
    assert state["specific_force_body"][2] == pytest.approx(9.81, abs=2)
    assert np.linalg.norm(state["angular_velocity_body"]) < 0.5
    assert state["time"] == sim.data.time
    assert state["epoch"] == sim.epoch


def test_scan_ignores_robot_and_tracks_obstacle_geometry(sampled):
    sim, sampler, runtime = sampled
    sampler.capture(runtime)
    before = sampler.scan()
    # At the reset pose a forward horizontal ray hits the east wall at x=5.9.
    assert before[180] == pytest.approx(5.9 - 0.16, abs=0.01)
    sim.data.qpos[0] += 0.5  # Test fixture perturbation, never a movement backend.
    sampler.capture(runtime)
    after = sampler.scan()
    assert after[180] == pytest.approx(before[180] - 0.5, abs=0.01)
    # Off-axis obstacle is nearer than the room, proving nonconstant ranges.
    assert float(np.min(before)) < 3.0
    assert np.count_nonzero(np.isfinite(before)) == 360


def test_pause_does_not_refresh_sensor_samples(sampled):
    sim, sampler, runtime = sampled
    assert sampler.capture(runtime)
    sample_time = sampler.state()["time"]
    sim.pause()
    assert not sampler.capture(runtime)
    assert sampler.state()["time"] == sample_time
