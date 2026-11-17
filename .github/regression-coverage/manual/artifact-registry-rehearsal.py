#!/usr/bin/env python3
"""Prepare2504/2521 external acceptance, or consume packages on an admitted GCE guest.

Preparation is offline. Consumption downloads packages into a new private directory;
it never installs packages, changes system repositories, uploads, or creates a VM.
"""
import argparse
import hashlib
import json
import os
from pathlib import Path
import platform
import re
import shutil
import subprocess
import sys
import urllib.request
import uuid

HERE = Path(__file__).resolve().parent
ROOT = HERE.parents[2]


def require(condition, message):
    if not condition:
        raise ValueError(message)


def sha(path):
    result = hashlib.sha256()
    with Path(path).open("rb") as source:
        for chunk in iter(lambda: source.read(4 * 1024**2), b""):
            result.update(chunk)
    return result.hexdigest()


def artifact(value):
    path = Path(value).absolute()
    require(not path.is_symlink() and path.is_file() and 0 < path.stat().st_size <= 1024**3,
            "require regular nonempty file <=1GiB")
    return {"path": str(path), "sha256": sha(path), "bytes": path.stat().st_size}


def atom(value, expression, description):
    require(isinstance(value, str) and re.fullmatch(expression, value) is not None, description)
    return value


def no_secrets(value):
    if isinstance(value, dict):
        for name, item in value.items():
            require(not any(word in name.lower() for word in ("password", "token", "private_key", "secret")),
                    "do not include credentials in inventory")
            no_secrets(item)
    elif isinstance(value, list):
        for item in value:
            no_secrets(item)


def validate_package(item):
    fmt, arch, name = item["format"], item["architecture"], item["name"]
    require(fmt in ("deb", "rpm") and arch in ("amd64", "arm64") and name in ("wendy", "wendy-agent"),
            "unsupported candidate package")
    atom(item["version"], r"[0-9][A-Za-z0-9.+:_~-]*", "invalid exact package version")
    record = artifact(item["path"])
    require(record["sha256"] == item["sha256"], "candidate package hash changed")
    with Path(record["path"]).open("rb") as source:
        lead = source.read(8)
    require(lead == b"!<arch>\n" if fmt == "deb" else lead[:6] == bytes.fromhex("edabeedb0300"),
            "not a DEB archive or RPMv4/3.0 lead")
    proof_record = artifact(item["native_result"])
    proof = json.loads(Path(proof_record["path"]).read_text())
    require(proof["status"] == "passed" and proof["case"] == "normal", "native normal transaction proof required")
    manifest = proof["manifest"]
    require(manifest["format"] == fmt, "native proof has another package format")
    previous = manifest["packages"]["candidate" if name == "wendy-agent" else "cli"]
    require(previous["sha256"] == record["sha256"] and previous["name"] == name
            and previous["version"] == item["version"] and previous["elf_architecture"] == arch,
            "native proof does not bind these exact candidate bytes/version/architecture")
    return {**record, "format": fmt, "architecture": arch, "name": name, "version": item["version"],
            "native_result": proof_record, "binary_sha256": previous["binary_sha256"]}


