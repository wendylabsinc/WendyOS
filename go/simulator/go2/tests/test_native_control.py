"""Native-control mechanics tested only against the real local MuJoCo world."""

import math

import mujoco
import numpy as np
import pytest

from go2_sim.simulation import LOW_LEVEL_TIMEOUT, Simulation, TIMESTEP


class Clock:
    now = 100.0

    def __call__(self):
        return self.now


@pytest.fixture
def robot():
    clock = Clock()
    return Simulation(monotonic=clock), clock


def advance(robot, seconds, callback=None, *, verify_integration=False):
    sim, clock = robot
    minimum_vz, maximum_vz, maximum_torque = 0.0, 0.0, 0.0
    for tick in range(round(seconds / TIMESTEP)):
        if callback is not None and tick % 10 == 0:
            callback(tick * TIMESTEP)
        previous_position = sim.data.qpos[:3].copy()
        clock.now += TIMESTEP
        sim.step()
        if verify_integration:
            np.testing.assert_allclose((sim.data.qpos[:3] - previous_position) / TIMESTEP,
                                       sim.data.qvel[:3], atol=0.03, rtol=0.05)
        minimum_vz = min(minimum_vz, sim.data.qvel[2])
        maximum_vz = max(maximum_vz, sim.data.qvel[2])
        maximum_torque = max(maximum_torque, float(np.max(np.abs(sim.data.ctrl[sim.actuators]))))
        assert np.isfinite(sim.data.qpos).all()
        assert np.all(sim.data.ctrl[sim.actuators] >= sim.torque_ranges[:, 0])
        assert np.all(sim.data.ctrl[sim.actuators] <= sim.torque_ranges[:, 1])
    return minimum_vz, maximum_vz, maximum_torque


def low_command(sim, **changes):
    command = {"q": sim.policy.default.copy(), "dq": np.zeros(12),
               "kp": np.full(12, 50.0), "kd": np.full(12, 3.0),
               "tau": np.zeros(12), "active": np.ones(12, dtype=bool)}
    command.update(changes)
    return command


def test_postures_lower_and_raise_through_torques_without_reset_or_root_writes(robot):
    sim, _ = robot
    advance(robot, 2.0)
    token = sim.arm()
    epoch, standing_height = sim.epoch, sim.data.qpos[2]
    before = sim.data.qpos.copy()
    sim.stand_down(token)
    np.testing.assert_array_equal(sim.data.qpos, before)
    assert sim.mode == "standing_down"
    minimum_vz, _, torque = advance(robot, 3.2, verify_integration=True)
    assert sim.mode == "lying"
    assert minimum_vz < -0.03 and torque > 1.0
    assert 0.05 < sim.data.qpos[2] < 0.12
    assert standing_height - sim.data.qpos[2] > 0.2
    assert sim.data.ncon > 0
    resting_pose = sim.data.qpos[:3].copy()
    updates = sim.policy.updates
    advance(robot, 1.0)
    assert sim.policy.updates == updates
    np.testing.assert_allclose(sim.data.qpos[:3], resting_pose, atol=0.01)
    assert np.linalg.norm(sim.data.qvel[:3]) < 0.05
    assert sim.data.xmat[sim.body_id].reshape(3, 3)[2, 2] > 0.95

    before = sim.data.qpos.copy()
    sim.stand_up(token)
    np.testing.assert_array_equal(sim.data.qpos, before)
    assert sim.mode == "standing_up"
    _, maximum_vz, torque = advance(robot, 3.5, verify_integration=True)
    assert sim.mode == "standing"
    assert maximum_vz > 0.03 and torque > 1.0
    assert sim.data.qpos[2] > 0.25 and sim.data.ncon > 0
    assert sim.policy.updates > updates
    assert sim.owner == token and sim.epoch == epoch
    assert sim.data.time > 9.0
    before_x = sim.data.qpos[0]
    advance(robot, 1.5, lambda _: sim.command_velocity(0.3, 0.0, 0.0, token))
    assert sim.data.qpos[0] - before_x > 0.2, "walking must work after standing up"


def test_stand_down_stops_a_walking_robot_before_folding_legs(robot):
    sim, _ = robot
    advance(robot, 2.0)
    token = sim.arm()
    advance(robot, 1.0, lambda _: sim.command_velocity(0.4, 0.0, 0.0, token))
    assert sim.applied_command[0] > 0.3
    sim.stand_down(token)
    assert not np.any(sim.command)
    advance(robot, 0.1)
    assert sim.snapshot()["posture_phase"] == "stopping"
    assert sim.data.qpos[2] > 0.2
    # Repeated Sport calls must not restart the transition or its stop phase.
    advance(robot, 4.0, lambda _: sim.stand_down(token))
    assert sim.mode == "lying" and sim.data.qpos[2] < 0.12


