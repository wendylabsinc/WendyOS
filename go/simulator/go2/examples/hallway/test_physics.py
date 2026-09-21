"""Full locomotion checks. Set GO2_SIM_ROOT when running outside this checkout."""
import os
import json
from pathlib import Path
import pytest
from simulate import run


def record(name, result):
    directory = os.environ.get("HALLWAY_RESULTS")
    if directory:
        path = Path(directory)
        path.mkdir(parents=True, exist_ok=True)
        (path / (name + ".json")).write_text(json.dumps(result, indent=2, allow_nan=False) + "\n")


@pytest.mark.parametrize("layout,width,prefer", [("straight", 1.0, "left"),
    ("corner", 1.4, "left"), ("junction", 1.4, "left"), ("junction", 1.4, "right"),
    ("blocked", 1.4, "left")])
def test_hallway_physics(layout, width, prefer):
    result = run(layout, width, 40, sim_root=os.environ.get("GO2_SIM_ROOT"), prefer=prefer)
    record(f"{layout}-{width}-{prefer}", result)
    assert result["started"]
    assert result["wall_contacts"] == 0
    assert result["mode"] == "standing"
    assert result["status"]["command"] == (0, 0)
    expected = ("blocked_or_dead_end", "exploration_limit", "insufficient_turn_space") if width == 1.0 else ("blocked_or_dead_end", "exploration_limit")
    assert result["status"]["reason"] in expected
    if layout in ("corner", "junction"):
        assert result["status"]["turns"] == 1
        assert result["final_position"][1] * (1 if prefer == "left" else -1) > 2
    elif layout == "straight":
        assert result["displacement_m"] > 4
    else:
        assert .5 < result["final_position"][0] < 2.0


@pytest.mark.parametrize("sensor", ["scan", "odom"])
def test_sensor_loss_stops_actual_walking(sensor):
    result = run("straight", 1.4, 12, sim_root=os.environ.get("GO2_SIM_ROOT"), fault=sensor)
    record("sensor-loss-" + sensor, result)
    assert result["wall_contacts"] == 0
    assert result["stopped_at_s"] < 4.45
    expected = ("stale_scan",) if sensor == "scan" else (
        "stale_odom", "Cloud and odometry captures differ by more than 100 ms")
    assert result["status"]["reason"] in expected
    assert result["mode"] == "standing"