def prepare(inventory, output):
    raw = json.loads(Path(inventory).read_text())
    no_secrets(raw)
    trial = atom(raw["trial"], r"[a-f0-9]{32}", "fresh128-bit nonce required")
    atom(raw["source_commit"], r"[a-f0-9]{40}", "full release source commit required")
    atom(raw["release_run"], r"https://github.com/wendylabsinc/[Ww]endy[Oo][Ss]/actions/runs/[0-9]+", "exact release run URL required")
    target = raw["target"]
    atom(target["project"], r"[a-z][a-z0-9-]{4,61}[a-z0-9]", "exact GCP project ID required")
    atom(target["location"], r"[a-z][a-z0-9-]+", "exact GCP location required")
    require(target["mode"] in ("reserved", "authorized-release"), "choose reserved or authorized-release context")
    records = {name: artifact(raw["records"][name]) for name in
               ("authorization", "release_provenance", "repository_identity", "guest_reservation", "cleanup")}
    for fmt in ("apt", "yum"):
        name = atom(target[fmt], r"[a-z][a-z0-9-]+", "invalid repository name")
        if target["mode"] == "reserved":
            require(name == f"wendy-audit-{trial[:16]}-{fmt}", "reserved repository must have trial identity")
        else:
            require(name == "wendy-" + fmt and target["location"] == "us-central1", "release target differs from production workflow")
    packages = [validate_package(item) for item in raw["packages"]]
    keys = [(p["format"], p["architecture"], p["name"]) for p in packages]
    expected = {(fmt, arch, name) for fmt in ("deb", "rpm") for arch in ("amd64", "arm64") for name in ("wendy", "wendy-agent")}
    require(len(keys) == 8 and set(keys) == expected, "require all eight native release packages exactly once")
    guests = raw["guests"]
    require(set(guests) == {f"{fmt}-{arch}" for fmt in ("deb", "rpm") for arch in ("amd64", "arm64")}, "require four native consumer guests")
    ids = set()
    for name, guest in guests.items():
        atom(guest["instance_id"], r"[0-9]+", "exact Compute Engine instance ID required")
        atom(guest["project"], r"[a-z][a-z0-9-]{4,61}[a-z0-9]", "exact guest project required")
        atom(guest["zone"], r"[a-z0-9-]+", "exact guest zone required")
        require(str(uuid.UUID(guest["boot_id"])) == guest["boot_id"], "exact guest boot UUID required")
        for field in ("os_release_sha256", "client_sha256", "metadata_tool_sha256"):
            atom(guest[field], r"[a-f0-9]{64}", "exact guest tool/OS hash required")
        require(guest["instance_id"] not in ids, "consumer guest identities must be distinct")
        ids.add(guest["instance_id"])
    keyring = artifact(raw["apt_keyring"])
    require(keyring["sha256"] == raw["apt_keyring_sha256"], "reviewed APT keyring hash differs")
    output = Path(output).resolve()
    output.mkdir(mode=0o700, parents=True, exist_ok=False)
    shutil.copyfile(keyring["path"], output / "apt-keyring.gpg")
    shutil.copyfile(__file__, output / "acceptance.py")
    shutil.copyfile(HERE / "artifact-registry-rehearsal.txt", output / "procedure.txt")
    guardian = ROOT / ".github/scripts/run-owned-media-test.py"
    shutil.copyfile(guardian, output / "guardian.py")
    commands = {}
    base = ["gcloud", "--project=" + target["project"]]
    for fmt in ("apt", "yum"):
        commands["describe-" + fmt] = base + ["artifacts", "repositories", "describe", target[fmt], "--location=" + target["location"], "--format=json"]
        commands["settings-" + fmt] = base + ["beta", "artifacts", "print-settings", fmt, "--repository=" + target[fmt], "--location=" + target["location"]]
    if target["mode"] == "reserved":
        for index, package in enumerate(packages):
            fmt = "apt" if package["format"] == "deb" else "yum"
            commands[f"upload-{index}"] = base + ["artifacts", fmt, "upload", target[fmt], "--location=" + target["location"], "--source=" + package["path"]]
    # All external calls remain operator actions. Never auto-create repositories,
    # IAM bindings, credentials, VMs or production releases from this generator.
    commands = {name: ["python3", str(output / "guardian.py"), "--timeout", "600", "--", *argv] for name, argv in commands.items()}
    (output / "commands.json").write_text(json.dumps(commands, indent=2) + "\n")
    plan = {"audit": ["2504", "2521"], "schema": 1, "external_execution": "not_run", "trial": trial,
            "source_commit": raw["source_commit"], "release_run": raw["release_run"], "target": target,
            "records": records, "packages": packages, "guests": guests, "apt_keyring": keyring,
            "cases": [{"id": name, "status": "not_run"} for name in ("exact-upload", *guests)],
            "sources": [artifact(__file__), artifact(guardian), artifact(HERE / "artifact-registry-rehearsal.txt")]}
    (output / "plan.json").write_text(json.dumps(plan, indent=2) + "\n")
    (output / "prepared-files.json").write_text(json.dumps({p.name: sha(p) for p in sorted(output.iterdir()) if p.is_file()}, indent=2) + "\n")
    return plan


def metadata_value(name):
    class NoRedirect(urllib.request.HTTPRedirectHandler):
        def redirect_request(self, *_args):
            raise ValueError("metadata redirect refused")
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}), NoRedirect())
    request = urllib.request.Request("http://169.254.169.254/computeMetadata/v1/" + name, headers={"Metadata-Flavor": "Google"})
    with opener.open(request, timeout=5) as response:
        require(response.status == 200 and response.headers.get("Metadata-Flavor") == "Google", "unexpected metadata peer")
        value = response.read(4097)
        require(len(value) <= 4096, "metadata exceeds bound")
        return value.decode().strip()


