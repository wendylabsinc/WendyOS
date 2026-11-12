#!/usr/bin/env python3
"""Run native package upgrade cases in owned, network-isolated systemd guests.

Hosted GitHub Linux runners only. Preparation resolves distribution dependencies
without installing Wendy. Actual Agent execution starts only after private
networking is enabled. No Docker container is started by this script.
"""
import argparse
import importlib.util
import json
import os
from pathlib import Path
import platform
import re
import shlex
import shutil
import signal
import subprocess
import sys
import tempfile
import time
import types
import uuid

HERE = Path(__file__).resolve().parent
SPEC = importlib.util.spec_from_file_location("package_checks", HERE / "test-installed-linux-packages.py")
CHECKS = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(CHECKS)

# Official Docker Library manifests, verified against Docker-Content-Digest on
# 2026-11-12. Use native child manifests, never a moving tag or emulation.
ROOTS = (
    {"name": "ubuntu22", "id": "ubuntu", "version": "22.04", "format": "deb", "repository": "docker.io/library/ubuntu",
     "index": "sha256:74b12b11ffa550fba1929ad4d8d16533c34f6612728469671afba1b03b66881c",
     "amd64": "sha256:45e9ddc6895604674618a409ca9f73d301da8fdb75fe330e8e9a27b3b219e4ad",
     "arm64": "sha256:0904b10431fdf0800b659e0039e484e3af097f7d81539dc55dd65d23729d7735"},
    {"name": "ubuntu24", "id": "ubuntu", "version": "24.04", "format": "deb", "repository": "docker.io/library/ubuntu",
     "index": "sha256:2198e4c65076e9cb0b3d695219df667c0e3b0072da64ca3c2648aca17dcecaf5",
     "amd64": "sha256:77eb101c607d6e3d4f5ad0c8a02c2136c689b98aba1f359e6502ba517e5c57ec",
     "arm64": "sha256:c92376659a7f14cc1f51615d73c9f4b7d8e18d59f370e02b4ed683ab4485c66c"},
    {"name": "fedora44", "id": "fedora", "version": "44", "format": "rpm", "repository": "docker.io/library/fedora",
     "index": "sha256:15da97ed9aa94dd3d92c3925eb89520f6c80e07ea7ee24cba848d1f8b6912b3e",
     "amd64": "sha256:b189269dfee735c54ebcd3b4e43ff2dc0d4a4b07855468970217c9df57968eb0",
     "arm64": "sha256:4388ba9b8c96a9c1af4026220ac17ea7a577c1352cc8855409e4615b9f1843f9"},
)


def hosted_guard():
    CHECKS.require(sys.platform == "linux" and os.geteuid() == 0, "requires hosted Linux root")
    CHECKS.require(os.environ.get("GITHUB_ACTIONS") == "true" and os.environ.get("RUNNER_ENVIRONMENT") == "github-hosted",
                   "refusing guest provisioning outside a GitHub-hosted runner")
    CHECKS.require(Path("/proc/1/comm").read_text().strip() == "systemd", "host PID 1 must be systemd")
    architecture = {"x86_64": "amd64", "aarch64": "arm64"}.get(platform.machine())
    CHECKS.require(architecture is not None, "unsupported native architecture")
    for tool in ("docker", "systemd-nspawn", "systemd-run", "systemctl", "journalctl", "findmnt", "tar", "cp"):
        CHECKS.require(shutil.which(tool) is not None, f"missing host tool {tool}")
    return architecture


def select_packages(directory, architecture):
    rpm_arch = {"amd64": "x86_64", "arm64": "aarch64"}[architecture]
    result = {}
    for fmt, role, pattern in (("deb", "agent", f"wendy-agent_*_{architecture}.deb"),
                               ("deb", "cli", f"wendy_*_{architecture}.deb"),
                               ("rpm", "agent", f"wendy-agent-*.{rpm_arch}.rpm"),
                               ("rpm", "cli", f"wendy-[0-9]*.{rpm_arch}.rpm")):
        matches = list(directory.glob(pattern))
        CHECKS.require(len(matches) == 1 and matches[0].is_file() and not matches[0].is_symlink(),
                       f"need exactly one native package matching {pattern}, got {matches}")
        result.setdefault(fmt, {})[role] = matches[0]
    return result


