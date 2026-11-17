#!/usr/bin/env python3
"""Sequential Go source coverage for the existing Swift E2E tests.

Native evidence is immutable. Reports use Go covdata (including -pcombine),
then exclude generated files from the derived handwritten-source profile.
"""

import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import socket
import subprocess
import sys
import tempfile
import xml.etree.ElementTree as ET


def command(*args, **kwargs):
    return subprocess.check_output(args, text=True, **kwargs)


def write_json(path, value):
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps(value, indent=2, sort_keys=True) + "\n")


def source_id(repo, source_paths=()):
    # Includes dirty and new source, independent of checkout path. Never combine
    # evidence solely because two binaries claim the same Git revision.
    paths = command("git", "ls-files", "-co", "--exclude-standard", "-z", cwd=repo).split("\0")
    digest = hashlib.sha256()
    for name in sorted(set(paths) | set(source_paths)):
        if name.endswith(".go") or name in ("go.mod", "go.sum"):
            path = repo / name
            digest.update(name.encode() + b"\0" + (b"present\0" + path.read_bytes() if path.exists() else b"deleted") + b"\0")
    return digest.hexdigest()


def prepare(repo, run, managed):
    repo = repo.resolve()
    files, packages = {}, {}
    for role in (["wendy", "wendy-agent"] if managed else ["wendy"]):
        output = command("go", "list", "-deps", "-json", "-tags=e2e_coverage", f"./go/cmd/{role}", cwd=repo)
        decoder = json.JSONDecoder()
        entries = []
        while output.strip():
            pkg, end = decoder.raw_decode(output.lstrip())
            output = output.lstrip()[end:]
            if not pkg.get("Module", {}).get("Main"):
                continue
            entries.append(pkg["ImportPath"])
            for name in pkg.get("GoFiles", []) + pkg.get("CgoFiles", []):
                path = Path(pkg["Dir"]) / name
                generated = bool(re.search(r"(?m)^// Code generated .* DO NOT EDIT\.$", path.read_text().split("\npackage ", 1)[0]))
                files[pkg["ImportPath"] + "/" + name] = {
                    "path": str(path.relative_to(repo)), "generated": generated,
                    "testHarness": name == "coverage_e2e.go",
                }
        packages[role] = sorted(entries)
    write_json(run / "coverage/source.json", {
        "sourceID": source_id(repo, [f["path"] for f in files.values()]), "revision": command("git", "rev-parse", "HEAD", cwd=repo).strip(),
        "goVersion": command("go", "version").strip(), "files": files, "packages": packages,
        "scope": "Go main-module packages linked into the selected binaries on this platform; generated files excluded only from derived reports",
        "agent": "instrumented managed Go Agent" if managed else "uninstrumented/not collected",
        "counterMode": "atomic", "nonGo": "out of scope",
    })


def agent_control(path, operation, directory=""):
    with socket.socket(socket.AF_UNIX) as conn:
        conn.settimeout(30)
        conn.connect(str(path))
        conn.sendall(json.dumps({"Operation": operation, "Directory": str(directory)}).encode() + b"\n")
        with conn.makefile() as response:
            result = json.loads(response.readline())
        if result["error"]:
            raise RuntimeError("Agent coverage " + operation + ": " + result["error"])
        return result["pid"]


def profile(native_dirs, destination, source):
    for native in native_dirs:
        metadata = {path.name.removeprefix("covmeta.") for path in native.glob("covmeta.*")}
        counters = {path.name.split(".")[1] for path in native.glob("covcounters.*")}
        if not metadata or not counters:
            raise RuntimeError("Missing native Go metadata/counters: " + str(native))
        # covdata can silently ignore counters whose binary's metadata is gone,
        # shrinking a mixed CLI/Agent denominator. Require every binary's pair.
        if counters - metadata:
            raise RuntimeError("Missing native Go metadata for counters: " + str(native))
        if metadata - counters:
            raise RuntimeError("Missing native Go counters for metadata: " + str(native))
    destination.mkdir(parents=True, exist_ok=True)
    # Temporary derived data avoids overwriting native files, even on reruns.
    with tempfile.TemporaryDirectory(prefix="merge-", dir=destination) as merged:
        command("go", "tool", "covdata", "merge", "-pcombine", "-i=" + ",".join(map(str, native_dirs)), "-o=" + merged)
        text = Path(merged) / "coverage.out"
        command("go", "tool", "covdata", "textfmt", "-i=" + merged, "-o=" + str(text))
        raw = text.read_text()
    if not raw.startswith("mode: atomic\n"):
        raise RuntimeError("Expected atomic native Go coverage")
    lines, totals = ["mode: atomic"], {}
    for line in raw.splitlines()[1:]:
        location, statements, count = line.rsplit(" ", 2)
        filename = location.rsplit(":", 1)[0]
        if filename not in source["files"]:
            raise RuntimeError("Coverage source outside recorded build scope: " + filename)
        if source["files"][filename]["generated"] or source["files"][filename]["testHarness"]:
            continue
        lines.append(line)
        package = filename.rsplit("/", 1)[0]
        summary = totals.setdefault(package, {"statements": 0, "covered": 0})
        summary["statements"] += int(statements)
        summary["covered"] += int(statements) if int(count) > 0 else 0
    (destination / "coverage.out").write_text("\n".join(lines) + "\n")
    statements = sum(p["statements"] for p in totals.values())
    covered = sum(p["covered"] for p in totals.values())
    return {"statements": statements, "covered": covered,
            "percent": 100 * covered / statements if statements else None, "packages": totals}


