#!/usr/bin/env python3
"""Generate physical first-boot acceptance for 49fa7f963/2805, offline only.

Inventory contains cli_binary, agent_binary, cli_source_commit, agent_source_commit,
release_descriptor, release_archive, identity_record, recovery_record, ap_record;
board {device, storage, serial, owner, interface, station_mac, bssid, ssid,
gateway, management_address, device_name, boot_timeout_seconds}.
The release descriptor is the existing Orin seed fixture JSON schema. Only the
operator's separately authorized physical procedure writes or boots a board.
"""
import argparse
import hashlib
import ipaddress
import json
import os
from pathlib import Path
import re
import shlex

ROOT = Path(__file__).resolve().parents[3]


def line(value):
    if not isinstance(value, str) or not value or any(ord(c) < 32 or ord(c) == 127 for c in value):
        raise ValueError("require a nonempty single-line identity")
    return value


def artifact(value):
    path = Path(line(value)).resolve(strict=True)
    if not path.is_file() or path.stat().st_size <= 0:
        raise ValueError("artifact must be a nonempty file")
    digest = hashlib.sha256()
    with path.open("rb") as stream:
        for chunk in iter(lambda: stream.read(4 * 1024 * 1024), b""):
            digest.update(chunk)
    return {"path": str(path), "sha256": digest.hexdigest(), "size_bytes": path.stat().st_size}


