from pathlib import Path

import mujoco
import numpy as np
import pytest

from drone_formation.choreography import PADS, FlightPlan
from drone_formation.model import CF2_XML, N_DRONES, PHYSICS_HZ, ROTOR_MAX_THRUST, build_model
from drone_formation.simulation import Simulation
from drone_formation.wind import PHASES, WindField, drag_forces


ROOT = Path(__file__).resolve().parents[1]


def test_swarm_uses_the_vendored_crazyflie_with_four_rotors_each():
    model = build_model(PADS)
    assert CF2_XML == ROOT / "models/bitcraze_crazyflie_2/cf2.xml"
    assert model.nu == 4 * N_DRONES
    assert model.na == 4 * N_DRONES  # every rotor has first-order motor lag
    assert np.allclose(model.actuator_ctrlrange[:, 1], ROTOR_MAX_THRUST)
    for index in range(N_DRONES):
        body = mujoco.mj_name2id(model, mujoco.mjtObj.mjOBJ_BODY, f"cf{index}")
        assert body >= 0
        assert model.body_mass[body] == pytest.approx(0.027)
    # The upstream body-torque actuators are gone.
    assert mujoco.mj_name2id(model, mujoco.mjtObj.mjOBJ_ACTUATOR, "x_moment") == -1


def test_formation_references_keep_drones_apart():
    plan = FlightPlan()
    for t in np.arange(0.0, plan.duration, 0.05):
        slots = plan.position(t)
        distance = np.linalg.norm(slots[:, None] - slots[None], axis=2) + np.eye(N_DRONES)
        assert distance.min() > 0.38, t


def test_wind_changes_pattern_and_speed_over_the_show():
    wind = WindField(duration=FlightPlan().duration)
    names = {phase.name for phase in PHASES}
    assert {"Calm", "Steady breeze", "Wind shift", "Gust fronts", "Vortex"} <= names
    point = np.array([[0.0, 0.0, 1.5]])
    assert np.linalg.norm(wind.velocity(point, 1.0)) < 0.1
    assert np.linalg.norm(wind.velocity(point, 14.0)) > 1.5
    # The mean wind turns during the wind shift.
    early = wind.velocity(point, 16.0)[0]
    late = wind.velocity(point, 27.0)[0]
    assert np.degrees(np.arccos(early[:2] @ late[:2] / np.linalg.norm(early[:2]) / np.linalg.norm(late[:2]))) > 45
    # Still air at the floor.
    assert np.linalg.norm(wind.velocity(np.array([[0.0, 0.0, 0.02]]), 14.0)) < 0.1


def test_drag_opposes_air_relative_motion():
    rotation = np.eye(3)[None]
    force = drag_forces(np.zeros((1, 3)), np.array([[3.0, 0.0, 0.0]]), rotation, np.array([0.265]))
    assert force[0, 0] > 0.02
    assert abs(force[0, 1]) < 1e-9


def test_full_show_flies_through_the_wind_without_collisions():
    simulation = Simulation()
    worst = 0.0
    closest = np.inf
    for step in range(int(simulation.plan.duration * PHYSICS_HZ) - 10):
        simulation.step()
        if step % 25 == 0 and simulation.plan.state(simulation.data.time).airborne:
            worst = max(worst, float(simulation.error_now.max()))
            closest = min(closest, simulation.min_separation)
    assert simulation.error is None
    assert simulation.drone_contacts == 0
    assert worst < 0.5
    assert closest > 0.3
    position, *_ = simulation.sensors()
    # Back on the pads at the end of the loop.
    assert np.abs(position[:, 2] - PADS[:, 2]).max() < 0.01