def check_cli_counters(native):
    counters = list(native.glob("covcounters.*"))
    starts = list(native.glob("invocation.*"))
    for start in starts:
        _, pid, timestamp = start.name.split(".")
        if not any(p.name.split(".")[2] == pid and int(p.name.split(".")[3]) >= int(timestamp) for p in counters):
            raise RuntimeError("CLI invocation did not write counters: " + start.name)
    if starts and (not counters or not list(native.glob("covmeta.*"))):
        raise RuntimeError("Missing native Go counters or metadata")
    return len(starts)


def run_tests(run, filters, control):
    run = run.resolve()
    source = json.loads((run / "coverage/source.json").read_text())
    source["binaries"] = {}
    for role in source["packages"]:
        binary = run / "coverage/bin" / role
        build_info = command("go", "version", "-m", str(binary))
        if "-cover=true" not in build_info or "e2e_coverage" not in build_info:
            raise RuntimeError("Expected instrumented Go binary: " + str(binary))
        source["binaries"][role] = {"sha256": hashlib.sha256(binary.read_bytes()).hexdigest(), "buildInfo": build_info}
    write_json(run / "coverage/source.json", source)
    write_json(run / "coverage/tests.json", [])
    listed = command("swift", "test", "list")
    tests = [name for name in listed.splitlines() if "/" in name and any(re.search(f, name) for f in filters)]
    combined = ET.Element("testsuites")
    ET.ElementTree(combined).write(run / "test-results.xml", encoding="utf-8", xml_declaration=True)
    status = 0
    records = []
    write_json(run / "coverage/tests.json", records)
    for index, name in enumerate(tests):
        receipt = run / "coverage/current-test.txt"
        receipt.unlink(missing_ok=True)
        xml = run / f"coverage/test-{index}.xml"
        record = {"test": name, "sourceID": source["sourceID"], "status": "incomplete"}
        records.append(record)
        write_json(run / "coverage/tests.json", records)
        try:
            if control:
                record["agentPID"] = agent_control(control, "reset")
            env = dict(os.environ, WENDY_E2E_COVERAGE_RECEIPT=str(receipt))
            # Swift Testing's full ID can append a source-location component
            # which SwiftPM's listing omits. Match only that optional suffix.
            code = subprocess.call(["swift", "test", "--skip-build", "--no-parallel", "--filter", "^" + re.escape(name) + "(/|$)", "--xunit-output", str(xml)], env=env)
            record["testExitStatus"] = code
            status = status or code
            # Export before parsing results, including nonzero/crashed test
            # processes. The Agent is still alive and test-owned cleanup ended.
            if receipt.exists():
                test_dir = Path(receipt.read_text()).resolve()
                native = test_dir / "coverage/go"
                record["directory"] = str(test_dir.relative_to(run))
                if control:
                    if agent_control(control, "export", native) != record["agentPID"]:
                        raise RuntimeError("Agent changed between reset and export")
                    record["agentExported"] = True
                write_json(run / "coverage/tests.json", records)
            swift_xml = xml.with_name(xml.stem + "-swift-testing.xml")
            tree = ET.parse(swift_xml if swift_xml.exists() else xml)
            cases = list(tree.getroot().iter("testcase"))
            if not cases:
                raise RuntimeError("Selected test produced no xUnit cases")
            for suite in ([tree.getroot()] if tree.getroot().tag == "testsuite" else tree.getroot()):
                combined.append(suite)
            if all(case.find("skipped") is not None for case in cases):
                record["status"] = "skipped"
                continue
            if not receipt.exists():
                record["status"] = "no-data"
                record["cli"] = "not invoked through the scenario harness"
                continue
            record["cliInvocations"] = check_cli_counters(native)
            record["status"] = "collected" if record["cliInvocations"] or control else "no-data"
            record["cli"] = "collected" if record["cliInvocations"] else "not invoked"
        except Exception as error:
            record["error"] = str(error)
            print("ERROR: E2E coverage:", name, error, file=sys.stderr, flush=True)
            status = status or 1
        finally:
            write_json(run / "coverage/tests.json", records)
            ET.ElementTree(combined).write(run / "test-results.xml", encoding="utf-8", xml_declaration=True)
    return status


