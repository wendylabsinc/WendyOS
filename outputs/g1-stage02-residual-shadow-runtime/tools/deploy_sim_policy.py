#!/usr/bin/env python3
"""Validate and deploy a same-ABI simulator policy without editing runtime code.

Examples:
  python3 tools/deploy_sim_policy.py /path/to/sim-export
  python3 tools/deploy_sim_policy.py https://host/run/policy.tar.gz --deploy

Without --deploy this only creates and validates a minimal Wendy deployment
context.  With --deploy it updates the motion-zero inference app; the physical
wrapper remains a separate, operator-confirmed command owner.
"""
from __future__ import annotations

import argparse
import hashlib
import json
import math
import os
from pathlib import Path, PurePosixPath
import shutil
import subprocess
import sys
import tarfile
import tempfile
import time
from typing import Any
import urllib.parse
import urllib.request

HERE = Path(__file__).resolve()
TEMPLATE = HERE.parents[1]
sys.path.insert(0, str(TEMPLATE))

from runtime.policy_abi import (  # noqa: E402
    EXPECTED_ARCHITECTURE,
    EXPECTED_OWNED_INDICES,
    EXPECTED_SOURCE_SHA256,
    POLICY_SCHEMA,
    RUNTIME_ABI,
)


MAXIMUM_DOWNLOAD_BYTES = 512 * 1024 * 1024
APP_ID = "g1-stage02-residual-shadow-runtime"


