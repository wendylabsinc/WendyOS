"""Validate and run a manifest-bound MuJoCo world package.

The JSONL output is a simulation-adapter contract, not the production World API.
No renderer, network connection, credentials, or hardware adapter is opened.
MJCF dependencies are loaded only from the manifest-declared verified inventory.
"""

from __future__ import annotations

import argparse
import hashlib
import json
import math
import os
import stat
from pathlib import Path
import random
import re
import sys
import xml.etree.ElementTree as ET


class PackageError(ValueError):
    """The package is unsupported or invalid."""


def number(value, name, minimum=None, maximum=None):
    try:
        finite = math.isfinite(value) if isinstance(value, (int, float)) else False
    except OverflowError:
        finite = False
    if isinstance(value, bool) or not finite:
        raise PackageError(f"{name} must be a finite number")
    if minimum is not None and value < minimum or maximum is not None and value > maximum:
        raise PackageError(f"{name} is outside its allowed range")
    return value


def vector(value, name):
    if not isinstance(value, list) or len(value) != 3:
        raise PackageError(f"{name} must contain three numbers")
    return [number(v, name) for v in value]


def read_bounded(path, limit, name):
    with path.open("rb") as source:
        data = source.read(limit + 1)
    if len(data) > limit:
        raise PackageError(f"{name} exceeds {limit} bytes")
    return data


MAX_MANIFEST = 1_000_000
MAX_FILE = 8_000_000
MAX_TOTAL = 64 << 20
MAX_FILES = 256


def portable_path(value):
    if not isinstance(value, str) or not value or len(value) > 1024:
        raise PackageError("asset path escapes portable package namespace")
    parts = value.split("/")
    if len(parts) > 32:
        raise PackageError("asset path exceeds directory depth")
    for part in parts:
        if not re.fullmatch(r"[A-Za-z0-9_-][A-Za-z0-9_.-]*", part) or part.endswith(".") or len(part) > 128:
            raise PackageError("asset path escapes portable package namespace")
        stem = part.split(".")[0].upper()
        if stem in {"CON", "PRN", "AUX", "NUL"} or re.fullmatch(r"(?:COM|LPT)[1-9]", stem):
            raise PackageError("reserved asset path")
    return value


def secure_read(root, relative, limit, name):
    """Open beneath a trusted root using no-follow descriptors at every step."""
    parts = relative.split("/")
    directory = os.open(root, os.O_RDONLY | os.O_DIRECTORY)
    try:
        for part in parts[:-1]:
            next_dir = os.open(part, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW, dir_fd=directory)
            os.close(directory)
            directory = next_dir
        fd = os.open(parts[-1], os.O_RDONLY | os.O_NONBLOCK | os.O_NOFOLLOW, dir_fd=directory)
        with os.fdopen(fd, "rb") as source:
            if not stat.S_ISREG(os.fstat(source.fileno()).st_mode):
                raise PackageError(f"{name} must be a regular file")
            data = source.read(limit + 1)
        if len(data) > limit:
            raise PackageError(f"{name} exceeds {limit} bytes")
        return data
    except OSError as exc:
        raise PackageError(f"{name} unavailable or symlink/path escapes package: {exc}") from exc
    finally:
        os.close(directory)


def unique_object(pairs):
    result = {}
    for key, value in pairs:
        if key in result:
            raise PackageError(f"duplicate manifest field {key}")
        result[key] = value
    return result


def finite_float(value):
    result = float(value)
    if not math.isfinite(result):
        raise PackageError("non-finite JSON number")
    return result


def invalid_constant(value):
    raise PackageError(f"non-finite JSON constant {value}")


def parse_xml(raw):
    try:
        text = raw.decode("utf-8-sig")
    except UnicodeDecodeError as exc:
        raise PackageError("MJCF must use UTF-8") from exc
    if "\x00" in text:
        raise PackageError("MJCF must use UTF-8 without NUL bytes")
    declaration = re.match(r"\s*<\?xml\b[^?]*\?>", text)
    if declaration:
        encoding = re.search(r"encoding\s*=\s*['\"]([^'\"]+)['\"]", declaration.group())
        if encoding and encoding.group(1).lower() not in {"utf-8", "utf8"}:
            raise PackageError("MJCF must declare UTF-8")
    if "<!DOCTYPE" in text.upper() or "<!ENTITY" in text.upper():
        raise PackageError("XML document types and entities are not supported")
    root = ET.fromstring(text)
    if any(not isinstance(e.tag, str) or "}" in e.tag for e in root.iter()):
        raise PackageError("XML namespaces are not supported")
    if sum(1 for _ in root.iter()) > 100000:
        raise PackageError("MJCF element budget exceeded")
    return root


