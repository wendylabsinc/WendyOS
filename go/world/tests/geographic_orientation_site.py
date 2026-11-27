"""Exercise versioned placement bounds through the offline World CLI."""
import argparse
import copy
import hashlib
import json
from pathlib import Path
import subprocess
import tempfile


def run(linux_image=None):
    root = Path(__file__).resolve().parents[1]
    old_path = root / "examples/geography/office.json"
    new_path = root / "examples/geography/office-orientation-v2.json"
    old = json.loads(old_path.read_text())
    new = json.loads(new_path.read_text())
    request = json.loads((root / "examples/geography/point.json").read_text())
    with tempfile.TemporaryDirectory(prefix="wendy-orientation-") as temporary:
        folder = Path(temporary)
        binary = folder / "world"
        if not linux_image:
            subprocess.run(["go", "build", "-o", str(binary), "./cmd/world"], cwd=root, check=True)

        def command(site, query, verb="convert", accepted=True):
            (folder / "site.json").write_text(json.dumps(site))
            (folder / "request.json").write_text(json.dumps(query))
            if linux_image:
                prefix = ["docker", "run", "--rm", "--network", "none", "--read-only",
                          "--cap-drop", "ALL", "--security-opt", "no-new-privileges",
                          "--mount", f"type=bind,src={folder},dst=/inputs,readonly",
                          "--entrypoint", "/world", linux_image]
                inputs = Path("/inputs")
            else:
                prefix, inputs = [str(binary)], folder
            result = subprocess.run(prefix + ["frame", verb, "--site", str(inputs / "site.json"),
                                    "--request", str(inputs / "request.json"), "--json"],
                                    capture_output=True, text=True, cwd=root, timeout=30)
            assert (result.returncode == 0) == accepted, result.stderr
            if not accepted:
                assert not result.stdout, "refused conversion emitted a partial result"
                return
            return json.loads(result.stdout)

        legacy = command(old, request)
        current = command(new, request)
        assert legacy["position_m"] == current["position_m"]
        assert legacy["geographic"] == current["geographic"]
        assert legacy["local_position_error"] == {"known": False}
        assert legacy["geographic_position_error"] == {"known": False}
        assert current["local_position_error"]["known"] is True
        assert current["geographic_position_error"]["known"] is True
        assert current["geographic_position_error"]["max_distance_metres"] > 100

        # The input frame's unknown rotation cancels for local same-frame
        # coordinates, but geographic placement still depends on it.
        unknown = copy.deepcopy(new)
        unknown["frames"][1]["rotation_error"] = {"known": False}
        same_request = dict(request, to=request["from"])
        same = command(unknown, same_request)
        assert same["local_position_error"] == {"known": True, "max_distance_metres": request["uncertainty_metres"]}
        assert same["geographic_position_error"] == {"known": False}

        # An unknown target frame affects local coordinates, not the independent
        # source-to-root geographic output.
        target_unknown = copy.deepcopy(new)
        target_unknown["frames"][0]["rotation_error"] = {"known": False}
        separate = command(target_unknown, dict(request, to=new["frames"][0]["ref"]))
        assert separate["local_position_error"] == {"known": False}
        assert separate["geographic_position_error"]["known"] is True

        reverse_request = {"geographic": current["geographic"], "to": request["from"],
                           "at": request["at"], "uncertainty_metres": .4}
        reverse = command(new, reverse_request, "locate")
        assert reverse["geographic_position_error"] == {"known": True, "max_distance_metres": .4}
        assert reverse["local_position_error"]["max_distance_metres"] > 100
        assert max(abs(a-b) for a,b in zip(reverse["position_m"], request["position_m"])) < 1e-6

        refusals = []
        for name, mutation in [
            ("missing_root_error", lambda s: s.pop("root_basis_error")),
            ("unknown_with_number", lambda s: s.update(root_basis_error={"known": False, "max_angle_radians": 0})),
            ("known_without_number", lambda s: s.update(root_basis_error={"known": True})),
            ("negative_angle", lambda s: s.update(root_basis_error={"known": True, "max_angle_radians": -1})),
            ("unsupported_schema", lambda s: s.update(schema_version="wendy.world.georeference.v3")),
            ("v1_with_v2_fields", lambda s: s.update(schema_version="wendy.world.georeference.v1")),
        ]:
            bad = copy.deepcopy(new)
            mutation(bad)
            command(bad, request, accepted=False)
            refusals.append(name)
        return {"mode": "offline Linux container on existing Docker VM" if linux_image else "host offline CLI",
                "image": linux_image, "v1_sha256": hashlib.sha256(old_path.read_bytes()).hexdigest(),
                "v2_sha256": hashlib.sha256(new_path.read_bytes()).hexdigest(), "nominal_coordinates_unchanged": True,
                "legacy": legacy, "version2": current, "same_frame_unknown_ancestry": same,
                "unknown_target_independent_geography": separate, "geographic_input": reverse,
                "rejected": refusals,
                "limits": ["synthetic asserted bounds, not a measured office survey",
                           "no physical device or Go2 simulator action", "no signature or frame activation claim"]}


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--linux-image")
    print(json.dumps(run(parser.parse_args().linux_image), indent=2))
