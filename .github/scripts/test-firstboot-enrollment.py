#!/usr/bin/env python3
"""Run the real first-boot ACME transport sequence and its no-wait control."""
import argparse
import importlib.util
import json
from pathlib import Path
import tempfile

ROOT = Path(__file__).resolve().parents[2]
SPEC = importlib.util.spec_from_file_location("firstboot_guardian", ROOT / ".github/scripts/run-owned-media-test.py")
GUARDIAN = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(GUARDIAN)
TEST = "TestFirstBootDelayedTransport"


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()
    out = args.output.resolve()
    out.mkdir(parents=True, exist_ok=False)
    source = ROOT / "go/internal/agent/services/provisioning_acme_file.go"
    needle = "waitForFirstBootACME(ctx, bake.DirectoryURL, svc.logger)"
    original = source.read_text()
    if original.count(needle) != 1:
        raise SystemExit("production first-boot wait changed; review control")
    command = ["go", "test", "-gcflags=all=-c=1", "-vet=off", "-race", "./go/internal/agent/services", "-run", "^" + TEST + "$", "-count=1", "-timeout=90s", "-v"]
    results = {}
    with tempfile.TemporaryDirectory(prefix="wendy-firstboot-control-") as tmp:
        mutation = Path(tmp) / source.name
        mutation.write_text(original.replace(needle, "func() error { return nil }()"))
        overlay = Path(tmp) / "overlay.json"
        overlay.write_text(json.dumps({"Replace": {str(source): str(mutation)}}))
        for name, argv in (("normal", command), ("no-wait-control", command[:2] + ["-overlay", str(overlay)] + command[2:])):
            log = out / (name + ".log")
            with log.open("x") as stream:
                code = GUARDIAN.run(argv, 600, cwd=ROOT, stdout=stream)
            text = log.read_text()
            if name == "normal":
                valid = code == 0 and "--- PASS: " + TEST in text and "--- SKIP:" not in text
            else:
                valid = code != 0 and "--- FAIL: " + TEST in text and "offline boot created acme-first-boot-attempt.json" in text
            results[name] = {"exit_code": code, "expected_result_observed": valid, "log": str(log)}
            (out / "evidence.json").write_text(json.dumps({"audit_entry":"2805", "scope":"real loopback ACME and gRPC; no NetworkManager or physical boot", "results":results}, indent=2) + "\n")
            if not valid:
                raise SystemExit(f"{name} failed acceptance; inspect {log}")
    print("first-boot transport sequence and no-wait control passed")


if __name__ == "__main__":
    main()