def test_posture_commands_validate_owner_and_do_not_accept_velocity_during_transition(robot):
    sim, _ = robot
    token = sim.arm()
    with pytest.raises(PermissionError):
        sim.stand_down("unknown")
    sim.stand_down(token)
    with pytest.raises(ValueError):
        sim.command_velocity(0.1, 0.0, 0.0, token)
    with pytest.raises(ValueError):
        sim.stand_up(token)
    advance(robot, 3.5)
    assert sim.mode == "lying"
    with pytest.raises(ValueError):
        sim.command_velocity(0.1, 0.0, 0.0, token)
    sim.stand_up(token)
    with pytest.raises(ValueError):
        sim.stand_down(token)


def test_low_level_pd_physically_stands_from_lying_without_policy_updates(robot):
    sim, _ = robot
    advance(robot, 2.0)
    sport_token = sim.arm()
    sim.stand_down(sport_token)
    advance(robot, 3.2)
    sim.release(sport_token)
    starting_joints = sim.data.qpos[sim.qaddr].copy()
    starting_height, updates = sim.data.qpos[2], sim.policy.updates
    token = sim.arm(mode="lowlevel")
    command = low_command(sim)
    before = sim.data.qpos.copy()
    command["q"] = starting_joints
    sim.command_low_level(**command, token=token)
    np.testing.assert_array_equal(sim.data.qpos, before)

    def raise_robot(elapsed):
        alpha = min(1.0, elapsed / 2.0)
        alpha = alpha * alpha * (3.0 - 2.0 * alpha)
        command["q"] = starting_joints + alpha * (sim.policy.default - starting_joints)
        sim.command_low_level(**command, token=token)

    _, maximum_vz, torque = advance(robot, 3.0, raise_robot, verify_integration=True)
    assert sim.mode == "lowlevel" and sim.control_mode == "lowlevel"
    assert sim.data.qpos[2] - starting_height > 0.18
    assert sim.data.ncon > 0 and maximum_vz > 0.03 and torque > 1.0
    assert sim.policy.updates == updates
    # A finite-gain PD stand carries gravity through spring deflection; it
    # does not hold the unloaded joint target exactly.
    np.testing.assert_allclose(sim.data.qpos[sim.qaddr], sim.policy.default, atol=0.12)
    assert np.max(np.abs(sim.data.qvel[sim.vaddr])) < 0.12


def test_sport_and_low_level_control_are_exclusive(robot):
    sim, _ = robot
    token = sim.arm()
    with pytest.raises(ValueError):
        sim.command_low_level(**low_command(sim), token=token)
    with pytest.raises(PermissionError):
        sim.arm(mode="lowlevel")
    sim.release(token)
    token = sim.arm(mode="lowlevel")
    for action in [lambda: sim.command_velocity(0.0, 0.0, 0.0, token),
                   lambda: sim.stop(token), lambda: sim.stand_up(token), lambda: sim.stand_down(token)]:
        with pytest.raises(ValueError):
            action()
    with pytest.raises(PermissionError):
        sim.arm()
    sim.release(token)
    assert sim.mode == "damping" and sim.owner is None
    with pytest.raises(ValueError):
        sim.arm()


def test_low_level_watchdog_revokes_ownership_and_never_resumes_policy(robot):
    sim, clock = robot
    advance(robot, 1.0)
    token = sim.arm(mode="lowlevel")
    sim.command_low_level(**low_command(sim), token=token)
    updates = sim.policy.updates
    advance(robot, 0.02)
    assert sim.mode == "lowlevel" and sim.owner == token
    clock.now = sim._low_level_received + LOW_LEVEL_TIMEOUT + 0.0001
    sim.step()
    assert sim.mode == "damping" and sim.owner is None
    with pytest.raises(PermissionError):
        sim.command_low_level(**low_command(sim), token=token)
    advance(robot, 1.0)
    assert sim.mode == "damping" and sim.policy.updates == updates
    assert sim.control_mode is None


