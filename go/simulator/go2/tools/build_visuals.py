#!/usr/bin/env python3
"""Build reduced Go2 render meshes; never modify the original physics assets.

Install requirements-visuals.txt in the project venv for generation. --check
requires only the standard library and verifies original and generated hashes.
Normal builds reproduce visuals.lock.json; --update-lock records an intentional
new generation. Output OBJ basenames match the original MJCF mesh references.
"""

from __future__ import annotations

import argparse
from collections import Counter
import hashlib
from importlib.metadata import version
import json
from pathlib import Path
import platform
import sys
import tempfile
import xml.etree.ElementTree as ET

from fetch_assets import destination, sha256, valid


ROOT = Path(__file__).resolve().parents[1]
ALGORITHM = {
    "name": "fast-simplification quadric-error edge collapse",
    "version": "0.1.12",
    "numpy_version": "1.26.4",
    "target_reduction": 0.70,
    "aggressiveness": 7,
    "weld": "exact positions within each original material mesh",
    "normals": "area-weighted face-corner normals with 60-degree crease threshold",
    "normal_decimal_places": 10,
    "uv": "discarded; pinned MJCF uses solid materials",
    "max_bounds_change_m": 0.002,
    "bounds_failure": "retain the complete original mesh and normals",
}


def atomic_write(path: Path, content: bytes) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    temporary = None
    try:
        with tempfile.NamedTemporaryFile(dir=path.parent, prefix=f".{path.name}.", suffix=".partial", delete=False) as stream:
            temporary = Path(stream.name)
            stream.write(content)
        temporary.chmod(0o644)
        temporary.replace(path)
    finally:
        if temporary is not None:
            temporary.unlink(missing_ok=True)


def verify_inputs(asset_dir: Path) -> dict:
    lock = json.loads((ROOT / "assets.lock.json").read_text())
    for entry in lock["files"]:
        if not valid(destination(asset_dir, entry["path"]), entry):
            raise ValueError(f"Original asset missing or altered: {entry['path']}; run tools/fetch_assets.py --check")
    return lock


def read_obj(path: Path):
    """Read pinned triangular solid-material OBJ geometry; reject unsupported faces."""
    import numpy as np

    vertices, faces = [], []
    materials = set()
    for line in path.read_text().splitlines():
        parts = line.split()
        if not parts or parts[0].startswith("#"):
            continue
        if parts[0] == "v":
            if len(parts) != 4:
                raise ValueError(f"Unsupported vertex record in {path}")
            vertices.append([float(value) for value in parts[1:]])
        elif parts[0] == "f":
            if len(parts) != 4:
                raise ValueError(f"Expected triangular faces in {path}")
            indices = [int(value.split("/")[0]) for value in parts[1:]]
            if any(index == 0 for index in indices):
                raise ValueError(f"Zero OBJ vertex index in {path}")
            faces.append([index - 1 if index > 0 else len(vertices) + index for index in indices])
        elif parts[0] == "usemtl":
            materials.add(" ".join(parts[1:]))
        elif parts[0] not in {"vn", "vt", "mtllib", "o", "g", "s"}:
            raise ValueError(f"Unsupported OBJ record {parts[0]!r} in {path}")
    if len(materials) > 1:
        raise ValueError(f"Multiple material regions require separate simplification: {path}")
    points = np.asarray(vertices, dtype=np.float64)
    triangles = np.asarray(faces, dtype=np.int32)
    if points.ndim != 2 or points.shape[1] != 3 or not np.isfinite(points).all():
        raise ValueError(f"Invalid OBJ vertices in {path}")
    if triangles.ndim != 2 or triangles.shape[1] != 3 or triangles.min() < 0 or triangles.max() >= len(points):
        raise ValueError(f"Invalid OBJ face indices in {path}")
    return points, triangles


def clean_mesh(points, triangles):
    import numpy as np

    # Source OBJ duplicates vertices at UV and normal seams. Weld within one
    # material part so the simplifier sees shared edges. Do not merge parts.
    points, remap = np.unique(points, axis=0, return_inverse=True)
    triangles = remap[triangles].astype(np.int32)
    crosses = np.cross(points[triangles[:, 1]] - points[triangles[:, 0]], points[triangles[:, 2]] - points[triangles[:, 0]])
    triangles = triangles[np.linalg.norm(crosses, axis=1) > 1e-16]
    if not len(triangles):
        raise ValueError("Mesh contains no nondegenerate triangles")
    return points, triangles


