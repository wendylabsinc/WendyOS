"""Pinned policy contract and actual unsupported humanoid motion regressions."""

import math

import mujoco
import numpy as np
import pytest

from g1_sim.policy import POLICY_TO_MOTOR, WalkingPolicy
from g1_sim.simulation import DEFAULT_ASSETS, Simulation, TIMESTEP


@pytest.fixture
def simulation():
    clock = [0.0]
    sim = Simulation(monotonic=lambda: clock[0])
    return sim, clock


def advance(sim, clock, seconds, command=None, token=None):
    for index in range(round(seconds / TIMESTEP)):
        clock[0] += TIMESTEP
        if command is not None and index % 20 == 0:
            sim.command_velocity(*command, token=token)
        sim.step()
        assert np.isfinite(sim.data.qpos).all()
        assert np.isfinite(sim.data.qvel).all()
        assert np.all(sim.data.ctrl[sim.actuators] <= sim.torque_ranges[:, 1])
        assert np.all(sim.data.ctrl[sim.actuators] >= sim.torque_ranges[:, 0])
        assert not np.any(sim.data.xfrc_applied)
        assert not np.any(sim.data.qfrc_applied)


def test_policy_term_history_and_motor_mapping():
    policy = WalkingPolicy(DEFAULT_ASSETS)
    np.testing.assert_allclose(policy.default[[0, 6, 3, 9, 18, 25]], [-.1, -.1, .3, .3, .97, .97])
    assert policy.kp[3] == 150 and policy.kp[12] == 200
    q = policy.default + np.arange(29) / 100
    dq = np.arange(29) / 10
    first = policy.observation([1, 2, 3], [0, 0, -1], [.1, .2, -.1], q, dq)
    assert first.shape == (1, 480) and first.dtype == np.float32
    np.testing.assert_allclose(first[0, :15].reshape(5, 3), np.tile([.2, .4, .6], (5, 1)))
    np.testing.assert_allclose(first[0, 45:190].reshape(5, 29), np.tile((q - policy.default)[POLICY_TO_MOTOR], (5, 1)), atol=1e-7)
    np.testing.assert_allclose(first[0, 190:335].reshape(5, 29), np.tile(dq[POLICY_TO_MOTOR] * .05, (5, 1)), atol=1e-7)
    assert np.all(first[0, 335:] == 0)
    policy.action = np.arange(29, dtype=np.float32)
    second = policy.observation([4, 5, 6], [0, 0, -1], [.1, .2, -.1], q, dq)
    np.testing.assert_allclose(second[0, :12], first[0, 3:15])
    np.testing.assert_allclose(second[0, 12:15], [.8, 1., 1.2])
    np.testing.assert_array_equal(second[0, -29:], policy.action)
    assert np.all(second[0, 335:-29] == 0)
    policy.reset()
    target = policy.targets([0, 0, 0], [0, 0, -1], [0, 0, 0], policy.default, np.zeros(29))
    np.testing.assert_allclose(target[POLICY_TO_MOTOR], policy.policy_default + .25 * policy.action)


@pytest.mark.parametrize("command,axis,direction", [
    ((.3, 0., 0.), 0, 1), ((-.3, 0., 0.), 0, -1),
    ((0., .3, 0.), 1, 1), ((0., -.3, 0.), 1, -1),
])
def test_floating_policy_walks_and_stops(simulation, command, axis, direction):
    sim, clock = simulation
    assert (sim.model.nq, sim.model.nv, sim.model.nu, sim.model.neq) == (36, 35, 29, 0)
    token = sim.arm()
    advance(sim, clock, 2)
    start = sim.data.qpos[:3].copy()
    advance(sim, clock, 8, command, token)
    assert sim.mode == "moving"
    assert direction * (sim.data.qpos[axis] - start[axis]) > 0.8
    assert sim.data.qpos[2] > .65 and sim.data.xmat[sim.body_id, 8] > .95
    sim.stop(token)
    advance(sim, clock, 3)
    assert sim.mode == "standing"
    assert np.linalg.norm(sim.data.qvel[:3]) < .08
    assert sim.policy.updates == 650