def verified_model(package, root_path, manifest_size):
    model = package["mujoco"]
    model_path = portable_path(model["path"])
    inventory = {model_path: (model["sha256"], None)}
    assets = package.get("assets", [])
    if not isinstance(assets, list) or len(assets) >= MAX_FILES:
        raise PackageError("asset inventory exceeds file count")
    for asset in assets:
        if not isinstance(asset, dict) or set(asset) != {"path", "sha256", "size"}:
            raise PackageError("invalid asset inventory entry")
        path = portable_path(asset["path"])
        if path in inventory:
            raise PackageError("duplicate asset path")
        size = asset["size"]
        if type(size) is not int or not 1 <= size <= MAX_FILE:
            raise PackageError("invalid asset size")
        inventory[path] = (asset["sha256"], size)
    names = {"world.json"}
    spellings = {}
    for path in inventory:
        components = path.split("/")
        for depth in range(1, len(components) + 1):
            prefix = "/".join(components[:depth])
            previous = spellings.setdefault(prefix.lower(), prefix)
            if previous != prefix:
                raise PackageError("ambiguous path casing")
        folded = path.lower()
        if folded in names or any(folded.startswith(n + "/") or n.startswith(folded + "/") for n in names):
            raise PackageError("ambiguous or colliding asset paths")
        names.add(folded)
    data = {}
    total = manifest_size
    for path, (digest, expected_size) in inventory.items():
        if not isinstance(digest, str) or not re.fullmatch(r"[0-9a-f]{64}", digest):
            raise PackageError("invalid asset digest")
        raw = secure_read(root_path, path, MAX_FILE, "model" if path == model_path else "asset")
        if not raw or (expected_size is not None and len(raw) != expected_size):
            raise PackageError("asset size mismatch")
        if hashlib.sha256(raw).hexdigest() != digest:
            raise PackageError("model or asset digest mismatch")
        total += len(raw)
        if total > MAX_TOTAL:
            raise PackageError("package byte budget exceeded")
        data[path] = raw
    main = parse_xml(data[model_path])
    if main.tag != "mujoco":
        raise PackageError("model must be MJCF")
    base = model_path.rpartition("/")[0]
    included = set()
    origins = {element: model_path for element in main.iter()}
    element_count = sum(1 for _ in main.iter())
    used = {model_path}
    def candidate(reference, directory):
        try:
            portable_path(reference)
            return portable_path("/".join(p for p in (directory, reference) if p))
        except PackageError as exc:
            raise PackageError(f"MJCF dependency not supported: {exc}") from exc

    def require(path):
        if path not in data:
            raise PackageError(f"undeclared MJCF dependency not supported: {path}")
        used.add(path)
        return path

    def resolve_include(reference, origin):
        primary = candidate(reference, base)
        fallback = candidate(reference, origin.rpartition("/")[0])
        return require(primary if primary in data else fallback)

    def expand(parent, stack):
        nonlocal element_count
        if len(stack) > 32:
            raise PackageError("MJCF include depth exceeded")
        for child in list(parent):
            if child.tag in {"plugin", "extension"}:
                raise PackageError("plugins and extensions are not supported")
            if child.tag == "include":
                if set(child.attrib) != {"file"} or list(child):
                    raise PackageError("invalid include element")
                name = resolve_include(child.attrib["file"], origins[child])
                if name in stack or name in included:
                    raise PackageError("cyclic or repeated MJCF include")
                included.add(name)
                document = parse_xml(data[name])
                origins.update({element: name for element in document.iter()})
                element_count += sum(1 for _ in document.iter())
                if element_count > 100000:
                    raise PackageError("expanded MJCF element budget exceeded")
                expand(document, stack + [name])
                index = list(parent).index(child)
                parent.remove(child)
                for element in list(document):
                    parent.insert(index, element)
                    index += 1
            else:
                expand(child, stack)
    expand(main, [model_path])
    # Directory settings are global and relative to the main MJCF, not includes.
    # A single declaration avoids ambiguous override order across compiler blocks.
    dirs = {}
    directory_compilers = 0
    for element in main.iter("compiler"):
        if any(key in element.attrib for key in ("assetdir", "meshdir", "texturedir")):
            directory_compilers += 1
            if directory_compilers > 1:
                raise PackageError("multiple compiler asset-directory blocks are not supported")
        if element.attrib.get("strippath", "false") != "false":
            raise PackageError("strippath is not supported")
        for key in ("assetdir", "meshdir", "texturedir"):
            if key in element.attrib:
                if key in dirs:
                    raise PackageError("repeated compiler asset directories are not supported")
                directory = element.attrib.pop(key)
                if directory.endswith("/"):
                    directory = directory[:-1]
                dirs[key] = portable_path(directory) if directory else ""
    meshdir = dirs.get("meshdir", dirs.get("assetdir", ""))
    texturedir = dirs.get("texturedir", dirs.get("assetdir", ""))
    vfs = {}
    aliases = {}
    bindings = []
    supported = {"mesh", "hfield", "skin", "texture"}
    texture_files = {"file", "fileright", "fileleft", "fileup", "filedown", "filefront", "fileback"}
    for element in main.iter():
        if element.tag in {"plugin", "extension"}:
            raise PackageError("plugins and extensions are not supported")
        for key, value in list(element.attrib.items()):
            if not key.startswith("file"):
                continue
            if element.tag not in supported or (key != "file" and not (element.tag == "texture" and key in texture_files)):
                raise PackageError("this MJCF file reference is not supported")
            directory = texturedir if element.tag == "texture" else meshdir
            # MuJoCo 3.3.7 first probes the main model directory, then
            # resolves included resources relative to their declaring XML.
            # Its parser probes assetdir for skin/hfield, while the compiler
            # loads their unresolved relative names through meshdir.
            parser_dir = dirs.get("assetdir", "") if element.tag in {"hfield", "skin"} else (directory or dirs.get("assetdir", ""))
            primary = candidate(value, "/".join(p for p in (base, parser_dir) if p))
            if primary in data or origins[element] == model_path:
                path = candidate(value, "/".join(p for p in (base, directory) if p))
            else:
                path = candidate(value, origins[element].rpartition("/")[0])
            path = require(path)
            # Preserve implicit asset names before changing their file basename.
            if key == "file" and "name" not in element.attrib:
                element.set("name", Path(value).stem)
            if path not in aliases:
                aliases[path] = "wendy_asset_" + str(len(aliases)) + Path(path).suffix
                vfs[aliases[path]] = data[path]
            alias = aliases[path]
            element.set(key, alias)
            bindings.append({"path": path, "vfs_name": alias, "kind": element.tag})
    if sum(1 for _ in main.iter()) > 100000:
        raise PackageError("expanded MJCF element budget exceeded")
    report = {"schema_version": "wendy.mujoco.import.v1", "source": "synthetic-package", "model_path": model_path,
              "verified_files": len(data), "verified_bytes": total, "included_files": sorted(included), "asset_bindings": bindings,
              "unused_declared_assets": sorted(set(data) - used),
              "preserved": ["MJCF geometry and explicit physical parameters", "joints, actuators and sensor declarations", "declared mesh, texture, height-field and skin bytes"],
              "changes": ["includes expanded in document order", "file references remapped to verified in-memory assets"],
              "not_established": ["real-world physical fidelity or survey accuracy", "hardware bindings and commissioning", "sensor simulation beyond the selected adapter", "compiler or runtime memory and CPU bounds"]}
    return ET.tostring(main, encoding="unicode"), vfs, report


