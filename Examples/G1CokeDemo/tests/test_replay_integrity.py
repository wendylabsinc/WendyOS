"""Small trusted-input integrity checks; no supplied model or scene execution."""
from unittest.mock import Mock

import pytest


@pytest.mark.parametrize("name", ["result.json", "qualification-60s.json", "verification.json", "replay_expert.py"])
def test_replay_rejects_modified_expert_inputs_before_model_load(tmp_path, monkeypatch, name):
    from pathlib import Path
    from coke_demo import simulation
    import hashlib
    import json

    # This is an integrity-policy fixture, not a supplied expert/model replay.
    # Keep the real lock's complete expert name set, including the controller
    # and verification inputs, but use small local bytes and their own hashes.
    # Actual pinned expert assets remain a separate scene acceptance requirement.
    source_lock = Path(__file__).resolve().parents[1] / "assets.lock.json"
    lock = json.loads(source_lock.read_text())
    protected = {"result.json", "qualification-60s.json", "verification.json", "replay_expert.py"}
    names = {key.removeprefix("expert/") for key in lock["files"] if key.startswith("expert/")}
    assert protected <= names
    fixture_root = tmp_path / "fixture-root"
    (fixture_root / "coke_demo").mkdir(parents=True)
    expert = tmp_path / "expert"
    expert.mkdir()
    fixture_lock = {"files": {}}
    for relative in lock["files"]:
        if not relative.startswith("expert/"):
            continue
        filename = relative.removeprefix("expert/")
        data = ("integrity fixture for " + filename).encode()
        fixture_lock["files"][relative] = {"sha256": hashlib.sha256(data).hexdigest()}
        destination = expert / filename
        destination.parent.mkdir(parents=True, exist_ok=True)
        destination.write_bytes(bytes([data[0] ^ 1]) + data[1:] if filename == name else data)
    (fixture_root / "assets.lock.json").write_text(json.dumps(fixture_lock))
    monkeypatch.setattr(simulation, "__file__", str(fixture_root / "coke_demo" / "simulation.py"))
    model_load = Mock(side_effect=AssertionError("model loaded before verification"))
    monkeypatch.setattr(simulation.mujoco.MjModel, "from_binary_path", model_load)
    with pytest.raises(ValueError, match=f"checksum mismatch: {name}"):
        simulation.Simulation(expert)
    model_load.assert_not_called()