def sha256_file(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as source:
        for chunk in iter(lambda: source.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def _download(url: str, destination: Path) -> Path:
    parsed = urllib.parse.urlsplit(url)
    if parsed.scheme not in {"http", "https"} or not parsed.hostname:
        raise ValueError("policy URL must use http or https")
    request = urllib.request.Request(url, headers={"User-Agent": "wendy-policy-import/1"})
    total = 0
    with urllib.request.urlopen(request, timeout=60) as response, destination.open("wb") as output:
        declared = int(response.headers.get("Content-Length", "0") or 0)
        if declared > MAXIMUM_DOWNLOAD_BYTES:
            raise ValueError("policy archive exceeds 512 MiB")
        while True:
            chunk = response.read(1024 * 1024)
            if not chunk:
                break
            total += len(chunk)
            if total > MAXIMUM_DOWNLOAD_BYTES:
                raise ValueError("policy archive exceeds 512 MiB")
            output.write(chunk)
    return destination


def _extract_tar(archive: Path, destination: Path) -> Path:
    destination.mkdir(parents=True, exist_ok=False)
    with tarfile.open(archive, "r:*") as value:
        members = value.getmembers()
        for member in members:
            logical = PurePosixPath(member.name)
            if logical.is_absolute() or ".." in logical.parts or member.issym() or member.islnk():
                raise ValueError(f"unsafe archive member: {member.name}")
            if not (member.isdir() or member.isfile()):
                raise ValueError(f"unsupported archive member: {member.name}")
        value.extractall(destination, members=members, filter="data")
    return destination


def _source_root(path: Path) -> Path:
    if path.is_dir() and (path / "checkpoint.pt").is_file():
        return path
    candidates = [item.parent for item in path.rglob("checkpoint.pt") if item.is_file()]
    if len(candidates) != 1:
        raise ValueError(f"expected one checkpoint.pt, found {len(candidates)}")
    return candidates[0]


def _read_source_manifest(root: Path) -> dict[str, Any]:
    for name in ("POLICY.json", "MANIFEST.json", "manifest.json"):
        path = root / name
        if path.is_file():
            value = json.loads(path.read_text())
            if isinstance(value, dict):
                return value
    return {}


def _find_reference_result(root: Path) -> Path:
    candidates = []
    for path in (root / "reference").rglob("result.json"):
        try:
            value = json.loads(path.read_text())
            bounds = value.get("joint_bounds")
            valid = (
                isinstance(bounds, list)
                and len(bounds) == 43
                and all(
                    isinstance(row, list)
                    and len(row) == 2
                    and all(isinstance(item, (int, float)) and math.isfinite(item) for item in row)
                    and row[0] < row[1]
                    for row in bounds
                )
            )
            if valid:
                candidates.append(path)
        except (OSError, ValueError, TypeError, json.JSONDecodeError):
            continue
    if len(candidates) != 1:
        raise ValueError(f"expected one reference result with [43,2] joint_bounds, found {len(candidates)}")
    return candidates[0]


def _inspect_checkpoint(
    path: Path, source_manifest: dict[str, Any], root: Path
) -> tuple[int, dict[str, Any], bool]:
    try:
        import torch
    except ImportError:
        update = source_manifest.get("checkpoint_update")
        if isinstance(update, bool) or not isinstance(update, int) or update < 0:
            raise RuntimeError(
                "PyTorch is unavailable and the source manifest has no valid checkpoint_update"
            )
        contract = source_manifest.get("policy_contract") or source_manifest
        checkpoint_contract = root / "checkpoint-contract.json"
        if checkpoint_contract.is_file():
            contract = json.loads(checkpoint_contract.read_text())
        declared = source_manifest.get("checkpoint_sha256")
        files = source_manifest.get("files") or {}
        file_entry = files.get("checkpoint.pt")
        if isinstance(file_entry, dict):
            file_entry = file_entry.get("sha256")
        declared = declared or file_entry
        if declared != sha256_file(path):
            raise ValueError("source manifest checkpoint hash does not match checkpoint.pt")
        architecture = contract.get("architecture")
        sensor = contract.get("sensor_input_contract") or {}
        observation = (
            contract.get("observation_dimension")
            or contract.get("embedded_actor_input_dim")
            or sensor.get("actor_input_dim")
        )
        owned = contract.get("owned_indices") or contract.get("owned_sim_action_indices")
        action = contract.get("action_dimension") or contract.get("action_dim") or len(owned or ())
        if (
            architecture != EXPECTED_ARCHITECTURE
            or observation != 316
            or action != 15
            or owned != EXPECTED_OWNED_INDICES
            or contract.get("control_hz") != 40
            or contract.get("camera_hz") != 20
        ):
            raise ValueError("source manifest differs from the installed runtime ABI")
        return update, dict(contract), False
    payload = torch.load(path, map_location="cpu", weights_only=False)
    update = payload.get("update")
    if isinstance(update, bool) or not isinstance(update, int) or update < 0:
        raise ValueError("checkpoint update must be a non-negative integer")
    contract = payload.get("contract") or {}
    state = payload.get("model") or {}
    if contract.get("architecture") != EXPECTED_ARCHITECTURE:
        raise ValueError("checkpoint architecture differs from the installed runtime ABI")
    if contract.get("control_hz") != 40 or contract.get("camera_hz") != 20:
        raise ValueError("checkpoint rates differ from the installed 40/20 Hz ABI")
    if contract.get("owned_indices") != EXPECTED_OWNED_INDICES:
        raise ValueError("checkpoint owned-joint map differs from the installed ABI")
    sensor = contract.get("sensor_input_contract") or {}
    if sensor.get("actor_input_dim") != 316:
        raise ValueError("checkpoint does not expose the 316-input actor contract")
    if tuple(getattr(state.get("trunk.0.weight"), "shape", ())) != (256, 316):
        raise ValueError("checkpoint trunk tensor is not [256,316]")
    if tuple(getattr(state.get("actor.weight"), "shape", ())) != (15, 128):
        raise ValueError("checkpoint actor tensor is not [15,128]")
    return update, contract, True


def _copy_file(source: Path, destination: Path) -> str:
    destination.parent.mkdir(parents=True, exist_ok=True)
    shutil.copy2(source, destination)
    return sha256_file(destination)


def build_context(source: Path, destination: Path) -> dict[str, Any]:
    root = _source_root(source)
    source_manifest = _read_source_manifest(root)
    checkpoint = root / "checkpoint.pt"
    reference = root / "reference-contract.npz"
    if not reference.is_file():
        raise ValueError("reference-contract.npz is required")
    update, _checkpoint_contract, tensor_validated = _inspect_checkpoint(
        checkpoint, source_manifest, root
    )
    candidate_id = str(source_manifest.get("candidate_id") or root.name)
    result = _find_reference_result(root)

    if destination.exists():
        raise FileExistsError(f"deployment context already exists: {destination}")
    bundle = destination / "bundle"
    files: dict[str, str] = {}
    files["checkpoint.pt"] = _copy_file(checkpoint, bundle / "checkpoint.pt")
    files["reference-contract.npz"] = _copy_file(reference, bundle / "reference-contract.npz")
    files["reference/result.json"] = _copy_file(result, bundle / "reference/result.json")
    for relative, expected in EXPECTED_SOURCE_SHA256.items():
        source_file = root / relative
        if not source_file.is_file():
            raise ValueError(f"sim export is missing runtime ABI source: {relative}")
        actual = _copy_file(source_file, bundle / relative)
        if actual != expected:
            raise ValueError(f"sim export source differs from installed ABI: {relative}")
        files[relative] = actual

    policy = {
        "schema": POLICY_SCHEMA,
        "runtime_abi": RUNTIME_ABI,
        "candidate_id": candidate_id,
        "checkpoint_update": update,
        "checkpoint_sha256": files["checkpoint.pt"],
        "reference_contract_sha256": files["reference-contract.npz"],
        "reference_result_path": "reference/result.json",
        "policy_contract": {
            "architecture": EXPECTED_ARCHITECTURE,
            "observation_dimension": 316,
            "action_dimension": 15,
            "owned_indices": EXPECTED_OWNED_INDICES,
            "control_hz": 40,
            "camera_hz": 20,
            "recurrent_memory": "GRU-128",
        },
        "files": files,
        "source_qualification": source_manifest.get("qualification")
        or source_manifest.get("deployment_state")
        or {"simulation_only": True, "physical_robot_qualified": False},
    }
    (bundle / "POLICY.json").write_text(json.dumps(policy, indent=2) + "\n")

    shutil.copytree(
        TEMPLATE / "runtime",
        destination / "runtime",
        ignore=shutil.ignore_patterns("__pycache__", "*.pyc"),
    )
    shutil.copy2(TEMPLATE / "build.stagefile.yaml", destination / "build.stagefile.yaml")
    version = f"0.3.0-policy-{files['checkpoint.pt'][:12]}"
    (destination / "wendy.json").write_text(
        json.dumps(
            {
                "appId": APP_ID,
                "version": version,
                "platform": "linux",
                "entitlements": [{"type": "network"}, {"type": "gpu"}],
                "readiness": {"tcpSocket": {"port": 8117}, "timeoutSeconds": 120},
            },
            indent=2,
        )
        + "\n"
    )
    emitted = json.loads((bundle / "POLICY.json").read_text())
    for relative, expected in emitted["files"].items():
        if sha256_file(bundle / relative) != expected:
            raise ValueError(f"emitted bundle hash verification failed: {relative}")
    active_policy = {
        "schema": POLICY_SCHEMA,
        "runtime_abi": RUNTIME_ABI,
        "candidate_id": candidate_id,
        "checkpoint_sha256": files["checkpoint.pt"],
        "checkpoint_update": update,
        "reference_contract_sha256": files["reference-contract.npz"],
        "reference_frames": "validated on device before readiness",
    }
    return {
        "context": str(destination),
        "version": version,
        "active_policy": active_policy,
        "local_checkpoint_tensor_validation": tensor_validated,
        "on_device_contract_validation": "tensor shapes and reference arrays required before readiness",
    }


def deploy(context: Path, *, device: str) -> None:
    subprocess.run(
        [
            "wendy",
            "run",
            "--device",
            device,
            "--prefix",
            str(context),
            "--dockerfile",
            "build.stagefile.yaml",
            "--chunking",
            "off",
            "--detach",
            "--yes",
        ],
        check=True,
    )


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("source", help="sim bundle directory, tar archive, or HTTP(S) archive URL")
    parser.add_argument("--device", default="unitree-g1-nx-2.local")
    parser.add_argument("--output-root", type=Path, default=TEMPLATE.parent / "g1-policy-deployments")
    parser.add_argument("--deploy", action="store_true", help="deploy after local ABI validation")
    args = parser.parse_args()

    with tempfile.TemporaryDirectory(prefix="g1-policy-import-") as temporary:
        temp = Path(temporary)
        source_value = args.source
        if urllib.parse.urlsplit(source_value).scheme in {"http", "https"}:
            archive = _download(source_value, temp / "policy.tar")
            source = _extract_tar(archive, temp / "extracted")
        else:
            path = Path(source_value).expanduser().resolve()
            if path.is_dir():
                source = path
            elif path.is_file() and tarfile.is_tarfile(path):
                source = _extract_tar(path, temp / "extracted")
            else:
                raise ValueError("source must be a bundle directory or tar archive")

        root = _source_root(source)
        source_manifest = _read_source_manifest(root)
        candidate = str(source_manifest.get("candidate_id") or root.name)
        checkpoint_hash = sha256_file(root / "checkpoint.pt")
        safe_candidate = "".join(ch if ch.isalnum() or ch in "._-" else "-" for ch in candidate)[:96]
        destination = args.output_root.expanduser().resolve() / f"{safe_candidate}-{checkpoint_hash[:12]}"
        result = build_context(root, destination)
        print(json.dumps(result, indent=2), flush=True)
        if args.deploy:
            deploy(destination, device=args.device)
            print(json.dumps({"deployed": True, "device": args.device, **result}, indent=2), flush=True)


if __name__ == "__main__":
    main()
