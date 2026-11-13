#!/usr/bin/env python3
"""Prepare audit2341 physical OTA/reboot/rollback evidence; never operate a device."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import shlex
import subprocess
import tarfile
from urllib.parse import urlsplit

ROOT = Path(__file__).resolve().parents[3]
UPDATER_REVISION = "1f160d407833783755af153a64f1b386e8f922ac"


def line(value):
    if not isinstance(value, str) or not value or any(ord(c) < 32 or ord(c) == 127 for c in value):
        raise ValueError("require a nonempty single-line value")
    return value


def digest(value):
    if not isinstance(value, str) or not re.fullmatch(r"[0-9a-f]{64}", value):
        raise ValueError("require a complete SHA256")
    return value


def record(value):
    path = Path(line(value)).resolve(strict=True)
    if not path.is_file() or not 0 < path.stat().st_size <= 64 * 1024**3:
        raise ValueError("require a nonempty bounded regular file")
    h = hashlib.sha256()
    with path.open("rb") as source:
        for chunk in iter(lambda: source.read(4 * 1024**2), b""):
            h.update(chunk)
    return {"path": str(path), "sha256": h.hexdigest(), "size_bytes": path.stat().st_size}


def manifest(path):
    # Inspect the exact plain tar format at the pinned updater revision. Never
    # extract or mount the image. Refuse extended headers rather than allocate
    # attacker-sized PAX records. The producer emits these two regular members.
    with Path(path).open("rb") as source:
        def header():
            raw = source.read(512)
            if len(raw) != 512:
                raise ValueError("truncated artifact header")
            return tarfile.TarInfo.frombuf(raw, "utf-8", "strict")
        first = header()
        if not first.isfile() or first.name != "manifest.json" or not 0 < first.size <= 4 * 1024**2:
            raise ValueError("require bounded manifest.json as first regular member")
        data = json.loads(source.read(first.size))
        source.read((-first.size) % 512)
        payload = header()
        p = data["payload"]
        if (data.get("format_version") != 1 or type(data.get("format_version")) is not int
                or p.get("compression") != "zstd" or not payload.isfile()
                or payload.name != p["name"] or "/" in payload.name or "\\" in payload.name
                or not 0 < payload.size <= 64 * 1024**3):
            raise ValueError("require the pinned v1 regular zstd payload")
        digest(p["sha256"])
        expected = digest(p["compressed_sha256"])
        h = hashlib.sha256()
        remaining = payload.size
        while remaining:
            chunk = source.read(min(remaining, 4 * 1024**2))
            if not chunk:
                raise ValueError("truncated payload")
            h.update(chunk)
            remaining -= len(chunk)
        if h.hexdigest() != expected:
            raise ValueError("stored payload hash mismatch")
        source.read((-payload.size) % 512)
        if source.read(1024) != bytes(1024):
            raise ValueError("unexpected member after payload")
    return data


def generate(inventory, output):
    data = json.loads(Path(inventory).read_text())
    def no_secrets(value):
        if isinstance(value, dict):
            for key, child in value.items():
                if any(word in key.lower() for word in ("password", "passphrase", "private_key", "token")):
                    raise ValueError("do not put credentials in the inventory")
                no_secrets(child)
        elif isinstance(value, list):
            for child in value:
                no_secrets(child)
    no_secrets(data)
    board = data["board"]
    devices = {"jetson-orin-nano": "tegra234", "jetson-agx-orin": "tegra234", "jetson-agx-thor": "tegra264"}
    if devices.get(board["device_type"]) != board["soc"] or board["storage"] not in ("nvme", "emmc"):
        raise ValueError("require an exact Orin/Thor board and storage identity")
    for key in ("serial", "device_tree_serial", "ecid", "owner", "carrier", "root_disk_serial", "firmware", "l4t", "address"):
        line(board[key])
    if board["address"].startswith(("-", "cloud:", "mesh:", "vm:")):
        raise ValueError("require a direct reserved physical management endpoint")
    if board["initial_slot"] not in ("a", "b"):
        raise ValueError("pin the observed initial slot")
    for key in ("app_partuuid", "app_b_partuuid", "esp_partuuid"):
        if not re.fullmatch(r"[a-fA-F0-9]{8}(?:-[a-fA-F0-9]{4}){3}-[a-fA-F0-9]{12}", board[key]):
            raise ValueError("require exact GPT partition UUIDs")
        board[key]=board[key].lower()
    if len({board[k].lower() for k in ("app_partuuid", "app_b_partuuid", "esp_partuuid")}) != 3:
        raise ValueError("root slots and ESP must be distinct")
    for key, maximum in (("redundancy", 1), ("retries", 3)):
        setting = board[key]
        if type(setting["attributes"]) is not int or setting["attributes"] not in (6, 7) or type(setting["value"]) is not int or not 0 <= setting["value"] <= maximum:
            raise ValueError("require observed EFI attributes and bounded values")
    for key, low, high in (("install_seconds", 300, 3600), ("boot_seconds", 60, 900)):
        if type(data[key]) is not int or not low <= data[key] <= high:
            raise ValueError("require bounded installation and boot windows")
    for key in ("cli_revision", "agent_revision"):
        if not re.fullmatch(r"[a-f0-9]{40}", data[key]):
            raise ValueError("pin full source commits")
    trial = data["trial"]
    if not re.fullmatch(r"[a-f0-9]{32}", trial):
        raise ValueError("require an independent random lab trial identifier")
    records = {key: record(data[key]) for key in ("cli_binary", "agent_binary", "updater_binary", "identity_record", "recovery_record", "authorization_record")}
    if not os.access(records["cli_binary"]["path"], os.X_OK):
        raise ValueError("CLI must be executable")
    source = Path(data["updater_source"]).resolve(strict=True)
    def git(*args):
        return subprocess.check_output(["git", *args], cwd=source, text=True, timeout=10).strip()
    if git("rev-parse", "HEAD") != UPDATER_REVISION or git("status", "--porcelain", "--untracked-files=no"):
        raise ValueError("require the clean exact shipped updater source")
    hook = "#!/bin/sh\nprintf '%s\\n' 'CI2341_HEALTH_REJECT " + trial + "' >&2\nexit 1\n"
    hook_path = "etc/wendyos-update/health.d/99-ci-2341-" + trial
    releases = {}
    for role in ("normal", "reject"):
        supplied = data["artifacts"][role]
        archive = record(supplied["archive"])
        descriptor_record = record(supplied["descriptor"])
        descriptor = json.loads(Path(descriptor_record["path"]).read_text())
        url = urlsplit(line(descriptor["url"]))
        if url.scheme != "https" or not url.hostname or url.username or url.password or url.query or url.fragment or not url.path.endswith(".wendy") or any(v in url.path.split("/") for v in ("latest", "nightly")):
            raise ValueError("require immutable credential-free HTTPS .wendy URLs")
        if descriptor.get("sha256") != archive["sha256"] or type(descriptor.get("size_bytes")) is not int or descriptor["size_bytes"] != archive["size_bytes"]:
            raise ValueError("artifact differs from published digest/size")
        m = manifest(archive["path"])
        if m.get("compatible_devices") != [board["device_type"]] or descriptor.get("storage") != board["storage"]:
            raise ValueError("artifact must match only this board/storage")
        for key in ("artifact_name", "artifact_version"):
            if line(m[key]) != descriptor[key] or m[key] in ("latest", "nightly"):
                raise ValueError("manifest and producer descriptor must agree")
        inspection_record = record(supplied["payload_inspection"])
        inspection = json.loads(Path(inspection_record["path"]).read_text())
        expected = {"artifact_sha256": archive["sha256"], "rootfs_sha256": m["payload"]["sha256"], "agent_sha256": records["agent_binary"]["sha256"], "updater_sha256": records["updater_binary"]["sha256"], "bootloader_marker_present": False,
                    "health_hook_path": hook_path if role == "reject" else None, "health_hook_mode": "0755" if role == "reject" else None,
                    "health_hook_sha256": hashlib.sha256(hook.encode()).hexdigest() if role == "reject" else None}
        if any(inspection.get(k) != v for k, v in expected.items()) or inspection.get("bootloader_marker_present") is not False:
            raise ValueError("require reviewed rootfs inspection bound to exact image/binaries/hook")
        line(inspection["reviewer"])
        releases[role] = {"archive": archive, "descriptor_record": descriptor_record, "descriptor": descriptor, "manifest": m, "payload_inspection": inspection_record}
    if releases["normal"]["archive"]["sha256"] == releases["reject"]["archive"]["sha256"] or releases["normal"]["manifest"]["artifact_version"] == releases["reject"]["manifest"]["artifact_version"]:
        raise ValueError("normal and rejecting images need distinct bytes and versions")
    out = Path(output).resolve()
    out.mkdir(mode=0o700, parents=True, exist_ok=False)
    cli = records["cli_binary"]["path"]
    target = ["--device", board["address"]]
    commands = {"console_install_normal": ["/usr/bin/wendyos-update", "install", releases["normal"]["descriptor"]["url"]], "console_rollback": ["/usr/bin/wendyos-update", "rollback"], "console_reboot": ["systemctl", "reboot"], "info": [cli, "--json", "device", "info", *target], "status": [cli, "--json", "os", "update-status", *target]}
    for role in releases:
        commands["update_"+role] = [cli, "device", "update", *target, "--binary", records["agent_binary"]["path"], "--artifact-url", releases[role]["descriptor"]["url"], "--yes"]
    (out/"commands.json").write_text(json.dumps(commands, indent=2)+"\n")
    (out/"health-reject.sh").write_text(hook)
    observer = Path(__file__).with_name("jetson-ab-snapshot.py")
    (out/observer.name).write_text(observer.read_text())
    (out/"snapshot-config.json").write_text(json.dumps({"board":board,"updater_sha256":records["updater_binary"]["sha256"],"agent_sha256":records["agent_binary"]["sha256"]},indent=2)+"\n")
    guide = Path(__file__).with_suffix(".txt").read_text()
    for key, value in {"@SERIAL@":board["serial"], "@INSTALL@":str(data["install_seconds"]), "@BOOT@":str(data["boot_seconds"]), "@TRIAL@":trial, "@HOOK@":hook_path,"@ORIGIN@":board["initial_slot"],"@TARGET@":"b" if board["initial_slot"]=="a" else "a"}.items():
        guide = guide.replace(key,value)
    guide += "\nExact commands, not executed by generation (console_* only on the reserved board; other commands on the host):\n" + "\n".join(k+": "+shlex.join(v) for k,v in commands.items())+"\n"
    (out/"procedure.txt").write_text(guide)
    sources = [Path(__file__), Path(__file__).with_suffix(".txt"), observer, ROOT/"go/internal/agent/services/tegra_redundancy.go", ROOT/"go/internal/agent/services/wendyos_backend.go", ROOT/"go/internal/agent/services/wendyos_readiness.go", ROOT/"go/internal/agent/oshealth/gate.go", ROOT/"go/internal/cli/commands/device.go"]
    sources += [source/p for p in ("cmd/wendyos-update/main.go", "internal/connector/tegrauefi/tegrauefi.go", "internal/connector/tegrauefi/swap-slot.go", "internal/engine/commit.go", "docs/cli-contract.md", "docs/state-schema.md")]
    evidence = {"audit_entries":["2341"],"hardware_execution":"not_run","inventory":data,"records":records,"releases":releases,"updater_revision":UPDATER_REVISION,"sources":[record(str(p)) for p in sources],
                "cases":[{"id":case,"status":"not_run","observations":[],"reviewer":None} for case in ("identity_and_baseline", "legacy_preparation_or_boot_chain_exception", "agent_ota_commit", "second_boot_persistence", "health_rejection_and_actual_rollback", "healthy_origin_after_rollback", "pre_reboot_updater_rollback", "recovery")],
                "pending_cases":["stale pending deployment on volatile-zero Orin is not fixed by readiness delegation", "capsule/bootloader-update artifacts and physical power-loss durability", "older updater/Mender combinations require their own exact firmware/image qualification"],
                "generated":[record(str(out/name)) for name in ("commands.json","health-reject.sh","jetson-ab-snapshot.py","snapshot-config.json","procedure.txt")],
                "generation_scope":"Hashes whole archives and stored payload bytes; does not decompress/mount rootfs or verify human payload inspection, operate a device, or claim any physical result."}
    (out/"evidence.json").write_text(json.dumps(evidence,indent=2)+"\n")
    return evidence


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("inventory", type=Path)
    parser.add_argument("output", type=Path)
    args = parser.parse_args()
    generate(args.inventory,args.output)
