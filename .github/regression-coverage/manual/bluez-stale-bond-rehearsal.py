#!/usr/bin/env python3
"""Generate audit1595's reserved BlueZ/HID procedure; never operate hardware.

Inventory: cli_binary, agent_binary, agent_source_commit; host {serial, owner,
management_selector, os_release, kernel, bluez_version, architecture, adapter_mac,
adapter_index}; peripheral {model, serial, address, profile, action_labels}; records
{host_identity, adapter_identity, peripheral_identity, original_state, recovery,
peripheral_bond_reset}. Record values are existing file paths. profile is le-hid
or classic-hid. action_labels names at least two safe physical keys/buttons.

peripheral_bond_reset is owner-reviewed JSON with peripheral_serial, model,
owner_approved:true, scope:"peripheral-bond-only", vendor_url (HTTPS), slot,
max_seconds (1..120), steps (nonempty literal instructions), expected_confirmation.
A reset that also resets the host/controller is not an eligible stale-bond setup.
"""
import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import secrets
import shlex
import shutil

ROOT = Path(__file__).resolve().parents[3]


def line(value):
    if not isinstance(value, str) or not value.strip() or any(ord(c) < 32 or ord(c) == 127 for c in value):
        raise ValueError("identity and recipe fields require nonempty single lines")
    return value


def artifact(value):
    path = Path(line(value)).resolve(strict=True)
    if not path.is_file() or not path.stat().st_size:
        raise ValueError("record must be a nonempty file")
    digest = hashlib.sha256()
    with path.open("rb") as stream:
        for chunk in iter(lambda: stream.read(1024 * 1024), b""):
            digest.update(chunk)
    return {"path": str(path), "sha256": digest.hexdigest()}


def address(value):
    if not re.fullmatch(r"(?:[0-9A-Fa-f]{2}:){5}[0-9A-Fa-f]{2}", line(value)) or value.upper() in ("00:00:00:00:00:00", "FF:FF:FF:FF:FF:FF"):
        raise ValueError("require a real observed Bluetooth address")
    return value.upper()