def corner_normals(points, triangles):
    """Smooth neighboring faces within 60 degrees while retaining hard creases."""
    import numpy as np

    areas = np.cross(points[triangles[:, 1]] - points[triangles[:, 0]], points[triangles[:, 2]] - points[triangles[:, 0]])
    lengths = np.linalg.norm(areas, axis=1)
    if np.any(lengths <= 1e-16):
        raise ValueError("Cannot calculate normals for a degenerate triangle")
    face_normals = areas / lengths[:, None]
    flat = triangles.ravel()
    order = np.argsort(flat, kind="stable")
    offsets = np.concatenate(([0], np.cumsum(np.bincount(flat, minlength=len(points)))))
    normals = np.empty((len(flat), 3), dtype=np.float64)
    for vertex in range(len(points)):
        corners = order[offsets[vertex]:offsets[vertex + 1]]
        if not len(corners):
            continue
        adjacent = corners // 3
        local = face_normals[adjacent]
        weights = local @ local.T >= 0.5  # cos(60 degrees)
        smoothed = weights @ areas[adjacent]
        norms = np.linalg.norm(smoothed, axis=1)
        if np.any(norms <= 1e-16):
            raise ValueError("Degenerate smoothed normal")
        normals[corners] = smoothed / norms[:, None]
    # Equal smooth normals share an OBJ normal index. Face-corner references
    # keep sharp edges without duplicating positions or changing triangles.
    normals, indices = np.unique(np.round(normals, ALGORITHM["normal_decimal_places"]), axis=0, return_inverse=True)
    return normals, indices.reshape((-1, 3))


def obj_bytes(points, triangles) -> bytes:
    normals, normal_indices = corner_normals(points, triangles)
    lines = ["# Wendy Go2 visual LOD; derived from pinned assets; see visuals.lock.json and UPSTREAM.md"]
    lines.extend("v " + " ".join(format(value, ".17g") for value in point) for point in points)
    lines.extend("vn " + " ".join(format(value, ".10f") for value in normal) for normal in normals)
    lines.extend("f " + " ".join(f"{int(vertex) + 1}//{int(normal) + 1}" for vertex, normal in zip(face, face_normals)) for face, face_normals in zip(triangles, normal_indices))
    return ("\n".join(lines) + "\n").encode()


def check_outputs(lock: dict, output_dir: Path, asset_dir: Path) -> None:
    if lock.get("schema_version") != 1 or lock.get("algorithm") != ALGORITHM:
        raise ValueError("Visual lock schema or algorithm differs; rebuild intentionally with --update-lock")
    if lock.get("generator_sha256") != sha256(Path(__file__)):
        raise ValueError("Visual generator differs from the lock")
    if lock.get("input_asset_lock_sha256") != sha256(ROOT / "assets.lock.json"):
        raise ValueError("Original asset lock differs from the visual lock")
    for entry in lock["files"]:
        if not valid(destination(output_dir, entry["path"]), entry):
            raise ValueError(f"Generated visual missing or altered: {entry['path']}")
        if not valid(destination(asset_dir, entry["input"]["path"]), entry["input"]):
            raise ValueError(f"Visual input differs: {entry['input']['path']}")