def dependencies(packages, fmt):
    if fmt == "deb":
        fields = [CHECKS.output("dpkg-deb", "-f", str(package), field)
                  for package in packages.values() for field in ("Pre-Depends", "Depends")]
        return [", ".join(field for field in fields if field)]
    requirements = set()
    for package in packages.values():
        requirements.update(CHECKS.output("rpm", "-qp", "--requires", str(package)).splitlines())
    # rpmlib capabilities are provided by the RPM engine, not repository packages.
    # The actual offline transaction must still accept all of those requirements.
    return sorted(item for item in requirements if not item.startswith("rpmlib("))


def provision_script(spec, requirements):
    CHECKS.require(requirements and all(requirements), "candidate package dependency metadata is empty")
    lines = ["#!/bin/bash", "set -euxo pipefail", ". /etc/os-release",
             f"test \"$ID\" = {shlex.quote(spec['id'])}",
             f"test \"$VERSION_ID\" = {shlex.quote(spec['version'])}",
             "test ! -e /usr/bin/wendy-agent", "test ! -e /usr/bin/wendy"]
    if spec["format"] == "deb":
        lines += ["export DEBIAN_FRONTEND=noninteractive",
                  "apt-get update --error-on=any",
                  "apt-get install -y --no-install-recommends systemd-sysv dbus python3 ca-certificates",
                  "apt-get satisfy -y --no-install-recommends " + shlex.join(requirements),
                  "dpkg-query -W > /fixture-evidence/dependency-versions.txt"]
    else:
        lines += ["dnf -y --setopt=install_weak_deps=False install systemd dbus-daemon python3 ca-certificates",
                  "dnf -y --setopt=install_weak_deps=False install " + shlex.join(requirements),
                  "rpm -qa --qf '%{NAME} %{VERSION}-%{RELEASE} %{ARCH}\\n' | sort > /fixture-evidence/dependency-versions.txt"]
    lines += ["test -x /usr/lib/systemd/systemd", "test ! -e /usr/bin/wendy-agent", "test ! -e /usr/bin/wendy"]
    return "\n".join(lines) + "\n"


def mount_targets(value):
    result = []
    for entry in value.get("filesystems", []):
        result.append(entry["target"])
        result.extend(mount_targets({"filesystems": entry.get("children", [])}))
    return result


