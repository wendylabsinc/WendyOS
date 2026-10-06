import json
import re

from drone_formation import assets


def test_lock_pins_every_third_party_file_to_a_commit_and_hash():
    lock = json.loads(assets.LOCK.read_text())
    for entry in lock["files"]:
        assert re.fullmatch(r"[0-9a-f]{64}", entry["sha256"]), entry["path"]
        assert re.search(r"/[0-9a-f]{40}/", entry["url"]), entry["url"]
    paths = {entry["path"] for entry in lock["files"]}
    assert "models/bitcraze_crazyflie_2/cf2.xml" in paths
    assert "drone_formation/static/three/three.module.js" in paths
    assert "drone_formation/static/three/addons/controls/OrbitControls.js" in paths


def test_fetched_assets_match_the_lock():
    assets.ensure(check_only=True)
