#!/usr/bin/env python3
"""Rehearse full Orin seed writes from a producer-owned flashpack, without devices."""

import argparse
import hashlib
import importlib.util
import json
import os
from pathlib import Path, PurePosixPath
import re
import shutil
import signal
import subprocess
import sys
import tarfile
import tempfile
import time
import xml.etree.ElementTree as ET

ROOT = Path(__file__).resolve().parents[2]
SPEC = importlib.util.spec_from_file_location("owned_orin", Path(__file__).with_name("run-owned-media-test.py"))
GUARDIAN = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(GUARDIAN)
GIB = 1024 ** 3


def digest(path):
    value = hashlib.sha256()
    with path.open("rb") as stream:
        for chunk in iter(lambda: stream.read(4 * 1024 * 1024), b""):
            value.update(chunk)
    return value.hexdigest()


def archive_accounting(stream):
    """Read headers and the real manifest; never extract archive paths."""
    sizes = {}
    manifest = None
    layouts = {}
    with tarfile.open(fileobj=stream, mode="r|") as archive:
        for member in archive:
            name = str(PurePosixPath(member.name))
            if name.startswith("/") or ".." in PurePosixPath(name).parts:
                raise ValueError("archive contains an unsafe path")
            if member.isdir():
                continue
            if not member.isfile():
                # Production archive.ExtractTarZst also skips links/specials;
                # ResolveRecovery rejects any required payload then missing.
                continue
            if name in sizes:
                raise ValueError("archive contains a duplicate regular file")
            sizes[name] = member.size
            if name == "manifest.json":
                if member.size > 1024 * 1024:
                    raise ValueError("unexpectedly large flashpack manifest")
                manifest = json.load(archive.extractfile(member))
            elif name.endswith(".xml") and member.size <= 1024 * 1024:
                layouts[name] = archive.extractfile(member).read()
    if not manifest:
        raise ValueError("flashpack has no manifest.json")
    config = str(PurePosixPath(manifest["layout"]["config_image"]))
    config_bytes = sizes[config]
    layout_path = str(PurePosixPath(manifest["layout"]["partition_layout"]))
    layout = ET.fromstring(layouts[layout_path])
    config_partitions = [int(part.findtext("size").strip(), 0)
                         for part in layout.findall("./device/partition") if part.get("name") == "config"]
    if not config_partitions or min(config_partitions) <= 0:
        raise ValueError("cannot account for config partition readback")
    # A flashed partition can be larger than its source image. Account for the
    # complete extracted partition and ext4 metadata, not just source bytes.
    config_partition_bytes = max(config_partitions)
    expanded = sum(sizes.values())
    # Cache + dense writer payload upper bound + mutable config + six retained
    # readback copies. Workspaces hard-link all other files (production code).
    # Reserve 4 GiB for GPT/partition-edge zeroing, ext4 metadata and binaries.
    required = max(40 * GIB, 2 * expanded + 8 * max(config_bytes, config_partition_bytes) + 4 * GIB)
    return {"expanded_bytes": expanded, "config_bytes": config_bytes,
            "config_partition_bytes": config_partition_bytes,
            "regular_files": len(sizes), "required_free_bytes": required,
            "virtual_disk_bytes": 128 * GIB, "manifest_version": manifest["wendyos_version"],
            "manifest_target": manifest.get("target", {})}


def inspect_archive(archive, fixture, output):
    descriptor = json.loads(fixture.read_text())
    if not archive.is_file() or archive.stat().st_size != descriptor["size_bytes"]:
        raise ValueError("producer archive size does not match fixture")
    if digest(archive) != descriptor["sha256"]:
        raise ValueError("producer archive SHA256 does not match fixture")
    # This process runs inside the pinned guardian; a deadline drains zstd too.
    with subprocess.Popen(["zstd", "-dc", "--", str(archive)], stdout=subprocess.PIPE) as decoder:
        accounting = archive_accounting(decoder.stdout)
        decoder.stdout.close()
        if decoder.wait() != 0:
            raise ValueError("zstd failed to decode the complete producer archive")
    if accounting["manifest_version"] != descriptor["version"]:
        raise ValueError("flashpack version differs from the pinned fixture")
    accounting["archive_sha256"] = descriptor["sha256"]
    accounting["archive_bytes"] = descriptor["size_bytes"]
    output.write_text(json.dumps(accounting, indent=2) + "\n")


