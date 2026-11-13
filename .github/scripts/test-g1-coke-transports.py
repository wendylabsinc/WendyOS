#!/usr/bin/env python3
"""Require actual Coke HTTP/thread behavior and discriminating source controls."""
import argparse
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import shutil
import sys

ROOT = Path(__file__).resolve().parents[2]
EXAMPLE = ROOT / "Examples/G1CokeDemo"
SPEC = importlib.util.spec_from_file_location("owned_coke_process", ROOT / ".github/scripts/run-owned-media-test.py")
GUARDIAN = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(GUARDIAN)
CONTROLS = {
    "physical-wildcard": ("physical_io/service.py", 'host: str = "127.0.0.1", port: int = PORT', 'host: str = "0.0.0.0", port: int = PORT', "CONTROL_BOUND_LOOPBACK"),
    "fsm-wildcard": ("physical_io/fsm_bridge.py", 'host: str = "127.0.0.1", port: int = PORT', 'host: str = "0.0.0.0", port: int = PORT', "CONTROL_BOUND_LOOPBACK"),
    "post-replay": ("runtime/inference_client.py", 'if attempt or method != "GET":', 'if attempt:', "POST_NO_AUTOMATIC_REPLAY"),
    "deactivation-forgets-session": ("runtime/inference_client.py", '            self.client.request("POST", "/deactivate", {"session_id": self.client.session_id})\n            self.client.session_id = None', '            session_id = self.client.session_id\n            self.client.session_id = None\n            self.client.request("POST", "/deactivate", {"session_id": session_id})', "DEACTIVATE_RETAINS_IDENTITY"),
    "stream-keeps-old-generation": ("runtime/async_vision.py", '                if frame.stream_id != old_stream:', '                if False:  # restore pre-441ab9ce1 stream retention', "STREAM_CHANGE_INVALIDATES_CACHED"),
    "daemon-handler-shutdown": ("coke_demo/http_server.py", '    daemon_threads = False', '    daemon_threads = True', "HTTP_DRAIN_WAITS_FOR_HANDLER"),
}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()
    output = args.output.resolve()
    output.mkdir(mode=0o700, parents=True, exist_ok=False)
    evidence = {"audit": "2022", "status": "running", "python": sys.version, "variants": []}
    try:
        for variant in ("normal", *CONTROLS):
            target = output / variant
            target.mkdir()
            # Copy code only. No model, simulator, image, robot SDK or user state.
            for package in ("physical_io", "runtime", "coke_demo"):
                for source in (EXAMPLE / package).rglob("*.py"):
                    if source.is_symlink():
                        raise RuntimeError("refuse symlinked source")
                    destination = target / source.relative_to(EXAMPLE)
                    destination.parent.mkdir(parents=True, exist_ok=True)
                    shutil.copyfile(source, destination)
            test = target / "test_transport_boundaries.py"
            shutil.copyfile(EXAMPLE / "tests/test_transport_boundaries.py", test)
            record = {"variant": variant, "source_sha256": {str(path.relative_to(target)): hashlib.sha256(path.read_bytes()).hexdigest() for path in sorted(target.rglob("*.py"))}}
            if variant != "normal":
                name, old, new, marker = CONTROLS[variant]
                source = target / name
                text = source.read_text()
                if text.count(old) != 1:
                    raise RuntimeError(f"{variant}: production source changed; review mutation")
                text = text.replace(old, new)
                if variant.endswith("wildcard"):
                    # The original change added both default-loopback and admission.
                    if text.count("    require_loopback(host)\n") != 1:
                        raise RuntimeError("listener admission source changed")
                    text = text.replace("    require_loopback(host)\n", "", 1)
                source.write_text(text)
                record["mutation"] = {"path": name, "before": old, "after": new, "sha256": hashlib.sha256(source.read_bytes()).hexdigest()}
            environment = os.environ.copy()
            environment["PYTHONPATH"] = str(target)
            environment["PYTHONDONTWRITEBYTECODE"] = "1"
            with (output / f"{variant}.log").open("w") as log:
                result = GUARDIAN.run([sys.executable, str(test)], 30, env=environment, stdout=log)
            log = (output / f"{variant}.log").read_text()
            record["exit_code"] = result
            evidence["variants"].append(record)
            if "Ran 5 tests" not in log or "skipped" in log.lower():
                raise RuntimeError(f"{variant}: missing required non-skipped execution")
            if variant == "normal":
                if result != 0 or "\nOK\n" not in log:
                    raise RuntimeError("normal HTTP/thread journey failed")
            elif result != 1 or marker not in log or "FAIL:" not in log or "\nERROR:" in log:
                raise RuntimeError(f"{variant}: expected named assertion failure, never timeout/import failure")
        evidence["status"] = "passed"
        print("COKE_TRANSPORT_BOUNDARIES_VERIFIED: normal and six source controls")
    except BaseException as error:
        evidence["status"] = "failed"
        evidence["error"] = f"{type(error).__name__}: {error}"
        raise
    finally:
        (output / "evidence.json").write_text(json.dumps(evidence, indent=2) + "\n")


if __name__ == "__main__":
    main()
