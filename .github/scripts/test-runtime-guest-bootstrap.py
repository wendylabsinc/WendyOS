#!/usr/bin/env python3
"""Build RuntimeGuest from isolated sources and inspect/run its native toolchain.

Uses an existing Docker-driver builder only. Does not boot a VM, load modules,
open vsock ports, create builders, or change a Docker context/default builder.
"""
import argparse
import contextlib
import gzip
import hashlib
import importlib.util
import json
import os
from pathlib import Path, PurePosixPath
import re
import shutil
import signal
import stat
import struct
import subprocess
import sys
import tempfile
import uuid

import yaml

HERE = Path(__file__).resolve().parent
ROOT = HERE.parents[1]
GUEST = Path("swift/WendyAgentMac/RuntimeGuest")
PROXY = Path("go/cmd/wendy-runtime-guest-proxy")


def require(condition, message):
    if not condition:
        raise RuntimeError(message)


def output(*command, env=None):
    return subprocess.run(command, check=True, text=True, capture_output=True, timeout=30, env=env).stdout.strip()


def setup_files(workflow):
    job = workflow["jobs"]["build-macos-runtime-guest"]
    steps = [step for step in job["steps"] if step.get("uses", "").startswith("actions/setup-go@")]
    require(len(steps) == 1, "expected one runtime guest Go setup")
    paths = [steps[0]["with"][key] for key in ("go-version-file", "cache-dependency-path")]
    for path in paths:
        require(isinstance(path, str) and not Path(path).is_absolute() and ".." not in Path(path).parts,
                "Go setup must refer to repository files")
        require((ROOT / path).is_file(), f"runtime guest Go setup file does not exist: {path}")
    require(paths == ["go.mod", "go.sum"], "runtime guest must use the real root module and checksums")
    return paths


def isolated_sources(destination):
    workflow = yaml.safe_load((ROOT / ".github/workflows/build.yml").read_text())
    files = setup_files(workflow)
    tracked = output("git", "-C", str(ROOT), "ls-files", "--", str(GUEST), str(PROXY)).splitlines()
    require(tracked and not any(path.endswith("/wendy-runtime-guest-proxy") for path in tracked), "unexpected bootstrap inputs")
    for name in files + tracked:
        source, target = ROOT / name, destination / name
        require(source.is_file() and not source.is_symlink(), f"missing regular tracked source: {name}")
        target.parent.mkdir(parents=True, exist_ok=True)
        shutil.copyfile(source, target)
        target.chmod(source.stat().st_mode & 0o777)
    require(not (destination / "go/go.mod").exists(), "isolated context contains stale nested module")
    return files + tracked


def kernel_release(data, architecture):
    if architecture == "arm64":
        require(data[56:60] == b"ARMd", "kernel is not a raw ARM64 Image")
        match = re.search(rb"Linux version ([^ \x00]+)", data)
        require(match is not None, "raw ARM64 kernel has no version banner")
        return match.group(1).decode("ascii")
    require(data[0x1fe:0x200] == b"\x55\xaa" and data[0x202:0x206] == b"HdrS", "kernel is not an x86 bzImage")
    require(len(data) > 0x238 and struct.unpack_from("<H", data, 0x236)[0] & 1, "x86 kernel is not 64-bit")
    offset = struct.unpack_from("<H", data, 0x20e)[0] + 0x200
    require(0x200 < offset < len(data), "invalid x86 kernel version offset")
    return data[offset:].split(b"\x00", 1)[0].split(b" ", 1)[0].decode("ascii")


def archive_names(archive, bsdtar):
    with gzip.open(archive, "rb") as stream:
        require(stream.read(6) == b"070701", "initramfs must contain newc CPIO")
    names = output(bsdtar, "-tf", str(archive)).splitlines()
    normalized = []
    for name in names:
        path = PurePosixPath(name)
        require(not path.is_absolute() and ".." not in path.parts, f"unsafe archive member: {name}")
        normalized.append(str(path))
    require(not any(name == "boot" or name.startswith("boot/") for name in normalized), "initramfs duplicates boot kernel")
    return normalized