def verify_log(text, *, control=False):
    if "--- SKIP:" in text:
        raise ValueError("Orin rehearsal skipped")
    if control:
        if ("--- FAIL: TestOrinFullInstallSeedSim " not in text or
                not re.search(r"run 1: wrote in .*; config image", text) or
                not re.search(r"run 1 flashed: (reading wendy.conf:|the agent would not apply this seed:)", text)):
            raise ValueError("seed omission was not rejected after the actual full writer")
    elif ("--- PASS: TestOrinFullInstallSeedSim " not in text or
          re.findall(r"ORIN_SEED_VERIFIED run=(\d) ", text) != ["1", "2", "3"]):
        raise ValueError("all three full Orin writer/readback runs must pass")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--archive", type=Path, required=True)
    parser.add_argument("--fixture", type=Path, required=True)
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--work-parent", type=Path, default=Path(os.environ.get("RUNNER_TEMP", tempfile.gettempdir())))
    parser.add_argument("--agent-binary", type=Path)
    parser.add_argument("--inspect-only", action="store_true", help=argparse.SUPPRESS)
    args = parser.parse_args()
    if args.inspect_only:
        inspect_archive(args.archive, args.fixture, args.output)
        return 0
    if sys.platform != "linux":
        parser.error("the producer rehearsal requires Linux; it never uses sudo or block devices")
    for command in ("go", "zstd"):
        if not shutil.which(command):
            parser.error(f"missing {command}")
    archive, fixture = args.archive.resolve(strict=True), args.fixture.resolve(strict=True)
    args.output.mkdir(parents=True, exist_ok=False)
    output = args.output.resolve()
    work = Path(tempfile.mkdtemp(prefix="wendy-orin-seed-", dir=args.work_parent)).resolve()
    evidence = {"passed": False, "physical_boot_verified": False, "commands": [], "cleanup_errors": []}
    previous = {sig: signal.getsignal(sig) for sig in (signal.SIGINT, signal.SIGTERM)}

    def interrupted(signum, frame):
        raise InterruptedError(f"interrupted by signal {signum}")

    def run(name, command, timeout, env=None, expected=0):
        start = time.monotonic()
        print(f"Orin rehearsal: {name} (deadline {timeout}s; log {output / (name + '.log')})", flush=True)
        with (output / f"{name}.log").open("w") as log:
            code = GUARDIAN.run([str(x) for x in command], timeout, cwd=ROOT, env=env, stdout=log)
        evidence["commands"].append({"name": name, "exit_code": code, "seconds": time.monotonic() - start})
        print(f"Orin rehearsal: {name} exited {code} after {time.monotonic() - start:.1f}s", flush=True)
        if code != expected:
            raise RuntimeError(f"{name} returned {code}, expected {expected}; see {output / (name + '.log')}")

    try:
        for sig in previous:
            signal.signal(sig, interrupted)
        if shutil.disk_usage(work).free < 40 * GIB:
            raise RuntimeError("full Orin rehearsal requires at least 40 GiB free before archive inspection/builds")
        # Full hash and streamed headers precede any expanded payload or build.
        run("archive-inspection", [sys.executable, __file__, "--inspect-only", "--archive", archive,
                                  "--fixture", fixture, "--output", output / "allocation.json"], 900)
        allocation = json.loads((output / "allocation.json").read_text())
        # A cross-filesystem source requires a private archive copy as well.
        required = allocation["required_free_bytes"] + (archive.stat().st_size if archive.stat().st_dev != work.stat().st_dev else 0)
        evidence["allocation"] = allocation | {"required_with_archive_copy_bytes": required,
                                              "free_before_build_bytes": shutil.disk_usage(work).free}
        if shutil.disk_usage(work).free < required:
            raise RuntimeError(f"complete flashpack needs {required / GIB:.1f} GiB free on the private work filesystem")
        shutil.copyfile(fixture, output / "fixture.json")
        run("source-commit", ["git", "rev-parse", "HEAD"], 10)
        evidence["source_commit"] = (output / "source-commit.log").read_text().strip()
        (work / "tmp").mkdir()
        env = dict(os.environ, TMPDIR=str(work / "tmp"), XDG_CACHE_HOME=str(work / "cache"),
                   WENDY_ORIN_SEED_SIM_FIXTURE=str(fixture), WENDY_ANALYTICS="false")
        agent = args.agent_binary.resolve(strict=True) if args.agent_binary else work / "wendy-agent"
        if not args.agent_binary:
            run("build-arm64-agent", ["go", "build", "-o", agent, "./go/cmd/wendy-agent"], 600,
                dict(env, GOOS="linux", GOARCH="arm64", CGO_ENABLED="0"))
        env["WENDY_ORIN_SEED_SIM_AGENT"] = str(agent)
        evidence["agent_sha256"] = digest(agent)
        source = ROOT / "go/internal/cli/commands/os_provision.go"
        before = 'if err := t.WriteFile("wendy.conf", conf, 0o644); err != nil {'
        original = source.read_text()
        evidence["seed_writer_source_sha256"] = digest(source)
        if original.count(before) != 1:
            raise ValueError("seed omission control no longer identifies one production write")
        replacement = work / "os_provision.go"
        replacement.write_text(original.replace(before, 'if err := func() error { _ = conf; return nil }(); err != nil {'))
        overlay = work / "omit-seed.json"
        overlay.write_text(json.dumps({"Replace": {str(source): str(replacement)}}))
        for name in ("normal", "omit-seed"):
            command = ["go", "test", "-c", "-o", work / f"{name}.test"]
            if name == "omit-seed":
                command += ["-overlay", overlay]
            run(f"compile-{name}", command + ["./go/internal/cli/commands"], 600, env)
        for name, runs, timeout in (("normal", "3", 1800), ("omit-seed", "1", 900)):
            if name == "normal":
                if shutil.disk_usage(work).free < required:
                    raise RuntimeError("free capacity fell below the complete rehearsal requirement during compilation")
                env["WENDY_ORIN_SEED_SIM_ARCHIVE"] = str(archive)
            else:
                env.pop("WENDY_ORIN_SEED_SIM_ARCHIVE", None)  # reuse our verified extracted cache
            target = work / name
            run(name, [work / f"{name}.test", "-test.run=^TestOrinFullInstallSeedSim$", "-test.v", "-test.count=1", "-test.timeout=0"],
                timeout, dict(env, WENDY_ORIN_SEED_SIM_DIR=str(target), WENDY_ORIN_SEED_SIM_RUNS=runs),
                expected=1 if name == "omit-seed" else 0)
            verify_log((output / f"{name}.log").read_text(), control=name == "omit-seed")
            evidence[name] = {"runs": int(runs), "files": [
                {"name": p.name, "apparent_bytes": p.stat().st_size, "allocated_bytes": p.stat().st_blocks * 512}
                for p in sorted(target.iterdir()) if p.is_file()]}
            # Never retain two full targets. Evidence keeps sizes/logs, not images.
            shutil.rmtree(target)
        evidence["passed"] = True
    except Exception as error:
        evidence["error"] = str(error)
        print(error, file=sys.stderr)
    finally:
        for sig in previous:
            signal.signal(sig, signal.SIG_IGN)
        try:
            shutil.rmtree(work)
        except Exception as error:
            evidence["cleanup_errors"].append(str(error))
            evidence["passed"] = False
        try:
            (output / "evidence.json").write_text(json.dumps(evidence, indent=2) + "\n")
        finally:
            for sig, handler in previous.items():
                signal.signal(sig, handler)
    return 0 if evidence["passed"] else 1


if __name__ == "__main__":
    sys.exit(main())
