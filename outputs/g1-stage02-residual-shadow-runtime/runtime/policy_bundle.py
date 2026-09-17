"""Manifest-driven policy bundle validation for the fixed Stage 2 runtime ABI.

Checkpoint identity is intentionally data, not runtime source.  A new simulator
checkpoint can be installed without editing Python as long as it implements the
already-installed observation/action/rate/source ABI and every consumed file is
sealed by the bundle manifest.
"""
from __future__ import annotations

from dataclasses import dataclass
import hashlib
import json
from pathlib import Path, PurePosixPath
import re
from typing import Any

import numpy as np

from .policy_abi import (
    EXPECTED_ARCHITECTURE,
    EXPECTED_OWNED_INDICES,
    EXPECTED_SOURCE_SHA256,
    POLICY_MANIFEST,
    POLICY_SCHEMA,
    RUNTIME_ABI,
)
REQUIRED_FILES = {
    "checkpoint.pt",
    "reference-contract.npz",
    *EXPECTED_SOURCE_SHA256,
}
_CANDIDATE_ID = re.compile(r"^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$")
_SHA256 = re.compile(r"^[0-9a-f]{64}$")


@dataclass(frozen=True)
class PolicyBundleIdentity:
    candidate_id: str
    checkpoint_sha256: str
    checkpoint_update: int
    reference_contract_sha256: str
    reference_frames: int
    manifest: dict[str, Any]

    def public(self) -> dict[str, Any]:
        return {
            "schema": POLICY_SCHEMA,
            "runtime_abi": RUNTIME_ABI,
            "candidate_id": self.candidate_id,
            "checkpoint_sha256": self.checkpoint_sha256,
            "checkpoint_update": self.checkpoint_update,
            "reference_contract_sha256": self.reference_contract_sha256,
            "reference_frames": self.reference_frames,
        }


def sha256_file(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as source:
        for chunk in iter(lambda: source.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def _safe_file(root: Path, relative: str) -> Path:
    logical = PurePosixPath(relative)
    if logical.is_absolute() or ".." in logical.parts or not logical.parts:
        raise ValueError(f"unsafe bundle path: {relative!r}")
    path = root.joinpath(*logical.parts)
    if path.is_symlink() or not path.is_file():
        raise ValueError(f"bundle file is missing or not regular: {relative}")
    try:
        path.resolve().relative_to(root.resolve())
    except ValueError as exc:
        raise ValueError(f"bundle path escapes root: {relative}") from exc
    return path


def verify_runtime_bundle(bundle: Path) -> PolicyBundleIdentity:
    bundle = Path(bundle)
    manifest_path = bundle / POLICY_MANIFEST
    if manifest_path.is_symlink() or not manifest_path.is_file():
        raise ValueError(f"{POLICY_MANIFEST} is required")
    manifest = json.loads(manifest_path.read_text())
    if manifest.get("schema") != POLICY_SCHEMA:
        raise ValueError("unsupported policy bundle schema")
    if manifest.get("runtime_abi") != RUNTIME_ABI:
        raise ValueError("policy bundle targets a different runtime ABI")

    candidate_id = manifest.get("candidate_id")
    if not isinstance(candidate_id, str) or not _CANDIDATE_ID.fullmatch(candidate_id):
        raise ValueError("invalid candidate_id")
    checkpoint_update = manifest.get("checkpoint_update")
    if isinstance(checkpoint_update, bool) or not isinstance(checkpoint_update, int) or checkpoint_update < 0:
        raise ValueError("checkpoint_update must be a non-negative integer")

    contract = manifest.get("policy_contract")
    expected_contract = {
        "architecture": EXPECTED_ARCHITECTURE,
        "observation_dimension": 316,
        "action_dimension": 15,
        "owned_indices": EXPECTED_OWNED_INDICES,
        "control_hz": 40,
        "camera_hz": 20,
        "recurrent_memory": "GRU-128",
    }
    if not isinstance(contract, dict):
        raise ValueError("policy_contract is required")
    for key, expected in expected_contract.items():
        if contract.get(key) != expected:
            raise ValueError(f"policy contract mismatch: {key}")

    files = manifest.get("files")
    if not isinstance(files, dict) or not REQUIRED_FILES.issubset(files):
        raise ValueError("bundle file seal is incomplete")
    reference_result_path = manifest.get("reference_result_path")
    if not isinstance(reference_result_path, str) or reference_result_path not in files:
        raise ValueError("reference_result_path is missing from the file seal")
    for relative, expected in files.items():
        if not isinstance(relative, str) or not isinstance(expected, str) or not _SHA256.fullmatch(expected):
            raise ValueError("bundle file seal contains an invalid entry")
        actual = sha256_file(_safe_file(bundle, relative))
        if actual != expected:
            raise ValueError(f"runtime bundle SHA-256 mismatch: {relative}")
    for relative, expected in EXPECTED_SOURCE_SHA256.items():
        if files.get(relative) != expected:
            raise ValueError(f"policy source differs from runtime ABI: {relative}")

    checkpoint_sha256 = files["checkpoint.pt"]
    if manifest.get("checkpoint_sha256") != checkpoint_sha256:
        raise ValueError("checkpoint identity differs from file seal")
    reference_sha256 = files["reference-contract.npz"]
    if manifest.get("reference_contract_sha256") != reference_sha256:
        raise ValueError("reference identity differs from file seal")

    with np.load(_safe_file(bundle, "reference-contract.npz"), allow_pickle=False) as values:
        required_arrays = {
            "joint_names",
            "reference_joint_targets_43",
            "reference_owned_velocities_15",
        }
        if not required_arrays.issubset(values.files):
            raise ValueError("reference contract arrays are incomplete")
        names = tuple(str(value) for value in values["joint_names"].tolist())
        targets = np.asarray(values["reference_joint_targets_43"])
        velocities = np.asarray(values["reference_owned_velocities_15"])
    if len(names) != 43 or len(set(names)) != 43:
        raise ValueError("reference joint order must contain 43 unique names")
    if (
        targets.ndim != 2
        or targets.shape[1] != 43
        or velocities.shape != (len(targets), 15)
        or len(targets) < 1
        or not np.isfinite(targets).all()
        or not np.isfinite(velocities).all()
    ):
        raise ValueError("reference contract shapes or values changed")

    result = json.loads(_safe_file(bundle, reference_result_path).read_text())
    bounds = np.asarray(result.get("joint_bounds"), dtype=float)
    if bounds.shape != (43, 2) or not np.isfinite(bounds).all() or not np.all(bounds[:, 0] < bounds[:, 1]):
        raise ValueError("reference result joint_bounds must be finite [43,2]")

    return PolicyBundleIdentity(
        candidate_id=candidate_id,
        checkpoint_sha256=checkpoint_sha256,
        checkpoint_update=checkpoint_update,
        reference_contract_sha256=reference_sha256,
        reference_frames=len(targets),
        manifest=manifest,
    )