def inspect_artifacts(artifacts, extracted, architecture, bsdtar, source, go="go"):
    kernel = artifacts / f"vmlinuz-{architecture}"
    archive = artifacts / f"initramfs-{architecture}.img"
    require(kernel.is_file() and archive.is_file(), "bootstrap did not produce both artifacts")
    release = kernel_release(kernel.read_bytes(), architecture)
    names = archive_names(archive, bsdtar)
    module_prefix = f"lib/modules/{release}/"
    require(module_prefix + "modules.dep" in names, "kernel and initramfs modules do not match")
    require(any(name.startswith(module_prefix) and ".ko" in name for name in names), "initramfs has no kernel modules")
    subprocess.run([bsdtar, "-xpf", str(archive), "-C", str(extracted)], check=True, capture_output=True, timeout=120)
    inventory = (extracted / "usr/share/wendy-runtime-packages.txt").read_text()
    for package in ("buildkit", "containerd", "nerdctl", "runc", "cni-plugins", "linux-virt"):
        require(re.search(r"(?m)^" + package + r"-[0-9]", inventory), f"missing actual APK inventory: {package}")
    for relative in ("init", "etc/buildkit/buildkitd.toml", "etc/containerd/config.toml", "etc/cni/net.d/10-wendy.conflist"):
        target = extracted / relative
        require(target.is_file() and not target.is_symlink(), f"missing regular guest configuration: {relative}")
        require(target.read_bytes() == (source / GUEST / "rootfs" / relative).read_bytes(), f"wrong guest configuration: {relative}")
    require((extracted / "init").stat().st_mode & stat.S_IXUSR, "guest init is not executable")
    proxy = extracted / "usr/local/libexec/wendy-runtime-guest-proxy"
    require(proxy.is_file() and not proxy.is_symlink() and proxy.stat().st_mode & stat.S_IXUSR, "proxy is missing or not executable")
    elf = proxy.read_bytes()[:20]
    require(elf[:6] == b"\x7fELF\x02\x01" and struct.unpack_from("<H", elf, 18)[0] == {"arm64": 183, "amd64": 62}[architecture],
            "proxy architecture differs from kernel")
    build_info = output(str(go), "version", "-m", str(proxy), env={**os.environ, "GOTOOLCHAIN": "local"})
    for expected in ("github.com/wendylabsinc/wendy/go/cmd/wendy-runtime-guest-proxy", "GOOS=linux", "GOARCH=" + architecture, "CGO_ENABLED=0"):
        require(expected in build_info, f"proxy build information missing {expected}")
    return {"kernel_release": release, "module_files": sum(name.startswith(module_prefix) and ".ko" in name for name in names),
            "proxy_build_info": build_info, "apk_inventory": inventory,
            "artifacts": {path.name: hashlib.sha256(path.read_bytes()).hexdigest() for path in (kernel, archive)},
            "proxy_sha256": hashlib.sha256(proxy.read_bytes()).hexdigest()}


def wrappers(directory, docker, builder, architecture, calls):
    # Force the ordinary Docker driver path even if the host has an unrelated
    # buildctl daemon. The real source build script and Docker build still run.
    (directory / "buildctl").write_text("#!/bin/sh\nexit 1\n")
    code = f'''#!{sys.executable}
import json, os, sys
args = sys.argv[1:]
with open({str(calls)!r}, "a") as stream:
    stream.write(json.dumps(args) + "\\n")
if args == ["buildx", "version"]:
    pass
elif args[:2] == ["buildx", "build"]:
    if "--builder" in args or "--platform" not in args or args[args.index("--platform")+1] != {("linux/" + architecture)!r}:
        raise SystemExit("unexpected builder/platform request")
    args[2:2] = ["--builder", {builder!r}, "--no-cache"]
else:
    raise SystemExit("unexpected Docker operation in RuntimeGuest build")
os.execv({docker!r}, [{docker!r}, *args])
'''
    (directory / "docker").write_text(code)
    for name in ("buildctl", "docker"):
        (directory / name).chmod(0o755)


def remove_work(directory):
    # Go module downloads deliberately make directories read-only. This is the
    # fixture's own module cache, never the user's/shared Go cache.
    for current, dirs, _ in os.walk(directory, followlinks=False):
        Path(current).chmod(Path(current).stat().st_mode | 0o700)
        for name in dirs:
            path = Path(current) / name
            if not path.is_symlink():
                path.chmod(path.stat().st_mode | 0o700)
    shutil.rmtree(directory)


