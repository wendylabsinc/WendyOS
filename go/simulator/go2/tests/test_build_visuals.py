"""Per-platform visual lock: section choice, updates, offline checks and messages."""

import hashlib
import json
from pathlib import Path
import sys

import pytest

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT / "tools"))
import build_visuals
from build_visuals import ALGORITHM, check_outputs, merge_platform, mismatch_message, platform_section


def test_section_is_chosen_by_platform_and_unknown_platforms_are_refused():
    lock = {"schema_version": 2, "platforms": {"Linux/aarch64": {"files": []}, "Linux/x86_64": {"files": [1]}}}
    assert platform_section(lock, "Linux/x86_64") == {"files": [1]}
    with pytest.raises(ValueError, match="Darwin/arm64"):
        platform_section(lock, "Darwin/arm64")


def test_update_rewrites_only_the_current_platform():
    existing = {"schema_version": 2, "generator_sha256": "old", "platforms": {"Linux/aarch64": {"files": ["a"]}}}
    merged = merge_platform(existing, {"schema_version": 2, "generator_sha256": "new"}, "Linux/x86_64", {"files": ["b"]})
    assert merged["platforms"] == {"Linux/aarch64": {"files": ["a"]}, "Linux/x86_64": {"files": ["b"]}}
    assert merged["generator_sha256"] == "new"
    assert merge_platform(None, {"schema_version": 2}, "Linux/x86_64", {"files": []})["platforms"] == {"Linux/x86_64": {"files": []}}


def test_committed_lock_keeps_the_arm64_entries_and_matches_the_generator():
    lock = json.loads((ROOT / "visuals.lock.json").read_text())
    assert lock["schema_version"] == 2 and set(lock["platforms"]) == {"Linux/aarch64", "Linux/x86_64"}
    assert "generator_platform" not in lock
    arm = lock["platforms"]["Linux/aarch64"]
    assert arm["output_render_triangles"] == 120417 and len(arm["files"]) == 16
    assert arm["files"][0]["sha256"] == "ce7a2db397df2ee213f0858c838bbacdb1ce17b119bdff227b905c93acacc942"
    assert len(lock["platforms"]["Linux/x86_64"]["files"]) == 16
    assert lock["generator_sha256"] == hashlib.sha256((ROOT / "tools/build_visuals.py").read_bytes()).hexdigest()


def _entry(directory: Path, name: str, content: bytes) -> dict:
    (directory / name).write_bytes(content)
    return {"path": name, "size": len(content), "sha256": hashlib.sha256(content).hexdigest()}


def test_check_uses_this_platforms_section_and_any_full_match_elsewhere(tmp_path, monkeypatch):
    assets, outputs = tmp_path / "assets", tmp_path / "visuals"
    assets.mkdir()
    outputs.mkdir()
    source = _entry(assets, "part.obj", b"original mesh")
    x86 = _entry(outputs, "part.obj", b"x86 output")
    arm = {"path": "part.obj", "size": 10, "sha256": hashlib.sha256(b"arm output").hexdigest()}
    lock = {
        "schema_version": 2,
        "algorithm": ALGORITHM,
        "generator_sha256": hashlib.sha256((ROOT / "tools/build_visuals.py").read_bytes()).hexdigest(),
        "input_asset_lock_sha256": hashlib.sha256((ROOT / "assets.lock.json").read_bytes()).hexdigest(),
        "physics_model_modified": False,
        "platforms": {
            "Linux/aarch64": {"input_render_triangles": 2, "output_render_triangles": 1, "files": [{**arm, "input": source}]},
            "Linux/x86_64": {"input_render_triangles": 2, "output_render_triangles": 1, "files": [{**x86, "input": source}]},
        },
    }
    monkeypatch.setattr(build_visuals, "current_platform", lambda: "Linux/x86_64")
    assert check_outputs(lock, outputs, assets) == lock["platforms"]["Linux/x86_64"]
    monkeypatch.setattr(build_visuals, "current_platform", lambda: "Linux/aarch64")
    with pytest.raises(ValueError):
        check_outputs(lock, outputs, assets)
    monkeypatch.setattr(build_visuals, "current_platform", lambda: "Darwin/arm64")
    assert check_outputs(lock, outputs, assets) == lock["platforms"]["Linux/x86_64"]


def test_x86_mismatch_names_the_cpu_requirement():
    assert "AVX2 and FMA" in mismatch_message("Linux/x86_64") and "--arch arm64" in mismatch_message("Linux/x86_64")
    assert "--arch" not in mismatch_message("Linux/aarch64")
    assert mismatch_message("Linux/aarch64") == "Generation differs from visuals.lock.json; outputs were not replaced"
