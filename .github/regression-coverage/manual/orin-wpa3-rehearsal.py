#!/usr/bin/env python3
"""Prepare physical WPA3 checks for audit 2565. Never contacts a device.

Inventory requires cli_binary, agent_binary, agent_source_commit, recovery_record,
ap_config_record, and boards. Supply one WendyOS jetson-orin-nano and one
jetson-agx-orin with selector, serial, owner, device_info, interface, station_mac,
wpa3_ssid, mixed_ssid, gateway, wpa3_bssid and mixed_bssid. Records contain no Wi-Fi password.
Use dedicated nonproduction boards and an AP capable of reporting negotiated AKM.
"""
import argparse
import importlib.util
import ipaddress
import json
import os
from pathlib import Path
import re
import shlex
import subprocess

SPEC = importlib.util.spec_from_file_location("gpu_helpers", Path(__file__).with_name("jetson-gpu-rehearsal.py"))
HELPERS = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(HELPERS)
KINDS = {"jetson-orin-nano", "jetson-agx-orin"}
PHASES = ("fresh-sae", "saved-sae-forced-open", "startup-repair", "reboot-sae",
          "fresh-mixed-psk", "saved-sae-on-mixed", "wrong-password-recovery")


def generate(inventory, output):
    raw = json.loads(inventory.read_text())
    artifacts = {key: HELPERS.artifact(raw[key]) for key in
                 ("cli_binary", "agent_binary", "recovery_record", "ap_config_record")}
    cli = artifacts["cli_binary"]["path"]
    if not os.access(cli, os.X_OK):
        raise ValueError("CLI must be executable")
    source = HELPERS.line(raw["agent_source_commit"], "agent_source_commit")
    if not re.fullmatch(r"[0-9a-f]{40}", source):
        raise ValueError("Agent source must be a full commit SHA")
    boards = []
    for item in raw["boards"]:
        board = {key: HELPERS.line(item[key], key) for key in
                 ("selector", "serial", "owner", "interface", "station_mac", "wpa3_ssid", "mixed_ssid", "gateway", "wpa3_bssid", "mixed_bssid")}
        info_record = HELPERS.artifact(item["device_info"])
        info = json.loads(Path(info_record["path"]).read_text())
        if info.get("deviceType") not in KINDS or info.get("os") != "wendyos" or info.get("cpuArchitecture") not in ("arm64", "aarch64"):
            raise ValueError("requires the two affected WendyOS Orin board variants")
        if board["selector"].startswith("-") or not re.fullmatch(r"[A-Za-z0-9_.-]{1,15}", board["interface"]):
            raise ValueError("invalid selector or interface")
        for field in ("station_mac", "wpa3_bssid", "mixed_bssid"):
            if not re.fullmatch(r"(?:[0-9a-fA-F]{2}:){5}[0-9a-fA-F]{2}", board[field]):
                raise ValueError("an exact station MAC and WPA3 AP BSSID are required")
        ipaddress.ip_address(board["gateway"])
        if board["wpa3_ssid"] == board["mixed_ssid"] or any(len(board[key].encode()) > 32 for key in ("wpa3_ssid", "mixed_ssid")):
            raise ValueError("use distinct valid dedicated WPA3 and mixed SSIDs")
        for key in ("version", "osVersion", "jetpackVersion"):
            HELPERS.line(info.get(key), key)
        board.update(kind=info["deviceType"], info=info, info_record=info_record)
        boards.append(board)
    if len(boards) != 2 or {b["kind"] for b in boards} != KINDS:
        raise ValueError("require exactly one Orin Nano and one AGX Orin")
    for key in ("serial", "selector", "station_mac"):
        if len({b[key].lower() for b in boards}) != 2:
            raise ValueError("boards must have distinct " + key)
    head = subprocess.check_output(["git", "rev-parse", "HEAD"], cwd=HELPERS.ROOT, text=True).strip()
    dirty = bool(subprocess.check_output(["git", "status", "--porcelain"], cwd=HELPERS.ROOT, text=True).strip())
    out = output.resolve()
    out.mkdir(mode=0o700, parents=True, exist_ok=False)
    commands, cases = {}, []
    for board in boards:
        kind, selector = board["kind"], board["selector"]
        commands[kind] = {
            "info": [cli, "--json", "device", "info", "--device", selector],
            "list": [cli, "--json", "device", "wifi", "list", "--device", selector],
            "status": [cli, "--json", "device", "wifi", "status", "--device", selector],
            "connect-sae": [cli, "device", "wifi", "connect", "--device", selector, "--ssid", board["wpa3_ssid"]],
            "connect-mixed": [cli, "device", "wifi", "connect", "--device", selector, "--ssid", board["mixed_ssid"]],
            "forget-sae": [cli, "device", "wifi", "forget", "--device", selector, "--ssid", board["wpa3_ssid"]],
            "forget-mixed": [cli, "device", "wifi", "forget", "--device", selector, "--ssid", board["mixed_ssid"]],
        }
        for phase in PHASES:
            cases.append({"id": kind + "/" + phase, "board": kind, "phase": phase, "status": "not_run",
                          "start_utc": None, "end_utc": None, "profile_uuid": None,
                          "snapshot_before": None, "snapshot_after": None, "ap_association_record": None,
                          "interface_ping_record": None, "operator": None, "result": None})
        snapshot = f'''#!/bin/sh
# Read-only capture on the physical board, with an operator-selected fixture UUID.
set -eu
test "$#" = 1 || {{ echo 'usage: sh snapshot.sh FIXTURE_PROFILE_UUID' >&2; exit 2; }}
case "$1" in *[!0-9a-fA-F-]*|'') echo 'invalid profile UUID' >&2; exit 2;; esac
test "${{#1}}" = 36
wifi_iface={shlex.quote(board['interface'])}
date -u
tr '\\000' '\\n' < /proc/device-tree/model
uname -r
cat /etc/os-release
cat "/sys/class/net/$wifi_iface/address"
readlink -f "/sys/class/net/$wifi_iface/device/driver"
timeout 15 nmcli -g connection.uuid,connection.interface-name,802-11-wireless.ssid,802-11-wireless-security.key-mgmt,802-11-wireless-security.auth-alg,802-11-wireless-security.pmf connection show uuid "$1"
timeout 15 nmcli -f GENERAL,IP4,IP6 device show "$wifi_iface"
timeout 15 iw dev "$wifi_iface" link
timeout 30 ping -I "$wifi_iface" -c 5 {shlex.quote(board['gateway'])}
'''
        HELPERS.write(out / (kind + "-snapshot.sh"), snapshot)
    evidence = {"audit_entries": ["2565"], "source_head": head, "dirty_preview": dirty,
                "agent_source_commit": source, "hardware_execution": "not_run", "boards": boards,
                "artifacts": list(artifacts.values()), "cases": cases, "cleanup": None}
    HELPERS.write(out / "evidence.json", json.dumps(evidence, indent=2) + "\n")
    HELPERS.write(out / "commands.json", json.dumps(commands, indent=2) + "\n")
    invocations = "\n".join(kind + " " + stage + ": " + shlex.join(command)
                            for kind, groups in commands.items() for stage, command in groups.items())
    HELPERS.write(out / "procedure.txt", f'''Physical Orin WPA3 association, audit 2565
Source {head}; Agent source {source}; dirty preview {dirty}.
All fourteen cases are NOT RUN. Generation runs no device commands.

Prerequisites and identity
1. Regenerate from a clean committed checkout. Reserve both exact boards with
   their named owners. Keep a physical console and independent wired management
   link throughout; Wi-Fi under test must not be the only recovery/control link.
   Match physical serial, live device info, interface MAC and AP BSSID to the
   inventory. Record kernel, radio/firmware model/version and JetPack version.
   Verify the installed candidate Agent binary hash against the recorded local
   artifact, using the executable of its actual service PID. Record the CLI hash
   and build provenance. A matching version string alone is insufficient.
2. Review the hashed recovery record and AP configuration record. Use a dedicated
   WPA3-Personal SAE-only SSID with PMF required, plus a separate WPA2/WPA3
   transition SSID. Record AP model/firmware, channel/band, BSSID and security
   settings. Keep passphrases out of inventory, commands, logs and shell history.
   The AP must report the negotiated AKM and station MAC. Desired nmcli profile
   settings alone cannot prove SAE association. No usable AP evidence means
   not_run, not pass. Use lab credentials with no authority elsewhere.
3. Inventory saved Wi-Fi profiles on each board. Obtain the owner's approval to
   change only the dedicated fixture SSIDs/profiles and to restart its Agent and
   reboot later. Retain original UUIDs, priorities, autoconnect and active wired
   connection details. Any backup containing credentials stays private on the
   board under root-only permissions and is never added to this evidence bundle.
   Never delete or rewrite unrelated network profiles.

For each board, use its exact generated commands below
4. Run info/list/status and capture UTC plus selected identity. Run connect in a
   real terminal, without --password, without piping stdin and without a terminal
   transcript recorder. Decline host-keychain lookup and type the lab passphrase
   at the hidden prompt. Never use --allow-unverified-device. Each connect has a
   90-second observation budget; if it stalls, interrupt once, inspect status and
   the local console before retrying. Retain the exit status, elapsed time and
   sanitized error category, never the entered password.
   Before each reconnect, use nmcli connection down uuid UUID at the console
   for only the active fixture UUID, and observe its AP disassociation. Preserve
   the wired control link. An already-working cached association is not a test.
5. After each success, record the active fixture UUID via nmcli and run that
   board's generated read-only snapshot script at its physical console. Capture
   stdout/stderr on the operator host. Require the exact SSID/BSSID, activated
   interface, assigned address and 5/5 ping replies bound to that Wi-Fi interface.
   The wired recovery link cannot satisfy this check. Save the AP's association
   record for the same station MAC and time, including actual AKM, cipher and PMF.
   Require a fresh association/reassociation event after the current connect,
   matching wpa3_bssid or mixed_bssid for that phase, not an older status record.

Seven cases per board
6. fresh-sae: confirm no saved profile exists for the dedicated WPA3 SSID, then
   connect-sae. Require key-mgmt=sae, auth-alg unset, AP-confirmed SAE/PMF and step5.
7. saved-sae-forced-open: retain that UUID. At the maintenance console set ONLY
   its 802-11-wireless-security.auth-alg to open using nmcli connection modify uuid
   UUID 802-11-wireless-security.auth-alg open. Record the field before connecting.
   Run connect-sae through Wendy. Require that same saved UUID, cleared auth-alg,
   no duplicate fixture profiles, successful SAE association and step5.
8. startup-repair: this is a separate, explicit test-reset boundary. Stop the
   candidate Agent service at the maintenance console. Record its actual unit
   name from the running installation; do not assume a different user service.
   The repair acts on ALL saved WPA profiles, so proceed only on a reserved board
   whose complete saved Wi-Fi inventory contains exclusively the owner's fixture
   profiles. Otherwise leave this case not_run. Set the fixture SAE profile's
   auth-alg=open again. Save the original existence/metadata of
   /var/lib/wendy/wifi-auth-alg-cleared, then move only that marker to an owned
   root-only backup path. Start the same Agent service. Within60 seconds require
   auth-alg to become unset and the marker to reappear, then connect-sae and step5.
   Do not directly edit the marker while the Agent is running. Never reset other
   Agent state. Any incomplete repair or timeout is a failure to investigate.
9. reboot-sae: after explicit owner approval, reboot this reserved board using its
   normal maintenance procedure. Keep console/power observation. Do not reflash
   or reinstall on a timeout. Within the recovery record's approved boot window,
   require the same board/Agent hash, retained UUID and unset auth-alg. Run the
   connect/status/snapshot checks again and verify AP-confirmed SAE after reboot.
10. fresh-mixed-psk: with no fixture profile for the transition SSID, connect-mixed.
    Require key-mgmt=wpa-psk with auth-alg unset, AP-confirmed WPA2-PSK, and step5.
11. saved-sae-on-mixed: change ONLY that fixture UUID's key-mgmt to sae and leave
    auth-alg unset, then successfully activate it once using nmcli --ask at the
    physical console: nmcli --ask connection up uuid UUID ifname IFACE. Confirm
    SAE, then disconnect only that fixture UUID. Run connect-mixed through Wendy.
    Require the same UUID,
    key-mgmt still sae and AP-confirmed SAE with fresh traffic. This verifies a
    working saved SAE profile is preserved on a mixed AP.
12. wrong-password-recovery: disconnect the fixture Wi-Fi connection at the
    console, retaining wired access. Run connect-sae with an intentionally wrong
    password entered at the hidden prompt. Require nonzero exit and no false
    Connected success. Confirm no SAE association was established. Then run it
    again with the correct lab password and require all step5 checks. A cached
    working association cannot stand in for either observation.

Failure comparison and evidence
13. Where the old bug reproduces on these exact radios, an owner-approved previous
    Agent can be exercised on the same reserved board/AP first, following the
    recovery record and recording both executable hashes. Do not downgrade an
    unreserved device. An old build's failure is useful evidence, but missing
    hardware evidence never becomes a pass based only on the existing fake-nmcli
    unit tests. Record failed attempts and recovered attempts separately.
14. Complete each case's times, profile UUID, snapshots, AP negotiation record,
    Wi-Fi-bound ping result and operator in evidence.json. Every required field
    must describe an actual observation. Keep all untouched cases not_run.

Cleanup and recovery
15. On each board, forget only the two dedicated fixture SSIDs through Wendy after
    verifying their UUIDs are owned by this run. Restore prior fixture/AP settings,
    priorities and the original wired control path. At the console stop the Agent
    before restoring the saved one-shot marker state, then start the same unit.
    Restore only owner-approved saved profiles from private backups if this test
    changed them. Remove private lab credential backups through the owner's
    normal process after verifying recovery; record cleanup and final info/status.
    Do not delete unrelated profiles, clear all NetworkManager state or repeat a
    reboot/update after losing contact. Keep the physical console and follow the
    recorded recovery procedure with the owner instead.

Exact CLI invocations, executed manually at their named stages
{invocations}
''')
    return evidence


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("inventory", type=Path)
    parser.add_argument("output", type=Path)
    args = parser.parse_args()
    result = generate(args.inventory, args.output)
    print(json.dumps({"hardware_execution": result["hardware_execution"], "cases": len(result["cases"])}))