def validate_generic_manifest(package):
    """v1 is an inert model inventory, independent of the v0 fixture adapter."""
    required = {"schema_version", "site_id", "package_revision", "frame_revision", "provenance",
                "geographic_anchor", "local_frame", "mujoco"}
    optional = {"assets", "description", "fidelity", "asset_license", "scenario"}
    if not isinstance(package, dict) or not required <= set(package) or set(package) - required - optional:
        raise PackageError("invalid generic package fields")

    def scalar_json(value, depth=0):
        if depth > 32:
            raise PackageError("generic package JSON depth exceeds 32")
        if isinstance(value, str):
            value.encode("utf-8", errors="strict")
        elif isinstance(value, dict):
            for key, item in value.items():
                key.encode("utf-8", errors="strict")
                scalar_json(item, depth + 1)
        elif isinstance(value, list):
            for item in value:
                scalar_json(item, depth + 1)
        elif type(value) in (int, float):
            number(value, "JSON number")
    scalar_json(package)
    for field in ("site_id", "package_revision", "frame_revision"):
        value = package[field]
        go_space = "\t\n\v\f\r \u0085\u00a0\u1680\u2000\u2001\u2002\u2003\u2004\u2005\u2006\u2007\u2008\u2009\u200a\u2028\u2029\u202f\u205f\u3000"
        if not isinstance(value, str) or not 1 <= len(value.encode("utf-8")) <= 256 or value != value.strip(go_space):
            raise PackageError(f"invalid generic package {field}")
    anchor = package["geographic_anchor"]
    if not isinstance(anchor, dict) or set(anchor) != {"latitude_deg", "longitude_deg", "altitude_m", "altitude_datum", "placement"}:
        raise PackageError("invalid generic geographic anchor fields")
    model = package["mujoco"]
    if not isinstance(model, dict) or set(model) != {"path", "sha256", "to_local_position"}:
        raise PackageError("invalid generic MuJoCo fields")


