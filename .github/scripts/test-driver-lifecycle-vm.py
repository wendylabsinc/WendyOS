#!/usr/bin/env python3
"""Audit2049: real driver replacement/rollback/removal in an owned QEMU kernel.

Hosted Linux only. No host modules, mounts, services, devices, KVM, bridge or tap.
Ordinary Docker builds the guest; QEMU has no network and only its private disk.
"""
import argparse
import hashlib
import importlib.util
import json
import os
from pathlib import Path, PurePosixPath
import platform
import shutil
import signal
import subprocess
import tarfile
import tempfile
import time
import uuid

ROOT = Path(__file__).resolve().parents[2]
FIXTURE = ROOT / ".github/scripts/fixtures/driver-vm"
SPEC = importlib.util.spec_from_file_location("driver_guardian", ROOT / ".github/scripts/run-owned-media-test.py")
GUARDIAN = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(GUARDIAN)
# Fedora 45 official amd64 manifest (2026-11-02); systemd >=260 is required
# by the unchanged Builder apply script's --always-refresh option.
BASE = "docker.io/library/fedora@sha256:53d138f25f6738257c78553d51369b662efc0e0eb1cd1556a2fe0ad5a115be88"
BUILDER = "f72a1b354c80523dc2b092e52e38e44fe56c8d5a"
SOURCES = {
    "recipes-core/wendyos-sysext-apply/files/wendyos-sysext-apply.sh": "ee68d5fdd1bf76378e221e94a58da3c016325e930ebdc73143d7721d31ece9e9",
    "scripts/pack-sysext.sh": "482d6e4e7408478b222ec63d12b35d1dbfcf3fe7f3b5ba2062a04bb289e5baa4",
}
LABEL = "sh.wendy.regression.driver-vm"
GIB = 1024 ** 3
WORK_TIMEOUT = 25 * 60
CLEANUP_TIMEOUT = 3 * 60


def require(condition, message):
    if not condition:
        raise ValueError(message)


def digest(path):
    value = hashlib.sha256()
    with Path(path).open("rb") as stream:
        for chunk in iter(lambda: stream.read(4 * 1024 * 1024), b""):
            value.update(chunk)
    return value.hexdigest()


def validate_export(path):
    """Reject traversal/link ancestors before privileged extraction of image bytes."""
    nodes = {}
    total = 0
    with tarfile.open(path) as archive:
        for item in archive:
            name = str(PurePosixPath(item.name))
            require(name not in (".", "") and not name.startswith("/") and ".." not in PurePosixPath(name).parts,
                    "unsafe image archive member")
            require(name not in nodes, "duplicate image archive member")
            require(item.isdir() or item.isfile() or item.issym() or item.islnk(), "special image archive member")
            if item.islnk():
                target = PurePosixPath(item.linkname)
                require(not target.is_absolute() and ".." not in target.parts, "unsafe image hardlink")
            nodes[name] = item
            total += item.size
            require(len(nodes) <= 400_000 and total <= 3 * GIB, "guest export exceeds declared capacity")
    for name, item in nodes.items():
        for parent in PurePosixPath(name).parents:
            ancestor = nodes.get(str(parent))
            require(ancestor is None or ancestor.isdir(), "archive member descends through a symlink/file")
        if item.islnk():
            target = nodes.get(str(PurePosixPath(item.linkname)))
            require(target is not None and target.isfile(), "image hardlink target is not a regular member")
    return {"members": len(nodes), "expanded_bytes": total}


def accept_log(text, nonce, control, phase, code):
    require(code == 0, "QEMU did not shut down normally")
    require("--- SKIP:" not in text, "guest driver test skipped")
    marker = f"DRIVER_VM_EXIT nonce={nonce} control={control} phase={phase} status="
    lines = [line.strip() for line in text.splitlines() if line.startswith(marker)]
    require(len(lines) == 1, "missing or duplicated owned guest completion")
    if control == "normal":
        require(lines[0] == marker + "0" and "--- PASS: TestDriverLifecycleVM" in text, "guest lifecycle failed")
        required = "DRIVER_VM_REBOOT_VERIFIED" if phase == "reboot" else "DRIVER_VM_LIFECYCLE_VERIFIED " + nonce
        require(required in text, "missing actual lifecycle/kernel observation")
    else:
        require(lines[0] != marker + "0" and "--- FAIL: TestDriverLifecycleVM" in text, "regression control unexpectedly passed")
        expected = "live driver version mismatch: wendy_fixture_target" if control == "omit-unload" else "driver removal rolled back due to unrelated add-on:"
        require(expected in text, "control failed outside its intended boundary")
        require("DRIVER_VM_LIFECYCLE_VERIFIED" not in text, "control reported normal completion")