def admit(plan, guest_name, confirmation):
    require(sys.platform == "linux" and confirmation == plan["trial"], "explicit trial confirmation on reserved Linux guest required")
    require(os.geteuid() != 0, "consume as the reserved unprivileged guest user; no installation/root operation")
    guest = plan["guests"][guest_name]
    require(Path("/proc/sys/kernel/random/boot_id").read_text().strip() == guest["boot_id"], "guest boot differs")
    require(sha("/etc/os-release") == guest["os_release_sha256"], "guest OS bytes differ")
    fmt, arch = guest_name.split("-")
    require(platform.machine() == {"amd64": "x86_64", "arm64": "aarch64"}[arch], "native consumer architecture required")
    container = subprocess.run(["systemd-detect-virt", "--container"], capture_output=True, text=True, timeout=10)
    vm = subprocess.run(["systemd-detect-virt", "--vm"], capture_output=True, text=True, timeout=10)
    require(container.returncode == 1 and vm.returncode == 0, "require VM, refuse host/container/unknown")
    require(metadata_value("instance/id") == guest["instance_id"] and metadata_value("project/project-id") == guest["project"]
            and metadata_value("instance/zone").split("/")[-1] == guest["zone"], "Compute Engine target differs")
    client, metadata_tool = ("/usr/bin/apt-get", "/usr/bin/dpkg-deb") if fmt == "deb" else ("/usr/bin/dnf", "/usr/bin/rpm")
    require(sha(client) == guest["client_sha256"] and sha(metadata_tool) == guest["metadata_tool_sha256"], "native client/tool changed")
    return fmt, arch, client, metadata_tool


def command(argv, output, name, *, env=None, cwd=None):
    # The outer copied guardian bounds and owns all descendants. Per-command
    # timeout is shorter; no result from a timed-out client can become acceptance.
    with (output / (name + ".log")).open("wb") as log:
        result = subprocess.run(argv, stdin=subprocess.DEVNULL, stdout=log, stderr=subprocess.STDOUT,
                                env=env, cwd=cwd, timeout=300)
    require(result.returncode == 0, name + " failed; retain complete log")


def verify_downloads(packages, download, metadata_tool):
    files = sorted(download.iterdir())
    require(len(files) == len(packages) and all(p.is_file() and not p.is_symlink() for p in files), "unexpected/missing download files")
    observed = {sha(p): p for p in files}
    require(len(observed) == len(files) and set(observed) == {p["sha256"] for p in packages}, "repository bytes differ from tested upload candidates")
    result = []
    for item in packages:
        path = observed[item["sha256"]]
        if item["format"] == "deb":
            argv = [metadata_tool, "-W", "--showformat=${Package}\\n${Version}\\n${Architecture}", str(path)]
        else:
            argv = [metadata_tool, "-qp", "--qf", "%{NAME}\\n%{VERSION}-%{RELEASE}\\n%{ARCH}", str(path)]
        response = subprocess.run(argv, capture_output=True, text=True, timeout=30, check=True)
        expected_arch = item["architecture"] if item["format"] == "deb" else {"amd64": "x86_64", "arm64": "aarch64"}[item["architecture"]]
        require(response.stdout.strip().splitlines() == [item["name"], item["version"], expected_arch], "downloaded package native metadata mismatch")
        result.append({"name": item["name"], "sha256": item["sha256"], "version": item["version"], "architecture": expected_arch})
    return result