def load_model_package(path):
    """Load verified model bytes; v1 does not require an actuator or gas adapter."""
    path = Path(path).absolute()
    root_path = path.parent.resolve()
    raw = secure_read(root_path, path.name, MAX_MANIFEST, "manifest")
    try:
        package = json.loads(raw, object_pairs_hook=unique_object, parse_constant=invalid_constant, parse_float=finite_float)
        if package["schema_version"] not in {"wendy.world.package.v0", "wendy.world.package.v1"}:
            raise PackageError("unsupported package schema")
        generic = package["schema_version"] == "wendy.world.package.v1"
        if generic:
            raw.decode("utf-8", errors="strict")
            if raw.startswith(b"\xef\xbb\xbf"):
                raise PackageError("generic package JSON must not contain a BOM")
            validate_generic_manifest(package)
        for field in ("site_id", "package_revision", "frame_revision"):
            if not isinstance(package[field], str) or not package[field]:
                raise PackageError(f"{field} must be a nonempty string")
        if package["provenance"] != "synthetic":
            raise PackageError("this bootstrap accepts synthetic packages only")
        anchor = package["geographic_anchor"]
        number(anchor["latitude_deg"], "latitude", -90, 90)
        number(anchor["longitude_deg"], "longitude", -180, 180)
        number(anchor["altitude_m"], "altitude")
        if anchor["altitude_datum"] != "WGS84-ellipsoid" or anchor["placement"] != "illustrative":
            raise PackageError("anchor must explicitly identify illustrative ellipsoid placement")
        if package["local_frame"] != {"units": "metres", "handedness": "right", "up": "Y", "axes": ["east", "up", "south"]}:
            raise PackageError("unsupported local frame")
        model = package["mujoco"]
        if model["to_local_position"] != ["x", "z", "-y"]:
            raise PackageError("MuJoCo position transform must be explicit x,z,-y")
        model_xml, verified_assets, import_report = verified_model(package, root_path, len(raw))
        if not generic:
            for field in ("body", "actuator"):
                if not isinstance(model[field], str) or not model[field]:
                    raise PackageError(f"mujoco.{field} must name a model element")
            number(model["control"], "control", 0, 1)
            gas = package["gas_model"]
            if gas["kind"] != "analytic-gaussian-fixture" or gas["unit"] != "ppm":
                raise PackageError("unsupported gas model or unit")
            vector(gas["source_local_m"], "gas source")
            number(gas["background_ppm"], "gas background", 0)
            number(gas["peak_ppm"], "gas peak", 0)
            number(gas["sigma_m"], "gas sigma", 0.000001)
            number(gas["noise_stddev_ppm"], "gas noise", 0)
    except (KeyError, TypeError, json.JSONDecodeError, ET.ParseError, RecursionError, UnicodeError) as exc:
        raise PackageError(f"invalid package: {exc}") from exc
    package["_digest"] = hashlib.sha256(raw).hexdigest()
    package["_model_xml"] = model_xml
    package["_assets"] = verified_assets
    package["_import_report"] = import_report
    return package


def load_package(path):
    """The original pose/gas adapter remains explicitly v0-only."""
    package = load_model_package(path)
    if package["schema_version"] != "wendy.world.package.v0":
        raise PackageError("generic packages require the explicit package runner")
    return package


def compile_model(package, mujoco):
    """MuJoCo 4 loads assets from verified memory, never the original paths."""
    with mujoco.MjVfs() as vfs:
        for name, raw in package["_assets"].items():
            vfs[name] = raw
        return mujoco.MjModel.from_xml_string(package["_model_xml"], vfs=vfs)


