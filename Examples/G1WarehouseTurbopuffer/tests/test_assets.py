import json
import re

from warehouse import assets


def test_lock_pins_every_third_party_file_to_a_commit_and_hash():
    lock = json.loads(assets.LOCK.read_text())
    for entry in lock["files"]:
        assert re.fullmatch(r"[0-9a-f]{64}", entry["sha256"]), entry["path"]
        assert re.search(r"/[0-9a-f]{40}/", entry["url"]), entry["url"]
    paths = {entry["path"] for entry in lock["files"]}
    assert "models/g1/g1_gear_wbc.xml" in paths
    assert "models/g1/policy/GR00T-WholeBodyControl-Walk.onnx" in paths
    assert "models/g1/policy/NVIDIA Open Model License" in paths
    assert "warehouse/static/three/addons/utils/BufferGeometryUtils.js" in paths


def test_every_mesh_the_model_uses_is_locked():
    xml = (assets.ROOT / "models/g1/g1_gear_wbc.xml").read_text()
    meshes = set(re.findall(r'file="([^"]+\.STL)"', xml))
    paths = {entry["path"] for entry in json.loads(assets.LOCK.read_text())["files"]}
    assert meshes and {f"models/g1/meshes/{mesh}" for mesh in meshes} <= paths


def test_fetched_assets_match_the_lock():
    assets.ensure(check_only=True)