def generate(inventory, output):
    data = json.loads(Path(inventory).read_text())
    host, peripheral = data["host"], data["peripheral"]
    for key in ("serial", "owner", "management_selector", "os_release", "kernel", "bluez_version"):
        line(host[key])
    selector = host["management_selector"]
    if selector.startswith(("-", "cloud:", "mesh:", "bluetooth:", "wendy-lite:")):
        raise ValueError("reserve a direct wired management selector")
    if host["architecture"] not in ("arm64", "amd64") or type(host["adapter_index"]) is not int or not 0 <= host["adapter_index"] < 256:
        raise ValueError("require a Linux64 host and exact adapter index")
    adapter_mac, peripheral_mac = address(host["adapter_mac"]), address(peripheral["address"])
    if adapter_mac == peripheral_mac:
        raise ValueError("controller and peripheral must differ")
    for key in ("model", "serial"):
        line(peripheral[key])
    if peripheral["profile"] not in ("le-hid", "classic-hid"):
        raise ValueError("profile must be le-hid or classic-hid")
    labels = peripheral["action_labels"]
    if not isinstance(labels, list) or len(labels) < 2 or len(labels) > 8 or len(set(labels)) != len(labels):
        raise ValueError("predeclare two to eight distinct safe physical keys/buttons")
    for label in labels:
        line(label)
    cli, agent = artifact(data["cli_binary"]), artifact(data["agent_binary"])
    if not os.access(cli["path"], os.X_OK) or not re.fullmatch(r"[0-9a-f]{40}", data["agent_source_commit"]):
        raise ValueError("pin executable CLI and full Agent source revision")
    records = {key: artifact(data["records"][key]) for key in ("host_identity", "adapter_identity", "peripheral_identity", "original_state", "recovery", "peripheral_bond_reset")}
    recipe = json.loads(Path(records["peripheral_bond_reset"]["path"]).read_text())
    if (recipe.get("peripheral_serial") != peripheral["serial"] or recipe.get("model") != peripheral["model"]
            or recipe.get("owner_approved") is not True or recipe.get("scope") != "peripheral-bond-only"
            or not line(recipe["vendor_url"]).startswith("https://") or type(recipe.get("max_seconds")) is not int
            or not 1 <= recipe["max_seconds"] <= 120):
        raise ValueError("require an exact-target owner-approved peripheral-only vendor reset")
    for key in ("slot", "expected_confirmation"):
        line(recipe[key])
    if not isinstance(recipe["steps"], list) or not recipe["steps"]:
        raise ValueError("vendor recipe must contain concrete steps")
    for step in recipe["steps"]:
        line(step)
    out = Path(output).resolve()
    out.mkdir(mode=0o700, parents=True, exist_ok=False)
    nonce = secrets.token_hex(8)
    challenge = [secrets.choice(labels) for _ in range(6)]
    challenge[:2] = secrets.SystemRandom().sample(labels, 2)
    guardian = str(ROOT / ".github/scripts/run-owned-media-test.py")
    def cli_command(seconds, *arguments):
        return ["python3", guardian, "--timeout", str(seconds), "--", cli["path"], "--device", selector, "device", "bluetooth", *arguments]
    adapter = f"/org/bluez/hci{host['adapter_index']}"
    device = adapter + "/dev_" + peripheral_mac.replace(":", "_")
    commands = {
        "baseline_pair": cli_command(60, "connect", peripheral_mac, "--pair=true", "--trust=true"),
        "disconnect_target": cli_command(30, "disconnect", peripheral_mac),
        "repair_preserving_live_trust": cli_command(60, "connect", peripheral_mac, "--pair=true", "--trust=false"),
        "no_pair_control": cli_command(60, "connect", peripheral_mac, "--pair=false", "--trust=false"),
        "controller_address_on_target": ["busctl", "--system", "--timeout=5s", "get-property", "org.bluez", adapter, "org.bluez.Adapter1", "Address"],
        "controller_trace_on_target": ["sudo", "-n", "timeout", "--signal=INT", "--kill-after=3s", "100s", "btmon", "--index", str(host["adapter_index"]), "--write", "controller.btsnoop"],
        "input_registry_on_target": ["timeout", "--signal=TERM", "--kill-after=3s", "70s", "python3", "bluez-input-observer.py", "--address", peripheral_mac, "--seconds", "65", "--output", "input-registry.jsonl"],
    }
    methods = [
        f"type='method_call',destination='org.bluez',path='{adapter}',interface='org.bluez.Adapter1',member='RemoveDevice'",
        f"type='method_call',destination='org.bluez',path='{device}',interface='org.bluez.Device1',member='Pair'",
        f"type='method_call',destination='org.bluez',path='{device}',interface='org.freedesktop.DBus.Properties',member='Get'",
    ]
    commands["target_methods_on_target"] = ["sudo", "-n", "timeout", "--signal=INT", "--kill-after=3s", "100s",
        "stdbuf", "-oL", "-eL", "dbus-monitor", "--system", "--monitor", *methods]
    for prop in ("Address", "Paired", "Trusted", "Connected", "UUIDs"):
        commands["target_" + prop.lower()] = ["busctl", "--system", "--timeout=5s", "get-property", "org.bluez", device, "org.bluez.Device1", prop]
    shutil.copyfile(Path(__file__).with_name("bluez-input-observer.py"), out / "bluez-input-observer.py")
    shutil.copyfile(records["peripheral_bond_reset"]["path"], out / "peripheral-bond-reset.json")
    shutil.copyfile(records["recovery"]["path"], out / "recovery.txt")
    (out / "commands.json").write_text(json.dumps(commands, indent=2) + "\n")
    guide = Path(__file__).with_suffix(".txt").read_text()
    replacements = {"@NONCE@":nonce,"@PROFILE@":peripheral["profile"],"@ADDRESS@":peripheral_mac,"@ADAPTER@":adapter_mac,
                    "@CHALLENGE@":"; ".join(challenge),"@SERIAL@":peripheral["serial"],"@HOST@":host["serial"],
                    "@RESET_SECONDS@":str(recipe["max_seconds"])}
    for key,value in replacements.items():
        guide=guide.replace(key,value)
    guide += "\nExact commands, not executed during generation:\n" + "\n".join(name + ":\n" + shlex.join(argv) for name,argv in commands.items()) + "\n"
    (out / "procedure.txt").write_text(guide)
    sources = [Path(__file__), Path(__file__).with_suffix(".txt"), Path(__file__).with_name("bluez-input-observer.py"), ROOT / "go/internal/agent/bluetooth/manager_linux.go", ROOT / "go/internal/agent/bluetooth/pairing_agent_linux.go", ROOT / "go/internal/cli/commands/bluetooth.go", Path(guardian)]
    evidence = {"audit_entries":["1595"],"hardware_execution":"not_run","nonce":nonce,"profile":peripheral["profile"],
        "inventory":data,"cli":cli,"agent":agent,"records":records,"sources":[artifact(str(p)) for p in sources],
        "challenge":challenge,"cases":[{"id":name,"status":"not_run","observed_trigger":None,"evidence":[],"reviewer":None}
        for name in ("identity_and_baseline","peripheral_only_bond_loss","key_error_recovery","hollow_hid_recovery","no_pair_control","reconnect_and_input","restore")],
        "other_profile":"not_run; generate a separate plan for a physical peripheral using the other HID transport"}
    (out / "evidence.json").write_text(json.dumps(evidence,indent=2)+"\n")
    return evidence


if __name__ == "__main__":
    parser=argparse.ArgumentParser(description=__doc__)
    parser.add_argument("inventory",type=Path)
    parser.add_argument("output",type=Path)
    args=parser.parse_args()
    generate(args.inventory,args.output)
