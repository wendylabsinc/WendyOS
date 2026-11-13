"""Offline acceptance against the real R2 model; does not touch a running VM.

Run from any directory: python3 Examples/R2Explorer/tests/simulator_acceptance.py
"""
from pathlib import Path
import sys

root = Path(__file__).resolve().parents[3]
sys.path.insert(0, str(root/'go/simulator/rosmaster_r2'))
sys.path.insert(0, str(root/'Examples/R2Explorer'))
from r2_sim.simulation import Simulation
from planner import Planner


def run(x, y, yaw, seconds):
    sim, planner = Simulation(), Planner()
    sim.x, sim.y, sim.yaw = x, y, yaw
    reversed_once = False
    for _ in range(seconds*10):
        speed, steer, _ = planner.command(sim.state(), sim.scan(), dt=.1)
        reversed_once |= speed < 0
        sim.drive(speed, steer)
        sim.step(.1)
        assert not sim.collision, f'Collision at {sim.x:.2f}, {sim.y:.2f}'
    print(f'{seconds}s: {sim.distance:.1f} m, {len(planner.visits)} visited cells, no collisions')
    return sim, planner, reversed_once


sim, planner, _ = run(0, 0, 0, 120)
assert sim.distance > 20
assert len(planner.visits) > 60
sim, planner, reversed_once = run(4.5, 0, 0, 30)
assert reversed_once, 'The car must back away from the nearby wall'
assert sim.distance > 3
print('Open-room exploration and near-wall reverse recovery passed')