class Runner:
    def __init__(self, work, output, nonce):
        self.work, self.output, self.nonce = work, output, nonce
        self.counter = 0
        self.container = "wendy-driver-vm-" + nonce
        self.tag = "wendy-driver-vm:" + nonce
        self.created_container = False
        self.created_image = False
        self.results = {}
        self.started = time.monotonic()
        self.deadline = self.started + WORK_TIMEOUT

    def remaining_timeout(self, requested):
        remaining = self.deadline - time.monotonic()
        require(remaining > 0, "driver VM shared wall deadline exhausted")
        return min(requested, remaining)

    def command(self, argv, timeout=120, capture=False, check=True):
        self.counter += 1
        log = self.output / f"command-{self.counter:02d}.log"
        with log.open("x") as stream:
            code = GUARDIAN.run(argv, self.remaining_timeout(timeout), cwd=ROOT, stdout=stream)
        text = log.read_text(errors="replace")
        if check:
            require(code == 0, f"command failed ({code}); inspect {log}")
        return text if capture else code

    def verify_docker_owner(self, kind, name):
        result = self.command(["docker", kind, "inspect", name], capture=True)
        entries = json.loads(result)
        require(len(entries) == 1 and entries[0]["Config"]["Labels"].get(LABEL) == self.nonce,
                "refusing cleanup of a Docker resource with another owner")

    def prepare(self):
        self.results["source_head"] = self.command(["git", "rev-parse", "HEAD"], capture=True).strip()
        sources = [Path(__file__), ROOT / "go/internal/agent/services/driver_lifecycle_vm_linux_test.go",
                   ROOT / "go/internal/agent/services/driver_service.go", *FIXTURE.iterdir()]
        self.results["source_sha256"] = {str(path.relative_to(ROOT)): digest(path) for path in sources}
        context = self.work / "context"
        context.mkdir()
        for source in FIXTURE.iterdir():
            require(source.is_file(), "unexpected fixture subdirectory")
            shutil.copyfile(source, context / source.name)
        for path, expected in SOURCES.items():
            destination = context / Path(path).name
            self.command(["curl", "-q", "--fail", "--location", "--silent", "--show-error", "--connect-timeout", "15", "--max-time", "90",
                          "--output", str(destination), f"https://raw.githubusercontent.com/wendylabsinc/WendyOS-Builder/{BUILDER}/{path}"], timeout=100)
            require(digest(destination) == expected, "shipped Builder script checksum mismatch")
        self.command(["env", "CGO_ENABLED=0", "GOOS=linux", "GOARCH=amd64", "go", "test", "-c", "-o", str(context / "services.test"), "./go/internal/agent/services"], timeout=600)
        source = ROOT / "go/internal/agent/services/driver_service.go"
        needle = 'if errors.Is(err, context.DeadlineExceeded) || !s.removalSettled(stream.Context(), name) {'
        text = source.read_text()
        require(text.count(needle) == 1, "removal rollback control site changed")
        mutation = self.work / "driver_service.go"
        mutation.write_text(text.replace(needle, "if true { // original unconditional rollback policy"))
        overlay = self.work / "removal-overlay.json"
        overlay.write_text(json.dumps({"Replace": {str(source): str(mutation)}}))
        self.command(["env", "CGO_ENABLED=0", "GOOS=linux", "GOARCH=amd64", "go", "test", "-overlay", str(overlay), "-c", "-o", str(context / "removal-rollback.test"), "./go/internal/agent/services"], timeout=600)
        self.results["binary_sha256"] = {name: digest(context / name) for name in ("services.test", "removal-rollback.test")}
        (context / "nonce").write_text(self.nonce + "\n")
        (context / "driver-fixture.service").write_text('''[Unit]
Description=Owned driver regression VM
After=local-fs.target systemd-udevd.service dbus.socket
Requires=systemd-udevd.service
[Service]
Type=oneshot
ExecStart=/bin/bash /fixture/run-guest.sh
StandardInput=null
StandardOutput=tty
StandardError=tty
TTYPath=/dev/ttyS0
TimeoutStartSec=480
[Install]
WantedBy=multi-user.target
''')
        (context / "Dockerfile").write_text(f'''FROM {BASE}
LABEL {LABEL}={self.nonce}
RUN dnf -y --setopt=install_weak_deps=False install systemd systemd-udev dbus-daemon kernel-core kernel-modules kernel-devel kmod squashfs-tools dracut gcc make elfutils-libelf-devel python3 findutils diffutils procps-ng util-linux e2fsprogs && dnf clean all
COPY . /fixture/
RUN install -m755 /fixture/wendyos-sysext-apply.sh /usr/sbin/wendyos-sysext-apply.sh && bash /fixture/build-guest.sh
RUN install -m644 /fixture/driver-fixture.service /etc/systemd/system/driver-fixture.service && systemctl enable driver-fixture.service && systemctl set-default multi-user.target && systemctl mask serial-getty@ttyS0.service && printf '%s\\n' '{self.nonce}' > /etc/machine-id && mkdir -p /data/extensions/enabled && rm -f /fixture/Dockerfile
''')
        self.created_image = True # Track before build; a failed late build may still tag it.
        self.command(["docker", "build", "--pull=false", "--network=default", "--label", LABEL + "=" + self.nonce, "-t", self.tag, str(context)], timeout=1200)
        self.verify_docker_owner("image", self.tag)
        self.created_container = True
        self.command(["docker", "create", "--name", self.container, "--label", LABEL + "=" + self.nonce, "--network=none", self.tag, "/usr/bin/false"])
        self.verify_docker_owner("container", self.container)
        for name in ("vmlinuz", "initrd", "kernel-release", "packages.txt", "artifacts.sha256"):
            destination = self.work / name if name in ("vmlinuz", "initrd") else self.output / name
            self.command(["docker", "cp", self.container + ":/fixture/" + name, str(destination)])
        export = self.work / "rootfs.tar"
        self.command(["docker", "export", "--output", str(export), self.container], timeout=180)
        self.results["export"] = validate_export(export)
        rootfs = self.work / "rootfs"
        rootfs.mkdir()
        # The exported archive is validated before this only privileged file write.
        # It contains no member below a symlink and no devices or outside hardlinks.
        self.command(["sudo", "-n", "tar", "--extract", "--file", str(export), "--directory", str(rootfs), "--numeric-owner", "--same-owner", "--same-permissions"], timeout=180)
        disk = self.work / "base.ext4"
        with disk.open("xb") as stream:
            stream.truncate(4 * GIB)
        self.command(["sudo", "-n", "mkfs.ext4", "-q", "-F", "-L", "wendy-driver-vm", "-d", str(rootfs), str(disk)], timeout=180)
        self.remove_rootfs(rootfs)
        export.unlink()
        self.results["guest_base"] = {"disk_bytes": disk.stat().st_size, "kernel_sha256": digest(self.work / "vmlinuz"), "initrd_sha256": digest(self.work / "initrd")}

    def remove_rootfs(self, rootfs):
        require(rootfs.parent == self.work and rootfs.name == "rootfs" and not rootfs.is_symlink(), "unsafe rootfs cleanup target")
        mounts = self.command(["findmnt", "--json", "--output", "TARGET"], capture=True)
        def mounted(entries):
            for entry in entries:
                target = Path(entry["target"])
                if target == rootfs or rootfs in target.parents:
                    return True
                if mounted(entry.get("children", [])):
                    return True
            return False
        require(not mounted(json.loads(mounts).get("filesystems", [])), "refusing to remove a mounted rootfs")
        self.command(["sudo", "-n", "rm", "-rf", "--", str(rootfs)], timeout=120)

    def boot(self, disk, control, phase):
        append = f"root=/dev/vda rw console=ttyS0 loglevel=4 selinux=0 audit=0 systemd.show_status=false wendy.driver={self.nonce} wendy.control={control} wendy.phase={phase} "
        command = ["qemu-system-x86_64", "-machine", "q35,accel=tcg", "-cpu", "max", "-smp", "2", "-m", "2048",
                   "-display", "none", "-monitor", "none", "-serial", "stdio", "-no-reboot", "-nic", "none",
                   "-smbios", "type=1,product=WendyDriverFixture-" + self.nonce,
                   "-kernel", str(self.work / "vmlinuz"), "-initrd", str(self.work / "initrd"),
                   "-append", append, "-drive", "file=" + str(disk) + ",format=qcow2,if=virtio"]
        log = self.output / (control + "-" + phase + ".log")
        start = time.monotonic()
        with log.open("x") as stream:
            code = GUARDIAN.run(command, self.remaining_timeout(600), cwd=ROOT, stdout=stream)
        require(log.stat().st_size <= 32 * 1024 * 1024, "guest console exceeded evidence limit")
        accept_log(log.read_text(errors="replace"), self.nonce, control, phase, code)
        self.results[control + "/" + phase] = {"passed": True, "seconds": round(time.monotonic() - start, 2), "log": str(log)}

    def run(self):
        self.prepare()
        for control in ("normal", "omit-unload", "removal-rollback"):
            disk = self.work / (control + ".qcow2")
            self.command(["qemu-img", "create", "-f", "qcow2", "-F", "raw", "-b", str(self.work / "base.ext4"), str(disk)])
            self.boot(disk, control, "first")
            if control == "normal":
                self.boot(disk, control, "reboot")
            disk.unlink()

    def cleanup(self):
        # Cleanup has its own reserve even when the work deadline expires.
        self.results["work_seconds"] = round(time.monotonic() - self.started, 3)
        self.deadline = time.monotonic() + CLEANUP_TIMEOUT
        failures = []
        for kind, name, created in (("container", self.container, self.created_container), ("image", self.tag, self.created_image)):
            if not created:
                continue
            try:
                # A failed build/create may not have produced this exact resource.
                listed = self.command(["docker", kind, "ls", "-aq", "--filter", "label=" + LABEL + "=" + self.nonce], capture=True)
                if not listed.strip():
                    continue
                self.verify_docker_owner(kind, name)
                self.command(["docker", kind, "rm", name], timeout=120)
            except BaseException as error:
                failures.append(f"{kind}: {error}")
        rootfs = self.work / "rootfs"
        if rootfs.exists():
            try:
                self.remove_rootfs(rootfs)
            except BaseException as error:
                failures.append(f"rootfs: {error}")
        if not failures:
            shutil.rmtree(self.work)
        return failures


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--work-parent", type=Path, required=True)
    args = parser.parse_args()
    require(platform.system() == "Linux" and platform.machine() == "x86_64", "requires native hosted Linux x86_64")
    require(os.environ.get("GITHUB_ACTIONS") == "true" and os.environ.get("RUNNER_ENVIRONMENT") == "github-hosted", "refusing VM preparation outside disposable hosted CI")
    require(os.geteuid() != 0, "run as the ordinary runner user; only validated extraction/mkfs/cleanup use sudo")
    for tool in ("docker", "go", "curl", "qemu-system-x86_64", "qemu-img", "mkfs.ext4", "tar", "findmnt", "sudo"):
        require(shutil.which(tool), "missing prerequisite: " + tool)
    parent = args.work_parent.resolve(strict=True)
    require(not any(char in str(parent) for char in (",", "\n", "\r")), "work-parent cannot contain QEMU drive delimiters")
    require(shutil.disk_usage(parent).free >= 16 * GIB, "need 16 GiB free for bounded guest preparation")
    output = args.output.resolve()
    output.mkdir(parents=True, exist_ok=False)
    work = Path(tempfile.mkdtemp(prefix="wendy-driver-vm-", dir=parent))
    nonce = uuid.uuid4().hex
    runner = Runner(work, output, nonce)
    evidence = {"audit_entry":"2049", "nonce":nonce, "builder_commit":BUILDER, "builder_sources":SOURCES, "root_image":BASE, "work_budget_seconds":WORK_TIMEOUT, "cleanup_budget_seconds":CLEANUP_TIMEOUT,
                "scope":"real disposable QEMU kernel/systemd-sysext; no physical vendor hardware", "results":runner.results, "passed":False}
    old_handlers = {}
    def interrupted(signum, frame):
        raise InterruptedError(f"interrupted by signal {signum}")
    for signum in (signal.SIGTERM, signal.SIGINT):
        old_handlers[signum] = signal.signal(signum, interrupted)
    try:
        runner.run()
        evidence["passed"] = True
    finally:
        # Guardian waits for each owned command group before returning here.
        for signum in old_handlers:
            signal.signal(signum, signal.SIG_IGN)
        try:
            failures = runner.cleanup()
            evidence["cleanup_errors"] = failures
            if failures:
                evidence["passed"] = False
                evidence["retained_work"] = str(work)
        except BaseException as error:
            evidence["passed"] = False
            evidence["cleanup_errors"] = [str(error)]
            evidence["retained_work"] = str(work)
        finally:
            try:
                (output / "evidence.json").write_text(json.dumps(evidence, indent=2)+"\n")
            finally:
                for signum, handler in old_handlers.items():
                    signal.signal(signum, handler)
    require(evidence["passed"], "driver VM evidence/cleanup incomplete")
    print("real driver lifecycle VM and both regression controls passed")


if __name__ == "__main__":
    main()
