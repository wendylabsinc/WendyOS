"""Integrity checks for the explicit native ROS compatibility derivative."""

import hashlib
import json
from pathlib import Path
import shutil
import sys

import pytest

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT / "tools"))
from prepare_unitree import PATCHES, SOURCE_PREFIX, prepare


@pytest.fixture
def package_root(tmp_path):
    source = ROOT / SOURCE_PREFIX
    if not source.exists():
        pytest.skip("fetch_unitree.py assets required")
    shutil.copytree(source, tmp_path / SOURCE_PREFIX)
    manifest = json.loads((ROOT / "unitree.lock.json").read_text())
    return tmp_path, manifest


def test_derivative_changes_exactly_two_files_and_preserves_inputs(package_root):
    root, manifest = package_root
    output = root / "generated"
    original = {entry["path"]: (root / entry["path"]).read_bytes()
                for entry in manifest["files"] if entry["path"].startswith(SOURCE_PREFIX)}
    assert prepare(root, output, manifest) == 0
    changed = []
    for path, data in original.items():
        assert (root / path).read_bytes() == data
        relative = path[len(SOURCE_PREFIX):]
        if (output / relative).read_bytes() != data:
            changed.append(relative)
    assert set(changed) == PATCHES.keys()
    assert b"PathPoint[10] path_point" in (output / "unitree_go/msg/SportModeState.msg").read_bytes()
    assert b"uint8[] binary" in (output / "unitree_api/msg/Response.msg").read_bytes()
    provenance_bytes = (output / "wendy-compatibility.json").read_bytes()
    for entry in json.loads(provenance_bytes)["files"]:
        data = (output / entry["path"]).read_bytes()
        assert hashlib.sha256(data).hexdigest() == entry["sha256"]
        assert len(data) == entry["size"]
    assert prepare(root, output, manifest, check=True) == 0
    assert prepare(root, output, manifest) == 0
    assert (output / "wendy-compatibility.json").read_bytes() == provenance_bytes


def test_modified_source_rejected_before_output_write(package_root):
    root, manifest = package_root
    source = root / SOURCE_PREFIX / "unitree_go/msg/PathPoint.msg"
    source.write_bytes(source.read_bytes() + b"\n# tampered\n")
    output = root / "generated"
    with pytest.raises(ValueError, match="Pinned source mismatch"):
        prepare(root, output, manifest)
    assert not output.exists()


def test_changed_derivative_fails_offline_check(package_root):
    root, manifest = package_root
    output = root / "generated"
    prepare(root, output, manifest)
    derived = output / "unitree_go/msg/SportModeState.msg"
    derived.write_text("invalid")
    assert prepare(root, output, manifest, check=True) == 1
    assert derived.read_text() == "invalid"


def test_output_cannot_replace_pinned_source_tree(package_root):
    root, manifest = package_root
    with pytest.raises(ValueError, match="separate"):
        prepare(root, root / SOURCE_PREFIX, manifest)


def test_output_symlink_cannot_escape(package_root, tmp_path):
    root, manifest = package_root
    output = root / "generated"
    output.mkdir()
    outside = root / "outside"
    outside.mkdir()
    (output / "unitree_go").symlink_to(outside, target_is_directory=True)
    with pytest.raises(ValueError, match="escapes"):
        prepare(root, output, manifest)
    assert list(outside.iterdir()) == []
