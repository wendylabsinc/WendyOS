#!/usr/bin/env python3
"""Prepare release-package upgrade fixtures; verify them in a disposable systemd guest.

No commands here upload packages or install anything on the preparation host.
Guest mode deliberately requires systemd-nspawn and a bundle-specific nonce.
"""
import argparse
import hashlib
import io
import json
import os
from pathlib import Path
import platform
import re
import shutil
import struct
import subprocess
import sys
import tarfile
import tempfile
import time
import uuid

HISTORY = "68266f6b65251a7bab59ff66a4354f37636a6b7f"
HOOK_HASHES = {
    "postinstall.sh": "bd978501903482323a587c4bad5f2deeb0396b2b4268063174a22ffca226e55e",
    "preremove.sh": "931b187052a41b2970b3f96fca825fbfbcfae0e2d0eacdc645c702d1983814e2",
}
LEGACY_VERSION = "0.0.0"
SOCKET = "/var/lib/wendy/agent-control/agent.sock"
SERVICE = "wendy-agent.service"


def require(condition, message):
    if not condition:
        raise RuntimeError(message)


def run(*args, check=True, timeout=120, env=None):
    result = subprocess.run(args, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                            timeout=timeout, env=env)
    if check and result.returncode:
        raise RuntimeError(f"{args!r}: exit {result.returncode}\n"
                           + result.stdout.decode(errors="replace")[-12000:]
                           + result.stderr.decode(errors="replace")[-12000:])
    return result


def output(*args):
    return run(*args).stdout.decode().strip()


def sha(data):
    return hashlib.sha256(data).hexdigest()


def metadata(package, fmt):
    if fmt == "deb":
        fields = [output("dpkg-deb", "-f", str(package), field)
                  for field in ("Package", "Version", "Architecture")]
    else:
        fields = output("rpm", "-qp", "--qf", "%{NAME}\n%{VERSION}-%{RELEASE}\n%{ARCH}", str(package)).splitlines()
    require(len(fields) == 3, "unexpected package metadata")
    return dict(zip(("name", "version", "architecture"), fields))


def member_bytes(package, fmt, member):
    if fmt == "deb":
        archive = run("dpkg-deb", "--fsys-tarfile", str(package)).stdout
        with tarfile.open(fileobj=io.BytesIO(archive)) as source:
            matches = [item for item in source.getmembers()
                       if item.name.lstrip("./") == member.lstrip("/")]
            require(len(matches) == 1 and matches[0].isfile(), f"missing regular payload file {member}")
            return source.extractfile(matches[0]).read()
    # rpm2cpio only converts bytes; bsdtar extracts the named member to stdout.
    archive = run("rpm2cpio", str(package)).stdout
    result = subprocess.run(["bsdtar", "-xOf", "-", "." + member], input=archive,
                            stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=120, check=True)
    return result.stdout


def elf_arch(data):
    require(len(data) >= 20 and data[:4] == b"\x7fELF" and data[4:6] == b"\x02\x01",
            "package executable must be a 64-bit little-endian ELF")
    machine = struct.unpack_from("<H", data, 18)[0]
    require(machine in (62, 183), f"unsupported ELF machine {machine}")
    return {62: "amd64", 183: "arm64"}[machine]


def deb_repack(candidate, destination, hooks, *, legacy):
    with tempfile.TemporaryDirectory(prefix="wendy-deb-repack-") as temporary:
        root = Path(temporary) / "package"
        run("dpkg-deb", "-R", str(candidate), str(root))
        control = root / "DEBIAN"
        if legacy:
            text = (control / "control").read_text()
            text, count = re.subn(r"(?m)^Version: .+$", f"Version: {LEGACY_VERSION}", text)
            require(count == 1, "expected one DEB version")
            (control / "control").write_text(text)
            shutil.copyfile(hooks / "postinstall.sh", control / "postinst")
            shutil.copyfile(hooks / "preremove.sh", control / "prerm")
        else:
            text = (control / "postinst").read_text()
            needle = "systemctl restart wendy-agent"
            require(text.count(needle) == 1, "candidate DEB does not contain the expected restart fix")
            (control / "postinst").write_text(text.replace(needle, "systemctl try-restart wendy-agent"))
        for hook in ("postinst", "prerm"):
            (control / hook).chmod(0o755)
        run("dpkg-deb", "--root-owner-group", "-b", str(root), str(destination))