class Guests:
    def __init__(self, output, architecture, duration):
        self.output = output
        self.architecture = architecture
        self.nonce = uuid.uuid4().hex
        self.description = "Wendy package regression " + self.nonce
        self.units = set()
        self.containers = set()
        self.deadline = time.monotonic() + duration
        # Do not put roots under the evidence directory; failed logs must survive.
        self.work = Path(tempfile.mkdtemp(prefix="wendy-package-guests-", dir=os.environ["RUNNER_TEMP"]))
        self.report = {"status": "failed", "architecture": architecture, "nonce": self.nonce,
                       "artifact_registry_acceptance": "pending", "cases": [], "roots": [], "cleanup_errors": []}

    def command(self, arguments, *, timeout=120, check=True, log=None, cleanup=False):
        if not cleanup:
            timeout = min(timeout, self.deadline - time.monotonic())
            CHECKS.require(timeout > 0, "overall package guest deadline exceeded")
        with open(log, "a+b") if log else tempfile.TemporaryFile() as stream:
            stream.write(("$ " + shlex.join(map(str, arguments)) + "\n").encode())
            stream.flush()
            result = subprocess.run(list(map(str, arguments)), stdout=stream, stderr=subprocess.STDOUT, timeout=timeout)
            stream.seek(0)
            data = stream.read()
        if check and result.returncode:
            raise RuntimeError(f"command failed ({result.returncode}): {shlex.join(map(str, arguments))}\n"
                               + data.decode(errors="replace")[-16000:])
        return result.returncode, data.decode(errors="replace")

    def captured(self, arguments, *, timeout=30, check=True, cleanup=False):
        if not cleanup:
            timeout = min(timeout, self.deadline - time.monotonic())
            CHECKS.require(timeout > 0, "overall package guest deadline exceeded")
        result = subprocess.run(arguments, capture_output=True, text=True, timeout=timeout)
        if check and result.returncode:
            raise RuntimeError(f"{arguments!r}: {result.stderr}")
        return result

    def start_unit(self, name, command, *, seconds, wait, log):
        CHECKS.require(re.fullmatch(r"wendy-pkg-[0-9a-f]{16}-[a-z0-9-]+", name), "invalid owned unit name")
        unit = name + ".service"
        self.units.add(unit)  # Track before a command can succeed remotely and time out locally.
        arguments = ["systemd-run", "--unit=" + unit, "--collect", "--service-type=exec",
                     "--description=" + self.description, "--property=Delegate=yes",
                     "--property=KillMode=control-group", "--property=TimeoutStopSec=20s",
                     f"--property=RuntimeMaxSec={seconds}s"]
        if wait:
            arguments += ["--wait", "--pipe"]
        arguments += command
        self.command(arguments, timeout=seconds + 30 if wait else 30, log=log)
        return unit

    def stop_unit(self, unit):
        query = self.captured(["systemctl", "show", unit, "--property=LoadState,Description"], check=False, cleanup=True)
        properties = dict(line.split("=", 1) for line in query.stdout.splitlines() if "=" in line)
        if properties.get("LoadState") == "not-found":
            self.units.discard(unit)
            return
        CHECKS.require(query.returncode == 0 and properties.get("Description") == self.description,
                       f"refusing to stop unit with unknown ownership: {unit}")
        self.command(["systemctl", "stop", unit], timeout=45, cleanup=True)
        state = self.captured(["systemctl", "show", unit, "--property=ActiveState", "--value"],
                              check=False, cleanup=True).stdout.strip()
        CHECKS.require(state in ("", "inactive", "failed"), f"owned guest failed to stop: {unit}: {state}")
        self.units.discard(unit)

    def remove_container(self, name):
        result = self.captured(["docker", "inspect", name], check=False, cleanup=True)
        if result.returncode:
            CHECKS.require("No such" in result.stderr, f"cannot inspect owned Docker export: {result.stderr}")
        else:
            record = json.loads(result.stdout)[0]
            CHECKS.require(record["Config"]["Labels"].get("wendy.package-fixture") == self.nonce,
                           "refusing to remove Docker container with unknown ownership")
            self.command(["docker", "rm", "-f", name], timeout=30, cleanup=True)
        self.containers.discard(name)

    def remove_root(self, root):
        CHECKS.require(root.is_relative_to(self.work) and root != self.work, "cleanup path is outside owned work root")
        mounts = json.loads(self.captured(["findmnt", "--json", "--output", "TARGET"], cleanup=True).stdout)
        CHECKS.require(not any(Path(target) == root or Path(target).is_relative_to(root) for target in mount_targets(mounts)),
                       f"refusing to delete still-mounted guest root {root}")
        shutil.rmtree(root)

    def template(self, spec, packages):
        evidence = self.output / spec["name"]
        evidence.mkdir()
        image = spec["repository"] + "@" + spec[self.architecture]
        self.command(["docker", "pull", "--platform=linux/" + self.architecture, image], timeout=300,
                     log=evidence / "image.log")
        info = json.loads(self.captured(["docker", "image", "inspect", image]).stdout)[0]
        CHECKS.require(info["Architecture"] == self.architecture and info["Os"] == "linux", "image architecture mismatch")
        self.report["roots"].append({**spec, "selected_manifest": spec[self.architecture], "image_id": info["Id"]})
        name = "wendy-pkg-" + self.nonce[:16] + "-export-" + spec["name"]
        self.containers.add(name)
        self.command(["docker", "create", "--name", name, "--label", "wendy.package-fixture=" + self.nonce,
                      "--platform=linux/" + self.architecture, image, "/bin/true"], log=evidence / "image.log")
        archive = self.work / (spec["name"] + ".tar")
        try:
            self.command(["docker", "export", "--output", archive, name], timeout=180, log=evidence / "image.log")
        finally:
            self.remove_container(name)
        root = self.work / (spec["name"] + "-template")
        root.mkdir()
        self.command(["tar", "--extract", "--numeric-owner", "--file", archive, "--directory", root], timeout=120)
        archive.unlink()
        requirements = dependencies(packages, spec["format"])
        (evidence / "requested-dependencies.json").write_text(json.dumps(requirements, indent=2) + "\n")
        script = self.work / (spec["name"] + "-provision.sh")
        script.write_text(provision_script(spec, requirements))
        # Debian packages must not start services during dependency preparation.
        policy = root / "usr/sbin/policy-rc.d"
        old_policy = policy.read_bytes() if policy.exists() else None
        old_mode = policy.stat().st_mode & 0o777 if policy.exists() else None
        if spec["format"] == "deb":
            policy.write_text("#!/bin/sh\nexit 101\n")
            policy.chmod(0o755)
        unit = "wendy-pkg-" + self.nonce[:16] + "-provision-" + spec["name"]
        try:
            self.start_unit(unit, ["systemd-nspawn", "--quiet", "--keep-unit", "--register=no", "--directory=" + str(root),
                                  "--resolv-conf=copy-host", "--bind-ro=" + str(script) + ":/provision.sh",
                                  "--bind=" + str(evidence) + ":/fixture-evidence", "/bin/bash", "/provision.sh"],
                            seconds=900, wait=True, log=evidence / "provision.log")
        finally:
            self.stop_unit(unit + ".service")
            if spec["format"] == "deb":
                if old_policy is None:
                    policy.unlink()
                else:
                    policy.write_bytes(old_policy)
                    policy.chmod(old_mode)
        return root, evidence

    def boot_case(self, spec, template, evidence, bundle, case):
        name = "wendy-pkg-" + self.nonce[:16] + "-" + spec["name"] + "-" + case
        root = self.work / name
        root.mkdir()
        self.command(["cp", "-a", "--reflink=auto", str(template) + "/.", root], timeout=120)
        # Each fresh root gets its own identity; no host or previous guest ID.
        machine_id = root / "etc/machine-id"
        machine_id.unlink(missing_ok=True)
        machine_id.write_text("")
        dbus_id = root / "var/lib/dbus/machine-id"
        dbus_id.unlink(missing_ok=True)
        log = evidence / (case + "-host.log")
        manifest = CHECKS.load_bundle(bundle)
        command = ["systemd-nspawn", "--quiet", "--keep-unit", "--register=yes", "--boot", "--directory=" + str(root),
                   "--machine=" + name, "--uuid=" + uuid.uuid4().hex, "--private-network", "--resolv-conf=off",
                   "--link-journal=no", "--bind-ro=" + str(bundle) + ":/fixture",
                   "--bind=" + str(evidence) + ":/evidence"]
        unit = name + ".service"
        try:
            self.start_unit(name, command, seconds=1200, wait=False, log=log)
            deadline = min(self.deadline, time.monotonic() + 120)
            while True:
                result = self.captured(["systemctl", "--machine=" + name, "show", "--property=SystemState", "--value"],
                                       timeout=10, check=False)
                if result.returncode == 0 and result.stdout.strip() in ("running", "degraded"):
                    break
                CHECKS.require(time.monotonic() < deadline, f"guest did not boot: {name}")
                time.sleep(1)
            self.command(["systemd-run", "--machine=" + name, "--unit=package-verifier.service", "--wait", "--pipe", "--collect",
                          "--service-type=exec", "--property=RuntimeMaxSec=900s", "--property=TimeoutStopSec=20s",
                          "--setenv=WENDY_PACKAGE_GUEST_NONCE=" + manifest["nonce"],
                          "/usr/bin/python3", "/fixture/test-installed-linux-packages.py", "guest", "--bundle", "/fixture",
                          "--nonce", manifest["nonce"], "--case", case, "--evidence", "/evidence/" + case], timeout=940, log=log)
            result = json.loads((evidence / case / "result.json").read_text())
            CHECKS.require(result["status"] == "passed" and result["case"] == case, "guest result is missing or failed")
            CHECKS.require(result["manifest"] == manifest, "guest verified another package bundle")
            if case == "negative-control":
                CHECKS.require(result.get("negative_control_detected") is True, "negative control was not detected")
            self.report["cases"].append({"distribution": spec["name"], "case": case, "status": "passed"})
        finally:
            self.command(["journalctl", "--unit=" + unit, "--no-pager", "-n", "400"], check=False,
                         log=evidence / (case + "-boot.log"), cleanup=True)
            self.stop_unit(unit)
            self.remove_root(root)

    def cleanup(self):
        for unit in list(self.units):
            try:
                self.stop_unit(unit)
            except Exception as error:
                self.report["cleanup_errors"].append(str(error))
        for name in list(self.containers):
            try:
                self.remove_container(name)
            except Exception as error:
                self.report["cleanup_errors"].append(str(error))
        if not self.units and not self.containers:
            try:
                for root in self.work.iterdir():
                    if root.is_dir():
                        self.remove_root(root)
                    else:
                        root.unlink()
                self.work.rmdir()
            except Exception as error:
                self.report["cleanup_errors"].append(str(error))
        if self.report["cleanup_errors"]:
            self.report["status"] = "failed"
        (self.output / "summary.json").write_text(json.dumps(self.report, indent=2) + "\n")
        print(json.dumps(self.report, indent=2), flush=True)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--packages", type=Path, required=True)
    parser.add_argument("--expected-version", required=True)
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--timeout", type=int, default=3000)
    args = parser.parse_args()
    architecture = hosted_guard()
    CHECKS.require(300 <= args.timeout <= 3600, "timeout must be between 300 and 3600 seconds")
    packages = select_packages(args.packages.resolve(), architecture)
    output = args.output.resolve()
    output.mkdir(parents=True, exist_ok=False)
    fixture = Guests(output, architecture, args.timeout)
    def interrupted(signum, frame):
        raise InterruptedError(f"package guests interrupted by signal {signum}")
    previous = {sig: signal.signal(sig, interrupted) for sig in (signal.SIGINT, signal.SIGTERM)}
    try:
        bundles = {}
        for fmt in ("deb", "rpm"):
            bundle = fixture.work / (fmt + "-bundle")
            CHECKS.prepare(types.SimpleNamespace(format=fmt, agent=str(packages[fmt]["agent"]), cli=str(packages[fmt]["cli"]),
                                                expected_version=args.expected_version, output=str(bundle)))
            bundles[fmt] = bundle
            shutil.copyfile(bundle / "manifest.json", output / (fmt + "-manifest.json"))
        for spec in ROOTS:
            template, evidence = fixture.template(spec, packages[spec["format"]])
            try:
                for case in ("normal", "negative-control"):
                    fixture.boot_case(spec, template, evidence, bundles[spec["format"]], case)
            finally:
                fixture.remove_root(template)
        CHECKS.require(len(fixture.report["cases"]) == 6, "missing distro/case result")
        fixture.report["status"] = "passed"
    except BaseException as error:
        fixture.report["error"] = str(error)
        raise
    finally:
        # Finish bounded, ownership-checked cleanup even if CI sends TERM twice.
        for sig in previous:
            signal.signal(sig, signal.SIG_IGN)
        fixture.cleanup()
        for sig, handler in previous.items():
            signal.signal(sig, handler)
    CHECKS.require(fixture.report["status"] == "passed", "package guest cleanup failed")


if __name__ == "__main__":
    main()