def build(asset_dir: Path, output_dir: Path, lock_path: Path, update_lock: bool) -> dict:
    import fast_simplification
    import numpy as np

    if version("fast-simplification") != ALGORITHM["version"] or np.__version__ != ALGORITHM["numpy_version"]:
        raise ValueError("Install the exact versions in requirements-visuals.txt before generating")
    if output_dir.resolve().is_relative_to((asset_dir / "robot").resolve()):
        raise ValueError("Visual output must not overwrite the original robot directory")
    if lock_path.resolve().is_relative_to(asset_dir.resolve()) or lock_path.resolve() == (ROOT / "assets.lock.json").resolve():
        raise ValueError("Visual lock must not overwrite original asset files")
    expected = None if update_lock else json.loads(lock_path.read_text())
    if expected is not None and (expected.get("algorithm") != ALGORITHM or expected.get("generator_sha256") != sha256(Path(__file__))):
        raise ValueError("Generator or algorithm changed; use --update-lock for an intentional change")
    generator_platform = f"{platform.system()}/{platform.machine()}"
    if expected is not None and expected.get("generator_platform") != generator_platform:
        raise ValueError("Generate the locked visuals in the Linux ARM64 Docker build; native compiler output differs")
    sources = verify_inputs(asset_dir)
    entries = {entry["path"]: entry for entry in sources["files"]}
    model = ET.parse(asset_dir / "robot/go2.xml").getroot()
    mesh_assets = model.findall("asset/mesh")
    instances = Counter(geom.get("mesh") for geom in model.findall(".//geom") if geom.get("mesh"))
    materials = {name: sorted({geom.get("material", "") for geom in model.findall(".//geom") if geom.get("mesh") == name}) for name in instances}
    generated = []
    payloads = []
    for mesh in mesh_assets:
        filename = mesh.attrib["file"]
        mesh_name = mesh.get("name", Path(filename).stem)
        source = entries[f"robot/assets/{filename}"]
        points, triangles = read_obj(destination(asset_dir, source["path"]))
        input_triangles, input_vertices = len(triangles), len(points)
        input_bounds = np.array([points.min(axis=0), points.max(axis=0)])
        points, triangles = clean_mesh(points, triangles)
        welded_vertices, clean_triangles = len(points), len(triangles)
        points, triangles = fast_simplification.simplify(points, triangles, target_reduction=ALGORITHM["target_reduction"], agg=ALGORITHM["aggressiveness"])
        points, triangles = clean_mesh(points, triangles)
        if not np.isfinite(points).all() or len(triangles) >= input_triangles:
            raise ValueError(f"Invalid or unreduced output: {filename}")
        bounds = np.array([points.min(axis=0), points.max(axis=0)])
        attempted_bounds_change = float(np.max(np.abs(bounds - input_bounds)))
        retained_original = attempted_bounds_change > ALGORITHM["max_bounds_change_m"]
        if retained_original:
            # Thin/disconnected front details can disappear during decimation.
            # Preserve that entire original part rather than distort it further.
            data = destination(asset_dir, source["path"]).read_bytes()
            points, triangles = read_obj(destination(asset_dir, source["path"]))
            bounds = input_bounds
        else:
            data = obj_bytes(points, triangles)
        result = {
            "path": filename,
            "size": len(data),
            "sha256": hashlib.sha256(data).hexdigest(),
            "input": {key: source[key] for key in ("path", "size", "sha256")},
            "input_vertices": input_vertices,
            "welded_vertices": welded_vertices,
            "input_triangles": input_triangles,
            "clean_input_triangles": clean_triangles,
            "vertices": len(points),
            "triangles": len(triangles),
            "render_instances": instances[mesh_name],
            "mjcf_materials": materials[mesh_name],
            "retained_original": retained_original,
            "attempted_bounds_change_m": attempted_bounds_change,
            "bounds_max_change_m": float(np.max(np.abs(bounds - input_bounds))),
        }
        generated.append(result)
        payloads.append((destination(output_dir, filename), data))
        print(f"{filename}: {input_triangles} -> {len(triangles)} triangles", flush=True)
    manifest = {
        "schema_version": 1,
        "algorithm": ALGORITHM,
        "generator_sha256": sha256(Path(__file__)),
        "generator_platform": generator_platform,
        "input_asset_lock_sha256": sha256(ROOT / "assets.lock.json"),
        "physics_model_modified": False,
        "input_render_triangles": sum(entry["input_triangles"] * entry["render_instances"] for entry in generated),
        "output_render_triangles": sum(entry["triangles"] * entry["render_instances"] for entry in generated),
        "files": generated,
    }
    if expected is not None and manifest != expected:
        raise ValueError("Generation differs from visuals.lock.json; outputs were not replaced")
    for path, data in payloads:
        atomic_write(path, data)
    if update_lock:
        atomic_write(lock_path, (json.dumps(manifest, indent=2) + "\n").encode())
    return manifest


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--asset-dir", type=Path, default=ROOT / "assets")
    parser.add_argument("--output-dir", type=Path, default=ROOT / "assets/visuals")
    parser.add_argument("--lock", type=Path, default=ROOT / "visuals.lock.json")
    parser.add_argument("--check", action="store_true", help="Verify all original assets and generated hashes offline")
    parser.add_argument("--update-lock", action="store_true", help="Intentionally regenerate the visual lock")
    args = parser.parse_args()
    if args.check and args.update_lock:
        parser.error("--check and --update-lock are mutually exclusive")
    if args.check:
        verify_inputs(args.asset_dir)
        manifest = json.loads(args.lock.read_text())
        check_outputs(manifest, args.output_dir, args.asset_dir)
    else:
        manifest = build(args.asset_dir, args.output_dir, args.lock, args.update_lock)
    print(f"Verified {len(manifest['files'])} visual meshes; rendered triangles {manifest['input_render_triangles']} -> {manifest['output_render_triangles']}")
    return 0


if __name__ == "__main__":
    try:
        sys.exit(main())
    except (OSError, ValueError, KeyError, ImportError) as error:
        print(f"Visual generation failed: {error}", file=sys.stderr)
        sys.exit(1)