def rpm_repack(candidate, destination, hooks, *, legacy):
    with tempfile.TemporaryDirectory(prefix="wendy-rpm-repack-") as temporary:
        noop = Path(temporary) / "posttrans.sh"
        noop.write_text("#!/bin/sh\n# Deliberately omit PR2352 recovery.\nexit 0\n")
        command = ["fpm", "-s", "rpm", "-t", "rpm", "--rpm-rpmbuild-define", "_rpmformat 4",
                   "--rpm-posttrans", str(noop), "-p", str(destination)]
        if legacy:
            command += ["-v", LEGACY_VERSION, "--iteration", "1",
                        "--after-install", str(hooks / "postinstall.sh"),
                        "--before-remove", str(hooks / "preremove.sh")]
        run(*command, str(candidate), timeout=300)
        posttrans = output("rpm", "-qp", "--qf", "%{POSTTRANS}", str(destination))
        require("systemctl" not in posttrans and "exit 0" in posttrans,
                "FPM did not replace the imported RPM posttrans")
        if legacy:
            require(output("rpm", "-qp", "--qf", "%{VERSION}", str(destination)) == LEGACY_VERSION,
                    "FPM did not replace the imported RPM version")
            for tag, hook in (("POSTIN", "postinstall.sh"), ("PREUN", "preremove.sh")):
                require((hooks / hook).read_text().strip() in output("rpm", "-qp", "--qf", "%{" + tag + "}", str(destination)),
                        f"FPM did not preserve historical {hook}")
        else:
            for tag in ("POSTIN", "PREUN"):
                require(output("rpm", "-qp", "--qf", "%{" + tag + "}", str(candidate)) ==
                        output("rpm", "-qp", "--qf", "%{" + tag + "}", str(destination)),
                        f"negative control unexpectedly changed {tag}")


def prepare(args):
    fmt = args.format
    for tool in (("dpkg-deb",) if fmt == "deb" else ("fpm", "rpm", "rpm2cpio", "bsdtar", "cpio")):
        require(shutil.which(tool) is not None, f"preparation requires {tool}")
    source = {"candidate": Path(args.agent).resolve(), "cli": Path(args.cli).resolve()}
    for role, package in source.items():
        require(package.is_file(), f"missing {role} package")
        require(metadata(package, fmt)["name"] == ("wendy-agent" if role == "candidate" else "wendy"),
                f"wrong {role} package")
    hooks = Path(__file__).parent / "fixtures/package-upgrade-2352"
    for name, expected in HOOK_HASHES.items():
        require(sha((hooks / name).read_bytes()) == expected, "historical script fixture changed")
    bundle = Path(args.output).resolve()
    require(not bundle.exists(), "bundle output must be a new directory")
    bundle.mkdir(parents=True)
    packages = {}
    for role, package in source.items():
        dest = bundle / f"{role}.{fmt}"
        shutil.copyfile(package, dest)
        packages[role] = dest
    for role in ("legacy", "negative-control"):
        dest = bundle / f"{role}.{fmt}"
        (deb_repack if fmt == "deb" else rpm_repack)(packages["candidate"], dest, hooks, legacy=role == "legacy")
        packages[role] = dest
    records = {}
    for role, package in packages.items():
        binary = member_bytes(package, fmt, "/usr/bin/wendy" if role == "cli" else "/usr/bin/wendy-agent")
        records[role] = {**metadata(package, fmt), "file": package.name, "sha256": sha(package.read_bytes()),
                         "binary_sha256": sha(binary), "elf_architecture": elf_arch(binary)}
        if fmt == "rpm":
            require(package.read_bytes()[:6].hex() == "edabeedb0300", f"{role} is not an RPMv4 lead")
    expected_arch = records["candidate"]["elf_architecture"]
    for role, record in records.items():
        require(record["elf_architecture"] == expected_arch, f"mixed executable architectures: {role}")
        require(record["architecture"] == {"deb": {"amd64": "amd64", "arm64": "arm64"},
                                           "rpm": {"amd64": "x86_64", "arm64": "aarch64"}}[fmt][expected_arch],
                f"package metadata disagrees with ELF architecture: {role}")
        if role != "cli":
            require(record["binary_sha256"] == records["candidate"]["binary_sha256"], "fixture changed Agent payload")
    require(records["negative-control"]["version"] == records["candidate"]["version"], "control version changed")
    require(records["legacy"]["version"] != records["candidate"]["version"], "candidate must be newer than the legacy fixture")
    for member in ("/usr/lib/systemd/system/wendy-agent.service", "/usr/lib/wendy-agent/setup-mdns.sh",
                   "/etc/default/wendy-agent", "/etc/wendy-agent/config.json"):
        expected = member_bytes(packages["candidate"], fmt, member)
        for role in ("legacy", "negative-control"):
            require(member_bytes(packages[role], fmt, member) == expected, f"fixture changed {member}")
    manifest = {"schema": 1, "nonce": uuid.uuid4().hex, "format": fmt, "packages": records,
                "expected_binary_version": args.expected_version, "historical_scripts_commit": HISTORY,
                "baseline_note": "Current Agent payload with exact historical upgrade hooks; not a historical Agent binary.",
                "artifact_registry_acceptance": "pending; this local transaction does not test GCP upload"}
    (bundle / "manifest.json").write_text(json.dumps(manifest, indent=2) + "\n")
    shutil.copyfile(__file__, bundle / Path(__file__).name)
    print(json.dumps(manifest, indent=2))