def generate(inventory, output):
    data = json.loads(Path(inventory).read_text())
    def reject_secrets(value):
        if isinstance(value, dict):
            for key, child in value.items():
                if any(word in key.lower() for word in ("password", "passphrase", "eabhmac", "private_key")):
                    raise ValueError("inventory must not contain credentials")
                reject_secrets(child)
        elif isinstance(value, list):
            for child in value:
                reject_secrets(child)
    reject_secrets(data)
    board = data["board"]
    for key in ("serial", "owner", "management_address"):
        line(board[key])
    if board["management_address"].startswith(("-", "cloud:", "mesh:", "vm:", "wendy-lite:")):
        raise ValueError("first boot needs an explicit direct wired management endpoint")
    allowed = {"jetson-orin-nano": ("nvme",), "jetson-agx-orin": ("nvme", "emmc")}
    if board["storage"] not in allowed.get(board["device"], ()):
        raise ValueError("require a supported physical Orin developer-kit storage pair")
    if not re.fullmatch(r"[a-z][a-z0-9-]{2,54}", board["device_name"]) or board["device_name"].endswith("-"):
        raise ValueError("require a unique valid first-boot device name")
    if not re.fullmatch(r"[A-Za-z0-9_.-]{1,15}", board["interface"]):
        raise ValueError("require an exact Wi-Fi interface")
    for key in ("station_mac", "bssid"):
        if not re.fullmatch(r"(?:[a-fA-F0-9]{2}:){5}[a-fA-F0-9]{2}", board[key]):
            raise ValueError("require observed station/AP MACs")
    ipaddress.ip_address(board["gateway"])
    if len(line(board["ssid"]).encode()) > 32 or any(c in board["ssid"] for c in ",\\"):
        raise ValueError("fixture SSID must fit 32 bytes without comma/backslash")
    if type(board["boot_timeout_seconds"]) is not int or not 60 <= board["boot_timeout_seconds"] <= 900:
        raise ValueError("predeclare a bounded 60..900 second boot window")
    for key in ("cli_source_commit", "agent_source_commit"):
        if not re.fullmatch(r"[a-f0-9]{40}", data[key]):
            raise ValueError("pin exact source revisions")
    records = {key: artifact(data[key]) for key in ("cli_binary", "agent_binary", "release_descriptor", "release_archive", "identity_record", "recovery_record", "ap_record")}
    if not os.access(records["cli_binary"]["path"], os.X_OK):
        raise ValueError("CLI must be executable")
    release = json.loads(Path(records["release_descriptor"]["path"]).read_text())
    if any(release.get(k) != board[k] for k in ("device", "storage")):
        raise ValueError("release does not match this exact board/storage")
    if not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9._-]*", release["version"]) or release["version"] in ("latest", "nightly"):
        raise ValueError("use an immutable release version")
    if not line(release["url"]).startswith("https://") or "flashpack.tar.zst" not in release["url"]:
        raise ValueError("require the actual Orin release flashpack URL")
    archive = records["release_archive"]
    if release.get("sha256") != archive["sha256"] or type(release.get("size_bytes")) is not int or release["size_bytes"] != archive["size_bytes"]:
        raise ValueError("local release bytes do not match published descriptor")
    out = Path(output).resolve()
    out.mkdir(mode=0o700, parents=True, exist_ok=False)
    cli = records["cli_binary"]["path"]
    selection = ["--device-type", board["device"], "--carrier", "developer-kit", "--storage", board["storage"], "--version", release["version"]]
    commands = {
        "plan": [cli, "install", "plan", *selection],
        "job_start_prefix": [cli, "install", "jobs", "start", *selection, "--device-name", board["device_name"]],
        "pre_enroll_fresh_install": [cli, "install", "--device-type", board["device"], "--storage", board["storage"], "--version", release["version"], "--binary", records["agent_binary"]["path"], "--device-name", board["device_name"], "--no-wifi", "--pre-enroll"],
        "wifi_setup": [cli, "device", "wifi", "connect", "--device", board["management_address"], "--ssid", board["ssid"], "--allow-unverified-device"],
        "wifi_status": [cli, "--json", "device", "wifi", "status", "--device", board["management_address"]],
        "verify": [cli, "install", "verify", "--address", board["management_address"], "--expected-device-type", board["device"], "--expected-os-version", release["version"], "--timeout", "30s"],
        "discover": [cli, "--json", "discover"],
    }
    (out / "commands.json").write_text(json.dumps(commands, indent=2)+"\n")
    # The helper only plans a job. Password input is never written into the plan
    # or shell history; the actual CLI still necessarily receives its --wifi arg.
    helper = '''#!/usr/bin/env python3
import getpass, json, subprocess, sys
from pathlib import Path
if not sys.stdin.isatty():
    raise SystemExit("use a private local terminal")
commands = json.loads(Path(__file__).with_name("commands.json").read_text())
password = getpass.getpass("Dedicated lab Wi-Fi password: ")
if not password or any(c in password for c in ",\\\\\\r\\n") or password != password.strip():
    raise SystemExit("use a nonempty lab password without comma/backslash or surrounding whitespace")
argv = commands["job_start_prefix"] + ["--wifi", "ssid=" + SSID + ",password=" + password]
try:
    result = subprocess.run(argv, timeout=60, check=False)
except subprocess.TimeoutExpired:
    raise SystemExit("job planning timed out; inspect jobs before retrying")
raise SystemExit(result.returncode)
'''.replace("SSID", repr(board["ssid"]))
    (out / "start-seeded-job.py").write_text(helper)
    snapshot = f'''#!/bin/sh
# Run read-only on the reserved physical board. Never print a seed/password/key.
set -eu
test "$#" = 1 || {{ echo 'usage: sh board-snapshot.sh OBSERVED_PROFILE_UUID' >&2; exit 2; }}
case "$1" in *[!a-fA-F0-9-]*|'') echo 'invalid profile UUID' >&2; exit 2;; esac
test "${{#1}}" = 36
date -u
cat /proc/sys/kernel/random/boot_id
uname -a
cat /etc/os-release
hostname
cat /etc/hostname
cat /etc/wendyos/device-name
cat /sys/class/net/{board['interface']}/address
timeout 15 nmcli -g connection.uuid,connection.interface-name,connection.autoconnect,802-11-wireless.ssid,802-11-wireless-security.key-mgmt,802-11-wireless-security.auth-alg,802-11-wireless-security.pmf connection show uuid "$1"
timeout 15 nmcli -f GENERAL,IP4,IP6 device show {shlex.quote(board['interface'])}
timeout 15 iw dev {shlex.quote(board['interface'])} link
timeout 30 ping -I {shlex.quote(board['interface'])} -c 5 {shlex.quote(board['gateway'])}
for file in /config/wendy.conf /config/acme-enrollment.json /etc/wendy-agent/acme-first-boot-attempt.json /etc/wendy-agent/provisioning.json; do
  if test -f "$file"; then printf '%s present\\n' "$file"; else printf '%s absent\\n' "$file"; fi
done
'''
    (out / "board-snapshot.sh").write_text(snapshot)
    guide = Path(__file__).with_suffix(".txt").read_text()
    for key, value in {"@SERIAL@":board["serial"], "@NAME@":board["device_name"], "@SECONDS@":str(board["boot_timeout_seconds"]), "@SSID@":board["ssid"]}.items():
        guide = guide.replace(key, value)
    guide += "\nExact commands, not executed during generation:\n"+"\n".join(key+": "+shlex.join(value) for key,value in commands.items())+"\n"
    (out / "procedure.txt").write_text(guide)
    sources = [Path(__file__), Path(__file__).with_suffix(".txt"), ROOT/"go/internal/agent/configpartition/apply.go", ROOT/"go/internal/agent/services/provisioning_acme_file.go", ROOT/"go/internal/cli/commands/os_install_jobs.go", ROOT/"go/internal/cli/commands/wifi.go"]
    evidence = {"audit_entries":["49fa7f963","2805"],"hardware_execution":"not_run","inventory":data,"release":release,"records":records,"sources":[artifact(str(p)) for p in sources],
        "cases":[{"id":case,"status":"not_run","evidence":[],"reviewer":None} for case in ("target_and_seed_provenance","jobs_seed_first_boot","hostname_wifi_second_boot","offline_pre_enrolled_first_boot","wifi_before_enrollment","enrollment_second_boot","recovery")],
        "scope":"physical USB recovery on this exact board/storage only; media-write and other boards require separate execution"}
    (out/"evidence.json").write_text(json.dumps(evidence,indent=2)+"\n")
    return evidence


if __name__ == "__main__":
    parser=argparse.ArgumentParser(description=__doc__)
    parser.add_argument("inventory",type=Path)
    parser.add_argument("output",type=Path)
    args=parser.parse_args()
    generate(args.inventory,args.output)
