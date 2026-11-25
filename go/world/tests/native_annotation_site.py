"""Exercise a native annotation outbox through reader loss and replica restart."""
import argparse
import hashlib
import re
import stat
import json
import os
from pathlib import Path
import subprocess
import tempfile
import time

from native_client_site import NativeSite
from virtual_site import ROOT, command



def without_age(component):
    copy = json.loads(json.dumps(component))
    copy["timing"]["age_hint_ns"] = 0
    return copy


def bounded_public_state(path, limit):
    # Only explicit node-N.state/proofs paths call this helper. Configs and
    # credentials are never read into the report.
    fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
    with os.fdopen(fd, "rb") as source:
        assert stat.S_ISREG(os.fstat(source.fileno()).st_mode)
        raw = source.read(limit + 1)
    assert len(raw) <= limit
    return raw


def durable_note(site, node, expected):
    assert node in (1, 2, 3)
    raw = bounded_public_state(site.folder / f"node-{node}.state", 64 << 20)
    prefix = re.match(rb'^\{"sha256":"([0-9a-f]{64})","state":', raw)
    assert prefix and raw.endswith(b"}")
    state_bytes = raw[prefix.end():-1]
    assert hashlib.sha256(state_bytes).hexdigest() == prefix[1].decode()
    state = json.loads(state_bytes)
    assert state["version"] == 1
    world = state["world"]
    assert world["tenant"] == "lab" and world["site"] == "synthetic-office"
    assert world["clock_domain"] == "simulation:" + site.run_id
    entries = [entry for entry in world["entries"] if entry["component"]["key"] == expected["key"]]
    assert len(entries) == 1 and without_age(entries[0]["component"]) == without_age(expected)
    proofs_raw = bounded_public_state(site.folder / f"node-{node}.proofs", 16 << 20)
    proofs = json.loads(proofs_raw)
    assert proofs["version"] in (1, 2)
    matching = [proof for proof in proofs["records"]
                if without_age(proof["component"]) == without_age(expected)]
    assert len(matching) == 1
    # Proofs contain public grants/signatures, never private signing material.
    assert set(matching[0]) == {"version", "grant", "component", "signed_at_unix_ms", "expires_at_unix_ms", "signature"}
    proof = matching[0]
    proof["component"] = without_age(proof["component"])
    return {"expires_at": entries[0]["expires_at"], "proof": proof,
            "captured_at": world["captured_at"],
            "state_file_sha256": hashlib.sha256(raw).hexdigest(),
            "proof_file_sha256": hashlib.sha256(proofs_raw).hexdigest()}