def load_bundle(bundle):
    manifest = json.loads((bundle / "manifest.json").read_text())
    require(manifest["schema"] == 1 and manifest["format"] in ("deb", "rpm"), "invalid bundle schema")
    require(re.fullmatch(r"[0-9a-f]{32}", manifest["nonce"]) is not None, "invalid bundle nonce")
    require(set(manifest["packages"]) == {"candidate", "legacy", "negative-control", "cli"}, "invalid package roles")
    for record in manifest["packages"].values():
        path = bundle / record["file"]
        require(path.parent == bundle and not path.is_symlink() and path.is_file(), "package path escapes bundle")
        require(sha(path.read_bytes()) == record["sha256"], "package checksum mismatch")
    return manifest


def guest_guard(manifest, nonce):
    require(sys.platform == "linux" and os.geteuid() == 0, "guest verification requires Linux root")
    require(Path("/proc/1/comm").read_text().strip() == "systemd", "guest PID 1 must be systemd")
    require(Path("/run/systemd/container").read_text().strip() == "systemd-nspawn", "refusing non-nspawn host")
    require(nonce == manifest["nonce"] and os.environ.get("WENDY_PACKAGE_GUEST_NONCE") == nonce,
            "guest nonce must match the explicitly provisioned disposable guest")
    native = {"x86_64": "amd64", "aarch64": "arm64"}.get(platform.machine())
    require(native == manifest["packages"]["candidate"]["elf_architecture"], "native architecture required; no emulation")


def service_state():
    fields = output("systemctl", "show", SERVICE, "--property=ActiveState,SubState,MainPID,ExecMainStartTimestampMonotonic")
    return dict(line.split("=", 1) for line in fields.splitlines() if "=" in line)


def installed(fmt, name):
    if fmt == "deb":
        result = run("dpkg-query", "-W", "-f=${db:Status-Status}\n${Version}", name, check=False)
        fields = result.stdout.decode().splitlines()
        return fields[1] if result.returncode == 0 and len(fields) == 2 and fields[0] == "installed" else None
    result = run("rpm", "-q", "--qf", "%{VERSION}-%{RELEASE}", name, check=False)
    return result.stdout.decode().strip() if result.returncode == 0 else None


def transaction(fmt, action, paths, log):
    if fmt == "deb":
        command = ["apt-get", "-y", "-o", "Dpkg::Options::=--force-confold", "--no-install-recommends", action, *paths]
    else:
        command = ["dnf", "-y", "--setopt=install_weak_deps=False", "--setopt=gpgcheck=False",
                   "--setopt=localpkg_gpgcheck=False", action, *paths]
    result = run(*command, check=False, timeout=300, env={**os.environ, "DEBIAN_FRONTEND": "noninteractive"})
    log.write_text(result.stdout.decode(errors="replace") + result.stderr.decode(errors="replace"))
    require(result.returncode == 0, f"package transaction failed ({result.returncode}); see {log}")
    # rpm scriptlet failures may be warnings despite a zero DNF exit status.
    require(not re.search(r"scriptlet.*failed|error:.*scriptlet", log.read_text(), re.I), "RPM scriptlet failed")


def healthy(manifest, timeout=60):
    deadline = time.monotonic() + timeout
    last = "not started"
    while time.monotonic() < deadline:
        try:
            state = service_state()
            require(state.get("ActiveState") == "active" and state.get("SubState") == "running", f"service state {state}")
            pid = int(state["MainPID"])
            require(pid > 1, "no Agent PID")
            require(sha(Path(f"/proc/{pid}/exe").read_bytes()) == manifest["packages"]["candidate"]["binary_sha256"],
                    "service runs the wrong executable")
            response = run("/usr/bin/wendy", "--json", "device", "info", timeout=10,
                           env={**os.environ, "WENDY_AGENT_SOCKET": SOCKET})
            info = json.loads(response.stdout)
            require(info["version"] == manifest["expected_binary_version"], "Agent RPC version mismatch")
            require(info["cliVersion"] == manifest["expected_binary_version"], "CLI version mismatch")
            require(info["cpuArchitecture"] == manifest["packages"]["candidate"]["elf_architecture"], "RPC architecture mismatch")
            require(service_state() == state, "Agent restarted during health RPC")
            return {"service": state, "device_info": info}
        except (RuntimeError, OSError, ValueError, KeyError, subprocess.TimeoutExpired) as error:
            last = str(error)
            time.sleep(1)
    raise RuntimeError(f"Agent did not become healthy: {last}")