def cleanup_resources(docker, containers, nonce, work):
    errors = []
    for name in containers:
        try:
            result = subprocess.run([docker, "inspect", name], capture_output=True, text=True, timeout=15)
            if result.returncode:
                require(re.search(r"\bno such (?:object|container)\b", result.stderr, re.I), result.stderr)
                continue
            record = json.loads(result.stdout)[0]
            require(record["Config"]["Labels"].get("wendy.runtime-fixture") == nonce,
                    "refused cleanup for container with unknown ownership")
            result = subprocess.run([docker, "rm", "-f", name], capture_output=True, text=True, timeout=20)
            require(result.returncode == 0 or re.search(r"\bno such (?:object|container)\b", result.stderr, re.I), result.stderr)
        except Exception as error:
            errors.append(f"{name}: {error}")
    if not errors:
        try:
            remove_work(work)
        except Exception as error:
            errors.append(f"{work}: {error}")
    return errors


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--retain-artifacts", type=Path,
                        help="new directory for the exact checked kernel/initramfs, usable after a successful exit")
    parser.add_argument("--timeout", type=int, default=900)
    args = parser.parse_args()
    require(30 <= args.timeout <= 1200, "timeout must be in [30, 1200]")
    require(not args.output.exists() and not args.output.is_symlink(), "output must be new")
    if args.retain_artifacts:
        require(not args.retain_artifacts.exists() and not args.retain_artifacts.is_symlink(), "retained artifacts directory must be new")
    for name in ("go", "docker", "cpio", "bsdtar"):
        require(shutil.which(name), f"missing tool: {name}")
    docker, bsdtar = shutil.which("docker"), shutil.which("bsdtar")
    require(not os.environ.get("DOCKER_HOST"), "use a named Docker context rather than DOCKER_HOST")
    architecture = {"aarch64": "arm64", "arm64": "arm64", "x86_64": "amd64", "amd64": "amd64"}.get(
        output(docker, "info", "--format", "{{.Architecture}}"))
    require(architecture is not None, "Docker must expose a native amd64/arm64 Linux engine")
    require(output(docker, "info", "--format", "{{.OSType}}") == "linux", "Linux Docker engine required")
    builder = output(docker, "context", "show")
    inspect = output(docker, "buildx", "inspect", builder)
    require(re.search(r"(?m)^Driver:\s+docker\s*$", inspect), "an existing ordinary Docker-driver builder is required")
    require(output("go", "env", "GOHOSTARCH") == architecture, "refusing emulated toolchain execution")
    # Resolve setup-go's selected toolchain before isolating module caches. A
    # local Go launcher can select a newer installed toolchain from go.mod.
    go = Path(output("go", "env", "GOROOT")) / "bin/go"
    args.output.mkdir(parents=True)
    evidence = args.output.resolve()
    work = Path(tempfile.mkdtemp(prefix="wendy-runtime-bootstrap-", dir=os.environ.get("RUNNER_TEMP"))).resolve()
    nonce = uuid.uuid4().hex
    containers = set()
    report = {"status": "failed", "architecture": architecture, "builder": builder,
              "scope": "clean build and native tool execution only; no VM boot, VZ, vsock transport or hardware"}
    helper_spec = importlib.util.spec_from_file_location("owned_runtime_build", HERE / "run-owned-media-test.py")
    helper = importlib.util.module_from_spec(helper_spec)
    helper_spec.loader.exec_module(helper)
    def execute(command, name, *, env=None, cwd=None, timeout=60, expected=0):
        with (evidence / name).open("w") as log, contextlib.redirect_stderr(log), contextlib.redirect_stdout(log):
            status = helper.run(list(map(str, command)), timeout, env=env, cwd=cwd, stdout=log)
        require(status == expected, f"{name}: exit {status}, expected {expected}; see {evidence / name}")
        return (evidence / name).read_text()
    def interrupted(signum, frame):
        raise InterruptedError(f"runtime bootstrap interrupted by signal {signum}")
    previous = {sig: signal.signal(sig, interrupted) for sig in (signal.SIGINT, signal.SIGTERM)}
    try:
        source, tools, artifacts, extracted = [work / name for name in ("source", "tools", "artifacts", "extracted")]
        for directory in (source, tools, artifacts, extracted, work / "tmp"):
            directory.mkdir()
        report["source_files"] = isolated_sources(source)
        required_go = re.search(r"(?m)^go (\S+)$", (source / "go.mod").read_text()).group(1)
        report["go_toolchain"] = output(str(go), "version", env={**os.environ, "GOTOOLCHAIN": "local"})
        require("go" + required_go + " " in report["go_toolchain"], "selected Go toolchain differs from root go.mod")
        wrappers(tools, docker, builder, architecture, evidence / "docker-calls.jsonl")
        env = {**os.environ, "PATH": str(tools) + os.pathsep + str(go.parent) + os.pathsep + os.environ["PATH"], "GOWORK": "off", "GOTOOLCHAIN": "local",
               "GOCACHE": str(work / "go-cache"), "GOMODCACHE": str(work / "go-modules"), "TMPDIR": str(work / "tmp"),
               "WENDY_RUNTIME_GUEST_OUT": str(artifacts), "WENDY_RUNTIME_PLATFORM": "linux/" + architecture}
        for key in ("BUILDKIT_HOST", "BUILDX_BUILDER", "DOCKER_HOST"):
            env.pop(key, None)
        execute(["bash", source / GUEST / "build.sh"], "build.log", env=env, cwd=source, timeout=args.timeout)
        require(not (source / GUEST / "wendy-runtime-guest-proxy").exists(), "source build left its generated proxy behind")
        report.update(inspect_artifacts(artifacts, extracted, architecture, bsdtar, source, go))
        # Run the binaries in the produced rootfs, using only ordinary Docker's
        # default chroot capability. /init is never executed and networking is off.
        dockerfile = (source / GUEST / "Dockerfile").read_text()
        image = re.search(r"(?m)^ARG ALPINE_IMAGE=(\S+)$", dockerfile).group(1)
        require("@sha256:" in image, "runtime base image must be digest-pinned")
        commands = [(["/bin/sh", "-ec", "buildkitd --version; containerd --version; nerdctl --version; runc --version"], "versions.log", 0),
                    (["/usr/local/libexec/wendy-runtime-guest-proxy", "--forward", "invalid"], "proxy-invalid.log", 2)]
        for command, log, expected in commands:
            name = "wendy-runtime-" + nonce + "-" + log.removesuffix(".log")
            containers.add(name)
            text = execute([docker, "run", "--rm", "--name", name, "--label", "wendy.runtime-fixture=" + nonce,
                            "--platform", "linux/" + architecture, "--network", "none", "--read-only",
                            "--mount", f"type=bind,src={extracted},dst=/fixture,readonly",
                            "--entrypoint", "/bin/busybox", image, "chroot", "/fixture", *command], log, expected=expected)
            if expected == 2:
                require("must be PORT=/absolute/unix/socket" in text, "proxy failed before parsing its input")
            else:
                for tool in ("buildkit", "containerd", "nerdctl", "runc"):
                    require(tool in text.lower(), f"missing actual tool version output: {tool}")
        calls = [json.loads(line) for line in (evidence / "docker-calls.jsonl").read_text().splitlines()]
        require(sum(call[:2] == ["buildx", "build"] for call in calls) == 1, "did not execute exactly one actual bootstrap build")
        if args.retain_artifacts:
            args.retain_artifacts.mkdir(parents=True)
            for name, expected in report["artifacts"].items():
                target = args.retain_artifacts / name
                shutil.copyfile(artifacts / name, target)
                require(hashlib.sha256(target.read_bytes()).hexdigest() == expected, "retained artifact differs from checked bytes")
            report["retained_artifacts"] = str(args.retain_artifacts.resolve())
        report["status"] = "passed"
    except BaseException as error:
        report["error"] = str(error)
        raise
    finally:
        for sig in previous:
            signal.signal(sig, signal.SIG_IGN)
        try:
            cleanup_errors = cleanup_resources(docker, containers, nonce, work)
            if cleanup_errors:
                report["status"] = "failed"
                report["cleanup_errors"] = cleanup_errors
                report["retained_work"] = str(work)
            (evidence / "evidence.json").write_text(json.dumps(report, indent=2) + "\n")
            print(json.dumps(report, indent=2))
        finally:
            for sig, handler in previous.items():
                signal.signal(sig, handler)
    require(report["status"] == "passed", "runtime fixture cleanup failed")


if __name__ == "__main__":
    main()