def test_turn_and_watchdog_stop_use_real_physics(simulation):
    sim, clock = simulation
    token = sim.arm()
    advance(sim, clock, 2)
    advance(sim, clock, 8, (.3, 0., .2), token)
    rotation = sim.data.xmat[sim.body_id].reshape(3, 3)
    assert math.atan2(rotation[1, 0], rotation[0, 0]) > .7
    assert sim.data.qpos[2] > .65
    advance(sim, clock, 3)
    assert np.all(sim.command == 0) and np.all(sim.applied_command == 0)
    assert np.linalg.norm(sim.data.qvel[:3]) < .08


def test_lifecycle_revokes_control_and_zero_torque_is_physical(simulation):
    sim, clock = simulation
    token = sim.arm()
    with pytest.raises(PermissionError):
        sim.arm()
    with pytest.raises(ValueError):
        sim.command_velocity(-.51, 0, 0, token)
    with pytest.raises(ValueError):
        sim.command_velocity(0, 0, .21, token)
    with pytest.raises(ValueError):
        sim.stand_down(token)
    sim.pause()
    pose, stamp = sim.data.qpos.copy(), sim.data.time
    advance(sim, clock, .1)
    np.testing.assert_array_equal(sim.data.qpos, pose)
    assert sim.data.time == stamp
    sim.resume()
    with pytest.raises(PermissionError):
        sim.command_velocity(.1, 0, 0, token)
    token = sim.arm()
    sim.zero_torque(token)
    advance(sim, clock, 1)
    assert sim.mode == "zero_torque" and sim.owner is None
    assert np.all(sim.data.ctrl == 0)
    assert sim.data.qpos[2] < .6  # The unsupported robot physically collapses.
    epoch = sim.epoch
    sim.reset()
    assert sim.epoch == epoch + 1 and sim.mode == "standing"
    assert not sim.policy.history and np.all(sim.policy.action == 0)


def test_low_level_motor_order_limits_and_watchdog(simulation):
    sim, clock = simulation
    token = sim.arm("lowlevel")
    q = sim.policy.default.copy()
    q[3] += .1
    zeros = np.zeros(29)
    active = np.zeros(29, dtype=bool)
    active[3] = True
    sim.command_low_level(q, zeros, np.full(29, 300.), zeros, zeros, active, token)
    sim.step()
    assert sim.data.ctrl[sim.actuators[3]] == pytest.approx(30.)
    assert np.count_nonzero(sim.data.ctrl) == 1
    with pytest.raises(ValueError):
        sim.command_low_level(q, zeros, np.full(29, 301.), zeros, zeros, active, token)
    bad_torque = zeros.copy()
    bad_torque[21] = 6.  # Wrist yaw is physically limited to 5 Nm.
    with pytest.raises(ValueError):
        sim.command_low_level(q, zeros, zeros, zeros, bad_torque, active, token)
    clock[0] += .04
    sim.step()
    assert sim.mode == "damping" and sim.owner is None
    with pytest.raises(PermissionError):
        sim.command_low_level(q, zeros, zeros, zeros, zeros, active, token)


def test_support_constraint_is_rejected():
    model = mujoco.MjModel.from_xml_string(
        (DEFAULT_ASSETS / "robot/scene_29dof.xml").read_text().replace(
            '</mujoco>', '<equality><weld body1="pelvis"/></equality></mujoco>'),
        assets={
            'g1_29dof.xml': (DEFAULT_ASSETS / 'robot/g1_29dof.xml').read_bytes(),
            **{str(p.relative_to(DEFAULT_ASSETS / 'robot')): p.read_bytes()
               for p in (DEFAULT_ASSETS / 'robot/meshes').glob('*.STL')},
        })
    with pytest.raises(ValueError, match="support equality"):
        Simulation(model=model)