def stopped_stably(seconds=8):
    observations = []
    deadline = time.monotonic() + seconds
    while time.monotonic() < deadline:
        state = service_state()
        require(state.get("ActiveState") == "inactive" and state.get("MainPID") == "0", f"Agent must remain stopped: {state}")
        observations.append(state)
        time.sleep(1)
    return observations


def guest(args):
    bundle = Path(args.bundle).resolve()
    manifest = load_bundle(bundle)
    guest_guard(manifest, args.nonce)
    fmt = manifest["format"]
    evidence = Path(args.evidence)
    evidence.mkdir(parents=True, exist_ok=False)
    report = {"status": "failed", "case": args.case, "manifest": manifest,
              "os_release": Path("/etc/os-release").read_text(), "uname": platform.uname()._asdict()}
    try:
        require(installed(fmt, "wendy-agent") is None and installed(fmt, "wendy") is None,
                "fresh guest required; Wendy packages are already installed")
        require(not Path("/usr/bin/wendy-agent").exists() and not Path("/usr/bin/wendy").exists(), "pre-existing binaries")
        # Network remains private to this guest. Keep the actual Agent on its local policy.
        Path("/etc/wendy-agent").mkdir(exist_ok=True)
        Path("/etc/wendy-agent/network-access").write_text("local\n")
        path = lambda role: str(bundle / manifest["packages"][role]["file"])
        transaction(fmt, "install", [path("cli"), path("legacy")], evidence / "install.log")
        require(installed(fmt, "wendy-agent") == manifest["packages"]["legacy"]["version"], "legacy version not installed")
        require(installed(fmt, "wendy") == manifest["packages"]["cli"]["version"], "CLI package not installed")
        require(sha(Path("/usr/bin/wendy").read_bytes()) == manifest["packages"]["cli"]["binary_sha256"], "wrong CLI payload")
        require(output("/usr/bin/wendy-agent", "--version") == manifest["expected_binary_version"], "Agent binary cannot run")
        require(output("systemctl", "is-enabled", SERVICE) == "enabled", "legacy Agent was not enabled")
        report["before"] = healthy(manifest)
        config = Path("/etc/default/wendy-agent")
        config.write_text(config.read_text() + f"\n# upgrade-fixture-{manifest['nonce']}\n")
        config_hash = sha(config.read_bytes())
        role = "candidate" if args.case == "normal" else "negative-control"
        transaction(fmt, "install", [path(role)], evidence / "upgrade.log")
        require(installed(fmt, "wendy-agent") == manifest["packages"][role]["version"], "upgrade did not install requested version")
        require(sha(config.read_bytes()) == config_hash, "upgrade overwrote local configuration")
        require(output("systemctl", "is-enabled", SERVICE) == "enabled", "upgrade disabled Agent")
        if args.case == "normal":
            report["after"] = healthy(manifest)
            require(report["after"]["service"]["ExecMainStartTimestampMonotonic"] !=
                    report["before"]["service"]["ExecMainStartTimestampMonotonic"], "upgrade did not restart Agent")
            time.sleep(5)
            stable = healthy(manifest, timeout=10)
            require(stable["service"] == report["after"]["service"], "Agent is crash-looping after upgrade")
            report["stable"] = stable
        else:
            report["stopped_control"] = stopped_stably()
            report["negative_control_detected"] = True
        transaction(fmt, "remove", ["wendy-agent"], evidence / "remove.log")
        require(installed(fmt, "wendy-agent") is None, "Agent package survived removal")
        report["removed"] = stopped_stably()
        require(not Path("/usr/bin/wendy-agent").exists(), "Agent executable survived package removal")
        report["status"] = "passed"
    finally:
        journal = run("journalctl", "-u", SERVICE, "--no-pager", "-n", "300", check=False)
        (evidence / "agent-journal.log").write_bytes(journal.stdout + journal.stderr)
        (evidence / "result.json").write_text(json.dumps(report, indent=2) + "\n")
        print(json.dumps(report, indent=2))


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    sub = parser.add_subparsers(dest="command", required=True)
    build = sub.add_parser("prepare")
    build.add_argument("--format", choices=("deb", "rpm"), required=True)
    build.add_argument("--agent", required=True)
    build.add_argument("--cli", required=True)
    build.add_argument("--expected-version", required=True)
    build.add_argument("--output", required=True)
    verify = sub.add_parser("guest")
    verify.add_argument("--bundle", required=True)
    verify.add_argument("--nonce", required=True)
    verify.add_argument("--case", choices=("normal", "negative-control"), required=True)
    verify.add_argument("--evidence", required=True)
    args = parser.parse_args()
    (prepare if args.command == "prepare" else guest)(args)


if __name__ == "__main__":
    main()