def report(repo, run, html=False, control=None):
    run = run.resolve()
    source = json.loads((run / "coverage/source.json").read_text())
    records = json.loads((run / "coverage/tests.json").read_text())
    errors, natives = [], []
    (run / "coverage/coverage.out").unlink(missing_ok=True)
    if (run / "timeout.json").exists():
        errors.append("Test process timed out; coverage intervals may be incomplete")
    compatible = source_id(repo, [f["path"] for f in source["files"].values()]) == source["sourceID"]
    if not compatible:
        errors.append("Source changed since coverage build; refusing to mix revisions")
    # A timeout can kill the orchestrator before it saves the current receipt.
    # Preserve best-effort evidence, but never promote that interval to complete.
    if records and records[-1]["status"] == "incomplete":
        receipt = run / "coverage/current-test.txt"
        if receipt.exists():
            try:
                test_dir = Path(receipt.read_text()).resolve()
                records[-1]["directory"] = str(test_dir.relative_to(run))
                if control and not records[-1].get("agentExported"):
                    agent_control(control, "export", test_dir / "coverage/go")
                    records[-1]["agentExported"] = True
            except Exception as error:
                errors.append(str(error))
    for record in records:
        if record["sourceID"] != source["sourceID"]:
            errors.append("Incompatible source identity: " + record["test"])
            continue
        if record["status"] in ("skipped", "no-data"):
            if "directory" in record:
                path = run / record["directory"] / "test.json"
                metadata = json.loads(path.read_text()) if path.exists() else {}
                metadata["coverage"] = dict(record, agent=source["agent"])
                write_json(path, metadata)
            continue
        if record["status"] != "collected":
            errors.append(record.get("error", "Incomplete collection: " + record["test"]))
        if "directory" not in record:
            continue
        test_dir = run / record["directory"]
        native = test_dir / "coverage/go"
        (test_dir / "coverage/coverage.out").unlink(missing_ok=True)
        summary = None
        try:
            if not compatible:
                raise RuntimeError("Incompatible coverage source")
            check_cli_counters(native)
            if "agentPID" in record and not list(native.glob(f"covcounters.*.{record['agentPID']}.*")):
                raise RuntimeError("Missing managed Agent counters: " + record["test"])
            summary = profile([native], test_dir / "coverage", source)
            natives.append(native)
        except Exception as error:
            errors.append(str(error))
            record["status"], record["error"] = "incomplete", str(error)
        metadata_path = test_dir / "test.json"
        metadata = json.loads(metadata_path.read_text()) if metadata_path.exists() else {}
        metadata["coverage"] = dict(record, summary=summary, agent=source["agent"])
        write_json(metadata_path, metadata)
    result = {"sourceID": source["sourceID"], "agent": source["agent"], "tests": records, "errors": errors}
    if natives:
        try:
            result["summary"] = profile(natives, run / "coverage", source)
            if html:
                command("go", "tool", "cover", "-html=" + str(run / "coverage/coverage.out"), "-o=" + str(run / "coverage/index.html"), cwd=repo)
        except Exception as error:
            errors.append(str(error))
    result["status"] = "incomplete" if errors else "collected" if natives else "no-data"
    if errors:
        (run / "coverage/index.html").unlink(missing_ok=True)
    write_json(run / "coverage/tests.json", records)
    write_json(run / "coverage/summary.json", result)
    info = run / "attempt.json"
    if info.exists():
        metadata = json.loads(info.read_text())
        metadata["coverage"] = {k: v for k, v in result.items() if k != "tests"}
        if errors and metadata["exitStatus"] == 0:
            metadata["exitStatus"] = 1
        write_json(info, metadata)
    for error in errors:
        print("ERROR: E2E coverage:", error, file=sys.stderr)
    return 1 if errors else 0


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("operation", choices=["prepare", "run", "report"])
    parser.add_argument("--run-dir", required=True, type=Path)
    parser.add_argument("--repo", type=Path, default=Path(__file__).resolve().parents[2])
    parser.add_argument("--managed-agent", action="store_true")
    parser.add_argument("--control")
    parser.add_argument("--html", action="store_true")
    parser.add_argument("--filter", action="append", default=[])
    args = parser.parse_args()
    args.repo = args.repo.resolve()
    args.run_dir = args.run_dir.resolve()
    try:
        if args.operation == "prepare":
            prepare(args.repo, args.run_dir, args.managed_agent)
            return 0
        if args.operation == "run":
            return run_tests(args.run_dir, args.filter, args.control)
        return report(args.repo, args.run_dir, args.html, args.control)
    except Exception as error:
        print("ERROR: E2E coverage:", error, file=sys.stderr)
        return 1


if __name__ == "__main__":
    sys.exit(main())
