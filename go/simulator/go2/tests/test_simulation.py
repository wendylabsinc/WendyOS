"""Exercise the exported controller through real MuJoCo dynamics.

These are simulation-only tests: the injected monotonic clock advances alongside
physics, so command watchdogs are covered without sleeping or opening hardware.
"""

import math

import mujoco
import numpy as np
import pytest

from go2_sim.simulation import Simulation


DT = 0.002


class Clock:
    def __init__(self):
        self.now = 100.0

    def __call__(self):
        return self.now

    def advance(self, seconds):
        self.now += seconds


@pytest.fixture
def robot():
    clock = Clock()
    simulation = Simulation(monotonic=clock)
    yield simulation, clock
    close = getattr(simulation, "close", None)
    if close is not None:
        close()


def advance(robot, seconds, *, command=None, token=None):
    simulation, clock = robot
    for tick in range(round(seconds / DT)):
        if command is not None and tick % 25 == 0:
            simulation.command_velocity(*command, token)
        clock.advance(DT)
        simulation.step()
    return simulation.snapshot()


def yaw(snapshot):
    w, x, y, z = snapshot["quaternion_wxyz"]
    return math.atan2(2 * (w * z + x * y), 1 - 2 * (y * y + z * z))


def angle_difference(after, before):
    return math.atan2(math.sin(after - before), math.cos(after - before))


def test_startup_has_no_velocity_request_and_remains_supported(robot):
    simulation, _ = robot
    before = simulation.snapshot()
    after = advance(robot, 2.0)

    assert after["command"] == pytest.approx([0.0, 0.0, 0.0])
    assert after["time"] - before["time"] == pytest.approx(2.0)
    assert np.linalg.norm(np.subtract(after["position"][:2], before["position"][:2])) < 0.15
    assert abs(angle_difference(yaw(after), yaw(before))) < 0.2
    assert 0.15 < after["position"][2] < 0.6
    assert np.isfinite(simulation.data.qpos).all()
    assert np.isfinite(simulation.data.qvel).all()
    assert simulation.data.ncon > 0, "the standing robot must contact the floor"


@pytest.mark.parametrize(
    ("command", "axis", "direction", "minimum"),
    [
        ((0.35, 0.0, 0.0), 0, 1, 0.25),
        ((-0.35, 0.0, 0.0), 0, -1, 0.25),
        ((0.0, 0.25, 0.0), 1, 1, 0.15),
        ((0.0, -0.25, 0.0), 1, -1, 0.15),
        ((0.0, 0.0, 0.5), 2, 1, 0.35),
        ((0.0, 0.0, -0.5), 2, -1, 0.35),
    ],
)
def test_velocity_request_produces_signed_physical_motion(robot, command, axis, direction, minimum):
    simulation, _ = robot
    advance(robot, 2.0)
    token = simulation.arm()
    before = simulation.snapshot()
    after = advance(robot, 3.0, command=command, token=token)

    if axis == 2:
        displacement = angle_difference(yaw(after), yaw(before))
    else:
        # Velocity commands are body-relative: project world displacement into
        # the robot's starting heading instead of assuming an exact zero yaw.
        dx, dy = np.subtract(after["position"][:2], before["position"][:2])
        heading = yaw(before)
        displacement = (
            dx * math.cos(heading) + dy * math.sin(heading)
            if axis == 0
            else -dx * math.sin(heading) + dy * math.cos(heading)
        )
    assert direction * displacement > minimum, (command, displacement, after)
    assert 0.15 < after["position"][2] < 0.6
    assert np.isfinite(simulation.data.qpos).all()
    assert np.isfinite(simulation.data.ctrl).all()


def test_commands_actuate_joints_without_teleporting_the_free_root(robot):
    simulation, clock = robot
    advance(robot, 2.0)
    token = simulation.arm()
    before = simulation.data.qpos.copy()
    simulation.command_velocity(0.35, 0.0, 0.0, token)
    np.testing.assert_array_equal(simulation.data.qpos, before)

    active_torque = False
    joint_travel = 0.0
    contact_steps = 0
    for tick in range(1000):
        if tick % 25 == 0:
            simulation.command_velocity(0.35, 0.0, 0.0, token)
        position = simulation.data.qpos[:3].copy()
        clock.advance(DT)
        simulation.step()
        # MuJoCo integrates free-joint translation from its generalized
        # velocity. Directly animating qpos breaks this physical relationship.
        measured_velocity = (simulation.data.qpos[:3] - position) / DT
        np.testing.assert_allclose(measured_velocity, simulation.data.qvel[:3], atol=0.03, rtol=0.05)
        active_torque |= bool(np.any(np.abs(simulation.data.ctrl) > 0.1))
        joint_travel = max(joint_travel, float(np.max(np.abs(simulation.data.qpos[7:] - before[7:]))))
        contact_steps += simulation.data.ncon > 0

    assert active_torque, "the controller must drive MuJoCo actuators"
    assert joint_travel > 0.05, "walking must change joint positions"
    assert contact_steps > 500, "the robot must be supported by contacts during the gait"


def test_expired_command_clears_target_and_robot_settles(robot):
    simulation, _ = robot
    advance(robot, 2.0)
    token = simulation.arm()
    advance(robot, 1.5, command=(0.35, 0.0, 0.0), token=token)
    expired = advance(robot, 0.22)
    assert expired["command"] == pytest.approx([0.0, 0.0, 0.0])
    settled = advance(robot, 2.0)
    assert np.linalg.norm(settled["linear_velocity_world"][:2]) < 0.15
    assert abs(settled["angular_velocity_body"][2]) < 0.25
    assert 0.15 < settled["position"][2] < 0.6