def gas_value(gas, local_position, rng):
    # Scale before squaring so finite large sigmas do not overflow. An infinite
    # normalized distance has zero contribution from the Gaussian field.
    distance = math.hypot(*((a - b) / gas["sigma_m"] for a, b in zip(local_position, gas["source_local_m"])))
    concentration = gas["background_ppm"] + gas["peak_ppm"] * math.exp(-0.5 * distance * distance)
    value = concentration + rng.gauss(0, gas["noise_stddev_ppm"])
    if not math.isfinite(value):
        raise PackageError("gas model produced non-finite concentration")
    return max(0.0, value)


def observations(package, *, steps=100, every=10, seed=1, run_id="smoke"):
    if type(steps) is not int or not 1 <= steps <= 1_000_000:
        raise PackageError("steps must be between 1 and 1,000,000")
    if type(every) is not int or not 1 <= every <= steps:
        raise PackageError("every must be between 1 and steps")
    if type(seed) is not int or not 0 <= seed < 2**32:
        raise PackageError("seed must be an unsigned 32-bit integer")
    if not isinstance(run_id, str) or not re.fullmatch(r"[A-Za-z0-9_-]{1,80}", run_id):
        raise PackageError("run ID must contain 1-80 letters, digits, underscores or hyphens")
    import mujoco

    # Compile only the bytes checked above; do not reopen a mutable model path.
    model = compile_model(package, mujoco)
    data = mujoco.MjData(model)
    body = mujoco.mj_name2id(model, mujoco.mjtObj.mjOBJ_BODY, package["mujoco"]["body"])
    actuator = mujoco.mj_name2id(model, mujoco.mjtObj.mjOBJ_ACTUATOR, package["mujoco"]["actuator"])
    if body <= 0 or actuator < 0:
        raise PackageError("configured body or actuator does not exist")
    if not math.isfinite(model.opt.timestep) or model.opt.timestep <= 0:
        raise PackageError("model timestep must be positive and finite")
    rng = random.Random(seed)
    seq = 0
    for step in range(1, steps + 1):
        data.ctrl[actuator] = package["mujoco"]["control"]
        mujoco.mj_step(model, data)
        if step % every != 0 and step != steps:
            continue
        mujoco.mj_forward(model, data)
        x, y, z = map(float, data.xpos[body])
        local = [x, z, -y]
        if not all(math.isfinite(v) for v in local) or not math.isfinite(data.time):
            raise PackageError("simulation produced non-finite state")
        for kind, value in (
            ("pose", {"position_m": local}),
            ("gas", {"concentration": gas_value(package["gas_model"], local, rng), "unit": "ppm", "species": "synthetic-test-gas", "noise_stddev_ppm": package["gas_model"]["noise_stddev_ppm"], "position_m": local}),
        ):
            seq += 1
            yield {
                "schema_version": "wendy.sim.observation.v0",
                "id": f"{run_id}/{seq}",
                "site_id": package["site_id"],
                "frame_revision": package["frame_revision"],
                "kind": kind,
                "source": {"device": "sim-gateway-1", "app": "wendy-world-sim", "service": "mujoco", "stream": kind},
                "sequence": seq,
                "timing": {"clock_domain": f"simulation:{run_id}", "sample_ns": round(float(data.time) * 1_000_000_000)},
                "provenance": {"mode": "simulated", "run_id": run_id, "seed": seed, "package_revision": package["package_revision"], "package_sha256": package["_digest"], "mujoco_version": mujoco.__version__, "sensor_model": "mujoco-body-position" if kind == "pose" else "analytic-gaussian-fixture"},
                "value": value,
            }


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("package", type=Path)
    parser.add_argument("--steps", type=int, default=100)
    parser.add_argument("--every", type=int, default=10)
    parser.add_argument("--seed", type=int, default=1)
    parser.add_argument("--run-id", default="smoke")
    parser.add_argument("--validate-only", action="store_true")
    args = parser.parse_args(argv)
    try:
        package = load_model_package(args.package) if args.validate_only else load_package(args.package)
        if args.validate_only:
            print(json.dumps({"valid": True, "package_sha256": package["_digest"], "import_report": package["_import_report"], "limitations": ["synthetic package only", "structural import validation does not establish physical fidelity"]}, sort_keys=True))
        else:
            for observation in observations(package, steps=args.steps, every=args.every, seed=args.seed, run_id=args.run_id):
                print(json.dumps(observation, sort_keys=True, allow_nan=False))
        return 0
    except (OSError, ValueError, ImportError) as exc:
        print(f"simulation failed: {exc}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