def test_a_late_packet_cannot_renew_an_expired_lease_before_the_next_physics_step(robot):
    sim, clock = robot
    token = sim.arm(mode="lowlevel")
    command = low_command(sim)
    sim.command_low_level(**command, token=token)
    clock.now += LOW_LEVEL_TIMEOUT + 0.001
    # A stalled physics worker must not let the next packet revive the old
    # grant merely because the step-time watchdog has not run yet.
    with pytest.raises(PermissionError):
        sim.command_low_level(**command, token=token)
    assert sim.mode == "damping" and sim.owner is None
    assert sim.policy.updates == 0


@pytest.mark.parametrize("event", ["missing", "backward_clock", "pause", "damp"])
def test_low_level_authority_cannot_survive_missing_packets_or_lifecycle_events(robot, event):
    sim, clock = robot
    token = sim.arm(mode="lowlevel")
    updates = sim.policy.updates
    if event == "missing":
        advance(robot, 0.05)
    elif event == "backward_clock":
        clock.now -= 1.0
        sim.step()
    elif event == "pause":
        sim.command_low_level(**low_command(sim), token=token)
        sim.pause()
        frozen = sim.data.qpos.copy()
        advance(robot, 0.1)
        np.testing.assert_array_equal(sim.data.qpos, frozen)
        sim.resume()
        sim.step()
    else:
        sim.damp(token)
        sim.step()
    assert sim.mode == "damping" and sim.owner is None
    assert sim.policy.updates == updates


@pytest.mark.parametrize(("field", "value"), [
    ("q", [0.0] * 20), ("q", [True] * 12), ("q", [math.nan] * 12),
    ("q", [100.0] * 12), ("q", [10 ** 400] * 12),
    ("dq", [math.inf] * 12), ("dq", [40.01] * 12),
    ("kp", [-0.1] * 12), ("kp", [100.01] * 12),
    ("kd", [-0.1] * 12), ("kd", [10.01] * 12),
    ("tau", [35.56] * 12), ("active", [1] * 12), ("active", [True] * 20),
])
def test_invalid_low_level_commands_do_not_modify_or_extend_the_lease(robot, field, value):
    sim, clock = robot
    token = sim.arm(mode="lowlevel")
    command = low_command(sim)
    sim.command_low_level(**command, token=token)
    receipt = sim._low_level_received
    accepted = {key: item.copy() for key, item in sim._low_level.items()}
    clock.now += 0.01
    command[field] = value
    with pytest.raises(ValueError):
        sim.command_low_level(**command, token=token)
    assert sim._low_level_received == receipt
    for key, item in accepted.items():
        np.testing.assert_array_equal(sim._low_level[key], item)


def test_low_level_command_copies_inputs_and_passive_slots_apply_no_motor_torque(robot):
    sim, _ = robot
    token = sim.arm(mode="lowlevel")
    command = low_command(sim, active=np.zeros(12, dtype=bool))
    sim.command_low_level(**command, token=token)
    command["q"][:] = 100
    command["active"][:] = True
    advance(robot, 0.02)
    np.testing.assert_array_equal(sim.data.ctrl[sim.actuators], np.zeros(12))
    assert sim.policy.updates == 0


def test_reset_revokes_old_low_level_packets_and_restores_default_sport_mode(robot):
    sim, _ = robot
    token = sim.arm(mode="lowlevel")
    command = low_command(sim)
    sim.command_low_level(**command, token=token)
    sim.reset()
    assert sim.control_mode == "sport" and sim.mode == "standing" and sim.owner is None
    with pytest.raises(PermissionError):
        sim.command_low_level(**command, token=token)
    replacement = sim.arm()
    with pytest.raises(ValueError):
        sim.command_low_level(**command, token=replacement)


def test_low_level_total_torque_is_capped_and_a_real_fall_revokes_control(robot):
    sim, _ = robot
    advance(robot, 1.0)
    token = sim.arm(mode="lowlevel")
    command = low_command(sim, q=sim.joint_ranges[:, 1].copy(), kp=np.full(12, 100.0),
                          tau=sim.torque_ranges[:, 1].copy())
    sim.command_low_level(**command, token=token)
    advance(robot, TIMESTEP)
    assert np.any(np.isclose(sim.data.ctrl[sim.actuators], sim.torque_ranges[:, 1]))
    sim.data.qpos[3:7] = [math.sqrt(0.5), math.sqrt(0.5), 0.0, 0.0]
    mujoco.mj_forward(sim.model, sim.data)
    advance(robot, TIMESTEP)
    assert sim.mode == "fallen" and sim.owner is None
    with pytest.raises(PermissionError):
        sim.command_low_level(**command, token=token)