def test_stop_clears_target_immediately(robot):
    simulation, _ = robot
    token = simulation.arm()
    simulation.command_velocity(0.35, 0.0, 0.0, token)
    simulation.stop(token)
    assert simulation.snapshot()["command"] == pytest.approx([0.0, 0.0, 0.0])


def test_reset_revokes_owner_and_requires_a_new_token(robot):
    simulation, _ = robot
    token = simulation.arm()
    before = advance(robot, 1.0, command=(0.35, 0.0, 0.0), token=token)
    simulation.reset()
    reset = simulation.snapshot()
    assert reset["epoch"] != before["epoch"]
    assert reset["time"] == pytest.approx(0.0)
    assert reset["command"] == pytest.approx([0.0, 0.0, 0.0])
    with pytest.raises((PermissionError, ValueError)):
        simulation.command_velocity(0.35, 0.0, 0.0, token)
    new_token = simulation.arm()
    assert new_token != token
    simulation.command_velocity(0.35, 0.0, 0.0, new_token)
    assert simulation.snapshot()["command"] == pytest.approx([0.35, 0.0, 0.0])


def test_pause_freezes_physics_and_resume_does_not_restore_ownership(robot):
    simulation, clock = robot
    token = simulation.arm()
    advance(robot, 1.0, command=(0.35, 0.0, 0.0), token=token)
    simulation.pause()
    paused = simulation.snapshot()
    position = simulation.data.qpos.copy()
    clock.advance(1.0)
    simulation.step()
    np.testing.assert_array_equal(simulation.data.qpos, position)
    assert simulation.snapshot()["time"] == paused["time"]
    assert simulation.snapshot()["command"] == pytest.approx([0.0, 0.0, 0.0])
    simulation.resume()
    with pytest.raises((PermissionError, ValueError)):
        simulation.command_velocity(0.35, 0.0, 0.0, token)
    assert simulation.arm() != token


@pytest.mark.parametrize(
    "command",
    [
        (math.nan, 0.0, 0.0),
        (0.0, math.inf, 0.0),
        (0.0, 0.0, -math.inf),
        (0.81, 0.0, 0.0),
        (0.0, -0.51, 0.0),
        (0.0, 0.0, 1.01),
    ],
)
def test_invalid_velocity_cannot_modify_the_target(robot, command):
    simulation, _ = robot
    token = simulation.arm()
    with pytest.raises(ValueError):
        simulation.command_velocity(*command, token)
    assert simulation.snapshot()["command"] == pytest.approx([0.0, 0.0, 0.0])


def test_unknown_owner_cannot_command_or_stop(robot):
    simulation, _ = robot
    token = simulation.arm()
    simulation.command_velocity(0.35, 0.0, 0.0, token)
    with pytest.raises((PermissionError, ValueError)):
        simulation.command_velocity(0.0, 0.0, 0.0, "not-the-owner")
    with pytest.raises((PermissionError, ValueError)):
        simulation.stop("not-the-owner")
    assert simulation.snapshot()["command"] == pytest.approx([0.35, 0.0, 0.0])


def test_a_physical_fall_revokes_control_and_reset_restores_a_standing_robot(robot):
    simulation, _ = robot
    advance(robot, 2.0)
    token = simulation.arm()
    simulation.command_velocity(0.35, 0.0, 0.0, token)
    # Inject a reproducible disturbance, then let the real physics/controller
    # detect it. This is test setup, not an alternative locomotion mechanism.
    simulation.data.qpos[3:7] = [math.sqrt(0.5), math.sqrt(0.5), 0.0, 0.0]
    mujoco.mj_forward(simulation.model, simulation.data)
    fallen = advance(robot, 0.1)

    assert fallen["mode"] == "fallen"
    assert not fallen["armed"]
    assert fallen["command"] == pytest.approx([0.0, 0.0, 0.0])
    assert fallen["applied_command"] == pytest.approx([0.0, 0.0, 0.0])
    assert np.isfinite(simulation.data.qpos).all()
    with pytest.raises(PermissionError):
        simulation.command_velocity(0.35, 0.0, 0.0, token)
    with pytest.raises(ValueError):
        simulation.arm()

    simulation.reset()
    restored = advance(robot, 2.0)
    assert restored["epoch"] != fallen["epoch"]
    assert restored["mode"] == "standing"
    assert not restored["armed"]
    assert restored["command"] == pytest.approx([0.0, 0.0, 0.0])
    assert 0.15 < restored["position"][2] < 0.6
    assert simulation.data.xmat[simulation.body_id].reshape(3, 3)[2, 2] > 0.9
    assert simulation.arm() != token


def test_a_continuing_old_publisher_cannot_reactivate_motion_after_reset(robot):
    simulation, _ = robot
    token = simulation.arm()
    advance(robot, 2.0, command=(0.35, 0.0, 0.0), token=token)
    simulation.reset()

    for _ in range(40):
        with pytest.raises(PermissionError):
            simulation.command_velocity(0.35, 0.0, 0.0, token)
        state = advance(robot, 0.05)
        assert state["command"] == pytest.approx([0.0, 0.0, 0.0])
        assert not state["armed"]

    replacement = simulation.arm()
    simulation.command_velocity(-0.35, 0.0, 0.0, replacement)
    with pytest.raises(PermissionError):
        simulation.command_velocity(0.35, 0.0, 0.0, token)
    assert simulation.snapshot()["command"] == pytest.approx([-0.35, 0.0, 0.0])