def consume(plan_path, guest_name, confirmation, output):
    plan_path = Path(plan_path).resolve(strict=True)
    prepared = json.loads((plan_path.parent / "prepared-files.json").read_text())
    for name, expected in prepared.items():
        require(Path(name).name == name and sha(plan_path.parent / name) == expected, "prepared acceptance files changed")
    plan = json.loads(plan_path.read_text())
    fmt, arch, client, metadata_tool = admit(plan, guest_name, confirmation)
    output = Path(output).resolve()
    output.mkdir(mode=0o700, parents=True, exist_ok=False)
    report = {"status": "failed", "guest": guest_name, "plan_sha256": sha(plan_path), "external_acceptance": "pending_upload_review"}
    try:
        target = plan["target"]
        packages = [p for p in plan["packages"] if p["format"] == fmt and p["architecture"] == arch]
        require(len(packages) == 2, "require exact CLI and Agent pair")
        downloads = output / "downloads"; downloads.mkdir()
        cache = output / "cache"; cache.mkdir()
        repo = output / "repos"; repo.mkdir()
        home = output / "home"; home.mkdir()
        env = {"PATH": "/usr/bin:/bin", "HOME": str(home), "LC_ALL": "C", "LANG": "C"}
        if fmt == "deb":
            key = plan_path.parent / "apt-keyring.gpg"
            require(sha(key) == plan["apt_keyring"]["sha256"], "APT keyring changed")
            lists = output / "lists"; lists.mkdir(); (lists / "partial").mkdir()
            sources = repo / "candidate.list"
            sources.write_text(f'deb [signed-by={key}] ar+https://{target["location"]}-apt.pkg.dev/projects/{target["project"]} {target["apt"]} main\n')
            config = output / "apt.conf"
            settings = {"Dir::Etc::main": "/dev/null", "Dir::Etc::parts": "-", "Dir::Etc::sourcelist": str(sources),
                        "Dir::Etc::sourceparts": "-", "Dir::State::lists": str(lists), "Dir::Cache": str(cache),
                        "APT::Architecture": arch, "Acquire::Retries": "0", "Acquire::http::Timeout": "30",
                        "Acquire::https::Timeout": "30", "APT::Update::Error-Mode": "any",
                        "Acquire::AllowInsecureRepositories": "false", "APT::Get::AllowUnauthenticated": "false"}
            config.write_text("".join(key + " " + json.dumps(value) + ";\n" for key, value in settings.items()))
            env["APT_CONFIG"] = str(config)
            command([client, "update"], output, "metadata", env=env)
            selectors = [f'{p["name"]}:{arch}={p["version"]}' for p in packages]
            command([client, "download", *selectors], output, "download", env=env, cwd=downloads)
        else:
            plugin_config = Path("/etc/dnf/plugins/artifact-registry.conf")
            require(plugin_config.is_file(), "qualified DNF Artifact Registry plugin configuration missing")
            for line in plugin_config.read_text().splitlines():
                require(not re.match(r"\s*service_account_json\s*=", line),
                        "consumer must use reserved VM identity, not a configured credential file")
            version = subprocess.run([client, "--version"], capture_output=True, text=True, timeout=15, check=True).stdout
            require(re.match(r"4\.", version), "this procedure qualifies DNF4 and its Artifact Registry plugin")
            (repo / "candidate.repo").write_text(f'[{target["yum"]}]\nname=Exact candidate\nbaseurl=https://{target["location"]}-yum.pkg.dev/projects/{target["project"]}/{target["yum"]}\nenabled=1\ngpgcheck=0\nrepo_gpgcheck=0\nsslverify=1\n')
            config = output / "dnf.conf"
            config.write_text(f'[main]\nreposdir={repo}\ncachedir={cache}\npersistdir={output / "persist"}\nlogdir={output}\nplugins=1\nkeepcache=1\ntimeout=30\nretries=0\n')
            base = [client, "--config", str(config), "--disablerepo=*", "--enablerepo=" + target["yum"]]
            command([*base, "--refresh", "makecache"], output, "metadata", env=env)
            rpm_arch = {"amd64": "x86_64", "arm64": "aarch64"}[arch]
            selectors = [f'{p["name"]}-{p["version"]}.{rpm_arch}' for p in packages]
            command([*base, "download", "--downloaddir=" + str(downloads), *selectors], output, "download", env=env)
        report["downloaded"] = verify_downloads(packages, downloads, metadata_tool)
        admit(plan, guest_name, confirmation)
        report["status"] = "passed"
        report["scope"] = "real native repository metadata/download plus exact bytes already covered by native install/upgrade proof; no new installation"
    except BaseException as error:
        report["error"] = type(error).__name__ + ": " + str(error)
        raise
    finally:
        (output / "result.json").write_text(json.dumps(report, indent=2) + "\n")
    return report


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    sub = parser.add_subparsers(dest="action", required=True)
    generation = sub.add_parser("prepare")
    generation.add_argument("inventory", type=Path)
    generation.add_argument("output", type=Path)
    consumption = sub.add_parser("consume")
    consumption.add_argument("plan", type=Path)
    consumption.add_argument("guest", choices=[f"{fmt}-{arch}" for fmt in ("deb", "rpm") for arch in ("amd64", "arm64")])
    consumption.add_argument("--confirm-trial", required=True)
    consumption.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()
    if args.action == "prepare":
        prepare(args.inventory, args.output)
    else:
        consume(args.plan, args.guest, args.confirm_trial, args.output)