def run(node_image, scene_image):
    command(["swift", "build", "--package-path", "../macos/WorldClient", "--product", "WorldClientProbe"])
    with tempfile.TemporaryDirectory(prefix="wendy-native-annotations-") as parent:
        site = NativeSite(Path(parent) / "site", node_image, scene_image)
        try:
            info = site.setup()
            environment = os.environ.copy()
            environment["WORLD_PROFILE_PATH"] = info["contributor_profiles"]["apple"]
            environment["WORLD_CACHE_DIR"] = str(site.folder / "annotation-cache")

            def probe(mode, cache=None):
                environment["WORLD_PROBE_MODE"] = mode
                environment["WORLD_CACHE_DIR"] = str(cache or site.folder / "annotation-cache")
                result = subprocess.run([str(ROOT / "../macos/WorldClient/.build/debug/WorldClientProbe")],
                    cwd=ROOT, env=environment, capture_output=True, timeout=45)
                if result.returncode:
                    raise RuntimeError(result.stderr.decode()[-1500:])
                return json.loads(result.stdout)

            owner = f"native-node-3-{site.suffix}"
            port_before = command(["docker", "port", owner, "8080/tcp"])
            # Bootstrap while connected; the next invocation is a different
            # native process and must restore the same-boot monotonic anchor.
            clock = probe("annotation-clock")
            command(["docker", "stop", owner])
            authoring_call_start_ms = time.time_ns() // 1_000_000
            draft = probe("annotation-draft")
            authoring_call_end_ms = time.time_ns() // 1_000_000
            # Each invocation is a fresh native process. The second one must
            # recover the original note, including its absolute expiry.
            restored = probe("annotation-status")
            assert draft["pending"] == 1 and draft["published"] == 0
            assert restored["entries"] == draft["entries"]
            authored = draft["entries"][0]
            assert authored["schema_version"] == 2 and authored["clock"]
            frozen_interval = authored["interval"]
            # This compares the declared gateway-relative interval with this
            # fixture host's call bracket. It is not independent UTC calibration.
            assert frozen_interval["lowerUnixMS"] <= authoring_call_end_ms
            assert frozen_interval["upperUnixMS"] >= authoring_call_start_ms
            untimed_cache = site.folder / "untimed-cache"
            untimed = probe("annotation-untimed", untimed_cache)
            assert untimed["entries"][0]["state"] == "untimed"
            assert untimed["entries"][0]["authored_at_unix_ms"] == 0
            command(["docker", "start", owner])
            assert command(["docker", "port", owner, "8080/tcp"]) == port_before
            published = site.until(lambda: probe("annotation-sync"), lambda value: value["published"] == 1 and value.get("error") is None and value.get("clock") is not None)
            repeated = site.until(lambda: probe("annotation-sync"), lambda value: value.get("error") is None and value.get("clock") is not None)
            assert repeated["published"] == 1
            assert repeated["entries"] == published["entries"]
            untimed_after = site.until(lambda: probe("annotation-sync", untimed_cache),
                lambda value: value.get("error") is None and value.get("clock") is not None, timeout=10)
            assert untimed_after["entries"] == untimed["entries"]
            assert untimed_after["published"] == 0

            def notes(node=1):
                snapshot = site.cli("world-query", "--credentials", f"/lab/client-{node}.json")
                return [c for c in snapshot["components"] if c["key"]["type"] == "wendy.annotation.note.v2"]

            replicated = site.until(notes, lambda values: len(values) == 1)
            note = replicated[0]
            assert note["provenance"]["origin"] == "authored"
            assert note["provenance"]["simulation"]["run_id"] == site.run_id
            assert note["key"]["source"]["device"] == "sim-contributor-apple"
            for field in ("id", "text", "authored_at_unix_ms", "expires_at_unix_ms"):
                assert note["payload"][field] == authored[field]
            assert note["payload"]["clock"] == authored["clock"]
            observation = note["payload"]["clock"]["observation"]["observation"]
            assert observation["reference"] == "simulation-gateway-relative-v1"
            assert observation["run_id"] == site.run_id
            replicas = {}
            for node in (1, 2, 3):
                values = site.until(lambda node=node: notes(node),
                    lambda values: len(values) == 1 and without_age(values[0]) == without_age(note))
                replicas[str(node)] = {"component": values[0], "durable": durable_note(site, node, note)}
            assert all(value["durable"]["proof"] == replicas["1"]["durable"]["proof"]
                       for value in replicas.values())
            # Isolate the retained reader from every replica that could repair
            # it, then restart its process and require the exact same record.
            command(["docker", "stop", owner, f"native-node-2-{site.suffix}"])
            reader = f"native-node-1-{site.suffix}"
            isolated_before = durable_note(site, 1, note)
            command(["docker", "kill", "--signal", "KILL", reader])
            command(["docker", "start", reader])
            recovered = site.until(notes, lambda values: len(values) == 1 and without_age(values[0]) == without_age(note))
            isolated_after = durable_note(site, 1, note)
            assert isolated_after["expires_at"] == isolated_before["expires_at"]
            assert isolated_after["proof"] == isolated_before["proof"]
            return {"mode": "Swift native annotation outbox against three Linux replicas and live MuJoCo",
                "run_id": site.run_id, "node_image": info["node_image"], "scene_image": info["scene_image"],
                "clock_bootstrap": clock, "draft": draft, "restored_in_new_process": restored,
                "host_authoring_call_bracket_ms": [authoring_call_start_ms, authoring_call_end_ms],
                "untimed_before_reconnect": untimed, "untimed_after_reconnect": untimed_after,
                "published": published, "repeated_sync": repeated,
                "replicated_note": note, "all_three_replicas": replicas,
                "all_three_exact_components_ignoring_unsigned_age": True,
                "all_three_exact_origin_proofs_ignoring_unsigned_age": True,
                "isolated_before_restart": isolated_before, "isolated_after_restart": isolated_after,
                "isolated_reader_recovered_exact_record": without_age(recovered[0]) == without_age(note),
                "isolated_reader_retained_exact_expiry_cap_and_proof": True,
                "limits": ["authored notes in simulation; no measured sensor or motion authority",
                    "signed simulation-gateway relative time, not calibrated UTC; drift policy assumes 100 ppm",
                    "SDK workflow; native application UI requires separate runtime verification"]}
        finally:
            site.cleanup()


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--node-image", default="wendy-world-node:local")
    parser.add_argument("--scene-image", default="wendy-world-live:local")
    args = parser.parse_args()
    print(json.dumps(run(args.node_image, args.scene_image), indent=2))
